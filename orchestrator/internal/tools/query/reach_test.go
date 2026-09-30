package query

import (
	"testing"

	"flexie.io/sag/internal/tools/template"
)

// The checkbox an administrator ticked survives being stored.
//
// This is the defect it closes, and it means the box did nothing at all: the
// form names the field with a dot, Config stores settings by splitting dotted
// keys into nested objects, and the reader looked for a flat key with a dot in
// its JSON name. ThroughChat was always false, so a database on an office
// network was dialled from this server rather than from the computer that can
// see it, and nothing said so, because a database that cannot be reached is
// exactly what somebody ticking the box already has.
func TestTheReachCheckboxSurvivesBeingStored(t *testing.T) {
	cfg, err := queryTemplate{}.Config("mysql", map[string]any{
		"host": "db.internal.example", "database": "acct", "username": "u",
		"access": "read", template.ReachChatKey: "true",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	s, err := ParseConfig(cfg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !s.ThroughChat {
		t.Fatalf("the checkbox did not survive: %s", cfg)
	}

	// And a tool nobody ticked it on is unchanged.
	plain, err := queryTemplate{}.Config("mysql", map[string]any{
		"host": "db", "database": "acct", "username": "u", "access": "read",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if other, err := ParseConfig(plain); err != nil || other.ThroughChat {
		t.Fatalf("a tool with the box unticked reads as ticked: %v %v", other.ThroughChat, err)
	}
}
