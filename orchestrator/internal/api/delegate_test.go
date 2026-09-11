package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
)

func agentMessage(t *testing.T, frame chat.Frame) chat.AgentMessage {
	t.Helper()
	raw, err := json.Marshal(frame.Message)
	if err != nil {
		t.Fatalf("marshal agent frame: %v", err)
	}
	var msg chat.AgentMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("decode agent frame: %v", err)
	}
	return msg
}

// Delegation end to end: the Gateway routes a task to an agent that runs its
// own turn, and the handoff mode decides who has the last word. The agent
// is a real configured agent; the Gateway reaches it through the delegate tool
// the roster put in its loadout.

// agent configures one non-Gateway agent the Gateway can route to.
func (e *testEnv) agent(key, name, instructions string) {
	e.t.Helper()
	if err := e.app.Store.Agents().Create(context.Background(), &model.Agent{
		WorkspaceID:  e.ws.ID,
		Key:          key,
		Name:         name,
		Instructions: instructions,
		Status:       model.StatusActive,
	}); err != nil {
		e.t.Fatalf("create agent: %v", err)
	}
}

// agentWithTools configures an agent that holds some tools, so it can
// act (and, for an approval-gated tool, stop the turn to ask).
func (e *testEnv) agentWithTools(key, name string, tools ...string) {
	e.t.Helper()
	if err := e.app.Store.Agents().Create(context.Background(), &model.Agent{
		WorkspaceID:  e.ws.ID,
		Key:          key,
		Name:         name,
		Instructions: "Do the work.",
		Status:       model.StatusActive,
		Tools:        tools,
	}); err != nil {
		e.t.Fatalf("create agent: %v", err)
	}
}

// spareModel registers a second model the tests can toggle without disturbing
// the one the agents actually run on.
func (e *testEnv) spareModel() int64 {
	e.t.Helper()
	ctx := context.Background()
	v := &model.AIVendor{
		WorkspaceID: e.ws.ID, VendorKey: model.VendorOpenAICompatible,
		Name: "Spare", BaseURL: "http://spare.invalid",
	}
	if err := e.app.Store.Vendors().Create(ctx, v); err != nil {
		e.t.Fatalf("create spare vendor: %v", err)
	}
	m := &model.AIModel{
		WorkspaceID: e.ws.ID, VendorID: v.ID, ModelKey: "spare-1",
		Type: model.ModelTypeChat, ContextWindow: 1000,
	}
	if err := e.app.Store.AIModels().Create(ctx, m); err != nil {
		e.t.Fatalf("create spare model: %v", err)
	}
	return m.ID
}

