package brain

import (
	"context"
	"encoding/json"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
)

// A fake store: the read tool's whole job is to CONFINE what it asks the store
// for to the allow-list, so the test drives the handler and watches both the
// results it returns and the allow-list it passes down to search. The store's
// own behaviour (relevance, the graph) is covered by the store suite.
type fakeStore struct {
	brains        []*model.Brain
	categories    map[int64][]*model.BrainCategory
	docs          map[int64]*model.BrainDocument
	hits          []model.BrainHit
	lastSearchIDs []int64
}

func (f *fakeStore) Brains(_ context.Context, _ int64) ([]*model.Brain, error) { return f.brains, nil }

func (f *fakeStore) Categories(_ context.Context, _, brainID int64) ([]*model.BrainCategory, error) {
	return f.categories[brainID], nil
}

func (f *fakeStore) Document(_ context.Context, _, id int64) (*model.BrainDocument, error) {
	d, ok := f.docs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return d, nil
}

func (f *fakeStore) Search(_ context.Context, _ int64, brainIDs []int64, _ string, _ int) ([]model.BrainHit, error) {
	f.lastSearchIDs = brainIDs
	return f.hits, nil
}

func fixture() *fakeStore {
	return &fakeStore{
		brains: []*model.Brain{
			// Locked: read-only, but still readable, and that is tested below.
			{ID: 1, Name: "Support Playbook", Slug: "support-playbook", Locked: true},
			{ID: 2, Name: "Secret Ops", Slug: "secret-ops"},
		},
		categories: map[int64][]*model.BrainCategory{
			1: {{ID: 11, BrainID: 1, Name: "Billing"}, {ID: 12, BrainID: 1, Name: "Onboarding"}},
			2: {{ID: 21, BrainID: 2, Name: "Internal"}},
		},
		docs: map[int64]*model.BrainDocument{
			10: {ID: 10, BrainID: 1, CategoryID: 11, Title: "Refunds", Content: "within 30 days",
				Related: []model.BrainLink{{ID: 13, Title: "Invoices", Category: "Billing"}}},
			13: {ID: 13, BrainID: 1, CategoryID: 11, Title: "Invoices", Content: "sent monthly"},
			20: {ID: 20, BrainID: 2, CategoryID: 21, Title: "Secret", Content: "hidden"},
		},
		hits: []model.BrainHit{{DocumentID: 10, BrainID: 1, Brain: "Support Playbook",
			Category: "Billing", Title: "Refunds", Snippet: "within 30 days", Score: 1.2}},
	}
}

func run(t *testing.T, h tool.Handler, args map[string]any) (map[string]any, tool.ErrorKind) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("encode args: %v", err)
	}
	res, err := h(context.Background(), tool.Call{WorkspaceID: 1, Args: raw})
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Content, &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return out, res.Err
}

// discover shows only the brains the agent was assigned, and marks the read-only
// ones. A brain it was not given is not in the list at all.
func TestBrainReadDiscoverScopesToAllowList(t *testing.T) {
	h := ReadHandler(fixture(), []int64{1})

	out, errKind := run(t, h, map[string]any{"operation": "discover"})
	if errKind != tool.ErrorNone {
		t.Fatalf("discover failed: %v", out)
	}
	brains, _ := out["brains"].([]any)
	if len(brains) != 1 {
		t.Fatalf("discover did not scope to the allow-list: %+v", brains)
	}
	first, _ := brains[0].(map[string]any)
	if first["brain"] != "Support Playbook" {
		t.Fatalf("the wrong brain was listed: %+v", first)
	}
	if first["read_only"] != true {
		t.Fatalf("a locked brain was not marked read-only: %+v", first)
	}
	cats, _ := first["categories"].([]any)
	if len(cats) != 2 {
		t.Fatalf("the brain's categories are missing: %+v", first)
	}
}

// search confines itself to the allow-list: with no brain named it searches all
// assigned, and a named brain narrows to that one. A brain the agent was not
// given cannot be named.
func TestBrainReadSearchConfinesToAllowList(t *testing.T) {
	store := fixture()
	h := ReadHandler(store, []int64{1})

	// No brain named: search every assigned brain (here, just brain 1).
	if _, errKind := run(t, h, map[string]any{"operation": "search", "query": "refund"}); errKind != tool.ErrorNone {
		t.Fatalf("search failed unexpectedly: %v", errKind)
	}
	if len(store.lastSearchIDs) != 1 || store.lastSearchIDs[0] != 1 {
		t.Fatalf("search did not pass the allow-list down: %+v", store.lastSearchIDs)
	}

	// A brain the agent WAS given, named: narrow to it.
	if _, errKind := run(t, h, map[string]any{"operation": "search", "query": "refund", "brain": "support-playbook"}); errKind != tool.ErrorNone {
		t.Fatalf("named search failed: %v", errKind)
	}
	if len(store.lastSearchIDs) != 1 || store.lastSearchIDs[0] != 1 {
		t.Fatalf("a named search did not narrow to that brain: %+v", store.lastSearchIDs)
	}

	// A brain the agent was NOT given: it does not exist to the agent.
	if _, errKind := run(t, h, map[string]any{"operation": "search", "query": "x", "brain": "Secret Ops"}); errKind != tool.ErrorBadArguments {
		t.Fatalf("a foreign brain was searchable: %v", errKind)
	}
	// A search with no query is a mistake it can correct.
	if _, errKind := run(t, h, map[string]any{"operation": "search"}); errKind != tool.ErrorBadArguments {
		t.Fatalf("an empty query was accepted: %v", errKind)
	}
}

