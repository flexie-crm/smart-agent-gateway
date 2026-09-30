package agentguide

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/tool"
)

func ask(t *testing.T, roster []Agent, args map[string]any) tool.Result {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := New(func() []Agent { return roster }).Handle(context.Background(), tool.Call{Args: raw})
	if err != nil {
		t.Fatalf("system error: %v", err)
	}
	return res
}

// payload is the result exactly as the model receives it, so an assertion is
// about what was actually sent rather than about the shape of a Go value.
func payload(_ *testing.T, res tool.Result) string { return string(res.Content) }

var researcher = Agent{
	Key: "researcher", Name: "Researcher",
	Instructions: "Finds source material.\nAlways cite the source.",
	Abilities: []Ability{
		{Key: "http_request", Name: "API request", Description: "Fetch a URL.", Group: "Built-in"},
		{Key: "nli_query", Name: "Flexie Search", Description: "Search records.", Group: "NLI"},
		{Key: "nli_note", Name: "Note", Description: "Write a note.", Group: "NLI"},
		{Key: "local_sqlserver", Name: "Local SQLServer", Description: "Query it.", Group: "Custom"},
	},
	Knowledge: []Knowledge{
		{Name: "Support Playbook", ReadOnly: true},
		{Name: "Integration Recipes"},
	},
	Memory: "Researcher Memory",
}

// The whole point of the tool: one call answers everything about one agent, so
// the Gateway can decide where a task goes without carrying any of this in
// every prompt.
func TestOneCallAnswersEverythingAboutAnAgent(t *testing.T) {
	got := payload(t, ask(t, []Agent{researcher}, map[string]any{"agent": "researcher"}))

	for _, want := range []string{
		// what it is for, whole
		"Always cite the source.",
		// every ability, by BOTH the name a capability reads as and the key
		// tool_guide takes
		"API request", "http_request",
		"Flexie Search", "nli_query",
		"Local SQLServer", "local_sqlserver",
		// its knowledge and its memory
		"Support Playbook", "Integration Recipes", "Researcher Memory",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the answer does not carry %q:\n%s", want, got)
		}
	}
}

