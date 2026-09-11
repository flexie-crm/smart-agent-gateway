package api

import (
	"context"
	"testing"
	"time"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
)

// pointModelAt re-aims the model's vendor at a different fake server. A test
// that needs the model id inside the scripted tool arguments has to create the
// model first, so the vendor it talks to is swapped afterwards.
func (e *testEnv) pointModelAt(modelID int64, f *fakeVendor) {
	e.t.Helper()
	ctx := context.Background()

	m, err := e.app.Store.AIModels().GetByID(ctx, e.ws.ID, modelID)
	if err != nil {
		e.t.Fatalf("get model: %v", err)
	}
	vendor, err := e.app.Store.Vendors().GetByID(ctx, e.ws.ID, m.VendorID)
	if err != nil {
		e.t.Fatalf("get vendor: %v", err)
	}
	vendor.BaseURL = f.server.URL
	if err := e.app.Store.Vendors().Update(ctx, vendor); err != nil {
		e.t.Fatalf("update vendor: %v", err)
	}
}

// chatIDOf is the public id a stream announced. It is opaque: the test knows
// as little about it as a client does.
func chatIDOf(t *testing.T, frames []chat.Frame) string {
	t.Helper()
	created := framesOfType(frames, chat.FrameChatCreated)
	if len(created) == 0 {
		t.Fatalf("the stream never announced a chat id: %+v", frames)
	}
	return created[0].ChatID
}

// sessionOf resolves the row behind a conversation, which only the server may
// do. A test asserting on stored rows needs it; a client never sees it.
func (e *testEnv) sessionOf(frames []chat.Frame) *model.AgentSession {
	e.t.Helper()
	session, err := e.app.Store.Agent().GetSessionByUID(context.Background(), e.ws.ID, chatIDOf(e.t, frames))
	if err != nil {
		e.t.Fatalf("resolve chat: %v", err)
	}
	return session
}

// toolCalls reads back what the runtime recorded. This is the audit trail a
// customer is shown, so a test that claims a tool ran should read the row that
// proves it. It goes through the transcript, which is the one read path.
func (e *testEnv) toolCalls(sessionID int64) []*model.ToolCall {
	e.t.Helper()
	steps, err := e.app.Store.Agent().Transcript(context.Background(), sessionID)
	if err != nil {
		e.t.Fatalf("load transcript: %v", err)
	}
	out := []*model.ToolCall{}
	for _, step := range steps {
		out = append(out, step.ToolCalls...)
	}
	return out
}

