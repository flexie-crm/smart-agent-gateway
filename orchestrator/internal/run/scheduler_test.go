package run

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// The scheduler is the seam that lets the server start a turn on its own when a
// background delegation finishes (KB/27). These prove the rules a completion
// must obey: it waits behind a running user turn, several completions run one at
// a time in order, and none delivers while a confirmation card is up.

// fakeRunner records which turns ran, in order, and blocks a named turn until it
// is released, so a test can hold the lane and watch what waits.
type fakeRunner struct {
	mu    sync.Mutex
	order []string
	gates map[string]chan struct{}
}

func (f *fakeRunner) Run(_ context.Context, turn agent.Turn, _ *chat.Stream) error {
	f.mu.Lock()
	f.order = append(f.order, turn.Prompt)
	g := f.gates[turn.Prompt]
	f.mu.Unlock()
	if g != nil {
		<-g
	}
	return nil
}

func (f *fakeRunner) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

// schedStore is the little the Manager needs: create/close a run row, and read
// whether a card is up (the guard). It models the guard the way the Manager asks
// it, which is by looking for a PENDING PARK rather than by reading the session's
// status column. Those are different questions: the day they disagreed, a
// conversation whose status was left saying `waiting_approval` with nothing
// pending behind it never delivered another turn (KB/29).
type schedStore struct {
	store.Store
	runs  *schedRuns
	agent *schedAgent
}

func (s *schedStore) Runs() store.RunStore    { return s.runs }
func (s *schedStore) Agent() store.AgentStore { return s.agent }

type schedRuns struct {
	store.RunStore
	mu sync.Mutex
	id int64
}

func (r *schedRuns) Create(_ context.Context, rec *model.Run) error {
	r.mu.Lock()
	r.id++
	rec.ID = r.id
	r.mu.Unlock()
	return nil
}
func (r *schedRuns) SetStatus(context.Context, int64, string) error { return nil }

type schedAgent struct {
	store.AgentStore
	mu     sync.Mutex
	status string
	parked bool
}

func (a *schedAgent) setStatus(s string) {
	a.mu.Lock()
	a.status = s
	a.mu.Unlock()
}

// setCard puts a real card up, or takes it down.
func (a *schedAgent) setCard(up bool) {
	a.mu.Lock()
	a.parked = up
	a.mu.Unlock()
}

func (a *schedAgent) PendingPark(context.Context, int64) (*model.ParkSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.parked {
		return nil, store.ErrNotFound
	}
	return &model.ParkSnapshot{ID: 1}, nil
}

func (a *schedAgent) GetSession(_ context.Context, ws, id int64) (*model.AgentSession, error) {
	a.mu.Lock()
	st := a.status
	a.mu.Unlock()
	if st == "" {
		st = model.SessionRunning
	}
	return &model.AgentSession{ID: id, WorkspaceID: ws, Status: st}, nil
}
func (a *schedAgent) SetSessionStatus(context.Context, int64, string) error { return nil }

