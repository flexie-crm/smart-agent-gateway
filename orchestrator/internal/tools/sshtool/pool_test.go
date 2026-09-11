package sshtool

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"flexie.io/sag/internal/tool"
)

// testAgent stands in for the agent a call belongs to: a sign-in and a session
// belong to the agent that raised them, so the tests name one. testOtherAgent is
// a second agent on the same machine, which is what a Gateway and its background
// agents are.
const (
	testAgent      tool.Owner = "agent:test"
	testOtherAgent tool.Owner = "agent:other"
)

// The pool is what keeps one connection serving many calls. What has to hold:
// a second call reuses the first one's connection, a server that is full says so
// instead of signing in again, a released slot is available at once, and nothing
// is left connected after an idle spell or a shutdown.

func poolConfig(srv *testServer, channels int) Config {
	cfg := serverConfig(srv, Auth{Password: "hunter2"})
	cfg.Limits.MaxChannels = channels
	return cfg
}

func TestPoolSignsInOnceAndReuses(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	p := newPool()
	defer p.Close()

	cfg := poolConfig(srv, 4)

	first, err := p.acquire(context.Background(), cfg, testAgent)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	server, err := p.server(cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	client := server.current()
	if client == nil {
		t.Fatal("acquiring a slot did not connect")
	}
	first.release()

	second, err := p.acquire(context.Background(), cfg, testAgent)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	defer second.release()

	if server.current() != client {
		t.Fatal("the second call signed in again instead of reusing the connection")
	}
}

// Two tools pointing at the same server with the same credential are the same
// server, and share its connection and its slots.
func TestPoolSharesOneServerBetweenIdenticalSettings(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	p := newPool()
	defer p.Close()

	a, err := p.server(poolConfig(srv, 4))
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	b, err := p.server(poolConfig(srv, 4))
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if a != b {
		t.Fatal("the same settings produced two servers")
	}

	// A different setting is a different server, so an edited tool never keeps
	// talking to the machine it used to point at.
	other, err := p.server(poolConfig(srv, 5))
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if other == a {
		t.Fatal("changing a setting kept the old connection")
	}
}

func TestPoolReportsBusyWhenEverySlotIsHeld(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	p := newPool()
	defer p.Close()

	cfg := poolConfig(srv, 1)
	held, err := p.acquire(context.Background(), cfg, testAgent)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// The wait is what a real call would sit through; shorten the test's patience
	// by cancelling instead, then prove the busy answer with the real path.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.acquire(ctx, cfg, testAgent); err == nil {
		t.Fatal("a second call took a slot that was held")
	}

	held.release()

	free, err := p.acquire(context.Background(), cfg, testAgent)
	if err != nil {
		t.Fatalf("a released slot was not free: %v", err)
	}
	free.release()
}

// The busy answer itself, on the real path: with the wait shortened so the test
// does not sit through it.
func TestPoolBusyAnswerIsRetryable(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	p := newPool()
	defer p.Close()

	server, err := p.server(poolConfig(srv, 1))
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	server.slots <- struct{}{} // the one slot, taken

	start := time.Now()
	_, err = server.acquire(context.Background(), testAgent)
	if !errors.Is(err, errBusy) {
		t.Fatalf("got %v, want the busy answer", err)
	}
	if waited := time.Since(start); waited < slotWait {
		t.Fatalf("gave up after %s without waiting the %s a call is given", waited, slotWait)
	}
	<-server.slots
}

