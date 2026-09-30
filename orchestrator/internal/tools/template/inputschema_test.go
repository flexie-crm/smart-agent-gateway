package template

import (
	"encoding/json"
	"testing"
)

// The one builder every template's parameters go through.
//
// There were three near-identical copies of this and they had already drifted:
// only one emitted `items` for an array. That is not decoration, which is why
// it is asserted here rather than left to whichever template happens to have an
// array parameter today.
func TestTheSchemaIsTheTemplatesShapeAndTheAdministratorsWords(t *testing.T) {
	params := []Param{
		{Key: "sql", Type: "string", Required: true, Description: "The statement."},
		{Key: "params", Type: "array", Required: false, Description: "Positional values."},
		{Key: "limit", Type: "number", Required: false, Description: "How many rows."},
	}
	raw := InputSchema(params, map[string]string{
		"sql": "Only SELECT, and only on the orders schema.",
		// Given but blank: not an instruction, so the template's own stands.
		"limit": "   ",
	})

	var got struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type        string          `json:"type"`
			Description string          `json:"description"`
			Items       json.RawMessage `json:"items"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the schema is not readable: %v", err)
	}

	if got.Type != "object" || len(got.Properties) != 3 {
		t.Fatalf("shape = %q with %d properties", got.Type, len(got.Properties))
	}
	// An ARRAY says what it holds. A schema that says "array" and nothing else
	// is refused outright by some providers, so the tool would be unusable with
	// them and fine with the rest, which is the worst way for this to be wrong.
	if len(got.Properties["params"].Items) == 0 {
		t.Fatal("an array parameter carries no items, which some providers refuse")
	}
	if len(got.Properties["sql"].Items) != 0 {
		t.Fatalf("a string parameter was given items: %s", got.Properties["sql"].Items)
	}
	// Required is the template's and nobody else's.
	if len(got.Required) != 1 || got.Required[0] != "sql" {
		t.Fatalf("required = %v", got.Required)
	}
	// The administrator's wording where they gave any.
	if got.Properties["sql"].Description != "Only SELECT, and only on the orders schema." {
		t.Fatalf("their description was not used: %q", got.Properties["sql"].Description)
	}
	// And the template's where they did not, including where they gave blanks.
	if got.Properties["limit"].Description != "How many rows." {
		t.Fatalf("a blank override replaced the template's own wording: %q",
			got.Properties["limit"].Description)
	}
	if got.Properties["params"].Description != "Positional values." {
		t.Fatalf("an undescribed parameter lost its wording: %q",
			got.Properties["params"].Description)
	}
}

// No required parameters means no `required` key at all, rather than an empty
// list, which some providers read as a schema error.
func TestASchemaWithNothingRequiredOmitsTheKey(t *testing.T) {
	raw := InputSchema([]Param{{Key: "note", Type: "string"}}, nil)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, present := got["required"]; present {
		t.Fatalf("an empty required list was emitted: %s", raw)
	}
}

// The checkbox an administrator ticked has to survive being stored.
//
// It did not, in both templates that offered it. The form names the field with
// a dot, storing splits dotted keys into nested objects, and the readers looked
// for a flat key with a dot in its name: always false, the box did nothing, and
// nothing reported it because a failure to reach an address is exactly the
// situation somebody ticking it already has.
func TestTheReachCheckboxIsReadWhereverItIsStored(t *testing.T) {
	for _, c := range []struct {
		name   string
		config string
		want   bool
	}{
		{"nested, which is what every template writes", `{"reach":{"chat":"true"}}`, true},
		{"nested and off", `{"reach":{"chat":"false"}}`, false},
		{"nested as a real boolean", `{"reach":{"chat":true}}`, true},
		{"flat with a dot, which a hand-written config would hold", `{"reach.chat":"on"}`, true},
		{"absent, which is every tool made before the box existed", `{"host":"db"}`, false},
		{"not a config at all", `nonsense`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ReachChat(json.RawMessage(c.config)); got != c.want {
				t.Fatalf("ReachChat(%s) = %v, want %v", c.config, got, c.want)
			}
		})
	}
}
