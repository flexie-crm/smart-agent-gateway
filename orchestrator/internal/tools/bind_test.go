package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolguide"
)

// tool_guide bound to the code's registry cannot see a tool an administrator
// created: it is bound into the turn's loadout, never registered. Its guide and
// its topics exist, and without this the model asking for them is told there is
// no such ability, which is how a tool's whole documentation goes unreachable.
func TestToolGuideSeesTheTurnsOwnTools(t *testing.T) {
	guide, _ := json.Marshal("Run one command per call.")
	loadout := tool.Loadout{
		Schemas: []tool.Schema{
			{Name: toolguide.Name, Kind: tool.KindInternal},
			{
				Name:  "ssh_production",
				Guide: guide,
				Topics: []tool.Topic{
					{ID: "ssh_production/commands", Title: "Which commands this server permits", Body: "Only these may run."},
				},
			},
		},
		Handlers: map[string]tool.Handler{toolguide.Name: nil},
	}
	BindToolGuide(loadout)

	handle := loadout.Handlers[toolguide.Name]
	if handle == nil {
		t.Fatal("tool_guide was not rebound")
	}

	// By name: the guide and the table of contents come back.
	args, _ := json.Marshal(map[string]string{"tool_name": "ssh_production"})
	res, err := handle(context.Background(), tool.Call{Args: args})
	if err != nil {
		t.Fatalf("look up the tool: %v", err)
	}
	if res.Failed() {
		t.Fatalf("a tool in this turn's loadout was reported as unknown: %s", res.Content)
	}
	if !strings.Contains(string(res.Content), "Run one command per call") {
		t.Fatalf("the guide did not come back: %s", res.Content)
	}
	if !strings.Contains(string(res.Content), "ssh_production/commands") {
		t.Fatalf("the topics did not come back: %s", res.Content)
	}

	// By topic id: the body comes back, with the tool that owns it.
	args, _ = json.Marshal(map[string]string{"topic_id": "ssh_production/commands"})
	res, err = handle(context.Background(), tool.Call{Args: args})
	if err != nil {
		t.Fatalf("open the topic: %v", err)
	}
	if res.Failed() || !strings.Contains(string(res.Content), "Only these may run") {
		t.Fatalf("the topic did not open: %s", res.Content)
	}
}

// It answers for this turn and no further: an ability the assistant was not
// granted is not in the loadout, so it is not described either.
func TestToolGuideDoesNotDescribeWhatTheTurnDoesNotHave(t *testing.T) {
	guide, _ := json.Marshal("secret")
	loadout := tool.Loadout{
		Schemas:  []tool.Schema{{Name: toolguide.Name, Kind: tool.KindInternal}, {Name: "granted", Guide: guide}},
		Handlers: map[string]tool.Handler{toolguide.Name: nil},
	}
	BindToolGuide(loadout)

	args, _ := json.Marshal(map[string]string{"tool_name": "not_granted"})
	res, err := loadout.Handlers[toolguide.Name](context.Background(), tool.Call{Args: args})
	if err != nil {
		t.Fatalf("look up: %v", err)
	}
	// The property is that the withheld guide is not DESCRIBED, which is now
	// asserted directly rather than through "the call failed". Finding nothing
	// is an answer here, not a fault, and the old proxy would have passed just
	// as well if the refusal itself had carried the guide in it.
	if strings.Contains(string(res.Content), "secret") {
		t.Fatalf("an ability this turn does not have was described: %s", res.Content)
	}
	if !strings.Contains(string(res.Content), `"found":false`) {
		t.Fatalf("the answer did not say it found nothing: %s", res.Content)
	}
	// And what it offers instead is only what this turn does have.
	if !strings.Contains(string(res.Content), "granted") {
		t.Fatalf("the alternatives do not name this turn's abilities: %s", res.Content)
	}

	// The control for the assertion above, in the same test: the guide's body
	// IS returned for an ability this turn holds. Without this, "secret" could
	// be absent because nothing ever describes anything, and the check would
	// pass while proving nothing.
	args, _ = json.Marshal(map[string]string{"tool_name": "granted"})
	res, err = loadout.Handlers[toolguide.Name](context.Background(), tool.Call{Args: args})
	if err != nil {
		t.Fatalf("look up the granted one: %v", err)
	}
	if !strings.Contains(string(res.Content), "secret") {
		t.Fatalf("a granted ability's guide was not described, so the check above proves nothing: %s", res.Content)
	}
}

