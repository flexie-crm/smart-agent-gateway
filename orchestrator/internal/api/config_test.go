package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
)

// The admin surface of the layered configuration model, and the proof that
// what it configures is what a chat turn actually runs.

// --- tools ---------------------------------------------------------------------

// A tool arrives with a deploy, never with an API call. What an administrator
// decides is whether it is on, whether it needs a human, and who may reach it.
func TestToolsArriveFromTheCodeAndAreGovernedByTheAdmin(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodGet, "/v1/tools", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var tools []toolBody
	env.decode(rec, &tools)
	if len(tools) == 0 {
		t.Fatal("the workspace was offered none of the build's tools")
	}

	var clock toolBody
	for _, tool := range tools {
		if tool.Name == "current_time" {
			clock = tool
		}
		// The model-admin tools are infrastructure, kept out of the catalog: an
		// administrator has no reason to browse or assign them.
		if tool.Name == "set_model_status" || tool.Name == "list_models" {
			t.Fatalf("a hidden tool was offered in the catalog: %q", tool.Name)
		}
	}
	if clock.ID == 0 {
		t.Fatalf("the build did not offer current_time: %+v", tools)
	}

	// There is no way to create one.
	rec = env.do(http.MethodPost, "/v1/tools", token, map[string]any{"name": "rm_rf"})
	if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Fatalf("a tool was creatable over the API: %d %s", rec.Code, rec.Body.String())
	}

	// The admin restricts the tool to a group. What this endpoint owns is that
	// and whether the tool is on: where a call pauses for a person is the
	// agent's decision, and an approval sent here is not a field at all.
	group := &model.Group{WorkspaceID: env.ws.ID, Name: "ops"}
	if err := env.app.Store.Groups().Create(context.Background(), group); err != nil {
		t.Fatalf("create group: %v", err)
	}
	rec = env.do(http.MethodPut, fmt.Sprintf("/v1/tools/%d", clock.ID), token, toolUpdateBody{
		Status: model.StatusActive,
		Grants: []int64{group.ID},
	})
	env.expectStatus(rec, http.StatusOK)

	var updated toolBody
	env.decode(rec, &updated)
	if len(updated.Grants) != 1 || updated.Grants[0] != group.ID {
		t.Fatalf("the grant was not stored: %+v", updated)
	}

	// And approval is not a field here at all, for any kind of tool: an
	// unknown one is refused rather than quietly ignored, so a client still
	// sending it learns that it stopped meaning anything.
	rec = env.do(http.MethodPut, fmt.Sprintf("/v1/tools/%d", clock.ID), token, map[string]any{
		"status": model.StatusActive, "requires_approval": true, "grants": []int64{},
	})
	env.expectStatus(rec, http.StatusBadRequest)
	stored, err := env.app.Store.Tools().GetByID(context.Background(), env.ws.ID, clock.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.RequiresApproval || len(stored.Grants) != 1 {
		t.Fatalf("the refused request changed the tool anyway: %+v", stored)
	}
}

func TestToolsRequirePermission(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("nobody@acme.test", "dev-Passw0rd!")
	token, _ := env.login("nobody@acme.test", "dev-Passw0rd!")

	env.expectStatus(env.do(http.MethodGet, "/v1/tools", token, nil), http.StatusForbidden)
	env.expectStatus(env.do(http.MethodPut, "/v1/tools/1", token, toolUpdateBody{
		Status: model.StatusDisabled,
	}), http.StatusForbidden)
}

// --- agents ---------------------------------------------------------------------

func TestAgentFieldErrors(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// A nameless agent with an approval window shorter than a person: both
	// problems answer together, and the window's bounds are spoken in human
	// units, not in the notation of a duration type. The key is not among them:
	// it is internal, not something a person supplies.
	env.expectFields(env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"name": "", "approval_ttl_seconds": 5,
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"name":                 "a name is required",
		"approval_ttl_seconds": "must be between 1 minute and 7 days",
	})

	// Two Gateways cannot share the reserved "default" key. The collision lands
	// on the key field even though a person never typed it.
	body := map[string]any{"key": "default", "name": "Gateway"}
	env.expectStatus(env.do(http.MethodPost, "/v1/agents", token, body), http.StatusCreated)
	env.expectFields(env.do(http.MethodPost, "/v1/agents", token, body),
		http.StatusConflict, "conflict", map[string]string{
			"key": "another agent already uses this key",
		})
}

// An agent's key is internal: the server derives it from the name, and two
// agents with the same name get distinct keys rather than a conflict.
func TestAgentKeyIsGeneratedFromName(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{"name": "Sales Helper"})
	env.expectStatus(rec, http.StatusCreated)
	var a agentBody
	env.decode(rec, &a)
	if a.Key != "sales-helper" {
		t.Fatalf("the key was not derived from the name: %q", a.Key)
	}

	rec = env.do(http.MethodPost, "/v1/agents", token, map[string]any{"name": "Sales Helper"})
	env.expectStatus(rec, http.StatusCreated)
	env.decode(rec, &a)
	if a.Key != "sales-helper-2" {
		t.Fatalf("a second agent of the same name was not disambiguated: %q", a.Key)
	}
}

// aModel is any model these rows can point at: what the dialogs configure is not
// what this is about, only that saving the agent does not take it away.
func (e *testEnv) aModel() int64 {
	e.t.Helper()
	ctx := context.Background()
	vendor := &model.AIVendor{
		WorkspaceID: e.ws.ID, VendorKey: model.VendorAnthropic,
		Name: "For the dialogs", BaseURL: "https://example.test",
	}
	if err := e.app.Store.Vendors().Create(ctx, vendor); err != nil {
		e.t.Fatalf("create vendor: %v", err)
	}
	m := &model.AIModel{
		WorkspaceID: e.ws.ID, VendorID: vendor.ID, ModelKey: "for-the-dialogs",
		Type: model.ModelTypeChat, ContextWindow: 100_000,
	}
	if err := e.app.Store.AIModels().Create(ctx, m); err != nil {
		e.t.Fatalf("create model: %v", err)
	}
	return m.ID
}

