package app

import (
	"context"
	"sync"
	"testing"

	"flexie.io/sag/internal/state"
)

// What somebody says while their conversation is answering must not be lost, and
// must not be said twice. It is the one thing this exists to guarantee.
func TestWhatWasSaidIsKeptInOrderAndTakenOnce(t *testing.T) {
	ctx := context.Background()
	said := newSaid(state.NewMemory())

	if got := said.Take(ctx, 1); got != nil {
		t.Fatalf("nothing was said, got %v", got)
	}

	for _, text := range []string{"first", "second", "third"} {
		if err := said.Add(ctx, 1, text); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	// Another conversation's, which must not come back with this one's.
	if err := said.Add(ctx, 2, "somewhere else"); err != nil {
		t.Fatalf("add: %v", err)
	}

	got := said.Take(ctx, 1)
	if len(got) != 3 || got[0] != "first" || got[2] != "third" {
		t.Fatalf("said in the wrong order or lost: %v", got)
	}
	// Taken means taken. Handing the same words to a second step would put them
	// into the conversation twice, which reads as the person repeating
	// themselves.
	if again := said.Take(ctx, 1); again != nil {
		t.Fatalf("the same words came back twice: %v", again)
	}
	if other := said.Take(ctx, 2); len(other) != 1 || other[0] != "somewhere else" {
		t.Fatalf("a conversation was given another one's words: %v", other)
	}
}

// Typing fast is two requests at once. Without the lock they read the same list
// and write back over each other, and a message vanishes with nothing to show
// for it.
func TestNothingIsLostWhenTwoThingsAreSaidAtOnce(t *testing.T) {
	ctx := context.Background()
	said := newSaid(state.NewMemory())

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if err := said.Add(ctx, 7, string(rune('a'+n%26))); err != nil {
				t.Errorf("add: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if got := said.Take(ctx, 7); len(got) != 50 {
		t.Fatalf("said 50 things, %d survived", len(got))
	}
}
