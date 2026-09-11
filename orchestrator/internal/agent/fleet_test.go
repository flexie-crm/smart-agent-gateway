package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/model"
)

// What a fleet refuses, and why. Every one of these is a case where writing the
// rows first and discovering the problem second would leave a batch somebody
// has to clean up, so the whole call is refused with a sentence the Gateway can
// act on.

func fleetRunner() *Runner {
	return &Runner{log: zerolog.Nop()}
}

// roster answers with an agent per key, and an error for anything else.
func roster(agents map[string]AgentProfile) AgentResolver {
	return func(_ context.Context, key string) (AgentProfile, error) {
		sub, ok := agents[key]
		if !ok {
			return AgentProfile{}, errors.New("no such agent")
		}
		return sub, nil
	}
}

func TestFleetResolvesEveryMemberBeforeAnyRowIsWritten(t *testing.T) {
	r := fleetRunner()
	turn := Turn{Agent: roster(map[string]AgentProfile{
		"research": {Key: "research", Name: "Research"},
		"write":    {Key: "write", Name: "Writer"},
	})}

	members, refusal := r.fleetMembers(context.Background(), Delegation{
		Turn: turn,
		Tasks: []FleetTask{
			{Agent: "research", Task: "look it up"},
			{Agent: "write", Task: "write it down"},
		},
	})
	if refusal != "" {
		t.Fatalf("a fleet of two real agents was refused: %s", refusal)
	}
	if len(members) != 2 || members[0].Sub.Key != "research" || members[1].Task != "write it down" {
		t.Fatalf("members are not what was asked for: %+v", members)
	}

	// The twentieth agent not existing refuses the whole call, so the first
	// nineteen rows are never written.
	_, refusal = r.fleetMembers(context.Background(), Delegation{
		Turn: turn,
		Tasks: []FleetTask{
			{Agent: "research", Task: "look it up"},
			{Agent: "nobody", Task: "do something"},
		},
	})
	if !strings.Contains(refusal, `"nobody"`) {
		t.Fatalf("expected a refusal naming the missing agent, got %q", refusal)
	}
}

func TestFleetRefusesWhatItCannotRun(t *testing.T) {
	r := fleetRunner()
	turn := Turn{Agent: roster(map[string]AgentProfile{
		"research": {Key: "research", Name: "Research"},
		// An administrator pinned this one to run where the Gateway waits for
		// it, which a fleet member structurally cannot do.
		"inline": {Key: "inline", Name: "Inline", DelegationMode: model.DelegationModeInline},
	})}

	tooMany := make([]FleetTask, model.DefaultMaxFleetAgents+1)
	for i := range tooMany {
		tooMany[i] = FleetTask{Agent: "research", Task: fmt.Sprintf("task %d", i)}
	}

	cases := []struct {
		name  string
		d     Delegation
		wants string
	}{
		{"nothing asked for", Delegation{Turn: turn}, "at least one agent"},
		{"a task with no agent", Delegation{Turn: turn,
			Tasks: []FleetTask{{Task: "do it"}}}, "needs an agent and a task"},
		{"an agent with no task", Delegation{Turn: turn,
			Tasks: []FleetTask{{Agent: "research"}}}, "needs an agent and a task"},
		{"more than a fleet may hold", Delegation{Turn: turn, Tasks: tooMany}, "at most"},
		{"an agent pinned to run inline", Delegation{Turn: turn,
			Tasks: []FleetTask{{Agent: "inline", Task: "do it"}}}, "cannot run in a fleet"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			members, refusal := r.fleetMembers(context.Background(), c.d)
			if refusal == "" {
				t.Fatalf("expected a refusal, got %d members", len(members))
			}
			if !strings.Contains(refusal, c.wants) {
				t.Fatalf("refusal %q does not say %q", refusal, c.wants)
			}
			if members != nil {
				t.Fatalf("a refused fleet handed back members: %+v", members)
			}
		})
	}
}

// The cap on a batch is what an administrator set, and the code default only
// when they set nothing. It is not a number in the source any more.
func TestTheBatchCapIsASetting(t *testing.T) {
	r := fleetRunner()
	roster := roster(map[string]AgentProfile{"research": {Key: "research", Name: "Research"}})
	tasks := func(n int) []FleetTask {
		out := make([]FleetTask, n)
		for i := range out {
			out[i] = FleetTask{Agent: "research", Task: "look it up"}
		}
		return out
	}

	// Configured to three: four is refused, three is not, and the refusal says
	// the number the administrator chose rather than the one in the source.
	turn := Turn{Agent: roster, MaxFleetAgents: 3}
	_, refusal := r.fleetMembers(context.Background(), Delegation{Turn: turn, Tasks: tasks(4)})
	if !strings.Contains(refusal, "at most 3") {
		t.Fatalf("the refusal does not state the configured cap: %q", refusal)
	}
	if members, refusal := r.fleetMembers(context.Background(), Delegation{Turn: turn, Tasks: tasks(3)}); refusal != "" {
		t.Fatalf("three agents were refused by a cap of three: %s (%d members)", refusal, len(members))
	}

	// Configured to nothing: the code default applies, and a batch that would
	// have been fine under a bigger setting is refused.
	unset := Turn{Agent: roster}
	_, refusal = r.fleetMembers(context.Background(), Delegation{
		Turn: unset, Tasks: tasks(model.DefaultMaxFleetAgents + 1)})
	if !strings.Contains(refusal, fmt.Sprintf("at most %d", model.DefaultMaxFleetAgents)) {
		t.Fatalf("an unset cap should fall back to the code default: %q", refusal)
	}
}

