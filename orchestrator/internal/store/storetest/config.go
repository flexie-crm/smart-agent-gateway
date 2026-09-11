package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// The layered configuration model in the database: the tools a workspace has,
// the agents that use them, and the workflows that override the defaults.

func codeTools(workspaceID int64) []*model.Tool {
	return []*model.Tool{
		{
			WorkspaceID:  workspaceID,
			Name:         "current_time",
			Kind:         "builtin",
			FriendlyName: "Current time",
			Description:  "What day is it.",
			InputSchema:  json.RawMessage(`{"type":"object"}`),
			Risk:         "read_only",
		},
		{
			WorkspaceID:      workspaceID,
			Name:             "send_invoice",
			Kind:             "builtin",
			FriendlyName:     "Send an invoice",
			Description:      "Bill a customer.",
			Risk:             "financial_action",
			RequiresApproval: true,
		},
	}
}

func testToolSync(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "tools")

	if err := st.Tools().Sync(ctx, ws.ID, codeTools(ws.ID)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	tools, err := st.Tools().List(ctx, ws.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected the code's two tools, got %d", len(tools))
	}
	if tools[0].Name != "current_time" || tools[0].Status != model.StatusActive {
		t.Fatalf("unexpected tool: %+v", tools[0])
	}
	if !tools[1].RequiresApproval {
		t.Fatal("a financial tool must arrive requiring approval")
	}

	// An administrator switches one off, restricts the other to a group, and
	// adds friction to the one that had none.
	group := &model.Group{WorkspaceID: ws.ID, Name: "finance"}
	if err := st.Groups().Create(ctx, group); err != nil {
		t.Fatalf("create group: %v", err)
	}
	clock, invoice := tools[0], tools[1]
	clock.RequiresApproval = true
	if err := st.Tools().Update(ctx, clock); err != nil {
		t.Fatalf("update tool: %v", err)
	}
	invoice.Status = model.StatusDisabled
	invoice.Grants = []int64{group.ID}
	if err := st.Tools().Update(ctx, invoice); err != nil {
		t.Fatalf("update tool: %v", err)
	}

	// A deploy runs. It carries new copy for both tools, and it must not undo
	// any of the three decisions above.
	next := codeTools(ws.ID)
	next[0].Description = "Rewritten by a release."
	next[0].RequiresApproval = false
	next[1].Description = "Also rewritten."
	if err := st.Tools().Sync(ctx, ws.ID, next); err != nil {
		t.Fatalf("resync: %v", err)
	}

	tools, err = st.Tools().List(ctx, ws.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("sync created duplicates: %d rows", len(tools))
	}
	if tools[0].Description != "Rewritten by a release." {
		t.Fatal("the code owns the description, and the deploy did not deliver it")
	}
	if !tools[0].RequiresApproval {
		t.Fatal("a deploy removed the approval an administrator added")
	}
	if tools[1].Status != model.StatusDisabled {
		t.Fatal("a deploy re-enabled a tool an administrator switched off")
	}
	if len(tools[1].Grants) != 1 || tools[1].Grants[0] != group.ID {
		t.Fatalf("a deploy dropped the grants: %+v", tools[1].Grants)
	}
}

