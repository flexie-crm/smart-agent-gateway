package run

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
)

// A turn is ended by silence, not by its length: anything written to its
// stream counts as a sign of life, and only a turn that shows none for the
// whole window is ended.

// statusRuns is schedRuns that keeps the status a finished turn was recorded
// with, which is the only place the outcome is written down (settle).
type statusRuns struct {
	schedRuns
	mu     sync.Mutex
	status string
}

func (r *statusRuns) SetStatus(_ context.Context, _ int64, status string) error {
	r.mu.Lock()
	r.status = status
	r.mu.Unlock()
	return nil
}

func (r *statusRuns) recorded() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// shortWait makes the silence window 100ms and looks every 10ms.
func shortWait(t *testing.T) {
	t.Helper()
	window, look := idleTimeout, idleCheckEvery
	idleTimeout, idleCheckEvery = 100*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { idleTimeout, idleCheckEvery = window, look })
}

// signaller is a turn that writes one kind of frame every so often until it
// has run for lasts, and reports how it ended.
type signaller struct {
	every, lasts time.Duration
	sign         func(*chat.Stream) error
	finished     chan error
}

func (s *signaller) Run(ctx context.Context, _ agent.Turn, out *chat.Stream) error {
	over := time.After(s.lasts)
	ticker := time.NewTicker(s.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.finished <- ctx.Err()
			return ctx.Err()
		case <-over:
			s.finished <- nil
			return nil
		case <-ticker.C:
			if s.sign != nil {
				_ = s.sign(out)
			}
		}
	}
}

// runTurn starts a turn, optionally stops it, and answers why its runner
// returned and what the turn was recorded as.
func runTurn(t *testing.T, sr *signaller, show *chat.Show, stop bool) (error, string) {
	t.Helper()
	runs := &statusRuns{}
	m := NewManager(&schedStore{runs: runs, agent: &schedAgent{}}, sr, nil, zerolog.Nop())
	r, err := m.Start(context.Background(), agent.Turn{SessionID: 1, Prompt: "work", Show: show})
	if err != nil {
		t.Fatalf("the turn would not start: %v", err)
	}
	if stop {
		r.Cancel()
	}
	var cause error
	select {
	case cause = <-sr.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never came back")
	}
	waitFor(t, r.Done)
	waitFor(t, func() bool { return runs.recorded() != "" })
	return cause, runs.recorded()
}

// keepsGoing is a turn signing every 30ms for 400ms, four times the window.
func keepsGoing(t *testing.T, show *chat.Show, sign func(*chat.Stream) error) {
	t.Helper()
	shortWait(t)
	sr := &signaller{every: 30 * time.Millisecond, lasts: 400 * time.Millisecond, sign: sign, finished: make(chan error, 1)}
	if cause, status := runTurn(t, sr, show, false); cause != nil || status != model.RunCompleted {
		t.Fatalf("a working turn was ended: %v, recorded %q", cause, status)
	}
}

func TestAStreamingModelIsNotEnded(t *testing.T) {
	keepsGoing(t, nil, func(out *chat.Stream) error { return out.Delta("word ") })
}

// A running tool sends only its heartbeat.
func TestARunningToolIsNotEnded(t *testing.T) {
	keepsGoing(t, nil, func(out *chat.Stream) error { return out.Heartbeat() })
}

// Thinking counts even when this person may not see it, which is why the
// silence is read from the stream and not from what reaches the run.
func TestAThinkingModelIsNotEndedWhenItsThinkingIsWithheld(t *testing.T) {
	keepsGoing(t, &chat.Show{Reasoning: false, Tools: true},
		func(out *chat.Stream) error { return out.ReasoningDelta("thinking ") })
}

func TestASilentTurnIsEnded(t *testing.T) {
	shortWait(t)
	sr := &signaller{every: time.Hour, lasts: 30 * time.Second, finished: make(chan error, 1)}
	started := time.Now()
	cause, status := runTurn(t, sr, nil, false)
	if cause != context.Canceled || status != model.RunCancelled {
		t.Fatalf("a silent turn ended with %v, recorded %q", cause, status)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("the window did not bite: %v", took)
	}
}

// With no deadline on the run, a lost cancel would leave stop doing nothing.
func TestStoppingATurnEndsIt(t *testing.T) {
	sr := &signaller{every: time.Hour, lasts: 30 * time.Second, finished: make(chan error, 1)}
	if cause, status := runTurn(t, sr, nil, true); cause != context.Canceled || status != model.RunCancelled {
		t.Fatalf("a stopped turn ended with %v, recorded %q", cause, status)
	}
}
