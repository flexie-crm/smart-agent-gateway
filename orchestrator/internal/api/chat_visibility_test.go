package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
)

// What a tool was sent and what it answered, asked for when somebody opens the
// row rather than carried on every turn.
//
// The permission is checked on the SERVER, because a console cannot withhold
// anything: without it the answer is a refusal, not a hidden button.

func TestOpeningAToolCallShowsWhatItCarried(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	chatID := env.turnWithAToolCall(t, token)
	call := env.firstToolRow(t, token, chatID)

	// The row carries an id, which is the whole reason it can be opened.
	if call.ID == "" {
		t.Fatalf("a tool row with no id cannot be opened: %+v", call)
	}
	rec := env.do(http.MethodPost, "/v1/chat/tool-call", token, map[string]any{"id": call.ID})
	env.expectStatus(rec, http.StatusOK)

	var opened toolCallBody
	env.decode(rec, &opened)
	if opened.Name != "current_time" || opened.Status != model.ToolCallCompleted {
		t.Fatalf("the wrong call came back: %+v", opened)
	}
	// current_time declares nothing, so it is shown whole: what it was sent
	// and what it answered are both there.
	if len(opened.Sent) == 0 && len(opened.Answered) == 0 {
		t.Fatalf("what it was sent and what it answered are missing: %+v", opened)
	}
}

// Without the permission there is nothing to open. The row is still part of the
// conversation: what is withheld is what the tool carried.
func TestOpeningAToolCallNeedsThePermission(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	admin, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	chatID := env.turnWithAToolCall(t, admin)
	call := env.firstToolRow(t, admin, chatID)

	env.createUser("watcher@acme.test", "dev-Passw0rd!", model.PermChatsDelete)
	watcher, _ := env.login("watcher@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/chat/tool-call", watcher, map[string]any{"id": call.ID})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("somebody without the permission was shown a tool call: %d %s", rec.Code, rec.Body.String())
	}
}

