package queue

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// InProcess is a Queue with no broker at all: subjects are matched in memory and
// a message is a send on a channel.
//
// It is not a stub or a test double. It is the whole queue for a deployment that
// runs the web stack and the jobs stack in ONE process, which is what the
// desktop application does (KB/36) and what the smallest server topology can do.
//
// # Why this is sound
//
// Because the broker was never where jobs live. This package's own contract says
// it: the row in `jobs` is durable and auditable, recovery is a DB scan, and a
// message is "a doorbell, lose it and the row is still there; the sweep finds
// it". Everything the broker contributes is latency. Take it away and the system
// still works, one scan interval slower.
//
// That is what lets Enqueue here be allowed to DROP. Callers commit the row
// before publishing (the enqueue-after-commit rule), so a dropped send costs a
// delay and never a job.
//
// # Why the channels are unbuffered
//
// So that "one at a time" stays exactly as true as it is with a broker. A worker
// that is busy is not sitting in a receive, so a send to it fails immediately and
// the next member is tried; nothing accumulates in front of a goroutine that
// cannot get to it. It is the same property NATS gives through Want(), by the
// same means: work is handed over only to somebody free to take it.
//
// A crash therefore loses nothing that was not already being worked on, which is
// the guarantee Subscription documents, and the claimed rows a dead process
// leaves behind are reclaimed when their lease expires.
type InProcess struct {
	mu     sync.Mutex
	groups map[groupKey]*deliveryGroup
	casts  map[*inProcSub]struct{}
	closed bool
}

// broadcastBuffer is small and deliberately not zero. A broadcast carries an
// instruction rather than work ("stop what you are doing on this fleet"), and
// its listeners are reacting rather than working, so a listener that is a
// moment late should still hear it. Work is the opposite and is unbuffered.
const broadcastBuffer = 8

type groupKey struct{ subject, group string }

// deliveryGroup is the set of subscriptions racing for one subject. A member
// holds its own channel rather than sharing one, so closing a subscription
// closes exactly that subscription's channel and leaves its siblings running.
type deliveryGroup struct {
	members []*inProcSub
	next    int
}

func NewInProcess() *InProcess {
	return &InProcess{
		groups: make(map[groupKey]*deliveryGroup),
		casts:  make(map[*inProcSub]struct{}),
	}
}

// Enqueue hands the task to one member of every matching group, and to every
// matching broadcast listener.
//
// It never blocks and never fails for want of a reader. When every member of a
// group is busy the task is not delivered, which is the documented shape of this
// queue rather than a loss: the row is committed and the scan is what finds it.
func (q *InProcess) Enqueue(_ context.Context, subject string, t Task) error {
	_, err := q.publish(subject, t)
	return err
}

// publish is Enqueue with the one fact Enqueue cannot report: how many readers
// actually took the message. Nothing in production wants it (a caller that
// changed behaviour on the count would be treating the doorbell as the job), but
// a test asserting "exactly one member of a group got this" has no other way to
// tell delivery from a lucky read, and "nobody was free, so it was dropped" is
// behaviour worth pinning rather than inferring.
func (q *InProcess) publish(subject string, t Task) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return 0, fmt.Errorf("queue: closed")
	}
	delivered := 0

	for key, g := range q.groups {
		if !subjectMatches(key.subject, subject) {
			continue
		}
		// Start where the last delivery left off, so a group with more members
		// than work does not always wake the same one.
	members:
		for i := 0; i < len(g.members); i++ {
			m := g.members[(g.next+i)%len(g.members)]
			select {
			case m.tasks <- t:
				g.next = (g.next + i + 1) % len(g.members)
				delivered++
				break members
			default:
			}
		}
	}

	for s := range q.casts {
		if !subjectMatches(s.subject, subject) {
			continue
		}
		select {
		case s.tasks <- t:
			delivered++
		default: // a listener too far behind to hear an instruction it is not acting on
		}
	}
	return delivered, nil
}

// Subscribe joins a group: every member races for a message and exactly one gets
// it, because exactly one of them can be free to receive it first.
func (q *InProcess) Subscribe(ctx context.Context, subject, group string) (Subscription, error) {
	return q.join(ctx, subject, group)
}

