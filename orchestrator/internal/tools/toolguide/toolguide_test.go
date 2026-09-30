package toolguide

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/tool"
)

// fakeRegistry stands in for the real one: it holds a couple of tools so the
// guide lookup can be tested without the whole registry.
type fakeRegistry struct {
	tools map[string]tool.Schema
	// owners names the agent holding a tool the caller does not itself hold.
	owners map[string]string
}

func (f fakeRegistry) Lookup(name string) (tool.Schema, bool) {
	s, ok := f.tools[name]
	return s, ok
}

func (f fakeRegistry) LookupTopic(id string) (tool.Topic, string, bool) {
	for name, s := range f.tools {
		for _, topic := range s.Topics {
			if topic.ID == id {
				return topic, name, true
			}
		}
	}
	return tool.Topic{}, "", false
}

func (f fakeRegistry) Owner(name string) string { return f.owners[name] }

func (f fakeRegistry) Guided() []string {
	var out []string
	for name, s := range f.tools {
		if len(s.Guide) > 0 || len(s.Topics) > 0 {
			out = append(out, name)
		}
	}
	return out
}

func call(t *testing.T, reg Registry, args map[string]any) tool.Result {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := New(reg).Handle(context.Background(), tool.Call{Args: raw})
	if err != nil {
		t.Fatalf("system error: %v", err)
	}
	return res
}

// A tool with a guide returns it verbatim, so the model gets the deep contract
// it asked for.
func TestReturnsGuide(t *testing.T) {
	reg := fakeRegistry{tools: map[string]tool.Schema{
		"http_request": {Name: "http_request", Guide: json.RawMessage(`{"summary":"makes a request"}`)},
	}}
	res := call(t, reg, map[string]any{"tool_name": "http_request"})
	if res.Failed() {
		t.Fatalf("a guided tool lookup failed: %s", res.Content)
	}
	if !strings.Contains(string(res.Content), "makes a request") {
		t.Fatalf("the guide was not returned: %s", res.Content)
	}
}

// A tool without a guide says so, and points at its short description instead of
// failing.
func TestToolWithoutGuide(t *testing.T) {
	reg := fakeRegistry{tools: map[string]tool.Schema{
		"current_time": {Name: "current_time", Description: "Get the current time."},
	}}
	res := call(t, reg, map[string]any{"tool_name": "current_time"})
	if res.Failed() {
		t.Fatalf("a guideless tool should not be a failure: %+v", res)
	}
	if !strings.Contains(string(res.Content), "Get the current time.") {
		t.Fatalf("the short description was not offered: %s", res.Content)
	}
}

// An unknown name is an ANSWER that lists what does have a guide, so the model
// can correct itself.
//
// Not a failure: this is an internal lookup, and "there is no ability by that
// name, here are the ones there are" is the question answered, not a fault. It
// used to fail, which put a red row in somebody's conversation for a
// documentation lookup that behaved exactly as designed.
func TestUnknownTool(t *testing.T) {
	reg := fakeRegistry{tools: map[string]tool.Schema{
		"http_request": {Name: "http_request", Guide: json.RawMessage(`{}`)},
	}}
	res := call(t, reg, map[string]any{"tool_name": "does_not_exist"})
	if res.Failed() {
		t.Fatalf("an internal lookup that found nothing should not fail: %s", res.Content)
	}
	// It has to be unmistakable that nothing was found, or a short message
	// reads as short documentation.
	if !strings.Contains(string(res.Content), `"found":false`) {
		t.Fatalf("the answer did not say it found nothing: %s", res.Content)
	}
	if !strings.Contains(string(res.Content), "http_request") {
		t.Fatalf("the answer did not list the guided tools: %s", res.Content)
	}
}

// A tool with a topic graph returns its table of contents (ids + titles, no
// bodies) when opened by name, so the model can choose which concept to drill
// into.
func TestOpensToolTableOfContents(t *testing.T) {
	reg := fakeRegistry{tools: map[string]tool.Schema{
		"http_request": {
			Name: "http_request",
			Topics: []tool.Topic{
				{ID: "http_request/auth", Title: "Sending credentials", Body: "the body"},
				{ID: "http_request/errors", Title: "Reading failures", Body: "the body"},
			},
		},
	}}
	res := call(t, reg, map[string]any{"tool_name": "http_request"})
	if res.Failed() {
		t.Fatalf("opening a tool with topics failed: %s", res.Content)
	}
	body := string(res.Content)
	// The TOC carries the ids and titles.
	for _, want := range []string{"http_request/auth", "Sending credentials", "http_request/errors"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the table of contents is missing %q:\n%s", want, body)
		}
	}
	// But not the bodies: the TOC is a menu, not the content.
	if strings.Contains(body, "the body") {
		t.Fatalf("the table of contents leaked topic bodies:\n%s", body)
	}
}

