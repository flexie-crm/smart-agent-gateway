package run

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
)

// A conversation being compacted is held: nothing is said into it until the
// summary is written, and a compaction does not start under a running turn.
// Both are decided under the one lock a turn claims its lane with.

// While a conversation is held, a turn is refused before anything is recorded,
// and it runs once the lane is given back.
func TestAHeldConversationTakesNoTurn(t *testing.T) {
	fr := &fakeRunner{}
	m, _ := newSchedManager(fr)
	runs := m.store.Runs().(*schedRuns)

	release, err := m.Hold(1)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if !m.Holding(1) || m.Holding(2) {
		t.Fatal("holding one conversation is not reported as exactly that one")
	}
	if _, err := m.Start(context.Background(), turn(1, "while held")); !errors.Is(err, ErrHeld) {
		t.Fatalf("a turn in a held conversation answered %v, want ErrHeld", err)
	}
	if runs.id != 0 {
		t.Fatal("a run was recorded for a turn that was refused")
	}
	// Another conversation is not held by this one.
	if _, err := m.Start(context.Background(), turn(2, "elsewhere")); err != nil {
		t.Fatalf("a different conversation was refused: %v", err)
	}

	release()
	release() // giving it back twice is giving it back once
	if m.Holding(1) {
		t.Fatal("the lane was not given back")
	}
	if _, err := m.Start(context.Background(), turn(1, "after")); err != nil {
		t.Fatalf("a turn after the hold was refused: %v", err)
	}
	waitFor(t, func() bool { return len(fr.ran()) == 2 })
}

// A compaction does not start while a turn is answering, and a second one does
// not start while the first holds the lane.
func TestAHoldWaitsForNothingAndTakesNothingThatIsTaken(t *testing.T) {
	fr := &fakeRunner{gates: map[string]chan struct{}{"answering": make(chan struct{})}}
	m, _ := newSchedManager(fr)

	if _, err := m.Start(context.Background(), turn(1, "answering")); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, func() bool { return len(fr.ran()) == 1 })
	if _, err := m.Hold(1); !errors.Is(err, ErrBusy) {
		t.Fatalf("holding a conversation that is answering answered %v, want ErrBusy", err)
	}
	close(fr.gates["answering"])
	waitFor(t, func() bool { return !m.Answering(1) })

	release, err := m.Hold(1)
	if err != nil {
		t.Fatalf("hold once the turn ended: %v", err)
	}
	defer release()
	if _, err := m.Hold(1); !errors.Is(err, ErrHeld) {
		t.Fatalf("a second hold answered %v, want ErrHeld", err)
	}
}

// Work the server queued while the conversation was held (a background result
// to deliver) waits, and starts when the lane is given back.
func TestWhatQueuedBehindAHoldStartsWhenItEnds(t *testing.T) {
	fr := &fakeRunner{}
	m, _ := newSchedManager(fr)

	release, err := m.Hold(1)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	m.Schedule(turn(1, "completion"))
	time.Sleep(60 * time.Millisecond)
	if got := fr.ran(); len(got) != 0 {
		t.Fatalf("a queued turn ran while the conversation was held: %v", got)
	}
	release()
	waitFor(t, func() bool { g := fr.ran(); return len(g) == 1 && g[0] == "completion" })
}

// Nothing new is held on the way down.
func TestAQuiescingManagerHoldsNothing(t *testing.T) {
	m, _ := newSchedManager(&fakeRunner{})
	m.Quiesce()
	if _, err := m.Hold(1); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("hold while shutting down answered %v, want ErrShuttingDown", err)
	}
}

// measuring is a runner whose turn reports one measurement, the way the loop
// does, so the test can see where it lands.
type measuring struct{}

func (measuring) Run(_ context.Context, turn agent.Turn, _ *chat.Stream) error {
	if turn.ContextUsed != nil {
		turn.ContextUsed(&model.AIModel{ID: 42}, 1200, 300)
	}
	return nil
}

// Every turn started here reports how full its conversation is, tagged with
// whose conversation it is.
func TestEveryTurnReportsHowFullItsConversationIs(t *testing.T) {
	m, _ := newSchedManager(measuring{})
	var mu sync.Mutex
	var heard []int64
	m.OnContext(func(workspaceID, userID, sessionID int64, mdl *model.AIModel, chars, baseChars int) {
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, workspaceID, userID, sessionID, mdl.ID, int64(chars), int64(baseChars))
	})

	started := agent.Turn{WorkspaceID: 7, UserID: 8, SessionID: 9, Prompt: "hi"}
	if _, err := m.Start(context.Background(), started); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(heard) > 0 })
	mu.Lock()
	defer mu.Unlock()
	want := []int64{7, 8, 9, 42, 1200, 300}
	for i := range want {
		if heard[i] != want[i] {
			t.Fatalf("heard %v, want %v", heard, want)
		}
	}
}
