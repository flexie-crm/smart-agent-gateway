package machine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/cmdpolicy"
	"flexie.io/sag/internal/link"
	"flexie.io/sag/internal/tool"
)

// What the terminal may run is decided HERE, before anything reaches somebody's
// computer. The machine is a dumb executor: it does what it is told, and what it
// is told has already been read.

func TestTheRulesAreAppliedBeforeAnythingIsSent(t *testing.T) {
	sent := &recordingMachine{}
	policy := cmdpolicy.Policy{Mode: cmdpolicy.PolicyDenylist, Denied: "rm\nshutdown"}
	handle := terminalHandler(sent, policy)

	// A refused command never leaves this side. That is the point of checking
	// here: a computer that is asked to run something and refuses has still been
	// asked, and the refusal then depends on every application being up to date.
	result, err := handle(context.Background(), call(`{"command":"rm -rf /tmp/x"}`))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !strings.Contains(string(result.Content), "rm") {
		t.Fatalf("the refusal does not name what was refused: %s", result.Content)
	}
	if sent.calls != 0 {
		t.Fatalf("a refused command was sent to the computer anyway")
	}

	// And the ways around it are read, not matched: the command is parsed, so a
	// denied program is found wherever it is written in the line.
	//
	// `sh -c "rm ..."` is NOT in this list, and that is the policy's documented
	// limit rather than an oversight: what is inside a shell's argument is that
	// shell's business and nothing here parses it. The lists stop a mistake and
	// the obvious ways round a name; what bounds this tool is that it runs as
	// the person, on their own computer, in the folder they chose.
	for _, line := range []string{
		"echo hi && rm -rf /tmp/x",
		"ls | xargs rm",
		"$(rm -rf /tmp/x)",
		"rm=1 rm -rf /tmp/x",
	} {
		result, err := handle(context.Background(), call(`{"command":`+quote(line)+`}`))
		if err != nil {
			t.Fatalf("handle %q: %v", line, err)
		}
		if sent.calls != 0 {
			t.Fatalf("%q reached the computer", line)
		}
		if len(result.Content) == 0 {
			t.Fatalf("%q was refused without saying why", line)
		}
	}

	// What the rules allow does go, unchanged.
	if _, err := handle(context.Background(), call(`{"command":"git status"}`)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if sent.calls != 1 {
		t.Fatalf("an allowed command did not reach the computer: %d", sent.calls)
	}
	if sent.tool != "terminal" {
		t.Fatalf("it was sent as %q", sent.tool)
	}
}

// A conversation with no computer behind it says so, in words a person can act
// on, rather than failing as though the tool were broken.
func TestWithoutAComputerItSaysSo(t *testing.T) {
	handle := terminalHandler(&recordingMachine{},
		cmdpolicy.Policy{Mode: cmdpolicy.PolicyDenylist})
	result, err := handle(context.Background(), tool.Call{WorkspaceID: 1, UserID: 2, Args: json.RawMessage(`{"command":"git status"}`)})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !strings.Contains(string(result.Content), "chat application") {
		t.Fatalf("it does not say where this works: %s", result.Content)
	}
}

func call(args string) tool.Call {
	return tool.Call{WorkspaceID: 1, UserID: 2, DeviceID: "the-laptop", Args: json.RawMessage(args)}
}

func quote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// recordingMachine stands in for somebody's computer, and records whether it
// was asked to do anything at all.
type recordingMachine struct {
	calls int
	tool  string
}

func (m *recordingMachine) Call(_ context.Context, _, _ int64, _, name string, _ json.RawMessage, _ string) (link.Result, error) {
	m.calls++
	m.tool = name
	return link.Result{OK: true, Content: json.RawMessage(`{"exit_code":0}`)}, nil
}

func (m *recordingMachine) Runs(_, _ int64, _ string) map[string]int {
	return map[string]int{"terminal": 1}
}

// The zero policy runs nothing, and says the one thing that is true of it.
//
// This test used to be called "a terminal nobody has configured says so", and
// it asserted the message that came with that idea. Both were wrong. The zero
// policy is not what an unconfigured terminal has: a tool's settings are its
// declared defaults where a row says nothing (tool.Settings), and the terminal
// declares a denylist, so nothing that reaches a person goes through here. What
// this is now is the floor under a policy that could not be read at all, and
// the assertion is that it explains ITSELF rather than blaming an absent
// administrator.
func TestTheZeroPolicyRunsNothingAndSaysWhy(t *testing.T) {
	sent := &recordingMachine{}
	handle := terminalHandler(sent, cmdpolicy.Policy{})

	result, err := handle(context.Background(), call(`{"command":"git status"}`))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !strings.Contains(string(result.Content), "its list is empty") {
		t.Fatalf("it does not say what is actually wrong: %s", result.Content)
	}
	// The words that sent people to configure a tool they had configured.
	if strings.Contains(string(result.Content), "no commands have been permitted") {
		t.Fatalf("it still blames an administrator: %s", result.Content)
	}
	if sent.calls != 0 {
		t.Fatal("a terminal with no usable policy ran something")
	}
}
