package app

import (
	"testing"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/machine"
)

// Where a fleet member runs is decided by one thing: whether it holds a tool
// that runs on the person's own computer.
//
// It cannot run on a worker if it does. The link is a socket the chat
// application dialled to the gateway process and keeps alive itself; another
// process cannot join it. Measured before this existed: three fleet members
// asked to run one command each reported "no shell reachable in that run" with
// no tool call between them, because the tools were not in their loadouts at
// all.
func TestAMemberThatReachesTheirComputerIsRecognised(t *testing.T) {
	// Every tool that runs on somebody's machine, one at a time, so a tool
	// added to that package cannot be missed here.
	for _, held := range machine.Tools(nil) {
		loadout := tool.Loadout{Schemas: []tool.Schema{
			{Name: "http_request"},
			{Name: held.Schema.Name},
		}}
		if !reachesTheirComputer(loadout) {
			t.Errorf("a member holding %q was not recognised as needing this computer", held.Schema.Name)
		}
	}
}

// A member that holds none of them runs anywhere, which is what a fleet is for.
func TestAMemberThatNeedsNoComputerGoesToTheQueue(t *testing.T) {
	loadout := tool.Loadout{Schemas: []tool.Schema{
		{Name: "http_request"},
		{Name: "brain"},
		{Name: "query_customers"},
	}}
	if reachesTheirComputer(loadout) {
		t.Fatalf("a member with no machine tool was kept out of the queue: %+v", loadout.Schemas)
	}
}

// An empty loadout is not a special case: nothing to run means nothing that
// needs a computer.
func TestAMemberWithNoToolsGoesToTheQueue(t *testing.T) {
	if reachesTheirComputer(tool.Loadout{}) {
		t.Fatal("a member with no tools at all was treated as needing this computer")
	}
}
