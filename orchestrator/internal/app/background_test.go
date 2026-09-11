package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/run"
	"flexie.io/sag/internal/store"
)

// park writes an approval a background agent is waiting on: the live one, or one
// queued behind it. Returns nothing; the tests read them back through the store.
func (e *env) park(t *testing.T, session *model.AgentSession, user *model.User, token string, queued bool) {
	e.parkOf(t, session, user, token, queued, model.HandoffBackground, "research", "call_p")
}

// parkOf writes a park with an explicit owner, so a test can put the GATEWAY's
// own card on the same conversation as a background agent's.
func (e *env) parkOf(t *testing.T, session *model.AgentSession, user *model.User, token string, queued bool, mode, agentKey, parentCall string) {
	t.Helper()
	p := &model.ParkSnapshot{
		TokenHash:        agent.HashToken(token),
		WorkspaceID:      e.ws.ID,
		SessionID:        session.ID,
		UserID:           user.ID,
		ModelID:          e.aiModel("park-model"),
		ToolName:         "http_request",
		ToolCallID:       "call_" + token,
		ToolArgs:         json.RawMessage(`{"url":"https://example.test"}`),
		ActionHash:       token,
		AgentKey:         agentKey,
		ParentToolCallID: parentCall,
		HandoffMode:      mode,
		ExpiresAt:        time.Now().UTC().Add(time.Hour),
	}
	if queued {
		if _, err := e.app.Store.Agent().CreateParkSequenced(context.Background(), p); err != nil {
			t.Fatalf("create queued park: %v", err)
		}
		return
	}
	if err := e.app.Store.Agent().CreatePark(context.Background(), p); err != nil {
		t.Fatalf("create park: %v", err)
	}
}

// recordingRunner answers nothing and signals every completion turn it is asked
// to run on a channel, so a test waits for exactly the completions it expects
// rather than polling a wall clock, which starves and flakes under a loaded
// suite. The channel is buffered past the expected count so a run goroutine
// never blocks on the send.
type recordingRunner struct {
	completed chan int64
}

func (r *recordingRunner) Run(_ context.Context, turn agent.Turn, _ *chat.Stream) error {
	if turn.CompletedDelegationID != 0 {
		r.completed <- turn.CompletedDelegationID
	}
	return nil
}

// A restart kills a background agent's goroutine but not its row (KB/27).
// Boot recovery must not leave the chip spinning or the result vanishing
// silently: it settles every still-running delegation as failed with a
// person-safe reason AND fires the Gateway's completion turn, so the person is
// told plainly that the task did not finish.
func TestRecoveryNarratesInterruptedDelegations(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("ghost@acme.test")

	// A default agent pinning a model, so the completion turn resolves something
	// to run on: a delegation record stores no model of its own.
	pinned := e.aiModel("house-model")
	ag := &model.Agent{
		WorkspaceID: e.ws.ID, Key: model.DefaultAgentKey, Name: "House", ModelID: &pinned,
	}
	if err := e.app.Store.Agents().Create(ctx, ag); err != nil {
		t.Fatalf("create default agent: %v", err)
	}

	session := &model.AgentSession{
		WorkspaceID: e.ws.ID, UserID: user.ID, Channel: model.ChannelChat, Title: "Heavy work",
	}
	if err := e.app.Store.Agent().CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Two delegations a dead process left running, and one that had already
	// finished before the lights went out.
	running1 := &model.AgentDelegation{SessionID: session.ID, WorkspaceID: e.ws.ID, ParentToolCallID: "call_a", AgentKey: "research", Mode: model.HandoffBackground}
	running2 := &model.AgentDelegation{SessionID: session.ID, WorkspaceID: e.ws.ID, ParentToolCallID: "call_b", AgentKey: "research", Mode: model.HandoffBackground}
	doneAlready := &model.AgentDelegation{SessionID: session.ID, WorkspaceID: e.ws.ID, ParentToolCallID: "call_c", AgentKey: "research", Mode: model.HandoffBackground}
	for _, d := range []*model.AgentDelegation{running1, running2, doneAlready} {
		if err := e.app.Store.Agent().CreateDelegation(ctx, d); err != nil {
			t.Fatalf("create delegation: %v", err)
		}
	}
	if err := e.app.Store.Agent().CompleteDelegation(ctx, doneAlready.ID, model.DelegationDone, nil, ""); err != nil {
		t.Fatalf("complete already-done delegation: %v", err)
	}

	// Swap in a runner that records rather than calls a model, so recovery's
	// completion turns run hermetically here.
	rec := &recordingRunner{completed: make(chan int64, 8)}
	e.app.Runs = run.NewManager(e.app.Store, rec, nil, zerolog.Nop())

	n, err := e.app.RecoverInterruptedDelegations(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 interrupted delegations recovered, got %d", n)
	}

	// Both running rows are now failed, with a person-safe reason and a stamp;
	// the already-done one is left exactly as it was.
	for _, d := range []*model.AgentDelegation{running1, running2} {
		got, err := e.app.Store.Agent().GetDelegation(ctx, d.ID)
		if err != nil {
			t.Fatalf("get delegation: %v", err)
		}
		if got.Status != model.DelegationFailed || got.CompletedAt == nil {
			t.Fatalf("an interrupted delegation was not settled failed: %+v", got)
		}
		if got.ErrorText != "the service was interrupted before it could finish" {
			t.Fatalf("an interrupted delegation lacks the person-safe reason: %q", got.ErrorText)
		}
	}
	if got, _ := e.app.Store.Agent().GetDelegation(ctx, doneAlready.ID); got.Status != model.DelegationDone {
		t.Fatalf("an already-finished delegation was disturbed by recovery: %+v", got)
	}
	if left, _ := e.app.Store.Agent().RunningDelegations(ctx, session.ID); len(left) != 0 {
		t.Fatalf("a recovered delegation still shows running: %+v", left)
	}

	// Each recovered delegation gets a Gateway completion turn (the anti-silent
	// guarantee), scheduled onto the session's one lane and drained in order. Wait
	// for exactly the two completions to fire, generously bounded so a loaded suite
	// never flakes: the work is trivial once scheduled, so this is not a race.
	want := map[int64]bool{running1.ID: true, running2.ID: true}
	got := map[int64]bool{}
	timeout := time.After(60 * time.Second)
	for len(got) < 2 {
		select {
		case id := <-rec.completed:
			if !want[id] {
				t.Fatalf("a completion turn ran for an unexpected delegation: %d", id)
			}
			got[id] = true
		case <-timeout:
			t.Fatalf("only %d of 2 completion turns fired: %v", len(got), got)
		}
	}
}

