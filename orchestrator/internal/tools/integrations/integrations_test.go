package integrations

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/tool"
)

// A projected tool, as the turn holds one: its service is recorded in the
// approval title, which is where the projection writes it.
func projected(name, service, description string) tool.Schema {
	return tool.Schema{
		Name:          name,
		Description:   description,
		Kind:          tool.KindMCP,
		ApprovalTitle: "Something, on " + service,
		InputSchema:   json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}
}

func ask(t *testing.T, held []tool.Schema, services []Service, args string) map[string]any {
	t.Helper()
	tl := New(func() []tool.Schema { return held }, func() []Service { return services })
	result, err := tl.Handle(context.Background(), tool.Call{Args: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("the ability failed: %v", err)
	}
	var answer map[string]any
	if err := json.Unmarshal(result.Content, &answer); err != nil {
		t.Fatalf("the answer is not readable: %v (%s)", err, result.Content)
	}
	return answer
}

// The point of the whole thing: a service is named, and what it can do is not,
// until somebody asks.
func TestServicesAreNamedAndTheirToolsAreNot(t *testing.T) {
	held := []tool.Schema{
		projected("crm_search", "CRM", "Search the CRM for people and companies. Long text about it."),
		projected("crm_note", "CRM", "Write a note."),
		projected("wiki_read", "Wiki", "Read a page."),
	}
	answer := ask(t, held, []Service{{Name: "CRM", Alias: "crm"}, {Name: "Wiki", Alias: "wiki"}},
		`{"operation":"services"}`)

	listed, _ := json.Marshal(answer["services"])
	for _, want := range []string{"CRM", "crm", "Wiki"} {
		if !strings.Contains(string(listed), want) {
			t.Fatalf("the services listing is missing %q: %s", want, listed)
		}
	}
	// Not a description, not a schema: the listing is the map.
	if strings.Contains(string(listed), "Search the CRM") {
		t.Fatalf("a tool's description was in the services listing: %s", listed)
	}
}

// One service's tools: names and one line each, so the model can choose without
// being handed three pages.
func TestOneServicesToolsComeBackAsOneLineEach(t *testing.T) {
	held := []tool.Schema{
		projected("crm_search", "CRM", "Search the CRM for people and companies. Then a second sentence that is not needed to choose."),
		projected("wiki_read", "Wiki", "Read a page."),
	}
	answer := ask(t, held, []Service{{Name: "CRM", Alias: "crm"}}, `{"operation":"tools","service":"CRM"}`)
	listed, _ := json.Marshal(answer["tools"])

	if !strings.Contains(string(listed), "crm_search") {
		t.Fatalf("the service's own tool is missing: %s", listed)
	}
	if strings.Contains(string(listed), "wiki_read") {
		t.Fatalf("another service's tool was listed: %s", listed)
	}
	if strings.Contains(string(listed), "second sentence") {
		t.Fatalf("the whole description came back where one line was asked for: %s", listed)
	}
	// And the alias finds it too, because that is the other thing a person sees.
	byAlias := ask(t, held, []Service{{Name: "CRM", Alias: "crm"}}, `{"operation":"tools","service":"crm"}`)
	if listed2, _ := json.Marshal(byAlias["tools"]); !strings.Contains(string(listed2), "crm_search") {
		t.Fatalf("the alias did not find the service: %s", listed2)
	}
}

// And the full thing, including the arguments, when one is actually going to be
// used. This is where the tokens are spent, and only here.
func TestDescribeGivesTheArgumentsInFull(t *testing.T) {
	held := []tool.Schema{projected("crm_search", "CRM", "Search the CRM. Every detail of it.")}
	answer := ask(t, held, nil, `{"operation":"describe","tool":"crm_search"}`)

	if answer["does"] != "Search the CRM. Every detail of it." {
		t.Fatalf("describe did not give the whole description: %v", answer["does"])
	}
	arguments, _ := json.Marshal(answer["arguments"])
	if !strings.Contains(string(arguments), "\"q\"") {
		t.Fatalf("describe did not give the arguments: %s", arguments)
	}
}

// A tool this person cannot reach does not exist to them, and the refusal says
// how to find what does rather than leaving the model to guess again.
func TestWhatIsNotHeldIsNotFound(t *testing.T) {
	tl := New(func() []tool.Schema { return nil }, func() []Service { return nil })
	result, err := tl.Handle(context.Background(), tool.Call{
		Args: json.RawMessage(`{"operation":"describe","tool":"crm_search"}`),
	})
	if err != nil {
		t.Fatalf("the ability failed: %v", err)
	}
	if !result.Failed() {
		t.Fatal("a tool nobody holds was described")
	}
	if !strings.Contains(string(result.Content), "operation") {
		t.Fatalf("the refusal does not say what to do instead: %s", result.Content)
	}
}

// Running one is NOT this handler's job: the loop resolves it into a call to
// the tool itself, so the approval card, the drift check and the transcript row
// are the real tool's. Reaching here means that did not happen.
func TestRunningIsNotDoneHere(t *testing.T) {
	tl := New(func() []tool.Schema { return nil }, func() []Service { return nil })
	result, _ := tl.Handle(context.Background(), tool.Call{
		Args: json.RawMessage(`{"operation":"call","tool":"crm_search","arguments":{}}`),
	})
	if !result.Failed() {
		t.Fatal("this ability ran a tool by itself, which would bypass approval and the drift check")
	}
}