func newSchedManager(fr Runner) (*Manager, *schedAgent) {
	ag := &schedAgent{}
	return NewManager(&schedStore{runs: &schedRuns{}, agent: ag}, fr, nil, zerolog.Nop()), ag
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func turn(session int64, label string) agent.Turn {
	return agent.Turn{SessionID: session, WorkspaceID: 1, UserID: 1, Prompt: label}
}

// A completion waits behind a running user turn and never barges it.
func TestAScheduledTurnWaitsBehindARunningTurn(t *testing.T) {
	fr := &fakeRunner{gates: map[string]chan struct{}{"user": make(chan struct{})}}
	m, _ := newSchedManager(fr)

	if _, err := m.Start(context.Background(), turn(1, "user")); err != nil {
		t.Fatalf("start user turn: %v", err)
	}
	waitFor(t, func() bool { return len(fr.ran()) == 1 })

	// Scheduled while the lane is busy: it must not run.
	m.Schedule(turn(1, "completion"))
	time.Sleep(60 * time.Millisecond)
	if got := fr.ran(); len(got) != 1 {
		t.Fatalf("the completion barged the running turn: %v", got)
	}

	// The user turn ends; the completion runs after it.
	close(fr.gates["user"])
	waitFor(t, func() bool { g := fr.ran(); return len(g) == 2 && g[1] == "completion" })
}

// Several completions run ONE at a time, in the order scheduled.
func TestScheduledTurnsRunOneAtATimeInOrder(t *testing.T) {
	fr := &fakeRunner{gates: map[string]chan struct{}{"A": make(chan struct{}), "B": make(chan struct{})}}
	m, _ := newSchedManager(fr)

	m.Schedule(turn(1, "A"))
	m.Schedule(turn(1, "B"))

	// A runs first and blocks; B must wait.
	waitFor(t, func() bool { return len(fr.ran()) == 1 })
	time.Sleep(60 * time.Millisecond)
	if got := fr.ran(); len(got) != 1 || got[0] != "A" {
		t.Fatalf("completions did not serialize in order: %v", got)
	}
	// A ends; B runs.
	close(fr.gates["A"])
	waitFor(t, func() bool { g := fr.ran(); return len(g) == 2 && g[0] == "A" && g[1] == "B" })
	close(fr.gates["B"])
}

// A completion does not deliver while a confirmation card is on screen; it waits
// until the person answers and the lane frees.
func TestAScheduledTurnHoldsWhileACardIsUp(t *testing.T) {
	fr := &fakeRunner{}
	m, ag := newSchedManager(fr)
	ag.setCard(true)
	ag.setStatus(model.SessionWaitingApproval)

	m.Schedule(turn(1, "completion"))
	time.Sleep(60 * time.Millisecond)
	if got := fr.ran(); len(got) != 0 {
		t.Fatalf("a completion ran while a card was up: %v", got)
	}

	// The person answers: the card is gone, and the resume turn finishing drains
	// the queue.
	ag.setCard(false)
	ag.setStatus(model.SessionRunning)
	if _, err := m.Start(context.Background(), turn(1, "resume")); err != nil {
		t.Fatalf("start resume: %v", err)
	}
	waitFor(t, func() bool {
		for _, s := range fr.ran() {
			if s == "completion" {
				return true
			}
		}
		return false
	})
}

// The stall this guard caused, and must not cause again (KB/29).
//
// A restart killed three parked background agents. Their parks were closed out
// later, but the session's status column was left reading `waiting_approval`.
// The guard read that column, so every completion turn queued from then on was
// held back, forever: there was no card anywhere for the person to answer, and
// therefore nothing that could ever clear it. The conversation went silent.
//
// The status is a cached opinion. The pending park is the fact.
func TestAStaleWaitingStatusDoesNotSilenceAConversation(t *testing.T) {
	fr := &fakeRunner{}
	m, ag := newSchedManager(fr)

	// Exactly the wreckage: the column says a card is up, and no card is.
	ag.setStatus(model.SessionWaitingApproval)
	ag.setCard(false)

	m.Schedule(turn(1, "completion"))
	waitFor(t, func() bool {
		for _, s := range fr.ran() {
			if s == "completion" {
				return true
			}
		}
		return false
	})
}

// Graceful shutdown means work already under way FINISHES.
//
// It used to mean the opposite: Shutdown cancelled every live turn immediately,
// so an ordinary deploy interrupted answers people were reading and left the
// ghost rows that boot recovery then had to clean up (KB/29). The order is now
// quiesce, drain, and only then cancel.
func TestShutdownLetsWorkUnderWayFinish(t *testing.T) {
	fr := &fakeRunner{gates: map[string]chan struct{}{"answering": make(chan struct{})}}
	m, _ := newSchedManager(fr)

	// A turn is in the middle of answering.
	if _, err := m.Start(context.Background(), turn(1, "answering")); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Wait for the RUNNER, not just for the registration: Start returns as soon
	// as the run is tracked, a moment before its goroutine reaches Run.
	waitFor(t, func() bool { g := fr.ran(); return len(g) == 1 && g[0] == "answering" })

	// The restart begins. Nothing new is accepted...
	m.Quiesce()
	if _, err := m.Start(context.Background(), turn(2, "after")); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("a new turn was accepted during shutdown: %v", err)
	}
	// ...and the one in flight is NOT touched.
	if m.Live() != 1 {
		t.Fatal("quiescing cancelled a turn that was still answering")
	}
	if got := fr.ran(); len(got) != 1 {
		t.Fatalf("the wrong set of turns ran: %v", got)
	}

	// Draining waits for it, and gives up rather than hanging when it will not
	// finish in the time we were willing to give it.
	impatient, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if m.DrainForShutdown(impatient) {
		t.Fatal("the drain claimed a turn had finished while it was still answering")
	}

	// It finishes; now the drain completes.
	close(fr.gates["answering"])
	patient, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if !m.DrainForShutdown(patient) {
		t.Fatal("the drain did not see the turn finish")
	}
}

// A turn queued for a session that is going away is dropped rather than started
// into a process that is leaving. Its delegation is still recorded, so the next
// process picks it up.
func TestQuiescingDropsWhatHadNotStarted(t *testing.T) {
	fr := &fakeRunner{}
	m, ag := newSchedManager(fr)
	ag.setCard(true) // a card holds the queue, so nothing drains it

	m.Schedule(turn(1, "queued"))
	m.Quiesce()
	ag.setCard(false)
	m.Drain(1)

	time.Sleep(80 * time.Millisecond)
	if got := fr.ran(); len(got) != 0 {
		t.Fatalf("a queued turn was started into a process that is shutting down: %v", got)
	}
}

// Nothing to wait for means no waiting.
//
// The grace period is a CEILING, not a delay: an idle process must go down at
// once. Pinned with an already-expired context, so a drain that consulted the
// clock before counting the work would fail here.
func TestDrainingAnIdleManagerIsInstant(t *testing.T) {
	m, _ := newSchedManager(&fakeRunner{})

	expired, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	if !m.DrainForShutdown(expired) {
		t.Fatal("an idle manager reported work still in flight")
	}
	if elapsed := time.Since(started); elapsed > 20*time.Millisecond {
		t.Fatalf("draining an idle manager waited %s", elapsed)
	}
}
