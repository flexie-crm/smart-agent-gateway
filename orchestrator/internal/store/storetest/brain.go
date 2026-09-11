package storetest

import (
	"errors"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// Brains: a curated knowledge base the agent navigates and writes back to.
//
// Two rules make it a knowledge base rather than a pile of text, and both are
// tested for what they DO and for what they must refuse:
//
//   - a LOCKED brain is read-only to the agent while an administrator still
//     edits it by hand;
//   - the graph is SYMMETRIC and confined to one brain, because a document
//     reachable from one side and invisible from the other is lost.

func brainWith(t *testing.T, st store.Store, wsID int64, name string) *model.Brain {
	t.Helper()
	b := &model.Brain{WorkspaceID: wsID, Name: name}
	if err := st.Brains().CreateBrain(ctx(), b); err != nil {
		t.Fatalf("create brain: %v", err)
	}
	return b
}

func categoryWith(t *testing.T, st store.Store, wsID int64, brain *model.Brain, name string) *model.BrainCategory {
	t.Helper()
	c := &model.BrainCategory{BrainID: brain.ID, Name: name}
	if err := st.Brains().CreateCategory(ctx(), wsID, c); err != nil {
		t.Fatalf("create category: %v", err)
	}
	return c
}

func documentWith(t *testing.T, st store.Store, wsID int64, category *model.BrainCategory, title, content string, related ...int64) *model.BrainDocument {
	t.Helper()
	d := &model.BrainDocument{CategoryID: category.ID, Title: title, Content: content}
	if err := st.Brains().SaveDocument(ctx(), wsID, d, related); err != nil {
		t.Fatalf("save document: %v", err)
	}
	return d
}

func testBrainTree(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")

	brain := brainWith(t, st, ws.ID, "Product Manual")
	if brain.Slug != "product-manual" {
		t.Fatalf("the brain has no usable handle: %q", brain.Slug)
	}

	billing := categoryWith(t, st, ws.ID, brain, "Billing")
	documentWith(t, st, ws.ID, billing, "Refunds", "A refund is possible within 30 days.")

	// The list says how much is in it. A brain with nothing in it looks exactly
	// like a brain with a thousand documents until it says so.
	brains, err := st.Brains().Brains(ctx(), ws.ID)
	if err != nil {
		t.Fatalf("list brains: %v", err)
	}
	if len(brains) != 1 || brains[0].Categories != 1 || brains[0].Documents != 1 {
		t.Fatalf("the counts are wrong: %+v", brains)
	}

	// A category lists its documents WITHOUT their bodies: hundreds of documents
	// of Markdown that nobody is reading is a list that hangs.
	documents, err := st.Brains().Documents(ctx(), ws.ID, billing.ID)
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	if len(documents) != 1 || documents[0].Title != "Refunds" {
		t.Fatalf("the document is missing: %+v", documents)
	}
	if documents[0].Content != "" {
		t.Fatal("the list carried the whole document body")
	}
	// A document with no links lists an EMPTY related array, never a null: a caller
	// that maps over related must not meet a null where the shape promises an array.
	if documents[0].Related == nil {
		t.Fatalf("a linkless document has a null related list, not an empty one: %+v", documents[0])
	}

	// Opening one gives you the content.
	document, err := st.Brains().Document(ctx(), ws.ID, documents[0].ID)
	if err != nil {
		t.Fatalf("open document: %v", err)
	}
	if document.Content == "" {
		t.Fatal("the opened document has no content")
	}

	// Deleting the brain takes the whole tree, and the database enforces it.
	if err := st.Brains().DeleteBrain(ctx(), ws.ID, brain.ID); err != nil {
		t.Fatalf("delete brain: %v", err)
	}
	if _, err := st.Brains().Document(ctx(), ws.ID, document.ID); err == nil {
		t.Fatal("a document outlived the brain it was in")
	}
}

// DocumentByTitle locates a document by its title within a category: the primitive
// that lets a write update an existing document instead of colliding on the unique
// (category, title) key. It is what makes "told the same thing twice, it updates"
// true, rather than a duplicate or a refusal.
func testBrainDocumentByTitle(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	brain := brainWith(t, st, ws.ID, "Manual")
	billing := categoryWith(t, st, ws.ID, brain, "Billing")
	onboarding := categoryWith(t, st, ws.ID, brain, "Onboarding")

	refunds := documentWith(t, st, ws.ID, billing, "Refunds", "within 30 days")

	// Found within its own category.
	got, err := st.Brains().DocumentByTitle(ctx(), ws.ID, billing.ID, "Refunds")
	if err != nil {
		t.Fatalf("locate by title: %v", err)
	}
	if got.ID != refunds.ID {
		t.Fatalf("located the wrong document: got %d, want %d", got.ID, refunds.ID)
	}

	// The title is unique PER CATEGORY: the same title under another category is a
	// different document, so this one is not found there.
	if _, err := st.Brains().DocumentByTitle(ctx(), ws.ID, onboarding.ID, "Refunds"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a title leaked across categories: %v", err)
	}
	// A title nobody wrote is simply not found.
	if _, err := st.Brains().DocumentByTitle(ctx(), ws.ID, billing.ID, "Nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unwritten title was found: %v", err)
	}

	// Why the primitive exists: a naive create of the same title collides on the
	// unique key rather than silently updating.
	dup := &model.BrainDocument{CategoryID: billing.ID, Title: "Refunds", Content: "again"}
	if err := st.Brains().SaveDocument(ctx(), ws.ID, dup, nil); err == nil {
		t.Fatal("a duplicate title was created rather than refused")
	}

	// The point in use: resolve the id first, then save through it, and the one
	// document is updated in place.
	existing, err := st.Brains().DocumentByTitle(ctx(), ws.ID, billing.ID, "Refunds")
	if err != nil {
		t.Fatalf("re-locate: %v", err)
	}
	if err := st.Brains().SaveDocument(ctx(), ws.ID,
		&model.BrainDocument{ID: existing.ID, CategoryID: billing.ID, Title: "Refunds", Content: "within 14 days"},
		nil); err != nil {
		t.Fatalf("idempotent save: %v", err)
	}
	docs, err := st.Brains().Documents(ctx(), ws.ID, billing.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("the same title was saved twice as two documents: %+v", docs)
	}
	reopened, err := st.Brains().Document(ctx(), ws.ID, refunds.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if reopened.Content != "within 14 days" {
		t.Fatalf("the update did not take: %q", reopened.Content)
	}
}

// The graph is symmetric. A link the reader can follow one way and not the other
// is a document that is findable from one place and lost from every other.
func testBrainGraphIsSymmetric(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	brain := brainWith(t, st, ws.ID, "Manual")
	category := categoryWith(t, st, ws.ID, brain, "Billing")

	refunds := documentWith(t, st, ws.ID, category, "Refunds", "within 30 days")
	invoices := documentWith(t, st, ws.ID, category, "Invoices", "sent monthly")

	// Say it once, in one direction.
	if err := st.Brains().SaveDocument(ctx(), ws.ID, refunds, []int64{invoices.ID}); err != nil {
		t.Fatalf("link: %v", err)
	}

	// It is true in both.
	forward, err := st.Brains().Document(ctx(), ws.ID, refunds.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(forward.Related) != 1 || forward.Related[0].ID != invoices.ID {
		t.Fatalf("the link was not made: %+v", forward.Related)
	}

	back, err := st.Brains().Document(ctx(), ws.ID, invoices.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(back.Related) != 1 || back.Related[0].ID != refunds.ID {
		t.Fatalf("the link is one-directional, so the document is lost from the other side: %+v", back.Related)
	}

	// Removing it removes both directions.
	forward.Related = nil
	if err := st.Brains().SaveDocument(ctx(), ws.ID, forward, nil); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	back, err = st.Brains().Document(ctx(), ws.ID, invoices.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(back.Related) != 0 {
		t.Fatalf("the other side still claims a link that was removed: %+v", back.Related)
	}
}

// A link may only join documents in the same brain. Otherwise an agent allowed
// one brain could walk out of it, through a document it was allowed, into a brain
// it was not.
func testBrainGraphCannotLeaveItsBrain(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")

	ours := brainWith(t, st, ws.ID, "Public")
	theirs := brainWith(t, st, ws.ID, "Secret")
	here := categoryWith(t, st, ws.ID, ours, "Here")
	there := categoryWith(t, st, ws.ID, theirs, "There")

	open := documentWith(t, st, ws.ID, here, "Open", "anyone may read this")
	secret := documentWith(t, st, ws.ID, there, "Secret", "nobody may read this")

	// Ask for the link anyway.
	if err := st.Brains().SaveDocument(ctx(), ws.ID, open, []int64{secret.ID}); err != nil {
		t.Fatalf("save: %v", err)
	}

	linked, err := st.Brains().Document(ctx(), ws.ID, open.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(linked.Related) != 0 {
		t.Fatalf("a document was linked out of its brain: %+v", linked.Related)
	}
}

// Search returns the sentence that MATCHED. The agent has to decide whether a
// document is worth opening, and a title alone is not something you can decide
// with.
func testBrainSearch(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	brain := brainWith(t, st, ws.ID, "Manual")
	category := categoryWith(t, st, ws.ID, brain, "Billing")

	documentWith(t, st, ws.ID, category, "Refunds",
		"Our policy is simple. A customer may request a refund within thirty days of purchase, "+
			"and the money returns to the original card.")
	documentWith(t, st, ws.ID, category, "Shipping", "Orders leave the warehouse within two days.")

	hits, err := st.Brains().Search(ctx(), ws.ID, []int64{brain.ID}, "refund", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].Title != "Refunds" {
		t.Fatalf("the search did not find the document: %+v", hits)
	}
	if hits[0].Snippet == "" {
		t.Fatal("the hit has no snippet, so nobody can tell why it matched")
	}
	if !strings.Contains(strings.ToLower(hits[0].Snippet), "refund") {
		t.Fatalf("the snippet is not where the match was: %q", hits[0].Snippet)
	}
	if hits[0].Brain != "Manual" || hits[0].Category != "Billing" {
		t.Fatalf("the hit does not say where it lives: %+v", hits[0])
	}

	// THE SECURITY SPINE: search is confined to the brains it was given. An agent
	// with no brains assigned reaches nothing at all.
	none, err := st.Brains().Search(ctx(), ws.ID, nil, "refund", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(none) != 0 {
		t.Fatal("an empty allow-list found something, so the allow-list is not one")
	}

	other := brainWith(t, st, ws.ID, "Other")
	elsewhere, err := st.Brains().Search(ctx(), ws.ID, []int64{other.ID}, "refund", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(elsewhere) != 0 {
		t.Fatalf("a search of one brain returned another brain's documents: %+v", elsewhere)
	}
}

// The allow-list is what an agent may reach, and it cannot be pointed at a brain
// from another workspace by knowing its id.
func testAgentBrains(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	other := mustWorkspace(t, st, "globex")

	ours := brainWith(t, st, ws.ID, "Ours")
	theirs := brainWith(t, st, other.ID, "Theirs")

	// The agent is created already naming both brains: the one from another
	// workspace must not stick (the config store writes agent_brains).
	agent := &model.Agent{
		WorkspaceID: ws.ID, Key: model.DefaultAgentKey, Name: "House",
		Brains: []int64{ours.ID, theirs.ID},
	}
	if err := st.Agents().Create(ctx(), agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	assigned, err := st.Brains().AgentBrains(ctx(), ws.ID, agent.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(assigned) != 1 || assigned[0].ID != ours.ID {
		t.Fatalf("an agent was assigned a brain from another workspace: %+v", assigned)
	}

	// The config store reads the ids back onto the agent, for the form to prefill.
	loaded, err := st.Agents().GetByID(ctx(), ws.ID, agent.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if len(loaded.Brains) != 1 || loaded.Brains[0] != ours.ID {
		t.Fatalf("the assigned brains did not round-trip: %+v", loaded.Brains)
	}

	// Replaced wholesale, so revoking is possible: an empty (not nil) list clears.
	loaded.Brains = []int64{}
	if err := st.Agents().Update(ctx(), loaded); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	assigned, err = st.Brains().AgentBrains(ctx(), ws.ID, agent.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(assigned) != 0 {
		t.Fatalf("a revoked brain is still assigned: %+v", assigned)
	}
}

// A brain is a separate CONTEXT. A document cannot move out of it: carrying one
// across would either drag its links out of the brain or silently cut them, and
// both are worse than refusing. Moving between categories of the SAME brain is
// the only move there is.
func testADocumentCannotMoveBetweenBrains(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")

	manual := brainWith(t, st, ws.ID, "Manual")
	playbook := brainWith(t, st, ws.ID, "Playbook")

	billing := categoryWith(t, st, ws.ID, manual, "Billing")
	shipping := categoryWith(t, st, ws.ID, manual, "Shipping")
	elsewhere := categoryWith(t, st, ws.ID, playbook, "Elsewhere")

	refunds := documentWith(t, st, ws.ID, billing, "Refunds", "within 30 days")
	invoices := documentWith(t, st, ws.ID, billing, "Invoices", "sent monthly")
	if err := st.Brains().SaveDocument(ctx(), ws.ID, refunds, []int64{invoices.ID}); err != nil {
		t.Fatalf("link: %v", err)
	}

	// Within the brain, a move is fine, and the graph survives it.
	refunds.CategoryID = shipping.ID
	if err := st.Brains().SaveDocument(ctx(), ws.ID, refunds, []int64{invoices.ID}); err != nil {
		t.Fatalf("a document could not move between categories of its own brain: %v", err)
	}
	moved, err := st.Brains().Document(ctx(), ws.ID, refunds.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if moved.CategoryID != shipping.ID {
		t.Fatal("the document did not move")
	}
	if len(moved.Related) != 1 {
		t.Fatalf("moving inside the brain cut the graph: %+v", moved.Related)
	}

	// Out of the brain, it is refused.
	moved.CategoryID = elsewhere.ID
	err = st.Brains().SaveDocument(ctx(), ws.ID, moved, nil)
	if err == nil {
		t.Fatal("a document was carried into another brain")
	}
	if !errors.Is(err, store.ErrWrongBrain) {
		t.Fatalf("the refusal should name the brain boundary, got: %v", err)
	}

	// And it is still where it was, with its links.
	unmoved, err := st.Brains().Document(ctx(), ws.ID, refunds.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if unmoved.BrainID != manual.ID || len(unmoved.Related) != 1 {
		t.Fatalf("the refused move damaged the document: %+v", unmoved)
	}
}
