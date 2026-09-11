package app

import (
	"encoding/json"
	"testing"

	"flexie.io/sag/internal/cmdpolicy"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/machine"
)

// What the terminal may run when nobody has configured it.
//
// The bug: it could run nothing, and said so by blaming an administrator. A row
// nobody has touched has no settings, the zero policy has an empty mode,
// cmdpolicy reads an empty mode as an allowlist, and an empty allowlist permits
// nothing. The tool's own form has always declared it ships as a DENYLIST, so
// the screen said one thing and the gate did another.

// The real schema, from the real registration, so this cannot pass against a
// declaration the product does not ship.
func terminalSchema(t *testing.T) tool.Schema {
	t.Helper()
	for _, registered := range machine.Tools(nil) {
		if registered.Schema.Name == machine.TerminalName {
			return registered.Schema
		}
	}
	t.Fatal("the terminal is not among the machine tools")
	return tool.Schema{}
}

// The declared default reaches the gate: no settings at all is the tool as its
// own form describes it, which is everything except what is listed, and nothing
// is listed.
func TestAnUnconfiguredTerminalRunsWhatItsFormSaysItWill(t *testing.T) {
	policy, failed := terminalPolicy(terminalSchema(t), nil)
	if failed != nil {
		t.Fatalf("the declared settings could not be resolved: %v", failed)
	}
	if policy.Mode != cmdpolicy.PolicyDenylist {
		t.Fatalf("an unconfigured terminal is %q, and its form declares %q",
			policy.Mode, cmdpolicy.PolicyDenylist)
	}
	if err := policy.WithDefaults().Validate(); err != nil {
		t.Fatalf("an unconfigured terminal cannot be enforced: %v", err)
	}
	// The whole point, said as the person experiences it.
	if reason := policy.WithDefaults().Check("echo ok"); reason != "" {
		t.Fatalf("echo ok was refused by a terminal nobody has restricted: %s", reason)
	}
}

// And the floor is still a floor: the declared default is a denylist, not an
// absence of rules. Running as another user stays refused until somebody says
// otherwise, which is the other half of what the form declares.
func TestTheDeclaredDefaultStillRefusesRunningAsAnotherUser(t *testing.T) {
	resolved, failed := terminalPolicy(terminalSchema(t), nil)
	if failed != nil {
		t.Fatalf("the declared settings could not be resolved: %v", failed)
	}
	ready := resolved.WithDefaults()
	if ready.Sudo != cmdpolicy.SudoDeny {
		t.Fatalf("sudo defaulted to %q", ready.Sudo)
	}
	if reason := ready.Check("sudo rm -rf /"); reason == "" {
		t.Fatal("an unconfigured terminal permitted sudo")
	}
}

// An administrator's choice still wins, in both directions.
func TestWhatAnAdministratorWroteWins(t *testing.T) {
	schema := terminalSchema(t)
	strict, _ := terminalPolicy(schema, json.RawMessage(`{"policy":{"mode":"allowlist","allowed":"git"}}`))
	if strict.Mode != cmdpolicy.PolicyAllowlist {
		t.Fatalf("a chosen allowlist was overwritten by the default: %q", strict.Mode)
	}
	if reason := strict.WithDefaults().Check("rm -rf /"); reason == "" {
		t.Fatal("an allowlist of git permitted rm")
	}
	if reason := strict.WithDefaults().Check("git status"); reason != "" {
		t.Fatalf("an allowlist of git refused git: %s", reason)
	}
}

// The case that made the old code wrong in a second way: settings saved with a
// denied list and no rule. The declared rule fills the gap, per key, instead of
// the whole object falling back to a zero value.
func TestAPartialPolicyGetsTheDeclaredRule(t *testing.T) {
	policy, _ := terminalPolicy(terminalSchema(t), json.RawMessage(`{"policy":{"denied":"rm"}}`))
	if policy.Mode != cmdpolicy.PolicyDenylist {
		t.Fatalf("a policy saved without a rule became %q", policy.Mode)
	}
	ready := policy.WithDefaults()
	if reason := ready.Check("rm -rf /"); reason == "" {
		t.Fatal("the denied command was permitted")
	}
	if reason := ready.Check("echo ok"); reason != "" {
		t.Fatalf("a command nobody denied was refused: %s", reason)
	}
}

// Settings that cannot be read are not a terminal that runs everything.
func TestUnreadableSettingsDoNotOpenTheTerminal(t *testing.T) {
	policy, failed := terminalPolicy(terminalSchema(t), json.RawMessage(`{"policy":`))
	if failed == nil {
		t.Fatal("unreadable settings were accepted")
	}
	if policy.Mode == cmdpolicy.PolicyDenylist {
		t.Fatal("unreadable settings were read as a denylist, which permits everything")
	}
}
