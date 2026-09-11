package worker

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/rs/zerolog"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/queue"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/testdb"
)

// The worker loop, against a real database and a real broker. What is worth
// asserting is not that a job runs, which is the easy half, but what happens
// around it: a job nobody here can do is handed back rather than failed, a
// failure is retried and eventually left dead, and a worker asked to stop
// returns what it was holding instead of burning its attempts.

const dbSuffix = "worker"

func TestMain(m *testing.M) { os.Exit(testdb.Main(m, dbSuffix)) }

type harness struct {
	store store.Store
	q     *queue.NATS
	pool  *Pool
	ws    int64
}

func newHarness(t *testing.T, concurrency int) *harness {
	t.Helper()
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN is not set, so the worker suite has no database to run against")
	}
	st, reset := testdb.Open(t, dsn, dbSuffix)
	reset(t)

	dir := t.TempDir()
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1,
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

	q, err := queue.Connect(context.Background(), srv.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })

	ws := &model.Workspace{Slug: "worker-test", Name: "worker-test"}
	if err := st.Workspaces().Create(context.Background(), ws); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	pool := New(st, q, zerolog.Nop(), "default", concurrency)
	// Tight, so a test does not wait on production cadences.
	pool.heartbeat = 50 * time.Millisecond
	pool.stale = 200 * time.Millisecond
	pool.scanEvery = 100 * time.Millisecond

	return &harness{store: st, q: q, pool: pool, ws: ws.ID}
}

// enqueue writes the row and rings the bell, in that order, which is the rule.
func (h *harness) enqueue(t *testing.T, kind string) *model.Job {
	t.Helper()
	ctx := context.Background()
	j := &model.Job{
		WorkspaceID: h.ws, Kind: kind,
		Subject: queue.SubjectRoot + ".default." + kind,
		Payload: []byte(`{"n":1}`),
	}
	if err := h.store.Jobs().Enqueue(ctx, j); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := h.q.Enqueue(ctx, j.Subject, queue.Task{ID: j.ID, Kind: j.Kind, WorkspaceID: h.ws}); err != nil {
		t.Fatalf("signal: %v", err)
	}
	return j
}

func (h *harness) run(t *testing.T) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.pool.Run(ctx, "default")
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the pool did not stop")
		}
	})
	return cancel
}

// waitFor gives a condition a while to come true, so a test says what it is
// waiting for rather than how long it decided to sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAJobIsClaimedRunAndCompleted(t *testing.T) {
	h := newHarness(t, 2)
	var (
		mu   sync.Mutex
		seen []string
	)
	h.pool.Handle("greet", func(_ context.Context, job *model.Job) error {
		mu.Lock()
		seen = append(seen, job.ID)
		mu.Unlock()
		return nil
	})
	h.run(t)

	j := h.enqueue(t, "greet")
	waitFor(t, "the job to run", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == 1 && seen[0] == j.ID
	})
	waitFor(t, "the job to be marked done", func() bool {
		got, err := h.store.Jobs().Get(context.Background(), j.ID)
		return err == nil && got.Status == model.JobCompleted
	})
}

// A kind this node does not serve is handed straight back, WITHOUT burning an
// attempt. Failing it would eventually kill work that nothing is wrong with,
// on a system where another node was willing to do it all along.
func TestAJobThisNodeCannotDoIsHandedBack(t *testing.T) {
	h := newHarness(t, 1)
	h.pool.Handle("greet", func(context.Context, *model.Job) error { return nil })
	h.run(t)

	j := h.enqueue(t, "somebody-elses-work")
	waitFor(t, "the job to be handed back", func() bool {
		got, err := h.store.Jobs().Get(context.Background(), j.ID)
		return err == nil && got.Status == model.JobPending && got.LockedBy == ""
	})

	got, _ := h.store.Jobs().Get(context.Background(), j.ID)
	if got.Attempts != 0 {
		t.Fatalf("arriving at the wrong door spent an attempt: %d", got.Attempts)
	}
}