// A turn without tool_guide is left alone.
func TestBindToolGuideIgnoresATurnWithoutIt(t *testing.T) {
	loadout := tool.Loadout{Handlers: map[string]tool.Handler{}}
	BindToolGuide(loadout)
	if len(loadout.Handlers) != 0 {
		t.Fatal("tool_guide was added to a turn that does not have it")
	}
}

// gatewayWith builds a Gateway loadout holding tool_guide and the given tools.
func gatewayWith(schemas ...tool.Schema) tool.Loadout {
	return tool.Loadout{
		Schemas:  append([]tool.Schema{{Name: toolguide.Name, Kind: tool.KindInternal}}, schemas...),
		Handlers: map[string]tool.Handler{toolguide.Name: nil},
	}
}

func guideFor(t *testing.T, loadout tool.Loadout, args map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := loadout.Handlers[toolguide.Name](context.Background(), tool.Call{Args: raw})
	if err != nil {
		t.Fatalf("system error: %v", err)
	}
	return string(res.Content)
}

// A tool the Gateway holds ITSELF is never reported as an agent's, even when an
// agent holds it too.
//
// Both being granted the same tool is ordinary configuration, and getting this
// backwards is worse than not labelling at all: the Gateway would be told it
// cannot call a tool that is sitting in its own tool list, and would delegate
// work it could have done.
func TestAToolTheCallerHoldsIsNeverReportedAsAnAgents(t *testing.T) {
	guide, _ := json.Marshal("Fetch a URL.")
	own := tool.Schema{Name: "http_request", Guide: guide}

	loadout := gatewayWith(own)
	BindToolGuide(loadout, AgentTool{Schema: own, Agent: "researcher"})

	got := guideFor(t, loadout, map[string]any{"tool_name": "http_request"})
	if !strings.Contains(got, "Fetch a URL.") {
		t.Fatalf("the guide did not come back:\n%s", got)
	}
	if strings.Contains(got, "researcher") || strings.Contains(got, "cannot call") {
		t.Fatalf("the Gateway's own tool was reported as its agent's:\n%s", got)
	}
}

// The list of what CAN be looked up is the caller's own tools, deliberately not
// its agents'.
//
// That list reads as an inventory. An agent's tool in it would be a name the
// Gateway found among its own with nothing saying otherwise, which is the
// shortest path to a call it cannot make; agent_guide is what advertises an
// agent's abilities, and it says whose they are in the same answer.
func TestTheListOfWhatCanBeLookedUpIsTheCallersOwn(t *testing.T) {
	guide, _ := json.Marshal("either one")
	loadout := gatewayWith(tool.Schema{Name: "http_request", Guide: guide})
	BindToolGuide(loadout, AgentTool{Schema: tool.Schema{Name: "nli_query", Guide: guide}, Agent: "researcher"})

	// Called with nothing, tool_guide lists what it can look up.
	listed := guideFor(t, loadout, map[string]any{})
	if !strings.Contains(listed, "http_request") {
		t.Fatalf("the caller's own documented tool is not listed:\n%s", listed)
	}
	if strings.Contains(listed, "nli_query") {
		t.Fatalf("an agent's tool was advertised in the caller's own inventory:\n%s", listed)
	}
	// It still resolves when the Gateway names it, which is the whole point of
	// keeping it out of the list rather than out of the registry.
	named := guideFor(t, loadout, map[string]any{"tool_name": "nli_query"})
	if !strings.Contains(named, "researcher") {
		t.Fatalf("an agent's tool could not be read by name:\n%s", named)
	}
}
