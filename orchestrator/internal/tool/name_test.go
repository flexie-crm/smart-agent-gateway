package tool

import (
	"strings"
	"testing"
)

// What a tool may be called, which is decided by the model APIs and not by us.
//
// The stakes are not "this tool is rejected". The tools are sent as one array,
// so one name a vendor refuses fails the whole request and the agent loses
// EVERY tool it has, on every turn.

func TestANameAModelAcceptsIsAccepted(t *testing.T) {
	for _, name := range []string{
		"http_request",
		"tool_guide",
		"ssh_production",
		"flexie-crm-fx_invoice", // a projected MCP tool, as they are now
		"a",
		strings.Repeat("x", MaxNameLength),
	} {
		if err := ValidName(name); err != nil {
			t.Errorf("%q was refused: %v", name, err)
		}
	}
}

func TestANameAModelWouldRefuseIsRefused(t *testing.T) {
	cases := map[string]string{
		"a dot, which is how this was found": "flexie-crm-fx.invoice",
		"a space":                            "read file",
		"one character over the limit":       strings.Repeat("x", MaxNameLength+1),
		"nothing at all":                     "",
		"punctuation":                        "invoice!",
		"a slash":                            "crm/invoice",
		"not ascii":                          "fatturä",
	}
	for what, name := range cases {
		if err := ValidName(name); err == nil {
			t.Errorf("%s: %q was accepted", what, name)
		}
	}
}

// The limit is in characters, not bytes: a name counted in bytes would refuse
// a legal name and, worse, could accept one the vendor then rejects.
func TestTheLimitCountsCharacters(t *testing.T) {
	if err := ValidName(strings.Repeat("é", MaxNameLength)); err == nil {
		t.Error("an accented name is not in the alphabet and must be refused")
	}
	// The message has to name the real number, because an administrator reads
	// it and counts.
	err := ValidName(strings.Repeat("x", 100))
	if err == nil || !strings.Contains(err.Error(), "100") {
		t.Errorf("the reason should say how long it actually is: %v", err)
	}
}

func TestANameFromSomewhereElseIsRewrittenRatherThanRefused(t *testing.T) {
	cases := map[string]string{
		"search.docs":  "search_docs",
		"read file":    "read_file",
		"crm/invoice":  "crm_invoice",
		"already_fine": "already_fine",
		"with-hyphen":  "with-hyphen",
	}
	for given, want := range cases {
		if got := UsableName(given); got != want {
			t.Errorf("UsableName(%q) = %q, want %q", given, got, want)
		}
		if err := ValidName(UsableName(given)); err != nil {
			t.Errorf("UsableName(%q) still is not usable: %v", given, err)
		}
	}
}

// Rewriting cannot fix length, and must not pretend to: a name shortened by
// guesswork could land on another tool's name, and then one tool's calls go
// somewhere else entirely.
func TestRewritingDoesNotShortenAName(t *testing.T) {
	long := strings.Repeat("a.", 40)
	if err := ValidName(UsableName(long)); err == nil {
		t.Error("a name too long was silently made to fit")
	}
}
