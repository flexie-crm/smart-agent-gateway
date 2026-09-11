package app

import (
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
)

// The delegate tool offers a mode parameter only while at least one agent
// leaves the choice to the Gateway. When every agent pins its mode, the
// Gateway has nothing to decide and the parameter is dropped (KB/27).
func TestAgentToolSchemaDropsModeWhenAllPinned(t *testing.T) {
	hasMode := func(subs []agentInfo) bool {
		var parsed struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(delegateToolSchema(subs).InputSchema, &parsed); err != nil {
			t.Fatalf("parse schema: %v", err)
		}
		_, ok := parsed.Properties["mode"]
		return ok
	}

	allPinned := []agentInfo{
		{Key: "a", DelegationMode: model.DelegationModeBackground},
		{Key: "b", DelegationMode: model.DelegationModeInline},
	}
	if hasMode(allPinned) {
		t.Fatal("the mode parameter was offered even though every agent pins its mode")
	}

	someAuto := []agentInfo{
		{Key: "a", DelegationMode: model.DelegationModeBackground},
		{Key: "b", DelegationMode: model.DelegationModeAuto},
	}
	if !hasMode(someAuto) {
		t.Fatal("the mode parameter was dropped even though an agent leaves the mode to the Gateway")
	}
}

// A pinned agent's roster entry tells the Gateway its mode is fixed, so the
// Gateway routes accordingly and does not try to override it.
func TestRosterStatesAPinnedMode(t *testing.T) {
	got := agentsBody([]agentInfo{
		{Key: "fetcher", Name: "Fetcher", Instructions: "Fetches data.", DelegationMode: model.DelegationModeBackground},
		{Key: "helper", Name: "Helper", Instructions: "Helps.", DelegationMode: model.DelegationModeAuto},
	})
	if !strings.Contains(got, "Runs in the background") || !strings.Contains(got, "You do not choose its mode") {
		t.Fatalf("the roster did not state the pinned background mode:\n%s", got)
	}
	// An auto agent gets no such line.
	if strings.Count(got, "Runs in the background") != 1 {
		t.Fatalf("the auto agent was wrongly marked as pinned:\n%s", got)
	}
}

// Every pinned mode says what it is in the roster, and a mode the prompt does
// not know about is an agent the Gateway is told nothing about: it reads the
// same as `auto`, so the Gateway thinks the choice is its own and runs the
// agent whichever way it likes.
func TestEveryPinnedModeTellsTheGatewayWhatItIs(t *testing.T) {
	pinned := []string{
		model.DelegationModeBackground,
		model.DelegationModeInline,
		model.DelegationModeFleet,
	}
	for _, mode := range pinned {
		line := pinnedModeLine(mode)
		if line == "" {
			t.Errorf("an agent pinned to %q is described to the Gateway as if it chose", mode)
		}
	}
	// `auto` IS the Gateway choosing, so it says nothing.
	if pinnedModeLine(model.DelegationModeAuto) != "" {
		t.Error("an agent that leaves the choice to the Gateway should not be described as pinned")
	}
	if pinnedModeLine("") != "" {
		t.Error("an unset mode is auto, and should not be described as pinned")
	}
}
