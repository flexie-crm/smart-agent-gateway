package model

import "encoding/json"

// Settings is a bag of chosen values for keys the CODE declares.
//
// Vendors keep inventing attributes that are neither standard nor optional: a
// reasoning effort here, a thinking budget there, and more every quarter that
// will not agree with each other. A column each would be a migration per vendor
// whim and a schema recording who shipped what and when.
//
// So the row carries one JSON object and the code says what may be in it. That
// is the half that matters, and it is what separates this from a bag where
// anything goes and nothing is knowable: a Spec declares the key, what it may be
// set to, and what it is when nobody says. A key nothing declares is IGNORED
// rather than obeyed, so a value left behind by a feature that was removed
// cannot quietly steer anything, there is always a form to render, and there is
// always an answer to "what does this mean".
//
// It is the same shape `tools` has had since it was built (a Go Template beside
// a config JSON) and the same shape the inference node uses for the settings it
// declares. This applies it consistently rather than inventing anything.
type Settings map[string]string

// SettingKind is how a value is chosen, which is also how it is rendered.
type SettingKind string

const (
	// SettingChoice is one of a fixed list. Rendered as a select.
	SettingChoice SettingKind = "choice"
	// SettingText is free text. Rendered as a field.
	SettingText SettingKind = "text"
	// SettingNumber is a whole number. Rendered as a number field.
	SettingNumber SettingKind = "number"
)

// SettingSpec declares one setting: what it is called, what it may be, and what
// it is when nobody has said.
type SettingSpec struct {
	Key   string      `json:"key"`
	Label string      `json:"label"`
	Kind  SettingKind `json:"kind"`
	// Help is the sentence under the field. It says what the setting DOES, not
	// what it is called again.
	Help string `json:"help,omitempty"`
	// Choices is the list for SettingChoice, in the order to show them.
	Choices []SettingChoiceOption `json:"choices,omitempty"`
	// Default is what the code uses when the bag says nothing. It lives HERE and
	// not in the database on purpose: a model added before we knew better picks
	// up a better default on upgrade, and only an explicit choice overrides it.
	Default string `json:"default,omitempty"`
	// RequiresReasoning marks a setting that only means anything while the model
	// is thinking. It is not offered for a model that cannot, because a choice
	// that can never be honoured is worse than no choice: somebody picks "high",
	// sees it saved, and believes it.
	//
	// The flag lives on the SPEC rather than being a key the client recognises,
	// so a second setting of this kind needs no client change and the console
	// never has to know what "reasoning_effort" means.
	RequiresReasoning bool `json:"requires_reasoning,omitempty"`
}

// SettingChoiceOption is one option, with the words a person reads.
type SettingChoiceOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Value returns what this setting resolves to, most specific source first,
// falling back to the spec's default.
//
// The order is the layered-configuration order the rest of the product already
// uses (KB/15): the narrower decision wins. Pass the bags nearest-first.
func (s SettingSpec) Value(bags ...Settings) string {
	for _, bag := range bags {
		if v, ok := bag[s.Key]; ok && v != "" {
			if s.allows(v) {
				return v
			}
			// Declared key, undeclared value: a choice that was legal once and
			// is not any more. Fall through rather than send it to a vendor
			// that will refuse the whole request over it.
			continue
		}
	}
	return s.Default
}

// allows says whether a stored value is still one this setting may take.
func (s SettingSpec) allows(v string) bool {
	if s.Kind != SettingChoice {
		return true
	}
	for _, c := range s.Choices {
		if c.Value == v {
			return true
		}
	}
	return false
}

// ParseSettings reads a bag from a column, treating anything unreadable as
// empty rather than as an error.
//
// A bag that cannot be parsed must not stop a turn: every value in it has a
// declared default, so the worst case is a model that behaves the way it did
// before anybody configured it. Failing the request instead would make a
// corrupt row somewhere in the configuration into an outage.
func ParseSettings(raw []byte) Settings {
	if len(raw) == 0 {
		return nil
	}
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	return s
}

// Marshal writes a bag back, dropping empty values so "unset" and "set to
// nothing" cannot drift apart.
func (s Settings) Marshal() ([]byte, error) {
	kept := Settings{}
	for k, v := range s {
		if v != "" {
			kept[k] = v
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	return json.Marshal(kept)
}
