package queue

import (
	"context"
	"testing"
	"time"
)

const settle = 2 * time.Second

// reader is a subscription with somebody actually reading it, which is what a
// worker is. It matters here more than it would with a broker: delivery is a
// send on an unbuffered channel, so a subscription nobody is waiting on is a
// subscription that is busy, and the queue is right to skip it.
type reader struct {
	sub  Subscription
	got  chan Task
	done chan struct{}
}

func newReader(t *testing.T, q *InProcess, subject, group string) *reader {
	t.Helper()
	sub, err := q.Subscribe(t.Context(), subject, group)
	if err != nil {
		t.Fatal(err)
	}
	return startReader(sub)
}

func newListener(t *testing.T, q *InProcess, subject string) *reader {
	t.Helper()
	sub, err := q.Broadcast(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	return startReader(sub)
}

// readerBacklog is generous on purpose. What it holds is what the reader has
// already taken, so a full one stops the goroutine mid-handover and it is no
// longer parked on Tasks() waiting for work: the queue then correctly treats it
// as busy, and a test measuring delivery measures the buffer instead. Sized
// past anything a test here sends.
const readerBacklog = 512

func startReader(sub Subscription) *reader {
	r := &reader{sub: sub, got: make(chan Task, readerBacklog), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for task := range sub.Tasks() {
			r.got <- task
		}
	}()
	return r
}

func (r *reader) take(t *testing.T) Task {
	t.Helper()
	select {
	case task := <-r.got:
		return task
	case <-time.After(settle):
		t.Fatal("expected a task, none arrived")
		return Task{}
	}
}

// empty asserts nothing arrived. The pause is real, because an absence cannot be
// proved instantly.
func (r *reader) empty(t *testing.T) {
	t.Helper()
	select {
	case task := <-r.got:
		t.Fatalf("expected nothing, got %q", task.ID)
	case <-time.After(150 * time.Millisecond):
	}
}

// deliver publishes until somebody takes it, and reports how many did.
//
// The retry is not papering over a race, it is the system: a reader that has not
// yet reached its receive is indistinguishable from one that is busy, and the
// real answer to both is that the row is still there and the scan will come back
// for it. This is that scan, at test speed.
func deliver(t *testing.T, q *InProcess, subject string, task Task) int {
	t.Helper()
	deadline := time.Now().Add(settle)
	for time.Now().Before(deadline) {
		n, err := q.publish(subject, task)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return n
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("nothing took %q on %q", task.ID, subject)
	return 0
}

func TestInProcessGroupDeliversToExactlyOneMember(t *testing.T) {
	q := NewInProcess()
	defer func() { _ = q.Close() }()

	const members, sends = 3, 100
	readers := make([]*reader, members)
	for i := range readers {
		readers[i] = newReader(t, q, "jobs.default.>", "workers")
	}

	// Asserting this over ONE publish proves nothing, which mutation testing
	// showed rather than theory: if only one member happens to be parked in its
	// receive at that instant, a queue that fans out to every free member also
	// delivers exactly once, so the test passes while the property is broken.
	//
	// Two things make it real. The invariant is asserted many times, so the
	// moment when several members are ready together certainly arrives. And each
	// publish is preceded by a pause, because publishing in a tight loop starves
	// the reader goroutines: measured, a hot loop delivered to nobody 171 times
	// out of 200 and to one member 29 times, and never to two even with
	// exclusivity deliberately removed.
	//
	// The invariant: no publish, ever, goes to more than one member of a group.
	delivered := 0
	for i := 0; i < sends; i++ {
		time.Sleep(time.Millisecond)
		n, err := q.publish("jobs.default.model.pull", Task{ID: "j"})
		if err != nil {
			t.Fatal(err)
		}
		if n > 1 {
			t.Fatalf("publish %d went to %d members; a group hands a task to one", i, n)
		}
		delivered += n
	}
	// With every member idle and waiting, essentially every publish should land.
	// Requiring most of them proves the readers really were ready, which is what
	// makes the assertion above meaningful.
	if delivered < sends*9/10 {
		t.Fatalf("only %d of %d publishes were taken; the members were not idle, "+
			"so the one-member assertion proved little", delivered, sends)
	}
	if delivered == 0 {
		t.Fatal("nothing was delivered at all, so the assertion above proved nothing")
	}

	// And what was handed over is what was received: no duplicates anywhere.
	received := 0
	for _, r := range readers {
		for {
			select {
			case <-r.got:
				received++
				continue
			case <-time.After(50 * time.Millisecond):
			}
			break
		}
	}
	if received != delivered {
		t.Fatalf("delivered %d tasks but readers saw %d", delivered, received)
	}
}

func TestInProcessSkipsBusyMemberAndUsesAFreeOne(t *testing.T) {
	q := NewInProcess()
	defer func() { _ = q.Close() }()

	// Subscribed but never read, which is exactly what a worker mid-job is.
	busy, err := q.Subscribe(t.Context(), "jobs.default.>", "workers")
	if err != nil {
		t.Fatal(err)
	}
	free := newReader(t, q, "jobs.default.>", "workers")

	if n := deliver(t, q, "jobs.default.chat.title", Task{ID: "j2"}); n != 1 {
		t.Fatalf("delivered to %d members", n)
	}
	if task := free.take(t); task.ID != "j2" {
		t.Fatalf("free member got %q", task.ID)
	}
	select {
	case task := <-busy.Tasks():
		t.Fatalf("a busy member must be skipped, it got %q", task.ID)
	default:
	}
}

func TestInProcessDropsWhenNoMemberIsFree(t *testing.T) {
	q := NewInProcess()
	defer func() { _ = q.Close() }()

	// A group whose only member never reads. The row is committed before this
	// call and the worker's scan is what finds it, so the queue must report
	// success and must not wait for a reader.
	if _, err := q.Subscribe(t.Context(), "jobs.default.>", "workers"); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		n, err := q.publish("jobs.default.model.pull", Task{ID: "j3"})
		if err != nil {
			t.Error(err)
		}
		done <- n
	}()
	select {
	case n := <-done:
		if n != 0 {
			t.Fatalf("nobody was free, so nothing should have been delivered, got %d", n)
		}
	case <-time.After(settle):
		t.Fatal("publish blocked; it must never wait for a reader")
	}
}

func TestInProcessBroadcastReachesEveryListener(t *testing.T) {
	q := NewInProcess()
	defer func() { _ = q.Close() }()

	a := newListener(t, q, "ops.fleet.stop")
	b := newListener(t, q, "ops.fleet.stop")

	if n := deliver(t, q, "ops.fleet.stop", Task{ID: "stop"}); n != 2 {
		t.Fatalf("a broadcast must reach every listener, reached %d", n)
	}
	if task := a.take(t); task.ID != "stop" {
		t.Fatalf("listener a got %q", task.ID)
	}
	if task := b.take(t); task.ID != "stop" {
		t.Fatalf("listener b got %q", task.ID)
	}
}

func TestInProcessSubjectFilteringKeepsCapabilitiesApart(t *testing.T) {
	q := NewInProcess()
	defer func() { _ = q.Close() }()

	def := newReader(t, q, "jobs.default.>", "workers")
	gpu := newReader(t, q, "jobs.gpu.>", "workers")

	if n := deliver(t, q, "jobs.gpu.model.pull", Task{ID: "gpu-job"}); n != 1 {
		t.Fatalf("delivered to %d subscriptions", n)
	}
	if task := gpu.take(t); task.ID != "gpu-job" {
		t.Fatalf("gpu worker got %q", task.ID)
	}
	def.empty(t)
}

func TestInProcessClosingOneMemberLeavesItsSiblingWorking(t *testing.T) {
	q := NewInProcess()
	defer func() { _ = q.Close() }()

	doomed, err := q.Subscribe(t.Context(), "jobs.default.>", "workers")
	if err != nil {
		t.Fatal(err)
	}
	survivor := newReader(t, q, "jobs.default.>", "workers")

	if err := doomed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-doomed.Tasks(); ok {
		t.Fatal("a closed subscription must close its channel")
	}

	if n := deliver(t, q, "jobs.default.chat.title", Task{ID: "j4"}); n != 1 {
		t.Fatalf("delivered to %d members", n)
	}
	if task := survivor.take(t); task.ID != "j4" {
		t.Fatalf("survivor got %q", task.ID)
	}
}

func TestInProcessSubscriptionEndsWithItsContext(t *testing.T) {
	q := NewInProcess()
	defer func() { _ = q.Close() }()
	ctx, cancel := context.WithCancel(context.Background())

	s, err := q.Subscribe(ctx, "jobs.default.>", "workers")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, ok := <-s.Tasks():
		if ok {
			t.Fatal("expected the channel to close, not to deliver")
		}
	case <-time.After(settle):
		t.Fatal("cancelling the context must end the subscription")
	}
}

