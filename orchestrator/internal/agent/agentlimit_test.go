package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/tool"
)

// An agent that runs out of steps says which limit stopped it, whose it is,
// that the work may be part done, and where to raise it.

// askingForNothing is a model that calls a tool it was never given, for ever,
// so the loop spins to its limit without running anything.
type askingForNothing struct {
	provider.Provider
	mu    sync.Mutex
	calls int
}

func (a *askingForNothing) Stream(context.Context, provider.GenerateRequest) (<-chan provider.StreamEvent, error) {
	a.mu.Lock()
	a.calls++
	n := a.calls
	a.mu.Unlock()

	events := make(chan provider.StreamEvent, 2)
	events <- provider.StreamEvent{Kind: provider.EventToolCall, ToolCall: &provider.ToolCall{
		ID: "call_" + string(rune('a'+n)), Name: "a_tool_it_never_had", Args: json.RawMessage(`{}`),
	}}
	events <- provider.StreamEvent{Kind: provider.EventDone}
	close(events)
	return events, nil
}

func (a *askingForNothing) asked() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

type nowhere struct{}

func (nowhere) Write(chat.Frame) error { return nil }

func TestAnAgentOutOfStepsSaysWhatStoppedIt(t *testing.T) {
	r, _ := fakeRunner()
	vendor := &askingForNothing{}
	resolved := &provider.Resolved{
		Provider: vendor,
		Model:    &model.AIModel{ID: 7, ModelKey: "test-model"},
		Vendor:   &model.AIVendor{VendorKey: "test-vendor"},
	}
	// A name ending in "assistant", so a sentence that appends the noun shows.
	sub := AgentProfile{Key: "research", Name: "Research assistant", MaxIterations: 3, Tools: tool.Loadout{}}

	said, err := r.agentLoop(context.Background(), Turn{SessionID: 4, WorkspaceID: 1}, sub, resolved,
		[]provider.Message{{Role: provider.RoleUser, Content: "find everything"}},
		"call_delegate", model.HandoffTerminal, chat.NewStream(nowhere{}))
	if err != nil {
		t.Fatalf("the agent loop failed instead of running out: %v", err)
	}
	if vendor.asked() != 3 {
		t.Fatalf("the loop stopped after %d of its 3 steps", vendor.asked())
	}
	if strings.Contains(said, "I was not able to finish this task") || strings.Contains(said, "assistant assistant") {
		t.Fatalf("the agent's last words are wrong: %q", said)
	}
	for _, fact := range []string{"limit", "3 steps", "Research assistant", "may already be done", "console"} {
		if !strings.Contains(said, fact) {
			t.Fatalf("the agent's last words do not say %q: %q", fact, said)
		}
	}
}

func TestAnAgentWithNoNameIsNamedByItsKey(t *testing.T) {
	for name, want := range map[string]string{"": "research", "  ": "research", "Research assistant": "Research assistant"} {
		if got := agentName(AgentProfile{Key: "research", Name: name}); got != want {
			t.Fatalf("an agent named %q came out as %q, want %q", name, got, want)
		}
	}
}