// agentParkingCall scripts a delegation whose agent calls an
// approval-gated tool (disabling a model) that stops the turn, then, after the
// person answers, says something.
func agentParkingCall(modelID int64) []string {
	args := `{\"model_id\":` + strconv.FormatInt(modelID, 10) + `,\"status\":\"disabled\"}`
	return []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_op","function":{"name":"set_model_status","arguments":"` + args + `"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

func agentCall(agent, task, mode string) []string {
	args := `{\"agent\":\"` + agent + `\",\"task\":\"` + task + `\",\"mode\":\"` + mode + `\"}`
	return []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"delegate","arguments":"` + args + `"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

func answerChunks(text string) []string {
	return []string{
		`{"choices":[{"index":0,"delta":{"content":"` + text + `"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
}

// taggedDeltas joins the deltas an agent produced (Agent == want).
func taggedDeltas(frames []chat.Frame, want bool) string {
	var b strings.Builder
	for _, f := range framesOfType(frames, chat.FrameDelta) {
		if f.Agent != want {
			continue
		}
		if text, ok := f.Message.(string); ok {
			b.WriteString(text)
		}
	}
	return b.String()
}

func TestGatewayDelegatesInContinueMode(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")
	env.agent("finance", "Finance", "Assess whether a customer is a payment risk.")

	vendor := newFakeVendor(t,
		agentCall("finance", "Assess Acme risk", model.HandoffContinue),
		answerChunks("Acme is high risk."),          // the agent's own turn
		answerChunks("Recommendation: be careful."), // the Gateway, having read it
	)
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "review Acme", "model_id": modelID})

	// The delegation is bracketed, and names who and how.
	start := framesOfType(frames, chat.FrameAgentStart)
	if len(start) != 1 {
		t.Fatalf("the delegation was not announced: %+v", frames)
	}
	if len(framesOfType(frames, chat.FrameAgentEnd)) != 1 {
		t.Fatalf("the delegation was not closed: %+v", frames)
	}

	// In continue mode the agent's prose is NOT streamed to the person: it
	// reports its result back to the Gateway (as data), and the GATEWAY narrates the
	// person-facing answer. So the agent's own words never reach the chat,
	// and what the person reads is the Gateway's.
	if got := taggedDeltas(frames, true); got != "" {
		t.Fatalf("the agent's prose leaked into the chat: %q", got)
	}
	if got := taggedDeltas(frames, false); got != "Recommendation: be careful." {
		t.Fatalf("the Gateway did not narrate the delegation result: %q", got)
	}

	// The agent was never handed the delegate tool: one level, no recursion.
	if sub := vendor.sentToolsAsking("Assess Acme risk"); contains(sub, model.DelegateToolName) {
		t.Fatalf("the agent could delegate again: %v", sub)
	}
	// The Gateway was told who it could route to.
	prompt := vendor.sentSystemPromptAsking("review Acme")
	if !strings.Contains(prompt, "finance") || !strings.Contains(prompt, "Agents") {
		t.Fatalf("the roster was not in the Gateway's prompt: %q", prompt)
	}
}

func TestGatewayDelegatesInTerminalMode(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")
	env.agent("builder", "Builder", "Build the thing end to end.")

	vendor := newFakeVendor(t,
		agentCall("builder", "Build it", model.HandoffTerminal),
		answerChunks("Built and done."), // the agent has the last word
	)
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "make it", "model_id": modelID})

	start := framesOfType(frames, chat.FrameAgentStart)
	if len(start) != 1 {
		t.Fatalf("the delegation was not announced: %+v", frames)
	}
	if msg := agentMessage(t, start[0]); msg.Agent != "builder" || msg.Mode != model.HandoffTerminal {
		t.Fatalf("the start frame does not name the handoff: %+v", msg)
	}

	// In terminal mode the agent's answer IS the turn's answer: the Gateway
	// adds nothing, so the result is the agent's words.
	last := frames[len(frames)-1]
	if last.Type != chat.FrameResult || !last.Final {
		t.Fatalf("the turn did not end with a result: %+v", last)
	}
	if text, _ := last.Message.(string); text != "Built and done." {
		t.Fatalf("the terminal answer was not the agent's: %q", text)
	}
}

// A delegation's inner steps are durable: the agent's own work is written
// to the transcript, attributed to it and to the Gateway's `delegate` call, and
// its tool calls are resolved (never left "running"), so an interrupted
// delegation can be rebuilt. The Gateway's own view, model and reload alike,
// still shows only the delegation, never the agent's steps replayed inline.
func TestADelegationPersistsTheAgentsSteps(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	// An agent that can read the clock.
	if err := env.app.Store.Agents().Create(context.Background(), &model.Agent{
		WorkspaceID: env.ws.ID, Key: "clock", Name: "Clock",
		Instructions: "Tell the time.", Status: model.StatusActive,
		Tools: []string{"current_time"},
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	vendor := newFakeVendor(t,
		agentCall("clock", "what time is it", model.HandoffContinue), // Gateway delegates
		[]string{ // the agent calls its tool
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_time","function":{"name":"current_time","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		},
		answerChunks("It is midnight in the vault."),   // the agent answers
		answerChunks("The vault clock says midnight."), // the Gateway, having read it
	)
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "ask the clock", "model_id": modelID})
	session := env.sessionOf(frames)
	chatID := chatIDOf(t, frames)

	steps, err := env.app.Store.Agent().Transcript(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	var agentSteps int
	var clockResolved bool
	for _, s := range steps {
		if s.AgentKey == "" {
			continue
		}
		agentSteps++
		if s.AgentKey != "clock" || s.ParentToolCallID != "call_1" {
			t.Fatalf("an agent step is misattributed: key=%q parent=%q", s.AgentKey, s.ParentToolCallID)
		}
		for _, c := range s.ToolCalls {
			if c.ToolName == "current_time" {
				if c.Status != model.ToolCallCompleted {
					t.Fatalf("the agent's tool call was left %q, so a reload would spin it forever", c.Status)
				}
				clockResolved = true
			}
		}
	}
	if agentSteps == 0 {
		t.Fatal("the agent's steps were not persisted")
	}
	if !clockResolved {
		t.Fatal("the agent's tool call was not persisted and resolved")
	}

	// The reloaded conversation shows the delegation as the Gateway's chip and the
	// Gateway's answer, never the agent's own steps inline.
	rec := env.do(http.MethodPost, "/v1/chat/history", token, map[string]string{"chat_id": chatID})
	env.expectStatus(rec, http.StatusOK)
	var resp historyResponse
	env.decode(rec, &resp)
	history := resp.Messages

	var sawAgentChip, sawGatewayAnswer bool
	for _, m := range history {
		for _, tl := range m.Tools {
			if tl.Name == "current_time" {
				t.Fatalf("the agent's inner tool leaked into the Gateway's history: %+v", history)
			}
			if tl.Name == model.DelegateToolName {
				sawAgentChip = true
			}
		}
		if m.Content == "It is midnight in the vault." {
			t.Fatalf("the agent's inner answer leaked into the Gateway's history: %+v", history)
		}
		if m.Content == "The vault clock says midnight." {
			sawGatewayAnswer = true
		}
	}
	if !sawAgentChip || !sawGatewayAnswer {
		t.Fatalf("the Gateway's own view lost the delegation or its answer: %+v", history)
	}
}

// An agent that holds an approval-gated tool stops the WHOLE turn and asks,
// exactly as the Gateway does. Approving re-enters the delegation, runs the
// action, and lets it finish; in continue mode the Gateway then has the last
// word. Nothing runs before approval, and the delegation chip does not spin
// while the card waits.
func TestAAgentParksForApprovalAndResumes(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	env.agentWithTools("ops", "Ops", "set_model_status")

	vendor := newFakeVendor(t,
		agentCall("ops", "disable that model", model.HandoffContinue), // Gateway delegates
		agentParkingCall(modelID),                                     // agent parks
		answerChunks("Done, the model is disabled."),                  // agent, after approval
		answerChunks("I have disabled it for you."),                   // the Gateway narrates
	)
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{"prompt": "disable it", "model_id": modelID})
	card := confirmRequest(t, frames[len(frames)-1])
	session := env.sessionOf(frames)

	ctx := context.Background()
	// Nothing ran: the model is still active while the card waits.
	if m, _ := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, modelID); m.Status != model.StatusActive {
		t.Fatal("an agent's action ran before it was approved")
	}
	// The Gateway's delegation chip is stubbed as waiting, not left spinning.
	subCall := agentCallRow(t, env, session.ID)
	if subCall.Status != model.ToolCallApprovalRequired {
		t.Fatalf("the delegation chip was not stubbed as waiting: %+v", subCall)
	}

	// Approve: the delegation resumes, the action runs, the agent finishes,
	// and the Gateway narrates.
	resumed := env.streamTurn(token, map[string]any{"resume_token": card.Token, "resume_action": "approved"})
	if last := resumed[len(resumed)-1]; last.Type != chat.FrameResult || !last.Final {
		t.Fatalf("the resumed delegation did not complete: %+v", last)
	}
	if m, _ := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, modelID); m.Status != model.StatusDisabled {
		t.Fatal("the approved agent action did not run")
	}
	if got := taggedDeltas(resumed, false); got != "I have disabled it for you." {
		t.Fatalf("the Gateway did not narrate the delegation's result: %q", got)
	}
	// The delegation chip is now completed, and there is no second card.
	if subCall := agentCallRow(t, env, session.ID); subCall.Status != model.ToolCallCompleted {
		t.Fatalf("the delegation chip was not resolved on completion: %+v", subCall)
	}
	if len(framesOfType(resumed, chat.FrameConfirmRequest)) != 0 {
		t.Fatalf("the resumed delegation asked again: %+v", resumed)
	}
}

// Rejecting an agent's action ends the delegation and hands control BACK to
// the Gateway to respond, but no new card can be raised. Here the Gateway even
// re-delegates the refused task; the agent's second attempt at the tool is
// silently refused (no card), and the Gateway then speaks. So the person hears
// back, and the re-ask loop is impossible at either level.
func TestRejectingAAgentsActionHandsBackToTheGateway(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	env.agentWithTools("ops", "Ops", "set_model_status")

	vendor := newFakeVendor(t,
		agentCall("ops", "disable that model", model.HandoffContinue),       // Gateway delegates
		agentParkingCall(modelID),                                           // agent parks -> reject
		agentCall("ops", "disable that model again", model.HandoffContinue), // Gateway re-delegates the refused task
		agentParkingCall(modelID),                                           // agent tries the tool -> BLOCKED, no card
		answerChunks("I could not: it needs approval."),                     // agent reports back
		answerChunks("Okay, I've left the model as it was."),                // Gateway responds to the person
	)
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{"prompt": "disable it", "model_id": modelID})
	card := confirmRequest(t, frames[len(frames)-1])

	resumed := env.streamTurn(token, map[string]any{"resume_token": card.Token, "resume_action": "rejected"})

	ctx := context.Background()
	// The action never ran.
	if m, _ := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, modelID); m.Status != model.StatusActive {
		t.Fatal("a rejected agent action ran anyway")
	}
	// No new card, even though the Gateway re-delegated: the agent's second
	// attempt was refused without a card.
	if len(framesOfType(resumed, chat.FrameConfirmRequest)) != 0 {
		t.Fatalf("a rejected agent action asked for approval again: %+v", resumed)
	}
	// The Gateway responds to the person (not silence).
	if got := taggedDeltas(resumed, false); got != "Okay, I've left the model as it was." {
		t.Fatalf("the Gateway did not respond after the refusal: %q", got)
	}
}

// A parked delegation in TERMINAL mode resumes into the agent and stops
// there: the agent's own answer is the turn's answer, and the Gateway never
// runs a narration turn.
func TestAAgentParkResumesTerminally(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	env.agentWithTools("ops", "Ops", "set_model_status")

	vendor := newFakeVendor(t,
		agentCall("ops", "disable that model", model.HandoffTerminal), // terminal handoff
		agentParkingCall(modelID),                                     // agent parks
		answerChunks("All set: the model is disabled."),               // agent has the last word
	)
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{"prompt": "disable it", "model_id": modelID})
	card := confirmRequest(t, frames[len(frames)-1])

	resumed := env.streamTurn(token, map[string]any{"resume_token": card.Token, "resume_action": "approved"})

	// The agent's answer IS the turn's answer, delivered as the top-level
	// result, with no Gateway narration after it.
	last := resumed[len(resumed)-1]
	if last.Type != chat.FrameResult || !last.Final {
		t.Fatalf("the terminal delegation did not end with a result: %+v", last)
	}
	if text, _ := last.Message.(string); text != "All set: the model is disabled." {
		t.Fatalf("the terminal answer was not the agent's: %q", text)
	}
	if m, _ := env.app.Store.AIModels().GetByID(context.Background(), env.ws.ID, modelID); m.Status != model.StatusDisabled {
		t.Fatal("the approved agent action did not run")
	}
}

// An agent can park AGAIN inside a resumed delegation: it calls a second
// approval-gated tool after the first was approved. The second card carries the
// same delegation, and the second resume completes it. This is the re-park path.
func TestAAgentReParksInsideAResumedDelegation(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	env.agentWithTools("ops", "Ops", "set_model_status")

	// The agent toggles a SEPARATE model, so the one the agents run on stays
	// active across both resumes.
	targetID := env.spareModel()
	setStatus := func(callID, status string) []string {
		args := `{\"model_id\":` + strconv.FormatInt(targetID, 10) + `,\"status\":\"` + status + `\"}`
		return []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"` + callID + `","function":{"name":"set_model_status","arguments":"` + args + `"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		}
	}
	vendor := newFakeVendor(t,
		agentCall("ops", "toggle that model", model.HandoffContinue), // Gateway delegates
		setStatus("call_a", "disabled"),                              // agent parks (1)
		setStatus("call_b", "active"),                                // agent parks AGAIN (2)
		answerChunks("Toggled it off and back on."),                  // agent finishes
		answerChunks("All done."),                                    // Gateway narrates
	)
	env.pointModelAt(modelID, vendor)

	ctx := context.Background()
	frames := env.streamTurn(token, map[string]any{"prompt": "toggle it", "model_id": modelID})
	card1 := confirmRequest(t, frames[len(frames)-1])

	// First approval runs the disable, then the agent parks again.
	resumed1 := env.streamTurn(token, map[string]any{"resume_token": card1.Token, "resume_action": "approved"})
	if m, _ := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, targetID); m.Status != model.StatusDisabled {
		t.Fatal("the first approved action did not run")
	}
	card2 := confirmRequest(t, resumed1[len(resumed1)-1])
	if card2.Token == card1.Token {
		t.Fatal("the re-park reused the first token")
	}

	// Second approval runs the enable and the delegation finishes.
	resumed2 := env.streamTurn(token, map[string]any{"resume_token": card2.Token, "resume_action": "approved"})
	if last := resumed2[len(resumed2)-1]; last.Type != chat.FrameResult || !last.Final {
		t.Fatalf("the re-parked delegation did not complete: %+v", last)
	}
	if m, _ := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, targetID); m.Status != model.StatusActive {
		t.Fatal("the second approved action did not run")
	}
	if got := taggedDeltas(resumed2, false); got != "All done." {
		t.Fatalf("the Gateway did not narrate after the re-park: %q", got)
	}
}

// An agent's pending card survives a reload too: the history endpoint
// resolves the AGENT's tool schema to rebuild it, and the fresh token
// resumes straight back into the delegation.
func TestReloadRestoresAAgentCard(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	env.agentWithTools("ops", "Ops", "set_model_status")

	vendor := newFakeVendor(t,
		agentCall("ops", "disable that model", model.HandoffContinue),
		agentParkingCall(modelID),
		answerChunks("Done, disabled."), // agent, after approval
		answerChunks("I disabled it."),  // Gateway narrates
	)
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{"prompt": "disable it", "model_id": modelID})
	confirmRequest(t, frames[len(frames)-1]) // parked
	chatID := chatIDOf(t, frames)

	rec := env.do(http.MethodPost, "/v1/chat/history", token, map[string]string{"chat_id": chatID})
	env.expectStatus(rec, http.StatusOK)
	var resp historyResponse
	env.decode(rec, &resp)
	history := resp.Messages

	last := history[len(history)-1]
	if last.Role != "confirm" || last.Confirm == nil {
		t.Fatalf("the agent's pending card was not restored: %+v", history)
	}
	if last.Confirm.Title == "" || len(last.Confirm.Details) == 0 {
		t.Fatalf("the restored agent card is missing its title or details: %+v", last.Confirm)
	}

	// The fresh token resumes back into the delegation, runs the action, and the
	// Gateway finishes.
	resumed := env.streamTurn(token, map[string]any{"resume_token": last.Confirm.Token, "resume_action": "approved"})
	if fr := resumed[len(resumed)-1]; fr.Type != chat.FrameResult || !fr.Final {
		t.Fatalf("the reloaded agent card could not be approved: %+v", fr)
	}
	if m, _ := env.app.Store.AIModels().GetByID(context.Background(), env.ws.ID, modelID); m.Status != model.StatusDisabled {
		t.Fatal("approving the reloaded agent card did not run the action")
	}
}

