package tool

import (
	"encoding/json"
	"fmt"
	"strings"
)

// A declared form.
//
// Some tools need settings, and the console has to render them without knowing
// what any of them mean: a database driver's host and port, a terminal's list
// of what may run. So a tool DECLARES its form, and the console draws whatever
// it is handed.
//
// It lives beside the schema rather than with the templates, because it is not
// a template's idea. A template needs a form because each instance is a
// different remote service to configure; a native tool can need one for its own
// reasons and in the same shape, and a second vocabulary for the same thing
// would mean a second renderer and two ways to be wrong.

// FieldType tells the admin form how to render a settings input.
type FieldType string

const (
	FieldText     FieldType = "text"
	FieldNumber   FieldType = "number"
	FieldPassword FieldType = "password"
	FieldSelect   FieldType = "select"
	FieldTextarea FieldType = "textarea"
	// FieldCheckbox is a setting that is on or off. Its value travels as a
	// string like every other declared field ("true" / ""), because the form is
	// one shape for every type and a second one would be a second renderer.
	FieldCheckbox FieldType = "checkbox"
)

// Option is one choice in a select: the value that is stored, and the words a
// person reads.
//
// They were the same string, so every select in the console showed the value:
// "disable", "denylist", "both". Those are names for the code's benefit, and a
// person reading one has to work out what it means to them, which is the
// opposite of what a form is for.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Choice pairs a stored value with what it says to a person.
func Choice(value, label string) Option { return Option{Value: value, Label: label} }

// Field is one settings input a template needs. Secret fields are sealed at rest
// and write-only in the form.
type Field struct {
	Key      string    `json:"key"`
	Label    string    `json:"label"`
	Type     FieldType `json:"type"`
	Required bool      `json:"required"`
	Secret   bool      `json:"secret,omitempty"`
	Options  []Option  `json:"options,omitempty"`
	Default  string    `json:"default,omitempty"`
	Help     string    `json:"help,omitempty"`
	// Span is the field's width in a six-column row, so the template decides the
	// layout. Zero means a sensible default; a textarea always fills the row.
	Span int `json:"span,omitempty"`
}

// Section groups related settings under a heading (the connection, the SSH
// tunnel, what a terminal may run). The tool decides the grouping and the
// order, so the console renders whatever sections it is given without knowing
// what any of them are.
type Section struct {
	Title  string  `json:"title"`
	Hint   string  `json:"hint,omitempty"`
	Fields []Field `json:"fields"`
}

// Settings resolves what a tool's settings ACTUALLY are: what an administrator
// stored, and the tool's own declared default for everything they did not.
//
// One resolver, because there were two and they disagreed. The terminal's form
// declares its rule ships as a denylist, which is what an administrator reads
// when they open it; the runtime read a policy struct with an empty mode and
// defaulted it to an allowlist, which permits nothing. Both were defensible on
// their own and together they meant a tool that said it would run everything
// and ran nothing, with the refusal blaming an administrator who had not been
// given a way to fix it.
//
// So the declaration is the single truth for what a tool ships as, and the form
// and the runtime are both readers of it. Merged per declared key rather than
// used as a whole-object fallback: settings saved before a field existed, or
// written without one, still get that field's default rather than falling back
// to the zero value of whatever type parses them.
//
// Only declared keys survive, for the reason the settings bag gives: a value
// left behind by a setting that was removed must not go on steering anything.
// Unreadable settings are an ERROR and not an absence, which is a distinction
// the two readers need to answer differently. A form shows the defaults, which
// is the best it can offer somebody who has come to fix it. A gate must refuse,
// because "we could not read what you are allowed to do" is not permission: the
// terminal's declared default is a denylist, so swallowing the error would have
// turned a corrupt row into a terminal that runs anything.
func Settings(sections []Section, stored json.RawMessage) (map[string]any, error) {
	var raw map[string]any
	var failed error
	if len(stored) > 0 {
		if err := json.Unmarshal(stored, &raw); err != nil {
			raw, failed = nil, fmt.Errorf("these settings could not be read: %w", err)
		}
	}
	values := make(map[string]any, 8)
	for _, section := range sections {
		for _, field := range section.Fields {
			if value, ok := Nested(raw, field.Key); ok {
				values[field.Key] = value
				continue
			}
			if field.Default != "" {
				values[field.Key] = field.Default
			}
		}
	}
	return values, failed
}

// SettingsConfig is the same answer in the shape settings are STORED in, which
// is what anything parsing them into its own type reads.
func SettingsConfig(sections []Section, stored json.RawMessage) (json.RawMessage, error) {
	values, failed := Settings(sections, stored)
	if failed != nil {
		return nil, failed
	}
	raw, err := json.Marshal(Nest(values))
	if err != nil {
		return nil, fmt.Errorf("these settings could not be stored: %w", err)
	}
	return raw, nil
}

// Nested reads a dotted key (policy.mode) out of the object it was stored in.
// Settings are declared flat and stored nested.
func Nested(raw map[string]any, key string) (any, bool) {
	var current any = raw
	for _, part := range strings.Split(key, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// Nest expands dotted keys (policy.mode) into the object they are stored as.
func Nest(flat map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range flat {
		parts := strings.Split(key, ".")
		object := out
		for i, part := range parts {
			if i == len(parts)-1 {
				object[part] = value
				break
			}
			next, ok := object[part].(map[string]any)
			if !ok {
				next = map[string]any{}
				object[part] = next
			}
			object = next
		}
	}
	return out
}
