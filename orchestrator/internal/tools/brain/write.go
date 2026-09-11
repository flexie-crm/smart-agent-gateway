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

// WriteName is the write tool's canonical name.
const WriteName = "brain_write"

const (
	opSaveCategory   = "save_category"
	opSaveDocument   = "save_document"
	opImport         = "import"
	opDeleteDocument = "delete_document"
	opDeleteCategory = "delete_category"
)

// WriteStore is the brain store as the write tool uses it: the reads it needs to
// resolve and validate a write, plus the writes themselves. The full BrainStore
// satisfies it.
type WriteStore interface {
	ReadStore
	DocumentByTitle(ctx context.Context, workspaceID, categoryID int64, title string) (*model.BrainDocument, error)
	CreateCategory(ctx context.Context, workspaceID int64, c *model.BrainCategory) error
	SaveDocument(ctx context.Context, workspaceID int64, d *model.BrainDocument, related []int64) error
	DeleteDocument(ctx context.Context, workspaceID, id int64) error
	DeleteCategory(ctx context.Context, workspaceID, id int64) error
}

type writeArgs struct {
	Operation   string  `json:"operation"`
	Brain       string  `json:"brain"`
	Category    string  `json:"category"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Title       string  `json:"title"`
	Content     string  `json:"content"`
	Related     []int64 `json:"related"`
	Markdown    string  `json:"markdown"`
	Document    int64   `json:"document"`
}

// writePlan is a resolved, validated write: every id looked up, every rule
// checked, so executing it cannot fail on a bad argument. It also carries the
// card copy for THIS write. Validate builds a plan and shows its card; Handle
// builds it AGAIN and runs it. Building it twice, rather than carrying one across
// the park, means the resume re-checks against the world as it is now: a brain
// locked while the card waited refuses the write instead of running it.
type writePlan struct {
	op          string
	brain       *model.Brain
	category    *model.BrainCategory
	name        string
	description string
	title       string
	content     string
	related     []int64
	docID       int64
	docTitle    string
	documents   []parsedDoc
	cardTitle   string
	cardPrompt  string
}

// NewWrite builds the write tool bound to an EMPTY allow-list (it reaches
// nothing until the loadout rebinds it over the agent's brains). Approval is not
// a code floor: writing knowledge is a normal tool an administrator gates per
// agent, so RequiresApproval is left off here.
func NewWrite(st WriteStore) tool.Tool {
	return tool.Tool{
		Schema:   writeSchema(),
		Handle:   WriteHandler(st, nil),
		Validate: WriteValidator(st, nil),
	}
}

// WriteValidator is the pre-park check: it plans the write and, if anything is
// wrong (a locked or unassigned brain, a missing category, empty content), it
// refuses BEFORE any card. On success it hands back the per-call card copy, so a
// person reads what will actually happen. This is what makes approval equal
// success for the write tool.
func WriteValidator(st WriteStore, allowed []int64) tool.Validator {
	return func(ctx context.Context, call tool.Call) tool.Validation {
		var args writeArgs
		if err := json.Unmarshal(call.Args, &args); err != nil {
			res, _ := toolkit.BadArguments("the arguments were not valid JSON.")
			return tool.Validation{OK: false, Result: res}
		}
		p, refusal, err := plan(ctx, st, call.WorkspaceID, allowed, args)
		if err != nil {
			// The store could not be read to validate the write. Refuse the card
			// (nothing is approved for something we could not check) and let the
			// model try again.
			res, _ := toolkit.Transient("the knowledge base could not be read just now; try again.")
			return tool.Validation{OK: false, Result: res}
		}
		if refusal != nil {
			return tool.Validation{OK: false, Result: *refusal}
		}
		return tool.Validation{OK: true, ApprovalTitle: p.cardTitle, ApprovalPrompt: p.cardPrompt}
	}
}

// WriteHandler runs the write. It re-plans (so the resume checks the world as it
// is now) and, if the plan still holds, executes it. Because Validate already
// planned the same call, an approved write lands.
func WriteHandler(st WriteStore, allowed []int64) tool.Handler {
	return func(ctx context.Context, call tool.Call) (tool.Result, error) {
		var args writeArgs
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return toolkit.BadArguments("the arguments were not valid JSON.")
		}
		p, refusal, err := plan(ctx, st, call.WorkspaceID, allowed, args)
		if err != nil {
			return tool.Result{}, err
		}
		if refusal != nil {
			return *refusal, nil
		}
		return execute(ctx, st, call.WorkspaceID, p)
	}
}

func writeSchema() tool.Schema {
	return tool.Schema{
		Name:         WriteName,
		FriendlyName: "Update knowledge",
		About: "Lets the agent add to and correct the knowledge bases it has been given, so what it learns in " +
			"one conversation is there for the next one. It can only change a knowledge base that has been left " +
			"unlocked; a locked one stays read-only.",
		FriendlyNarration: "Updating knowledge",
		Description: "Add to and maintain a knowledge base you have write access to: save a category, " +
			"save a document (re-using a title updates it), import many from Markdown, or delete " +
			"either. A read-only base refuses every write. Content is plain Markdown, never HTML. " +
			"Confirm WHERE new knowledge goes before writing it.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "operation": {"type": "string", "enum": ["save_category", "save_document", "import", "delete_document", "delete_category"], "description": "What to do."},
    "brain": {"type": "string", "description": "The knowledge base to write to, by name or slug. Required for every operation except delete_document."},
    "category": {"type": "string", "description": "The category, by name, for save_document, import and delete_category."},
    "name": {"type": "string", "description": "For save_category: the category name."},
    "description": {"type": "string", "description": "For save_category: an optional short description."},
    "title": {"type": "string", "description": "For save_document: the document title. Re-using an existing title updates that document."},
    "content": {"type": "string", "description": "For save_document: the document body, as plain Markdown."},
    "related": {"type": "array", "items": {"type": "integer"}, "description": "For save_document: ids of related documents in the SAME knowledge base to link (from search or get)."},
    "markdown": {"type": "string", "description": "For import: the Markdown to split into documents on top-level '# ' headings; each heading becomes a title."},
    "document": {"type": "integer", "description": "For delete_document: the id of the document to delete."}
  },
  "required": ["operation"]
}`),
		Kind: tool.KindBuiltin,
		Risk: tool.RiskInternalWrite,
	}
}

