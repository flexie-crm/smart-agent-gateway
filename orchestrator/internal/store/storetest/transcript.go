package storetest

import (
	"encoding/json"
	"sync"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// The transcript: an ordered list of steps, each carrying what it produced.

// A step's position must be allocated atomically. A background agent (KB/27)
// writes to the same session as the Gateway's turn, concurrently: if two writers
// can be handed the same seq, their two different steps collapse onto one
// (session, seq) row and one loses its attribution. Every concurrent allocation
// must therefore be distinct.
func testNextSeqIsAtomic(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	user := mustUser(t, st, ws.ID, "u@acme.test")
	chat := mustChat(t, st, ws.ID, user.ID, "")

	const n = 12
	seqs := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seqs[i], errs[i] = st.Agent().NextSeq(ctx(), chat.ID)
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("NextSeq: %v", errs[i])
		}
		if seen[seqs[i]] {
			t.Fatalf("seq %d handed out to two concurrent callers: they would collide on one row", seqs[i])
		}
		seen[seqs[i]] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct seqs from %d concurrent calls", len(seen), n)
	}
}

// The end-to-end shape of the delegation bug: the Gateway's narration turn and a
// background agent's tool step, saved concurrently, must land as two rows
// with their own attribution, never as one row where the Gateway's text carries
// the agent's tool calls.
func testConcurrentStepsKeepTheirAttribution(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	user := mustUser(t, st, ws.ID, "u@acme.test")
	chat := mustChat(t, st, ws.ID, user.ID, "")

	save := func(subKey string, calls []*model.ToolCall) error {
		seq, err := st.Agent().NextSeq(ctx(), chat.ID)
		if err != nil {
			return err
		}
		return st.Agent().SaveStep(ctx(), &model.AgentStep{
			SessionID: chat.ID, Seq: seq, Kind: model.StepAssistant,
			AgentKey: subKey, Text: "step " + subKey, ToolCalls: calls,
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var GatewayErr, subErr error
	go func() {
		defer wg.Done()
		// The Gateway's "started" narration: text, no tools (it holds none here).
		GatewayErr = save("", nil)
	}()
	go func() {
		defer wg.Done()
		// The agent's real tool step, tagged to its delegation.
		subErr = save("worker", []*model.ToolCall{{
			WorkspaceID: ws.ID, ToolCallID: "s1", ToolName: "http_request",
			Args: json.RawMessage(`{}`), Status: model.ToolCallRunning,
		}})
	}()
	wg.Wait()
	if GatewayErr != nil || subErr != nil {
		t.Fatalf("save: Gateway=%v sub=%v", GatewayErr, subErr)
	}

	steps, err := st.Agent().Transcript(ctx(), chat.ID)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2: the two writers merged onto one row", len(steps))
	}
	for _, s := range steps {
		if len(s.ToolCalls) > 0 && s.AgentKey != "worker" {
			t.Fatalf("a tool-calling step is attributed to %q, not the agent: the Gateway row carries the agent's calls", s.AgentKey)
		}
	}
}

func testSteps(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	user := mustUser(t, st, ws.ID, "u@acme.test")
	chat := mustChat(t, st, ws.ID, user.ID, "")

	if err := st.Agent().SaveStep(ctx(), &model.AgentStep{
		SessionID: chat.ID, Seq: 0, Kind: model.StepUser, Text: "book it",
	}); err != nil {
		t.Fatalf("save user step: %v", err)
	}

	// One assistant step: it thought, it said something, and it called two
	// tools at once. All three at once is the case the old shape could not hold.
	step := &model.AgentStep{
		SessionID: chat.ID, Seq: 1, Kind: model.StepAssistant,
		Vendor: "anthropic", Model: "claude-x",
		Text:      "Checking, then booking.",
		Reasoning: "Availability first.",
		ToolCalls: []*model.ToolCall{
			{
				WorkspaceID: ws.ID, ToolCallID: "c1", ToolName: "check",
				FriendlyName: "Check availability",
				Args:         json.RawMessage(`{"date":"friday"}`),
				Status:       model.ToolCallRunning,
			},
			{
				WorkspaceID: ws.ID, ToolCallID: "c2", ToolName: "book",
				Args:   json.RawMessage(`{"seat":"12A"}`),
				Status: model.ToolCallRunning,
			},
		},
	}
	if err := st.Agent().SaveStep(ctx(), step); err != nil {
		t.Fatalf("save assistant step: %v", err)
	}
	if step.ID == 0 {
		t.Fatal("the step did not come back with its id")
	}

	// Each call is resolved in place, against the row the step created.
	for i, c := range step.ToolCalls {
		c.Status = model.ToolCallCompleted
		c.Result = json.RawMessage(`{"ok":true}`)
		c.DurationMS = int64(10 + i)
		if err := st.Agent().ResolveToolCall(ctx(), c); err != nil {
			t.Fatalf("resolve call: %v", err)
		}
	}

	steps, err := st.Agent().Transcript(ctx(), chat.ID)
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("expected two steps, got %d", len(steps))
	}
	if steps[0].Kind != model.StepUser || steps[0].Text != "book it" {
		t.Fatalf("the user step did not round-trip: %+v", steps[0])
	}

	assistant := steps[1]
	if assistant.Text != "Checking, then booking." {
		t.Fatalf("the step's text was lost: %q", assistant.Text)
	}
	if assistant.Reasoning != "Availability first." {
		t.Fatalf("the step's reasoning was lost: %q", assistant.Reasoning)
	}
	if assistant.Vendor != "anthropic" || assistant.Model != "claude-x" {
		t.Fatalf("the step lost its attribution: %+v", assistant)
	}
	if len(assistant.ToolCalls) != 2 {
		t.Fatalf("expected two calls, got %d", len(assistant.ToolCalls))
	}
	// Parallel calls come back in the order they ran.
	if assistant.ToolCalls[0].ToolCallID != "c1" || assistant.ToolCalls[1].ToolCallID != "c2" {
		t.Fatalf("the calls are out of order: %+v", assistant.ToolCalls)
	}
	first := assistant.ToolCalls[0]
	if first.FriendlyName != "Check availability" {
		t.Fatalf("the label was lost: %q", first.FriendlyName)
	}
	if string(first.Args) != `{"date":"friday"}` {
		t.Fatalf("the arguments were lost: %s", first.Args)
	}
	if string(first.Result) != `{"ok":true}` {
		t.Fatalf("the result was lost: %s", first.Result)
	}
	if first.Status != model.ToolCallCompleted || first.DurationMS != 10 {
		t.Fatalf("the outcome was not recorded: %+v", first)
	}
}

// A step written twice lands in place. The checkpoint depends on it: it writes
// the same step every couple of seconds while the answer grows, and a second
// row each time would fill the timeline with drafts.
func testStepCheckpointing(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	user := mustUser(t, st, ws.ID, "u@acme.test")
	chat := mustChat(t, st, ws.ID, user.ID, "")

	for _, draft := range []struct {
		text    string
		partial bool
	}{
		{"The answer", true},
		{"The answer is", true},
		{"The answer is 42.", false},
	} {
		if err := st.Agent().SaveStep(ctx(), &model.AgentStep{
			SessionID: chat.ID, Seq: 0, Kind: model.StepAssistant,
			Text: draft.text, Partial: draft.partial,
		}); err != nil {
			t.Fatalf("checkpoint: %v", err)
		}
	}

	steps, err := st.Agent().Transcript(ctx(), chat.ID)
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("the checkpoints piled up: %d steps", len(steps))
	}
	if steps[0].Text != "The answer is 42." {
		t.Fatalf("the finished answer did not replace the draft: %q", steps[0].Text)
	}
	if steps[0].Partial {
		t.Fatal("the committed step is still marked partial")
	}

	// And the conversation is one message long, not three.
	if err := st.Agent().TouchChat(ctx(), chat.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	session, err := st.Agent().GetSession(ctx(), ws.ID, chat.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.MessageCount != 1 {
		t.Fatalf("the checkpoints were counted as messages: %d", session.MessageCount)
	}
}

// requested_approval is sticky. The approved run that follows a confirmation
// card never asks again, and it must not erase the fact that a person was once
// asked: that is the audit trail.
func testToolCallApprovalIsSticky(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	user := mustUser(t, st, ws.ID, "u@acme.test")
	chat := mustChat(t, st, ws.ID, user.ID, "")

	step := &model.AgentStep{
		SessionID: chat.ID, Seq: 0, Kind: model.StepAssistant,
		ToolCalls: []*model.ToolCall{{
			WorkspaceID: ws.ID, ToolCallID: "c1", ToolName: "refund",
			Args:              json.RawMessage(`{"amount":100}`),
			Status:            model.ToolCallApprovalRequired,
			RequestedApproval: true,
		}},
	}
	if err := st.Agent().SaveStep(ctx(), step); err != nil {
		t.Fatalf("park the call: %v", err)
	}

	// The approved run resolves the same call, and does not itself ask.
	if err := st.Agent().ResolveToolCall(ctx(), &model.ToolCall{
		SessionID: chat.ID, WorkspaceID: ws.ID,
		ToolCallID: "c1", ToolName: "refund",
		Args:              json.RawMessage(`{"amount":100}`),
		Result:            json.RawMessage(`{"refunded":true}`),
		Status:            model.ToolCallCompleted,
		RequestedApproval: false,
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	steps, err := st.Agent().Transcript(ctx(), chat.ID)
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	call := steps[0].ToolCalls[0]
	if call.Status != model.ToolCallCompleted {
		t.Fatalf("the approved call was not completed: %+v", call)
	}
	if !call.RequestedApproval {
		t.Fatal("the record forgot that a person was asked")
	}
	// One row, not two: the resolve refined the parked call rather than adding
	// a second one beside it.
	if len(steps[0].ToolCalls) != 1 {
		t.Fatalf("the approved run duplicated the call: %+v", steps[0].ToolCalls)
	}
}

// A tool call that says it is running in a process that has just started is a
// ghost, and it has to be closed out or the transcript shows a spinner beside a
// tool nothing is doing (KB/29).
//
// A call writes itself as running before it starts and is resolved when it
// returns, so anything that stops the turn in between leaves the row saying
// "running" for good: a process that dies mid-call, and a turn that parks for
// approval and therefore never runs the calls queued behind the parked one.
func testInterruptedToolCallsAreClosedOut(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "sweep")
	user := mustUser(t, st, ws.ID, "sweep@acme.test")
	chat := mustChat(t, st, ws.ID, user.ID, "")

	step := &model.AgentStep{
		SessionID: chat.ID, Seq: 1, Kind: model.StepAssistant,
		ToolCalls: []*model.ToolCall{
			// The one that parked: the turn ended here.
			{WorkspaceID: ws.ID, ToolCallID: "p1", ToolName: "http_request", Status: model.ToolCallApprovalRequired},
			// Queued behind it, and therefore never run.
			{WorkspaceID: ws.ID, ToolCallID: "r1", ToolName: "http_request", Status: model.ToolCallRunning},
			{WorkspaceID: ws.ID, ToolCallID: "r2", ToolName: "http_request", Status: model.ToolCallRunning},
			// And one that genuinely finished, which must be left alone.
			{WorkspaceID: ws.ID, ToolCallID: "d1", ToolName: "http_request", Status: model.ToolCallCompleted},
		},
	}
	if err := st.Agent().SaveStep(ctx(), step); err != nil {
		t.Fatalf("save step: %v", err)
	}

	n, err := st.Agent().MarkToolCallsInterrupted(ctx())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n < 2 {
		t.Fatalf("expected at least the two running calls to be closed, got %d", n)
	}

	steps, err := st.Agent().Transcript(ctx(), chat.ID)
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	byID := map[string]*model.ToolCall{}
	for _, c := range steps[0].ToolCalls {
		byID[c.ToolCallID] = c
	}
	for _, id := range []string{"r1", "r2"} {
		got := byID[id]
		if got == nil || got.Status != model.ToolCallFailed {
			t.Fatalf("an interrupted call still claims to be running: %+v", got)
		}
		if got.ErrorText == "" {
			t.Fatalf("an interrupted call was closed with no reason: %+v", got)
		}
		if got.CompletedAt == nil {
			t.Fatalf("an interrupted call was closed with no stamp: %+v", got)
		}
	}
	// The parked call is NOT swept: it is genuinely waiting on a person, and
	// answering the card resolves it. Sweeping it would refuse an approval
	// nobody had declined.
	if byID["p1"].Status != model.ToolCallApprovalRequired {
		t.Fatalf("the sweep closed a card that was still waiting on a person: %+v", byID["p1"])
	}
	if byID["d1"].Status != model.ToolCallCompleted {
		t.Fatalf("the sweep disturbed a call that had finished: %+v", byID["d1"])
	}
}

// A resolved call knows which row it is.
//
// The chat opens a finished call by its row id, so the id has to be on the call
// the moment it finishes rather than only when the conversation is read back.
// Today it is, and this says so out loud, because how it got there was the
// server's answer to LastInsertId after an upsert that updated rather than
// inserted, which is behaviour and not a contract.
func testResolveToolCallKnowsItsRow(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	user := mustUser(t, st, ws.ID, "u@acme.test")
	chat := mustChat(t, st, ws.ID, user.ID, "")

	seq, err := st.Agent().NextSeq(ctx(), chat.ID)
	if err != nil {
		t.Fatalf("next seq: %v", err)
	}
	// The step writes the call as running, exactly as a turn does.
	step := &model.AgentStep{
		SessionID: chat.ID, Seq: seq, Kind: model.StepAssistant,
		Text: "working", ToolCalls: []*model.ToolCall{{
			WorkspaceID: ws.ID, ToolCallID: "call-1", ToolName: "terminal",
			Args: json.RawMessage(`{"command":"git status"}`), Status: model.ToolCallRunning,
		}},
	}
	if err := st.Agent().SaveStep(ctx(), step); err != nil {
		t.Fatalf("save step: %v", err)
	}
	written := step.ToolCalls[0].ID
	if written == 0 {
		t.Fatal("the step's write left the call with no row id")
	}

	// A SEPARATE object for the same call, which is what a resumed turn or a
	// second writer holds: it knows the conversation and the tool call id, and
	// nothing about the row.
	resolving := &model.ToolCall{
		SessionID: chat.ID, WorkspaceID: ws.ID, ToolCallID: "call-1", ToolName: "terminal",
		Args: json.RawMessage(`{"command":"git status"}`), Status: model.ToolCallCompleted,
		Result: json.RawMessage(`{"exit_code":0}`), DurationMS: 26,
	}
	if err := st.Agent().ResolveToolCall(ctx(), resolving); err != nil {
		t.Fatalf("resolve tool call: %v", err)
	}
	if resolving.ID != written {
		t.Fatalf("a resolved call carries row %d, want the row it resolved (%d): the chat would have nothing to open",
			resolving.ID, written)
	}

	// And it is the row somebody opening the call would read.
	read, err := st.Agent().ToolCall(ctx(), ws.ID, resolving.ID)
	if err != nil {
		t.Fatalf("read the call back: %v", err)
	}
	if read.ToolName != "terminal" || read.Status != model.ToolCallCompleted {
		t.Fatalf("read back %s/%s, want terminal/completed", read.ToolName, read.Status)
	}

	// Resolving a call that was never started is still refused.
	orphan := &model.ToolCall{SessionID: chat.ID, WorkspaceID: ws.ID, ToolCallID: "never-started", ToolName: "terminal"}
	if err := st.Agent().ResolveToolCall(ctx(), orphan); err == nil {
		t.Fatal("resolving a call with no row was allowed")
	}
}
