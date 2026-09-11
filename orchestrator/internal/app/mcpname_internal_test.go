package app

import (
	"strings"
	"testing"

	"flexie.io/sag/internal/tool"
)

// The name a remote tool is offered to a model under.
//
// This is the defect that made three delegations in a row fail with nothing to
// show for them: the separator was a dot, no vendor accepts a dot, and because
// the tools go up as one array the whole request was refused. The agent did not
// lose its CRM tools, it lost all of them.

func TestARemoteToolIsNamedSomethingAModelAccepts(t *testing.T) {
	name, err := projectedToolName("flexie-crm-fx", "invoice")
	if err != nil {
		t.Fatalf("a perfectly ordinary tool was refused: %v", err)
	}
	if name != "flexie-crm-fx_invoice" {
		t.Errorf("got %q", name)
	}
	// The point of the whole exercise, stated where it cannot drift: whatever
	// this function returns has to be sendable.
	if err := tool.ValidName(name); err != nil {
		t.Errorf("%q is not usable: %v", name, err)
	}
}

// The remote names its own tools and owes us nothing. Refusing a good tool over
// a character in somebody else's naming convention would be our rule becoming
// their problem, so it is rewritten instead; the name we actually CALL is kept
// separately, in remote_name.
func TestANameTheRemoteChoseIsRewrittenRatherThanRefused(t *testing.T) {
	for _, remote := range []string{"search.docs", "read file", "crm/invoice", "a+b"} {
		name, err := projectedToolName("crm", remote)
		if err != nil {
			t.Errorf("%q was refused: %v", remote, err)
			continue
		}
		if err := tool.ValidName(name); err != nil {
			t.Errorf("%q became %q, which is still unusable: %v", remote, name, err)
		}
		if !strings.HasPrefix(name, "crm_") {
			t.Errorf("%q became %q, which is outside the connection's namespace", remote, name)
		}
	}
}

// Length is the one thing a rewrite cannot fix, so it is reported. The caller
// counts it as skipped and the console says so, which beats offering a tool
// that fails every request it appears in.
func TestANameTooLongToSendIsRefusedRatherThanTrimmed(t *testing.T) {
	_, err := projectedToolName("crm", strings.Repeat("x", tool.MaxNameLength))
	if err == nil {
		t.Fatal("a name well over the limit was accepted")
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Errorf("the reason should say what the limit is: %v", err)
	}
}

// Two different remote names can be rewritten into one. The registry holds one
// name per workspace, so treating them as the same tool would send one tool's
// calls to the other. They must be distinguishable here for the caller to be
// able to refuse the second.
func TestTwoRemoteNamesCanCollideAndItIsVisible(t *testing.T) {
	first, err1 := projectedToolName("crm", "a.b")
	second, err2 := projectedToolName("crm", "a-b")
	if err1 != nil || err2 != nil {
		t.Fatalf("both are legal to project: %v %v", err1, err2)
	}
	if first != "crm_a_b" {
		t.Errorf("got %q", first)
	}
	// Not equal here, which is why 'a.b' and 'a b' are the pair that matters.
	if second != "crm_a-b" {
		t.Errorf("got %q", second)
	}
	// The real collision, and the one the sync has to notice.
	dotted, _ := projectedToolName("crm", "a.b")
	spaced, _ := projectedToolName("crm", "a b")
	if dotted != spaced {
		t.Fatalf("this test no longer covers a collision: %q vs %q", dotted, spaced)
	}
}

// The name a person reads is the same name with our namespace taken back off,
// which is only safe if it is EXACTLY the inverse of what put it on: a prefix
// with a character the alphabet does not take was rewritten before it went on,
// and matching the raw prefix would leave half of it behind.
func TestTheNameAPersonReadsHasNoNamespaceOnIt(t *testing.T) {
	for _, c := range []struct{ prefix, remote, want string }{
		{"nli", "update_entity", "update_entity"},
		{"flexie-crm-fx", "invoice", "invoice"},
		// The prefix itself is rewritten on the way in, so it has to be
		// rewritten on the way out too.
		{"flexie crm", "invoice", "invoice"},
		// A remote name that happens to start with the prefix keeps every
		// character of its own: only the head we added comes off.
		{"nli", "nli_report", "nli_report"},
	} {
		name, err := projectedToolName(c.prefix, c.remote)
		if err != nil {
			t.Fatalf("%q/%q was refused: %v", c.prefix, c.remote, err)
		}
		if got := UnprefixedToolName(c.prefix, name); got != c.want {
			t.Errorf("%q under %q reads as %q, want %q", name, c.prefix, got, c.want)
		}
	}
}

// A tool of ours has no connection and therefore no prefix. Trimming an empty
// one would trim the separator off anything starting with an underscore, so
// the question is not asked at all.
func TestOurOwnToolKeepsItsWholeName(t *testing.T) {
	if got := UnprefixedToolName("", "_internal_thing"); got != "_internal_thing" {
		t.Errorf("got %q", got)
	}
}
