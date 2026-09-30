package app

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/events"
	"flexie.io/sag/internal/queue"
)

// How an agent's step reaches the listener: on the bus when the listener is in
// this process, over the queue from a worker, and the queue's reports put on the
// same bus. The real bus and the real in-process queue, no database.

// stepsHeard runs a bus and returns what it hears of agents' steps.
func stepsHeard(t *testing.T, a *App) <-chan events.AgentStepChanged {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Bus.Run(ctx)
	heard := make(chan events.AgentStepChanged, 16)
	a.Bus.Subscribe(events.KindAgentStepChanged, func(e events.Event) {
		if moved, ok := e.(events.AgentStepChanged); ok {
			heard <- moved
		}
	})
	return heard
}

func expectStep(t *testing.T, heard <-chan events.AgentStepChanged, want events.AgentStepChanged) {
	t.Helper()
	select {
	case got := <-heard:
		if got != want {
			t.Fatalf("heard %+v, want %+v", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("never heard %+v", want)
	}
}

func expectNothing(t *testing.T, heard <-chan events.AgentStepChanged) {
	t.Helper()
	select {
	case got := <-heard:
		t.Fatalf("heard %+v, expected nothing", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// A worker's agent tells of its step over the queue, and the listening process
// puts it on its bus exactly as it was told.
func TestAWorkersStepReachesTheBusOverTheQueue(t *testing.T) {
	q := queue.NewInProcess()
	t.Cleanup(func() { _ = q.Close() })
	listening := &App{Log: zerolog.Nop(), Queue: q, Bus: events.New(zerolog.Nop())}
	heard := stepsHeard(t, listening)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go listening.readAgentReports(ctx)

	// The worker is another App on the same queue, with no listener of its own:
	// its bus hears nothing, so what arrives came over the queue.
	worker := &App{Log: zerolog.Nop(), Queue: q, Bus: events.New(zerolog.Nop())}
	// A fleet member, so the batch it is one of travels with the step: that is
	// what lets the listener move the batch's totals too.
	tell := worker.tellSteps(1, 2, 3, 4, 7)
	want := events.AgentStepChanged{WorkspaceID: 1, UserID: 2, SessionID: 3, DelegationID: 4, StepID: 5, FleetID: 7}

	// The reader subscribes on its own goroutine; an in-process queue drops what
	// nobody is reading yet, so the worker tells until the reader is there.
	deadline := time.Now().Add(3 * time.Second)
	for {
		tell(5)
		select {
		case got := <-heard:
			if got != want {
				t.Fatalf("heard %+v, want %+v", got, want)
			}
			return
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("a worker's step never reached the bus")
		}
	}
}

// Beside the listener an agent tells the bus directly and never the queue: in
// the desktop the worker loop runs inside this process, and its queue is a
// channel that drops a message whenever the reader is busy.
func TestBesideTheListenerAStepGoesStraightToTheBus(t *testing.T) {
	q := queue.NewInProcess()
	t.Cleanup(func() { _ = q.Close() })
	a := &App{Log: zerolog.Nop(), Queue: q, Bus: events.New(zerolog.Nop())}
	heard := stepsHeard(t, a)

	// Not watching yet: the step goes to the queue, which nobody reads here,
	// so the bus hears nothing. This is the control for the half below.
	a.tellSteps(1, 2, 3, 4, 0)(5)
	expectNothing(t, heard)

	a.watchingAgents.Store(true)
	a.tellSteps(1, 2, 3, 4, 0)(6)
	expectStep(t, heard, events.AgentStepChanged{WorkspaceID: 1, UserID: 2, SessionID: 3, DelegationID: 4, StepID: 6})
}