// Saving an agent must not blank what a DIFFERENT dialog owns.
//
// Files and audio are written by their own endpoints, each from a dialog that
// asks one question (KB/19). The agent form does not edit them and does not send
// them, and a full replace turned "not sent" into "cleared": one save of the
// Gateway removed its microphone and its ability to read a document, from a
// screen showing neither, on a save that reported success.
//
// The general shape, worth repeating wherever a form was split into dialogs:
// save the form without touching anything, and assert that what the other
// dialogs own is still there.
func TestSavingAnAgentKeepsWhatItsOtherDialogsConfigured(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/agents", token,
		map[string]any{"name": "Talker", "instructions": "hello"})
	env.expectStatus(rec, http.StatusCreated)
	var agent agentBody
	env.decode(rec, &agent)

	// What the other dialogs own, put there the way they put it: on the row.
	// Going through their endpoints would need a transcribing model and a vendor
	// to hang it on, none of which this is about.
	stored, err := env.app.Store.Agents().GetByID(context.Background(), env.ws.ID, agent.ID)
	if err != nil {
		t.Fatalf("read the agent: %v", err)
	}
	modelID := env.aModel()
	stored.AudioModelID = &modelID
	stored.FileRules = []model.FileRule{{Types: []string{"application/pdf"}, ModelID: modelID}}
	if err := env.app.Store.Agents().Update(context.Background(), stored); err != nil {
		t.Fatalf("set what the dialogs own: %v", err)
	}

	// Now the agent form saves, exactly as the console sends it: everything it
	// owns, and nothing it does not.
	env.expectStatus(env.do(http.MethodPut, "/v1/agents/"+strconv.FormatInt(agent.ID, 10), token,
		map[string]any{
			"key": agent.Key, "name": "Talker", "instructions": "changed",
			"tools": []string{}, "confirm_tools": []string{}, "brains": []int64{},
			"status": "active",
		}), http.StatusOK)

	after, err := env.app.Store.Agents().GetByID(context.Background(), env.ws.ID, agent.ID)
	if err != nil {
		t.Fatalf("read the agent back: %v", err)
	}
	if after.Instructions != "changed" {
		t.Errorf("the save did not do what it was for: %q", after.Instructions)
	}
	if after.AudioModelID == nil {
		t.Error("saving the agent removed its audio model, so the chat lost its microphone")
	}
	if len(after.FileRules) == 0 {
		t.Error("saving the agent removed its file rules, so it can no longer read a document")
	}
}

// Confirmation is a decision about an agent's OWN tools: a tool named for
// confirmation that the agent cannot call is refused on the confirm_tools field,
// and a subset round-trips.
func TestAgentConfirmToolsMustBeAmongTheAgentsTools(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	env.expectFields(env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": "sales", "name": "Sales",
		"tools":         []string{"current_time"},
		"confirm_tools": []string{"set_model_status"},
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"confirm_tools": "must be tools this agent can use",
	})

	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": "sales", "name": "Sales",
		"tools":         []string{"current_time", "set_model_status"},
		"confirm_tools": []string{"set_model_status"},
	})
	env.expectStatus(rec, http.StatusCreated)
	var created agentBody
	env.decode(rec, &created)
	if len(created.ConfirmTools) != 1 || created.ConfirmTools[0] != "set_model_status" {
		t.Fatalf("the confirm set did not round-trip: %+v", created.ConfirmTools)
	}
}

// The catalog advertises only what an administrator should browse and assign.
// A tool flagged Hidden is real and grantable (the suite still assigns it), but
// it is infrastructure or a testing aid, so it is kept out of the tools UI.
func TestHiddenToolsAreNotInTheCatalog(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	var list []toolBody
	env.decode(env.do(http.MethodGet, "/v1/tools", token, nil), &list)

	// The real capabilities are shown.
	for _, name := range []string{"current_time", "http_request", "brain", "brain_write"} {
		if !containsTool(list, name) {
			t.Fatalf("a real tool is missing from the catalog: %q in %+v", name, list)
		}
	}
	// The testing/infrastructure tools are not.
	for _, name := range []string{"set_model_status", "list_models"} {
		if containsTool(list, name) {
			t.Fatalf("a hidden tool is advertised in the catalog: %q", name)
		}
	}
}

// An agent may READ any assigned brain, but its long-term MEMORY brain must be
// unlocked: the agent writes it back, and a locked brain is read-only.
func TestAgentMemoryBrainMustBeUnlocked(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	ctx := context.Background()
	locked := &model.Brain{WorkspaceID: env.ws.ID, Name: "Policy", Slug: "policy", Locked: true}
	if err := env.app.Store.Brains().CreateBrain(ctx, locked); err != nil {
		t.Fatalf("create locked brain: %v", err)
	}
	open := &model.Brain{WorkspaceID: env.ws.ID, Name: "Notes", Slug: "notes"}
	if err := env.app.Store.Brains().CreateBrain(ctx, open); err != nil {
		t.Fatalf("create open brain: %v", err)
	}

	// A locked brain cannot be an agent's memory: the field is refused.
	env.expectFields(env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": "m1", "name": "M1", "memory_brain_id": locked.ID,
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"memory_brain_id": "must be a brain that is not locked",
	})

	// An unlocked memory brain is accepted, and the assigned brains (a locked one
	// is fine to READ) round-trip.
	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": "m2", "name": "M2",
		"brains":          []int64{open.ID, locked.ID},
		"memory_brain_id": open.ID,
	})
	env.expectStatus(rec, http.StatusCreated)
	var created agentBody
	env.decode(rec, &created)
	if created.MemoryBrainID == nil || *created.MemoryBrainID != open.ID {
		t.Fatalf("the memory brain did not round-trip: %+v", created.MemoryBrainID)
	}
	if len(created.Brains) != 2 {
		t.Fatalf("the assigned brains did not round-trip: %+v", created.Brains)
	}
}

// Reconfiguring an agent wipes the workspace's distilled notes. They are learned
// under the setup as it stood, so a note can outlive the tool it describes (the
// assistant kept claiming it could query a database after the tool was removed).
// Creating, updating, or deleting an agent clears them; the assistant re-learns.
func TestChangingAnAgentForgetsTheWorkspaceNotes(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	ctx := context.Background()

	seed := func() {
		if err := env.app.Store.Memory().SetWorkspaceMemory(ctx, env.ws.ID, "I can query the CRM with SQL."); err != nil {
			t.Fatalf("seed memory: %v", err)
		}
	}
	assertCleared := func(after string) {
		note, err := env.app.Store.Memory().WorkspaceMemory(ctx, env.ws.ID)
		if err != nil {
			t.Fatalf("read memory: %v", err)
		}
		if note != "" {
			t.Fatalf("%s did not clear the workspace notes: %q", after, note)
		}
	}

	// Creating an agent clears the notes.
	seed()
	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": "default", "name": "Gateway", "tools": []string{},
	})
	env.expectStatus(rec, http.StatusCreated)
	var Gateway agentBody
	env.decode(rec, &Gateway)
	assertCleared("creating an agent")

	// Updating an agent (its tools) clears them again.
	seed()
	env.expectStatus(env.do(http.MethodPut, fmt.Sprintf("/v1/agents/%d", Gateway.ID), token, map[string]any{
		"key": "default", "name": "Gateway", "tools": []string{"current_time"},
	}), http.StatusOK)
	assertCleared("updating an agent")

	// Deleting an agent clears them too.
	seed()
	env.expectStatus(env.do(http.MethodDelete, fmt.Sprintf("/v1/agents/%d", Gateway.ID), token, nil), http.StatusNoContent)
	assertCleared("deleting an agent")
}

