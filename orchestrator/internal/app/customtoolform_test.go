package app

import (
	"encoding/json"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/query"
	"flexie.io/sag/internal/tools/template"
)

// The edit form is built from a stored row and the template that made it, and
// touches nothing else, so what it hands back can be checked without a database
// anywhere near it.
func formFor(t *testing.T, config string) CustomToolEdit {
	t.Helper()
	registry := template.NewRegistry()
	registry.Add(query.New(nil))
	app := &App{Templates: registry}

	view, err := app.CustomToolForEdit(&model.Tool{
		Kind:     string(tool.KindCustom),
		Template: query.TemplateName,
		Config:   json.RawMessage(config),
	})
	if err != nil {
		t.Fatalf("edit form: %v", err)
	}
	return view
}

// The case that actually happens: the setting IS stored, and is empty. A choice
// has no empty option to pick, so nobody chose that; it was written by a form
// that had nothing to send. Handing it back would show a rule the tool does not
// have, and the save would then be refused for a field the form appeared to have
// filled in.
func TestAChoiceStoredEmptyComesBackAsItsDefault(t *testing.T) {
	view := formFor(t, `{"driver":"mysql","host":"db.internal","database":"shop","username":"app_ro","access":"read",`+
		`"policy":{"table_mode":"denylist","tables":"quotes","field_mode":"","fields":""}}`)

	if view.Settings["policy.field_mode"] != "denylist" {
		t.Fatalf("an empty rule came back as %q, so adding a field would be refused for a rule the form showed as made",
			view.Settings["policy.field_mode"])
	}
	// The rule that IS stored is untouched, and so are the lists.
	if view.Settings["policy.table_mode"] != "denylist" || view.Settings["policy.tables"] != "quotes" {
		t.Fatalf("a stored value was disturbed: %+v", view.Settings)
	}
}

// A tool made before the template had a setting has nothing stored against it.
// For a choice, that is not a value somebody cleared: it is a control that
// cannot hold what it was given. The console would show its first option while
// the form held an empty string, and a save would then be refused for a rule the
// form appeared to have made.
func TestAChoiceTheToolHasNoValueForComesBackAsItsDefault(t *testing.T) {
	view := formFor(t, `{"driver":"mysql","host":"db.internal","database":"shop","username":"app_ro","access":"read"}`)

	for _, key := range []string{"policy.table_mode", "policy.field_mode"} {
		if view.Settings[key] != "denylist" {
			t.Errorf("%s came back as %q, so the form would show one rule and hold another", key, view.Settings[key])
		}
	}
	// The lists stay empty: nothing is kept back from a tool that has never had
	// anything kept back from it.
	for _, key := range []string{"policy.tables", "policy.fields"} {
		if value, present := view.Settings[key]; present && value != "" {
			t.Errorf("%s came back as %q", key, value)
		}
	}
	// And what the tool does have is what comes back, untouched.
	if view.Settings["access"] != "read" || view.Settings["host"] != "db.internal" {
		t.Fatalf("a stored setting did not survive: %+v", view.Settings)
	}
}

// A stored choice is the tool's own, whatever the template would have started
// from. Handing back the default here would quietly change a tool by opening it.
func TestAStoredChoiceIsNotOverwrittenByItsDefault(t *testing.T) {
	view := formFor(t, `{"driver":"mysql","host":"db.internal","database":"shop","username":"app_ro",`+
		`"access":"both","policy":{"table_mode":"allowlist","tables":"orders","field_mode":"allowlist","fields":"orders.id"}}`)

	if view.Settings["policy.table_mode"] != "allowlist" || view.Settings["policy.field_mode"] != "allowlist" {
		t.Fatalf("a stored rule was replaced by the template's default: %+v", view.Settings)
	}
	if view.Settings["policy.tables"] != "orders" || view.Settings["policy.fields"] != "orders.id" {
		t.Fatalf("the lists did not come back as they were typed: %+v", view.Settings)
	}
	if view.Settings["access"] != "both" {
		t.Fatalf("access came back as %q", view.Settings["access"])
	}
}

// A list somebody emptied was emptied on purpose. Offering its default back
// would put what they took out straight back in, and saving would write it to
// the row. Only a choice is filled, because only a choice cannot hold nothing.
func TestAnEmptiedListIsLeftEmpty(t *testing.T) {
	view := formFor(t, `{"driver":"mysql","host":"db.internal","database":"shop","username":"app_ro",`+
		`"access":"read","policy":{"table_mode":"denylist","tables":"","field_mode":"denylist","fields":""}}`)

	for _, key := range []string{"policy.tables", "policy.fields"} {
		if view.Settings[key] != "" {
			t.Errorf("%s was refilled with %q", key, view.Settings[key])
		}
	}
}

// A secret is blanked for the form to send back unchanged, and must never be
// refilled from anywhere, default or otherwise.
func TestASecretIsStillBlankedNotDefaulted(t *testing.T) {
	view := formFor(t, `{"driver":"mysql","host":"db.internal","database":"shop","username":"app_ro","access":"read","password":"hunter2"}`)
	if view.Settings["password"] != "" {
		t.Fatalf("the secret was handed back to the form: %v", view.Settings["password"])
	}
}