// And a call from somebody else's conversation does not exist, rather than
// being refused: an id tells nobody anything by being asked about.
func TestAnotherPersonsToolCallDoesNotExist(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("one@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	one, _ := env.login("one@acme.test", "dev-Passw0rd!")
	chatID := env.turnWithAToolCall(t, one)
	call := env.firstToolRow(t, one, chatID)

	env.createUser("two@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	two, _ := env.login("two@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/chat/tool-call", two, map[string]any{"id": call.ID})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("one person read another's tool call: %d %s", rec.Code, rec.Body.String())
	}
}

// firstToolRow reads the tool row out of a conversation's history.
func (e *testEnv) firstToolRow(t *testing.T, token, chatID string) historyTool {
	t.Helper()
	rec := e.do(http.MethodPost, "/v1/chat/history", token, map[string]any{"chat_id": chatID})
	e.expectStatus(rec, http.StatusOK)
	var answer historyResponse
	e.decode(rec, &answer)
	for _, message := range answer.Messages {
		for _, call := range message.Tools {
			return call
		}
	}
	t.Fatalf("the conversation has no tool call: %+v", answer.Messages)
	return historyTool{}
}

// turnWithAToolCall drives one turn in which the model calls a tool, and
// returns the conversation it happened in.
func (e *testEnv) turnWithAToolCall(t *testing.T, token string) string {
	t.Helper()
	vendor := newFakeVendor(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"current_time","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		},
		[]string{
			`{"choices":[{"index":0,"delta":{"content":"It is done."}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
	)
	placeholder := newFakeVendor(t)
	modelID := e.registerModel(placeholder)
	e.pointModelAt(modelID, vendor)

	rec := e.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": "default", "name": "Gateway", "tools": []string{"current_time"},
	})
	if rec.Code != http.StatusCreated && rec.Code != http.StatusConflict {
		t.Fatalf("create the gateway: %d %s", rec.Code, rec.Body.String())
	}

	frames := e.streamTurn(token, map[string]any{"prompt": "what time is it", "model_id": modelID})
	for _, frame := range frames {
		if frame.ChatID != "" {
			return frame.ChatID
		}
	}
	t.Fatal("the turn produced no conversation")
	return ""
}

// Our own plumbing does not open.
//
// Looking up an ability, handing work to an agent, keeping a note: these are how
// the assistant is put together, not work somebody asked for, and their
// arguments are our own wiring. The row stays part of the conversation; there is
// simply nothing to open, which is structural rather than a panel that refuses.
func TestAnInternalToolHasNothingToOpen(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	chatID := env.turnCalling(t, token, "tool_guide", `{"tool_name":"current_time"}`)

	rec := env.do(http.MethodPost, "/v1/chat/history", token, map[string]any{"chat_id": chatID})
	env.expectStatus(rec, http.StatusOK)
	var answer historyResponse
	env.decode(rec, &answer)

	found := false
	for _, message := range answer.Messages {
		for _, call := range message.Tools {
			if call.Name != "tool_guide" {
				continue
			}
			found = true
			if call.ID != "" {
				t.Fatalf("an internal tool can be opened: %+v", call)
			}
			// It is still IN the conversation: what happened is not hidden,
			// only what it carried.
			if call.FriendlyName == "" && call.Name == "" {
				t.Fatalf("the row is missing entirely: %+v", call)
			}
		}
	}
	if !found {
		t.Fatalf("the internal tool was never called: %+v", answer.Messages)
	}
}

// turnCalling drives one turn in which the model calls a named tool.
func (e *testEnv) turnCalling(t *testing.T, token, toolName, args string) string {
	t.Helper()
	quoted, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("arguments: %v", err)
	}
	vendor := newFakeVendor(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"` +
				toolName + `","arguments":` + string(quoted) + `}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		},
		[]string{
			`{"choices":[{"index":0,"delta":{"content":"Looked it up."}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
	)
	placeholder := newFakeVendor(t)
	modelID := e.registerModel(placeholder)
	e.pointModelAt(modelID, vendor)

	rec := e.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": "default", "name": "Gateway", "tools": []string{"current_time"},
	})
	if rec.Code != http.StatusCreated && rec.Code != http.StatusConflict {
		t.Fatalf("create the gateway: %d %s", rec.Code, rec.Body.String())
	}

	frames := e.streamTurn(token, map[string]any{"prompt": "how do I tell the time", "model_id": modelID})
	for _, frame := range frames {
		if frame.ChatID != "" {
			return frame.ChatID
		}
	}
	t.Fatal("the turn produced no conversation")
	return ""
}

// An AGENT's tool call opens exactly like the Gateway's own.
//
// This is the row somebody most wants to read: the Gateway says it asked a
// specialist, and the question is what the specialist actually ran. The call is
// persisted like any other and the endpoint already served it; what was missing
// was the id on the frame, so the one row nobody could open was that one.
func TestOpeningAnAgentsToolCall(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	env.agentWithTools("ops", "Ops", "current_time")

	vendor := newFakeVendor(t,
		GatewayDelegatesWith("Asking the specialist.", "ops", "what time is it", model.HandoffContinue),
		agentCallsCurrentTime(),
		answerChunks("it is done"),                // the agent's result, read by the Gateway
		answerChunks("It is Friday, on the dot."), // the Gateway's narration
	)
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{"prompt": "what time is it", "model_id": modelID})

	// The agent's finished row, as the chat receives it.
	var agentRow chat.ToolMessage
	var delegateRow chat.ToolMessage
	for _, frame := range framesOfType(frames, chat.FrameTool) {
		switch msg := toolMessage(t, frame); msg.Name {
		case "current_time":
			agentRow = msg
		case "delegate":
			delegateRow = msg
		}
	}
	if agentRow.Name == "" {
		t.Fatalf("the agent's tool call never reached the chat: %+v", frames)
	}
	if agentRow.ID == "" {
		t.Fatalf("the agent's row carries no id, so it cannot be opened: %+v", agentRow)
	}
	// And handing the delegation to an agent is our own wiring: nothing to open.
	if delegateRow.ID != "" {
		t.Fatalf("the delegation row is openable: %+v", delegateRow)
	}

	// The id works: it opens, and it is the agent's call that comes back.
	rec := env.do(http.MethodPost, "/v1/chat/tool-call", token, map[string]any{"id": agentRow.ID})
	env.expectStatus(rec, http.StatusOK)
	var opened toolCallBody
	env.decode(rec, &opened)
	if opened.Name != "current_time" || opened.Status != model.ToolCallCompleted {
		t.Fatalf("the wrong call came back: %+v", opened)
	}
	if len(opened.Answered) == 0 {
		t.Fatalf("what the agent's tool answered is missing: %+v", opened)
	}
}

// agentCallsCurrentTime scripts an agent reply that calls one plain, ungated tool.
func agentCallsCurrentTime() []string {
	return []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_time","function":{"name":"current_time","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}