// plan resolves and validates a write. It returns exactly one of: a plan (ready
// to run), a refusal (a bad argument the model corrects, never a card), or a Go
// error (the store could not be read).
func plan(ctx context.Context, st WriteStore, ws int64, allowed []int64, args writeArgs) (*writePlan, *tool.Result, error) {
	switch strings.TrimSpace(args.Operation) {
	case opSaveCategory:
		return planSaveCategory(ctx, st, ws, allowed, args)
	case opSaveDocument:
		return planSaveDocument(ctx, st, ws, allowed, args)
	case opImport:
		return planImport(ctx, st, ws, allowed, args)
	case opDeleteDocument:
		return planDeleteDocument(ctx, st, ws, allowed, args)
	case opDeleteCategory:
		return planDeleteCategory(ctx, st, ws, allowed, args)
	case "":
		return refuse(`"operation" is required: one of save_category, save_document, import, delete_document, delete_category.`)
	default:
		return refuse(fmt.Sprintf("unknown operation %q.", args.Operation))
	}
}

func planSaveCategory(ctx context.Context, st WriteStore, ws int64, allowed []int64, args writeArgs) (*writePlan, *tool.Result, error) {
	brain, refusal, err := writableBrain(ctx, st, ws, allowed, args.Brain)
	if err != nil || refusal != nil {
		return nil, refusal, err
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return refuse(`a category "name" is required.`)
	}
	return &writePlan{
		op: opSaveCategory, brain: brain, name: name, description: strings.TrimSpace(args.Description),
		cardTitle:  "Add a knowledge category",
		cardPrompt: fmt.Sprintf("Add the category %q to %s.", name, brain.Name),
	}, nil, nil
}

func planSaveDocument(ctx context.Context, st WriteStore, ws int64, allowed []int64, args writeArgs) (*writePlan, *tool.Result, error) {
	brain, refusal, err := writableBrain(ctx, st, ws, allowed, args.Brain)
	if err != nil || refusal != nil {
		return nil, refusal, err
	}
	cat, refusal, err := requireCategory(ctx, st, ws, brain, args.Category)
	if err != nil || refusal != nil {
		return nil, refusal, err
	}
	title := strings.TrimSpace(args.Title)
	if title == "" {
		return refuse(`a document "title" is required.`)
	}
	if strings.TrimSpace(args.Content) == "" {
		return refuse(`document "content" is required.`)
	}

	// Resolve create vs update by the unique (category, title), so a repeated
	// title updates in place instead of colliding.
	verb, preposition, docID := "Add", "to", int64(0)
	existing, err := st.DocumentByTitle(ctx, ws, cat.ID, title)
	switch {
	case err == nil:
		verb, preposition, docID = "Update", "in", existing.ID
	case errors.Is(err, store.ErrNotFound):
		// A new document.
	default:
		return nil, nil, err
	}

	return &writePlan{
		op: opSaveDocument, brain: brain, category: cat, title: title, content: args.Content,
		related: args.Related, docID: docID,
		cardTitle:  verb + " a knowledge document",
		cardPrompt: fmt.Sprintf("%s %q %s %s / %s.", verb, title, preposition, brain.Name, cat.Name),
	}, nil, nil
}

