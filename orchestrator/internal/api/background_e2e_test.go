package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
)

// The browser-like proof of Mode C (KB/27): the Gateway hands a heavy task to a
// agent that runs in the BACKGROUND, says "I've started it," and the turn
// ENDS. The person keeps chatting. When the agent finishes, a
// server-initiated completion turn narrates the result. These drive the real SSE
// endpoint and read the real frames, and they prove the two hard invariants: the
// agent's technical result never leaks into the chat, and a completion never
// barges an in-flight user turn.

// --- helpers ------------------------------------------------------------------

// seededChat creates a conversation that already has a title, so no background
// naming call fires. That keeps the shared fake vendor's turn counter exact:
// every model call in the test is one the test scripted, in order.
func (e *testEnv) seededChat(userID int64) *model.AgentSession {
	e.t.Helper()
	s := &model.AgentSession{
		WorkspaceID: e.ws.ID,
		UserID:      userID,
		Channel:     model.ChannelChat,
		Title:       "Seeded",
	}
	if err := e.app.Store.Agent().CreateSession(context.Background(), s); err != nil {
		e.t.Fatalf("seed chat: %v", err)
	}
	return s
}

// agentOnURL configures an agent pinned to its OWN model and vendor, so
// its model calls never share a turn counter with the Gateway's: the two fakes
// are separate servers, and their calls cannot interleave into one script.
func (e *testEnv) agentOnURL(key, name, baseURL string, tools ...string) {
	e.t.Helper()
	ctx := context.Background()
	vendor := &model.AIVendor{
		WorkspaceID: e.ws.ID, VendorKey: model.VendorOpenAICompatible,
		Name: name + " vendor", BaseURL: baseURL,
	}
	if err := e.app.Store.Vendors().Create(ctx, vendor); err != nil {
		e.t.Fatalf("create agent vendor: %v", err)
	}
	m := &model.AIModel{
		WorkspaceID: e.ws.ID, VendorID: vendor.ID, ModelKey: "spec-" + key,
		Type: model.ModelTypeChat, ContextWindow: 100_000,
	}
	if err := e.app.Store.AIModels().Create(ctx, m); err != nil {
		e.t.Fatalf("create agent model: %v", err)
	}
	mid := m.ID
	if err := e.app.Store.Agents().Create(ctx, &model.Agent{
		WorkspaceID: e.ws.ID, Key: key, Name: name,
		Instructions: "Do the work.", Status: model.StatusActive,
		ModelID: &mid, Tools: tools,
	}); err != nil {
		e.t.Fatalf("create agent: %v", err)
	}
}

// latestDelegationID reads the id of the session's most recent delegation row,
// which the test needs to watch the background record change state.
func (e *testEnv) latestDelegationID(sessionID int64) int64 {
	e.t.Helper()
	var id int64
	if err := e.sql.DB().QueryRowContext(context.Background(),
		`SELECT id FROM agent_delegations WHERE session_id = ? ORDER BY id DESC LIMIT 1`,
		sessionID).Scan(&id); err != nil {
		e.t.Fatalf("read delegation id: %v", err)
	}
	return id
}

