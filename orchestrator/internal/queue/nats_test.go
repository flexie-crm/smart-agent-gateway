package queue

import (
	"context"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// A real broker, in-process. The same argument as the real-database suites: a
// fake queue agrees with whatever the test expects, and the questions worth
// asking here (does exactly one member of a group get a message, does a signal
// survive a subscriber that was not there yet) are questions about the server.

func broker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, // an unused port, chosen by the OS
		JetStream: true, StoreDir: dir, NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatalf("start broker: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("the broker did not come up")
	}
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}

func connect(t *testing.T) *NATS {
	t.Helper()
	q, err := Connect(context.Background(), broker(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

// pull waits for one message the way a reader does: say you are free, then take
// what arrives. It gives up if the queue has nothing to say within the test's
// patience.
func pull(t *testing.T, sub Subscription) (Task, bool) {
	t.Helper()
	sub.Want()
	select {
	case task, ok := <-sub.Tasks():
		return task, ok
	case <-time.After(5 * time.Second):
		return Task{}, false
	}
}

func TestAMessageReachesAConsumer(t *testing.T) {
	q := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := q.Subscribe(ctx, "jobs.default.>", "default")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	sent := Task{ID: "job_one", Kind: "chat.title", WorkspaceID: 7, Payload: []byte(`{"a":1}`)}
	if err := q.Enqueue(ctx, "jobs.default.chat.title", sent); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	task, ok := pull(t, sub)
	if !ok {
		t.Fatal("nothing arrived")
	}
	if task.ID != sent.ID || task.Kind != sent.Kind || task.WorkspaceID != sent.WorkspaceID {
		t.Fatalf("the task did not survive the wire: %+v", task)
	}
	if string(task.Payload) != `{"a":1}` {
		t.Fatalf("the payload did not survive: %q", task.Payload)
	}
}

// The property the whole worker fleet rests on: many consumers, one delivery.
func TestExactlyOneMemberOfAGroupGetsEachMessage(t *testing.T) {
	q := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const messages = 30
	var (
		mu   sync.Mutex
		seen = map[string]int{}
		wg   sync.WaitGroup
	)
	wg.Add(messages)

	for i := 0; i < messages; i++ {
		task := Task{ID: "job_" + string(rune('a'+i)), Kind: "chat.title"}
		if err := q.Enqueue(ctx, "jobs.default.chat.title", task); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	// Four goroutines sharing one group, each pulling one at a time, which is
	// exactly what a worker process is.
	for i := 0; i < 4; i++ {
		sub, err := q.Subscribe(ctx, "jobs.default.>", "default")
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		go func() {
			for {
				sub.Want()
				task, ok := <-sub.Tasks()
				if !ok {
					return
				}
				mu.Lock()
				seen[task.ID]++
				mu.Unlock()
				wg.Done()
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("not every message was delivered")
	}

	// Give a duplicate a chance to show up before deciding there was not one.
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != messages {
		t.Fatalf("expected %d distinct messages, saw %d", messages, len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s was delivered %d times: two workers would run one job", id, n)
		}
	}
}

// A worker only hears what it serves. This is what keeps a CPU node from
// picking up a model pull it has no way to run.
func TestASubjectFilterIsACapability(t *testing.T) {
	q := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := q.Subscribe(ctx, "jobs.model-host.>", "model-host")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := q.Enqueue(ctx, "jobs.default.chat.title", Task{ID: "job_title"}); err != nil {
		t.Fatalf("enqueue title: %v", err)
	}
	if err := q.Enqueue(ctx, "jobs.model-host.model.pull", Task{ID: "job_pull"}); err != nil {
		t.Fatalf("enqueue pull: %v", err)
	}

	task, ok := pull(t, sub)
	if !ok {
		t.Fatal("the GPU node never got its own job")
	}
	if task.ID != "job_pull" {
		t.Fatalf("the GPU node was handed %q, which is not its work", task.ID)
	}
	if extra, ok := pull(t, sub); ok {
		t.Fatalf("the GPU node was also handed %q", extra.ID)
	}
}

// A signal published while nothing is listening is still there when a worker
// arrives. Without this, every restart would lose whatever was enqueued during
// it and only the database scan would ever find that work.
func TestASignalWaitsForAWorkerToExist(t *testing.T) {
	q := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := q.Enqueue(ctx, "jobs.default.chat.title", Task{ID: "job_early"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	sub, err := q.Subscribe(ctx, "jobs.default.>", "default")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	task, ok := pull(t, sub)
	if !ok {
		t.Fatal("a signal sent before the worker existed was lost")
	}
	if task.ID != "job_early" {
		t.Fatalf("got %q", task.ID)
	}
}

// Publishing the same job twice wakes one worker, not two. A retried publish
// after an ambiguous failure is the ordinary way this happens.
func TestTheSameJobIsNotSignalledTwice(t *testing.T) {
	q := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := q.Subscribe(ctx, "jobs.default.>", "default")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	task := Task{ID: "job_same", Kind: "chat.title"}
	for i := 0; i < 3; i++ {
		if err := q.Enqueue(ctx, "jobs.default.chat.title", task); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	if _, ok := pull(t, sub); !ok {
		t.Fatal("the job was never signalled at all")
	}
	if extra, ok := pull(t, sub); ok {
		t.Fatalf("one job signalled three times woke a second worker with %q", extra.ID)
	}
}

// The way back. A worker reports on its own subject and the orchestrator hears
// it; this is the leg that does not exist in one process, where an event bus
// does the same job for free.
func TestAWorkerReportsBack(t *testing.T) {
	q := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := q.Subscribe(ctx, ReportSubject+".>", "orchestrator")
	if err != nil {
		t.Fatalf("subscribe to reports: %v", err)
	}

	if err := q.Enqueue(ctx, ReportSubject+".delegation", Task{ID: "job_done", Kind: "delegation.finished"}); err != nil {
		t.Fatalf("report: %v", err)
	}

	task, ok := pull(t, sub)
	if !ok {
		t.Fatal("the orchestrator never heard the report")
	}
	if task.Kind != "delegation.finished" {
		t.Fatalf("the report did not arrive intact: %+v", task)
	}
}

// A job goes to ONE worker; an instruction goes to ALL of them. This is the
// difference the two subscribe methods exist for, and getting it backwards
// would mean a cancel reaching one of the twenty processes that might be
// running the work.
func TestABroadcastReachesEveryListener(t *testing.T) {
	q := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	subject := OpsSubject + ".fleet.cancel"
	listeners := make([]Subscription, 3)
	for i := range listeners {
		sub, err := q.Broadcast(ctx, subject)
		if err != nil {
			t.Fatalf("broadcast listener %d: %v", i, err)
		}
		listeners[i] = sub
		t.Cleanup(func() { _ = sub.Close() })
	}

	if err := q.Enqueue(ctx, subject, Task{ID: "op-1", Kind: "fleet.cancel"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	for i, sub := range listeners {
		task, ok := pull(t, sub)
		if !ok {
			t.Fatalf("listener %d never heard the instruction", i)
		}
		if task.Kind != "fleet.cancel" {
			t.Fatalf("listener %d heard %q", i, task.Kind)
		}
	}
}

// A listener hears what is published after it joined, and not the backlog. An
// instruction is about work in progress, so a worker that has just started has
// nothing the old ones were about, and replaying them would have it cancelling
// batches that finished hours ago.
func TestABroadcastListenerHearsOnlyWhatComesAfterIt(t *testing.T) {
	q := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	subject := OpsSubject + ".fleet.cancel"
	if err := q.Enqueue(ctx, subject, Task{ID: "op-old", Kind: "fleet.cancel"}); err != nil {
		t.Fatalf("publish the old instruction: %v", err)
	}

	sub, err := q.Broadcast(ctx, subject)
	if err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	defer func() { _ = sub.Close() }()

	if err := q.Enqueue(ctx, subject, Task{ID: "op-new", Kind: "fleet.cancel"}); err != nil {
		t.Fatalf("publish the new instruction: %v", err)
	}
	task, ok := pull(t, sub)
	if !ok {
		t.Fatal("the listener heard nothing at all")
	}
	if task.ID != "op-new" {
		t.Fatalf("a listener that joined late replayed %q", task.ID)
	}
}

// Two listeners on the same subject must not share a consumer, or "everybody
// hears it" quietly becomes "one of them does". This is the same assertion as
// above from the other side: it fails loudly if Broadcast ever grows a durable
// name.
func TestBroadcastAndSubscribeAreNotTheSameThing(t *testing.T) {
	q := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	subject := SubjectRoot + ".default.share"
	a, err := q.Subscribe(ctx, subject, "workers")
	if err != nil {
		t.Fatalf("subscribe a: %v", err)
	}
	b, err := q.Subscribe(ctx, subject, "workers")
	if err != nil {
		t.Fatalf("subscribe b: %v", err)
	}
	if err := q.Enqueue(ctx, subject, Task{ID: "job-1", Kind: "work"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got := 0
	if _, ok := pull(t, a); ok {
		got++
	}
	if _, ok := pull(t, b); ok {
		got++
	}
	if got != 1 {
		t.Fatalf("a queued job reached %d of the group, want exactly 1", got)
	}
}

// A message arrives when it is PUBLISHED, not when somebody next looks.
//
// This is the property the whole design rests on. A listener holds its
// connection open and the server delivers into it, so a hop across the broker
// costs a network round trip and nothing else. The moment it becomes "ask, wait
// for an answer, ask again" the platform is paying an interval per hop, and a
// fleet of fifteen agents is slower than doing the work in one process.
//
// So the bound here is deliberately far below any plausible polling interval: a
// test that allowed a second would still pass against the thing this exists to
// prevent.
func TestAMessageArrivesWhenItIsPublished(t *testing.T) {
	q := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := q.Subscribe(ctx, "jobs.default.>", "prompt")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	// Settled first, so this measures delivery rather than the subscription
	// being set up.
	if err := q.Enqueue(ctx, "jobs.default.chat.title", Task{ID: "job_warm", Kind: "chat.title"}); err != nil {
		t.Fatalf("warm-up publish: %v", err)
	}
	if _, ok := pull(t, sub); !ok {
		t.Fatal("the warm-up message never arrived")
	}

	start := time.Now()
	if err := q.Enqueue(ctx, "jobs.default.chat.title", Task{ID: "job_now", Kind: "chat.title"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	task, ok := pull(t, sub)
	elapsed := time.Since(start)
	if !ok {
		t.Fatal("the message never arrived")
	}
	if task.ID != "job_now" {
		t.Fatalf("got %q", task.ID)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("delivery took %v: a live connection should carry it in milliseconds, "+
			"and anything near a second means somebody is asking on a timer", elapsed)
	}
	t.Logf("delivered in %v", elapsed)
}

// A worker holds ONE message. Asking for a second would mean holding work
// nobody is doing, which is invisible in the broker and lost if the process
// dies; a listener that only reacts has no such problem and takes everything.
func TestAWorkerHoldsOneAndAListenerTakesAll(t *testing.T) {
	q := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	work, err := q.Subscribe(ctx, "jobs.default.>", "one-at-a-time")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = work.Close() }()

	for i := 0; i < 5; i++ {
		if err := q.Enqueue(ctx, "jobs.default.chat.title",
			Task{ID: "job_w" + string(rune('a'+i)), Kind: "chat.title"}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	// One is delivered, and nothing else follows while the reader has not asked
	// again: the rest stay in the broker, where a crash cannot lose them.
	if _, ok := pull(t, work); !ok {
		t.Fatal("the first message never arrived")
	}
	select {
	case extra := <-work.Tasks():
		t.Fatalf("a second message (%s) was pushed at a reader that had not asked", extra.ID)
	case <-time.After(300 * time.Millisecond):
	}

	// The listener takes them as fast as it reads, with nothing holding it to one.
	all, err := q.SubscribeAll(ctx, "reports.>", "all-at-once")
	if err != nil {
		t.Fatalf("subscribe all: %v", err)
	}
	defer func() { _ = all.Close() }()
	for i := 0; i < 5; i++ {
		if err := q.Enqueue(ctx, "reports.fleet",
			Task{ID: "rep_" + string(rune('a'+i)), Kind: "report"}); err != nil {
			t.Fatalf("publish report: %v", err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, ok := pull(t, all); !ok {
			t.Fatalf("report %d never arrived", i)
		}
	}
}
