package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolkit"
)

// ReadName is the read tool's canonical name.
const ReadName = "brain"

const (
	opDiscover = "discover"
	opSearch   = "search"
	opGet      = "get"
)

// A search returns enough to choose from, capped so a broad query cannot return
// a whole knowledge base at once.
const (
	searchDefaultLimit = 10
	searchMaxLimit     = 25
)

type readArgs struct {
	Operation      string `json:"operation"`
	Brain          string `json:"brain"`
	Query          string `json:"query"`
	Limit          int    `json:"limit"`
	Document       int64  `json:"document"`
	IncludeRelated bool   `json:"include_related"`
}

// NewRead builds the read tool bound to an EMPTY allow-list. Registered at boot,
// it reaches nothing until the loadout rebinds its handler with the agent's own
// brains for the turn: a fail-closed default, so a wiring slip hides knowledge
// rather than leaking it.
func NewRead(st ReadStore) tool.Tool {
	return tool.Tool{Schema: readSchema(), Handle: ReadHandler(st, nil)}
}

// ReadHandler is the read handler bound to a per-turn allow-list: the ids of the
// brains this agent may reach. Every operation is confined to it.
func ReadHandler(st ReadStore, allowed []int64) tool.Handler {
	return func(ctx context.Context, call tool.Call) (tool.Result, error) {
		var args readArgs
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return toolkit.BadArguments("the arguments were not valid JSON.")
		}
		switch strings.TrimSpace(args.Operation) {
		case opDiscover:
			return discover(ctx, st, call.WorkspaceID, allowed)
		case opSearch:
			return search(ctx, st, call.WorkspaceID, allowed, args)
		case opGet:
			return get(ctx, st, call.WorkspaceID, allowed, args)
		case "":
			return toolkit.BadArguments(`"operation" is required: one of "discover", "search", "get".`)
		default:
			return toolkit.BadArguments(fmt.Sprintf("unknown operation %q; use discover, search or get.", args.Operation))
		}
	}
}

func readSchema() tool.Schema {
	return tool.Schema{
		Name:         ReadName,
		FriendlyName: "Knowledge",
		About: "Lets the agent read the knowledge bases it has been given: your policies, procedures, product " +
			"detail, anything the business has written down for it. It reads only what each agent was assigned, " +
			"and nothing else in the workspace.",
		FriendlyNarration: "Consulting knowledge",
		Description: "Search and read the knowledge bases assigned to you: discover lists them and " +
			"their categories, search finds documents, get opens one in full with the related ones you " +
			"can follow. Search here before answering from memory.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "operation": {"type": "string", "enum": ["discover", "search", "get"], "description": "What to do."},
    "brain": {"type": "string", "description": "For search: narrow to one knowledge base, by name or slug. Omit to search all of yours."},
    "query": {"type": "string", "description": "For search: the text to look for. Required for search."},
    "limit": {"type": "integer", "description": "For search: the most results to return (default 10, max 25)."},
    "document": {"type": "integer", "description": "For get: the id of the document to open. Required for get."},
    "include_related": {"type": "boolean", "description": "For get: also return a short preview of each related document, so you can choose which to open in full."}
  },
  "required": ["operation"]
}`),
		Kind: tool.KindBuiltin,
		Risk: tool.RiskReadOnly,
	}
}

// discover lists the agent's knowledge bases, their categories, and which are
// read-only: the map it navigates from.
func discover(ctx context.Context, st ReadStore, workspaceID int64, allowed []int64) (tool.Result, error) {
	brains, err := reachable(ctx, st, workspaceID, allowed)
	if err != nil {
		return tool.Result{}, err
	}
	out := make([]map[string]any, 0, len(brains))
	for _, b := range brains {
		cats, err := st.Categories(ctx, workspaceID, b.ID)
		if err != nil {
			return tool.Result{}, err
		}
		names := make([]string, 0, len(cats))
		for _, c := range cats {
			names = append(names, c.Name)
		}
		out = append(out, map[string]any{
			"brain":       b.Name,
			"slug":        b.Slug,
			"description": b.Description,
			"read_only":   b.Locked,
			"categories":  names,
		})
	}
	return toolkit.Success(map[string]any{"brains": out})
}

// search finds documents by relevance. It searches every assigned brain unless
// the agent names one, and the store already intersects the ids it is given with
// the allow-list, so an empty list finds nothing.
func search(ctx context.Context, st ReadStore, workspaceID int64, allowed []int64, args readArgs) (tool.Result, error) {
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return toolkit.BadArguments(`"query" is required to search.`)
	}

	targets := allowed
	if ref := strings.TrimSpace(args.Brain); ref != "" {
		brains, err := reachable(ctx, st, workspaceID, allowed)
		if err != nil {
			return tool.Result{}, err
		}
		b := matchBrain(brains, ref)
		if b == nil {
			return toolkit.BadArguments("that knowledge base is not one of yours; use discover to list what you can reach.")
		}
		targets = []int64{b.ID}
	}

	limit := args.Limit
	if limit <= 0 {
		limit = searchDefaultLimit
	}
	if limit > searchMaxLimit {
		limit = searchMaxLimit
	}

	hits, err := st.Search(ctx, workspaceID, targets, query, limit)
	if err != nil {
		return tool.Result{}, err
	}
	return toolkit.Success(map[string]any{"query": query, "count": len(hits), "results": hits})
}

// get opens one document in full, guarded: a document in a brain the agent was
// not assigned does not exist to it, whatever id it names.
func get(ctx context.Context, st ReadStore, workspaceID int64, allowed []int64, args readArgs) (tool.Result, error) {
	if args.Document <= 0 {
		return toolkit.BadArguments(`"document" (a document id) is required for get.`)
	}
	set := idSet(allowed)

	doc, err := st.Document(ctx, workspaceID, args.Document)
	if errors.Is(err, store.ErrNotFound) {
		return toolkit.BadArguments("that document was not found.")
	}
	if err != nil {
		return tool.Result{}, err
	}
	if !set[doc.BrainID] {
		// In a brain the agent may not reach: to the agent, it is not there.
		return toolkit.BadArguments("that document is not one you can reach.")
	}

	related := make([]map[string]any, 0, len(doc.Related))
	for _, r := range doc.Related {
		entry := map[string]any{"document": r.ID, "title": r.Title, "category": r.Category}
		if args.IncludeRelated {
			// Links are confined to the same brain (which we just cleared), so a
			// preview cannot walk out of the allow-list.
			if rel, err := st.Document(ctx, workspaceID, r.ID); err == nil {
				entry["preview"] = preview(rel.Content)
			}
		}
		related = append(related, entry)
	}

	return toolkit.Success(map[string]any{
		"document": map[string]any{
			"id":      doc.ID,
			"title":   doc.Title,
			"content": doc.Content,
			"related": related,
		},
	})
}
