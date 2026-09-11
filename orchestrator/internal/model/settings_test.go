package model

import "testing"

func spec() SettingSpec {
	return SettingSpec{
		Key: "reasoning_effort", Label: "Reasoning effort", Kind: SettingChoice,
		Choices: []SettingChoiceOption{{Value: "low"}, {Value: "medium"}, {Value: "high"}},
		Default: "medium",
	}
}

func TestTheNarrowerDecisionWins(t *testing.T) {
	s := spec()
	agent := Settings{"reasoning_effort": "high"}
	model := Settings{"reasoning_effort": "low"}
	vendor := Settings{"reasoning_effort": "medium"}

	if got := s.Value(agent, model, vendor); got != "high" {
		t.Errorf("agent should win: %q", got)
	}
	if got := s.Value(nil, model, vendor); got != "low" {
		t.Errorf("model should win when the agent says nothing: %q", got)
	}
	if got := s.Value(nil, nil, vendor); got != "medium" {
		t.Errorf("vendor should win when nothing narrower says: %q", got)
	}
	if got := s.Value(nil, nil, nil); got != "medium" {
		t.Errorf("the code's default should win when nobody says: %q", got)
	}
}

func TestAValueThatIsNoLongerAllowedIsNotSentToAVendor(t *testing.T) {
	// A choice that was legal once and is not any more. Sending it would have a
	// vendor refuse the whole request over one field, so the next source down
	// answers instead.
	s := spec()
	if got := s.Value(Settings{"reasoning_effort": "max"}, Settings{"reasoning_effort": "low"}); got != "low" {
		t.Errorf("a withdrawn choice was used: %q", got)
	}
	if got := s.Value(Settings{"reasoning_effort": "max"}); got != "medium" {
		t.Errorf("a withdrawn choice did not fall back to the default: %q", got)
	}
}

func TestAnUndeclaredKeyIsIgnored(t *testing.T) {
	// The rule that keeps a JSON bag from becoming a swamp: the code declares
	// what may be in it, so a leftover from a removed feature steers nothing.
	s := spec()
	if got := s.Value(Settings{"thinking_budget": "9000"}); got != "medium" {
		t.Errorf("an undeclared key changed the answer: %q", got)
	}
}

func TestAnUnreadableBagBehavesAsUnconfigured(t *testing.T) {
	// A corrupt row somewhere in the configuration must not be an outage: every
	// value has a declared default, so the worst case is the behaviour from
	// before anybody configured it.
	if got := ParseSettings([]byte(`{not json`)); got != nil {
		t.Errorf("unreadable settings were not treated as empty: %v", got)
	}
	if got := spec().Value(ParseSettings([]byte(`{not json`))); got != "medium" {
		t.Errorf("a corrupt bag did not fall back: %q", got)
	}
}

func TestUnsetAndSetToNothingCannotDriftApart(t *testing.T) {
	raw, err := Settings{"a": "", "b": "kept"}.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back := ParseSettings(raw)
	if _, ok := back["a"]; ok {
		t.Error("an empty value was stored rather than dropped")
	}
	if back["b"] != "kept" {
		t.Errorf("a real value was lost: %v", back)
	}
	if empty, _ := (Settings{"a": ""}).Marshal(); empty != nil {
		t.Errorf("a bag of nothing was stored as an object: %s", empty)
	}
}
