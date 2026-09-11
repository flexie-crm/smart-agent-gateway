package supervise

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func quiet() zerolog.Logger { return zerolog.New(io.Discard) }

// fakeProc stands in for a real process so the decisions in this package can be
// tested without spawning anything: when a restart happens, when it stops
// happening, and what order children go down in.
type fakeProc struct {
	mu    sync.Mutex
	once  sync.Once
	dead  chan struct{}
	err   error
	stops int
	kills int
	// deaf ignores Stop, which is the only way to reach the Kill path.
	deaf bool
}

func newProc() *fakeProc { return &fakeProc{dead: make(chan struct{})} }

func (p *fakeProc) Stop() error {
	p.mu.Lock()
	p.stops++
	deaf := p.deaf
	p.mu.Unlock()
	if !deaf {
		p.die(nil)
	}
	return nil
}

func (p *fakeProc) Kill() error {
	p.mu.Lock()
	p.kills++
	p.mu.Unlock()
	p.die(errors.New("killed"))
	return nil
}

func (p *fakeProc) Wait() error { <-p.dead; return p.err }

func (p *fakeProc) die(err error) {
	p.once.Do(func() { p.err = err; close(p.dead) })
}

func (p *fakeProc) counts() (stops, kills int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stops, p.kills
}

// recorder collects what happened, in order, so ordering can be asserted rather
// than assumed.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf(format, args...))
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *recorder) count(s string) int {
	n := 0
	for _, e := range r.all() {
		if e == s {
			n++
		}
	}
	return n
}