// messageRows counts the rows in the message table. A tool result stored as a
// message would show up here, which is exactly what must never happen: the
// result belongs to its call, and one copy is the whole point.
func (e *testEnv) messageRows(sessionID int64) int {
	e.t.Helper()
	var count int
	if err := e.sql.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM agent_messages WHERE session_id = ?`, sessionID).Scan(&count); err != nil {
		e.t.Fatalf("count messages: %v", err)
	}
	return count
}

// registerReasoningModel points a reasoning-capable vendor at the fake server
// and switches reasoning on in the workspace's default agent, which is where
// reasoning lives: it is a configuration decision about how the assistant
// thinks, not a flag a caller sets per message.
func (e *testEnv) registerReasoningModel(f *fakeVendor) int64 {
	e.t.Helper()
	ctx := context.Background()

	vendor := &model.AIVendor{
		WorkspaceID: e.ws.ID,
		VendorKey:   model.VendorDeepSeek,
		Name:        "Fake thinker",
		BaseURL:     f.server.URL,
	}
	if err := e.app.Store.Vendors().Create(ctx, vendor); err != nil {
		e.t.Fatalf("create vendor: %v", err)
	}
	m := &model.AIModel{
		WorkspaceID: e.ws.ID, VendorID: vendor.ID, ModelKey: "fake-thinker",
		Type: model.ModelTypeChat, ContextWindow: 100_000,
	}
	if err := e.app.Store.AIModels().Create(ctx, m); err != nil {
		e.t.Fatalf("create model: %v", err)
	}
	if err := e.app.Store.Agents().Create(ctx, &model.Agent{
		WorkspaceID: e.ws.ID,
		Key:         model.DefaultAgentKey,
		Name:        "Thinker",
		Reasoning:   true,
		Tools:       e.app.DefaultTools(),
	}); err != nil {
		e.t.Fatalf("configure reasoning: %v", err)
	}
	return m.ID
}

// GatewayAgent gives the workspace a configured main agent so the chat answers
// through the model instead of the "no assistant set up" notice. It carries the
// default tool set explicitly, so the turn behaves exactly as the old always-on
// floor did (a configured agent's tools are its own now, never inherited).
func (e *testEnv) GatewayAgent() {
	e.t.Helper()
	if err := e.app.Store.Agents().Create(context.Background(), &model.Agent{
		WorkspaceID: e.ws.ID, Key: model.DefaultAgentKey, Name: "Assistant",
		Tools: e.app.DefaultTools(),
	}); err != nil {
		e.t.Fatalf("create Gateway agent: %v", err)
	}
}

// registerModelAt points a vendor row at an arbitrary URL, for a fake that is
// not the shared one.
func (e *testEnv) registerModelAt(baseURL string) int64 {
	e.t.Helper()
	ctx := context.Background()

	vendor := &model.AIVendor{
		WorkspaceID: e.ws.ID,
		VendorKey:   model.VendorOpenAICompatible,
		Name:        "Fake",
		BaseURL:     baseURL,
	}
	if err := e.app.Store.Vendors().Create(ctx, vendor); err != nil {
		e.t.Fatalf("create vendor: %v", err)
	}
	m := &model.AIModel{
		WorkspaceID: e.ws.ID, VendorID: vendor.ID, ModelKey: "fake-1",
		Type: model.ModelTypeChat, ContextWindow: 100_000,
	}
	if err := e.app.Store.AIModels().Create(ctx, m); err != nil {
		e.t.Fatalf("create model: %v", err)
	}
	return m.ID
}

func (e *testEnv) sessionByUID(uid string) *model.AgentSession {
	e.t.Helper()
	session, err := e.app.Store.Agent().GetSessionByUID(context.Background(), e.ws.ID, uid)
	if err != nil {
		e.t.Fatalf("resolve chat: %v", err)
	}
	return session
}

// waitForRun blocks until the conversation's turn is over AND its record says
// so. A turn now outlives the request that started it, so a test asserting on
// what it wrote has to wait for it rather than assume the response ending meant
// the work did. It waits for the row, not just for the goroutine: the run stops
// emitting frames a moment before its outcome is written down.
func (e *testEnv) waitForRun(sessionID int64) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := e.app.Runs.Latest(sessionID); !ok {
			return
		}
		if e.runStatus(sessionID) != model.RunRunning {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("the turn never finished")
}

// runStatus reads how the conversation's last turn ended.
func (e *testEnv) runStatus(sessionID int64) string {
	e.t.Helper()
	var status string
	if err := e.sql.DB().QueryRowContext(context.Background(),
		`SELECT status FROM agent_runs WHERE session_id = ? ORDER BY id DESC LIMIT 1`,
		sessionID).Scan(&status); err != nil {
		e.t.Fatalf("read run status: %v", err)
	}
	return status
}

// waitForTitle blocks until the conversation has been named.
//
// Naming is a model call of its own, made in the background once the answer is
// away. It costs tokens, so anything asserting on what a turn cost has to wait
// for it: otherwise the numbers depend on how loaded the machine is, which is
// the definition of a flaky test.
func (e *testEnv) waitForTitle(sessionID int64) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		session, err := e.app.Store.Agent().GetSession(context.Background(), e.ws.ID, sessionID)
		if err == nil && session.Title != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("the conversation was never named")
}