// Auto-approve reaches inside a delegation: with the conversation set to auto,
// an agent's approval-gated action runs with no card, exactly like the
// Gateway's does.
func TestAutoApproveAppliesToAgents(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	env.agentWithTools("ops", "Ops", "set_model_status")
	target := env.spareModel()

	args := `{\"model_id\":` + strconv.FormatInt(target, 10) + `,\"status\":\"disabled\"}`
	vendor := newFakeVendor(t,
		answerChunks("Hi."), // turn 1: create the session
		agentCall("ops", "disable the spare", model.HandoffContinue), // turn 2: delegate
		[]string{ // the agent calls its approval-gated tool (auto: no card)
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_op","function":{"name":"set_model_status","arguments":"` + args + `"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		},
		answerChunks("Agent disabled it."), // agent answer
		answerChunks("Done."),              // Gateway narration
	)
	env.pointModelAt(modelID, vendor)

	ctx := context.Background()
	frames := env.streamTurn(token, map[string]any{"prompt": "hello", "model_id": modelID})
	chatID := chatIDOf(t, frames)
	session := env.sessionOf(frames)

	if err := env.app.Store.Agent().SetSessionApprovalMode(ctx, session.ID, model.ApprovalAuto); err != nil {
		t.Fatalf("set auto: %v", err)
	}

	next := env.streamTurn(token, map[string]any{"prompt": "disable the spare", "model_id": modelID, "chat_id": chatID})
	if cards := framesOfType(next, chat.FrameConfirmRequest); len(cards) != 0 {
		t.Fatalf("auto-approve still asked for a card inside a delegation: %+v", next)
	}
	if last := next[len(next)-1]; last.Type != chat.FrameResult {
		t.Fatalf("the auto-approved delegation did not complete: %+v", last)
	}
	if m, _ := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, target); m.Status != model.StatusDisabled {
		t.Fatal("the agent's auto-approved action did not run")
	}
}

