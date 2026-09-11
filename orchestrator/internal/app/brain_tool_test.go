package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/brain"
)

// The brain read tool, resolved through a REAL loadout, is bound to exactly the
// brains it is given and nothing else. This is the wiring the unit test cannot
// see: the tool loads with an empty allow-list, and Loadout rebinds it.
func TestBrainToolIsScopedThroughTheLoadout(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("nobody@acme.test")

	// Two brains; the agent will be given only one of them.
	mine := &model.Brain{WorkspaceID: e.ws.ID, Name: "Mine"}
	if err := e.app.Store.Brains().CreateBrain(ctx, mine); err != nil {
		t.Fatalf("create brain: %v", err)
	}
	theirs := &model.Brain{WorkspaceID: e.ws.ID, Name: "Theirs"}
	if err := e.app.Store.Brains().CreateBrain(ctx, theirs); err != nil {
		t.Fatalf("create brain: %v", err)
	}

	mineDoc := brainDoc(t, e, mine.ID, "Billing", "Refunds", "within 30 days")
	theirDoc := brainDoc(t, e, theirs.ID, "Secret", "Hidden", "nope")

	// A loadout for an agent assigned ONLY "Mine".
	loadout, err := e.app.Loadout(ctx, e.ws.ID, user.ID, "", []string{brain.ReadName}, nil, []int64{mine.ID}, tool.OwnerOfAgent())
	if err != nil {
		t.Fatalf("loadout: %v", err)
	}
	h, ok := loadout.Handlers[brain.ReadName]
	if !ok {
		t.Fatal("the brain tool did not reach the loadout")
	}

	// discover sees only the assigned brain.
	out := decodeBrain(t, callBrain(t, h, e.ws.ID, map[string]any{"operation": "discover"}))
	if brains, _ := out["brains"].([]any); len(brains) != 1 {
		t.Fatalf("the brain tool was not scoped to the assignment: %+v", brains)
	}

	// The assigned document opens; the one in the unassigned brain does not exist.
	if got := callBrain(t, h, e.ws.ID, map[string]any{"operation": "get", "document": mineDoc}); got.Err != tool.ErrorNone {
		t.Fatalf("the assigned document could not be read: %v", got.Err)
	}
	if got := callBrain(t, h, e.ws.ID, map[string]any{"operation": "get", "document": theirDoc}); got.Err != tool.ErrorBadArguments {
		t.Fatalf("a document outside the assignment was reachable: %v", got.Err)
	}
}

