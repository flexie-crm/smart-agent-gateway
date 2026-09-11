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
	if !res.Failed() {
		t.Fatalf("an ability this turn does not have was described: %s", res.Content)
	}
	// And what it offers instead is only what this turn does have.
	if !strings.Contains(string(res.Content), "granted") {
		t.Fatalf("the alternatives do not name this turn's abilities: %s", res.Content)
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