// Opening a topic by id returns its body and the edges the model can follow
// next, with their type and the condition for following them.
func TestOpensTopicWithEdges(t *testing.T) {
	reg := fakeRegistry{tools: map[string]tool.Schema{
		"http_request": {
			Name: "http_request",
			Topics: []tool.Topic{
				{
					ID: "http_request/body-shapes", Title: "The three body shapes",
					Body: "json beats form beats body",
					Edges: []tool.TopicEdge{
						{To: "http_request/form-encoding", Type: tool.EdgeCompanion, When: "you are sending arrays"},
					},
				},
				{ID: "http_request/form-encoding", Title: "Form encoding", Body: "bracketed indices"},
			},
		},
	}}
	res := call(t, reg, map[string]any{"topic_id": "http_request/body-shapes"})
	if res.Failed() {
		t.Fatalf("opening a topic failed: %s", res.Content)
	}
	body := string(res.Content)
	for _, want := range []string{"json beats form beats body", "http_request/form-encoding", "companion", "you are sending arrays"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the opened topic is missing %q:\n%s", want, body)
		}
	}
}

// A topic id is global: the model can follow an edge without naming the tool
// again, so an unknown id answers by pointing back at the tool list.
//
// This is the call that was seen failing in a real conversation: the model
// followed a topic id it had inferred rather than read, and a documentation
// miss was drawn as a broken tool.
func TestUnknownTopic(t *testing.T) {
	reg := fakeRegistry{tools: map[string]tool.Schema{
		"http_request": {Name: "http_request", Topics: []tool.Topic{{ID: "http_request/auth", Title: "x", Body: "y"}}},
	}}
	res := call(t, reg, map[string]any{"topic_id": "http_request/does-not-exist"})
	if res.Failed() {
		t.Fatalf("an internal lookup that found nothing should not fail: %s", res.Content)
	}
	if !strings.Contains(string(res.Content), `"found":false`) {
		t.Fatalf("the answer did not say it found nothing: %s", res.Content)
	}
	// And it names what CAN be looked up, which is the whole reason this is an
	// answer rather than an error.
	if !strings.Contains(string(res.Content), "http_request") {
		t.Fatalf("the answer did not point back at the tool list: %s", res.Content)
	}
}

// With neither argument, the tool lists what can be looked up rather than
// failing, so the model can discover its documentation.
func TestListsWhatIsAvailable(t *testing.T) {
	reg := fakeRegistry{tools: map[string]tool.Schema{
		"http_request": {Name: "http_request", Topics: []tool.Topic{{ID: "http_request/auth", Title: "x", Body: "y"}}},
	}}
	res := call(t, reg, map[string]any{})
	if res.Failed() {
		t.Fatalf("listing should not be a failure: %s", res.Content)
	}
	if !strings.Contains(string(res.Content), "http_request") {
		t.Fatalf("the listing did not name the documented tool: %s", res.Content)
	}
}

// A tool one of the Gateway's agents holds resolves, and the answer says whose
// it is.
//
// Both halves matter and they pull against each other. It must resolve, because
// the Gateway chooses where to send a task and agent_guide hands it the keys of
// what each agent holds; refusing them would mean choosing blind. It must be
// labelled, because a guide that reads exactly like the guide of a tool you hold
// is an invitation to call it, which is the one thing the Gateway cannot do with
// somebody else's tool.
func TestAnAgentsToolIsReadableAndSaysWhoseItIs(t *testing.T) {
	reg := fakeRegistry{
		tools: map[string]tool.Schema{
			"http_request": {Name: "http_request", Guide: json.RawMessage(`{"summary":"ours"}`)},
			"nli_query":    {Name: "nli_query", Guide: json.RawMessage(`{"summary":"searches records"}`)},
		},
		owners: map[string]string{"nli_query": "researcher"},
	}

	got := string(call(t, reg, map[string]any{"tool_name": "nli_query"}).Content)
	// The guide itself came back.
	if !strings.Contains(got, "searches records") {
		t.Fatalf("an agent's tool could not be read at all:\n%s", got)
	}
	// And it is marked as the agent's, with what to do instead of calling it.
	for _, want := range []string{"researcher", "not yours", "cannot call", "Delegate"} {
		if !strings.Contains(got, want) {
			t.Errorf("the answer does not say the tool is the agent's (%q missing):\n%s", want, got)
		}
	}

	// The caller's OWN tool is not labelled: the note exists to mark the
	// exception, and on every answer it would be noise the model learns to skip.
	own := string(call(t, reg, map[string]any{"tool_name": "http_request"}).Content)
	if strings.Contains(own, "belongs_to") || strings.Contains(own, "cannot call") {
		t.Errorf("the caller's own tool was reported as somebody else's:\n%s", own)
	}
}

