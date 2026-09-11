package app_test

import (
	"context"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/queue"
	"flexie.io/sag/internal/tool"
)

// What the chat may open, and what it may not.
//
// A tool row in a conversation can be opened to see what the tool was sent and
// what it answered. That is for the work somebody asked for; it is not for our
// own wiring, and the difference is decided by App.InternalTool.
//
// The trap this guards is that internal tools arrive TWO ways. Most are
// registered in code and carry their kind on the schema. The rest (delegation,
// the fleet, remember, the background controls) are built per turn from what
// the conversation resolved to and are never registered at all, so asking the
// registry about them answers "not ours" and the fleet tool got an arrow on it
// in the chat.

// Every internal tool in a real, fully loaded loadout must be known as ours. It
// is built from the loadout rather than from a list, so a sixth internal tool
// added to the loop fails here rather than quietly becoming openable.
func TestEveryToolTheLoopAddsIsKnownAsOurs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("ida@acme.test")

	// A queue, so the fleet tool is offered: without one it is left out of the
	// loadout, which is exactly how it went unnoticed.
	e.app.Queue = queue.NewInProcess()

	// The Gateway, with tools, so `remember` rides along.
	if err := e.app.Store.Agents().Create(ctx, &model.Agent{
		WorkspaceID: e.ws.ID,
		Key:         model.DefaultAgentKey,
		Name:        "House",
		Tools:       e.app.DefaultTools(),
		Status:      model.StatusActive,
	}); err != nil {
		t.Fatalf("create the gateway: %v", err)
	}
	// And an agent, so the delegation tools ride along too.
	if err := e.app.Store.Agents().Create(ctx, &model.Agent{
		WorkspaceID: e.ws.ID,
		Key:         "researcher",
		Name:        "Researcher",
		Status:      model.StatusActive,
	}); err != nil {
		t.Fatalf("create an agent: %v", err)
	}

	_, loadout := e.resolveFull(user, model.ChannelChat)

	internal := 0
	for _, schema := range loadout.Schemas {
		if schema.Kind != tool.KindInternal {
			continue
		}
		internal++
		if !e.app.InternalTool(schema.Name) {
			t.Errorf("%q is our own wiring but the chat would offer to open it", schema.Name)
		}
	}
	// The count is asserted so the test cannot pass by loading nothing: a
	// loadout with no internal tools in it proves nothing about either half.
	if internal < 5 {
		t.Fatalf("only %d internal tools were loaded; the loadout is not the full one this is meant to check", internal)
	}

	// The two the registry cannot answer for, named, because they are the ones
	// this exists for.
	for _, name := range []string{model.DelegateToolName, model.FleetToolName} {
		if !e.app.InternalTool(name) {
			t.Errorf("%q is built per turn and never registered, and was not recognised as ours", name)
		}
	}
}

// And the other half of the rule: a tool that is NOT ours stays openable. A
// custom tool or one projected from a service does real work, and what it
// carried is what somebody wants to see.
func TestWorkAToolDidStaysOpenable(t *testing.T) {
	e := newEnv(t)
	if e.app.InternalTool("http_request") {
		t.Error("a tool that does real work was taken for our own wiring")
	}
	if e.app.InternalTool("some_tool_an_administrator_made") {
		t.Error("a tool the code has never heard of was taken for our own wiring")
	}
}