// agentCallRow returns the Gateway's `agent` tool-call row for a session.
func agentCallRow(t *testing.T, env *testEnv, sessionID int64) *model.ToolCall {
	t.Helper()
	for _, c := range env.toolCalls(sessionID) {
		if c.ToolName == model.DelegateToolName {
			return c
		}
	}
	t.Fatalf("no delegate call row for session %d", sessionID)
	return nil
}

// A model that fails (down, overloaded) must end the turn with an error the
// person can read, not crash the process. The turn runs in a goroutine, so a
// panic there would take the whole server down; this is the regression for
// exactly that (a nil step reached SaveStep and segfaulted).
func TestAModelErrorEndsTheTurnWithoutCrashing(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t)
	vendor.status = http.StatusServiceUnavailable // the vendor is overloaded
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "hello", "model_id": modelID})

	// The turn ends with an error frame, cleanly.
	last := frames[len(frames)-1]
	if last.Type != chat.FrameError || !last.Final {
		t.Fatalf("a model failure did not end the turn with an error frame: %+v", frames)
	}

	// And the session is recorded as failed, not left pretending to run.
	session := env.sessionOf(frames)
	got, err := env.app.Store.Agent().GetSession(context.Background(), env.ws.ID, session.ID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if got.Status != model.SessionFailed {
		t.Fatalf("expected the session marked failed, got %q", got.Status)
	}
}