// A topic of an agent's tool is labelled too. A drilldown is where the Gateway
// ends up after reading a guide, so a label that stopped at the first page
// would be a label it reads once and then loses.
func TestATopicOfAnAgentsToolSaysWhoseItIs(t *testing.T) {
	reg := fakeRegistry{
		tools: map[string]tool.Schema{
			"nli_query": {Name: "nli_query", Topics: []tool.Topic{
				{ID: "nli_query/limits", Title: "Limits", Body: "one statement at a time"},
			}},
		},
		owners: map[string]string{"nli_query": "researcher"},
	}
	got := string(call(t, reg, map[string]any{"topic_id": "nli_query/limits"}).Content)
	if !strings.Contains(got, "one statement at a time") {
		t.Fatalf("the topic body did not come back:\n%s", got)
	}
	for _, want := range []string{"researcher", "cannot call"} {
		if !strings.Contains(got, want) {
			t.Errorf("the topic does not say it is the agent's (%q missing):\n%s", want, got)
		}
	}
}

// A connected service's tool is not ours to document.
//
// This is the call that produced the defect. The model asked this guide about
// nli_query and was told "this ability has no deep documentation; its short
// description is all there is", which we cannot know: MCP carries no standard
// for documentation past the description, Flexie projects a guide tool of its
// own for exactly that, and the answer was therefore false. Told there was
// nothing deeper, the model went looking for a topic anyway and invented
// query/leads-and-contacts.
func TestAServicesToolIsRoutedToTheServiceNotDeclaredUndocumented(t *testing.T) {
	reg := fakeRegistry{tools: map[string]tool.Schema{
		"nli_query": {
			Name:        "nli_query",
			Kind:        tool.KindMCP,
			Service:     "NLI",
			RemoteName:  "query",
			Description: "Execute a read-only SQL SELECT.",
		},
	}}

	res := call(t, reg, map[string]any{"tool_name": "nli_query"})
	if res.Failed() {
		t.Fatalf("reading a projected tool failed: %s", res.Content)
	}
	got := string(res.Content)

	// The false claim is gone.
	if strings.Contains(got, "no deep documentation") {
		t.Fatalf("we told the model a third party's tool has no documentation: %s", got)
	}
	// It says whose it is, and routes to where the answer actually lives.
	for _, want := range []string{"NLI", "integrations"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the answer does not mention %q: %s", want, got)
		}
	}
	// And hands over the name the SERVICE uses, which is what its own tools
	// expect: our prefix is ours.
	var payload struct {
		Ours          *bool  `json:"ours"`
		NameOnService string `json:"name_on_service"`
		Does          string `json:"does"`
	}
	if err := json.Unmarshal(res.Content, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.NameOnService != "query" {
		t.Fatalf("name_on_service = %q, want the service's own name %q", payload.NameOnService, "query")
	}
	if payload.Ours == nil || *payload.Ours {
		t.Fatalf("a projected tool was not marked as not ours: %s", got)
	}
	if payload.Does != "Execute a read-only SQL SELECT." {
		t.Fatalf("the service's own description was dropped: %s", got)
	}
}

// And OUR tool with nothing deeper still says so plainly, which is the control
// for the branch above: the claim is not gone everywhere, only where we are not
// entitled to make it.
func TestOurOwnUndocumentedToolStillSaysSo(t *testing.T) {
	reg := fakeRegistry{tools: map[string]tool.Schema{
		"current_time": {Name: "current_time", Description: "The time where you are."},
	}}
	res := call(t, reg, map[string]any{"tool_name": "current_time"})
	if !strings.Contains(string(res.Content), "no deep documentation") {
		t.Fatalf("our own undocumented tool stopped saying so: %s", res.Content)
	}
}
