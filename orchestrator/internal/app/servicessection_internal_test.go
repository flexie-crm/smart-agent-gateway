package app

import (
	"strings"
	"testing"
)

// The prompt tells the assistant which guide is which.
//
// There are two guide mechanisms in play once a service is connected: ours,
// which documents the abilities we ship, and whatever the service offers for
// its own tools. Nothing in the tool list distinguishes them, and the model
// reached for the one it knew: it asked tool_guide about a CRM's query tool,
// was told there was no deeper documentation (which we cannot know, and which
// was false, because that service ships its own guide), and then invented a
// topic id. So the division is stated where the services are named.
func TestThePromptSaysOurGuideDoesNotCoverAServicesTools(t *testing.T) {
	got := servicesSection([]connectedService{{id: 1, name: "NLI", alias: "nli"}})

	if !strings.Contains(got, "tool_guide") {
		t.Fatalf("the services section never mentions our own guide:\n%s", got)
	}
	// Named, and said not to cover them. Mentioning it without the division
	// would be worse than silence.
	if !strings.Contains(got, "does NOT document these") {
		t.Fatalf("the division between our guide and the service's is not stated:\n%s", got)
	}
	// And the other half: our prefix is ours, so a name passed TO the service
	// must be the service's own.
	if !strings.Contains(got, "without the prefix") {
		t.Fatalf("the prompt does not say which name to give the service:\n%s", got)
	}
	// It still says where the depth actually lives.
	if !strings.Contains(got, "integrations") {
		t.Fatalf("the prompt does not route to the service's own tools:\n%s", got)
	}
}

// And none of it is said when there is nothing to confuse ours with.
//
// The control for the test above, and the rule this file follows throughout: a
// paragraph about integrations costs every turn of every conversation that has
// none. A workspace with no service connected gets no section at all.
func TestNothingIsSaidAboutServicesWhenNoneAreConnected(t *testing.T) {
	if got := servicesSection(nil); got != "" {
		t.Fatalf("a workspace with no connected service was told about them:\n%s", got)
	}
}