// The write tool, resolved through a real loadout, honours the lock and the
// allow-list, keeps its idempotency promise, deletes, and hands the loop a
// pre-park validator that refuses a bad write before any card.
func TestBrainWriteToolThroughTheLoadout(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("nobody@acme.test")

	writable := &model.Brain{WorkspaceID: e.ws.ID, Name: "Notes"}
	if err := e.app.Store.Brains().CreateBrain(ctx, writable); err != nil {
		t.Fatalf("create brain: %v", err)
	}
	locked := &model.Brain{WorkspaceID: e.ws.ID, Name: "Policy", Locked: true}
	if err := e.app.Store.Brains().CreateBrain(ctx, locked); err != nil {
		t.Fatalf("create brain: %v", err)
	}

	loadout, err := e.app.Loadout(ctx, e.ws.ID, user.ID, "", []string{brain.WriteName}, nil, []int64{writable.ID, locked.ID}, tool.OwnerOfAgent())
	if err != nil {
		t.Fatalf("loadout: %v", err)
	}
	h, ok := loadout.Handlers[brain.WriteName]
	if !ok {
		t.Fatal("the write tool did not reach the loadout")
	}
	v, ok := loadout.Validators[brain.WriteName]
	if !ok {
		t.Fatal("the write tool has no validator in the loadout")
	}

	// A read-only brain refuses a write BEFORE any card: the validator says no,
	// and so does the handler.
	lockedWrite := map[string]any{"operation": "save_category", "brain": "Policy", "name": "Nope"}
	if val := validateWrite(t, v, e.ws.ID, lockedWrite); val.OK {
		t.Fatal("the validator approved a write to a read-only brain")
	}
	if res := callBrain(t, h, e.ws.ID, lockedWrite); res.Err != tool.ErrorBadArguments {
		t.Fatalf("the handler wrote to a read-only brain: %v", res.Err)
	}

	// A brain the agent was not assigned does not exist to it.
	if res := callBrain(t, h, e.ws.ID, map[string]any{"operation": "save_category", "brain": "Ghost", "name": "X"}); res.Err != tool.ErrorBadArguments {
		t.Fatalf("a write to an unassigned brain was allowed: %v", res.Err)
	}

	// A document into a category that does not exist is refused (create it first).
	if res := callBrain(t, h, e.ws.ID, map[string]any{"operation": "save_document", "brain": "Notes", "category": "Ideas", "title": "T", "content": "c"}); res.Err != tool.ErrorBadArguments {
		t.Fatalf("a document went into a category that does not exist: %v", res.Err)
	}
	if res := callBrain(t, h, e.ws.ID, map[string]any{"operation": "save_category", "brain": "Notes", "name": "Ideas"}); res.Err != tool.ErrorNone {
		t.Fatalf("save_category failed: %v", res.Err)
	}

	// The validator hands back per-call card copy that names what will happen.
	saveDoc := map[string]any{"operation": "save_document", "brain": "Notes", "category": "Ideas", "title": "Refunds", "content": "within 30 days"}
	val := validateWrite(t, v, e.ws.ID, saveDoc)
	if !val.OK || val.ApprovalTitle == "" || !strings.Contains(val.ApprovalPrompt, "Refunds") || !strings.Contains(val.ApprovalPrompt, "Notes") {
		t.Fatalf("the write's card copy did not name the write: %+v", val)
	}

	// The document is created, then the same title UPDATES in place.
	first := decodeBrain(t, callBrain(t, h, e.ws.ID, saveDoc))
	if first["saved"] != "document" {
		t.Fatalf("save_document did not report a document: %+v", first)
	}
	update := map[string]any{"operation": "save_document", "brain": "Notes", "category": "Ideas", "title": "Refunds", "content": "within 14 days"}
	second := decodeBrain(t, callBrain(t, h, e.ws.ID, update))
	if second["id"] != first["id"] {
		t.Fatalf("a repeated title created a second document: %v vs %v", second["id"], first["id"])
	}
	ideas := categoryNamed(t, e, writable.ID, "Ideas")
	if docs, _ := e.app.Store.Brains().Documents(ctx, e.ws.ID, ideas.ID); len(docs) != 1 {
		t.Fatalf("the update duplicated the document: %+v", docs)
	}

	// import adds several; re-importing the same titles updates them.
	imp := map[string]any{"operation": "import", "brain": "Notes", "category": "Ideas", "markdown": "# Alpha\none\n# Beta\ntwo"}
	if res := decodeBrain(t, callBrain(t, h, e.ws.ID, imp)); res["created"] != float64(2) {
		t.Fatalf("import did not create two documents: %+v", res)
	}
	if res := decodeBrain(t, callBrain(t, h, e.ws.ID, imp)); res["updated"] != float64(2) || res["created"] != float64(0) {
		t.Fatalf("re-import duplicated instead of updating: %+v", res)
	}

	// delete_document removes it; delete_category takes the category and its rest.
	alpha := documentNamed(t, e, ideas.ID, "Alpha")
	if res := callBrain(t, h, e.ws.ID, map[string]any{"operation": "delete_document", "document": alpha}); res.Err != tool.ErrorNone {
		t.Fatalf("delete_document failed: %v", res.Err)
	}
	if _, err := e.app.Store.Brains().Document(ctx, e.ws.ID, alpha); err == nil {
		t.Fatal("the document survived its delete")
	}
	if res := callBrain(t, h, e.ws.ID, map[string]any{"operation": "delete_category", "brain": "Notes", "category": "Ideas"}); res.Err != tool.ErrorNone {
		t.Fatalf("delete_category failed: %v", res.Err)
	}
	if cats, _ := e.app.Store.Brains().Categories(ctx, e.ws.ID, writable.ID); len(cats) != 0 {
		t.Fatalf("the category survived its delete: %+v", cats)
	}
}

