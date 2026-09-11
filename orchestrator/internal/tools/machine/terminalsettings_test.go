package machine

import (
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/cmdpolicy"
	"flexie.io/sag/internal/tool"
)

// Settings the terminal could not enforce, refused before they are stored.
//
// The bug this exists for: a terminal saved with no rule at all reported
// success and then refused every command, telling the person an administrator
// had to choose some, when the administrator was the person who had just saved
// it. The tool's own settings are the only place that can tell.

func check(t *testing.T, config string) error {
	t.Helper()
	return checkTerminalSettings(json.RawMessage(config))
}

// Nothing chosen is not a terminal that runs everything, it is one that runs
// nothing: an unset rule defaults to an allowlist, and an empty allowlist
// permits nothing. Saving that has to fail.
func TestNothingChosenIsRefused(t *testing.T) {
	err := check(t, `{}`)
	if err == nil {
		t.Fatal("a terminal with no rule at all was accepted")
	}
	if !strings.Contains(err.Error(), "list the commands") {
		t.Fatalf("the refusal does not say what to do about it: %v", err)
	}
	if err := check(t, ``); err == nil {
		t.Fatal("an absent config was accepted")
	}
}

// The same rule, chosen deliberately and left empty. It is the identical
// terminal and it is refused for the identical reason.
func TestAnAllowlistOfNothingIsRefused(t *testing.T) {
	if err := check(t, `{"policy":{"mode":"allowlist","allowed":""}}`); err == nil {
		t.Fatal("an allowlist with nothing in it was accepted")
	}
	if err := check(t, `{"policy":{"mode":"allowlist","allowed":"  \n \n"}}`); err == nil {
		t.Fatal("an allowlist of blank lines was accepted")
	}
}

// And the two that really can be enforced.
func TestAWorkablePolicyIsAccepted(t *testing.T) {
	if err := check(t, `{"policy":{"mode":"allowlist","allowed":"git\nls"}}`); err != nil {
		t.Fatalf("an allowlist with commands in it was refused: %v", err)
	}
	// A denylist with nothing in it is a real choice: everything may run. It is
	// the opposite of the allowlist case and must not be caught by it.
	if err := check(t, `{"policy":{"mode":"denylist","denied":""}}`); err != nil {
		t.Fatalf("a denylist that refuses nothing was refused: %v", err)
	}
}

func TestSettingsThatCannotBeReadAreRefused(t *testing.T) {
	if err := check(t, `{"policy":`); err == nil {
		t.Fatal("unreadable settings were accepted")
	}
}

// The declaration itself: the schema has to carry the check, or nothing calls it.
func TestTheSchemaCarriesTheCheck(t *testing.T) {
	if terminalSchema().CheckSettings == nil {
		t.Fatal("the terminal declares settings but nothing validates them")
	}
}

// The exact failure a person reported, at the exact line that produced it.
//
// "echo ok" came back as "no commands have been permitted for this tool yet",
// on a terminal that was switched on and granted, because the settings a tool
// SHIPS with never reached the gate: the row was empty, the policy parsed to an
// empty mode, and an empty mode is an allowlist of nothing. The tool's own form
// declares a denylist. This asserts the gate is handed what the form declares.
func TestTheGateGetsWhatTheFormDeclares(t *testing.T) {
	settings, err := tool.SettingsConfig(terminalSchema().Settings, nil)
	if err != nil {
		t.Fatalf("the declared settings could not be resolved: %v", err)
	}
	var stored struct {
		Policy cmdpolicy.Policy `json:"policy"`
	}
	if err := json.Unmarshal(settings, &stored); err != nil {
		t.Fatalf("the declared settings are not a policy: %v", err)
	}
	if _, reason := checkTerminal(stored.Policy, json.RawMessage(`{"command":"echo ok"}`)); reason != "" {
		t.Fatalf("echo ok on an unconfigured terminal: %s", reason)
	}
	// And the message that was reported is genuinely unreachable from here.
	if _, reason := checkTerminal(stored.Policy, json.RawMessage(`{"command":"ls -la"}`)); reason != "" {
		t.Fatalf("ls on an unconfigured terminal: %s", reason)
	}
}

// The floor under it: an allowlist somebody emptied on purpose still refuses,
// and still says so in the words that tell them what to do.
func TestAnEmptiedAllowlistStillRefuses(t *testing.T) {
	empty := cmdpolicy.Policy{Mode: cmdpolicy.PolicyAllowlist}
	_, reason := checkTerminal(empty, json.RawMessage(`{"command":"echo ok"}`))
	if !strings.Contains(reason, "its list is empty") {
		t.Fatalf("an emptied allowlist did not refuse: %q", reason)
	}
}
