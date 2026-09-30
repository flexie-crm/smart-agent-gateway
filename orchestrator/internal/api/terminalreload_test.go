package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
)

// A terminal-mode agent's answer, which is the turn's answer, is in the
// conversation after a reload, including the notice an agent gives when it runs
// out of steps, and the model is shown it once on the next turn.

// reloaded is what a person is shown when they come back to the conversation.
func reloaded(t *testing.T, env *testEnv, token, chatID string) []historyMessage {
	t.Helper()
	rec := env.do(http.MethodPost, "/v1/chat/history", token, map[string]any{"chat_id": chatID})
	env.expectStatus(rec, http.StatusOK)
	var page historyResponse
	env.decode(rec, &page)
	return page.Messages
}

// saysAfterThePrompt reports whether an assistant message with this text comes
// after the person's prompt, which is where they saw it live.
func saysAfterThePrompt(messages []historyMessage, prompt, want string) bool {
	asked := false
	for _, m := range messages {
		if m.Role == model.StepUser && m.Content == prompt {
			asked = true
			continue
		}
		if asked && m.Role == model.StepAssistant && m.Content == want {
			return true
		}
	}
	return false
}

func TestATerminalAnswerIsThereAfterAReload(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")
	env.agent("builder", "Builder", "Build the thing end to end.")

	vendor := newFakeVendor(t,
		agentCall("builder", "Build it", model.HandoffTerminal),
		answerChunks("Built and done."), // the agent's answer, which is the turn's
		answerChunks("Glad it helped."), // the Gateway, on the next turn
	)
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "make it", "model_id": modelID})
	chatID := env.sessionOf(frames).UID

	if got := reloaded(t, env, token, chatID); !saysAfterThePrompt(got, "make it", "Built and done.") {
		t.Fatalf("the agent's answer is not in the conversation after a reload: %+v", got)
	}

	// And the model is told it ONCE on the next turn, as its own reply: the
	// agent's own step is still left out of the Gateway's transcript, so
	// recording the answer cannot put it there twice.
	env.streamTurn(token, map[string]any{"prompt": "thanks", "model_id": modelID, "chat_id": chatID})
	last := vendor.prompts[len(vendor.prompts)-1]
	raw, err := json.Marshal(last["messages"])
	if err != nil {
		t.Fatalf("marshal what the model was sent: %v", err)
	}
	var sent []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("decode what the model was sent: %v", err)
	}
	replies := 0
	for _, m := range sent {
		if text, _ := m.Content.(string); m.Role == "assistant" && strings.Contains(text, "Built and done.") {
			replies++
		}
	}
	if replies != 1 {
		t.Fatalf("the model was shown its terminal answer %d times on the next turn, want 1: %s", replies, raw)
	}
}

// The notice an agent gives when it runs out of steps is the case this was
// found through: it exists to say why a task stopped, and a reload lost it.
func TestAnAgentOutOfStepsIsStillExplainedAfterAReload(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	two := 2
	if err := env.app.Store.Agents().Create(context.Background(), &model.Agent{
		WorkspaceID: env.ws.ID, Key: "research", Name: "Research",
		Instructions: "Do the work.", Status: model.StatusActive,
		Tools: []string{"current_time"}, MaxIterations: &two,
	}, model.Nobody()); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	ask := func(id string) []string {
		return []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"` + id + `","function":{"name":"current_time","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		}
	}
	vendor := newFakeVendor(t,
		agentCall("research", "sweep everything", model.HandoffTerminal),
		ask("call_a"), ask("call_b"), ask("call_c"),
	)
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "sweep", "model_id": modelID})
	chatID := env.sessionOf(frames).UID

	var notice string
	for _, m := range reloaded(t, env, token, chatID) {
		if m.Role == model.StepAssistant && strings.Contains(m.Content, "limit of 2 steps") {
			notice = m.Content
		}
	}
	if notice == "" {
		t.Fatal("after a reload, nothing says the agent ran out of steps")
	}
	if !strings.Contains(notice, "Research") || !strings.Contains(notice, "console") {
		t.Fatalf("the notice after a reload is not the whole notice: %q", notice)
	}
}

// The second place a terminal answer ends a turn: an agent that stopped for a
// person's approval, was approved, and then answered.
func TestATerminalAnswerAfterAnApprovalIsThereAfterAReload(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	env.agentWithTools("ops", "Ops", "set_model_status")

	vendor := newFakeVendor(t,
		agentCall("ops", "disable that model", model.HandoffTerminal),
		agentParkingCall(modelID),
		answerChunks("All set: the model is disabled."),
	)
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{"prompt": "disable it", "model_id": modelID})
	chatID := env.sessionOf(frames).UID
	card := confirmRequest(t, frames[len(frames)-1])
	env.streamTurn(token, map[string]any{"resume_token": card.Token, "resume_action": "approved"})

	if got := reloaded(t, env, token, chatID); !saysAfterThePrompt(got, "disable it", "All set: the model is disabled.") {
		t.Fatalf("the agent's answer after the approval is not there after a reload: %+v", got)
	}
}
