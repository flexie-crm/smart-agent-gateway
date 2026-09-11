// Package events is the in-process event bus: the one primitive by which one
// part of the system reacts to something another part did, without the two
// knowing about each other.
//
// A publisher emits a typed Event and moves on; any number of subscribers react
// on their own goroutines. It is DELIBERATELY best-effort and in-process:
//
//   - Best-effort. Delivery never blocks the publisher, and under sustained
//     overload a subscriber drops events rather than stalling the emitter. A
//     dropped live-dashboard refresh is corrected by the next event or a full
//     reload; anything that MUST happen (an audit row, a charge, a side effect
//     the product depends on) does NOT belong here. It belongs on the durable
//     queue (KB/05). The rule is the whole point: this bus carries reactions,
//     never obligations.
//   - In-process. One process, one bus. When cross-node reactions are needed the
//     same Publish/Subscribe surface is backed by the queue, and emitters and
//     subscribers do not change (KB/17, KB/05).
//   - For cross-cutting fan-out, not control flow. Use it to let independent
//     concerns (a dashboard, metrics, later audit and cost) observe the system.
//     Do NOT route essential sequencing through it: an event bus that carries
//     the steps a turn depends on makes the flow impossible to follow.
//
// The shape mirrors the WebSocket hub (KB/24): a single goroutine owns the
// subscriber set, so there are no locks, and each subscriber drains its own
// buffered channel, so one slow handler drops only its own events.
package events

import (
	"context"
	"sync/atomic"

	"github.com/rs/zerolog"
)

// Event is anything a part of the system announces. Kind groups events so a
// subscriber can ask for exactly the ones it reacts to; concrete events are
// small structs in eventtypes.go.
type Event interface {
	// Kind is the stable name a subscriber matches on. It is product vocabulary,
	// never a Go type name, so a rename of the struct does not silently unsubscribe
	// every listener.
	Kind() string
}

// Handler reacts to an event. It runs on the subscriber's own goroutine, one
// event at a time, so it may block on I/O without affecting the publisher or any
// other subscriber. It must not panic; a handler that can fail should log and
// return, because there is nobody to return an error to.
type Handler func(Event)

// subscriberBuffer is how many events a slow subscriber may fall behind before
// it starts dropping. Generous enough to ride out a burst, bounded so a stuck
// handler cannot grow memory without limit.
const subscriberBuffer = 256

// Bus is the dispatcher. One goroutine (Run) owns the subscriber set; Publish,
// Subscribe, and unsubscribe reach it over channels, so nothing is locked.
type Bus struct {
	log zerolog.Logger

	publish chan Event
	sub     chan *subscriber
	unsub   chan uint64
	done    chan struct{}

	nextID atomic.Uint64
}

type subscriber struct {
	id      uint64
	kind    string
	ch      chan Event
	handler Handler
}

// New builds a bus. Call Run to start dispatching.
func New(log zerolog.Logger) *Bus {
	return &Bus{
		log: log,
		// Buffered so a publisher is never blocked by the dispatcher being briefly
		// busy; a full buffer drops (Publish says so) rather than stalling a
		// request path, exactly like the hub's Notify.
		publish: make(chan Event, 1024),
		sub:     make(chan *subscriber),
		unsub:   make(chan uint64),
		done:    make(chan struct{}),
	}
}

// Run owns the subscriber set until ctx is cancelled. It fans each published
// event out to the subscribers of its kind with a NON-BLOCKING send, so a
// subscriber that has fallen behind drops the event instead of stalling the bus.
func (b *Bus) Run(ctx context.Context) {
	// kind -> id -> subscriber.
	subs := map[string]map[uint64]*subscriber{}
	defer close(b.done)

	for {
		select {
		case <-ctx.Done():
			for _, byID := range subs {
				for _, s := range byID {
					close(s.ch)
				}
			}
			return

		case s := <-b.sub:
			byID := subs[s.kind]
			if byID == nil {
				byID = map[uint64]*subscriber{}
				subs[s.kind] = byID
			}
			byID[s.id] = s
			go s.drain()

		case id := <-b.unsub:
			for kind, byID := range subs {
				if s, ok := byID[id]; ok {
					delete(byID, id)
					if len(byID) == 0 {
						delete(subs, kind)
					}
					close(s.ch) // stops the drain goroutine; safe, only here removes it
					break
				}
			}

		case e := <-b.publish:
			for _, s := range subs[e.Kind()] {
				select {
				case s.ch <- e:
				default:
					b.log.Warn().Str("kind", e.Kind()).Uint64("subscriber", s.id).
						Msg("event dropped: subscriber is behind")
				}
			}
		}
	}
}

func (s *subscriber) drain() {
	for e := range s.ch {
		s.handler(e)
	}
}

// Publish announces an event to whoever is listening. It never blocks: if the
// dispatcher is briefly saturated the event is dropped and logged, because a
// dropped reaction is a smaller problem than a stalled emitter.
func (b *Bus) Publish(e Event) {
	select {
	case b.publish <- e:
	case <-b.done:
	default:
		b.log.Warn().Str("kind", e.Kind()).Msg("event dropped: bus is backed up")
	}
}

// Subscribe registers a handler for one kind of event and returns a function
// that unsubscribes it. The handler runs on its own goroutine, one event at a
// time. Subscribe from boot, before events start flowing; the returned cancel is
// for a subscriber with a shorter life than the bus.
func (b *Bus) Subscribe(kind string, handler Handler) (cancel func()) {
	s := &subscriber{
		id:      b.nextID.Add(1),
		kind:    kind,
		ch:      make(chan Event, subscriberBuffer),
		handler: handler,
	}
	select {
	case b.sub <- s:
	case <-b.done:
	}
	return func() {
		select {
		case b.unsub <- s.id:
		case <-b.done:
		}
	}
}