// get opens a document only when it lives in a brain the agent may reach. A
// locked brain is readable; a foreign brain's document is not.
func TestBrainReadGetGuardsTheBrain(t *testing.T) {
	h := ReadHandler(fixture(), []int64{1})

	// A document in a LOCKED but assigned brain: readable, with its links.
	out, errKind := run(t, h, map[string]any{"operation": "get", "document": 10})
	if errKind != tool.ErrorNone {
		t.Fatalf("get on a locked-but-assigned document failed: %v", out)
	}
	doc, _ := out["document"].(map[string]any)
	if doc["content"] != "within 30 days" {
		t.Fatalf("the document content is wrong: %+v", doc)
	}
	related, _ := doc["related"].([]any)
	if len(related) != 1 {
		t.Fatalf("the related links are missing: %+v", doc)
	}

	// include_related previews each linked document.
	out, _ = run(t, h, map[string]any{"operation": "get", "document": 10, "include_related": true})
	doc, _ = out["document"].(map[string]any)
	related, _ = doc["related"].([]any)
	link, _ := related[0].(map[string]any)
	if link["preview"] != "sent monthly" {
		t.Fatalf("include_related did not preview the linked document: %+v", link)
	}

	// A document in a brain the agent was NOT given: not reachable.
	if _, errKind := run(t, h, map[string]any{"operation": "get", "document": 20}); errKind != tool.ErrorBadArguments {
		t.Fatalf("a foreign document was readable: %v", errKind)
	}
	// A document that does not exist.
	if _, errKind := run(t, h, map[string]any{"operation": "get", "document": 999}); errKind != tool.ErrorBadArguments {
		t.Fatalf("a missing document was not refused: %v", errKind)
	}
	// get without a document id.
	if _, errKind := run(t, h, map[string]any{"operation": "get"}); errKind != tool.ErrorBadArguments {
		t.Fatalf("get with no id was accepted: %v", errKind)
	}
}

// An empty allow-list reaches nothing: an agent with no brains assigned discovers
// none, searches none, and can open none. Fail-closed.
func TestBrainReadEmptyAllowListReachesNothing(t *testing.T) {
	store := fixture()
	h := ReadHandler(store, nil)

	out, _ := run(t, h, map[string]any{"operation": "discover"})
	if brains, _ := out["brains"].([]any); len(brains) != 0 {
		t.Fatalf("an agent with no brains discovered some: %+v", brains)
	}
	run(t, h, map[string]any{"operation": "search", "query": "x"})
	if len(store.lastSearchIDs) != 0 {
		t.Fatalf("search widened past the (empty) allow-list: %+v", store.lastSearchIDs)
	}
	if _, errKind := run(t, h, map[string]any{"operation": "get", "document": 10}); errKind != tool.ErrorBadArguments {
		t.Fatalf("a document was reachable with no brains assigned: %v", errKind)
	}
}

// An unusable operation is a mistake the model can correct, not a dead turn.
func TestBrainReadOperationValidation(t *testing.T) {
	h := ReadHandler(fixture(), []int64{1})

	if _, errKind := run(t, h, map[string]any{"operation": "delete_everything"}); errKind != tool.ErrorBadArguments {
		t.Fatalf("an unknown operation was accepted: %v", errKind)
	}
	if _, errKind := run(t, h, map[string]any{}); errKind != tool.ErrorBadArguments {
		t.Fatalf("a missing operation was accepted: %v", errKind)
	}

	// Malformed JSON is a bad-arguments result, not a crash.
	res, err := h(context.Background(), tool.Call{WorkspaceID: 1, Args: json.RawMessage(`{"operation":`)})
	if err != nil {
		t.Fatalf("malformed args returned a Go error: %v", err)
	}
	if res.Err != tool.ErrorBadArguments {
		t.Fatalf("malformed args were not classified as bad arguments: %v", res.Err)
	}
}
