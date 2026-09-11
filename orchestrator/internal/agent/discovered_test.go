package agent

import (
	"encoding/json"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/integrations"
)

// A call made through discovery is RECORDED as the tool it runs.
//
// This is the test that was missing, and its absence is why a green suite sat
// beside a chat showing "Connected services" four times over while a service's
// tool was what actually ran. The first version of this asserted that the
// resolver returned the right name, which it did: the defect was that nothing
// called it until after the row had been written, so the transcript, the chip,
// the confirmation card and the panel all recorded the plumbing instead of the
// work. Testing the function proved nothing about the placement.
//
// So this drives the seam the row is actually built through.
func TestADiscoveredCallIsRecordedAsTheToolItRuns(t *testing.T) {
	byName := map[string]tool.Schema{
		integrations.Name: {Name: integrations.Name, FriendlyName: "Connected services", Kind: tool.KindInternal},
		"nli_portal_discover": {
			Name: "nli_portal_discover", FriendlyName: "Portals", Kind: tool.KindMCP,
		},
	}
	buf := &stepBuffer{}
	buf.addToolCall(provider.ToolCall{
		ID:   "call_1",
		Name: integrations.Name,
		Args: json.RawMessage(`{"operation":"call","tool":"nli_portal_discover","arguments":{"mode":"list"}}`),
	})

	step := buf.step(Turn{SessionID: 7, WorkspaceID: 1}, 3,
		&provider.Resolved{Model: &model.AIModel{}, Vendor: &model.AIVendor{}}, byName, false)

	if len(step.ToolCalls) != 1 {
		t.Fatalf("expected one call, got %d", len(step.ToolCalls))
	}
	call := step.ToolCalls[0]
	if call.ToolName != "nli_portal_discover" {
		t.Fatalf("the row records %q, so the transcript, the card and the panel all name the "+
			"plumbing instead of the tool that ran", call.ToolName)
	}
	if call.FriendlyName != "Portals" {
		t.Fatalf("the row reads %q in the chat, where the work should be named", call.FriendlyName)
	}
	if string(call.Args) != `{"mode":"list"}` {
		t.Fatalf("the tool's own arguments were not carried onto the row: %s", call.Args)
	}
	// The model's call id is what its next message answers to.
	if call.ToolCallID != "call_1" {
		t.Fatalf("the call id changed under the model: %q", call.ToolCallID)
	}
}

// The ability's own reading operations stay its own: listing services, listing
// one service's tools and describing a tool are answered by its handler, and
// must not be recorded as anything else.
func TestTheAbilitysOwnReadsAreRecordedAsItself(t *testing.T) {
	byName := map[string]tool.Schema{
		integrations.Name:     {Name: integrations.Name, FriendlyName: "Connected services", Kind: tool.KindInternal},
		"nli_portal_discover": {Name: "nli_portal_discover", FriendlyName: "Portals"},
	}
	for _, args := range []string{
		`{"operation":"services"}`,
		`{"operation":"tools","service":"NLI"}`,
		`{"operation":"describe","tool":"nli_portal_discover"}`,
	} {
		buf := &stepBuffer{}
		buf.addToolCall(provider.ToolCall{ID: "c", Name: integrations.Name, Args: json.RawMessage(args)})
		step := buf.step(Turn{}, 1, &provider.Resolved{Model: &model.AIModel{}, Vendor: &model.AIVendor{}}, byName, false)
		if got := step.ToolCalls[0].ToolName; got != integrations.Name {
			t.Fatalf("a reading operation was recorded as %q: %s", got, args)
		}
	}
}

// A tool this turn does not hold is left alone, so it fails the way any invented
// tool name fails rather than being rewritten into a call to nothing.
func TestAToolThisTurnDoesNotHoldIsNotResolved(t *testing.T) {
	_, _, ok := discovered(integrations.Name,
		json.RawMessage(`{"operation":"call","tool":"not_here","arguments":{}}`),
		map[string]tool.Schema{})
	if ok {
		t.Fatal("a tool nobody holds was resolved")
	}
}

// And an ordinary call is untouched.
func TestAnOrdinaryCallIsUntouched(t *testing.T) {
	if _, _, ok := discovered("current_time", json.RawMessage(`{}`),
		map[string]tool.Schema{"current_time": {Name: "current_time"}}); ok {
		t.Fatal("an ordinary call was resolved as a discovered one")
	}
}

// What a turn can RUN is both lists: a lookup that saw only the offered tools
// would leave a discovered one with no schema, and therefore no approval, no
// risk, no drift check and nothing for the transcript to show.
func TestRunnableSeesWhatIsHeldOnDemand(t *testing.T) {
	byName := runnable(tool.Loadout{
		Schemas:  []tool.Schema{{Name: "current_time"}},
		OnDemand: []tool.Schema{{Name: "crm_search", RequiresApproval: true}},
	})
	if _, ok := byName["current_time"]; !ok {
		t.Fatal("an offered tool is missing")
	}
	held, ok := byName["crm_search"]
	if !ok {
		t.Fatal("a tool held on demand is missing, so it would run with no schema")
	}
	if !held.RequiresApproval {
		t.Fatal("the held tool lost the fact that it needs approval")
	}
}
