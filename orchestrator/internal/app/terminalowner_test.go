package app_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/link"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/machine"
)

// Whose terminal is whose, proven through the REAL resolution path.
//
// The wire-level test in internal/tools/machine proves the stamping given a
// call. This proves the part that actually broke: that an agent resolved the
// way the product resolves one ends up calling the terminal with its OWN
// identity on it. Everything here is the real code except the computer at the
// far end, which is the one thing a test cannot have.

// fakeComputer stands in for the person's machine: it records what each call
// was sent, and reports every machine tool as runnable so the loadout keeps
// them (machine.Offers asks this before a tool is offered at all).
type fakeComputer struct {
	mu   sync.Mutex
	sent []json.RawMessage
}

func (f *fakeComputer) Call(_ context.Context, _, _ int64, _, _ string, args json.RawMessage, _ string) (link.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, args)
	return link.Result{OK: true, Content: json.RawMessage(`{"output":"","running":false}`)}, nil
}

func (f *fakeComputer) Runs(int64, int64, string) map[string]int { return machine.Versions() }

// scopes reads the terminal namespace out of everything that was sent.
func (f *fakeComputer) scopes(t *testing.T) []int64 {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int64, 0, len(f.sent))
	for _, raw := range f.sent {
		var sent struct {
			Conversation int64 `json:"conversation"`
		}
		if err := json.Unmarshal(raw, &sent); err != nil {
			t.Fatalf("what was sent could not be read: %v\n%s", err, raw)
		}
		out = append(out, sent.Conversation)
	}
	return out
}

// runTerminal calls a loadout's terminal exactly as the agent loop does.
func runTerminal(t *testing.T, loadout tool.Loadout, sessionID int64, deviceID string) {
	t.Helper()
	handler, ok := loadout.Handlers[machine.TerminalName]
	if !ok {
		t.Fatal("the terminal is not in this loadout, so nothing could reach the computer")
	}
	if _, err := handler(context.Background(), tool.Call{
		WorkspaceID: 1, SessionID: sessionID, DeviceID: deviceID,
		Name: machine.TerminalName, Args: json.RawMessage(`{"command":"pwd"}`),
	}); err != nil {
		t.Fatalf("terminal: %v", err)
	}
}

// Two running agents of one conversation never touch the same terminal.
//
// This is the failure that was measured on a real installation twice: two fleet
// members asked to run one command each landed on one shell, and the second was
// refused with "this conversation's terminal is busy with" the first one's
// command, quoted back. Both were resolved through ResolveAgent, so that is
// what this drives.
func TestTwoRunningAgentsNeverShareATerminal(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("router@acme.test")

	far := &fakeComputer{}
	// BindTerminal reads this when a loadout is built, not at boot, which is
	// what lets a test stand in for the person's computer at all.
	e.app.Machines = far

	if err := e.app.Store.Agents().Create(ctx, &model.Agent{
		WorkspaceID: e.ws.ID, Key: "terminal-agent", Name: "Terminal Agent",
		Status: model.StatusActive, Tools: []string{machine.TerminalName},
	}, model.Nobody()); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	const device = "the-laptop"
	const conversation = int64(77)

	// Two RUNNING agents: the same agent resolved twice is two of them, which
	// is exactly what a fleet of two copies is.
	first, err := e.app.ResolveAgent(ctx, e.ws.ID, user.ID,
		app.Computer{DeviceID: device}, "terminal-agent")
	if err != nil {
		t.Fatalf("resolve the first agent: %v", err)
	}
	second, err := e.app.ResolveAgent(ctx, e.ws.ID, user.ID,
		app.Computer{DeviceID: device}, "terminal-agent")
	if err != nil {
		t.Fatalf("resolve the second agent: %v", err)
	}

	runTerminal(t, first.Tools, conversation, device)
	runTerminal(t, second.Tools, conversation, device)

	got := far.scopes(t)
	if len(got) != 2 {
		t.Fatalf("expected two calls to reach the computer, got %d", len(got))
	}
	if got[0] == got[1] {
		t.Fatalf("two running agents were sent the same terminal namespace (%d): "+
			"one would be refused with the other's command", got[0])
	}
	// And neither is the conversation's own, which belongs to the Gateway.
	for i, scope := range got {
		if scope == conversation {
			t.Errorf("agent %d was sent the Gateway's terminals (%d)", i+1, scope)
		}
	}
}

// The Gateway's own terminal is still its conversation's, which is what makes
// the shell theirs rather than everybody's. Resolved through the real profile
// path, not ResolveAgent.
func TestTheGatewayStillHasItsConversationsTerminal(t *testing.T) {
	e := newEnv(t)
	user := e.user("person@acme.test")

	far := &fakeComputer{}
	e.app.Machines = far

	// The house package, holding the terminal: the Gateway runs as whatever the
	// default agent says, and a workspace that has never been configured does
	// not hand out a tool that reaches somebody's computer.
	if err := e.app.Store.Agents().Create(context.Background(), &model.Agent{
		WorkspaceID: e.ws.ID, Key: model.DefaultAgentKey, Name: "Gateway",
		Status: model.StatusActive, Tools: []string{machine.TerminalName},
	}, model.Nobody()); err != nil {
		t.Fatalf("create the gateway: %v", err)
	}

	const device = "the-laptop"
	const conversation = int64(91)
	req := app.ProfileRequest{
		WorkspaceID: e.ws.ID, UserID: user.ID, Channel: model.ChannelChat,
		DeviceID: device, SessionID: conversation,
	}
	profile, err := e.app.ResolveProfile(context.Background(), req)
	if err != nil {
		t.Fatalf("resolve profile: %v", err)
	}
	loadout, err := e.app.ResolveTools(context.Background(), req, profile)
	if err != nil {
		t.Fatalf("resolve tools: %v", err)
	}

	runTerminal(t, loadout, conversation, device)
	got := far.scopes(t)
	if len(got) != 1 || got[0] != conversation {
		t.Fatalf("the Gateway's terminal namespace is %v, want the conversation %d", got, conversation)
	}
}
