package api

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
)

// These are the closest-to-the-browser tests we have: they drive the real
// /v1/chat/stream endpoint, read the real SSE frames back exactly as the chat UI
// would, and assert the WHOLE sequence, every delta, every chip, the card, the
// result. They mirror the scenario that kept breaking in manual testing: the
// person asks for something whose ability lives on an agent behind an
// approval-gated tool.

// GatewayDelegatesWith scripts one Gateway reply that says something AND delegates.
func GatewayDelegatesWith(say, agent, task, mode string) []string {
	args := `{\"agent\":\"` + agent + `\",\"task\":\"` + task + `\",\"mode\":\"` + mode + `\"}`
	return []string{
		`{"choices":[{"index":0,"delta":{"content":"` + say + `"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"delegate","arguments":"` + args + `"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

// agentSaysThenCalls scripts an agent reply that emits PROSE (which must
// be dropped from the chat) and then calls an approval-gated tool (which parks).
func agentSaysThenCalls(prose string, modelID int64) []string {
	args := `{\"model_id\":` + strconv.FormatInt(modelID, 10) + `,\"status\":\"disabled\"}`
	return []string{
		`{"choices":[{"index":0,"delta":{"content":"` + prose + `"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_op","function":{"name":"set_model_status","arguments":"` + args + `"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

// allDeltaText joins every content delta, tagged or not, so a test can prove a
// specific string NEVER reached the chat, regardless of who "owned" it.
func allDeltaText(frames []chat.Frame) string {
	var b strings.Builder
	for _, f := range framesOfType(frames, chat.FrameDelta) {
		if s, ok := f.Message.(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

// A continue-mode delegation that parks, is approved, and finishes, watched
// frame by frame the way the browser sees it. The agent's prose never
// reaches the chat (its technical result goes to the Gateway); the Gateway
// narrates; the action runs only after approval.
func TestDelegationApprovalStreamEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	env.agentWithTools("ops", "Ops", "set_model_status")
	target := env.spareModel()

	vendor := newFakeVendor(t,
		GatewayDelegatesWith("Let me have Ops handle that.", "ops", "disable the spare model", model.HandoffContinue),
		agentSaysThenCalls("Disabling it now, model id 2, PATCH /models/2.", target), // prose MUST be dropped
		answerChunks("model 2 status=disabled ok"),                                   // agent's TECHNICAL result -> Gateway only
		answerChunks("Done, I've taken the spare model offline for you."),            // Gateway's narration -> the person
	)
	env.pointModelAt(modelID, vendor)

	// ── Turn 1: the Gateway speaks, delegates, the agent parks. ──
	frames := env.streamTurn(token, map[string]any{"prompt": "take the spare model offline", "model_id": modelID})

	// The Gateway's own words reach the person.
	if got := taggedDeltas(frames, false); got != "Let me have Ops handle that." {
		t.Fatalf("the Gateway's opening was not streamed: %q", got)
	}
	// The delegation is announced as the agent's.
	start := framesOfType(frames, chat.FrameAgentStart)
	if len(start) != 1 {
		t.Fatalf("the delegation was not announced: %+v", frames)
	}
	if msg := agentMessage(t, start[0]); msg.Agent != "ops" || msg.Mode != model.HandoffContinue {
		t.Fatalf("the delegation start is wrong: %+v", msg)
	}
	// The agent's PROSE never reaches the chat, in any form.
	if strings.Contains(allDeltaText(frames), "Disabling it now") || strings.Contains(allDeltaText(frames), "PATCH") {
		t.Fatalf("the agent's raw prose leaked into the chat: %q", allDeltaText(frames))
	}
	if got := taggedDeltas(frames, true); got != "" {
		t.Fatalf("an agent delta was streamed: %q", got)
	}
	// The person sees the agent's tool being prepared (a chip), tagged, and
	// it shows the FRIENDLY name while running, not the raw alias.
	prep := framesOfType(frames, chat.FrameToolPreparing)
	if len(prep) == 0 {
		t.Fatalf("the agent's tool chip was not shown: %+v", frames)
	}
	if msg := toolMessage(t, prep[0]); msg.FriendlyName == "" || msg.FriendlyName == msg.Name {
		t.Fatalf("the preparing chip shows the raw alias, not the friendly name: %+v", msg)
	}
	// The turn PARKS: the last frame is the card, and nothing ran.
	last := frames[len(frames)-1]
	if last.Type != chat.FrameConfirmRequest || !last.Final {
		t.Fatalf("the turn did not park with a card: %+v", last)
	}
	card := confirmRequest(t, last)
	if m, _ := env.app.Store.AIModels().GetByID(context.Background(), env.ws.ID, target); m.Status != model.StatusActive {
		t.Fatal("the agent's action ran before approval")
	}

	// ── Turn 2: approve. The delegation resumes, runs, the Gateway narrates. ──
	resumed := env.streamTurn(token, map[string]any{"resume_token": card.Token, "resume_action": "approved"})

	// The decision is echoed, the delegation is re-opened and closed.
	if len(framesOfType(resumed, chat.FrameConfirmResolved)) != 1 {
		t.Fatalf("the decision was not reported: %+v", resumed)
	}
	if len(framesOfType(resumed, chat.FrameAgentEnd)) != 1 {
		t.Fatalf("the delegation was not closed: %+v", resumed)
	}
	// The action ran.
	if m, _ := env.app.Store.AIModels().GetByID(context.Background(), env.ws.ID, target); m.Status != model.StatusDisabled {
		t.Fatal("the approved action did not run")
	}
	// The agent's TECHNICAL result never reaches the chat...
	if strings.Contains(allDeltaText(resumed), "status=disabled") {
		t.Fatalf("the agent's technical result leaked into the chat: %q", allDeltaText(resumed))
	}
	// ...only the Gateway's narration does.
	if got := taggedDeltas(resumed, false); got != "Done, I've taken the spare model offline for you." {
		t.Fatalf("the Gateway did not narrate the result: %q", got)
	}
	// The turn ends with a result.
	if fr := resumed[len(resumed)-1]; fr.Type != chat.FrameResult || !fr.Final {
		t.Fatalf("the turn did not end with a result: %+v", fr)
	}
	// The Gateway's delegation chip is completed (not spinning, not a red reject).
	if subCall := agentCallRow(t, env, env.sessionOf(frames).ID); subCall.Status != model.ToolCallCompleted {
		t.Fatalf("the delegation chip was not resolved: %+v", subCall)
	}
}

// The same scenario, but the person REJECTS. Watched frame by frame: the action
// never runs, the agent's prose never leaks, no second card is raised, and
// the Gateway is handed control back and RESPONDS to the person (not silence).
func TestDelegationRejectStreamEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	env.agentWithTools("ops", "Ops", "set_model_status")
	target := env.spareModel()

	vendor := newFakeVendor(t,
		GatewayDelegatesWith("Let me have Ops handle that.", "ops", "disable the spare model", model.HandoffContinue),
		agentSaysThenCalls("Disabling it now.", target),
		answerChunks("Understood, I've left the spare model as it was. Want me to do something else?"), // Gateway responds after refusal
	)
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{"prompt": "take the spare model offline", "model_id": modelID})
	card := confirmRequest(t, frames[len(frames)-1])

	resumed := env.streamTurn(token, map[string]any{"resume_token": card.Token, "resume_action": "rejected"})

	ctx := context.Background()
	// The action never ran.
	if m, _ := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, target); m.Status != model.StatusActive {
		t.Fatal("a rejected action ran anyway")
	}
	// No second card.
	if len(framesOfType(resumed, chat.FrameConfirmRequest)) != 0 {
		t.Fatalf("a rejected action asked again: %+v", resumed)
	}
	// The Gateway responds to the person, in the Gateway's voice.
	if got := taggedDeltas(resumed, false); got != "Understood, I've left the spare model as it was. Want me to do something else?" {
		t.Fatalf("the Gateway did not respond after the refusal: %q", got)
	}
	// The turn ends with a result.
	if fr := resumed[len(resumed)-1]; fr.Type != chat.FrameResult || !fr.Final {
		t.Fatalf("the turn did not end with a result: %+v", fr)
	}
}
