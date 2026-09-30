package model

import (
	"encoding/json"
	"testing"
)

// The embedded author fields must stay FLAT on the wire.
//
// Embedding is a readability choice, not a protocol change: every client reads
// `created_by_name` at the top of the object. Go flattens an anonymous embedded
// struct with no json tag, which is true until somebody gives it one, so it is
// asserted rather than trusted.
func TestTheAuthorFieldsAreFlatOnTheWire(t *testing.T) {
	brain := Brain{Name: "Product manual"}
	brain.Made(Actor{UserID: 7, Name: "Maren"})
	brain.Changed(Actor{Name: "Research agent"})

	raw, err := json.Marshal(brain)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var flat map[string]any
	if err := json.Unmarshal(raw, &flat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, field := range []string{"created_by", "created_by_name", "updated_by", "updated_by_name"} {
		if _, ok := flat[field]; !ok {
			t.Errorf("%q is not a field of the object: %s", field, raw)
		}
	}
	// And nothing arrived nested where the embedding is.
	for _, nested := range []string{"Authored", "Edited"} {
		if _, found := flat[nested]; found {
			t.Errorf("the author fields arrived nested under %q: %s", nested, raw)
		}
	}
	if flat["created_by_name"] != "Maren" || flat["updated_by_name"] != "Research agent" {
		t.Errorf("names = %v / %v", flat["created_by_name"], flat["updated_by_name"])
	}
}

// A row that cannot be edited carries no updated pair, on the wire or anywhere
// else. Two fields that are always zero are a promise the data does not keep.
func TestAnImmutableRowHasNoUpdatedPair(t *testing.T) {
	version := SkillVersion{Number: 1}
	version.Made(Actor{UserID: 3, Name: "An Importer"})

	raw, err := json.Marshal(version)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var flat map[string]any
	if err := json.Unmarshal(raw, &flat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, found := flat["updated_by"]; found {
		t.Errorf("an immutable row carries an updated_by: %s", raw)
	}
	if flat["created_by_name"] != "An Importer" {
		t.Errorf("created_by_name = %v", flat["created_by_name"])
	}
}

func TestMadeAndChangedRecordDifferentThings(t *testing.T) {
	type row struct {
		Authored
		Edited
	}
	var r row
	r.Made(Actor{UserID: 1, Name: "A Person"})
	r.Changed(Actor{UserID: 1, Name: "A Person"})
	if r.CreatedByName != "A Person" || r.UpdatedByName != "A Person" {
		t.Fatalf("after creating: %q / %q", r.CreatedByName, r.UpdatedByName)
	}

	r.Changed(Actor{Name: "Research agent"})
	if r.CreatedBy != 1 || r.CreatedByName != "A Person" {
		t.Errorf("an edit rewrote the author: %d/%q", r.CreatedBy, r.CreatedByName)
	}
	if r.UpdatedBy != 0 || r.UpdatedByName != "Research agent" {
		t.Errorf("the edit was recorded as %d/%q", r.UpdatedBy, r.UpdatedByName)
	}
}