// --- workflows -----------------------------------------------------------------

// Publishing is a separate permission from editing, and neither route around
// the other: a workflow changes what other people's assistants do.
func TestPublishingIsNotEditing(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("editor@acme.test", "dev-Passw0rd!",
		model.PermWorkflowsView, model.PermWorkflowsCreate, model.PermWorkflowsEdit)
	token, _ := env.login("editor@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/workflows", token, workflowBody{Name: "Support"})
	env.expectStatus(rec, http.StatusCreated)
	var wf workflowBody
	env.decode(rec, &wf)
	if wf.Status != model.WorkflowDraft {
		t.Fatalf("a new workflow must be a draft, got %q", wf.Status)
	}

	rec = env.do(http.MethodPost, fmt.Sprintf("/v1/workflows/%d/versions", wf.ID), token, versionBody{
		Definition: json.RawMessage(`{"kind":"profile","profile":{"reasoning":true}}`),
	})
	env.expectStatus(rec, http.StatusCreated)
	var version versionBody
	env.decode(rec, &version)

	// The editor cannot publish it.
	rec = env.do(http.MethodPost, fmt.Sprintf("/v1/workflows/%d/publish", wf.ID), token,
		publishBody{VersionID: version.ID})
	env.expectStatus(rec, http.StatusForbidden)

	// Nor can they publish it by writing the status, which would be the same
	// thing through a different door.
	rec = env.do(http.MethodPut, fmt.Sprintf("/v1/workflows/%d", wf.ID), token, workflowBody{
		Name: "Support", Status: model.WorkflowPublished,
	})
	env.expectStatus(rec, http.StatusBadRequest)

	// It is still a draft, so it still shapes nobody's turn.
	candidates, err := env.app.Store.Workflows().Candidates(context.Background(), env.ws.ID)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatal("an unpublished workflow reached a turn")
	}

	// The publisher can.
	env.createUser("publisher@acme.test", "dev-Passw0rd!", model.PermWorkflowsPublish)
	publisherToken, _ := env.login("publisher@acme.test", "dev-Passw0rd!")
	rec = env.do(http.MethodPost, fmt.Sprintf("/v1/workflows/%d/publish", wf.ID), publisherToken,
		publishBody{VersionID: version.ID})
	env.expectStatus(rec, http.StatusNoContent)
}