// An ability is grouped under where it came from, and a connected service's
// tools are grouped under that service: two services both offering a "search"
// is the case a flat list cannot describe.
func TestAbilitiesAreGroupedByWhereTheyCameFrom(t *testing.T) {
	res := ask(t, []Agent{researcher}, map[string]any{"agent": "researcher"})
	var body struct {
		Abilities []struct {
			From  string `json:"from"`
			Tools []struct {
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"abilities"`
	}
	if err := json.Unmarshal(res.Content, &body); err != nil {
		t.Fatalf("the answer could not be read: %v\n%s", err, res.Content)
	}
	found := map[string]int{}
	for _, group := range body.Abilities {
		found[group.From] = len(group.Tools)
	}
	want := map[string]int{"Built-in": 1, "Custom": 1, "NLI": 2}
	for group, count := range want {
		if found[group] != count {
			t.Errorf("group %q holds %d tools, want %d (all groups: %v)", group, found[group], count, found)
		}
	}
	if len(found) != len(want) {
		t.Errorf("unexpected groups: %v", found)
	}
}

// The answer that hands over callable keys says, in the same answer, that they
// are not the Gateway's to call. Without it the keys read as an offer, which is
// the hallucinated call the roster used to avoid by hiding them.
func TestTheAnswerSaysTheAbilitiesAreNotTheCallersToCall(t *testing.T) {
	got := payload(t, ask(t, []Agent{researcher}, map[string]any{"agent": "researcher"}))
	for _, want := range []string{"not yours", "cannot call", "Delegate the task to researcher"} {
		if !strings.Contains(got, want) {
			t.Errorf("the answer is missing %q:\n%s", want, got)
		}
	}
}

// An agent with no tools is said to have none, plainly. Silence there reads as
// an agent whose abilities were simply not listed.
func TestAnAgentWithNoToolsSaysSo(t *testing.T) {
	got := payload(t, ask(t, []Agent{{Key: "writer", Name: "Writer", Instructions: "Writes."}},
		map[string]any{"agent": "writer"}))
	if !strings.Contains(got, "holds no tools") {
		t.Fatalf("an agent with no tools did not say so:\n%s", got)
	}
}

// A key that is not an agent is ANSWERED with the keys that are, so the model
// can correct itself in one step instead of guessing again.
//
// Answered rather than refused: this is an internal lookup and finding nothing
// is the question answered. It used to fail, which drew a broken tool in the
// chat over a typo the model fixed on its next step.
func TestAnUnknownKeyIsAnsweredWithTheRealOnes(t *testing.T) {
	res := ask(t, []Agent{researcher, {Key: "writer"}}, map[string]any{"agent": "reseacher"})
	if res.Err != tool.ErrorNone {
		t.Fatalf("an internal lookup that found nothing should not fail: %s", res.Content)
	}
	got := payload(t, res)
	if !strings.Contains(got, `"found":false`) {
		t.Fatalf("the answer did not say it found nothing:\n%s", got)
	}
	for _, want := range []string{"researcher", "writer"} {
		if !strings.Contains(got, want) {
			t.Errorf("the answer does not name %q:\n%s", want, got)
		}
	}
}

// A key the model spelled with the wrong case is the agent it meant, not a
// miss: the keys are ours and case carries no meaning in them.
func TestTheKeyIsMatchedWithoutCase(t *testing.T) {
	res := ask(t, []Agent{researcher}, map[string]any{"agent": "  Researcher  "})
	if res.Err != tool.ErrorNone {
		t.Fatalf("a key differing only in case and spacing was refused: %s", payload(t, res))
	}
}

// Called with no key it lists the agents rather than refusing, which is the
// answer to a model that has lost the roster.
func TestNoKeyListsTheAgents(t *testing.T) {
	got := payload(t, ask(t, []Agent{researcher, {Key: "writer", Name: "Writer"}}, map[string]any{}))
	for _, want := range []string{"researcher", "Researcher", "writer", "Writer"} {
		if !strings.Contains(got, want) {
			t.Errorf("the listing is missing %q:\n%s", want, got)
		}
	}
	// And the listing is only names: the point of it is that it costs nothing.
	if strings.Contains(got, "Always cite the source.") {
		t.Errorf("the listing spelled out an agent in full:\n%s", got)
	}
}

// The roster is read at CALL time, never captured when the tool is built: what
// a person may reach is decided per turn, and a stale answer here is an agent
// described with abilities it has since lost.
func TestTheRosterIsReadWhenTheToolIsCalled(t *testing.T) {
	roster := []Agent{{Key: "writer", Name: "Writer"}}
	built := New(func() []Agent { return roster })
	roster = append(roster, researcher)

	raw, _ := json.Marshal(map[string]any{"agent": "researcher"})
	res, err := built.Handle(context.Background(), tool.Call{Args: raw})
	if err != nil {
		t.Fatalf("system error: %v", err)
	}
	if res.Err != tool.ErrorNone {
		t.Fatalf("an agent added after the tool was built could not be found: %s", payload(t, res))
	}
}

// It is infrastructure, like tool_guide: it loads with any tool set, has
// nothing for an administrator to grant, and reads only.
func TestItIsInfrastructureAndReadsOnly(t *testing.T) {
	s := Schema()
	if s.Kind != tool.KindInternal {
		t.Errorf("kind is %q, want %q, or an administrator has to grant it", s.Kind, tool.KindInternal)
	}
	if s.Risk != tool.RiskReadOnly {
		t.Errorf("risk is %q, want read-only", s.Risk)
	}
	if s.RequiresApproval {
		t.Error("looking an agent up should not ask anybody for permission")
	}
	// It is appended to a loadout rather than registered, so it never passes
	// the registry's door check. Its name still goes up to a vendor with every
	// other tool, and a name a vendor refuses fails the WHOLE request, not just
	// this tool.
	if err := tool.ValidName(s.Name); err != nil {
		t.Errorf("the tool's own name would be refused by a vendor: %v", err)
	}
}
