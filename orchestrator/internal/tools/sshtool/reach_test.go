package sshtool

import (
	"testing"

	"flexie.io/sag/internal/tools/template"
)

// The checkbox an administrator ticked survives being stored.
//
// Same defect as the database tool's, and it meant the box did nothing: the
// form names the field with a dot, Config stores settings by splitting dotted
// keys into nested objects, and the reader looked for a flat key with a dot in
// its JSON name. A server on an office network was dialled from this server
// rather than from the computer that can see it.
func TestTheReachCheckboxSurvivesBeingStored(t *testing.T) {
	cfg, err := sshTemplate{}.Config(VariantSSH, map[string]any{
		"host": "box.internal.example", "username": "deploy",
		"auth.password": "s3cret", "policy.mode": "denylist", "policy.denied": "rm",
		template.ReachChatKey: "true",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	settings, err := parseSettings(cfg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	t.Logf("stored: %s", cfg)
	if !settings.ThroughChat {
		t.Fatalf("the checkbox did not survive: %s", cfg)
	}

	plain, err := sshTemplate{}.Config(VariantSSH, map[string]any{
		"host": "box", "username": "deploy", "auth.password": "s3cret",
		"policy.mode": "denylist", "policy.denied": "rm",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if other, err := parseSettings(plain); err != nil || other.ThroughChat {
		t.Fatalf("a tool with the box unticked reads as ticked: %v %v", other.ThroughChat, err)
	}
}