// waitForAgentCall blocks until the Gateway's agent chip reaches a status.
// A background agent resolves it from its own goroutine, after the turn
// that started it has ended, so a test that read the row the instant the stream
// closed would be racing the thing it is asserting about.
func (e *testEnv) waitForAgentCall(sessionID int64, status string) *model.ToolCall {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, c := range e.toolCalls(sessionID) {
			if c.ToolName == model.DelegateToolName && c.Status == status {
				return c
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("no delegate call for session %d reached %q", sessionID, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForDelegation blocks until a delegation reaches a status, so a test asserts
// on the background record only once the goroutine has actually moved it.
func (e *testEnv) waitForDelegation(id int64, status string) *model.AgentDelegation {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d, err := e.app.Store.Agent().GetDelegation(context.Background(), id)
		if err == nil && d.Status == status {
			return d
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("delegation %d never reached %q", id, status)
	return nil
}

// completionFrames waits for the server-initiated completion turn and returns its
// frames. The completion is a normal detached run, so it becomes the session's
// latest; it is identified by the Gateway's narration, which no earlier run
// carries.
func (e *testEnv) completionFrames(sessionID int64, narration string) []chat.Frame {
	e.t.Helper()

	// The wait is generous because of what it covers. The delegation being done,
	// which is what the caller waited for, happens BEFORE the completion turn is
	// scheduled: after it comes a full profile resolution, the system prompt
	// assembled from live context, and a model call, each of them several queries.
	// On a machine running the whole suite at once that is not a fast second.
	deadline := time.Now().Add(60 * time.Second)
	started := time.Now()
	for time.Now().Before(deadline) {
		if r, ok := e.app.Runs.Latest(sessionID); ok && r.Done() {
			frames := r.Frames()
			if strings.Contains(allDeltaText(frames), narration) {
				// How long this really took is the whole question when it fails
				// only under load, so it is recorded even when it passes.
				e.t.Logf("completion turn arrived after %s", time.Since(started).Round(time.Millisecond))
				return frames
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Say what was actually found, so a failure separates a turn that ran and
	// narrated something else from one that never ran at all.
	state := "no run for this session"
	if r, ok := e.app.Runs.Latest(sessionID); ok {
		state = fmt.Sprintf("latest run done=%v, text=%q", r.Done(), allDeltaText(r.Frames()))
	}
	session, err := e.app.Store.Agent().GetSession(context.Background(), e.ws.ID, sessionID)
	status := "unknown"
	if err == nil {
		status = session.Status
	}
	e.t.Fatalf("the completion turn never narrated %q after %s (%s; session status=%q)",
		narration, time.Since(started).Round(time.Millisecond), state, status)
	return nil
}

// waitForPendingCard blocks until a session has a confirmation card waiting. It
// keys off the durable park, not the session status, so it is immune to the race
// between a background agent parking and its Gateway turn completing.
func (e *testEnv) waitForPendingCard(sessionID int64) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := e.app.Store.Agent().PendingPark(context.Background(), sessionID); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("no confirmation card ever surfaced")
}

// historyCard reloads a conversation and returns the pending card the client
// would render, proving the card survives without a live stream (the durable
// path a background agent's approval leans on).
func (e *testEnv) historyCard(token, chatUID string) historyConfirm {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/v1/chat/history", token, map[string]string{"chat_id": chatUID})
	e.expectStatus(rec, http.StatusOK)
	var resp historyResponse
	e.decode(rec, &resp)
	for _, m := range resp.Messages {
		if m.Confirm != nil {
			return *m.Confirm
		}
	}
	e.t.Fatalf("no pending card in history: %+v", resp.Messages)
	return historyConfirm{}
}

// gatedVendor is a fake model that blocks on its first call until released, so a
// test can hold a background agent mid-run and observe the window in which
// the delegation is running while the person keeps chatting.
type gatedVendor struct {
	server  *httptest.Server
	hit     chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedVendor) unblock() { g.once.Do(func() { close(g.release) }) }

func newGatedVendor(t *testing.T, reply []string) *gatedVendor {
	t.Helper()
	g := &gatedVendor{hit: make(chan struct{}, 1), release: make(chan struct{})}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case g.hit <- struct{}{}:
		default:
		}
		<-g.release
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range reply {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	// Cleanup releases before closing, so a blocked handler never wedges the
	// server's shutdown when a test fails before it releases.
	t.Cleanup(func() { g.unblock(); g.server.Close() })
	return g
}

// --- the happy path -----------------------------------------------------------

// A background delegation, watched frame by frame: the Gateway says it started and
// the turn ENDS with a result (no card); the agent runs off-stream; when it
// finishes, the Gateway narrates the result. The agent's raw technical result
// never appears in any frame of either turn.
func TestBackgroundDelegationRunsAndNarratesEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	// The agent reports a technical result carrying unmistakable markers, so
	// a leak into the chat would be obvious.
	specVendor := newFakeVendor(t, answerChunks("TECHNICAL vendor_ids=5,6,7 payload=RAWXYZ"))
	env.agentOnURL("research", "Research", specVendor.server.URL)

	Gateway := newFakeVendor(t,
		GatewayDelegatesWith("I'll have Research look into that.", "research", "find the top three vendors", model.HandoffBackground),
		answerChunks("I've started Research in the background; I'll let you know when it is done."),
		answerChunks("Research is finished. Your top three vendors are ready for you."),
	)
	modelID := env.registerModel(Gateway)
	env.GatewayAgent()

	session := env.seededChat(user.ID)

	// ── The delegate turn: the Gateway narrates the hand-off; the turn ends. ──
	frames := env.streamTurn(token, map[string]any{
		"prompt": "find the top three vendors", "model_id": modelID, "chat_id": session.UID,
	})

	if got := taggedDeltas(frames, false); !strings.Contains(got, "I've started Research in the background") {
		t.Fatalf("the Gateway did not narrate the hand-off: %q", got)
	}
	start := framesOfType(frames, chat.FrameAgentStart)
	if len(start) != 1 {
		t.Fatalf("the background delegation was not announced: %+v", frames)
	}
	if msg := agentMessage(t, start[0]); msg.Agent != "research" || msg.Mode != model.HandoffBackground {
		t.Fatalf("the delegation start is wrong: %+v", msg)
	}
	if len(framesOfType(frames, chat.FrameAgentEnd)) != 1 {
		t.Fatalf("the delegation bracket was not closed: %+v", frames)
	}
	// A background delegation does NOT park: the turn ends with a result.
	if last := frames[len(frames)-1]; last.Type != chat.FrameResult || !last.Final {
		t.Fatalf("the delegate turn did not end with a result: %+v", last)
	}
	// The Gateway's agent chip is completed and carries the background flag, so a
	// reload can tell it apart from an inline hand-off. The agent resolves it
	// from its own goroutine after the turn has already ended, so this waits for
	// it rather than reading it the instant the stream closes.
	subCall := env.waitForAgentCall(session.ID, model.ToolCallCompleted)
	if !strings.Contains(string(subCall.Result), `"background":true`) {
		t.Fatalf("the delegation result is not marked background: %s", subCall.Result)
	}

	// The durable record exists and runs to done.
	delID := env.latestDelegationID(session.ID)
	env.waitForDelegation(delID, model.DelegationDone)

	// ── The completion turn: server-initiated, the Gateway narrates the result. ──
	completion := env.completionFrames(session.ID, "Your top three vendors are ready for you.")
	if fr := completion[len(completion)-1]; fr.Type != chat.FrameResult || !fr.Final {
		t.Fatalf("the completion turn did not end with a result: %+v", fr)
	}

	// Across BOTH turns, the agent's technical result never reaches the chat.
	everything := append(append([]chat.Frame{}, frames...), completion...)
	leaked := allDeltaText(everything)
	for _, marker := range []string{"TECHNICAL", "vendor_ids", "RAWXYZ"} {
		if strings.Contains(leaked, marker) {
			t.Fatalf("the agent's technical result leaked into the chat (%q): %q", marker, leaked)
		}
	}
	// And no agent-tagged delta ever streamed.
	if got := taggedDeltas(everything, true); got != "" {
		t.Fatalf("an agent delta was streamed: %q", got)
	}
}

// --- interleaving -------------------------------------------------------------

// The person keeps chatting while a background agent runs, and the Gateway's
// completion turn lands AFTER that user turn, not in the middle of it. The
// agent is gated so the delegation is provably in flight when the user turn
// runs. (The tight "waits behind a running turn" race is unit-tested in the run
// scheduler; here it is proven end to end through the real endpoint.)
func TestBackgroundCompletionInterleavesWithAUserTurn(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	gated := newGatedVendor(t, answerChunks("TECHNICAL gated payload=GATEDXYZ"))
	env.agentOnURL("research", "Research", gated.server.URL)

	Gateway := newFakeVendor(t,
		GatewayDelegatesWith("I'll put Research on it.", "research", "find the top three vendors", model.HandoffBackground),
		answerChunks("I've started Research in the background."),
		answerChunks("The sky is clear today."),
		answerChunks("Research is finished. The vendors are ready."),
	)
	modelID := env.registerModel(Gateway)
	env.GatewayAgent()

	session := env.seededChat(user.ID)

	// The delegate turn ends; the agent starts and blocks on the gate.
	env.streamTurn(token, map[string]any{
		"prompt": "find the top three vendors", "model_id": modelID, "chat_id": session.UID,
	})
	select {
	case <-gated.hit:
	case <-time.After(10 * time.Second):
		t.Fatal("the background agent never started")
	}

	// The delegation is provably in flight.
	running, err := env.app.Store.Agent().RunningDelegations(context.Background(), session.ID)
	if err != nil || len(running) != 1 || running[0].Status != model.DelegationRunning {
		t.Fatalf("the delegation is not running while the agent works: %v %+v", err, running)
	}

	// The person chats with the Gateway WHILE the background agent runs.
	userTurn := env.streamTurn(token, map[string]any{
		"prompt": "what's the weather", "model_id": modelID, "chat_id": session.UID,
	})
	if deltaText(userTurn) != "The sky is clear today." {
		t.Fatalf("the user turn did not answer while the delegation ran: %q", deltaText(userTurn))
	}
	userRun, ok := env.app.Runs.Latest(session.ID)
	if !ok {
		t.Fatal("the user turn left no run")
	}

	// The agent finishes; the completion turn runs, after the user turn.
	gated.unblock()
	delID := env.latestDelegationID(session.ID)
	env.waitForDelegation(delID, model.DelegationDone)
	completion := env.completionFrames(session.ID, "Research is finished. The vendors are ready.")

	// The completion is a distinct, later run than the user turn: it did not fold
	// into it.
	completionRun, ok := env.app.Runs.Latest(session.ID)
	if !ok || completionRun.UID == userRun.UID {
		t.Fatalf("the completion did not run as its own turn after the user turn: %+v", completionRun)
	}

	// The gated agent's technical result never leaked, across every frame.
	everything := append(append([]chat.Frame{}, userTurn...), completion...)
	if leaked := allDeltaText(everything); strings.Contains(leaked, "GATEDXYZ") || strings.Contains(leaked, "TECHNICAL") {
		t.Fatalf("the agent's technical result leaked into the chat: %q", leaked)
	}
}

// --- approval surface ---------------------------------------------------------

// A background agent that hits an approval-gated tool raises a real card:
// the Gateway turn already ended, the card surfaces on reload, and the Gateway's
// hand-off chip is a completed one, not a spinner. Approving it re-launches the
// agent in the background; the action runs, and the Gateway's completion turn
// narrates the result. The agent's technical result never leaks.
func TestBackgroundAgentParksAndResumesOnApproval(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	target := env.spareModel()
	specVendor := newFakeVendor(t,
		agentParkingCall(target),                                  // calls set_model_status -> parks
		answerChunks("TECHNICAL disabled model, payload=SPECXYZ"), // after approval, reports back
	)
	env.agentOnURL("ops", "Ops", specVendor.server.URL, "set_model_status")

	Gateway := newFakeVendor(t,
		GatewayDelegatesWith("I'll have Ops handle that.", "ops", "take the spare model offline", model.HandoffBackground),
		answerChunks("I've started Ops in the background."),
		answerChunks("Done, I've taken the spare model offline for you."),
	)
	modelID := env.registerModel(Gateway)
	env.GatewayAgent()

	session := env.seededChat(user.ID)

	// The delegate turn ends; the agent runs and parks for approval.
	frames := env.streamTurn(token, map[string]any{
		"prompt": "take the spare model offline", "model_id": modelID, "chat_id": session.UID,
	})
	if last := frames[len(frames)-1]; last.Type != chat.FrameResult || !last.Final {
		t.Fatalf("the delegate turn did not end cleanly: %+v", last)
	}
	env.waitForPendingCard(session.ID)

	// The Gateway's hand-off chip is COMPLETED (started), not re-stubbed as waiting.
	if subCall := agentCallRow(t, env, session.ID); subCall.Status != model.ToolCallCompleted {
		t.Fatalf("the background hand-off chip is not completed: %+v", subCall)
	}
	// The chip reads "waiting for approval" on reload, not a spinner: the parked
	// state is durable, not only a live socket push. Polled first, because each
	// history load re-mints the card token, so the token used to approve must come
	// from the LAST load.
	env.waitForChipStatus(token, session.UID, "waiting_approval")
	// The card surfaces on reload, describing the agent's action.
	card := env.historyCard(token, session.UID)
	if card.Token == "" || card.Status != "pending" {
		t.Fatalf("the reloaded card is not answerable: %+v", card)
	}
	// The action has not run yet.
	if m, _ := env.app.Store.AIModels().GetByID(context.Background(), env.ws.ID, target); m.Status != model.StatusActive {
		t.Fatal("the agent's action ran before approval")
	}

	// Approve. The card clears on this request; the agent continues in the
	// background.
	resumed := env.streamTurn(token, map[string]any{"resume_token": card.Token, "resume_action": "approved"})
	if len(framesOfType(resumed, chat.FrameConfirmResolved)) != 1 {
		t.Fatalf("the decision was not reported: %+v", resumed)
	}

	// The delegation finishes; the action ran; the Gateway narrates.
	delID := env.latestDelegationID(session.ID)
	env.waitForDelegation(delID, model.DelegationDone)
	if m, _ := env.app.Store.AIModels().GetByID(context.Background(), env.ws.ID, target); m.Status != model.StatusDisabled {
		t.Fatal("the approved action did not run")
	}
	completion := env.completionFrames(session.ID, "Done, I've taken the spare model offline for you.")

	everything := append(append([]chat.Frame{}, resumed...), completion...)
	if leaked := allDeltaText(everything); strings.Contains(leaked, "SPECXYZ") || strings.Contains(leaked, "TECHNICAL") {
		t.Fatalf("the agent's technical result leaked into the chat: %q", leaked)
	}
}

// The same, but the person REJECTS: the action never runs, the delegation ends
// with a person-safe reason, and the Gateway's completion turn tells the person it
// could not be done and reacts.
func TestBackgroundAgentParksAndEndsOnReject(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	target := env.spareModel()
	specVendor := newFakeVendor(t, agentParkingCall(target)) // parks; never continues after reject
	env.agentOnURL("ops", "Ops", specVendor.server.URL, "set_model_status")

	Gateway := newFakeVendor(t,
		GatewayDelegatesWith("I'll have Ops handle that.", "ops", "take the spare model offline", model.HandoffBackground),
		answerChunks("I've started Ops in the background."),
		answerChunks("I could not take the spare model offline because you declined it."),
	)
	modelID := env.registerModel(Gateway)
	env.GatewayAgent()

	session := env.seededChat(user.ID)

	env.streamTurn(token, map[string]any{
		"prompt": "take the spare model offline", "model_id": modelID, "chat_id": session.UID,
	})
	env.waitForPendingCard(session.ID)
	card := env.historyCard(token, session.UID)

	resumed := env.streamTurn(token, map[string]any{"resume_token": card.Token, "resume_action": "rejected"})
	if len(framesOfType(resumed, chat.FrameConfirmResolved)) != 1 {
		t.Fatalf("the decision was not reported: %+v", resumed)
	}

	delID := env.latestDelegationID(session.ID)
	env.waitForDelegation(delID, model.DelegationDone)
	// The action never ran.
	if m, _ := env.app.Store.AIModels().GetByID(context.Background(), env.ws.ID, target); m.Status != model.StatusActive {
		t.Fatal("a rejected action ran anyway")
	}
	// The Gateway's completion turn tells the person it could not be done.
	completion := env.completionFrames(session.ID, "I could not take the spare model offline because you declined it.")
	if fr := completion[len(completion)-1]; fr.Type != chat.FrameResult || !fr.Final {
		t.Fatalf("the completion turn did not end with a result: %+v", fr)
	}
}

// --- the chip and cancel ------------------------------------------------------

// historyChips reads the background-delegation chips a reload would render.
func (e *testEnv) historyChips(token, chatUID string) []delegationChip {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/v1/chat/history", token, map[string]string{"chat_id": chatUID})
	e.expectStatus(rec, http.StatusOK)
	var resp historyResponse
	e.decode(rec, &resp)
	return resp.Meta.RunningDelegations
}

// waitForChipStatus blocks until a delegation chip reaches a status on reload,
// so a test is immune to the small gap between a park landing and its waiting
// state being written to the durable record.
func (e *testEnv) waitForChipStatus(token, chatUID, status string) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range e.historyChips(token, chatUID) {
			if c.Status == status {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("no delegation chip reached status %q", status)
}

// runCount reports how many turns a conversation has recorded, so a test can
// prove a cancel raised NO completion turn.
func (e *testEnv) runCount(sessionID int64) int {
	e.t.Helper()
	var n int
	if err := e.sql.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM agent_runs WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		e.t.Fatalf("count runs: %v", err)
	}
	return n
}

// A running background delegation shows as a chip on reload, and cancelling it
// takes it terminal with NO completion turn: the person asked it to stop, so the
// Gateway does not narrate a result.
func TestBackgroundDelegationChipAndCancel(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	gated := newGatedVendor(t, answerChunks("TECHNICAL never delivered payload=NOPE"))
	env.agentOnURL("research", "Research", gated.server.URL)
	Gateway := newFakeVendor(t,
		GatewayDelegatesWith("I'll put Research on it.", "research", "a long job", model.HandoffBackground),
		answerChunks("I've started Research in the background."),
	)
	modelID := env.registerModel(Gateway)
	env.GatewayAgent()

	session := env.seededChat(user.ID)
	env.streamTurn(token, map[string]any{
		"prompt": "do a long job", "model_id": modelID, "chat_id": session.UID,
	})
	select {
	case <-gated.hit:
	case <-time.After(10 * time.Second):
		t.Fatal("the background agent never started")
	}

	// The chip shows on reload, named, running.
	chips := env.historyChips(token, session.UID)
	if len(chips) != 1 || chips[0].Name != "Research" || chips[0].Status != model.DelegationRunning || chips[0].ID == "" {
		t.Fatalf("the running delegation chip is wrong: %+v", chips)
	}

	// Cancel it.
	delID := env.latestDelegationID(session.ID)
	rec := env.do(http.MethodPost, "/v1/chat/delegation/cancel", token,
		map[string]any{"chat_id": session.UID, "delegation_id": delID})
	env.expectStatus(rec, http.StatusNoContent)

	env.waitForDelegation(delID, model.DelegationCancelled)
	gated.unblock()

	// No completion turn ran: the delegate turn is the only run.
	time.Sleep(100 * time.Millisecond)
	if n := env.runCount(session.ID); n != 1 {
		t.Fatalf("a cancelled delegation still raised a completion turn: %d runs", n)
	}

	// The chip STAYS, saying it was cancelled. A chip that vanishes when the
	// work stops leaves the person with no record that it ever happened, and
	// "what has been done here" is most of what they wanted the column for.
	chips = env.historyChips(token, session.UID)
	if len(chips) != 1 {
		t.Fatalf("the cancelled delegation should still be listed: %+v", chips)
	}
	if chips[0].Status != model.DelegationCancelled {
		t.Fatalf("the chip should say it was cancelled: %+v", chips[0])
	}
	if chips[0].CompletedAt == 0 {
		t.Error("a chip that has stopped should say when, so its time can stop ticking")
	}
}

// When the conversation is auto-approving, a background agent honours it: it
// does NOT raise a card, it runs the action, and the Gateway narrates. This is the
// regression for a background run that ignored the session's approval setting.
func TestBackgroundAgentAutoApprovesWhenSessionIsAuto(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	target := env.spareModel()
	specVendor := newFakeVendor(t,
		agentParkingCall(target), // would PARK in a manual session
		answerChunks("TECHNICAL disabled model, payload=AUTOXYZ"),
	)
	env.agentOnURL("ops", "Ops", specVendor.server.URL, "set_model_status")

	Gateway := newFakeVendor(t,
		GatewayDelegatesWith("I'll have Ops handle it.", "ops", "take the spare model offline", model.HandoffBackground),
		answerChunks("I've started Ops in the background."),
		answerChunks("Done, the spare model is offline."),
	)
	modelID := env.registerModel(Gateway)
	env.GatewayAgent()

	session := env.seededChat(user.ID)
	if err := env.app.Store.Agent().SetSessionApprovalMode(context.Background(), session.ID, model.ApprovalAuto); err != nil {
		t.Fatalf("set auto-approve: %v", err)
	}

	env.streamTurn(token, map[string]any{
		"prompt": "take the spare model offline", "model_id": modelID, "chat_id": session.UID,
	})

	// It finishes without ever parking.
	delID := env.latestDelegationID(session.ID)
	env.waitForDelegation(delID, model.DelegationDone)

	ctx := context.Background()
	if m, _ := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, target); m.Status != model.StatusDisabled {
		t.Fatal("the auto-approved background action did not run")
	}
	if _, err := env.app.Store.Agent().PendingPark(ctx, session.ID); err == nil {
		t.Fatal("a background agent raised a card despite the session auto-approving")
	}
	// As it ran, the agent wrote what it was doing to its durable record, so
	// the Gateway's status tool and the chip have more than a spinner to show.
	if d, _ := env.app.Store.Agent().GetDelegation(ctx, delID); !strings.Contains(string(d.Progress), "activity") {
		t.Fatalf("the background run did not write its progress: %q", d.Progress)
	}
	env.completionFrames(session.ID, "Done, the spare model is offline.")
}

// --- Gateway status + cancel tools ---------------------------------------------

// GatewayCallsTool scripts the Gateway invoking one of its own tools, then a
// narration turn after it reads the result.
func GatewayCallsTool(id, name, argsJSON string) []string {
	esc := strings.ReplaceAll(argsJSON, `"`, `\"`)
	return []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"` + id + `","function":{"name":"` + name + `","arguments":"` + esc + `"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

// toolOutput returns the JSON a named tool reported, read from its done frame.
func toolOutput(t *testing.T, frames []chat.Frame, name string) string {
	t.Helper()
	for _, f := range framesOfType(frames, chat.FrameTool) {
		msg := toolMessage(t, f)
		if msg.Name == name {
			return string(msg.Output)
		}
	}
	t.Fatalf("the %q tool never ran: %+v", name, frames)
	return ""
}

// The Gateway can read, live, how a background task is going: it calls
// background_status and gets the running agent back, without touching
// the work. This is the fix for a Gateway that said it had no status feed.
func TestGatewayChecksBackgroundTaskStatus(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	gated := newGatedVendor(t, answerChunks("TECHNICAL done payload=X"))
	env.agentOnURL("research", "Research", gated.server.URL)

	Gateway := newFakeVendor(t,
		GatewayDelegatesWith("I'll put Research on it.", "research", "a big job", model.HandoffBackground),
		answerChunks("I've started Research in the background."),
		GatewayCallsTool("call_check", "background_status", "{}"),
		answerChunks("Research is still working on it."),
	)
	modelID := env.registerModel(Gateway)
	env.GatewayAgent()

	session := env.seededChat(user.ID)
	env.streamTurn(token, map[string]any{"prompt": "do a big job", "model_id": modelID, "chat_id": session.UID})
	select {
	case <-gated.hit:
	case <-time.After(10 * time.Second):
		t.Fatal("the background agent never started")
	}

	// The Gateway asks for status → calls the tool → sees the running agent.
	frames := env.streamTurn(token, map[string]any{"prompt": "what's the status?", "model_id": modelID, "chat_id": session.UID})
	out := toolOutput(t, frames, "background_status")
	if !strings.Contains(out, `"Research"`) || !strings.Contains(out, `"running":1`) {
		t.Fatalf("the status tool did not report the running task: %s", out)
	}
	if got := taggedDeltas(frames, false); !strings.Contains(got, "Research is still working") {
		t.Fatalf("the Gateway did not narrate the status: %q", got)
	}
	gated.unblock()
}

// The Gateway can stop a background task itself: it calls cancel_background_task,
// the delegation ends with no completion turn, and the Gateway narrates.
func TestGatewayCancelsBackgroundTask(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	gated := newGatedVendor(t, answerChunks("TECHNICAL never delivered"))
	env.agentOnURL("research", "Research", gated.server.URL)

	Gateway := newFakeVendor(t,
		GatewayDelegatesWith("I'll put Research on it.", "research", "a long job", model.HandoffBackground),
		answerChunks("I've started Research in the background."),
		GatewayCallsTool("call_cancel", "cancel_background_task", `{"id":0}`), // real id injected via setReply once the delegation exists
		answerChunks("I've stopped Research for you."),
	)
	modelID := env.registerModel(Gateway)
	env.GatewayAgent()

	session := env.seededChat(user.ID)
	env.streamTurn(token, map[string]any{"prompt": "do a long job", "model_id": modelID, "chat_id": session.UID})
	select {
	case <-gated.hit:
	case <-time.After(10 * time.Second):
		t.Fatal("the background agent never started")
	}
	delID := env.latestDelegationID(session.ID)
	// The Gateway cancels by the running id it was handed at start, which the test
	// only learns now the delegation exists, so inject it into the scripted call.
	Gateway.setReply(2, GatewayCallsTool("call_cancel", "cancel_background_task", fmt.Sprintf(`{"id":%d}`, delID)))

	frames := env.streamTurn(token, map[string]any{"prompt": "stop it", "model_id": modelID, "chat_id": session.UID})
	if out := toolOutput(t, frames, "cancel_background_task"); !strings.Contains(out, `"cancelled":true`) {
		t.Fatalf("the cancel tool did not stop the task: %s", out)
	}
	env.waitForDelegation(delID, model.DelegationCancelled)
	gated.unblock()

	// A cancel raises no completion turn: the two runs are the delegate turn and
	// the cancel turn, and nothing else.
	time.Sleep(150 * time.Millisecond)
	if n := env.runCount(session.ID); n != 2 {
		t.Fatalf("a cancelled background task raised a completion turn: %d runs", n)
	}
}

// Deleting a conversation kills the background agents working under it, so no
// goroutine keeps running against records about to disappear.
func TestCancelSessionBackgroundStopsRunningDelegations(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	gated := newGatedVendor(t, answerChunks("TECHNICAL never delivered"))
	env.agentOnURL("research", "Research", gated.server.URL)
	Gateway := newFakeVendor(t,
		GatewayDelegatesWith("I'll put Research on it.", "research", "a long job", model.HandoffBackground),
		answerChunks("I've started Research in the background."),
	)
	modelID := env.registerModel(Gateway)
	env.GatewayAgent()

	session := env.seededChat(user.ID)
	env.streamTurn(token, map[string]any{"prompt": "do a long job", "model_id": modelID, "chat_id": session.UID})
	select {
	case <-gated.hit:
	case <-time.After(10 * time.Second):
		t.Fatal("the background agent never started")
	}
	delID := env.latestDelegationID(session.ID)

	// This is what the chat-delete handler calls.
	env.app.CancelSessionBackground(session.ID)
	env.waitForDelegation(delID, model.DelegationCancelled)
	gated.unblock()
}

// A turn queued while a conversation waits on a person is not stranded there.
//
// The run manager refuses to start a queued turn while a card is on screen, and
// the only things that try again are another Schedule and a turn finishing. So a
// completion turn queued while a card was up could sit there for good: the work
// is done, the chip settles, and the Gateway never says so. Answering the card
// drains the queue, which is what this proves.
func TestAnsweringACardDrainsWhatWasQueuedBehindIt(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)

	vendor := newFakeVendor(t, answerChunks("Queued turn ran."))
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	session := env.seededChat(user.ID)

	// A card really is on screen. This used to be faked by setting the session's
	// status alone, which is the very conflation that once stopped a conversation
	// for good: the status was left saying `waiting_approval` with no park behind
	// it, and every completion turn queued afterwards was held back forever
	// (KB/29). The guard asks for the park, so the test puts one there.
	park := &model.ParkSnapshot{
		TokenHash:   agent.HashToken("tok_queued_drain"),
		WorkspaceID: env.ws.ID,
		SessionID:   session.ID,
		UserID:      user.ID,
		ModelID:     modelID,
		ToolName:    "http_request",
		ToolCallID:  "call_drain",
		ToolArgs:    json.RawMessage(`{"url":"https://example.test"}`),
		ActionHash:  "hash_drain",
		ExpiresAt:   time.Now().UTC().Add(time.Hour),
	}
	if err := env.app.Store.Agent().CreatePark(context.Background(), park); err != nil {
		t.Fatalf("park the turn: %v", err)
	}
	if err := env.app.Store.Agent().SetSessionStatus(
		context.Background(), session.ID, model.SessionWaitingApproval); err != nil {
		t.Fatalf("mark the session waiting: %v", err)
	}

	profile, loadout, err := env.app.Resolve(context.Background(), app.ProfileRequest{
		WorkspaceID: env.ws.ID, UserID: user.ID, Channel: model.ChannelChat, PreferredModelID: modelID,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	env.app.Runs.Schedule(agent.Turn{
		WorkspaceID: env.ws.ID, UserID: user.ID, SessionID: session.ID, ModelID: profile.ModelID,
		SystemPrompt: profile.SystemPrompt, Tools: loadout, MaxIterations: profile.MaxIterations,
	})

	// It is held back, because a person is mid-decision.
	time.Sleep(300 * time.Millisecond)
	if r, ok := env.app.Runs.Latest(session.ID); ok && r != nil {
		t.Fatalf("a turn started while a card was on screen: %+v", r.Frames())
	}

	// The card is answered, so the wait is over and the queue drains itself.
	if err := env.app.Store.Agent().ResolveParkByID(
		context.Background(), park.ID, model.ParkApproved); err != nil {
		t.Fatalf("answer the card: %v", err)
	}
	if err := env.app.Store.Agent().SetSessionStatus(
		context.Background(), session.ID, model.SessionRunning); err != nil {
		t.Fatalf("clear the waiting status: %v", err)
	}
	env.app.Runs.Drain(session.ID)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok := env.app.Runs.Latest(session.ID); ok && r.Done() {
			if strings.Contains(allDeltaText(r.Frames()), "Queued turn ran.") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the queued turn never ran after the card was answered")
}
