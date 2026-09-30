package integrations

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/tool"
)

// A projected tool, as the turn holds one.
//
// Service and RemoteName are the schema's own now. They used to be recovered
// from the approval title, and this fixture spelled that out ("Something, on
// CRM"), which is how it went on passing after the title stopped being read:
// the service reaches the listing through serviceOf, and nothing here asserted
// what it put there. The title stays because the projection still writes one
// for the card, and leaving it proves the listing no longer depends on it.
func projected(name, service, description string) tool.Schema {
	_, remote, _ := strings.Cut(name, "_")
	return tool.Schema{
		Name:          name,
		Description:   description,
		Kind:          tool.KindMCP,
		Service:       service,
		RemoteName:    remote,
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

// A tool this person cannot reach does not exist to them, and the answer says
// how to find what does rather than leaving the model to guess again.
//
// The property is that it is not DESCRIBED, asserted directly. It used to be
// asserted through "the call failed", which would have passed just as well if
// the refusal had carried the tool's arguments in it, and which stopped being
// true when a miss became an answer: nothing broke, and nothing was described.
func TestWhatIsNotHeldIsNotFound(t *testing.T) {
	tl := New(func() []tool.Schema { return nil }, func() []Service { return nil })
	result, err := tl.Handle(context.Background(), tool.Call{
		Args: json.RawMessage(`{"operation":"describe","tool":"crm_search"}`),
	})
	if err != nil {
		t.Fatalf("the ability failed: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(result.Content, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Nothing about the tool itself came back.
	for _, leaked := range []string{"arguments", "does", "service"} {
		if _, ok := payload[leaked]; ok {
			t.Fatalf("a tool nobody holds was described (%q came back): %s", leaked, result.Content)
		}
	}
	if found, ok := payload["found"].(bool); !ok || found {
		t.Fatalf("the answer did not say it found nothing: %s", result.Content)
	}
	if !strings.Contains(string(result.Content), "operation") {
		t.Fatalf("the answer does not say what to do instead: %s", result.Content)
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

// The service reaches the listing, and is counted.
//
// serviceOf is what puts it there, and for a long time nothing asserted the
// result: the filtering is by name prefix, so every existing test passed with
// serviceOf answering an empty string. That is what this closes.
func TestEachToolIsAttributedToItsServiceAndCounted(t *testing.T) {
	held := []tool.Schema{
		projected("crm_search", "CRM", "Search records."),
		projected("crm_query", "CRM", "Read rows."),
		projected("wiki_read", "Wiki", "Read a page."),
	}
	services := []Service{{Name: "CRM", Alias: "crm"}, {Name: "Wiki", Alias: "wiki"}}

	// Counted per service in the services listing.
	answer := ask(t, held, services, `{"operation":"services"}`)
	listed, _ := json.Marshal(answer["services"])
	for _, want := range []string{`"service":"CRM","tools":2`, `"service":"Wiki","tools":1`} {
		if !strings.Contains(string(listed), want) {
			t.Fatalf("the services listing does not carry %s: %s", want, listed)
		}
	}

	// And named on each tool of one service.
	answer = ask(t, held, services, `{"operation":"tools","service":"CRM"}`)
	tools, _ := json.Marshal(answer["tools"])
	if strings.Contains(string(tools), `"service":""`) {
		t.Fatalf("a tool was listed with no service: %s", tools)
	}
	if !strings.Contains(string(tools), `"service":"CRM"`) {
		t.Fatalf("the tools listing does not attribute to CRM: %s", tools)
	}
	// The exact callable name, prefix and all, is what a model must call.
	if !strings.Contains(string(tools), `"tool":"crm_search"`) {
		t.Fatalf("the listing does not give the exact callable name: %s", tools)
	}
}

// Discovery is the ONLY way in to a service's tools, so a miss must not read as
// a fault.
//
// This is the real conversation. A service projects 29 tools, the agent is
// allowed 2, and the model went looking for the service's own documentation
// tool: describe nli_search, nli_report, nli_docs, nli_tool_guide, four guesses
// and four red rows, though nothing had broken. What it needed back was the
// names it can actually reach.
func TestAskingForAToolThisTurnCannotReachIsAnsweredWithWhatItCan(t *testing.T) {
	held := []tool.Schema{
		projected("nli_query", "NLI", "Execute a read-only SQL SELECT."),
		projected("nli_db_schema", "NLI", "Get the live schema."),
	}
	services := []Service{{Name: "NLI", Alias: "nli"}}

	// The exact call that failed: a tool the service has and this turn does not.
	tl := New(func() []tool.Schema { return held }, func() []Service { return services })
	res, err := tl.Handle(context.Background(), tool.Call{
		Args: json.RawMessage(`{"operation":"describe","service":"nli","tool":"nli_tool_guide"}`),
	})
	if err != nil {
		t.Fatalf("the ability errored: %v", err)
	}
	if res.Failed() {
		t.Fatalf("discovery reported a broken tool for a name it simply does not hold: %s", res.Content)
	}

	var payload struct {
		Found     *bool    `json:"found"`
		Available []string `json:"available"`
	}
	if err := json.Unmarshal(res.Content, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Found == nil || *payload.Found {
		t.Fatalf("the answer did not say it found nothing: %s", res.Content)
	}
	// The reachable names, exact and prefixed, which is what a call takes.
	for _, want := range []string{"nli_query", "nli_db_schema"} {
		if !contains(payload.Available, want) {
			t.Fatalf("the answer does not name %q as reachable: %v", want, payload.Available)
		}
	}
	// And NOT the one it cannot reach, which is the whole point: a list that
	// included it would send the model straight back to the same call.
	if contains(payload.Available, "nli_tool_guide") {
		t.Fatalf("a tool this turn cannot call was offered as available: %v", payload.Available)
	}
}

// An unknown SERVICE is answered the same way.
func TestAskingAboutAServiceThisTurnDoesNotHaveIsAnswered(t *testing.T) {
	held := []tool.Schema{projected("nli_query", "NLI", "Read rows.")}
	tl := New(func() []tool.Schema { return held }, func() []Service { return []Service{{Name: "NLI", Alias: "nli"}} })
	res, err := tl.Handle(context.Background(), tool.Call{
		Args: json.RawMessage(`{"operation":"tools","service":"jira"}`),
	})
	if err != nil {
		t.Fatalf("the ability errored: %v", err)
	}
	if res.Failed() {
		t.Fatalf("an unknown service reported a broken tool: %s", res.Content)
	}
	if !strings.Contains(string(res.Content), "nli_query") {
		t.Fatalf("the answer does not name what is reachable: %s", res.Content)
	}
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
