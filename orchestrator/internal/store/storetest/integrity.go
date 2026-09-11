package storetest

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// The integrity suite reads the database directly, because what it is checking
// is the DATABASE's behaviour, not the store's. Asking the store whether the
// store cleaned up would only prove the store believes it did.
type rawAccess interface{ DB() *sql.DB }

func rawDB(t *testing.T, st store.Store) *sql.DB {
	t.Helper()
	raw, ok := st.(rawAccess)
	if !ok {
		t.Skip("this suite needs to look at the database itself")
	}
	return raw.DB()
}

func future() time.Time { return time.Now().UTC().Add(time.Hour) }

// assertGone checks that nothing in the table still belongs to the conversation.
func assertGone(t *testing.T, st store.Store, table string, sessionID int64) {
	t.Helper()
	var count int
	//nolint:gosec // the table name is a literal from this test, not from a request
	if err := rawDB(t, st).QueryRowContext(ctx(),
		"SELECT COUNT(*) FROM "+table+" WHERE session_id = ?", sessionID).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if count != 0 {
		t.Fatalf("%d rows in %s outlived the conversation they belong to", count, table)
	}
}

func assertGoneByStep(t *testing.T, st store.Store, table string, stepID int64) {
	t.Helper()
	var count int
	//nolint:gosec // the table name is a literal from this test, not from a request
	if err := rawDB(t, st).QueryRowContext(ctx(),
		"SELECT COUNT(*) FROM "+table+" WHERE step_id = ?", stepID).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if count != 0 {
		t.Fatalf("%d rows in %s outlived the step they belong to", count, table)
	}
}

// Referential integrity, enforced by the database rather than remembered by the
// application.
//
// Every one of these used to depend on a function calling the right DELETEs in
// the right order. That works until somebody adds a table, or writes a second
// delete path, or runs a query by hand. These tests assert the rules hold
// because the SCHEMA holds them, so they keep holding for code nobody has
// written yet.

// A conversation's transcript is part of the conversation. All of it.
func testConversationCascade(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	user := mustUser(t, st, ws.ID, "u@acme.test")
	chat := mustChat(t, st, ws.ID, user.ID, "Doomed")

	step := &model.AgentStep{
		SessionID: chat.ID, Seq: 0, Kind: model.StepAssistant,
		Text: "answering", Reasoning: "thinking",
		ToolCalls: []*model.ToolCall{{
			WorkspaceID: ws.ID, ToolCallID: "c1", ToolName: "t",
			Args: json.RawMessage(`{}`), Status: model.ToolCallCompleted,
		}},
	}
	if err := st.Agent().SaveStep(ctx(), step); err != nil {
		t.Fatalf("save step: %v", err)
	}
	if err := st.Agent().RecordModelCall(ctx(), &model.ModelCall{
		WorkspaceID: ws.ID, SessionID: chat.ID, ModelID: 1, Status: "completed",
	}); err != nil {
		t.Fatalf("record model call: %v", err)
	}
	if err := st.Agent().CreatePark(ctx(), &model.ParkSnapshot{
		TokenHash: "hash-cascade", WorkspaceID: ws.ID, SessionID: chat.ID, UserID: user.ID,
		ModelID: 1, ToolName: "t", ToolCallID: "c1", ToolArgs: json.RawMessage(`{}`),
		ActionHash: "a", ExpiresAt: future(),
	}); err != nil {
		t.Fatalf("create park: %v", err)
	}

	if err := st.Agent().DeleteChat(ctx(), ws.ID, user.ID, chat.UID); err != nil {
		t.Fatalf("delete chat: %v", err)
	}

	// The transcript, the cost record and the parked approval are all gone,
	// because each of them IS the conversation, not something that merely
	// pointed at it.
	assertGone(t, st, "agent_steps", chat.ID)
	assertGone(t, st, "agent_messages", chat.ID)
	assertGone(t, st, "agent_tool_calls", chat.ID)
	assertGone(t, st, "model_calls", chat.ID)
	assertGone(t, st, "agent_park_snapshots", chat.ID)
	assertGoneByStep(t, st, "agent_reasoning", step.ID)
}

