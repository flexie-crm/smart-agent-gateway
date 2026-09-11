package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolkit"
)

// MemoryName is the memory tool's canonical name.
const MemoryName = "memory"

const (
	memOpSearch = "search"
	memOpSave   = "save"
	memOpGet    = "get"
)

type memArgs struct {
	Operation string  `json:"operation"`
	Query     string  `json:"query"`
	Limit     int     `json:"limit"`
	Category  string  `json:"category"`
	Title     string  `json:"title"`
	Content   string  `json:"content"`
	Related   []int64 `json:"related"`
	Document  int64   `json:"document"`
}

// MemorySchema is the agent's own long-term memory: one brain it manages itself,
// searching, saving and organising what it learns about its work. It is internal
// infrastructure, never approval-gated, present only when the agent has a memory
// brain assigned. Asking a person to approve an agent keeping its own notes would
// be absurd, so this tool never parks.
func MemorySchema() tool.Schema {
	return tool.Schema{
		Name:         MemoryName,
		FriendlyName: "Your memory",
		About: "Lets an agent keep its own working notes between conversations: what it learned about how you " +
			"work, decisions already made, mistakes not to repeat. It is private to that one agent and is not " +
			"the shared knowledge the team reads.",
		FriendlyNarration: "Working with my memory",
		Description: "Your own long-term memory, which stays with you across conversations: save what " +
			"you learn about doing the work, search it before work you may have done before, and open " +
			"a note in full. File each under a fitting category and link related notes, so it grows " +
			"into a map you can navigate.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "operation": {"type": "string", "enum": ["search", "save", "get"], "description": "What to do."},
    "query": {"type": "string", "description": "For search: what to look for. Required for search."},
    "limit": {"type": "integer", "description": "For search: the most results to return (default 10, max 25)."},
    "category": {"type": "string", "description": "For save: the category to file this note under, by name. A new name creates the category."},
    "title": {"type": "string", "description": "For save: the note's title. Re-using a title updates that note."},
    "content": {"type": "string", "description": "For save: the note itself, as plain Markdown."},
    "related": {"type": "array", "items": {"type": "integer"}, "description": "For save: ids of related notes (from search or get) to link this one to."},
    "document": {"type": "integer", "description": "For get: the id of the note to open."}
  },
  "required": ["operation"]
}`),
		Kind: tool.KindInternal,
		Risk: tool.RiskInternalWrite,
	}
}

// MemoryHandler binds the memory tool to the ONE brain this agent manages as its
// memory. There is no brain to name and no allow-list to intersect: it is the
// agent's own, always readable and writable by it.
func MemoryHandler(st WriteStore, memoryBrainID int64) tool.Handler {
	return func(ctx context.Context, call tool.Call) (tool.Result, error) {
		var args memArgs
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return toolkit.BadArguments("the arguments were not valid JSON.")
		}
		switch strings.TrimSpace(args.Operation) {
		case memOpSearch:
			return memorySearch(ctx, st, call.WorkspaceID, memoryBrainID, args)
		case memOpSave:
			return memorySave(ctx, st, call.WorkspaceID, memoryBrainID, args)
		case memOpGet:
			return memoryGet(ctx, st, call.WorkspaceID, memoryBrainID, args)
		case "":
			return toolkit.BadArguments(`"operation" is required: one of "search", "save", "get".`)
		default:
			return toolkit.BadArguments(fmt.Sprintf("unknown operation %q; use search, save or get.", args.Operation))
		}
	}
}

func memorySearch(ctx context.Context, st WriteStore, ws, memoryBrainID int64, args memArgs) (tool.Result, error) {
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return toolkit.BadArguments(`a "query" is required to search your memory.`)
	}
	limit := args.Limit
	if limit <= 0 {
		limit = searchDefaultLimit
	}
	if limit > searchMaxLimit {
		limit = searchMaxLimit
	}
	hits, err := st.Search(ctx, ws, []int64{memoryBrainID}, query, limit)
	if err != nil {
		return tool.Result{}, err
	}
	return toolkit.Success(map[string]any{"query": query, "count": len(hits), "results": hits})
}

func memorySave(ctx context.Context, st WriteStore, ws, memoryBrainID int64, args memArgs) (tool.Result, error) {
	catName := strings.TrimSpace(args.Category)
	if catName == "" {
		return toolkit.BadArguments(`a "category" is required to save a memory: reuse one that fits, or name a new one.`)
	}
	title := strings.TrimSpace(args.Title)
	if title == "" {
		return toolkit.BadArguments(`a "title" is required.`)
	}
	if strings.TrimSpace(args.Content) == "" {
		return toolkit.BadArguments(`"content" is required.`)
	}

	cat, err := findOrCreateCategory(ctx, st, ws, memoryBrainID, catName)
	if err != nil {
		return tool.Result{}, err
	}

	// Idempotent by title: telling itself the same thing updates the note rather
	// than piling up duplicates.
	docID := int64(0)
	verb := "saved"
	switch existing, err := st.DocumentByTitle(ctx, ws, cat.ID, title); {
	case err == nil:
		docID, verb = existing.ID, "updated"
	case errors.Is(err, store.ErrNotFound):
	default:
		return tool.Result{}, err
	}

	d := &model.BrainDocument{ID: docID, CategoryID: cat.ID, Title: title, Content: args.Content}
	if err := st.SaveDocument(ctx, ws, d, args.Related); err != nil {
		return tool.Result{}, err
	}
	return toolkit.Success(map[string]any{"memory": verb, "id": d.ID, "category": cat.Name, "title": title})
}

func memoryGet(ctx context.Context, st WriteStore, ws, memoryBrainID int64, args memArgs) (tool.Result, error) {
	if args.Document <= 0 {
		return toolkit.BadArguments(`a "document" id is required for get.`)
	}
	doc, err := st.Document(ctx, ws, args.Document)
	if errors.Is(err, store.ErrNotFound) {
		return toolkit.BadArguments("that memory was not found.")
	}
	if err != nil {
		return tool.Result{}, err
	}
	if doc.BrainID != memoryBrainID {
		return toolkit.BadArguments("that document is not in your memory.")
	}
	related := make([]map[string]any, 0, len(doc.Related))
	for _, r := range doc.Related {
		related = append(related, map[string]any{"document": r.ID, "title": r.Title, "category": r.Category})
	}
	return toolkit.Success(map[string]any{
		"document": map[string]any{"id": doc.ID, "title": doc.Title, "content": doc.Content, "related": related},
	})
}

// findOrCreateCategory reuses a category by name or creates it. The memory brain
// has no fixed set of categories: the agent invents them as it organises itself.
func findOrCreateCategory(ctx context.Context, st WriteStore, ws, brainID int64, name string) (*model.BrainCategory, error) {
	cats, err := st.Categories(ctx, ws, brainID)
	if err != nil {
		return nil, err
	}
	for _, c := range cats {
		if strings.EqualFold(c.Name, name) {
			return c, nil
		}
	}
	c := &model.BrainCategory{BrainID: brainID, Name: name}
	if err := st.CreateCategory(ctx, ws, c); err != nil {
		return nil, err
	}
	return c, nil
}
