package apitool

import (
	"encoding/json"
	"strings"
	"testing"
)

// A denylist runs anything except what is listed, which is what a new tool
// starts as: an assistant that can read an API without being able to change it.
func TestADenylistRefusesOnlyWhatIsListed(t *testing.T) {
	p := Policy{Mode: Denylist, Verbs: WriteVerbs()}
	if err := p.check(); err != nil {
		t.Fatalf("check: %v", err)
	}
	for _, verb := range []string{"GET", "HEAD", "OPTIONS"} {
		if ok, why := p.Permits(verb); !ok {
			t.Errorf("%s was refused by a denylist of writes: %s", verb, why)
		}
	}
	for _, verb := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		ok, why := p.Permits(verb)
		if ok {
			t.Errorf("%s was allowed by a denylist that lists it", verb)
		}
		if !strings.Contains(why, verb) {
			t.Errorf("the refusal for %s does not name it: %s", verb, why)
		}
	}
}

// An allowlist refuses everything it does not list, and says what it does, so
// the model can choose another call rather than repeat this one.
func TestAnAllowlistRefusesEverythingElseAndSaysWhatItAllows(t *testing.T) {
	p := Policy{Mode: Allowlist, Verbs: []string{"get"}}
	if err := p.check(); err != nil {
		t.Fatalf("check: %v", err)
	}
	// Normalised on the way in, so a form that said "get" still matches.
	if ok, why := p.Permits("GET"); !ok {
		t.Fatalf("GET was refused by an allowlist containing it: %s", why)
	}
	ok, why := p.Permits("POST")
	if ok {
		t.Fatal("POST was allowed by an allowlist that does not list it")
	}
	if !strings.Contains(why, "GET") {
		t.Fatalf("the refusal does not say what IS allowed: %s", why)
	}
}

// An allowlist with nothing in it permits nothing, so it is refused at save
// time rather than saved and puzzled over.
//
// This is the mistake the SSH tool shipped with (KB/33): an unconfigured tool
// read as an empty allowlist, and an empty allowlist ran nothing at all, while
// the form said the rule was a denylist.
func TestAnEmptyAllowlistIsRefusedRatherThanSaved(t *testing.T) {
	p := Policy{Mode: Allowlist}
	err := p.check()
	if err == nil {
		t.Fatal("an allowlist with no verbs was accepted, so the tool could never run")
	}
	// An administrator reads this on the form, so it has to say both what the
	// setting would DO and what to do instead. Asserted on the substance
	// rather than on one phrase, so rewording it cannot silently drop either.
	for _, want := range []string{"never make a call", "denylist"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not mention %q, so it does not say what happens or what to do: %v", want, err)
		}
	}
	// And the control: a denylist with nothing in it is the OPPOSITE, a tool
	// that may use every verb, which is legitimate and must be allowed.
	open := Policy{Mode: Denylist}
	if err := open.check(); err != nil {
		t.Fatalf("an empty denylist should permit everything, got %v", err)
	}
	if ok, why := open.Permits("DELETE"); !ok {
		t.Fatalf("an empty denylist refused DELETE: %s", why)
	}
}

// Something that is not a verb at all is refused, not passed to net/http.
func TestSomethingThatIsNotAVerbIsRefused(t *testing.T) {
	p := Policy{Mode: Denylist}
	_ = p.check()
	if ok, _ := p.Permits("CONNECT"); ok {
		t.Error("CONNECT was allowed, which this tool does not send")
	}
	if ok, _ := p.Permits("GET /x HTTP/1.1"); ok {
		t.Error("a whole request line was accepted as a verb")
	}
	// And a verb the form invented is refused at save time.
	bad := Policy{Mode: Denylist, Verbs: []string{"TRACE"}}
	if err := bad.check(); err == nil {
		t.Error("TRACE was accepted into the policy")
	}
}

// The stored shape round-trips, because the policy is read back from JSON on
// every load and a field that does not survive is a rule that stops applying.
func TestThePolicySurvivesBeingStored(t *testing.T) {
	raw := json.RawMessage(`{
		"base_url": "https://api.example.com/v1",
		"auth": {"kind": "bearer", "token": "t"},
		"policy": {"mode": "allowlist", "verbs": ["get", "POST", "get"]}
	}`)
	s, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Policy.Mode != Allowlist {
		t.Fatalf("mode = %q", s.Policy.Mode)
	}
	// Normalised, de-duplicated and sorted, so two forms of the same rule are
	// one rule.
	if len(s.Policy.Verbs) != 2 || s.Policy.Verbs[0] != "GET" || s.Policy.Verbs[1] != "POST" {
		t.Fatalf("verbs = %v, want [GET POST]", s.Policy.Verbs)
	}
}