// Deleting a workspace deletes the workspace. Everything in it was in it.
func testWorkspaceCascade(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "doomed")
	user := mustUser(t, st, ws.ID, "u@doomed.test")
	group := mustGroup(t, st, ws.ID, "people")
	chat := mustChat(t, st, ws.ID, user.ID, "Chat")

	if err := st.Tools().Sync(ctx(), ws.ID, codeTools(ws.ID)); err != nil {
		t.Fatalf("sync tools: %v", err)
	}
	if err := st.Agents().Create(ctx(), &model.Agent{
		WorkspaceID: ws.ID, Key: model.DefaultAgentKey, Name: "House",
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	wf := &model.Workflow{WorkspaceID: ws.ID, Name: "Flow", CreatedBy: user.ID}
	if err := st.Workflows().Create(ctx(), wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}

	// Nothing here is deleted by hand. The workspace goes, and its contents go
	// with it, because that is what "in a workspace" means.
	if _, err := rawDB(t, st).ExecContext(ctx(),
		`DELETE FROM workspaces WHERE id = ?`, ws.ID); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}

	for table, id := range map[string]int64{
		"user_groups_def": group.ID,
		"agent_sessions":  chat.ID,
		"workflows":       wf.ID,
	} {
		var count int
		//nolint:gosec // the table name is a literal from this test, not from a request
		if err := rawDB(t, st).QueryRowContext(ctx(),
			"SELECT COUNT(*) FROM "+table+" WHERE id = ?", id).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s outlived its workspace", table)
		}
	}

	// The PERSON survives, because a person is not part of a workspace: they
	// belong to the tenant and were a member of it. What dies with the workspace
	// is the membership, and a colleague who loses one workspace does not lose
	// their account.
	var alive int
	if err := rawDB(t, st).QueryRowContext(ctx(),
		`SELECT COUNT(*) FROM users WHERE id = ?`, user.ID).Scan(&alive); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if alive != 1 {
		t.Fatalf("the person was deleted with the workspace they were a member of")
	}
	var memberships int
	if err := rawDB(t, st).QueryRowContext(ctx(),
		`SELECT COUNT(*) FROM workspace_members WHERE workspace_id = ?`, ws.ID).Scan(&memberships); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if memberships != 0 {
		t.Fatalf("a membership outlived its workspace")
	}

	var tools int
	if err := rawDB(t, st).QueryRowContext(ctx(),
		`SELECT COUNT(*) FROM tools WHERE workspace_id = ?`, ws.ID).Scan(&tools); err != nil {
		t.Fatalf("count tools: %v", err)
	}
	if tools != 0 {
		t.Fatalf("%d tools outlived their workspace", tools)
	}
}

// A conversation REFERS to the agent that shaped it, and outlives it. Deleting
// an assistant must not delete what people said to it: it must only stop
// claiming they were shaped by something that no longer exists.
func testDeletingAnAgentDoesNotDeleteItsConversations(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	user := mustUser(t, st, ws.ID, "u@acme.test")

	agent := &model.Agent{WorkspaceID: ws.ID, Key: model.DefaultAgentKey, Name: "House"}
	if err := st.Agents().Create(ctx(), agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	session := &model.AgentSession{
		WorkspaceID: ws.ID, UserID: user.ID, Channel: model.ChannelChat,
		Title: "Shaped by the agent", AgentID: &agent.ID,
	}
	if err := st.Agent().CreateSession(ctx(), session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := st.Agents().Delete(ctx(), ws.ID, agent.ID); err != nil {
		t.Fatalf("delete agent: %v", err)
	}

	kept, err := st.Agent().GetSession(ctx(), ws.ID, session.ID)
	if err != nil {
		t.Fatalf("the conversation died with the agent that shaped it: %v", err)
	}
	if kept.AgentID != nil {
		t.Fatalf("the conversation still claims an agent that no longer exists: %v", *kept.AgentID)
	}
	if kept.Title != "Shaped by the agent" {
		t.Fatalf("the conversation was damaged: %+v", kept)
	}
}

// A row that points at a parent which does not exist cannot be written. It used
// to be possible, and it produced data that looked fine until something tried to
// follow the reference.
func testAnOrphanCannotBeCreated(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")

	// A conversation belonging to a user who does not exist.
	err := st.Agent().CreateSession(ctx(), &model.AgentSession{
		WorkspaceID: ws.ID, UserID: 999999, Channel: model.ChannelChat,
	})
	if err == nil {
		t.Fatal("a conversation was created for a user who does not exist")
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an orphan should read as a missing reference, got: %v", err)
	}

	// A step belonging to a conversation that does not exist.
	if err := st.Agent().SaveStep(ctx(), &model.AgentStep{
		SessionID: 999999, Seq: 0, Kind: model.StepUser, Text: "hello?",
	}); err == nil {
		t.Fatal("a step was written into a conversation that does not exist")
	}

	// An agent pinning a model that does not exist.
	missing := int64(999999)
	if err := st.Agents().Create(ctx(), &model.Agent{
		WorkspaceID: ws.ID, Key: "ghost", Name: "Ghost", ModelID: &missing,
	}); err == nil {
		t.Fatal("an agent was pinned to a model that does not exist")
	}
}
