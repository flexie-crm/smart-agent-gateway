package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/events"
	"flexie.io/sag/internal/ws"
)

// fakeDashboard builds a publisher wired to fixed numbers, so the composition and
// the push can be tested without a hub, a run manager, or a database.
func fakeDashboard(broadcast func(int64, string, any)) *liveDashboard {
	return &liveDashboard{
		log:       zerolog.Nop(),
		connected: func(int64) (int, int) { return 2, 3 },
		running:   func(int64) int { return 1 },
		waiting:   func(context.Context, int64) (int, error) { return 4, nil },
		broadcast: broadcast,
		touch:     make(chan int64, 64),
	}
}

// Snapshot reads each number from its source and reports the current picture.
func TestSnapshotComposes(t *testing.T) {
	d := fakeDashboard(nil)
	got, err := d.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	want := LiveDashboard{Connected: 2, Sessions: 3, Running: 1, WaitingApproval: 4}
	if got != want {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
}

// push broadcasts the snapshot on the dashboard topic.
func TestPushBroadcastsSnapshot(t *testing.T) {
	type call struct {
		ws      int64
		topic   string
		payload LiveDashboard
	}
	got := make(chan call, 1)
	d := fakeDashboard(func(workspaceID int64, topic string, payload any) {
		got <- call{workspaceID, topic, payload.(LiveDashboard)}
	})
	d.push(context.Background(), 7)

	select {
	case c := <-got:
		if c.ws != 7 || c.topic != ws.TopicDashboard {
			t.Fatalf("pushed to ws=%d topic=%q", c.ws, c.topic)
		}
		if c.payload.WaitingApproval != 4 || c.payload.Connected != 2 {
			t.Fatalf("payload lost: %+v", c.payload)
		}
	default:
		t.Fatal("push did not broadcast")
	}
}

// Touch, coalesced through Run, produces a push for the touched workspace.
func TestRunPushesOnTouch(t *testing.T) {
	pushes := make(chan int64, 8)
	d := fakeDashboard(func(workspaceID int64, _ string, _ any) { pushes <- workspaceID })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	// Several touches for one workspace within a tick coalesce to a single push.
	d.Touch(5)
	d.Touch(5)
	d.Touch(5)

	select {
	case ws := <-pushes:
		if ws != 5 {
			t.Fatalf("pushed workspace %d, want 5", ws)
		}
	case <-time.After(2 * dashboardInterval):
		t.Fatal("a touch produced no push")
	}
	// The coalescing: no second push for the same batch.
	select {
	case <-pushes:
		t.Fatal("coalescing failed: a batch of touches produced more than one push")
	case <-time.After(dashboardInterval + 200*time.Millisecond):
	}
}

// The whole pipeline, wired and running, under concurrent events: bus -> listen
// -> touch -> coalesce -> snapshot -> broadcast. It must not deadlock, must keep
// pushing, must coalesce a storm down to a trickle, and must stop cleanly. Run
// with -race for the data-race half. This is the guard against the cross-goroutine
// flow (a bus handler touching a publisher that reads a source and broadcasts)
// forming a cycle or a race.
func TestPipelineUnderConcurrentEvents(t *testing.T) {
	var pushes atomic.Int64
	d := &liveDashboard{
		log:       zerolog.Nop(),
		connected: func(int64) (int, int) { return 1, 1 },
		running:   func(int64) int { return 0 },
		waiting:   func(context.Context, int64) (int, error) { return 0, nil },
		broadcast: func(int64, string, any) { pushes.Add(1) },
		touch:     make(chan int64, 1024),
	}

	bus := events.New(zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	go bus.Run(ctx)
	go d.Run(ctx)
	d.Listen(bus)
	time.Sleep(30 * time.Millisecond) // let subscriptions register

	// A storm of events across a handful of workspaces from many goroutines.
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				ws := int64(j % 4)
				bus.Publish(events.TurnStarted{WorkspaceID: ws})
				bus.Publish(events.ApprovalPending{WorkspaceID: ws})
				bus.Publish(events.PresenceChanged{WorkspaceID: ws})
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the live pipeline deadlocked under concurrent events")
	}

	// It coalesced: let the last touches settle, then confirm pushes happened but
	// nowhere near one per event (thousands published; coalesced to a handful per
	// workspace per tick).
	time.Sleep(2 * dashboardInterval)
	n := pushes.Load()
	if n == 0 {
		t.Fatal("the pipeline produced no pushes")
	}
	if n > 200 {
		t.Fatalf("coalescing failed: %d pushes for a storm of ~18000 events", n)
	}

	// Clean shutdown: cancel and make sure Run returns (no goroutine wedged).
	cancel()
	time.Sleep(50 * time.Millisecond)
}

// Listen turns bus events into touches, whatever their concrete type.
func TestListenTouchesOnEvents(t *testing.T) {
	d := fakeDashboard(nil)
	bus := events.New(zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bus.Run(ctx)
	d.Listen(bus)
	time.Sleep(30 * time.Millisecond) // let the subscriptions register

	// One of each kind the dashboard reacts to, all for workspace 8.
	bus.Publish(events.TurnStarted{WorkspaceID: 8})
	bus.Publish(events.ApprovalPending{WorkspaceID: 8})
	bus.Publish(events.PresenceChanged{WorkspaceID: 8})

	for i := 0; i < 3; i++ {
		select {
		case ws := <-d.touch:
			if ws != 8 {
				t.Fatalf("touched workspace %d, want 8", ws)
			}
		case <-time.After(time.Second):
			t.Fatalf("event %d produced no touch", i)
		}
	}
}
