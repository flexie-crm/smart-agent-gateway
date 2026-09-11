package api

import (
	"net/http"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
)

// A turn that runs out of steps SAYS SO, and the saying survives a reload.
//
// It used to end in silence. The loop produced "I was not able to finish this
// task", streamed it, and never wrote it down: whoever was watching saw it, and
// whoever came back to the conversation saw a turn that stopped mid-task after
// a tool call, with nothing to say why. It reads as a disconnection, and the
// person who reported it as one was looking at a thirteen-minute turn that had
// simply reached its configured limit.
//
// So the notice is a step like any other, and it says what can be done: the
// limit is configuration, and the person reading it is usually the person who
// can raise it.
func TestATurnThatRunsOutOfStepsSaysSoAndItSurvivesAReload(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)

	// An assistant that may take two steps, and a model that never stops asking
	// for another: the shape of a sweep that runs away with itself.
	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": "default", "name": "Gateway",
		"tools": []string{"current_time"}, "max_iterations": 2,
	})
	env.expectStatus(rec, http.StatusCreated)

	askForTheTime := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"current_time","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	vendor := newFakeVendor(t, askForTheTime, askForTheTime, askForTheTime, askForTheTime)
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{"prompt": "sweep everything", "model_id": modelID})
	chatID := env.sessionOf(frames).UID

	// Reload: what a person coming back to it is shown.
	rec = env.do(http.MethodPost, "/v1/chat/history", token, map[string]any{"chat_id": chatID})
	env.expectStatus(rec, http.StatusOK)
	var page historyResponse
	env.decode(rec, &page)

	var last string
	for _, message := range page.Messages {
		if message.Role == model.StepAssistant && message.Content != "" {
			last = message.Content
		}
	}
	if last == "" {
		t.Fatalf("the conversation ends with nothing said, which reads as a disconnection:\n%+v", page.Messages)
	}
	if !strings.Contains(last, "limit") {
		t.Fatalf("the last thing said does not mention the limit: %q", last)
	}
	if !strings.Contains(last, "console") {
		t.Fatalf("the notice does not say where the limit can be raised: %q", last)
	}
	if !strings.Contains(last, "2 steps") {
		t.Fatalf("the notice does not say what the limit WAS: %q", last)
	}
}
