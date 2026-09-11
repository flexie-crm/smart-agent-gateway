package agent

import (
	"strings"
	"testing"

	"flexie.io/sag/internal/provider"
)

// Models wrap titles in quotes, prefix them, end them with a full stop, and
// occasionally explain themselves at length, no matter how plainly they were
// asked not to. Cleaning is not paranoia; it is the normal case.

func TestCleanTitle(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"plain", "Invoice rounding errors", "Invoice rounding errors"},
		{"quoted", `"Invoice rounding errors"`, "Invoice rounding errors"},
		{"smart quotes", `“Invoice rounding errors”`, "Invoice rounding errors"},
		{"prefixed", "Title: Invoice rounding errors", "Invoice rounding errors"},
		{"full stop", "Invoice rounding errors.", "Invoice rounding errors"},
		{"padded", "   Invoice rounding errors  ", "Invoice rounding errors"},
		{"quoted and prefixed", `Title: "Invoice rounding errors."`, "Invoice rounding errors"},
		{
			"the model explained itself",
			"Sales onboarding\n\nI chose this because the user asked about training staff.",
			"Sales onboarding",
		},
		{"empty", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanTitle(tc.raw); got != tc.want {
				t.Fatalf("cleanTitle(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// A model that ignored the word limit must not write an essay into a column a
// sidebar has to render.
func TestCleanTitleTruncates(t *testing.T) {
	long := strings.Repeat("word ", 100)
	got := cleanTitle(long)

	if len([]rune(got)) > maxTitleChars {
		t.Fatalf("a runaway title was not truncated: %d chars", len([]rune(got)))
	}
}

// Truncation must cut on a rune boundary, or a title in any language other
// than English comes back as broken bytes.
func TestCleanTitleKeepsNonLatinIntact(t *testing.T) {
	title := strings.Repeat("发票四舍五入", 30)
	got := cleanTitle(title)

	if !strings.HasPrefix(title, got) {
		t.Fatalf("truncation corrupted the text: %q", got)
	}
	if len([]rune(got)) > maxTitleChars {
		t.Fatalf("not truncated: %d runes", len([]rune(got)))
	}
}

// A conversation whose first message needed an approval is still named.
//
// The turn that parks has no answer to name it from, so naming falls to the turn
// that finishes the work. That turn carries no prompt of its own, so the
// question is read back from the conversation: without this a chat that opened
// with an approval-gated tool kept its default name for good.
func TestTheQuestionATitleIsAboutSurvivesAPark(t *testing.T) {
	transcript := []provider.Message{
		{Role: provider.RoleUser, Content: "take the spare model offline"},
		{Role: provider.RoleAssistant, Content: "I'll do that."},
		{Role: provider.RoleUser, Content: "and then tell me"},
	}

	// A turn with its own prompt names the conversation from that.
	if got := firstAsked("check the disks", transcript); got != "check the disks" {
		t.Fatalf("got %q, want the turn's own prompt", got)
	}
	// A resumed turn has none, so the first thing the person asked is used.
	if got := firstAsked("", transcript); got != "take the spare model offline" {
		t.Fatalf("got %q, want the first thing the person asked", got)
	}
	// Whitespace is not a prompt.
	if got := firstAsked("   ", transcript); got != "take the spare model offline" {
		t.Fatalf("got %q, want the first thing the person asked", got)
	}
	// Nothing to go on is not a title.
	if got := firstAsked("", nil); got != "" {
		t.Fatalf("got %q, want nothing", got)
	}
}
