package machine

import (
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/cmdpolicy"
)

// The gate in front of the terminal, now that the terminal remembers.
//
// A shell with a memory takes calls a one-shot never did: type into what is
// running, stop it, or ask for more of what it has printed. Each of those has
// no `command` in it, and the gate used to refuse anything without one, which
// would have made the new tool unusable in exactly the moments it exists for.

func gate(t *testing.T, policy cmdpolicy.Policy, args string) (json.RawMessage, string) {
	t.Helper()
	return checkTerminal(policy, json.RawMessage(args))
}

var denying = cmdpolicy.Policy{Mode: cmdpolicy.PolicyDenylist, Denied: "rm\ncurl"}

func TestTheGateReadsACommandAsItAlwaysDid(t *testing.T) {
	if _, reason := gate(t, denying, `{"command":"git status"}`); reason != "" {
		t.Fatalf("an ordinary command was refused: %s", reason)
	}
	_, reason := gate(t, denying, `{"command":"ls | xargs rm -rf"}`)
	if reason == "" {
		t.Fatal("a denied command hidden in a pipeline was allowed")
	}
	if !strings.Contains(reason, "rm") {
		t.Fatalf("the refusal does not say what it objected to: %s", reason)
	}
}

// Stopping is always allowed. Somebody watching a runaway command must never be
// told that the policy will not let them stop it.
func TestStoppingIsAlwaysAllowed(t *testing.T) {
	if _, reason := gate(t, denying, `{"stop":true}`); reason != "" {
		t.Fatalf("stopping was refused: %s", reason)
	}
	// Even with a policy that permits nothing at all.
	if _, reason := gate(t, cmdpolicy.Policy{}, `{"stop":true}`); reason != "" {
		t.Fatalf("stopping was refused by an empty policy: %s", reason)
	}
}

// Asking for more of what is running carries no command, and there is nothing
// to check: the machine says if there is nothing to read.
func TestAskingForMoreCarriesNoCommand(t *testing.T) {
	if _, reason := gate(t, denying, `{"wait":30}`); reason != "" {
		t.Fatalf("asking for more of a running command was refused: %s", reason)
	}
}

// Typing into something running is checked too. What is running might be a
// shell, and a denied program reached by typing at one is the same program.
func TestTypingIntoSomethingRunningIsChecked(t *testing.T) {
	if _, reason := gate(t, denying, `{"input":"yes"}`); reason != "" {
		t.Fatalf("an ordinary answer was refused: %s", reason)
	}
	_, reason := gate(t, denying, `{"input":"!rm -rf /"}`)
	if reason == "" {
		t.Fatal("a denied program typed into a running command was allowed")
	}
	if !strings.Contains(reason, "rm") {
		t.Fatalf("the refusal does not say what it objected to: %s", reason)
	}
}

// The zero policy refuses, which is the floor under a policy that could not be
// read. It is NOT what an unconfigured terminal has: that gets the tool's
// declared settings, which are a denylist (see the app package's tests for what
// a fresh terminal actually permits).
func TestTheZeroPolicyRefuses(t *testing.T) {
	_, reason := gate(t, cmdpolicy.Policy{}, `{"command":"ls"}`)
	if reason == "" {
		t.Fatal("a command ran under a policy that could not be read")
	}
	if !strings.Contains(reason, "its list is empty") {
		t.Fatalf("the refusal does not say what is wrong: %s", reason)
	}
}
