package app_test

import (
	"context"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
)

// The name a person reads when the assistant reaches a connected service.
//
// The frame the chat draws and the row the transcript keeps are the same
// string: agent_tool_calls.friendly_name is written from the schema resolved
// here, so whatever this says is also what the conversation says when it is
// read back tomorrow. That is why this is asserted on the resolved loadout
// rather than on the composing function alone, which would pass just as well
// if nothing ever called it.
func TestAProjectedToolIsShownWithTheConnectionItCameFrom(t *testing.T) {
	e := newEnv(t)
	user := e.user("reader@acme.test")
	remote := e.mcpTool("NLI", "nli", "search", "Flexie Search", "Search records.")

	// The Gateway holding both: the projected tool, and one of ours beside it
	// as the control.
	gateway := &model.Agent{
		WorkspaceID: e.ws.ID, Key: model.DefaultAgentKey, Name: "House",
		Instructions: "Answer plainly.",
		Status:       model.StatusActive,
		Tools:        []string{"current_time", remote},
	}
	if err := e.app.Store.Agents().Create(context.Background(), gateway, model.Nobody()); err != nil {
		t.Fatalf("create the gateway: %v", err)
	}

	loadout := e.gatewayTools(user)

	var projected *tool.Schema
	for i, s := range loadout.OnDemand {
		if s.Name == remote {
			projected = &loadout.OnDemand[i]
		}
	}
	if projected == nil {
		t.Fatalf("the projected tool was not in the loadout at all; on demand: %d", len(loadout.OnDemand))
	}
	if got, want := projected.FriendlyName, "NLI / Flexie Search"; got != want {
		t.Fatalf("a projected tool is shown as %q, want %q", got, want)
	}

	// The control, in the same loadout: a tool of ours is the product itself,
	// so it carries no connection and must not gain a prefix. Without this,
	// prefixing everything would pass the assertion above.
	var ours int
	for _, s := range loadout.Schemas {
		if s.Kind == tool.KindMCP {
			continue
		}
		ours++
		if strings.Contains(s.FriendlyName, " / ") {
			t.Fatalf("%s is ours and was given a prefix: %q", s.Name, s.FriendlyName)
		}
	}
	if ours == 0 {
		t.Fatal("no built-in tools in the loadout, so the control proved nothing")
	}
}
