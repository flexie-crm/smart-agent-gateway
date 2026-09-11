package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"flexie.io/sag/internal/link"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/machine"
)

// Which computer a message came from has to reach the tool.
//
// Everything else about this is proved elsewhere: the registry routes by
// device, a token carries one, the page sends one. What none of that proves is
// the part in between, which is a field copied twice, from the request to the
// turn and from the turn to the call. If either copy were missed, every tool
// that reaches somebody's own machine would answer "the chat application is not
// connected on this computer" and look like a broken link rather than a
// dropped field.
func TestTheComputerAMessageCameFromReachesTheTool(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// A tool that does nothing but remember who called it.
	seen := make(chan tool.Call, 4)
	if err := env.app.Tools.Register(tool.Tool{
		Schema: tool.Schema{
			Name:         "remember_the_caller",
			FriendlyName: "Remember the caller",
			Description:  "Records the call, for a test.",
			Kind:         tool.KindBuiltin,
			Risk:         tool.RiskReadOnly,
			InputSchema:  json.RawMessage(`{"type":"object","properties":{}}`),
		},
		Handle: func(_ context.Context, call tool.Call) (tool.Result, error) {
			seen <- call
			return tool.Result{Content: json.RawMessage(`{"ok":true}`)}, nil
		},
	}); err != nil {
		t.Fatalf("register the tool: %v", err)
	}
	if err := env.app.SyncTools(context.Background(), env.ws.ID); err != nil {
		t.Fatalf("sync tools: %v", err)
	}

	vendor := newFakeVendor(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"remember_the_caller","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		},
		[]string{
			`{"choices":[{"index":0,"delta":{"content":"Noted."}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
	)
	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.pointModelAt(modelID, vendor)

	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": "default", "name": "Gateway", "tools": []string{"remember_the_caller"},
	})
	env.expectStatus(rec, http.StatusCreated)

	env.streamTurn(token, map[string]any{
		"prompt": "remember me", "model_id": modelID, "device_id": "the-laptop",
	})

	select {
	case call := <-seen:
		if call.DeviceID != "the-laptop" {
			t.Fatalf("the tool was called from %q, not the computer the message came from", call.DeviceID)
		}
		if call.UserID == 0 || call.WorkspaceID == 0 {
			t.Fatalf("the call lost who was asking: %+v", call)
		}
	default:
		t.Fatal("the tool was never called")
	}

	// And a message from a browser carries no device, rather than the last one
	// seen: a tool then says so instead of reaching somebody's laptop.
	env.pointModelAt(modelID, newFakeVendor(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_2","function":{"name":"remember_the_caller","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		},
		[]string{
			`{"choices":[{"index":0,"delta":{"content":"Noted."}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
	))
	env.streamTurn(token, map[string]any{"prompt": "again", "model_id": modelID})

	select {
	case call := <-seen:
		if call.DeviceID != "" {
			t.Fatalf("a message with no computer produced one: %q", call.DeviceID)
		}
	default:
		t.Fatal("the tool was never called the second time")
	}
}

// A tool that runs on the person's computer reaches the MODEL, not just the
// tool call.
//
// This is the gap the other test left, and the bug that lived in it: the device
// travelled from the request to the turn to the call, and the loadout was built
// from a ProfileRequest that never carried it. Every visible signal was right —
// the machine was linked, the request had the device, the turn had seven tools
// — and the two that run on a computer were silently filtered out of all seven,
// so the assistant said it had no terminal and nothing anywhere disagreed.
//
// So this asserts what the MODEL was offered, which is the only thing that
// decides whether the ability exists.
func TestAMachineToolIsOfferedToTheModelWhenAComputerIsThere(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// A machine, linked, declaring the terminal at the version we speak.
	// A computer, linked, declaring both tools at the versions we speak. The
	// app depends on the capability rather than on the registry, so a test
	// answers it directly instead of standing up a websocket and a second
	// process to say one sentence.
	// What it declares is asked for rather than repeated: the numbers are the
	// gateway's own, and a fixture that wrote them out went on saying "the
	// terminal" until the terminal's arguments changed shape, at which point it
	// silently meant "a computer too old to be offered one".
	env.app.Machines = linkedMachine{runs: machine.Versions()}

	// Two turns, each answered without a tool call: what is being asked is what
	// the model was OFFERED, not what it did with it.
	said := []string{
		`{"choices":[{"index":0,"delta":{"content":"Noted."}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	vendor := newFakeVendor(t, said, said)
	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.pointModelAt(modelID, vendor)

	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": "default", "name": "Gateway",
		"tools": []string{"terminal", "machine_info", "current_time"},
	})
	env.expectStatus(rec, http.StatusCreated)

	// From a computer: the terminal is there.
	env.streamTurn(token, map[string]any{
		"prompt": "hello", "model_id": modelID, "device_id": "the-laptop",
	})
	offered := vendor.sentToolsAsking("hello")
	if !slices.Contains(offered, "terminal") {
		t.Fatalf("the model was not offered the terminal: %v", offered)
	}
	if !slices.Contains(offered, "machine_info") {
		t.Fatalf("the model was not offered machine_info: %v", offered)
	}

	// From a browser: it is not, because there is no computer to run it on.
	env.streamTurn(token, map[string]any{"prompt": "hello again", "model_id": modelID})
	offered = vendor.sentToolsAsking("hello again")
	if slices.Contains(offered, "terminal") {
		t.Fatalf("a browser was offered a tool that runs on a computer: %v", offered)
	}
}

// linkedMachine is a computer that is there and can do the things it says.
// Nothing calls through it here: what is being asked is what the model was
// OFFERED, which is decided before anything runs.
type linkedMachine struct {
	runs map[string]int
}

func (m linkedMachine) Call(context.Context, int64, int64, string, string, json.RawMessage, string) (link.Result, error) {
	return link.Result{OK: true, Content: json.RawMessage(`{}`)}, nil
}

func (m linkedMachine) Runs(_, _ int64, deviceID string) map[string]int {
	if deviceID == "" {
		return nil // a browser has no computer, and this must not pretend otherwise
	}
	return m.runs
}