func testToolGrants(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "grants")
	if err := st.Tools().Sync(ctx, ws.ID, codeTools(ws.ID)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	tools, err := st.Tools().List(ctx, ws.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	clock, invoice := tools[0], tools[1]

	finance := &model.Group{WorkspaceID: ws.ID, Name: "finance"}
	if err := st.Groups().Create(ctx, finance); err != nil {
		t.Fatalf("create group: %v", err)
	}
	insider := mustUser(t, st, ws.ID, "insider@acme.test")
	outsider := mustUser(t, st, ws.ID, "outsider@acme.test")
	if err := st.Groups().AddMember(ctx, finance.ID, insider.ID); err != nil {
		t.Fatalf("add member: %v", err)
	}

	// Before any grant exists, both tools are open to the workspace: a
	// workspace that never opens the admin UI still has working tools.
	for _, u := range []*model.User{insider, outsider} {
		allowed, err := st.Tools().ListForUser(ctx, ws.ID, u.ID)
		if err != nil {
			t.Fatalf("list for user: %v", err)
		}
		if len(allowed) != 2 {
			t.Fatalf("an ungranted tool must be open to everyone, %s got %d", u.Email, len(allowed))
		}
	}

	// The first grant makes the list exclusive.
	invoice.Grants = []int64{finance.ID}
	if err := st.Tools().Update(ctx, invoice); err != nil {
		t.Fatalf("grant: %v", err)
	}

	allowed, err := st.Tools().ListForUser(ctx, ws.ID, insider.ID)
	if err != nil {
		t.Fatalf("list for user: %v", err)
	}
	if len(allowed) != 2 {
		t.Fatalf("the granted user lost a tool: %d", len(allowed))
	}
	allowed, err = st.Tools().ListForUser(ctx, ws.ID, outsider.ID)
	if err != nil {
		t.Fatalf("list for user: %v", err)
	}
	if len(allowed) != 1 || allowed[0].Name != clock.Name {
		t.Fatalf("the ungranted user can still reach a restricted tool: %+v", allowed)
	}

	// A disabled tool is gone for everyone, grant or no grant.
	clock.Status = model.StatusDisabled
	if err := st.Tools().Update(ctx, clock); err != nil {
		t.Fatalf("disable: %v", err)
	}
	allowed, err = st.Tools().ListForUser(ctx, ws.ID, insider.ID)
	if err != nil {
		t.Fatalf("list for user: %v", err)
	}
	if len(allowed) != 1 || allowed[0].Name != "send_invoice" {
		t.Fatalf("a disabled tool survived: %+v", allowed)
	}
}

func testAgents(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "agents")
	if err := st.Tools().Sync(ctx, ws.ID, codeTools(ws.ID)); err != nil {
		t.Fatalf("sync tools: %v", err)
	}

	// A workspace with no default package is the normal state of a new
	// workspace, and asking for it is not an error to log.
	_, err := st.Agents().GetByKey(ctx, ws.ID, model.DefaultAgentKey)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an absent default agent must be ErrNotFound, got %v", err)
	}

	// A real model, because an agent cannot pin one that does not exist: the
	// schema refuses it, which is the whole point of the constraint.
	vendor := mustVendor(t, st, ws.ID, "anthropic", nil)
	aiModel := mustAIModel(t, st, ws.ID, vendor.ID, "claude-x")
	modelID := aiModel.ID
	brain := brainWith(t, st, ws.ID, "Memory")

	maxIter := 250
	bgTimeout := 15 * time.Minute
	agent := &model.Agent{
		WorkspaceID:       ws.ID,
		Key:               model.DefaultAgentKey,
		Name:              "House assistant",
		Instructions:      "Be brief.",
		ModelID:           &modelID,
		MemoryBrainID:     &brain.ID,
		Reasoning:         true,
		MaxIterations:     &maxIter,
		BackgroundTimeout: &bgTimeout,
		Tools:             []string{"current_time", "send_invoice"},
		ConfirmTools:      []string{"send_invoice"},
	}
	if err := st.Agents().Create(ctx, agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	loaded, err := st.Agents().GetByKey(ctx, ws.ID, model.DefaultAgentKey)
	if err != nil {
		t.Fatalf("get by key: %v", err)
	}
	if loaded.Instructions != "Be brief." || !loaded.Reasoning {
		t.Fatalf("agent did not round-trip: %+v", loaded)
	}
	if loaded.ModelID == nil || *loaded.ModelID != modelID {
		t.Fatalf("model not pinned: %+v", loaded.ModelID)
	}
	if loaded.MemoryBrainID == nil || *loaded.MemoryBrainID != brain.ID {
		t.Fatalf("the memory brain did not round-trip: %+v", loaded.MemoryBrainID)
	}
	if len(loaded.Tools) != 2 {
		t.Fatalf("tools did not round-trip: %+v", loaded.Tools)
	}
	if len(loaded.ConfirmTools) != 1 || loaded.ConfirmTools[0] != "send_invoice" {
		t.Fatalf("the confirm set did not round-trip: %+v", loaded.ConfirmTools)
	}
	if loaded.MaxIterations == nil || *loaded.MaxIterations != 250 {
		t.Fatalf("max iterations did not round-trip: %+v", loaded.MaxIterations)
	}
	if loaded.BackgroundTimeout == nil || *loaded.BackgroundTimeout != 15*time.Minute {
		t.Fatalf("background timeout did not round-trip: %+v", loaded.BackgroundTimeout)
	}

	// The tool list is replaced, not merged.
	loaded.Tools = []string{"send_invoice"}
	loaded.ModelID = nil
	if err := st.Agents().Update(ctx, loaded); err != nil {
		t.Fatalf("update agent: %v", err)
	}
	loaded, err = st.Agents().GetByID(ctx, ws.ID, loaded.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if len(loaded.Tools) != 1 || loaded.Tools[0] != "send_invoice" {
		t.Fatalf("the tool list was merged instead of replaced: %+v", loaded.Tools)
	}
	if loaded.ModelID != nil {
		t.Fatal("an agent must be able to stop pinning a model")
	}
	// The confirm set rides the update: it was left as it stood, so it survives.
	if len(loaded.ConfirmTools) != 1 || loaded.ConfirmTools[0] != "send_invoice" {
		t.Fatalf("the confirm set was lost on an unrelated update: %+v", loaded.ConfirmTools)
	}

	// Clearing the confirm set to empty is a real configuration, and it is
	// replaced wholesale, not merged.
	loaded.ConfirmTools = []string{}
	if err := st.Agents().Update(ctx, loaded); err != nil {
		t.Fatalf("update agent: %v", err)
	}
	loaded, err = st.Agents().GetByID(ctx, ws.ID, loaded.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if len(loaded.ConfirmTools) != 0 {
		t.Fatalf("the confirm set was not cleared: %+v", loaded.ConfirmTools)
	}

	// Naming a tool the workspace does not have stores nothing. The allow-list
	// is configuration and the registry is code, so configuration may lag a
	// deploy: it must not become a dangling row that comes alive later.
	loaded.Tools = []string{"send_invoice", "a_tool_this_build_does_not_have"}
	if err := st.Agents().Update(ctx, loaded); err != nil {
		t.Fatalf("update agent: %v", err)
	}
	loaded, err = st.Agents().GetByID(ctx, ws.ID, loaded.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if len(loaded.Tools) != 1 || loaded.Tools[0] != "send_invoice" {
		t.Fatalf("an unknown tool name was stored: %+v", loaded.Tools)
	}

	// An agent created with no key gets one derived from its name; a second
	// of the same name is disambiguated rather than rejected.
	sub := &model.Agent{WorkspaceID: ws.ID, Name: "Data Analyst"}
	if err := st.Agents().Create(ctx, sub); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if sub.Key != "data-analyst" {
		t.Fatalf("the key was not derived from the name: %q", sub.Key)
	}
	twin := &model.Agent{WorkspaceID: ws.ID, Name: "Data Analyst"}
	if err := st.Agents().Create(ctx, twin); err != nil {
		t.Fatalf("create twin agent: %v", err)
	}
	if twin.Key != "data-analyst-2" {
		t.Fatalf("a same-named agent was not disambiguated: %q", twin.Key)
	}
	// An agent with no delegation mode defaults to auto (the Gateway decides);
	// an explicit pin round-trips (KB/27).
	if got, _ := st.Agents().GetByKey(ctx, ws.ID, sub.Key); got.DelegationMode != model.DelegationModeAuto {
		t.Fatalf("an unset delegation mode did not default to auto: %q", got.DelegationMode)
	}
	// Unset run bounds stay nil (the resolver applies the code default), rather
	// than being read back as a stored zero.
	if got, _ := st.Agents().GetByKey(ctx, ws.ID, sub.Key); got.MaxIterations != nil || got.BackgroundTimeout != nil {
		t.Fatalf("unset run bounds did not round-trip as nil: iter=%v timeout=%v", got.MaxIterations, got.BackgroundTimeout)
	}
	pinned := &model.Agent{WorkspaceID: ws.ID, Name: "Fetcher", DelegationMode: model.DelegationModeBackground}
	if err := st.Agents().Create(ctx, pinned); err != nil {
		t.Fatalf("create pinned agent: %v", err)
	}
	if got, _ := st.Agents().GetByKey(ctx, ws.ID, pinned.Key); got.DelegationMode != model.DelegationModeBackground {
		t.Fatalf("the pinned delegation mode did not round-trip: %q", got.DelegationMode)
	}

	// An agent belongs to its workspace, and is invisible from another.
	other := mustWorkspace(t, st, "other-agents")
	if _, err := st.Agents().GetByID(ctx, other.ID, loaded.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an agent is visible from another workspace: %v", err)
	}
	if _, err := st.Agents().GetByKey(ctx, other.ID, model.DefaultAgentKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a default agent leaked into another workspace: %v", err)
	}

	if err := st.Agents().Delete(ctx, ws.ID, loaded.ID); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	if _, err := st.Agents().GetByID(ctx, ws.ID, loaded.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("agent survived deletion: %v", err)
	}
}

func testWorkflowVersions(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "workflows")
	author := mustUser(t, st, ws.ID, "author@acme.test")

	wf := &model.Workflow{WorkspaceID: ws.ID, Name: "Support", CreatedBy: author.ID}
	if err := st.Workflows().Create(ctx, wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	if wf.Status != model.WorkflowDraft {
		t.Fatalf("a new workflow must be a draft, got %q", wf.Status)
	}

	// Versions number themselves, in order.
	var versions []*model.WorkflowVersion
	for i := 0; i < 3; i++ {
		v := &model.WorkflowVersion{
			WorkflowID: wf.ID,
			Definition: json.RawMessage(`{"kind":"profile","profile":{"reasoning":true}}`),
			CreatedBy:  author.ID,
		}
		if err := st.Workflows().CreateVersion(ctx, ws.ID, v); err != nil {
			t.Fatalf("create version: %v", err)
		}
		if v.Version != i+1 {
			t.Fatalf("version %d numbered itself %d", i+1, v.Version)
		}
		if v.IsPublished {
			t.Fatal("a new version must not publish itself")
		}
		versions = append(versions, v)
	}

	// A draft workflow, even with versions, shapes nobody's turn.
	if err := st.Workflows().SetAssignments(ctx, ws.ID, wf.ID, []model.WorkflowAssignment{
		{MatchType: model.MatchChannel, MatchValue: model.ChannelChat},
	}); err != nil {
		t.Fatalf("set assignments: %v", err)
	}
	candidates, err := st.Workflows().Candidates(ctx, ws.ID)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatal("an unpublished workflow reached a turn")
	}

	// Publishing promotes exactly one version, and publishes the workflow.
	if err := st.Workflows().Publish(ctx, ws.ID, wf.ID, versions[1].ID); err != nil {
		t.Fatalf("publish: %v", err)
	}
	assertOnePublished(t, st, ws.ID, wf.ID, versions[1].ID)

	// Publishing another demotes the first. There is never a moment with two.
	if err := st.Workflows().Publish(ctx, ws.ID, wf.ID, versions[2].ID); err != nil {
		t.Fatalf("republish: %v", err)
	}
	assertOnePublished(t, st, ws.ID, wf.ID, versions[2].ID)

	candidates, err = st.Workflows().Candidates(ctx, ws.ID)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected one candidate, got %d", len(candidates))
	}
	if candidates[0].VersionID != versions[2].ID {
		t.Fatal("a turn would run the version that was demoted")
	}
	if len(candidates[0].Assignments) != 1 {
		t.Fatalf("the conditions did not travel with the candidate: %+v", candidates[0].Assignments)
	}

	// A workflow with no conditions is not a candidate: publishing must never
	// silently capture a whole workspace.
	if err := st.Workflows().SetAssignments(ctx, ws.ID, wf.ID, nil); err != nil {
		t.Fatalf("clear assignments: %v", err)
	}
	candidates, err = st.Workflows().Candidates(ctx, ws.ID)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatal("a workflow with no conditions was offered to a turn")
	}

	// Another workspace's published workflow is not ours.
	if err := st.Workflows().SetAssignments(ctx, ws.ID, wf.ID, []model.WorkflowAssignment{
		{MatchType: model.MatchChannel, MatchValue: model.ChannelChat},
	}); err != nil {
		t.Fatalf("set assignments: %v", err)
	}
	other := mustWorkspace(t, st, "other-workflows")
	candidates, err = st.Workflows().Candidates(ctx, other.ID)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatal("a workflow leaked across workspaces")
	}

	// Deleting takes the versions and the conditions with it.
	if err := st.Workflows().Delete(ctx, ws.ID, wf.ID); err != nil {
		t.Fatalf("delete workflow: %v", err)
	}
	remaining, err := st.Workflows().ListVersions(ctx, ws.ID, wf.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("%d versions outlived their workflow", len(remaining))
	}
	assignments, err := st.Workflows().ListAssignments(ctx, ws.ID, wf.ID)
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("%d conditions outlived their workflow", len(assignments))
	}
}