// SubscribeAll joins a group the same way. The difference the NATS
// implementation expresses through consumer configuration is, here, entirely on
// the reader's side: a caller that handles messages in parallel receives them as
// fast as they arrive, and one that handles them in a loop does not.
func (q *InProcess) SubscribeAll(ctx context.Context, subject, group string) (Subscription, error) {
	return q.join(ctx, subject, group)
}

func (q *InProcess) join(ctx context.Context, subject, group string) (Subscription, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, fmt.Errorf("queue: closed")
	}

	key := groupKey{subject: subject, group: group}
	g := q.groups[key]
	if g == nil {
		g = &deliveryGroup{}
		q.groups[key] = g
	}
	s := &inProcSub{q: q, subject: subject, key: key, grouped: true, tasks: make(chan Task)}
	g.members = append(g.members, s)
	s.watch(ctx)
	return s, nil
}

// Broadcast joins a subject as an independent listener: every subscriber hears
// every message rather than racing for it.
func (q *InProcess) Broadcast(ctx context.Context, subject string) (Subscription, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, fmt.Errorf("queue: closed")
	}
	s := &inProcSub{q: q, subject: subject, tasks: make(chan Task, broadcastBuffer)}
	q.casts[s] = struct{}{}
	s.watch(ctx)
	return s, nil
}

func (q *InProcess) Close() error {
	q.mu.Lock()
	subs := make([]*inProcSub, 0, len(q.casts))
	for s := range q.casts {
		subs = append(subs, s)
	}
	for _, g := range q.groups {
		subs = append(subs, g.members...)
	}
	q.closed = true
	q.groups = make(map[groupKey]*deliveryGroup)
	q.casts = make(map[*inProcSub]struct{})
	q.mu.Unlock()

	// Outside the lock: closing a subscription takes it, and it has already been
	// detached from the maps above.
	for _, s := range subs {
		s.finish()
	}
	return nil
}

type inProcSub struct {
	q       *InProcess
	subject string
	key     groupKey
	grouped bool
	tasks   chan Task
	once    sync.Once
}

// watch ends the subscription when its context does, which is the behaviour the
// interface promises and the only thing keeping a cancelled worker's channel
// from outliving it.
func (s *inProcSub) watch(ctx context.Context) {
	if ctx.Done() == nil {
		return
	}
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
}

func (s *inProcSub) Tasks() <-chan Task { return s.tasks }

// Want is a no-op, and that is the faithful translation rather than a gap. With
// a broker it means "fetch one more"; here the receive on Tasks() IS the request,
// because an unbuffered channel hands over a message only to a reader already
// waiting for one.
func (s *inProcSub) Want() {}

func (s *inProcSub) Close() error {
	s.q.detach(s)
	s.finish()
	return nil
}

func (s *inProcSub) finish() {
	s.once.Do(func() { close(s.tasks) })
}

func (q *InProcess) detach(s *inProcSub) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if s.grouped {
		g := q.groups[s.key]
		if g == nil {
			return
		}
		for i, m := range g.members {
			if m == s {
				g.members = append(g.members[:i], g.members[i+1:]...)
				break
			}
		}
		if len(g.members) == 0 {
			delete(q.groups, s.key)
		} else if g.next >= len(g.members) {
			g.next = 0
		}
		return
	}
	delete(q.casts, s)
}

// subjectMatches applies the broker's own subject rules, because the subjects
// are written for it and a second dialect of them would be a bug waiting to
// happen: tokens are separated by dots, "*" matches exactly one token, and ">"
// matches one or more and may only be last.
//
// The pattern is the subscriber's; the subject is what was published.
func subjectMatches(pattern, subject string) bool {
	if pattern == subject {
		return true
	}
	p := strings.Split(pattern, ".")
	s := strings.Split(subject, ".")
	for i, tok := range p {
		if tok == ">" {
			// Only as the last token, and it needs at least one to match.
			return i == len(p)-1 && len(s) > i
		}
		if i >= len(s) {
			return false
		}
		if tok != "*" && tok != s[i] {
			return false
		}
	}
	return len(p) == len(s)
}