func planImport(ctx context.Context, st WriteStore, ws int64, allowed []int64, args writeArgs) (*writePlan, *tool.Result, error) {
	brain, refusal, err := writableBrain(ctx, st, ws, allowed, args.Brain)
	if err != nil || refusal != nil {
		return nil, refusal, err
	}
	cat, refusal, err := requireCategory(ctx, st, ws, brain, args.Category)
	if err != nil || refusal != nil {
		return nil, refusal, err
	}
	if strings.TrimSpace(args.Markdown) == "" {
		return refuse(`provide the "markdown" to import.`)
	}
	docs := splitMarkdown(args.Markdown)
	if len(docs) == 0 {
		return refuse(`no documents found; each document must start with a top-level Markdown heading (a line beginning with "# ").`)
	}
	return &writePlan{
		op: opImport, brain: brain, category: cat, documents: docs,
		cardTitle:  "Import knowledge",
		cardPrompt: fmt.Sprintf("Import %d document(s) into %s / %s.", len(docs), brain.Name, cat.Name),
	}, nil, nil
}

func planDeleteDocument(ctx context.Context, st WriteStore, ws int64, allowed []int64, args writeArgs) (*writePlan, *tool.Result, error) {
	if args.Document <= 0 {
		return refuse(`a "document" id is required to delete a document.`)
	}
	doc, err := st.Document(ctx, ws, args.Document)
	if errors.Is(err, store.ErrNotFound) {
		return refuse("that document was not found.")
	}
	if err != nil {
		return nil, nil, err
	}
	brain, refusal, err := reachableWritable(ctx, st, ws, allowed, doc.BrainID)
	if err != nil || refusal != nil {
		return nil, refusal, err
	}
	return &writePlan{
		op: opDeleteDocument, brain: brain, docID: doc.ID, docTitle: doc.Title,
		cardTitle:  "Delete a knowledge document",
		cardPrompt: fmt.Sprintf("Delete %q from %s.", doc.Title, brain.Name),
	}, nil, nil
}

func planDeleteCategory(ctx context.Context, st WriteStore, ws int64, allowed []int64, args writeArgs) (*writePlan, *tool.Result, error) {
	brain, refusal, err := writableBrain(ctx, st, ws, allowed, args.Brain)
	if err != nil || refusal != nil {
		return nil, refusal, err
	}
	cat, refusal, err := requireCategory(ctx, st, ws, brain, args.Category)
	if err != nil || refusal != nil {
		return nil, refusal, err
	}
	return &writePlan{
		op: opDeleteCategory, brain: brain, category: cat,
		cardTitle:  "Delete a knowledge category",
		cardPrompt: fmt.Sprintf("Delete %q and its documents from %s.", cat.Name, brain.Name),
	}, nil, nil
}

// execute performs a planned write. The plan is already validated, so the only
// errors here are the store's own.
func execute(ctx context.Context, st WriteStore, ws int64, p *writePlan) (tool.Result, error) {
	switch p.op {
	case opSaveCategory:
		c := &model.BrainCategory{BrainID: p.brain.ID, Name: p.name, Description: p.description}
		if err := st.CreateCategory(ctx, ws, c); err != nil {
			return tool.Result{}, err
		}
		return toolkit.Success(map[string]any{"saved": "category", "name": c.Name, "brain": p.brain.Name})
	case opSaveDocument:
		d := &model.BrainDocument{ID: p.docID, CategoryID: p.category.ID, Title: p.title, Content: p.content}
		if err := st.SaveDocument(ctx, ws, d, p.related); err != nil {
			return tool.Result{}, err
		}
		return toolkit.Success(map[string]any{"saved": "document", "id": d.ID, "title": d.Title, "brain": p.brain.Name, "category": p.category.Name})
	case opImport:
		created, updated := 0, 0
		for _, pd := range p.documents {
			id := int64(0)
			existing, err := st.DocumentByTitle(ctx, ws, p.category.ID, pd.title)
			switch {
			case err == nil:
				id = existing.ID
			case errors.Is(err, store.ErrNotFound):
			default:
				return tool.Result{}, err
			}
			d := &model.BrainDocument{ID: id, CategoryID: p.category.ID, Title: pd.title, Content: pd.content}
			if err := st.SaveDocument(ctx, ws, d, nil); err != nil {
				return tool.Result{}, err
			}
			if id == 0 {
				created++
			} else {
				updated++
			}
		}
		return toolkit.Success(map[string]any{"imported": true, "created": created, "updated": updated, "brain": p.brain.Name, "category": p.category.Name})
	case opDeleteDocument:
		if err := st.DeleteDocument(ctx, ws, p.docID); err != nil {
			return tool.Result{}, err
		}
		return toolkit.Success(map[string]any{"deleted": "document", "title": p.docTitle, "brain": p.brain.Name})
	case opDeleteCategory:
		if err := st.DeleteCategory(ctx, ws, p.category.ID); err != nil {
			return tool.Result{}, err
		}
		return toolkit.Success(map[string]any{"deleted": "category", "name": p.category.Name, "brain": p.brain.Name})
	}
	return toolkit.Failed("unknown operation")
}