func TestChildrenStartInOrderAndOnlyAfterTheOneBeforeIsReady(t *testing.T) {
	rec := &recorder{}
	s := New(quiet(), DefaultIntensity)

	// "a" reports ready only on the third probe. If start were not gated on
	// readiness, "b" would begin before those probes finished and the recorded
	// order would show it.
	probes := 0
	s.Add(Child{
		Name:         "a",
		ReadyTimeout: 2 * time.Second,
		Start: func(context.Context) (Process, error) {
			rec.add("start a")
			return newProc(), nil
		},
		Ready: func(context.Context) error {
			probes++
			if probes < 3 {
				return errors.New("not yet")
			}
			rec.add("ready a")
			return nil
		},
	})
	s.Add(Child{
		Name: "b",
		Start: func(context.Context) (Process, error) {
			rec.add("start b")
			return newProc(), nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	waitFor(t, func() bool { return rec.count("start b") == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{"start a", "ready a", "start b"}
	got := rec.all()
	if len(got) < len(want) {
		t.Fatalf("got %v, want it to begin %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("event %d was %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
}

func TestCrashedChildIsRestarted(t *testing.T) {
	rec := &recorder{}
	s := New(quiet(), Intensity{MaxRestarts: 5, Period: time.Minute})

	var mu sync.Mutex
	var live *fakeProc
	s.Add(Child{
		Name: "db",
		Start: func(context.Context) (Process, error) {
			p := newProc()
			mu.Lock()
			live = p
			mu.Unlock()
			rec.add("start")
			return p, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	waitFor(t, func() bool { return rec.count("start") == 1 })
	mu.Lock()
	first := live
	mu.Unlock()
	first.die(errors.New("boom")) // let it crash

	waitFor(t, func() bool { return rec.count("start") == 2 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRestartingTooOftenGivesUpInsteadOfLooping(t *testing.T) {
	rec := &recorder{}
	s := New(quiet(), Intensity{MaxRestarts: 2, Period: time.Minute})

	// Dies the instant it is started, forever. A supervisor without a give-up
	// rule would restart this until the machine was turned off.
	s.Add(Child{
		Name: "db",
		Start: func(context.Context) (Process, error) {
			rec.add("start")
			p := newProc()
			p.die(errors.New("cannot open datadir"))
			return p, nil
		},
	})

	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrGaveUp) {
			t.Fatalf("want ErrGaveUp, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor never gave up; it is looping")
	}

	// Once to start, twice to restart, and the third death is the one that ends
	// it: more attempts than that means the rule is not being applied.
	if n := rec.count("start"); n != 3 {
		t.Fatalf("started %d times, want 3 (initial plus MaxRestarts)", n)
	}
}

// A running process that has stopped being useful is the case Restart exists
// for. A node whose engine has wedged still holds its port and still accepts a
// connection, so nothing the supervisor watches for ever fires: it is not dead,
// it has stopped answering, and before this the only way back was a task
// manager.
func TestARequestedRestartBringsTheChildBack(t *testing.T) {
	rec := &recorder{}
	procs := make(chan *fakeProc, 4)
	s := New(quiet(), DefaultIntensity)

	s.Add(Child{
		Name: "node",
		Start: func(context.Context) (Process, error) {
			rec.add("start")
			p := newProc()
			procs <- p
			return p, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	first := <-procs // it is up
	if err := s.Restart("node"); err != nil {
		t.Fatalf("restart refused: %v", err)
	}

	// The one that was running was asked to go, and a replacement was started.
	if stops, _ := first.counts(); stops != 1 {
		t.Errorf("the running process was not asked to stop, stops=%d", stops)
	}
	select {
	case <-procs:
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was started again: a restart that does not come back is a stop")
	}
	if n := rec.count("start"); n != 2 {
		t.Fatalf("started %d times, want 2 (the original and its replacement)", n)
	}

	// And the supervisor is still running, rather than having treated any of
	// that as a fault.
	select {
	case err := <-done:
		t.Fatalf("supervisor ended on a requested restart: %v", err)
	default:
	}
}

func TestRequestedRestartsAreNotCountedAgainstTheGiveUpRule(t *testing.T) {
	// The rule exists to decide what a process DYING means. A restart somebody
	// asked for did not die, it was replaced, and counting the two together
	// would hand a person a button that puts the whole application down: press
	// it three times in a minute and the supervisor gives up on a child that
	// never once failed.
	rec := &recorder{}
	procs := make(chan *fakeProc, 8)
	s := New(quiet(), Intensity{MaxRestarts: 2, Period: time.Minute})

	s.Add(Child{
		Name: "node",
		Start: func(context.Context) (Process, error) {
			rec.add("start")
			p := newProc()
			procs <- p
			return p, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	<-procs
	// Comfortably more than MaxRestarts, all of them deliberate.
	for i := range 5 {
		if err := s.Restart("node"); err != nil {
			t.Fatalf("restart %d refused: %v", i+1, err)
		}
		select {
		case <-procs:
		case <-time.After(5 * time.Second):
			t.Fatalf("restart %d never came back", i+1)
		}
	}

	select {
	case err := <-done:
		t.Fatalf("five deliberate restarts ended the supervisor: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if n := rec.count("start"); n != 6 {
		t.Fatalf("started %d times, want 6 (one original and five replacements)", n)
	}
}

func TestARequestedRestartInsistsWhenAskingIsIgnored(t *testing.T) {
	// The whole point: the process this is used on is one that has stopped
	// responding. If a wedged child could refuse to go by ignoring Stop, the
	// button would be useless in precisely the case it was added for.
	procs := make(chan *fakeProc, 4)
	s := New(quiet(), DefaultIntensity)

	s.Add(Child{
		Name:     "node",
		Shutdown: 50 * time.Millisecond,
		Start: func(context.Context) (Process, error) {
			p := newProc()
			p.deaf = true // ignores Stop, like a process with a blocked runtime
			procs <- p
			return p, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	wedged := <-procs
	if err := s.Restart("node"); err != nil {
		t.Fatalf("restart refused: %v", err)
	}

	stops, kills := wedged.counts()
	if stops != 1 {
		t.Errorf("it was not asked first, stops=%d", stops)
	}
	if kills != 1 {
		t.Fatalf("a process that ignored the request was not insisted upon, kills=%d", kills)
	}
	select {
	case <-procs:
	case <-time.After(5 * time.Second):
		t.Fatal("the wedged child was ended but never replaced")
	}
}

func TestRestartingSomethingThisSupervisorDoesNotRunIsRefused(t *testing.T) {
	s := New(quiet(), DefaultIntensity)
	s.Add(Child{
		Name:  "node",
		Start: func(context.Context) (Process, error) { return newProc(), nil },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	if err := s.Restart("database"); !errors.Is(err, ErrNoSuchChild) {
		t.Fatalf("want ErrNoSuchChild, got %v", err)
	}
}

func TestFailuresSpacedOutDoNotAccumulate(t *testing.T) {
	rec := &recorder{}
	// A narrow window: a crash older than this is forgotten, so a process that
	// misbehaves occasionally is never condemned for it.
	s := New(quiet(), Intensity{MaxRestarts: 1, Period: 50 * time.Millisecond})

	var mu sync.Mutex
	var live *fakeProc
	s.Add(Child{
		Name: "db",
		Start: func(context.Context) (Process, error) {
			p := newProc()
			mu.Lock()
			live = p
			mu.Unlock()
			rec.add("start")
			return p, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Four crashes, each well outside the window of the one before. With
	// MaxRestarts of 1 this would have given up long ago if the window were
	// not being applied.
	for i := 1; i <= 4; i++ {
		waitFor(t, func() bool { return rec.count("start") == i })
		time.Sleep(120 * time.Millisecond)
		mu.Lock()
		p := live
		mu.Unlock()
		p.die(errors.New("boom"))
	}
	waitFor(t, func() bool { return rec.count("start") == 5 })

	select {
	case err := <-done:
		t.Fatalf("gave up on spaced-out failures: %v", err)
	default:
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestShutdownStopsChildrenInReverseStartOrder(t *testing.T) {
	rec := &recorder{}
	s := New(quiet(), DefaultIntensity)

	for _, name := range []string{"database", "gateway", "node"} {
		s.Add(Child{
			Name:     name,
			Shutdown: time.Second,
			Start: func(context.Context) (Process, error) {
				rec.add("start %s", name)
				return &namedProc{fakeProc: newProc(), rec: rec, name: name}, nil
			},
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	waitFor(t, func() bool { return rec.count("start node") == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The gateway must finish before the database it writes to goes away.
	want := []string{"stop node", "stop gateway", "stop database"}
	var stops []string
	for _, e := range rec.all() {
		if len(e) > 5 && e[:5] == "stop " {
			stops = append(stops, e)
		}
	}
	if len(stops) != len(want) {
		t.Fatalf("stopped %v, want %v", stops, want)
	}
	for i := range want {
		if stops[i] != want[i] {
			t.Fatalf("stop order was %v, want %v", stops, want)
		}
	}
}

type namedProc struct {
	*fakeProc
	rec  *recorder
	name string
}

func (p *namedProc) Stop() error {
	p.rec.add("stop %s", p.name)
	return p.fakeProc.Stop()
}

func TestAChildThatNeverBecomesReadyIsAFailedStart(t *testing.T) {
	s := New(quiet(), DefaultIntensity)
	p := newProc()
	s.Add(Child{
		Name:         "db",
		ReadyTimeout: 150 * time.Millisecond,
		Shutdown:     time.Second,
		Start:        func(context.Context) (Process, error) { return p, nil },
		Ready:        func(context.Context) error { return errors.New("connection refused") },
	})

	err := s.Run(context.Background())
	if err == nil {
		t.Fatal("a child that never became usable must fail the start")
	}
	// And it must not be left running as though it had worked.
	if stops, _ := p.counts(); stops == 0 {
		t.Fatal("the unusable process was left running")
	}
}

func TestATemporaryChildIsNotRestarted(t *testing.T) {
	rec := &recorder{}
	s := New(quiet(), DefaultIntensity)
	s.Add(Child{
		Name:    "one-shot",
		Restart: Temporary,
		Start: func(context.Context) (Process, error) {
			rec.add("start")
			p := newProc()
			p.die(nil)
			return p, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	waitFor(t, func() bool { return rec.count("start") == 1 })
	time.Sleep(200 * time.Millisecond)
	if n := rec.count("start"); n != 1 {
		t.Fatalf("a temporary child was restarted %d times", n)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestAProcessThatIgnoresStopIsKilled(t *testing.T) {
	s := New(quiet(), DefaultIntensity)
	p := newProc()
	p.deaf = true
	s.Add(Child{
		Name:     "stubborn",
		Shutdown: 100 * time.Millisecond, // a ceiling, and this one will hit it
		Start:    func(context.Context) (Process, error) { return p, nil },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	stops, kills := p.counts()
	if stops == 0 {
		t.Fatal("it was killed without being asked first")
	}
	if kills != 1 {
		t.Fatalf("killed %d times, want exactly 1 after the grace period", kills)
	}
}

func TestAProcessThatGoesWhenAskedIsNotWaitedFor(t *testing.T) {
	s := New(quiet(), DefaultIntensity)
	p := newProc()
	s.Add(Child{
		Name:     "polite",
		Shutdown: 30 * time.Second, // a ceiling that must not become a delay
		Start:    func(context.Context) (Process, error) { return p, nil },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("shutdown took %s; the grace period is a ceiling, not a delay", took)
	}
	if _, kills := p.counts(); kills != 0 {
		t.Fatal("a process that stopped when asked must not be killed")
	}
}

func TestAFailureToStartLaterChildrenStopsTheEarlierOnes(t *testing.T) {
	first := newProc()
	s := New(quiet(), DefaultIntensity)
	s.Add(Child{
		Name:     "database",
		Shutdown: time.Second,
		Start:    func(context.Context) (Process, error) { return first, nil },
	})
	s.Add(Child{
		Name:  "gateway",
		Start: func(context.Context) (Process, error) { return nil, errors.New("port in use") },
	})

	if err := s.Run(context.Background()); err == nil {
		t.Fatal("expected the failed start to be reported")
	}
	if stops, _ := first.counts(); stops == 0 {
		t.Fatal("the child that did start was left running")
	}
}

// waitFor polls until cond holds, so a test never depends on a sleep being long
// enough.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition never became true")
}
