package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"flexie.io/sag/internal/model"
)

// The knowledge base, over HTTP. What matters here is that the tree is
// addressable the way the console navigates it, and that the graph the API
// reports back is the graph the store actually made.

func TestBrainsAreCuratedThroughTheAPI(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// A brain.
	rec := env.do(http.MethodPost, "/v1/brains", token, map[string]any{
		"name": "Product Manual", "description": "How the product works.", "locked": false,
	})
	env.expectStatus(rec, http.StatusCreated)
	var brain model.Brain
	env.decode(rec, &brain)
	if brain.Slug != "product-manual" {
		t.Fatalf("the brain has no usable handle: %q", brain.Slug)
	}

	// A category in it.
	rec = env.do(http.MethodPost, fmt.Sprintf("/v1/brains/%d/categories", brain.ID), token,
		map[string]any{"name": "Billing"})
	env.expectStatus(rec, http.StatusCreated)
	var category model.BrainCategory
	env.decode(rec, &category)

	// Two documents, one relating to the other.
	refunds := env.createDocument(token, category.ID, "Refunds",
		"A refund is possible within thirty days.", nil)
	invoices := env.createDocument(token, category.ID, "Invoices",
		"Invoices are sent monthly.", []int64{refunds.ID})

	// The relation is SYMMETRIC, and the API reports the graph as it now is
	// rather than echoing what was sent.
	if len(invoices.Related) != 1 || invoices.Related[0].ID != refunds.ID {
		t.Fatalf("the relation was not made: %+v", invoices.Related)
	}

	rec = env.do(http.MethodGet, fmt.Sprintf("/v1/brain-documents/%d", refunds.ID), token, nil)
	env.expectStatus(rec, http.StatusOK)
	var back model.BrainDocument
	env.decode(rec, &back)
	if len(back.Related) != 1 || back.Related[0].ID != invoices.ID {
		t.Fatalf("the relation is one-directional, so the document is lost from the other side: %+v", back.Related)
	}

	// The list of a category carries no content: a category may hold hundreds of
	// documents and nobody is reading them yet.
	rec = env.do(http.MethodGet, fmt.Sprintf("/v1/brain-categories/%d/documents", category.ID), token, nil)
	env.expectStatus(rec, http.StatusOK)
	var listed []model.BrainDocument
	env.decode(rec, &listed)
	if len(listed) != 2 {
		t.Fatalf("expected both documents, got %d", len(listed))
	}
	for _, d := range listed {
		if d.Content != "" {
			t.Fatalf("the list carried a whole document body: %q", d.Title)
		}
	}

	// Search says WHY a document matched.
	rec = env.do(http.MethodGet, "/v1/brains/search?q=refund", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var hits []model.BrainHit
	env.decode(rec, &hits)
	if len(hits) != 1 || hits[0].Title != "Refunds" {
		t.Fatalf("the search did not find it: %+v", hits)
	}
	if hits[0].Snippet == "" {
		t.Fatal("the hit has no snippet, so nobody can tell why it came back")
	}

	// Deleting the brain takes the tree with it.
	rec = env.do(http.MethodDelete, fmt.Sprintf("/v1/brains/%d", brain.ID), token, nil)
	env.expectStatus(rec, http.StatusNoContent)

	rec = env.do(http.MethodGet, fmt.Sprintf("/v1/brain-documents/%d", refunds.ID), token, nil)
	env.expectStatus(rec, http.StatusNotFound)
}

// Curating the knowledge an agent reads is not a small act: it changes what every
// agent that reads it believes.
func TestBrainsRequirePermission(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("reader@acme.test", "dev-Passw0rd!", model.PermBrainsView)
	reader, _ := env.login("reader@acme.test", "dev-Passw0rd!")
	env.createUser("curator@acme.test", "dev-Passw0rd!", model.PermBrainsView, model.PermBrainsEdit)
	curator, _ := env.login("curator@acme.test", "dev-Passw0rd!")
	env.createUser("maker@acme.test", "dev-Passw0rd!", model.PermBrainsCreate, model.PermBrainsDelete)
	maker, _ := env.login("maker@acme.test", "dev-Passw0rd!")
	env.createUser("nobody@acme.test", "dev-Passw0rd!")
	nobody, _ := env.login("nobody@acme.test", "dev-Passw0rd!")

	// A reader may look.
	env.expectStatus(env.do(http.MethodGet, "/v1/brains", reader, nil), http.StatusOK)
	// And may not write.
	env.expectStatus(env.do(http.MethodPost, "/v1/brains", reader,
		map[string]any{"name": "Mine"}), http.StatusForbidden)

	// Edit is the inside of a brain, not making or destroying brains: a
	// curator is refused both, but reaches an update (404 proves the
	// permission gate opened and only the id was wrong).
	env.expectStatus(env.do(http.MethodPost, "/v1/brains", curator,
		map[string]any{"name": "Mine"}), http.StatusForbidden)
	env.expectStatus(env.do(http.MethodDelete, "/v1/brains/999999", curator, nil), http.StatusForbidden)
	env.expectStatus(env.do(http.MethodPut, "/v1/brains/999999", curator,
		map[string]any{"name": "Renamed"}), http.StatusNotFound)

	// Create and delete are their own grants.
	rec := env.do(http.MethodPost, "/v1/brains", maker, map[string]any{"name": "Mine"})
	env.expectStatus(rec, http.StatusCreated)
	var created model.Brain
	env.decode(rec, &created)
	env.expectStatus(env.do(http.MethodDelete, fmt.Sprintf("/v1/brains/%d", created.ID), maker, nil),
		http.StatusNoContent)

	// Somebody with none of it sees nothing at all.
	env.expectStatus(env.do(http.MethodGet, "/v1/brains", nobody, nil), http.StatusForbidden)
}

// A brain belongs to its workspace. Knowing an id is not a way into another one.
func TestABrainFromAnotherWorkspaceIsNotThere(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	other := &model.Workspace{Slug: "globex", Name: "Globex"}
	if err := env.app.Store.Workspaces().Create(t.Context(), other); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	theirs := &model.Brain{WorkspaceID: other.ID, Name: "Theirs"}
	if err := env.app.Store.Brains().CreateBrain(t.Context(), theirs); err != nil {
		t.Fatalf("create brain: %v", err)
	}

	env.expectStatus(env.do(http.MethodGet, fmt.Sprintf("/v1/brains/%d", theirs.ID), token, nil),
		http.StatusNotFound)
	env.expectStatus(env.do(http.MethodDelete, fmt.Sprintf("/v1/brains/%d", theirs.ID), token, nil),
		http.StatusNotFound)

	rec := env.do(http.MethodGet, "/v1/brains", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var listed []model.Brain
	env.decode(rec, &listed)
	if len(listed) != 0 {
		t.Fatalf("another workspace's brain was listed here: %+v", listed)
	}
}

func (e *testEnv) createDocument(token string, categoryID int64, title, content string, related []int64) model.BrainDocument {
	e.t.Helper()
	rec := e.do(http.MethodPost, fmt.Sprintf("/v1/brain-categories/%d/documents", categoryID), token,
		map[string]any{"title": title, "content": content, "related": related})
	e.expectStatus(rec, http.StatusCreated)

	var document model.BrainDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		e.t.Fatalf("decode document: %v", err)
	}
	return document
}

// The console draws three panes from ONE answer. What it must never have to do
// is ask which brains exist, pick the first, ask for its categories, pick the
// first of those, and so on: that is a waterfall, and the person watching it
// sees the columns fill in one at a time.
func TestBrainViewAnswersTheWholeScreenAtOnce(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// Nothing exists yet: the answer is empty, not an error. A console with no
	// brains still has to draw itself.
	rec := env.do(http.MethodGet, "/v1/brains/view", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var empty brainView
	env.decode(rec, &empty)
	if len(empty.Brains) != 0 || empty.BrainID != 0 || empty.Document != nil {
		t.Fatalf("an empty knowledge base is not an empty answer: %+v", empty)
	}

	first := env.brain(token, "Platform")
	second := env.brain(token, "Sales")
	onboarding := env.category(token, first, "Onboarding")
	deploys := env.category(token, first, "Deploys")
	welcome := env.document(token, onboarding, "Welcome")
	env.document(token, onboarding, "Second Day")
	rollback := env.document(token, deploys, "Rollback")

	// Asking for nothing gets the first of everything, all the way down. "First"
	// means first in the order the panes are drawn in, not first created: what
	// opens must be the row at the top of the list, or the screen contradicts
	// itself.
	rec = env.do(http.MethodGet, "/v1/brains/view", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var view brainView
	env.decode(rec, &view)
	if len(view.Brains) != 2 || view.BrainID != view.Brains[0].ID || view.BrainID != first {
		t.Fatalf("the first brain was not opened: %+v", view)
	}
	if len(view.Categories) != 2 || view.CategoryID != view.Categories[0].ID {
		t.Fatalf("the first category was not opened: %+v", view)
	}
	if len(view.Documents) == 0 || view.DocumentID != view.Documents[0].ID {
		t.Fatalf("the first document was not opened: %+v", view)
	}
	// And the open document arrives WITH its content: a second request to read
	// the body would be the waterfall again, one step shorter. The listed ones
	// deliberately carry none.
	if view.Document == nil || view.Document.ID != view.DocumentID || view.Document.Content == "" {
		t.Fatalf("the open document came back without its content: %+v", view.Document)
	}

	// A selection deeper in the tree is honoured exactly.
	rec = env.do(http.MethodGet,
		fmt.Sprintf("/v1/brains/view?brain=%d&category=%d&document=%d", first, deploys, rollback),
		token, nil)
	env.expectStatus(rec, http.StatusOK)
	env.decode(rec, &view)
	if view.CategoryID != deploys || view.DocumentID != rollback || view.Document.ID != rollback {
		t.Fatalf("the asked-for selection was not honoured: %+v", view)
	}

	// A selection that names things which do not belong together is a stale
	// link, not an error: the category is not in that brain, so the first one
	// that is answers, and the document follows it.
	rec = env.do(http.MethodGet,
		fmt.Sprintf("/v1/brains/view?brain=%d&category=%d&document=%d", second, onboarding, welcome),
		token, nil)
	env.expectStatus(rec, http.StatusOK)
	env.decode(rec, &view)
	if view.BrainID != second || view.CategoryID != 0 || view.Document != nil {
		t.Fatalf("a stale selection must land somewhere real: %+v", view)
	}

	// The same for a brain that is gone.
	rec = env.do(http.MethodGet, "/v1/brains/view?brain=999999", token, nil)
	env.expectStatus(rec, http.StatusOK)
	env.decode(rec, &view)
	if view.BrainID != first || view.Document == nil {
		t.Fatalf("a deleted brain in a bookmark must open the first one: %+v", view)
	}

	// A document asked for in the wrong category is the same stale link: the
	// category answers, and its own first document opens.
	rec = env.do(http.MethodGet,
		fmt.Sprintf("/v1/brains/view?brain=%d&category=%d&document=%d", first, deploys, welcome),
		token, nil)
	env.expectStatus(rec, http.StatusOK)
	env.decode(rec, &view)
	if view.CategoryID != deploys || view.DocumentID != rollback {
		t.Fatalf("a document from another category must not open: %+v", view)
	}
}

// brainView mirrors app.BrainOverview on the wire.
type brainView struct {
	Brains     []*model.Brain         `json:"brains"`
	BrainID    int64                  `json:"brain_id"`
	Categories []*model.BrainCategory `json:"categories"`
	CategoryID int64                  `json:"category_id"`
	Documents  []*model.BrainDocument `json:"documents"`
	DocumentID int64                  `json:"document_id"`
	Document   *model.BrainDocument   `json:"document"`
}

func (e *testEnv) brain(token, name string) int64 {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/v1/brains", token, map[string]any{"name": name})
	e.expectStatus(rec, http.StatusCreated)
	var brain model.Brain
	e.decode(rec, &brain)
	return brain.ID
}

func (e *testEnv) category(token string, brainID int64, name string) int64 {
	e.t.Helper()
	rec := e.do(http.MethodPost, fmt.Sprintf("/v1/brains/%d/categories", brainID), token,
		map[string]any{"name": name})
	e.expectStatus(rec, http.StatusCreated)
	var category model.BrainCategory
	e.decode(rec, &category)
	return category.ID
}

func (e *testEnv) document(token string, categoryID int64, title string) int64 {
	e.t.Helper()
	return e.createDocument(token, categoryID, title, "# "+title, nil).ID
}

func TestDocumentFieldErrors(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	support := env.brain(token, "Support")
	refunds := env.category(token, support, "Refunds")
	policy := env.document(token, refunds, "Policy")

	// A second document with the same title in the same category collides on
	// the title field.
	env.expectFields(env.do(http.MethodPost, fmt.Sprintf("/v1/brain-categories/%d/documents", refunds),
		token, map[string]any{"title": "Policy", "content": "again"},
	), http.StatusConflict, "conflict", map[string]string{
		"title": "this category already has a document with this title",
	})

	// Moving a document into another brain's category is refused on the
	// category field: a brain is a closed graph.
	sales := env.brain(token, "Sales")
	deals := env.category(token, sales, "Deals")
	env.expectFields(env.do(http.MethodPut, fmt.Sprintf("/v1/brain-documents/%d", policy),
		token, map[string]any{"title": "Policy", "content": "# Policy", "category_id": deals},
	), http.StatusBadRequest, "invalid_request", map[string]string{
		"category_id": "a document cannot move to another brain",
	})

	// A blank title is refused before anything is written.
	env.expectFields(env.do(http.MethodPost, fmt.Sprintf("/v1/brain-categories/%d/documents", refunds),
		token, map[string]any{"title": "  ", "content": "body"},
	), http.StatusBadRequest, "invalid_request", map[string]string{
		"title": "a document needs a title",
	})
}
