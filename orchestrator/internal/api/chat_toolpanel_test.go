package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"flexie.io/sag/internal/link"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tools/machine"
)

// What a person reads when they open a call to a tool that DECLARED what to
// show, end to end: the model calls the terminal on somebody's own computer, the
// call is persisted, and the row is opened through the endpoint the chat uses.
//
// The declaration itself has close unit tests, and none of them runs a turn. The
// gap between them and the product is everything in the middle: the loop
// persisting the arguments and the answer, the endpoint finding them, and the
// tool's own account of itself being applied to what is really there rather than
// to a fixture written next to the assertion.
func TestOpeningATerminalCallIsLaidOutAsTheToolDeclares(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// The computer answers the way the application really answers a command:
	// what it printed, where it ran, and the two the model reads and a person
	// does not (the row already says whether it worked).
	env.app.Machines = terminalMachine{
		runs: machine.Versions(),
		answer: `{"output":"hello\n","exit_code":0,"running":false,` +
			`"directory":"/Users/someone/work"}`,
	}

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	vendor := newFakeVendor(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_sh","function":{"name":"terminal","arguments":"{\"command\":\"echo hello\",\"directory\":\"work\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		},
		answerChunks("It printed hello."),
	)
	env.pointModelAt(modelID, vendor)

	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": "default", "name": "Gateway", "tools": []string{"terminal"},
	})
	env.expectStatus(rec, http.StatusCreated)

	// An administrator permits the command, through the form they really use.
	// Without this the terminal refuses everything, which is the tool as it
	// ships: a test that skipped this would be asserting the layout of a
	// refusal and calling it the layout of a command.
	env.permitCommand(t, token, "echo")

	frames := env.streamTurn(token, map[string]any{
		"prompt": "run echo hello", "model_id": modelID, "device_id": "the-laptop",
	})
	chatID := env.sessionOf(frames).UID

	row := env.firstToolRow(t, token, chatID)
	if row.Name != "terminal" || row.ID == "" {
		t.Fatalf("the terminal row is not there to open: %+v", row)
	}

	rec = env.do(http.MethodPost, "/v1/chat/tool-call", token, map[string]any{"id": row.ID})
	env.expectStatus(rec, http.StatusOK)
	var opened toolCallBody
	env.decode(rec, &opened)

	// The folder is a fact about the whole call, so it is stated ONCE, above
	// both halves, and not repeated in either.
	if len(opened.Where) != 1 || opened.Where[0].Name != "directory" {
		t.Fatalf("where it ran is not stated once at the top: %+v", opened.Where)
	}
	if fieldNamed(opened.Sent, "directory") != nil || fieldNamed(opened.Answered, "directory") != nil {
		t.Fatalf("the folder is repeated inside the call: %+v", opened)
	}

	// What was run is drawn as a command, not as a labelled JSON string.
	command := fieldNamed(opened.Sent, "command")
	if command == nil || command.As != "command" {
		t.Fatalf("the command is not shown as a command: %+v", opened.Sent)
	}
	if string(command.Value) != `"echo hello"` {
		t.Fatalf("the command shown is not the command run: %s", command.Value)
	}

	// What it printed keeps its lines, and needs no label.
	output := fieldNamed(opened.Answered, "output")
	if output == nil || output.As != "text" {
		t.Fatalf("the output is not shown as text: %+v", opened.Answered)
	}
	if string(output.Value) != `"hello\n"` {
		t.Fatalf("the output shown is not what it printed: %s", output.Value)
	}

	// And what the tool did not declare is not shown at all: the exit code and
	// the running flag are for the model to read. A successful call that says
	// "exit_code: 0" is noise in front of the one thing somebody opened it for.
	for _, hidden := range []string{"exit_code", "running"} {
		if fieldNamed(opened.Sent, hidden) != nil || fieldNamed(opened.Answered, hidden) != nil {
			t.Fatalf("%q is shown to a person: %+v", hidden, opened)
		}
	}
}

// permitCommand writes the terminal's command policy the way the console does:
// find the tool, allow one command, save.
func (e *testEnv) permitCommand(t *testing.T, token, command string) {
	t.Helper()
	rec := e.do(http.MethodGet, "/v1/tools", token, nil)
	e.expectStatus(rec, http.StatusOK)
	var listed []toolBody
	e.decode(rec, &listed)
	for _, tool := range listed {
		if tool.Name != "terminal" {
			continue
		}
		rec = e.do(http.MethodPut, "/v1/tools/"+itoa64(tool.ID), token, map[string]any{
			"status": tool.Status,
			"grants": tool.Grants,
			"settings": map[string]any{
				"policy.mode": "allowlist", "policy.allowed": command,
			},
		})
		e.expectStatus(rec, http.StatusOK)
		return
	}
	t.Fatal("this build ships no terminal to configure")
}

// fieldNamed finds one field of a group, or nothing.
func fieldNamed(fields []shownField, name string) *shownField {
	for i, f := range fields {
		if f.Name == name {
			return &fields[i]
		}
	}
	return nil
}

// terminalMachine is a computer that runs one command and answers as the
// application does.
type terminalMachine struct {
	runs   map[string]int
	answer string
}

func (m terminalMachine) Call(
	context.Context, int64, int64, string, string, json.RawMessage, string,
) (link.Result, error) {
	return link.Result{OK: true, Content: json.RawMessage(m.answer)}, nil
}

func (m terminalMachine) Runs(_, _ int64, deviceID string) map[string]int {
	if deviceID == "" {
		return nil
	}
	return m.runs
}