func validateWrite(t *testing.T, v tool.Validator, ws int64, args map[string]any) tool.Validation {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("encode args: %v", err)
	}
	return v(context.Background(), tool.Call{WorkspaceID: ws, Args: raw})
}

func categoryNamed(t *testing.T, e *env, brainID int64, name string) *model.BrainCategory {
	t.Helper()
	cats, err := e.app.Store.Brains().Categories(context.Background(), e.ws.ID, brainID)
	if err != nil {
		t.Fatalf("list categories: %v", err)
	}
	for _, c := range cats {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("category %q not found", name)
	return nil
}

func documentNamed(t *testing.T, e *env, categoryID int64, title string) int64 {
	t.Helper()
	docs, err := e.app.Store.Brains().Documents(context.Background(), e.ws.ID, categoryID)
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	for _, d := range docs {
		if d.Title == title {
			return d.ID
		}
	}
	t.Fatalf("document %q not found", title)
	return 0
}

// The memory tool is the agent's own memory brain: present only when one is
// assigned, internal and never approval-gated, and it self-organises (creates
// its categories, updates by title).
func TestMemoryToolServesTheAgentsOwnBrain(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("nobody@acme.test")

	mem := &model.Brain{WorkspaceID: e.ws.ID, Name: "Field Notes"}
	if err := e.app.Store.Brains().CreateBrain(ctx, mem); err != nil {
		t.Fatalf("create brain: %v", err)
	}
	memID := mem.ID
	if err := e.app.Store.Agents().Create(ctx, &model.Agent{
		WorkspaceID: e.ws.ID, Key: model.DefaultAgentKey, Name: "Assistant",
		Tools: []string{"current_time"}, MemoryBrainID: &memID,
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	_, loadout, err := e.app.Resolve(ctx, app.ProfileRequest{
		WorkspaceID: e.ws.ID, UserID: user.ID, Channel: model.ChannelChat, PreferredModelID: 1,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Present, internal, and silent (never asks a person to approve its own notes).
	schema, ok := loadout.Schema(brain.MemoryName)
	if !ok {
		t.Fatal("an agent with a memory brain has no memory tool")
	}
	if schema.Kind != tool.KindInternal || schema.RequiresApproval {
		t.Fatalf("the memory tool is not internal-and-silent: %+v", schema)
	}
	h := loadout.Handlers[brain.MemoryName]
	if h == nil {
		t.Fatal("the memory tool has no handler")
	}

	// Save a note: the category is invented on the fly and the note lands.
	saved := decodeBrain(t, callBrain(t, h, e.ws.ID, map[string]any{
		"operation": "save", "category": "Procedures", "title": "Refunds", "content": "within 30 days",
	}))
	if saved["memory"] != "saved" {
		t.Fatalf("save did not report a new memory: %+v", saved)
	}
	// It is searchable.
	found := decodeBrain(t, callBrain(t, h, e.ws.ID, map[string]any{"operation": "search", "query": "refund"}))
	if found["count"] == float64(0) {
		t.Fatalf("the saved memory was not searchable: %+v", found)
	}
	// Saving the same title again UPDATES it, not duplicates.
	again := decodeBrain(t, callBrain(t, h, e.ws.ID, map[string]any{
		"operation": "save", "category": "Procedures", "title": "Refunds", "content": "within 14 days",
	}))
	if again["memory"] != "updated" || again["id"] != saved["id"] {
		t.Fatalf("a repeated memory was not an update: %+v", again)
	}
	cats, _ := e.app.Store.Brains().Categories(ctx, e.ws.ID, mem.ID)
	if len(cats) != 1 || cats[0].Name != "Procedures" {
		t.Fatalf("the category was not created exactly once: %+v", cats)
	}
	if docs, _ := e.app.Store.Brains().Documents(ctx, e.ws.ID, cats[0].ID); len(docs) != 1 {
		t.Fatalf("the memory duplicated instead of updating: %+v", docs)
	}
}

// The assembled prompt shows the agent's brains as a map: the assigned knowledge
// bases with their access and categories, and the memory brain by name, and a
// brain since deleted never appears.
func TestTheAssembledPromptShowsTheAgentsBrains(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("nobody@acme.test")

	live := &model.Brain{WorkspaceID: e.ws.ID, Name: "Live Manual"}
	if err := e.app.Store.Brains().CreateBrain(ctx, live); err != nil {
		t.Fatalf("create brain: %v", err)
	}
	if err := e.app.Store.Brains().CreateCategory(ctx, e.ws.ID, &model.BrainCategory{BrainID: live.ID, Name: "Setup"}); err != nil {
		t.Fatalf("create category: %v", err)
	}
	gone := &model.Brain{WorkspaceID: e.ws.ID, Name: "Deleted Manual"}
	if err := e.app.Store.Brains().CreateBrain(ctx, gone); err != nil {
		t.Fatalf("create brain: %v", err)
	}
	mem := &model.Brain{WorkspaceID: e.ws.ID, Name: "My Memory"}
	if err := e.app.Store.Brains().CreateBrain(ctx, mem); err != nil {
		t.Fatalf("create brain: %v", err)
	}
	memID := mem.ID
	if err := e.app.Store.Agents().Create(ctx, &model.Agent{
		WorkspaceID: e.ws.ID, Key: model.DefaultAgentKey, Name: "Assistant",
		Tools: []string{"current_time"}, Brains: []int64{live.ID, gone.ID}, MemoryBrainID: &memID,
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	// Delete an assigned brain: it must not appear in the prompt.
	if err := e.app.Store.Brains().DeleteBrain(ctx, e.ws.ID, gone.ID); err != nil {
		t.Fatalf("delete brain: %v", err)
	}

	profile, _, err := e.app.Resolve(ctx, app.ProfileRequest{
		WorkspaceID: e.ws.ID, UserID: user.ID, Channel: model.ChannelChat, PreferredModelID: 1,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	prompt := profile.SystemPrompt
	if !strings.Contains(prompt, "# Knowledge you can consult") ||
		!strings.Contains(prompt, "Live Manual (read/write)") {
		t.Fatalf("the assigned brain is missing from the prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "Deleted Manual") {
		t.Fatal("a deleted brain appeared in the prompt")
	}
	if !strings.Contains(prompt, "My Memory is your own long-term memory") {
		t.Fatal("the memory brain is missing from the prompt")
	}
}

// No memory brain, no memory tool: an agent that was not given one carries none.
func TestMemoryToolAbsentWithoutAMemoryBrain(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("nobody@acme.test")
	if err := e.app.Store.Agents().Create(ctx, &model.Agent{
		WorkspaceID: e.ws.ID, Key: model.DefaultAgentKey, Name: "Assistant",
		Tools: []string{"current_time"},
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	_, loadout, err := e.app.Resolve(ctx, app.ProfileRequest{
		WorkspaceID: e.ws.ID, UserID: user.ID, Channel: model.ChannelChat, PreferredModelID: 1,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, ok := loadout.Schema(brain.MemoryName); ok {
		t.Fatal("an agent with no memory brain was given a memory tool")
	}
}

func brainDoc(t *testing.T, e *env, brainID int64, category, title, content string) int64 {
	t.Helper()
	ctx := context.Background()
	cat := &model.BrainCategory{BrainID: brainID, Name: category}
	if err := e.app.Store.Brains().CreateCategory(ctx, e.ws.ID, cat); err != nil {
		t.Fatalf("create category: %v", err)
	}
	doc := &model.BrainDocument{CategoryID: cat.ID, Title: title, Content: content}
	if err := e.app.Store.Brains().SaveDocument(ctx, e.ws.ID, doc, nil); err != nil {
		t.Fatalf("save document: %v", err)
	}
	return doc.ID
}

func callBrain(t *testing.T, h tool.Handler, ws int64, args map[string]any) tool.Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("encode args: %v", err)
	}
	res, err := h(context.Background(), tool.Call{WorkspaceID: ws, Args: raw})
	if err != nil {
		t.Fatalf("brain handler returned a Go error: %v", err)
	}
	return res
}

func decodeBrain(t *testing.T, res tool.Result) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(res.Content, &out); err != nil {
		t.Fatalf("decode brain result: %v", err)
	}
	return out
}
