package api

import (
	"context"
	"net/http"
	"testing"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
)

// What the durable transcript keeps, proved through the real loop against a
// real database. These are the cases the old shape could not represent: a turn
// is several steps, and each of them has its own thinking and its own calls.

// A turn where the model thinks, calls a tool, thinks again, and answers must
// leave all of it behind. The old shape had one reasoning column per message,
// so it kept the last one and silently lost the rest.
func TestEveryStepOfATurnIsKept(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t,
		// Step one: the model thinks, says nothing, and calls a tool.
		[]string{
			`{"choices":[{"index":0,"delta":{"reasoning_content":"I need the clock."}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"current_time","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		},
		// Step two: it thinks again, and this time it answers.
		[]string{
			`{"choices":[{"index":0,"delta":{"reasoning_content":"Now I can answer."}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"It is midnight."}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
	)
	modelID := env.registerReasoningModel(vendor)

	// Reasoning is not a request field: it is configuration, decided by the
	// agent, so the caller cannot switch on a cost the administrator did not
	// agree to. The fake vendor sends thinking regardless, which is what a
	// vendor with reasoning enabled does.
	frames := env.streamTurn(token, map[string]any{
		"prompt": "what time is it", "model_id": modelID,
	})
	session := env.sessionOf(frames)

	steps, err := env.app.Store.Agent().Transcript(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("expected the question and both steps of the answer, got %d: %+v", len(steps), steps)
	}

	if steps[0].Kind != model.StepUser || steps[0].Text != "what time is it" {
		t.Fatalf("the question was not stored: %+v", steps[0])
	}

	// The tool step: reasoning, no text, one call, with its arguments AND its
	// result on the same row.
	toolStep := steps[1]
	if toolStep.Text != "" {
		t.Fatalf("a step that only called a tool invented text: %q", toolStep.Text)
	}
	if toolStep.Reasoning != "I need the clock." {
		t.Fatalf("the first step's reasoning was lost: %q", toolStep.Reasoning)
	}
	if len(toolStep.ToolCalls) != 1 {
		t.Fatalf("the call was not attached to the step that made it: %+v", toolStep.ToolCalls)
	}
	call := toolStep.ToolCalls[0]
	if call.ToolName != "current_time" || call.Status != model.ToolCallCompleted {
		t.Fatalf("the call was not recorded: %+v", call)
	}
	if len(call.Args) == 0 || len(call.Result) == 0 {
		t.Fatalf("the call is missing its arguments or its result: %+v", call)
	}
	if call.DurationMS < 0 {
		t.Fatalf("the call has no duration: %+v", call)
	}

	// The answering step: its own reasoning, and the text.
	answer := steps[2]
	if answer.Reasoning != "Now I can answer." {
		t.Fatalf("the second step's reasoning was lost: %q", answer.Reasoning)
	}
	if answer.Text != "It is midnight." {
		t.Fatalf("the answer was not stored: %q", answer.Text)
	}
	if answer.Partial {
		t.Fatal("the finished answer is still marked partial")
	}
	// Two reasoning blocks in one turn, which the old shape could not hold.
	if toolStep.Reasoning == answer.Reasoning {
		t.Fatal("the two steps share one reasoning: they were collapsed into one")
	}

	// The tool result exists exactly once, on the call. There is no second copy
	// masquerading as a message: the question and the answer are the only two
	// message rows this turn produced.
	if count := env.messageRows(session.ID); count != 2 {
		t.Fatalf("expected two message rows (the question and the answer), found %d: "+
			"a tool result was stored as a message", count)
	}
}

// The timeline a reloaded page renders is built from the same steps, so what
// the user saw live is what they see after a refresh.
func TestHistoryRendersTheSteps(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"current_time","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		},
		[]string{
			`{"choices":[{"index":0,"delta":{"content":"It is midnight."}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
	)
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "time?", "model_id": modelID})
	chatID := chatIDOf(t, frames)

	rec := env.do(http.MethodPost, "/v1/chat/history", token, map[string]string{"chat_id": chatID})
	env.expectStatus(rec, http.StatusOK)
	var resp historyResponse
	env.decode(rec, &resp)
	history := resp.Messages

	if len(history) != 3 {
		t.Fatalf("expected the question, the tool step and the answer, got %d: %+v", len(history), history)
	}
	if history[0].Role != model.StepUser || history[0].Content != "time?" {
		t.Fatalf("the question is missing: %+v", history[0])
	}
	// The tool-only step renders as its chip, which is exactly what the live
	// stream showed at the time.
	if len(history[1].Tools) != 1 || history[1].Tools[0].Name != "current_time" {
		t.Fatalf("the tool step lost its chip: %+v", history[1])
	}
	if history[1].Tools[0].Status != model.ToolCallCompleted {
		t.Fatalf("the chip does not say what happened: %+v", history[1].Tools[0])
	}
	if history[2].Content != "It is midnight." {
		t.Fatalf("the answer is missing: %+v", history[2])
	}
}

// An approved call stays attached to the step that made it. Losing that link
// would tear the call out of the conversation it happened in, and the model
// would then be shown a tool result that answers nothing.
func TestAnApprovedCallStaysInItsStep(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	vendor := parkingVendor(t, modelID, "The model is now disabled.")
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{
		"prompt": "disable that model", "model_id": modelID,
	})
	session := env.sessionOf(frames)
	card := confirmRequest(t, frames[len(frames)-1])

	// The card is up, and the pending call is already on the record: a page
	// refreshed right now still shows what is being asked, rather than a tool
	// that is mysteriously still spinning.
	calls := env.toolCalls(session.ID)
	if len(calls) != 1 || calls[0].Status != model.ToolCallApprovalRequired {
		t.Fatalf("the parked call was not recorded as pending: %+v", calls)
	}
	if !calls[0].RequestedApproval {
		t.Fatalf("the parked call does not say a person was asked: %+v", calls[0])
	}
	parkedStepID := calls[0].StepID
	if parkedStepID == 0 {
		t.Fatal("the parked call is not attached to a step")
	}

	env.streamTurn(token, map[string]any{
		"resume_token": card.Token, "resume_action": "approved",
	})

	// One call, still in its step, now completed, and still remembering that a
	// person was asked.
	calls = env.toolCalls(session.ID)
	if len(calls) != 1 {
		t.Fatalf("the approval duplicated the call: %+v", calls)
	}
	if calls[0].StepID != parkedStepID {
		t.Fatalf("the approved call was torn out of its step: %d became %d",
			parkedStepID, calls[0].StepID)
	}
	if calls[0].Status != model.ToolCallCompleted {
		t.Fatalf("the approved call did not complete: %+v", calls[0])
	}
	if !calls[0].RequestedApproval {
		t.Fatal("the approved call forgot that a person was asked")
	}
	if len(calls[0].Result) == 0 {
		t.Fatal("the approved call has no result")
	}
}

// The memory signal ("remember") does its work the instant it is called, but it
// is still a tool call on the record. It has to be RESOLVED, not left in the
// "running" state every call starts in: a page refreshed after the turn rebuilds
// the transcript from these rows, and a call still marked running renders as a
// chip spinning forever.
func TestTheMemorySignalIsRecordedAsFinished(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// The model flags the turn as worth remembering, then answers.
	vendor := newFakeVendor(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_m","function":{"name":"remember","arguments":"{\"note\":\"the user prefers brevity\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		},
		[]string{
			`{"choices":[{"index":0,"delta":{"content":"Noted."}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
	)
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "be brief", "model_id": modelID})
	session := env.sessionOf(frames)

	// Live, the chip is told it is done.
	var done bool
	for _, f := range framesOfType(frames, chat.FrameTool) {
		if msg := toolMessage(t, f); msg.Name == model.MemoryToolName {
			done = true
			if !msg.Done || msg.Status != model.ToolCallCompleted {
				t.Fatalf("the memory chip was not finished live: %+v", msg)
			}
		}
	}
	if !done {
		t.Fatal("the memory tool never reported done")
	}

	// And on the record, which is what a refresh rebuilds from: finished, never
	// still running.
	var remembered *model.ToolCall
	for _, c := range env.toolCalls(session.ID) {
		if c.ToolName == model.MemoryToolName {
			remembered = c
		}
	}
	if remembered == nil {
		t.Fatal("the memory call was not written to the transcript")
	}
	if remembered.Status != model.ToolCallCompleted {
		t.Fatalf("the memory call is still %q, so a reload would spin it forever", remembered.Status)
	}
}
