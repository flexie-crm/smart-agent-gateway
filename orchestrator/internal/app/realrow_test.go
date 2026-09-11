package app

import (
	"encoding/json"
	"testing"

	"flexie.io/sag/internal/cmdpolicy"
)

// The row a real installation actually had, copied out of it verbatim:
//
//	{"policy":{"denied":"rm\nshutdown"}}
//
// A denied list and no rule, because the settings form's select never sent its
// value. Every command was refused with "no commands have been permitted for
// this tool yet", which named the one thing the person had in fact done.
func TestTheRowFromARealInstallation(t *testing.T) {
	const real = `{"policy":{"denied":"rm\nshutdown"}}`
	resolved, failed := terminalPolicy(terminalSchema(t), json.RawMessage(real))
	if failed != nil {
		t.Fatalf("the row could not be resolved: %v", failed)
	}
	ready := resolved.WithDefaults()

	if ready.Mode != cmdpolicy.PolicyDenylist {
		t.Fatalf("the rule resolved to %q", ready.Mode)
	}
	if reason := ready.Check("echo ok"); reason != "" {
		t.Fatalf("echo ok was refused: %s", reason)
	}
	// And what they actually asked to be refused, still is.
	for _, forbidden := range []string{"rm -rf /", "shutdown -h now", "ls | xargs rm"} {
		if reason := ready.Check(forbidden); reason == "" {
			t.Fatalf("%q was permitted", forbidden)
		}
	}
}