// A failing job is retried, and its backoff really holds it back.
func TestAFailedJobIsRetriedLater(t *testing.T) {
	h := newHarness(t, 1)
	var (
		mu    sync.Mutex
		tries int
	)
	h.pool.Handle("flaky", func(context.Context, *model.Job) error {
		mu.Lock()
		tries++
		mu.Unlock()
		return errors.New("the vendor said no")
	})
	h.run(t)

	j := h.enqueue(t, "flaky")
	waitFor(t, "the first attempt", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return tries >= 1
	})
	waitFor(t, "the failure to be recorded", func() bool {
		got, err := h.store.Jobs().Get(context.Background(), j.ID)
		return err == nil && got.LastError == "the vendor said no" && got.Attempts == 1
	})

	got, _ := h.store.Jobs().Get(context.Background(), j.ID)
	if got.Status != model.JobPending {
		t.Fatalf("a job with attempts left should be waiting to try again: %+v", got)
	}
	if !got.RunAfter.After(time.Now().UTC()) {
		t.Fatal("a failed job was made claimable immediately, so it would spin")
	}
}

// The work of a worker that stopped reporting is given to another, and the
// first one is told to stop rather than finishing something already reassigned.
func TestStalledWorkIsReclaimedAndTheOwnerIsTold(t *testing.T) {
	h := newHarness(t, 1)
	var (
		mu      sync.Mutex
		stopped bool
		started = make(chan struct{})
		release = make(chan struct{})
		oneShot sync.Once
	)
	h.pool.Handle("slow", func(ctx context.Context, _ *model.Job) error {
		oneShot.Do(func() { close(started) })
		select {
		case <-ctx.Done():
			// The heartbeat found the claim gone and cancelled us. That is the
			// worker learning it no longer owns this job.
			mu.Lock()
			stopped = true
			mu.Unlock()
			return ctx.Err()
		case <-release:
			return nil
		}
	})
	h.run(t)

	j := h.enqueue(t, "slow")
	<-started

	// Take the job away underneath it, exactly as a reclaim after a stall does.
	if _, err := h.store.Jobs().Reclaim(context.Background(), 0, 0); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	waitFor(t, "the worker to notice it lost the job", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return stopped
	})
	close(release)
	_ = j
}

// Asked to stop, a worker returns what it was holding, attempt and all.
func TestShutdownHandsWorkBack(t *testing.T) {
	h := newHarness(t, 1)
	started := make(chan struct{})
	var once sync.Once
	h.pool.Handle("long", func(ctx context.Context, _ *model.Job) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	})
	cancel := h.run(t)

	j := h.enqueue(t, "long")
	<-started

	h.pool.Quiesce()
	cancel()

	waitFor(t, "the job to be handed back whole", func() bool {
		got, err := h.store.Jobs().Get(context.Background(), j.ID)
		return err == nil && got.Status == model.JobPending && got.Attempts == 0
	})
}

// The scan is the mechanism, not a nicety: a row with no signal behind it still
// gets done, which is what makes a worker useful with the broker unreachable.
func TestWorkWithNoSignalIsStillFound(t *testing.T) {
	h := newHarness(t, 1)
	done := make(chan string, 1)
	h.pool.Handle("quiet", func(_ context.Context, job *model.Job) error {
		done <- job.ID
		return nil
	})
	h.run(t)

	// Written straight to the database, with nothing published: the case where
	// the broker was down, or the process died between the ack and the claim.
	j := &model.Job{
		WorkspaceID: h.ws, Kind: "quiet",
		Subject: queue.SubjectRoot + ".default.quiet",
	}
	if err := h.store.Jobs().Enqueue(context.Background(), j); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	select {
	case id := <-done:
		if id != j.ID {
			t.Fatalf("ran %q", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a job nobody rang the bell for was never found")
	}
}