// One agent reaching the fleet handoff through its PINNED mode rather than the
// fleet tool is a fleet of one. It costs nothing to allow, and refusing it would
// make a pin that an administrator set unusable.
func TestAPinnedAgentIsAFleetOfOne(t *testing.T) {
	r := fleetRunner()
	sub := AgentProfile{Key: "research", Name: "Research", DelegationMode: model.DelegationModeFleet}

	members, refusal := r.fleetMembers(context.Background(), Delegation{
		Turn: Turn{Agent: roster(map[string]AgentProfile{"research": sub})},
		Sub:  sub,
		Task: "look it up",
	})
	if refusal != "" {
		t.Fatalf("a fleet of one was refused: %s", refusal)
	}
	if len(members) != 1 || members[0].Sub.Key != "research" || members[0].Task != "look it up" {
		t.Fatalf("the single member is wrong: %+v", members)
	}
}

// The chip's name is what somebody reads while the work is going, so it says
// what is working rather than listing five names nobody needs yet.
func TestFleetNameSaysWhatIsWorking(t *testing.T) {
	research := FleetMember{Sub: AgentProfile{Key: "research", Name: "Research"}}
	writer := FleetMember{Sub: AgentProfile{Key: "write", Name: "Writer"}}

	cases := []struct {
		members []FleetMember
		want    string
	}{
		{[]FleetMember{research}, "Research"},
		{[]FleetMember{research, research, research}, "3 × Research"},
		{[]FleetMember{research, writer}, "2 agents"},
	}
	for _, c := range cases {
		if got := fleetName(c.members); got != c.want {
			t.Errorf("fleetName(%d members) = %q, want %q", len(c.members), got, c.want)
		}
	}
}

// A mode is detached when the Gateway answers now and hears back later. It is
// what a fleet member has to be, and what decides which pins can be one.
func TestDetachedModes(t *testing.T) {
	for mode, want := range map[string]bool{
		model.HandoffBackground: true,
		model.HandoffFleet:      true,
		model.HandoffContinue:   false,
		model.HandoffTerminal:   false,
		"":                      false,
	} {
		if got := Detached(mode); got != want {
			t.Errorf("Detached(%q) = %v, want %v", mode, got, want)
		}
	}
}

// The fleet mode resolves like every other: a pin wins, and the Gateway asking
// for a fleet gets one when nothing overrules it.
func TestFleetModeResolution(t *testing.T) {
	cases := []struct{ requested, pinned, want string }{
		{model.HandoffFleet, model.DelegationModeAuto, model.HandoffFleet},
		{model.HandoffFleet, "", model.HandoffFleet},
		{model.HandoffContinue, model.DelegationModeFleet, model.HandoffFleet},
		{model.HandoffBackground, model.DelegationModeFleet, model.HandoffFleet},
		// Pinned inline overrules a requested fleet back to synchronous, which
		// is what makes a fleet member with that pin refusable rather than
		// silently run the wrong way.
		{model.HandoffFleet, model.DelegationModeInline, model.HandoffContinue},
	}
	for _, c := range cases {
		if got := ResolveHandoff(c.requested, c.pinned); got != c.want {
			t.Errorf("ResolveHandoff(%q, %q) = %q, want %q", c.requested, c.pinned, got, c.want)
		}
	}
}

// A fleet member CAN stop to ask. The only reason left for refusing a card is a
// rejection this turn, and a member's park is an ordinary durable row: it runs
// on a worker with no socket, and the server, which has one, puts it on screen.
func TestOnlyARejectionBlocksACard(t *testing.T) {
	if !strings.Contains(approvalsAfterRejection, "declined") {
		t.Fatalf("a refused card should say the person declined: %q", approvalsAfterRejection)
	}
	// A member is detached, which is what queues its card behind any other and
	// stops it re-stubbing the Gateway's already-settled call.
	if !Detached(model.HandoffFleet) {
		t.Fatal("a fleet member must be detached, or its card would re-stub a finished hand-off")
	}
}
