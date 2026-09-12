package app_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
)

// Which agent a tool is bound for.
//
// A tool that keeps anything between calls (the server tool's open program, its
// sign-in waiting on a code) files it under the agent that opened it. That only
// holds if the app hands the right owner to Bind, and getting it wrong is not
// visible from inside the template: it was the conversation for a while, and a
// background agent runs under its Gateway's conversation, so a Gateway and
// every agent it started shared one entry and could type into each other's
// programs.
//
// These prove the seam. The template below is an observation point: it records
// the owner it was bound with and does nothing else.

// ownerSpy is a template whose only behaviour is remembering who it was bound
// for, so a test can see what the app decided.
type ownerSpy struct {
	mu     sync.Mutex
	owners []tool.Owner
}

func (s *ownerSpy) record(owner tool.Owner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners = append(s.owners, owner)
}

// bound returns the owners handed out so far, newest last.
func (s *ownerSpy) bound() []tool.Owner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tool.Owner(nil), s.owners...)
}

func (s *ownerSpy) Name() string        { return "ownerspy" }
func (s *ownerSpy) Title() string       { return "Owner spy" }
func (s *ownerSpy) Description() string { return "Records the agent it was bound for." }

func (s *ownerSpy) Variants() []template.Variant {
	return []template.Variant{{Key: "spy", Label: "Spy"}}
}

func (s *ownerSpy) Fields(string) ([]template.Section, error) { return nil, nil }
func (s *ownerSpy) SecretPaths(string) []string               { return nil }
func (s *ownerSpy) Params() []template.Param                  { return nil }
func (s *ownerSpy) DefaultGuide() string                      { return "" }

func (s *ownerSpy) Config(string, map[string]any) (json.RawMessage, error) {
	return json.RawMessage(`{"driver":"spy"}`), nil
}

func (s *ownerSpy) Build(in template.Input) (template.Instance, error) {
	cfg, err := s.Config(in.Variant, in.Settings)
	if err != nil {
		return template.Instance{}, err
	}
	return template.Instance{
		Schema: tool.Schema{
			Name:        "ownerspy_" + in.Alias,
			Description: "Records the agent it was bound for.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Risk:        tool.RiskReadOnly,
		},
		Config: cfg,
	}, nil
}

func (s *ownerSpy) Bind(_ json.RawMessage, owner tool.Owner) (tool.Handler, error) {
	s.record(owner)
	return func(context.Context, tool.Call) (tool.Result, error) {
		return tool.Result{Content: json.RawMessage(`{"success":true}`)}, nil
	}, nil
}

func (s *ownerSpy) Test(context.Context, json.RawMessage) error { return nil }

func (s *ownerSpy) Documentation(json.RawMessage) template.Documentation {
	return template.Documentation{}
}

// spyTool registers the spy template and creates one tool from it, granted to
// everyone in the workspace, so a loadout that asks for it binds it.
func spyTool(t *testing.T, e *env, alias string) *ownerSpy {
	t.Helper()
	spy := &ownerSpy{}
	e.app.Templates.Add(spy)
	if _, err := e.app.CreateCustomTool(context.Background(), e.ws.ID, "ownerspy", template.Input{
		Alias: alias, Variant: "spy",
	}); err != nil {
		t.Fatalf("create spy tool: %v", err)
	}
	return spy
}

// Two agents are two agents, even when they are two runs of the SAME
// agent on the same machine in the same conversation. Each resolution
// binds its tools for one running agent, so nothing either of them opens can be
// reached by the other.
func TestEachAgentIsItsOwnAgent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("ops@acme.test")
	spy := spyTool(t, e, "one")

	if err := e.app.Store.Agents().Create(ctx, &model.Agent{
		WorkspaceID: e.ws.ID, Key: "sysadmin", Name: "Sysadmin",
		Tools: []string{"ownerspy_one"}, Status: model.StatusActive,
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	first, err := e.app.ResolveAgent(ctx, e.ws.ID, user.ID, app.Computer{}, "sysadmin")
	if err != nil {
		t.Fatalf("resolve first agent: %v", err)
	}
	second, err := e.app.ResolveAgent(ctx, e.ws.ID, user.ID, app.Computer{}, "sysadmin")
	if err != nil {
		t.Fatalf("resolve second agent: %v", err)
	}
	if _, ok := first.Tools.Schema("ownerspy_one"); !ok {
		t.Fatal("the agent did not get its tool")
	}
	if _, ok := second.Tools.Schema("ownerspy_one"); !ok {
		t.Fatal("the second agent did not get its tool")
	}

	owners := spy.bound()
	if len(owners) < 2 {
		t.Fatalf("the tool was bound %d times, want one per agent", len(owners))
	}
	a, b := owners[len(owners)-2], owners[len(owners)-1]
	if a == b {
		t.Fatalf("two running agents share one owner (%q): whatever one of them opens, the other reaches", a)
	}
	if a == tool.OwnerNone || b == tool.OwnerNone {
		t.Fatalf("an agent was bound with no owner at all: %q, %q", a, b)
	}
}

// The Gateway is one agent per conversation, and its loadout is rebuilt every
// turn. So the owner has to be the same on every turn of a conversation, or a
// program it opened on one turn would be unreachable on the next, and different
// between conversations, or two people would share one.
func TestTheGatewayIsItsConversation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("ops@acme.test")
	spy := spyTool(t, e, "two")

	profile := &model.Profile{Tools: []string{"ownerspy_two"}}

	turn := func(sessionID int64) tool.Owner {
		t.Helper()
		before := len(spy.bound())
		if _, err := e.app.ResolveTools(ctx, app.ProfileRequest{
			WorkspaceID: e.ws.ID,
			UserID:      user.ID,
			Channel:     model.ChannelChat,
			SessionID:   sessionID,
		}, profile); err != nil {
			t.Fatalf("resolve tools: %v", err)
		}
		owners := spy.bound()
		if len(owners) != before+1 {
			t.Fatalf("the tool was bound %d times this turn, want once", len(owners)-before)
		}
		return owners[len(owners)-1]
	}

	firstTurn := turn(42)
	secondTurn := turn(42)
	if firstTurn != secondTurn {
		t.Fatalf("the Gateway changed owner between turns (%q then %q): anything it left open is lost", firstTurn, secondTurn)
	}

	otherChat := turn(43)
	if otherChat == firstTurn {
		t.Fatalf("two conversations share one owner (%q): each would reach what the other left open", otherChat)
	}
}