func TestInProcessRefusesWorkAfterClose(t *testing.T) {
	q := NewInProcess()
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), "jobs.default.x", Task{ID: "j5"}); err == nil {
		t.Fatal("enqueue on a closed queue must fail")
	}
	if _, err := q.Subscribe(context.Background(), "jobs.default.>", "workers"); err == nil {
		t.Fatal("subscribe on a closed queue must fail")
	}
	if _, err := q.Broadcast(context.Background(), "ops.x"); err == nil {
		t.Fatal("broadcast on a closed queue must fail")
	}
}

func TestInProcessCloseEndsLiveSubscriptions(t *testing.T) {
	q := NewInProcess()
	s, err := q.Subscribe(context.Background(), "jobs.default.>", "workers")
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-s.Tasks():
		if ok {
			t.Fatal("expected the channel to close")
		}
	case <-time.After(settle):
		t.Fatal("closing the queue must end its subscriptions")
	}
}

func TestSubjectMatches(t *testing.T) {
	cases := []struct {
		pattern, subject string
		want             bool
	}{
		{"jobs.default.>", "jobs.default.model.pull", true},
		{"jobs.default.>", "jobs.default.chat.title", true},
		{"jobs.default.>", "jobs.gpu.model.pull", false},
		// ">" stands for one token or more, so it does not match the prefix alone.
		{"jobs.default.>", "jobs.default", false},
		{"jobs.*.model.pull", "jobs.default.model.pull", true},
		// "*" is exactly one token, never several.
		{"jobs.*.pull", "jobs.default.model.pull", false},
		{"jobs.default.chat.title", "jobs.default.chat.title", true},
		{"jobs.default.chat.title", "jobs.default.chat", false},
		{"reports.>", "reports.done", true},
		{"ops.fleet.stop", "ops.fleet.stop", true},
		{"ops.fleet.stop", "ops.fleet.start", false},
	}
	for _, c := range cases {
		if got := subjectMatches(c.pattern, c.subject); got != c.want {
			t.Errorf("subjectMatches(%q, %q) = %v, want %v", c.pattern, c.subject, got, c.want)
		}
	}
}