// writableBrain resolves a brain the agent named and confirms it may be written:
// in the allow-list, and not read-only.
func writableBrain(ctx context.Context, st WriteStore, ws int64, allowed []int64, ref string) (*model.Brain, *tool.Result, error) {
	if strings.TrimSpace(ref) == "" {
		res, _ := toolkit.BadArguments("specify which knowledge base to write to, by name or slug.")
		return nil, &res, nil
	}
	brains, err := reachable(ctx, st, ws, allowed)
	if err != nil {
		return nil, nil, err
	}
	b := matchBrain(brains, ref)
	if b == nil {
		res, _ := toolkit.BadArguments("that knowledge base is not one of yours; use the brain tool's discover to list what you can reach.")
		return nil, &res, nil
	}
	if b.Locked {
		res, _ := toolkit.BadArguments(fmt.Sprintf("the %q knowledge base is read-only and cannot be changed.", b.Name))
		return nil, &res, nil
	}
	return b, nil, nil
}

// reachableWritable confirms a brain id (from a document being deleted) is one
// the agent may write: assigned and not read-only.
func reachableWritable(ctx context.Context, st WriteStore, ws int64, allowed []int64, brainID int64) (*model.Brain, *tool.Result, error) {
	brains, err := reachable(ctx, st, ws, allowed)
	if err != nil {
		return nil, nil, err
	}
	for _, b := range brains {
		if b.ID == brainID {
			if b.Locked {
				res, _ := toolkit.BadArguments(fmt.Sprintf("the %q knowledge base is read-only and cannot be changed.", b.Name))
				return nil, &res, nil
			}
			return b, nil, nil
		}
	}
	res, _ := toolkit.BadArguments("that document is not one you can reach.")
	return nil, &res, nil
}

// requireCategory resolves a category by name within a brain, refusing when it is
// missing (the agent should create it first, or pick an existing one).
func requireCategory(ctx context.Context, st WriteStore, ws int64, brain *model.Brain, ref string) (*model.BrainCategory, *tool.Result, error) {
	name := strings.TrimSpace(ref)
	if name == "" {
		res, _ := toolkit.BadArguments(`specify the "category", by name.`)
		return nil, &res, nil
	}
	cats, err := st.Categories(ctx, ws, brain.ID)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range cats {
		if strings.EqualFold(c.Name, name) {
			return c, nil, nil
		}
	}
	res, _ := toolkit.BadArguments(fmt.Sprintf("no category %q in %q; create it first with save_category, or pick an existing one (the brain tool's discover lists them).", name, brain.Name))
	return nil, &res, nil
}

func refuse(msg string) (*writePlan, *tool.Result, error) {
	res, _ := toolkit.BadArguments(msg)
	return nil, &res, nil
}

type parsedDoc struct {
	title   string
	content string
}

// splitMarkdown cuts a Markdown blob into documents on top-level "# " headings.
// The heading is the title; everything until the next top-level heading is the
// body. Text before the first heading has no title, so it is ignored. "## " and
// deeper are NOT top-level and stay inside a document.
func splitMarkdown(md string) []parsedDoc {
	var docs []parsedDoc
	var title string
	var body []string
	flush := func() {
		if title != "" {
			docs = append(docs, parsedDoc{title: title, content: strings.TrimSpace(strings.Join(body, "\n"))})
		}
		body = nil
	}
	for _, line := range strings.Split(md, "\n") {
		if h := topHeading(line); h != "" {
			flush()
			title = h
			continue
		}
		if title != "" {
			body = append(body, line)
		}
	}
	flush()
	return docs
}

func topHeading(line string) string {
	t := strings.TrimRight(line, " \t")
	if strings.HasPrefix(t, "# ") {
		return strings.TrimSpace(t[2:])
	}
	return ""
}
