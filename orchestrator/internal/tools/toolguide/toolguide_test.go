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

// An unknown name is a plain failure that lists what does have a guide, so the
// model can correct itself.
func TestUnknownTool(t *testing.T) {
	reg := fakeRegistry{tools: map[string]tool.Schema{
		"http_request": {Name: "http_request", Guide: json.RawMessage(`{}`)},
	}}
	res := call(t, reg, map[string]any{"tool_name": "does_not_exist"})
	if !res.Failed() {
		t.Fatal("an unknown tool name should fail")
	}
	if !strings.Contains(string(res.Content), "http_request") {
		t.Fatalf("the failure did not list the guided tools: %s", res.Content)
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
// again, so an unknown id is a plain failure that points back at the tool list.
func TestUnknownTopic(t *testing.T) {
	reg := fakeRegistry{tools: map[string]tool.Schema{
		"http_request": {Name: "http_request", Topics: []tool.Topic{{ID: "http_request/auth", Title: "x", Body: "y"}}},
	}}
	res := call(t, reg, map[string]any{"topic_id": "http_request/does-not-exist"})
	if !res.Failed() {
		t.Fatal("an unknown topic id should fail")
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