func testWorkflowAssignmentReplacement(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "assignments")
	author := mustUser(t, st, ws.ID, "author@acme.test")

	wf := &model.Workflow{WorkspaceID: ws.ID, Name: "Sales", CreatedBy: author.ID}
	if err := st.Workflows().Create(ctx, wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}

	if err := st.Workflows().SetAssignments(ctx, ws.ID, wf.ID, []model.WorkflowAssignment{
		{MatchType: model.MatchGroup, MatchValue: "5", Priority: 10},
		{MatchType: model.MatchChannel, MatchValue: model.ChannelChat},
	}); err != nil {
		t.Fatalf("set assignments: %v", err)
	}

	// Setting again replaces wholesale. A workspace must not pass through a
	// half-applied rule set.
	if err := st.Workflows().SetAssignments(ctx, ws.ID, wf.ID, []model.WorkflowAssignment{
		{MatchType: model.MatchUser, MatchValue: "9", Priority: 3},
	}); err != nil {
		t.Fatalf("replace assignments: %v", err)
	}

	assignments, err := st.Workflows().ListAssignments(ctx, ws.ID, wf.ID)
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	if len(assignments) != 1 {
		t.Fatalf("expected the conditions to be replaced, found %d", len(assignments))
	}
	if assignments[0].MatchType != model.MatchUser || assignments[0].MatchValue != "9" ||
		assignments[0].Priority != 3 {
		t.Fatalf("unexpected condition: %+v", assignments[0])
	}
}

func assertOnePublished(t *testing.T, st store.Store, workspaceID, workflowID, wantID int64) {
	t.Helper()
	versions, err := st.Workflows().ListVersions(context.Background(), workspaceID, workflowID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	var published []int64
	for _, v := range versions {
		if v.IsPublished {
			published = append(published, v.ID)
		}
	}
	if len(published) != 1 {
		t.Fatalf("expected exactly one published version, found %d", len(published))
	}
	if published[0] != wantID {
		t.Fatalf("published version %d, want %d", published[0], wantID)
	}

	wf, err := st.Workflows().GetByID(context.Background(), workspaceID, workflowID)
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	if wf.Status != model.WorkflowPublished {
		t.Fatalf("a workflow with a published version must be published, got %q", wf.Status)
	}
}
