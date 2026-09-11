package api

import (
	"encoding/json"
	"testing"

	"flexie.io/sag/internal/tool"
)

// What a native tool's settings form opens filled with.
//
// The bug: nothing stored meant nothing sent, and a select with no value
// renders showing its first option. So the terminal's settings read "Every
// command except the ones I list", saved, said they were saved, and produced a
// tool that ran nothing, because the value never left the browser and an empty
// policy defaults to an allowlist of nothing. Choosing the option already on
// the screen fires no change event, so the only way through it was to pick the
// other rule and pick this one back.

var aForm = []tool.Section{{
	Title: "Commands",
	Fields: []tool.Field{
		{
			Key: "policy.mode", Type: tool.FieldSelect, Default: "denylist",
			Options: []tool.Option{
				tool.Choice("denylist", "Every command except the ones I list"),
				tool.Choice("allowlist", "Only the commands I list"),
			},
		},
		{Key: "policy.denied", Type: tool.FieldTextarea},
	},
}}

func TestAFormWithNothingStoredOpensOnItsDefaults(t *testing.T) {
	values := declaredValues(aForm, nil)
	if values["policy.mode"] != "denylist" {
		t.Fatalf("the declared default did not reach the form: %v", values["policy.mode"])
	}
	// A field with no default stays absent rather than arriving as an empty
	// string: the form shows nothing there, which is the truth.
	if _, present := values["policy.denied"]; present {
		t.Fatalf("a field with no default was invented: %v", values)
	}
}

func TestWhatIsStoredBeatsTheDefault(t *testing.T) {
	values := declaredValues(aForm, json.RawMessage(`{"policy":{"mode":"allowlist","denied":"rm"}}`))
	if values["policy.mode"] != "allowlist" {
		t.Fatalf("a stored value was overwritten by the default: %v", values["policy.mode"])
	}
	if values["policy.denied"] != "rm" {
		t.Fatalf("a stored value did not come back: %v", values["policy.denied"])
	}
}

// A stored value that is deliberately empty is a value, and the default must
// not creep back over it.
func TestAnEmptyStoredValueIsNotReplaced(t *testing.T) {
	values := declaredValues(aForm, json.RawMessage(`{"policy":{"mode":""}}`))
	if values["policy.mode"] != "" {
		t.Fatalf("an emptied setting was refilled from the default: %v", values["policy.mode"])
	}
}

// The rule that was already there: only declared keys, so a setting removed
// from the form cannot go on steering anything from the row it is still in.
func TestOnlyWhatTheFormDeclaresComesBack(t *testing.T) {
	values := declaredValues(aForm, json.RawMessage(`{"policy":{"mode":"denylist"},"gone":"x"}`))
	if _, present := values["gone"]; present {
		t.Fatalf("an undeclared setting reached the form: %v", values)
	}
}

// And the round trip, which is what makes the form's own default land in the
// row: what is shown is sent back, and what is sent back is stored.
func TestWhatTheFormShowsIsWhatIsStored(t *testing.T) {
	shown := declaredValues(aForm, nil)
	sent := map[string]string{}
	for key, value := range shown {
		sent[key] = value.(string)
	}
	config, reason := declaredConfig(aForm, sent)
	if reason != "" {
		t.Fatalf("the form's own values were refused: %s", reason)
	}
	var stored struct {
		Policy struct {
			Mode string `json:"mode"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(config, &stored); err != nil {
		t.Fatalf("the stored settings could not be read back: %v", err)
	}
	if stored.Policy.Mode != "denylist" {
		t.Fatalf("the rule shown on the form is not the one stored: %q", stored.Policy.Mode)
	}
}
