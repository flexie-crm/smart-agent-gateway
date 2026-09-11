package events

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// run starts a bus and returns it with a cancel that stops its goroutine.
func run(t *testing.T) (*Bus, func()) {
	t.Helper()
	b := New(zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)
	return b, cancel
}

// A subscriber receives the events of its kind, and only those.
func TestDeliversToKind(t *testing.T) {
	b, cancel := run(t)
	defer cancel()

	got := make(chan int64, 4)
	b.Subscribe(KindTurnStarted, func(e Event) {
		got <- e.(TurnStarted).WorkspaceID
	})
	// A subscriber to a DIFFERENT kind must not hear this.
	other := make(chan Event, 4)
	b.Subscribe(KindTurnEnded, func(e Event) { other <- e })

	// Give the subscriptions a moment to register on the dispatch goroutine.
	time.Sleep(20 * time.Millisecond)
	b.Publish(TurnStarted{WorkspaceID: 7})

	select {
	case ws := <-got:
		if ws != 7 {
			t.Fatalf("workspace = %d, want 7", ws)
		}
	case <-time.After(time.Second):
		t.Fatal("the matching subscriber never received the event")
	}
	select {
	case <-other:
		t.Fatal("a subscriber to a different kind received the event")
	case <-time.After(50 * time.Millisecond):
	}
}

// Every subscriber to a kind hears every event of that kind.
func TestFanOut(t *testing.T) {
	b, cancel := run(t)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(3)
	for i := 0; i < 3; i++ {
		b.Subscribe(KindApprovalPending, func(Event) { wg.Done() })
	}
	time.Sleep(20 * time.Millisecond)
	b.Publish(ApprovalPending{WorkspaceID: 1})

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("not every subscriber received the event")
	}
}

// A cancelled subscriber stops receiving.
func TestUnsubscribe(t *testing.T) {
	b, cancel := run(t)
	defer cancel()

	hits := make(chan struct{}, 8)
	stop := b.Subscribe(KindPresenceChanged, func(Event) { hits <- struct{}{} })
	time.Sleep(20 * time.Millisecond)

	b.Publish(PresenceChanged{WorkspaceID: 1})
	select {
	case <-hits:
	case <-time.After(time.Second):
		t.Fatal("the subscriber did not receive the first event")
	}

	stop()
	time.Sleep(20 * time.Millisecond)
	b.Publish(PresenceChanged{WorkspaceID: 1})
	select {
	case <-hits:
		t.Fatal("a cancelled subscriber still received an event")
	case <-time.After(50 * time.Millisecond):
	}
}

// Publishers, subscribers, and unsubscribers all hammering at once must not
// race or deadlock: the test simply has to COMPLETE (run with -race for the
// data-race half). It is the guard against the bus's lock-free dispatch and its
// subscribe/unsubscribe churn tripping over each other.
func TestConcurrentPublishSubscribeUnsubscribe(t *testing.T) {
	b, cancel := run(t)
	defer cancel()

	var wg sync.WaitGroup

	// A steady subscriber so there is always something to deliver to.
	b.Subscribe(KindTurnStarted, func(Event) {})

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				b.Publish(TurnStarted{WorkspaceID: int64(j)})
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				stop := b.Subscribe(KindTurnStarted, func(Event) {})
				stop()
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent publish/subscribe/unsubscribe deadlocked")
	}
}

// A slow subscriber drops its own overflow instead of blocking the publisher or
// starving a fast subscriber alongside it.
func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	b, cancel := run(t)
	defer cancel()

	// One handler blocks forever (until the test ends): once its buffer fills,
	// further events to it drop rather than back up the bus.
	block := make(chan struct{})
	defer close(block)
	b.Subscribe(KindTurnStarted, func(Event) { <-block })

	// A fast handler alongside it, with room for everything.
	const n = subscriberBuffer + 500
	fast := make(chan struct{}, n)
	b.Subscribe(KindTurnStarted, func(Event) { fast <- struct{}{} })
	time.Sleep(20 * time.Millisecond)

	// Publish more than the slow subscriber can buffer. Publish must never block.
	for i := 0; i < n; i++ {
		b.Publish(TurnStarted{WorkspaceID: 1})
	}

	// The fast subscriber keeps receiving alongside the blocked one rather than
	// being starved by it.
	//
	// Its own buffer is filled before anything of its can be dropped, so a whole
	// buffer's worth arrives however the two goroutines happen to be scheduled.
	// Past that the bus drops by design (the send above is non-blocking for every
	// subscriber, not only the slow one), and a fast handler can lose an event
	// purely because its goroutine has not been given the processor yet. Asking
	// for all of them here would be asserting a promise the bus does not make:
	// with one processor it does not hold, and it does not need to, because the
	// one thing subscribed to this treats every event as "something changed" and
	// rebuilds its picture whole.
	deadline := time.After(2 * time.Second)
	for i := 0; i < subscriberBuffer; i++ {
		select {
		case <-fast:
		case <-deadline:
			t.Fatalf("the fast subscriber was starved by the slow one at %d", i)
		}
	}
}