// A restart that lands while agents are PARKED must not leave the conversation
// unable to speak again (KB/29).
//
// The stall: three background agents were waiting for approval when the process
// restarted. Recovery settled their rows as failed, and stopped there. Their
// parks stayed open, so the person kept being shown cards for agents that no
// longer existed; and the session's status stayed `waiting_approval`, which is
// the gate `run.Manager` checks before delivering a completion turn. With no
// agent left to raise a card and no card left to answer, nothing could ever
// clear it, and the conversation went silent for good.
func TestRecoveryReleasesWhatADeadAgentWasWaitingOn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("parked@acme.test")

	pinned := e.aiModel("house-model")
	ag := &model.Agent{WorkspaceID: e.ws.ID, Key: model.DefaultAgentKey, Name: "House", ModelID: &pinned}
	if err := e.app.Store.Agents().Create(ctx, ag); err != nil {
		t.Fatalf("create default agent: %v", err)
	}

	session := &model.AgentSession{
		WorkspaceID: e.ws.ID, UserID: user.ID, Channel: model.ChannelChat, Title: "Parked work",
	}
	if err := e.app.Store.Agent().CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	parked := &model.AgentDelegation{
		SessionID: session.ID, WorkspaceID: e.ws.ID, ParentToolCallID: "call_p",
		AgentKey: "research", Mode: model.HandoffBackground,
	}
	if err := e.app.Store.Agent().CreateDelegation(ctx, parked); err != nil {
		t.Fatalf("create delegation: %v", err)
	}
	// Its progress is frozen mid-park, exactly as a killed goroutine leaves it.
	if err := e.app.Store.Agent().UpdateDelegationProgress(ctx, parked.ID,
		json.RawMessage(`{"activity":"Waiting for your approval","waiting":true}`)); err != nil {
		t.Fatalf("set progress: %v", err)
	}

	e.park(t, session, user, "tok_live", false)
	e.park(t, session, user, "tok_queued", true)
	if err := e.app.Store.Agent().SetSessionStatus(ctx, session.ID, model.SessionWaitingApproval); err != nil {
		t.Fatalf("mark session waiting: %v", err)
	}

	rec := &recordingRunner{completed: make(chan int64, 4)}
	e.app.Runs = run.NewManager(e.app.Store, rec, nil, zerolog.Nop())

	if _, err := e.app.RecoverInterruptedDelegations(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}

	// The live card and the one queued behind it are both closed, and REJECTED:
	// nobody agreed to either, and an unanswered card is not consent.
	if _, err := e.app.Store.Agent().PendingPark(ctx, session.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a card is still offered for an agent that no longer exists: %v", err)
	}
	if left, _ := e.app.Store.Agent().QueuedParks(ctx, session.ID); len(left) != 0 {
		t.Fatalf("a queued card outlived the agent that raised it: %+v", left)
	}
	// Closed for good, not merely hidden: the token a person is still holding no
	// longer buys anything, whichever of the two cards they were looking at.
	for _, tok := range []string{"tok_live", "tok_queued"} {
		if _, err := e.app.Store.Agent().ClaimPark(ctx, agent.HashToken(tok), "approved"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("an orphaned card for %q could still be claimed: %v", tok, err)
		}
	}

	// And the gate is open again. This is the line that mattered: while the
	// session read `waiting_approval`, run.Manager refused to deliver anything.
	got, err := e.app.Store.Agent().GetSession(ctx, e.ws.ID, session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Status == model.SessionWaitingApproval {
		t.Fatal("the conversation is still waiting for a card that can never come")
	}
}