func contains(items []string, want string) bool {
	for _, s := range items {
		if s == want {
			return true
		}
	}
	return false
}

// agentCallBesideAnother is the step this whole fix is about: the model asks to
// delegate AND to do something else, in one assistant step. Nothing prevents a
// model from doing this. `parallel_tool_calls` is never set on the request, so
// it defaults on, and the loop filters only hallucinated names.
func agentCallBesideAnother(agent, task, mode string) []string {
	args := `{\"agent\":\"` + agent + `\",\"task\":\"` + task + `\",\"mode\":\"` + mode + `\"}`
	return []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[` +
			`{"index":0,"id":"call_1","function":{"name":"delegate","arguments":"` + args + `"}},` +
			`{"index":1,"id":"call_2","function":{"name":"current_time","arguments":"{}"}}` +
			`]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

func TestACallAfterATerminalDelegationIsNotLeftRunning(t *testing.T) {
	// A terminal handoff ends the turn: the specialist's answer IS the answer,
	// so a sibling call is correctly never run. What was missing is SAYING so.
	//
	// SaveStep writes every call of a step as running before any of them runs,
	// and the delegate branch left the loop mid-list without settling the rest,
	// so `call_2` stayed running in the database with nothing to come back to
	// it. Live it looks fine, because no frame is sent either way. Reload the
	// conversation and the transcript is rebuilt from rows, and a running call
	// is drawn as a spinner, forever, for work nothing is doing.
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	if err := env.app.Store.Agents().Create(context.Background(), &model.Agent{
		WorkspaceID: env.ws.ID, Key: "clock", Name: "Clock",
		Instructions: "Tell the time.", Status: model.StatusActive,
		Tools: []string{"current_time"},
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	vendor := newFakeVendor(t,
		agentCallBesideAnother("clock", "what time is it", model.HandoffTerminal),
		answerChunks("It is midnight in the vault."), // the agent answers, and that ends the turn
	)
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "ask the clock", "model_id": modelID})
	session := env.sessionOf(frames)

	steps, err := env.app.Store.Agent().Transcript(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}

	var found bool
	for _, s := range steps {
		// The Gateway's own step, not the agent's.
		if s.AgentKey != "" {
			continue
		}
		for _, c := range s.ToolCalls {
			if c.ToolCallID != "call_2" {
				continue
			}
			found = true
			if c.Status == model.ToolCallRunning {
				t.Fatalf("the call after the delegation was left running, so a reload spins it forever")
			}
			if c.Status != model.ToolCallCompleted {
				t.Fatalf("the call after the delegation was left %q", c.Status)
			}
			if !strings.Contains(string(c.Result), "not carried out") {
				t.Errorf("it was settled without saying what happened: %s", c.Result)
			}
		}
	}
	if !found {
		t.Fatal("the sibling call was never written to the transcript at all")
	}
}
