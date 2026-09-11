package tools

import (
	"context"
	"strings"
	"testing"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/brain"
	"flexie.io/sag/internal/tools/currenttime"
	"flexie.io/sag/internal/tools/httprequest"
	"flexie.io/sag/internal/tools/listmodels"
	"flexie.io/sag/internal/tools/setmodelstatus"
	"flexie.io/sag/internal/tools/toolguide"
)

// Every built-in tool says what it is FOR in language a person can act on.
//
// The model's own Description is written for the model: long, full of request
// bodies and byte caps, and naming internal machinery. Showing that to
// somebody deciding whether a team should have an ability is showing them a
// prompt. This is the same rule ConfirmDescription exists for, applied to the
// console, and it is a rule that rots silently unless something holds it.
func TestEveryBuiltinExplainsItselfToAPerson(t *testing.T) {
	// The words that give away model-facing copy: internal tool names, wire
	// formats, and the vocabulary of the stack rather than of the work.
	giveaways := []string{
		"tool_guide", "application/json", "http", "https", "json", "utf-8",
		"parameter", "argument", "endpoint", "payload", "api call", "kb", "byte",
	}

	// The schemas as they ship, taken from the constructors rather than from a
	// registry, so this needs no database to ask a question about wording.
	schemas := builtinSchemas()
	// Assert the list is real before asserting anything about it. A loop that
	// silently examines nothing is the one kind of test that is worse than none.
	if len(schemas) < 7 {
		t.Fatalf("only %d built-in schemas to check; the list has drifted from what ships", len(schemas))
	}
	checked := 0

	for _, schema := range schemas {
		// Every tool this build ships, whatever kind it is filed under. tool_guide
		// is internal rather than built-in, and it still appears in the console
		// with a description somebody has to read.
		checked++
		about := schema.About
		if about == "" {
			t.Errorf("%s has nothing to say to a person, so the console will show it the model's copy", schema.Name)
			continue
		}
		if about == schema.Description {
			t.Errorf("%s hands a person the same words it hands the model", schema.Name)
		}
		lower := strings.ToLower(about)
		for _, word := range giveaways {
			if strings.Contains(lower, word) {
				t.Errorf("%s explains itself to a person using %q, which is the model's vocabulary: %q",
					schema.Name, word, about)
			}
		}
		if len(about) > 400 {
			t.Errorf("%s takes %d characters to say what it is for; a person reading a permission needs a paragraph",
				schema.Name, len(about))
		}
	}
	if checked != len(schemas) {
		t.Fatalf("checked %d of %d built-ins", checked, len(schemas))
	}
}

// builtinSchemas is every built-in tool's schema. The ones that take a store
// are constructed with none: nothing here calls a handler, and a schema is
// decided at construction.
func builtinSchemas() []tool.Schema {
	return []tool.Schema{
		currenttime.New(nil, nil).Schema,
		listmodels.New(nil, nil).Schema,
		setmodelstatus.New(nil, nil).Schema,
		httprequest.New().Schema,
		toolguide.New(nil).Schema,
		brain.NewRead(nil).Schema,
		brain.NewWrite(nil).Schema,
	}
}

// Every tool that actually ships can be put in front of a model.
//
// The registry refuses a bad name now, so this is really asserting that the
// refusal is reachable and that nothing shipped is skating past it. It is the
// cheap half of a lesson that cost three failed delegations: one tool named
// something a vendor will not take breaks every OTHER tool in the same request.
func TestEveryBuiltInHasANameAModelAccepts(t *testing.T) {
	// The schemas as they ship, from the constructors, so this needs no
	// database to ask a question about names.
	schemas := builtinSchemas()
	if len(schemas) < 7 {
		t.Fatalf("only %d schemas to check; the list has drifted from what ships", len(schemas))
	}
	for _, schema := range schemas {
		if err := tool.ValidName(schema.Name); err != nil {
			t.Errorf("%q cannot be sent to a model: %v", schema.Name, err)
		}
	}
}

func TestTheRegistryRefusesANameAModelWouldReject(t *testing.T) {
	registry := tool.NewRegistry()
	err := registry.Register(tool.Tool{
		Schema: tool.Schema{Name: "crm.invoice"},
		Handle: func(context.Context, tool.Call) (tool.Result, error) { return tool.Result{}, nil },
	})
	if err == nil {
		t.Fatal("a dotted name was registered; it would fail every request it appeared in")
	}
	if !strings.Contains(err.Error(), "crm.invoice") {
		t.Errorf("the error should name the tool: %v", err)
	}
}