// A definition that would only fail at run time fails in front of a user,
// mid-conversation. It is refused when it is saved instead.
func TestABrokenDefinitionIsRefusedAtSave(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/workflows", token, workflowBody{Name: "Support"})
	env.expectStatus(rec, http.StatusCreated)
	var wf workflowBody
	env.decode(rec, &wf)

	cases := []struct {
		name       string
		definition string
	}{
		{"empty", ``},
		{"not a profile", `{"kind":"graph","profile":{}}`},
		{"no profile", `{"kind":"profile"}`},
		{"a model that is not a model", `{"kind":"profile","profile":{"model_id":0}}`},
		{"a tool with no name", `{"kind":"profile","profile":{"tools":[""]}}`},
		// The one that matters most: a misspelled field would be accepted, do
		// nothing, and leave an administrator staring at a switch they are
		// certain they turned on.
		{"a misspelled field", `{"kind":"profile","profile":{"reasonning":true}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{}
			if tc.definition != "" {
				body["definition"] = json.RawMessage(tc.definition)
			}
			rec := env.do(http.MethodPost, fmt.Sprintf("/v1/workflows/%d/versions", wf.ID), token, body)
			env.expectStatus(rec, http.StatusBadRequest)
		})
	}
}

func TestAssignmentsAreValidated(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/workflows", token, workflowBody{Name: "Support"})
	env.expectStatus(rec, http.StatusCreated)
	var wf workflowBody
	env.decode(rec, &wf)
	path := fmt.Sprintf("/v1/workflows/%d/assignments", wf.ID)

	// A condition nothing could ever satisfy is a mistake, and it is caught
	// where it is made rather than by an administrator wondering why their
	// workflow never fires.
	env.expectStatus(env.do(http.MethodPut, path, token, []assignmentBody{
		{MatchType: model.MatchChannel, MatchValue: "carrier-pigeon"},
	}), http.StatusBadRequest)
	env.expectStatus(env.do(http.MethodPut, path, token, []assignmentBody{
		{MatchType: "department", MatchValue: "sales"},
	}), http.StatusBadRequest)
	env.expectStatus(env.do(http.MethodPut, path, token, []assignmentBody{
		{MatchType: model.MatchUser, MatchValue: "alice"},
	}), http.StatusBadRequest)

	env.expectStatus(env.do(http.MethodPut, path, token, []assignmentBody{
		{MatchType: model.MatchGroup, MatchValue: "5", Priority: 10},
		{MatchType: model.MatchChannel, MatchValue: model.ChannelChat},
	}), http.StatusNoContent)

	rec = env.do(http.MethodGet, path, token, nil)
	env.expectStatus(rec, http.StatusOK)
	var assignments []assignmentBody
	env.decode(rec, &assignments)
	if len(assignments) != 2 {
		t.Fatalf("expected the conditions to be stored, got %+v", assignments)
	}
}

// --- end to end ----------------------------------------------------------------

// The whole point of the layer, proved through a real chat turn: a workflow
// assigned to a group changes what the model is told, and what it is handed.
func TestAWorkflowReshapesTheTurn(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	user := env.createUser("sales@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("sales@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"Fine"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	})
	modelID := env.registerModel(vendor)
	// A configured main agent with no tool opinion: it inherits the code
	// defaults, so this is the default turn (a workspace answers only through a
	// configured assistant now).
	env.GatewayAgent()

	// With just the default main agent, the model is handed the default tools.
	frames := env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": modelID})
	if last := frames[len(frames)-1]; last.Type != chat.FrameResult {
		t.Fatalf("the turn did not complete: %+v", last)
	}
	sent := vendor.sentToolsAsking("hi")
	// The default tools, plus two the Gateway always rides along with when it has
	// any tools at all: the memory tool (it flags a turn as worth remembering, and
	// the note is distilled in the background) and tool_guide (it looks up how to
	// use its abilities).
	for _, want := range env.app.DefaultTools() {
		if !contains(sent, want) {
			t.Fatalf("the default turn was not handed the default tool %q: %+v", want, sent)
		}
	}
	if !contains(sent, model.MemoryToolName) {
		t.Fatalf("the Gateway was not given the memory tool: %+v", sent)
	}
	if !contains(sent, "tool_guide") {
		t.Fatalf("the Gateway was not given tool_guide: %+v", sent)
	}
	// The default tools, plus the internal ones that ride along with any set:
	// remembering, looking an ability up, reading back what was remembered, and
	// reaching a connected service. Named rather than counted, so adding one is
	// a decision here instead of a number that quietly moves.
	rideAlong := []string{"remember", "tool_guide", "recall", "integrations"}
	for _, name := range rideAlong {
		if !contains(sent, name) {
			t.Fatalf("the Gateway was not given %s: %+v", name, sent)
		}
	}
	if len(sent) != len(env.app.DefaultTools())+len(rideAlong) {
		t.Fatalf("the default turn was handed unexpected tools: %+v", sent)
	}

	// The administrator gives this group a workflow: no tools, and different
	// instructions.
	group := &model.Group{WorkspaceID: env.ws.ID, Name: "sales"}
	if err := env.app.Store.Groups().Create(ctx, group); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := env.app.Store.Groups().AddMember(ctx, group.ID, user.ID); err != nil {
		t.Fatalf("add member: %v", err)
	}

	wf := &model.Workflow{WorkspaceID: env.ws.ID, Name: "Sales", CreatedBy: user.ID}
	if err := env.app.Store.Workflows().Create(ctx, wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	version := &model.WorkflowVersion{
		WorkflowID: wf.ID,
		Definition: json.RawMessage(
			`{"kind":"profile","profile":{"tools":[],"system_prompt":"You are the sales assistant."}}`),
		CreatedBy: user.ID,
	}
	if err := env.app.Store.Workflows().CreateVersion(ctx, env.ws.ID, version); err != nil {
		t.Fatalf("create version: %v", err)
	}
	if err := env.app.Store.Workflows().Publish(ctx, env.ws.ID, wf.ID, version.ID); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := env.app.Store.Workflows().SetAssignments(ctx, env.ws.ID, wf.ID, []model.WorkflowAssignment{
		{MatchType: model.MatchGroup, MatchValue: fmt.Sprint(group.ID)},
		{MatchType: model.MatchChannel, MatchValue: model.ChannelChat},
	}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	// The very next message runs under the workflow. Nothing was restarted,
	// and no token was reissued: configuration takes effect live.
	frames = env.streamTurn(token, map[string]any{"prompt": "hi again", "model_id": modelID})
	if last := frames[len(frames)-1]; last.Type != chat.FrameResult {
		t.Fatalf("the turn did not complete: %+v", last)
	}
	if sent := vendor.sentToolsAsking("hi again"); len(sent) != 0 {
		t.Fatalf("the workflow said no tools, and the model was handed %+v", sent)
	}
	// The workflow's instructions are appended to the assembled base, so the
	// model gets both: its house instruction and the frame that keeps it safe.
	underWorkflow := vendor.sentSystemPromptAsking("hi again")
	if !strings.Contains(underWorkflow, "You are the sales assistant.") {
		t.Fatalf("the workflow's instructions did not reach the model: %q", underWorkflow)
	}
	if !strings.Contains(underWorkflow, "# How you communicate") {
		t.Fatalf("the workflow's instructions replaced the base instead of extending it: %q", underWorkflow)
	}
	// And the turn before it, under the defaults, got the same base but none of
	// the workflow's house text.
	underDefaults := vendor.sentSystemPromptAsking("hi")
	if !strings.Contains(underDefaults, "# How you communicate") {
		t.Fatalf("the default turn was not given the base instructions: %q", underDefaults)
	}
	if strings.Contains(underDefaults, "You are the sales assistant.") {
		t.Fatalf("the default turn leaked another group's workflow instructions: %q", underDefaults)
	}

	// The session records what shaped it, so months later the answer is
	// traceable to the configuration that produced it.
	session := env.sessionOf(frames)
	if session.WorkflowVersionID == nil || *session.WorkflowVersionID != version.ID {
		t.Fatalf("the session does not record the workflow that shaped it: %+v", session.WorkflowVersionID)
	}
}

// A workflow that pins a model outranks the picker. An administrator who pins
// one is giving an instruction, and the chat UI is not a way around it.
func TestAPinnedModelOutranksThePicker(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	user := env.createUser("sales@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("sales@acme.test", "dev-Passw0rd!")

	picked := newFakeVendor(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"the picked model"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	})
	pinned := newFakeVendor(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"the pinned model"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	})
	pickedID := env.registerModel(picked)
	pinnedID := env.registerModel(pinned)

	wf := &model.Workflow{WorkspaceID: env.ws.ID, Name: "Pinned", CreatedBy: user.ID}
	if err := env.app.Store.Workflows().Create(ctx, wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	version := &model.WorkflowVersion{
		WorkflowID: wf.ID,
		Definition: json.RawMessage(
			fmt.Sprintf(`{"kind":"profile","profile":{"model_id":%d}}`, pinnedID)),
		CreatedBy: user.ID,
	}
	if err := env.app.Store.Workflows().CreateVersion(ctx, env.ws.ID, version); err != nil {
		t.Fatalf("create version: %v", err)
	}
	if err := env.app.Store.Workflows().Publish(ctx, env.ws.ID, wf.ID, version.ID); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := env.app.Store.Workflows().SetAssignments(ctx, env.ws.ID, wf.ID, []model.WorkflowAssignment{
		{MatchType: model.MatchUser, MatchValue: fmt.Sprint(user.ID)},
	}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	// The client asks for the other model. It does not get it.
	frames := env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": pickedID})
	if got := deltaText(frames); got != "the pinned model" {
		t.Fatalf("the picker overrode a pinned model: %q", got)
	}
}

// The Gateway's file routing, and its audio.
//
// Files are a list of rules tried in order, so a business can send screenshots
// to a cheap model and contracts to a careful one, or point one catch-all rule
// at everything. Audio is separate because reading it is transcription.
//
// The half worth testing hardest is the refusals: a rule that could not work
// has to be caught at the form, not at the moment somebody uploads a file.
func TestTheGatewaysFileRulesAndAudio(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	vendor := env.createVendorViaAPI(token, "Anthropic", secretAPIKey)

	newModel := func(key, kind string) int64 {
		t.Helper()
		rec := env.do(http.MethodPost, "/v1/models", token, map[string]any{
			"vendor_id": vendor.ID, "model_key": key, "type": kind, "context_window": 200000,
		})
		env.expectStatus(rec, http.StatusCreated)
		var m modelBody
		env.decode(rec, &m)
		return m.ID
	}
	careful := newModel("claude-opus-4-8", model.ModelTypeChat)
	cheap := newModel("claude-haiku-4-5", model.ModelTypeChat)
	transcriber := newModel("claude-sonnet-5", model.ModelTypeSTT)

	// The GATEWAY specifically, by its key: the endpoints that own files and
	// audio find it by that, not by what it is called.
	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": model.DefaultAgentKey, "name": "Gateway", "model_id": careful,
		"file_rules": []map[string]any{
			{"types": []string{"pdf", "docx"}, "model_id": careful},
			{"types": []string{}, "model_id": cheap},
		},
		"audio_model_id": transcriber,
	})
	env.expectStatus(rec, http.StatusCreated)
	var created agentBody
	env.decode(rec, &created)
	if len(created.FileRules) != 2 || created.FileRules[0].ModelID != careful {
		t.Fatalf("the rules did not come back as sent: %+v", created.FileRules)
	}

	// Order is the rule, so it has to survive a reload in the order it was
	// written: a catch-all that came back first would silently swallow the rest.
	rec = env.do(http.MethodGet, fmt.Sprintf("/v1/agents/%d", created.ID), token, nil)
	env.expectStatus(rec, http.StatusOK)
	var loaded agentBody
	env.decode(rec, &loaded)
	if len(loaded.FileRules) != 2 {
		t.Fatalf("the rules did not survive a reload: %+v", loaded.FileRules)
	}
	if len(loaded.FileRules[0].Types) != 2 || loaded.FileRules[0].Types[0] != "pdf" {
		t.Fatalf("the first rule lost its types: %+v", loaded.FileRules[0])
	}
	if len(loaded.FileRules[1].Types) != 0 {
		t.Fatalf("the catch-all did not stay a catch-all: %+v", loaded.FileRules[1])
	}
	if loaded.AudioModelID == nil || *loaded.AudioModelID != transcriber {
		t.Fatalf("the audio model did not survive a reload: %+v", loaded.AudioModelID)
	}

	// An empty list clears, which is how an administrator stops accepting files.
	//
	// Through the endpoints that OWN these fields, which is where the Gateway
	// screen writes them from: a dialog per question (KB/19). The agent form does
	// not send them and no longer gets to blank them.
	env.expectStatus(env.do(http.MethodPut, "/v1/gateway/files", token,
		map[string]any{"rules": []map[string]any{}}), http.StatusNoContent)
	env.expectStatus(env.do(http.MethodPut, "/v1/gateway/audio", token,
		map[string]any{"model_id": nil}), http.StatusNoContent)

	rec = env.do(http.MethodGet, fmt.Sprintf("/v1/agents/%d", created.ID), token, nil)
	env.expectStatus(rec, http.StatusOK)
	var cleared agentBody
	env.decode(rec, &cleared)
	if len(cleared.FileRules) != 0 || cleared.AudioModelID != nil {
		t.Fatalf("clearing left something behind: %+v", cleared)
	}

	for _, bad := range []struct {
		name string
		body map[string]any
	}{
		{"a file read by a transcriber", map[string]any{
			"file_rules": []map[string]any{{"types": []string{"pdf"}, "model_id": transcriber}}}},
		{"audio read by a chat model", map[string]any{"audio_model_id": careful}},
		{"a model that does not exist", map[string]any{
			"file_rules": []map[string]any{{"types": []string{"pdf"}, "model_id": 999999}}}},
		{"a catch-all that is not last", map[string]any{
			"file_rules": []map[string]any{
				{"types": []string{}, "model_id": cheap},
				{"types": []string{"pdf"}, "model_id": careful},
			}}},
	} {
		// Refused by whichever endpoint owns the field being set, since that is
		// the one a person reaches it through.
		var rec *httptest.ResponseRecorder
		if rules, ok := bad.body["file_rules"]; ok {
			rec = env.do(http.MethodPut, "/v1/gateway/files", token, map[string]any{"rules": rules})
		} else {
			rec = env.do(http.MethodPut, "/v1/gateway/audio", token,
				map[string]any{"model_id": bad.body["audio_model_id"]})
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected it to be refused, got %d", bad.name, rec.Code)
		}
	}
}

// An update that does not name a key is not asking for the key to change.
//
// Taking the body's empty string literally blanked it, and the key is how the
// Gateway is found: a save that reported success left the workspace with no
// agent to answer with, and nothing said so.
func TestUpdatingAnAgentWithoutAKeyKeepsTheOneItHas(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": model.DefaultAgentKey, "name": "Gateway",
	})
	env.expectStatus(rec, http.StatusCreated)
	var created agentBody
	env.decode(rec, &created)

	rec = env.do(http.MethodPut, fmt.Sprintf("/v1/agents/%d", created.ID), token,
		map[string]any{"name": "Gateway renamed"})
	env.expectStatus(rec, http.StatusOK)
	var updated agentBody
	env.decode(rec, &updated)
	if updated.Key != model.DefaultAgentKey {
		t.Fatalf("the key was lost on an update that never mentioned it: %q", updated.Key)
	}
}

// The Gateway screen answers in the shape of the screen.
//
// It used to be served by the agents COLLECTION, and the client picked it
// apart: find the one whose key is "default", call that the Gateway, call the
// rest the agents, then reach inside the Gateway for its file rules and its
// audio model and present those as two more sections. That is the screen's own
// structure, rebuilt on the far side of the wire from a list that had been
// flattened to send it (KB/19).
func TestTheGatewayScreenIsShapedLikeTheScreen(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	modelID := env.registerModel(newFakeVendor(t, answerChunks("hi")))

	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"name": "Gateway", "key": model.DefaultAgentKey, "model_id": modelID,
		// No audio model: one has to be a transcriber, and the point here is the
		// SHAPE. An empty audio section still has to be a section.
		"file_rules": []map[string]any{{"types": []string{"pdf"}, "model_id": modelID}},
	})
	env.expectStatus(rec, http.StatusCreated)
	rec = env.do(http.MethodPost, "/v1/agents", token, map[string]any{"name": "Finance"})
	env.expectStatus(rec, http.StatusCreated)

	rec = env.do(http.MethodGet, "/v1/gateway", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var body gatewayScreenBody
	env.decode(rec, &body)

	// Four sections, each holding its own thing.
	if body.Gateway == nil || body.Gateway.Key != model.DefaultAgentKey {
		t.Fatalf("the gateway section does not hold the Gateway: %+v", body.Gateway)
	}
	if len(body.Agents) != 1 || body.Agents[0].Name != "Finance" {
		t.Fatalf("the agents section should hold the others and only the others: %+v", body.Agents)
	}

	// A ROW, not an entity. The screen draws a name, a model name, a tool count,
	// whether it reasons and whether it is on. Sending the whole agent put its
	// instructions, brains, confirm list, file rules and timestamps on every
	// line of a table that shows none of them.
	raw := rec.Body.String()
	for _, leaked := range []string{"instructions", "confirm_tools", "brains", "created_at", "model_names"} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("the screen is carrying %q, which nothing on it draws", leaked)
		}
	}
	if body.Gateway.ModelName == "" {
		t.Fatalf("the Gateway's row carries no model name: %+v", body.Gateway)
	}
	for _, a := range body.Agents {
		if a.Key == model.DefaultAgentKey {
			t.Fatal("the Gateway is in the agents section as well")
		}
	}

	// The files section says what will happen to the next file that arrives,
	// with the NAME of what reads it, not an id the client has to translate.
	if len(body.Files.Rules) != 1 || body.Files.Rules[0].Types[0] != "pdf" {
		t.Fatalf("the files section did not carry the rules: %+v", body.Files.Rules)
	}
	if body.Files.Rules[0].ModelName == "" {
		t.Fatalf("a file rule came back without the name of what reads it: %+v", body.Files.Rules[0])
	}
	// Nothing chosen is still an ANSWER, and the section is there to give it:
	// the screen says "nobody can talk instead of typing yet" from an empty name.
	if body.Audio.ModelName != "" {
		t.Fatalf("the audio section invented a model: %+v", body.Audio)
	}
}

// Each section of the Gateway screen is written on its own.
//
// A form that changes four file rules used to send the whole agent back, which
// meant fetching the whole agent first to have something to echo: its
// instructions, its brains, its confirm list and its timestamps, all
// round-tripped so one field could move (KB/19).
func TestEachGatewaySectionIsWrittenOnItsOwn(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	modelID := env.registerModel(newFakeVendor(t, answerChunks("hi")))

	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"name": "Gateway", "key": model.DefaultAgentKey, "model_id": modelID,
		"instructions": "keep it short",
	})
	env.expectStatus(rec, http.StatusCreated)

	// The files form sends its rules. Nothing else.
	env.expectStatus(env.do(http.MethodPut, "/v1/gateway/files", token, map[string]any{
		"rules": []map[string]any{{"types": []string{"pdf"}, "model_id": modelID}},
	}), http.StatusNoContent)

	rec = env.do(http.MethodGet, "/v1/gateway", token, nil)
	var screen gatewayScreenBody
	env.decode(rec, &screen)
	if len(screen.Files.Rules) != 1 || screen.Files.Rules[0].ModelID != modelID {
		t.Fatalf("the rule did not land: %+v", screen.Files.Rules)
	}
	// A rule carries the id its form edits AND the name its table prints.
	if screen.Files.Rules[0].ModelName == "" {
		t.Fatalf("the rule came back without the name its table prints: %+v", screen.Files.Rules[0])
	}

	// Writing one section leaves the rest of the agent exactly alone, which is
	// the whole point: nothing was echoed back, so nothing could be lost.
	rec = env.do(http.MethodGet, "/v1/agents", token, nil)
	var agents []agentBody
	env.decode(rec, &agents)
	for _, a := range agents {
		if a.Key == model.DefaultAgentKey && a.Instructions != "keep it short" {
			t.Fatalf("writing the files section disturbed the agent: %q", a.Instructions)
		}
	}

	// And the audio form sends one id.
	env.expectStatus(env.do(http.MethodPut, "/v1/gateway/audio", token, map[string]any{
		"model_id": nil,
	}), http.StatusNoContent)

	// A model that cannot transcribe is refused on its own field, not swallowed.
	rec = env.do(http.MethodPut, "/v1/gateway/audio", token, map[string]any{"model_id": modelID})
	env.expectStatus(rec, http.StatusBadRequest)
}

// A form asks ONE question, and the answer is that whole form: the values it
// edits, and the choices it offers.
//
// The Gateway screen had three dialogs and FIVE requests, every one of them
// fired the moment any dialog opened: the models, the vendors that only
// labelled them, the tools, the brains, and the agent. The dialog that sets one
// audio model was waiting on the tool catalogue to arrive.
func TestEachFormOnTheGatewayScreenIsOneAnswer(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	vendor := env.createVendorViaAPI(token, "Anthropic", secretAPIKey)

	newModel := func(key, kind string) int64 {
		t.Helper()
		rec := env.do(http.MethodPost, "/v1/models", token, map[string]any{
			"vendor_id": vendor.ID, "model_key": key, "type": kind, "context_window": 200000,
		})
		env.expectStatus(rec, http.StatusCreated)
		var m modelBody
		env.decode(rec, &m)
		return m.ID
	}
	reader := newModel("claude-opus-4-8", model.ModelTypeChat)
	transcriber := newModel("claude-sonnet-5", model.ModelTypeSTT)

	env.expectStatus(env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"name": "Gateway", "key": model.DefaultAgentKey, "model_id": reader,
		"file_rules":     []map[string]any{{"types": []string{"pdf"}, "model_id": reader}},
		"audio_model_id": transcriber,
	}), http.StatusCreated)

	// The Files dialog: the rules it edits, and what can READ a file. A model
	// that only transcribes is not a choice here, because choosing it could not
	// work.
	rec := env.do(http.MethodGet, "/v1/gateway/files/form", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var files filesFormBody
	env.decode(rec, &files)
	if len(files.Rules) != 1 || files.Rules[0].ModelID != reader {
		t.Fatalf("the files form did not carry the rules it edits: %+v", files.Rules)
	}
	if len(files.Models) != 1 || files.Models[0].ID != reader {
		t.Fatalf("the files form should offer the chat model and only it: %+v", files.Models)
	}
	// The label is the string the control PRINTS. Composing it on the far side
	// meant fetching every vendor to turn one number into one word.
	if files.Models[0].Label != "Anthropic / claude-opus-4-8" {
		t.Fatalf("a choice came back without the string it is drawn as: %q", files.Models[0].Label)
	}

	// The Audio dialog: the one id it sets, and what can transcribe.
	rec = env.do(http.MethodGet, "/v1/gateway/audio/form", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var audio audioFormBody
	env.decode(rec, &audio)
	if audio.ModelID == nil || *audio.ModelID != transcriber {
		t.Fatalf("the audio form did not carry the model it edits: %+v", audio.ModelID)
	}
	if len(audio.Models) != 1 || audio.Models[0].ID != transcriber {
		t.Fatalf("the audio form should offer the transcriber and only it: %+v", audio.Models)
	}
	// And it carries nothing of the other dialogs: it changes one id.
	if raw := rec.Body.String(); strings.Contains(raw, "tools") || strings.Contains(raw, "brains") {
		t.Fatalf("the audio form is carrying what another dialog needs: %s", raw)
	}

	// The agent dialog: the agent, and everything it can be given.
	rec = env.do(http.MethodGet, "/v1/agents/form", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var blank agentFormBody
	env.decode(rec, &blank)
	// No id is the form for an agent that does not exist yet, and the server
	// says so rather than leaving a form to invent a default.
	if blank.Agent != nil {
		t.Fatalf("the new-agent form invented an agent: %+v", blank.Agent)
	}
	if len(blank.Models) != 1 || blank.Models[0].ID != reader {
		t.Fatalf("an agent runs on a chat model, so that is what is offered: %+v", blank.Models)
	}
	if len(blank.Tools) == 0 {
		t.Fatal("the agent form offers no tools at all")
	}
	for _, tl := range blank.Tools {
		if schema, ok := env.app.Tools.Lookup(tl.Name); ok && schema.Hidden {
			t.Fatalf("a hidden tool reached the form: %q", tl.Name)
		}
	}
}

// And the form for an agent that DOES exist carries it, with the same choices.
func TestTheAgentFormCarriesTheAgentItEdits(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"name": "Finance", "instructions": "answer in euros", "tools": []string{"current_time"},
	})
	env.expectStatus(rec, http.StatusCreated)
	var created agentBody
	env.decode(rec, &created)

	rec = env.do(http.MethodGet, fmt.Sprintf("/v1/agents/%d/form", created.ID), token, nil)
	env.expectStatus(rec, http.StatusOK)
	var form agentFormBody
	env.decode(rec, &form)
	if form.Agent == nil || form.Agent.Name != "Finance" {
		t.Fatalf("the form did not carry the agent it edits: %+v", form.Agent)
	}
	if form.Agent.Instructions != "answer in euros" {
		t.Fatalf("the form carried a row, not the agent: %+v", form.Agent)
	}
	if len(form.Agent.Tools) != 1 || form.Agent.Tools[0] != "current_time" {
		t.Fatalf("what the agent already holds did not come back: %+v", form.Agent.Tools)
	}

	// `form` is a route, not an id: the two live side by side and the static one
	// wins, or every form request would be a lookup of an agent called "form".
	rec = env.do(http.MethodGet, "/v1/agents/form", token, nil)
	env.expectStatus(rec, http.StatusOK)
}

// The user dialog is one answer too: this workspace's groups, each already
// saying whether the person is in it.
//
// It was two requests and a join. The console fetched every group of the
// workspace AND every group the person belongs to anywhere, then kept the ones
// appearing in both. That intersection is the workspace scoping rule, and the
// rule is the server's.
func TestTheUserFormSaysWhoIsInWhat(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	newGroup := func(name string) int64 {
		t.Helper()
		rec := env.do(http.MethodPost, "/v1/groups", token, map[string]any{"name": name})
		env.expectStatus(rec, http.StatusCreated)
		var g groupBody
		env.decode(rec, &g)
		return g.ID
	}
	sales := newGroup("Sales")
	support := newGroup("Support")

	rec := env.do(http.MethodPost, "/v1/users", token, map[string]any{
		"email": "bob@acme.test", "name": "Bob", "password": "dev-Passw0rd!",
		"workspaces": []int64{env.ws.ID},
	})
	env.expectStatus(rec, http.StatusCreated)
	var bob userBody
	env.decode(rec, &bob)
	env.expectStatus(env.do(http.MethodPut, fmt.Sprintf("/v1/users/%d/groups", bob.ID), token,
		map[string]any{"groups": []int64{sales}}), http.StatusNoContent)

	rec = env.do(http.MethodGet, fmt.Sprintf("/v1/users/%d/form", bob.ID), token, nil)
	env.expectStatus(rec, http.StatusOK)
	var form userFormBody
	env.decode(rec, &form)
	member := map[int64]bool{}
	offered := map[int64]bool{}
	for _, g := range form.Groups {
		member[g.ID], offered[g.ID] = g.Member, true
		if g.Name == "" {
			t.Fatalf("a group came back without the name its checkbox prints: %+v", g)
		}
	}
	if !offered[sales] || !offered[support] {
		t.Fatalf("the form should offer this workspace's groups: %+v", form.Groups)
	}
	if !member[sales] || member[support] {
		t.Fatalf("the form did not say which groups Bob is in: %+v", form.Groups)
	}

	// A person who does not exist yet is in nothing, which is a FACT and not a
	// list still loading: every box is offered, unticked.
	rec = env.do(http.MethodGet, "/v1/users/form", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var blank userFormBody
	env.decode(rec, &blank)
	if len(blank.Groups) != len(form.Groups) {
		t.Fatalf("the new-user form should offer the same groups: %+v", blank.Groups)
	}
	for _, g := range blank.Groups {
		if g.Member {
			t.Fatalf("a person who does not exist is in a group: %+v", g)
		}
	}
}

// A tool's edit form carries who it MAY be granted to, beside who it IS granted
// to. The screen used to fetch the workspace's groups on every view of the
// tools table, on the chance that somebody opened a dialog.
func TestTheToolFormCarriesTheGroupsItCanBeGrantedTo(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/groups", token, map[string]any{"name": "Sales"})
	env.expectStatus(rec, http.StatusCreated)
	var sales groupBody
	env.decode(rec, &sales)

	rec = env.do(http.MethodGet, "/v1/tools", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var tools []toolBody
	env.decode(rec, &tools)
	if len(tools) == 0 {
		t.Fatal("this build ships no tools to test with")
	}

	rec = env.do(http.MethodGet, fmt.Sprintf("/v1/tools/%d", tools[0].ID), token, nil)
	env.expectStatus(rec, http.StatusOK)
	var detail toolDetailBody
	env.decode(rec, &detail)
	found := false
	for _, g := range detail.Groups {
		if g.ID == sales.ID && g.Name == "Sales" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the tool form cannot draw its grant checkboxes: %+v", detail.Groups)
	}
}

// How big a batch may be is an administrator's decision, so it travels the same
// road as every other bound on the Gateway: written on the form, validated,
// stored, handed back. It used to be a constant in the source, which meant a
// deployment on a vendor with a tighter rate limit could not have its own
// without a build.
func TestGatewayBatchSizeIsASetting(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": "default", "name": "Gateway", "max_fleet_agents": 6,
	})
	env.expectStatus(rec, http.StatusCreated)

	var created struct {
		ID             int64 `json:"id"`
		MaxFleetAgents *int  `json:"max_fleet_agents"`
	}
	env.decode(rec, &created)
	if created.MaxFleetAgents == nil || *created.MaxFleetAgents != 6 {
		t.Fatalf("the batch size was not stored: %v", created.MaxFleetAgents)
	}

	// Cleared means "use the default", not "use nothing".
	rec = env.do(http.MethodPut, "/v1/agents/"+itoa(created.ID), token, map[string]any{
		"name": "Gateway", "max_fleet_agents": nil,
	})
	env.expectStatus(rec, http.StatusOK)
	var updated struct {
		MaxFleetAgents *int `json:"max_fleet_agents"`
	}
	env.decode(rec, &updated)
	if updated.MaxFleetAgents != nil {
		t.Fatalf("clearing the batch size left %v", *updated.MaxFleetAgents)
	}

	// A number nobody could have meant is refused, with the range said out loud
	// rather than a bare "invalid".
	env.expectFields(env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"name": "Too many", "max_fleet_agents": model.MaxFleetAgents + 1,
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"max_fleet_agents": fmt.Sprintf("must be between %d and %d",
			model.MinFleetAgents, model.MaxFleetAgents),
	})
}

// How hard to think is chosen on the AGENT, because it is a property of the job
// and one model serves agents doing different ones. The bag was created by
// migration 56 and never wired: this pins the round trip.
func TestAnAgentKeepsItsChosenEffort(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("effort@test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("effort@test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"name": "Researcher", "reasoning": true,
		"settings": map[string]string{"reasoning_effort": "max"},
	})
	env.expectStatus(rec, http.StatusCreated)
	var created agentBody
	env.decode(rec, &created)
	if created.Settings["reasoning_effort"] != "max" {
		t.Fatalf("effort %q on create, want max", created.Settings["reasoning_effort"])
	}

	rec = env.do(http.MethodGet, "/v1/agents/"+strconv.FormatInt(created.ID, 10)+"/form", token, nil)
	env.expectStatus(rec, http.StatusOK)

	// Two agents on ONE model wanting different efforts is the case the model
	// level could not express, so it is the case worth pinning.
	rec = env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"name": "Classifier", "reasoning": true,
		"settings": map[string]string{"reasoning_effort": "low"},
	})
	env.expectStatus(rec, http.StatusCreated)
	var second agentBody
	env.decode(rec, &second)
	if second.Settings["reasoning_effort"] != "low" {
		t.Fatalf("effort %q on the second agent, want low", second.Settings["reasoning_effort"])
	}
	if created.Settings["reasoning_effort"] == second.Settings["reasoning_effort"] {
		t.Fatal("two agents ended up sharing one effort")
	}
}

// A projected tool is read under the service it came from, by its own name.
//
// The prefix exists so two services can each offer a `query` and stay two
// tools; it is not something to hand a person and ask them to decode. Every
// screen that shows a tool shows the heading and the short name together, and
// the full name stays what the model calls and what a grant records.
func TestAProjectedToolIsReadUnderItsServiceAndWithoutThePrefix(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	server := &model.MCPServer{
		WorkspaceID: env.ws.ID, Name: "NLI", URL: "https://nli.test/mcp",
		AuthType: model.MCPAuthNone, Status: model.StatusActive, ToolPrefix: "nli",
	}
	if err := env.app.Store.MCPServers().Create(context.Background(), server); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if _, err := env.app.Store.Tools().SyncMCPTools(context.Background(), env.ws.ID, server.ID, []*model.Tool{{
		WorkspaceID: env.ws.ID, Name: "nli_update_entity", Kind: "mcp",
		FriendlyName: "Update Entities", Description: "Change a record.",
		Risk: string(tool.RiskExternalCommunication), MCPServerID: &server.ID,
		RemoteName: "update_entity", DefinitionHash: "hash-a", Status: model.StatusActive,
	}}); err != nil {
		t.Fatalf("project the tool: %v", err)
	}

	// The catalogue.
	rec := env.do(http.MethodGet, "/v1/tools", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var tools []toolBody
	env.decode(rec, &tools)
	var projected toolBody
	for _, candidate := range tools {
		if candidate.Name == "nli_update_entity" {
			projected = candidate
		}
		// Ours keeps its whole name: there is no prefix on it to take off, and
		// trimming one would be inventing a namespace nobody asked for.
		if candidate.Name == "current_time" && candidate.ShortName != "current_time" {
			t.Fatalf("a built-in was renamed: %+v", candidate)
		}
	}
	if projected.ID == 0 {
		t.Fatalf("the projected tool was not in the catalogue: %+v", tools)
	}
	if projected.Source != "NLI" || projected.ShortName != "update_entity" {
		t.Fatalf("the catalogue reads it as %q under %q", projected.ShortName, projected.Source)
	}

	// The agent form, where the same tool is a checkbox.
	rec = env.do(http.MethodGet, "/v1/agents/form", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var form agentFormBody
	env.decode(rec, &form)
	found := false
	for _, choice := range form.Tools {
		if choice.Name != "nli_update_entity" {
			continue
		}
		found = true
		if choice.Source != "NLI" || choice.ShortName != "update_entity" {
			t.Fatalf("the agent form reads it as %q under %q", choice.ShortName, choice.Source)
		}
	}
	if !found {
		t.Fatalf("the agent form did not offer the projected tool: %+v", form.Tools)
	}

	// Our own MCP server's exposure list, where a relayed tool is offered on.
	rec = env.do(http.MethodGet, "/v1/mcp-server", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var screen mcpScreenBody
	env.decode(rec, &screen)
	found = false
	for _, row := range screen.Tools {
		if row.Name != "nli_update_entity" {
			continue
		}
		found = true
		if row.Source != "NLI" || row.ShortName != "update_entity" {
			t.Fatalf("the exposure list reads it as %q under %q", row.ShortName, row.Source)
		}
	}
	if !found {
		t.Fatalf("the exposure list did not offer the projected tool: %+v", screen.Tools)
	}
}
