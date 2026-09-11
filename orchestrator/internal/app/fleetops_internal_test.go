package app

import (
	"context"
	"sync"
	"testing"
)

// What a worker holds of a batch, so it can be told to stop.
//
// The registry is the whole of the cross-process cancel on the worker's side: a
// broadcast arrives naming a fleet, and this is what turns that number into the
// goroutines to unwind. Every case here is one that would otherwise show up as
// an agent that kept spending money after somebody pressed stop.

func TestAWorkerStopsOnlyTheBatchItWasTold(t *testing.T) {
	runs := newFleetRuns()
	stopped := map[int64]bool{}
	var mu sync.Mutex
	mark := func(id int64) context.CancelFunc {
		return func() {
			mu.Lock()
			defer mu.Unlock()
			stopped[id] = true
		}
	}

	// Two members of one batch and one of another, which is the ordinary state
	// of a worker: it holds a slice of several fleets, not a whole one.
	release1 := runs.add(7, 100, mark(100))
	release2 := runs.add(7, 101, mark(101))
	defer runs.add(9, 200, mark(200))()

	if n := runs.cancel(7); n != 2 {
		t.Fatalf("stopped %d members of the batch, want 2", n)
	}
	mu.Lock()
	if !stopped[100] || !stopped[101] {
		t.Fatalf("a member of the cancelled batch kept running: %+v", stopped)
	}
	if stopped[200] {
		t.Fatal("cancelling one batch stopped a member of another")
	}
	mu.Unlock()

	release1()
	release2()
	// Cancelling again finds nothing: the members are gone from the registry, so
	// a second instruction is not a second attempt to unwind the same goroutine.
	if n := runs.cancel(7); n != 0 {
		t.Fatalf("a cancelled batch still claims %d members", n)
	}
}

// A batch this process is running none of is the ordinary case: twenty workers
// and a fleet of three means seventeen of them do nothing.
func TestAWorkerWithNoneOfTheBatchDoesNothing(t *testing.T) {
	runs := newFleetRuns()
	defer runs.add(1, 10, func() {})()

	if n := runs.cancel(99); n != 0 {
		t.Fatalf("a worker holding none of the batch stopped %d things", n)
	}
}

// Releasing the last member takes the batch's entry with it, or a long-lived
// worker accumulates one empty map per fleet it has ever touched.
func TestTheRegistryDoesNotGrowForever(t *testing.T) {
	runs := newFleetRuns()
	for i := int64(1); i <= 50; i++ {
		release := runs.add(i, i*10, func() {})
		release()
	}
	runs.mu.Lock()
	defer runs.mu.Unlock()
	if len(runs.running) != 0 {
		t.Fatalf("fifty finished batches left %d entries behind", len(runs.running))
	}
}

// The cancels run OUTSIDE the lock. A goroutine unwinding takes the same lock
// on its way out to release itself, so cancelling while holding it would be
// waiting for the thing just stopped. This deadlocks rather than fails if that
// ever changes.
func TestCancellingDoesNotDeadlockAgainstARelease(t *testing.T) {
	runs := newFleetRuns()
	done := make(chan struct{})

	var release func()
	release = runs.add(3, 30, func() {
		// Exactly what a real member does when its context is cancelled.
		release()
		close(done)
	})

	runs.cancel(3)
	<-done
}
