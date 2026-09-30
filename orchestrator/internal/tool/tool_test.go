package tool

import "testing"

// An owner says which running agent it is, and the Gateway's says it is not one.
//
// It is what a tool keeping state on the person's computer files that state
// under: one terminal set per agent, so one cannot type into another's program.
// Every agent gets one, in every mode, because ResolveAgent mints one per
// running agent rather than per delegation row.
func TestAnOwnerNamesOneRunningAgent(t *testing.T) {
	first, second := OwnerOfAgent(), OwnerOfAgent()

	a, ok := first.Instance()
	if !ok || a <= 0 {
		t.Fatalf("an agent's owner did not name an agent: %q -> %d, %v", first, a, ok)
	}
	b, ok := second.Instance()
	if !ok {
		t.Fatalf("the second agent's owner did not name an agent: %q", second)
	}
	if a == b {
		t.Fatalf("two running agents were minted the same identity (%d): they would share a terminal", a)
	}

	// The Gateway's owner is a conversation, and must not read as an agent: its
	// terminals are the conversation's, which is what makes the shell theirs.
	if n, isAgent := OwnerOfSession(42).Instance(); isAgent {
		t.Fatalf("the Gateway's owner read as agent %d", n)
	}
	// Nor does a loadout built to be read rather than called.
	if _, isAgent := OwnerNone.Instance(); isAgent {
		t.Fatal("an unowned loadout read as an agent")
	}
}
