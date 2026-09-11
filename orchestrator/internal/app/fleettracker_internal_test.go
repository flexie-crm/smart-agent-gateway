package app

import (
	"sync"
	"testing"
	"time"

	"flexie.io/sag/internal/state"

	"flexie.io/sag/internal/model"
)

// The master's own picture of a batch it dispatched.
//
// Everything here is about one thing: hearing "one more is back" must cost
// nothing and must be exact. Nothing below touches a database, because the
// point of it is that the live path does not.

func track(size int) *fleetTracker {
	t := newFleetTracker(state.NewMemory())
	t.watch(&fleetTrack{FleetID: 7, WorkspaceID: 1, UserID: 2, SessionUID: "ch_x",
		Name: "3 × Research", Size: size}, nil)
	return t
}

func TestOnlyTheLastReportSaysTheBatchIsComplete(t *testing.T) {
	tr := track(3)
	for i, del := range []int64{10, 11} {
		snap, known, complete := tr.record(7, del)
		if !known {
			t.Fatalf("report %d: the batch was not being followed", i)
		}
		if complete {
			t.Fatalf("report %d said the batch was complete with %d of 3 back", i, snap.Done)
		}
		if snap.Done != i+1 {
			t.Fatalf("report %d counted %d", i, snap.Done)
		}
	}
	snap, _, complete := tr.record(7, 12)
	if !complete || snap.Done != 3 {
		t.Fatalf("the last report should complete the batch: done=%d complete=%v", snap.Done, complete)
	}
}

// Delivery is at-least-once, so the same member can report twice. It counts
// once, or a chip reads "4 of 3" and the batch is finished by a duplicate.
func TestAMemberCountsOnceHoweverOftenItReports(t *testing.T) {
	tr := track(3)
	tr.record(7, 10)
	snap, _, complete := tr.record(7, 10)
	if snap.Done != 1 || complete {
		t.Fatalf("a repeated report was counted twice: done=%d complete=%v", snap.Done, complete)
	}
}

// Exactly ONE caller is told the batch is complete, however many arrive at once.
// It is the one that reads the database and wakes the Gateway, and two of them
// would narrate the same batch twice.
func TestExactlyOneCallerCompletesABatch(t *testing.T) {
	tr := track(8)
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		completes int
		start     = make(chan struct{})
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(del int64) {
			defer wg.Done()
			<-start
			if _, _, complete := tr.record(7, del); complete {
				mu.Lock()
				completes++
				mu.Unlock()
			}
		}(int64(100 + i))
	}
	close(start)
	wg.Wait()

	if completes != 1 {
		t.Fatalf("eight members finishing together completed the batch %d times", completes)
	}
}

// A batch this process never dispatched is not tracked, and the caller is told
// so rather than being handed a zero: it falls back to the database, which is
// slower and always right.
func TestAnUnknownBatchIsSaidToBeUnknown(t *testing.T) {
	tr := track(3)
	if _, known, _ := tr.record(99, 10); known {
		t.Fatal("a batch nobody dispatched here was reported as tracked")
	}
	tr.forget(7)
	if _, known, _ := tr.record(7, 10); known {
		t.Fatal("a forgotten batch is still being followed")
	}
}

// The chip is built from memory alone, which is what lets a report move it
// without a query.
func TestTheChipIsBuiltWithoutReadingAnything(t *testing.T) {
	tr := track(3)
	snap, _, _ := tr.record(7, 10)
	chip := snap.chip(model.DelegationRunning)

	if chip.Kind != ChipKindFleet || chip.ID != FleetChipID(7) {
		t.Fatalf("the chip does not identify the batch: %+v", chip)
	}
	if chip.Agents != 3 || chip.Done != 1 {
		t.Fatalf("the chip counts wrong: %d of %d", chip.Done, chip.Agents)
	}
	if chip.ChatUID != "ch_x" || chip.Name != "3 × Research" {
		t.Fatalf("the chip lost what it was told at dispatch: %+v", chip)
	}
}

// A batch stops waiting when its agents' own time is up, and not on somebody
// else's schedule. The clock belongs to the batch, and it is stopped the moment
// the batch is home, so nothing gives up on work that finished.
func TestABatchGivesUpWhenItsOwnTimeIsUp(t *testing.T) {
	tr := newFleetTracker(state.NewMemory())
	fired := make(chan int64, 4)
	tr.watch(&fleetTrack{FleetID: 7, Size: 2, Deadline: time.Now().Add(40 * time.Millisecond)},
		func(id int64) { fired <- id })

	select {
	case id := <-fired:
		if id != 7 {
			t.Fatalf("gave up on the wrong batch: %d", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a batch past its deadline was never given up on")
	}
	// And exactly one caller may act on it, so the timer and a late report
	// cannot both close the same batch.
	if !tr.expire(7) {
		t.Fatal("the expiry could not claim the batch it fired for")
	}
	if tr.expire(7) {
		t.Fatal("a batch was claimed twice")
	}
}

func TestABatchThatFinishesIsNotGivenUpOn(t *testing.T) {
	tr := newFleetTracker(state.NewMemory())
	fired := make(chan int64, 4)
	tr.watch(&fleetTrack{FleetID: 8, Size: 1, Deadline: time.Now().Add(40 * time.Millisecond)},
		func(id int64) { fired <- id })

	if _, _, complete := tr.record(8, 100); !complete {
		t.Fatal("the only member did not complete the batch")
	}
	select {
	case <-fired:
		t.Fatal("a finished batch was given up on by its own clock")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestACancelledBatchIsNotGivenUpOn(t *testing.T) {
	tr := newFleetTracker(state.NewMemory())
	fired := make(chan int64, 4)
	tr.watch(&fleetTrack{FleetID: 9, Size: 3, Deadline: time.Now().Add(40 * time.Millisecond)},
		func(id int64) { fired <- id })
	tr.forget(9)

	select {
	case <-fired:
		t.Fatal("a cancelled batch was given up on by a clock nobody stopped")
	case <-time.After(300 * time.Millisecond):
	}
}