// The chip must never say an agent is waiting for approval when the agent is
// over. Its progress blob is a snapshot of the last thing it was doing, and a
// killed goroutine leaves that snapshot saying `waiting: true` forever.
func TestAFinishedDelegationIsNeverWaiting(t *testing.T) {
	frozen := json.RawMessage(`{"activity":"Waiting for your approval","waiting":true}`)

	running := &model.AgentDelegation{Status: model.DelegationRunning, Progress: frozen}
	if !running.Waiting() {
		t.Fatal("a running delegation parked for approval should read as waiting")
	}

	for _, status := range []string{model.DelegationFailed, model.DelegationDone, model.DelegationCancelled} {
		d := &model.AgentDelegation{Status: status, Progress: frozen}
		if d.Waiting() {
			t.Fatalf("a %s delegation still claims to be waiting for approval", status)
		}
	}
}

// Recovery must close out the DEAD AGENT's cards and nothing else.
//
// The regression this pins (found by adversarial review of the first fix): the
// sweep rejected every open park on the conversation. But a session's parks are
// not all one agent's. The Gateway parks on the same conversation whenever IT
// calls an approval-gated tool, and that card is fully durable across a restart:
// nothing in memory backs it, and the resume re-resolves the profile and runs
// the turn again. Closing it destroyed a card the person could still have
// answered, and left the tool call behind it reading "approval required" for
// good, because rejecting a park writes no result. Only a resume does that.
func TestRecoveryLeavesTheGatewaysOwnCardAlone(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("gatewaypark@acme.test")

	pinned := e.aiModel("house-model")
	ag := &model.Agent{WorkspaceID: e.ws.ID, Key: model.DefaultAgentKey, Name: "House", ModelID: &pinned}
	if err := e.app.Store.Agents().Create(ctx, ag); err != nil {
		t.Fatalf("create default agent: %v", err)
	}
	session := &model.AgentSession{
		WorkspaceID: e.ws.ID, UserID: user.ID, Channel: model.ChannelChat, Title: "Two owners",
	}
	if err := e.app.Store.Agent().CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// A background agent, interrupted by the restart, with a card of its own.
	parked := &model.AgentDelegation{
		SessionID: session.ID, WorkspaceID: e.ws.ID, ParentToolCallID: "call_p",
		AgentKey: "research", Mode: model.HandoffBackground,
	}
	if err := e.app.Store.Agent().CreateDelegation(ctx, parked); err != nil {
		t.Fatalf("create delegation: %v", err)
	}
	e.park(t, session, user, "tok_agent", true) // queued behind the Gateway's

	// And the GATEWAY's own card, live on screen. No delegation, no handoff mode.
	e.parkOf(t, session, user, "tok_gateway", false, "", "", "")
	if err := e.app.Store.Agent().SetSessionStatus(ctx, session.ID, model.SessionWaitingApproval); err != nil {
		t.Fatalf("mark session waiting: %v", err)
	}

	rec := &recordingRunner{completed: make(chan int64, 4)}
	e.app.Runs = run.NewManager(e.app.Store, rec, nil, zerolog.Nop())
	if _, err := e.app.RecoverInterruptedDelegations(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}

	// The Gateway's card survives, and is still answerable with the token the
	// person's browser is holding.
	live, err := e.app.Store.Agent().PendingPark(ctx, session.ID)
	if err != nil {
		t.Fatalf("the Gateway's own card was destroyed by recovery: %v", err)
	}
	if live.HandoffMode == model.HandoffBackground {
		t.Fatalf("the surviving card is the dead agent's, not the Gateway's: %+v", live)
	}
	if _, err := e.app.Store.Agent().ClaimPark(ctx, agent.HashToken("tok_gateway"), "approved"); err != nil {
		t.Fatalf("the Gateway's card could no longer be answered: %v", err)
	}

	// The dead agent's queued card IS closed: nothing will ever run it.
	if _, err := e.app.Store.Agent().ClaimPark(ctx, agent.HashToken("tok_agent"), "approved"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the dead agent's queued card is still claimable: %v", err)
	}

	// And the conversation is STILL waiting, because a card really is on screen.
	got, err := e.app.Store.Agent().GetSession(ctx, e.ws.ID, session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Status != model.SessionWaitingApproval {
		t.Fatalf("recovery cleared the wait while a real card was up: %q", got.Status)
	}
}

// A hard kill is not the end of the work.
//
// Graceful shutdown lets an agent finish, and covers a deploy, a restart, a
// rebuild. It cannot cover a SIGKILL, an out-of-memory, or the machine going
// away. The old answer to those was to settle the delegation failed and have
// the Gateway say it did not finish, which is honest and useless: the person has
// to ask again for work they already asked for. Everything the agent needs is on
// its row, so it is started again instead (KB/27).
func TestAnInterruptedAgentIsStartedAgain(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("resume@acme.test")

	pinned := e.aiModel("house-model")
	if err := e.app.Store.Agents().Create(ctx, &model.Agent{
		WorkspaceID: e.ws.ID, Key: model.DefaultAgentKey, Name: "House", ModelID: &pinned,
	}); err != nil {
		t.Fatalf("create default agent: %v", err)
	}
	// The agent that was working, so it can be resolved again on the way back.
	if err := e.app.Store.Agents().Create(ctx, &model.Agent{
		WorkspaceID: e.ws.ID, Key: "researcher", Name: "Researcher", ModelID: &pinned,
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	session := &model.AgentSession{
		WorkspaceID: e.ws.ID, UserID: user.ID, Channel: model.ChannelChat, Title: "Killed",
	}
	if err := e.app.Store.Agent().CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	killed := &model.AgentDelegation{
		SessionID: session.ID, WorkspaceID: e.ws.ID, ParentToolCallID: "call_k",
		AgentKey: "researcher", Mode: model.HandoffBackground,
		Task: "find the current price of gold",
	}
	if err := e.app.Store.Agent().CreateDelegation(ctx, killed); err != nil {
		t.Fatalf("create delegation: %v", err)
	}

	rec := &recordingRunner{completed: make(chan int64, 4)}
	e.app.Runs = run.NewManager(e.app.Store, rec, nil, zerolog.Nop())
	if _, err := e.app.RecoverInterruptedDelegations(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}

	// It is RUNNING again, not failed, and the attempt is counted.
	got, err := e.app.Store.Agent().GetDelegation(ctx, killed.ID)
	if err != nil {
		t.Fatalf("get delegation: %v", err)
	}
	if got.Status != model.DelegationRunning {
		t.Fatalf("an interrupted agent was mourned rather than restarted: %+v", got)
	}
	if got.Attempts != 2 {
		t.Fatalf("the restart was not counted, so nothing bounds it: attempts=%d", got.Attempts)
	}
	if got.Task != "find the current price of gold" {
		t.Fatalf("the instruction did not survive: %q", got.Task)
	}
}

// And it is bounded. A task that kills the process must not be restarted by
// every boot for ever, taking the process with it each time.
func TestAnAgentIsNotRestartedForEver(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("loop@acme.test")

	pinned := e.aiModel("house-model")
	if err := e.app.Store.Agents().Create(ctx, &model.Agent{
		WorkspaceID: e.ws.ID, Key: model.DefaultAgentKey, Name: "House", ModelID: &pinned,
	}); err != nil {
		t.Fatalf("create default agent: %v", err)
	}
	session := &model.AgentSession{
		WorkspaceID: e.ws.ID, UserID: user.ID, Channel: model.ChannelChat, Title: "Poison",
	}
	if err := e.app.Store.Agent().CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	// Already at the cap: it has been started once and restarted once.
	poison := &model.AgentDelegation{
		SessionID: session.ID, WorkspaceID: e.ws.ID, ParentToolCallID: "call_x",
		AgentKey: "researcher", Mode: model.HandoffBackground,
		Task: "the thing that kills us", Attempts: model.MaxDelegationAttempts,
	}
	if err := e.app.Store.Agent().CreateDelegation(ctx, poison); err != nil {
		t.Fatalf("create delegation: %v", err)
	}

	rec := &recordingRunner{completed: make(chan int64, 4)}
	e.app.Runs = run.NewManager(e.app.Store, rec, nil, zerolog.Nop())
	if _, err := e.app.RecoverInterruptedDelegations(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}

	got, err := e.app.Store.Agent().GetDelegation(ctx, poison.ID)
	if err != nil {
		t.Fatalf("get delegation: %v", err)
	}
	if got.Status != model.DelegationFailed {
		t.Fatalf("a delegation past the cap was restarted again: %+v", got)
	}
	if got.ErrorText != "the service was interrupted before it could finish" {
		t.Fatalf("it was not narrated the honest way: %q", got.ErrorText)
	}
}