// A call that has been sitting on a connection nobody is using gets a fresh one,
// rather than a stale connection the server has long since forgotten.
func TestPoolClosesIdleConnections(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	p := newPool()
	defer p.Close()

	cfg := poolConfig(srv, 2)
	cfg.Limits.IdleMinutes = 1

	lease, err := p.acquire(context.Background(), cfg, testAgent)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lease.release()

	server, err := p.server(cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if server.current() == nil {
		t.Fatal("nothing was connected")
	}

	// Not yet idle: still connected.
	p.closeIdle(time.Now().Add(30 * time.Second))
	if server.current() == nil {
		t.Fatal("a connection was closed before its idle time was up")
	}

	p.closeIdle(time.Now().Add(2 * time.Minute))
	if server.current() != nil {
		t.Fatal("an idle connection was left open")
	}

	// And the next call simply connects again.
	again, err := p.acquire(context.Background(), cfg, testAgent)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	again.release()
}

// Work in flight keeps the connection, however long the call has been running.
func TestPoolKeepsAConnectionSomethingIsUsing(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	p := newPool()
	defer p.Close()

	cfg := poolConfig(srv, 2)
	cfg.Limits.IdleMinutes = 1

	lease, err := p.acquire(context.Background(), cfg, testAgent)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lease.release()

	p.closeIdle(time.Now().Add(time.Hour))

	server, err := p.server(cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if server.current() == nil {
		t.Fatal("a connection with work on it was closed")
	}
}

// A connection dropped between calls must not be handed out: the next call signs
// in again rather than failing on a connection that is no longer there.
func TestPoolRecoversFromADroppedConnection(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	p := newPool()
	defer p.Close()

	cfg := poolConfig(srv, 2)
	lease, err := p.acquire(context.Background(), cfg, testAgent)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	server, err := p.server(cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	dropped := server.current()
	lease.release()

	// The far end goes away without telling us.
	_ = dropped.Close()

	next, err := p.acquire(context.Background(), cfg, testAgent)
	if err != nil {
		t.Fatalf("acquire after the connection dropped: %v", err)
	}
	defer next.release()

	session, err := next.session(context.Background(), testAgent)
	if err != nil {
		t.Fatalf("the dropped connection was handed back: %v", err)
	}
	_ = session.Close()
	if server.current() == dropped {
		t.Fatal("the closed connection is still the current one")
	}
}

// Shutdown ends everything the pool holds, and a call arriving afterwards is
// answered rather than handed a connection that is being torn down.
func TestPoolCloseDisconnectsAndRefuses(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	p := newPool()

	cfg := poolConfig(srv, 2)
	lease, err := p.acquire(context.Background(), cfg, testAgent)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	server, err := p.server(cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	lease.release()

	p.Close()

	if server.current() != nil {
		t.Fatal("shutdown left a connection open")
	}
	if _, err := p.acquire(context.Background(), cfg, testAgent); !errors.Is(err, errPoolClosed) {
		t.Fatalf("got %v, want the shutting-down answer", err)
	}
	// Closing twice is what a shutdown path may well do, and must be harmless.
	p.Close()
}

// The server's own limit, not ours, is what decides when to sign in again.
//
// sshd carries only so many jobs at once on one connection (MaxSessions, ten by
// default) and refuses the rest. Refusal arrives in the middle of somebody's
// work and reads as a broken tool, so the pool counts what it has handed out
// and opens another connection before reaching it. That is what lets "jobs at
// once" be a number about the work rather than a number about the protocol.
func TestASecondConnectionOpensWhenOneIsFull(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	cfg := serverConfig(srv, Auth{Password: "hunter2"})
	cfg.Limits.MaxChannels = 6
	cfg.Limits.JobsPerConnection = 2

	p := newPool()
	t.Cleanup(p.Close)

	// Two jobs fit on one sign-in.
	var held []*lease
	for i := 0; i < 2; i++ {
		lease, err := p.acquire(context.Background(), cfg, testAgent)
		if err != nil {
			t.Fatalf("job %d: %v", i, err)
		}
		held = append(held, lease)
	}
	server, err := p.server(cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if got := server.connections(); got != 1 {
		t.Fatalf("two jobs took %d connections, want them to share one", got)
	}

	// The third does not fit, so it gets a connection of its own rather than a
	// refusal from the far end.
	third, err := p.acquire(context.Background(), cfg, testAgent)
	if err != nil {
		t.Fatalf("the third job was refused instead of signing in again: %v", err)
	}
	held = append(held, third)
	if got := server.connections(); got != 2 {
		t.Fatalf("the third job is on %d connections, want a second one opened for it", got)
	}

	// Every one of them can actually run: a connection that was counted but not
	// usable would be worse than not having it.
	for i, lease := range held {
		session, err := lease.session(context.Background(), testAgent)
		if err != nil {
			t.Fatalf("job %d could not open a session: %v", i, err)
		}
		_ = session.Close()
	}

	// Giving the room back puts the next job on an existing connection rather
	// than signing in a third time.
	held[0].release()
	next, err := p.acquire(context.Background(), cfg, testAgent)
	if err != nil {
		t.Fatalf("after a job finished: %v", err)
	}
	defer next.release()
	if got := server.connections(); got != 2 {
		t.Fatalf("a freed job's room was not reused: %d connections", got)
	}
}

// Beyond every connection's room, the answer is "busy, try again" and not a
// failure. Telling an agent to give up on a server that is merely full is how
// work gets abandoned that would have run a second later.
func TestAFullServerIsRetryableRatherThanFailed(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	cfg := serverConfig(srv, Auth{Password: "hunter2"})
	cfg.Limits.MaxChannels = 1
	cfg.Limits.JobsPerConnection = 1

	p := newPool()
	t.Cleanup(p.Close)

	held, err := p.acquire(context.Background(), cfg, testAgent)
	if err != nil {
		t.Fatalf("first job: %v", err)
	}
	defer held.release()

	_, err = p.acquire(context.Background(), cfg, testAgent)
	if !errors.Is(err, errBusy) {
		t.Fatalf("a full server gave %v, want the retryable busy answer", err)
	}
}

// A crowd arriving on a server with no connection at all.
//
// This is what a fleet looks like at the start: ten agents given the same work
// at the same moment, on a machine nothing is connected to. Two things have to
// hold at once, and they pull against each other. They must all wait on ONE
// sign-in, or a server that wants a verification code asks ten people for ten
// codes. And they must not all be put onto that one connection, because a
// server carries only so many at once and refuses the rest, which arrives as a
// failure in the middle of somebody's work.
func TestACrowdArrivingAtOnceOnAColdServer(t *testing.T) {
	signIns := &counter{}
	srv := startServer(t, serverOptions{Password: "hunter2", OnSignIn: signIns.inc})
	cfg := serverConfig(srv, Auth{Password: "hunter2"})
	cfg.Limits.MaxChannels = 10
	cfg.Limits.JobsPerConnection = 4

	p := newPool()
	t.Cleanup(p.Close)

	// Ten at once, as close to simultaneous as a test can make them.
	const crowd = 10
	var wg sync.WaitGroup
	leases := make([]*lease, crowd)
	errs := make([]error, crowd)
	start := make(chan struct{})
	for i := 0; i < crowd; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			leases[i], errs[i] = p.acquire(context.Background(), cfg, tool.Owner(strconv.Itoa(i)))
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("agent %d was turned away on a server that had room: %v", i, err)
		}
	}

	// Every one of them can actually work. A connection counted but over its
	// limit fails here, which is the failure this is all about avoiding.
	for i, held := range leases {
		session, err := held.session(context.Background(), tool.Owner(strconv.Itoa(i)))
		if err != nil {
			t.Fatalf("agent %d could not run: %v", i, err)
		}
		_ = session.Close()
	}

	// Ten agents, four to a connection: three connections, and no more.
	server, err := p.server(cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if got := server.connections(); got != 3 {
		t.Fatalf("ten agents took %d connections, want 3 at four jobs each", got)
	}
	// And each connection was one sign-in: nobody signed in speculatively.
	if got := signIns.get(); got != 3 {
		t.Fatalf("the server was signed in to %d times for 3 connections", got)
	}

	for _, held := range leases {
		held.release()
	}
}

// counter counts something the test server did, from its own goroutines.
type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) inc() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func (c *counter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
