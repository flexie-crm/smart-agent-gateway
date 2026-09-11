package api

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/app"
)

// Taking the port from a predecessor that is still letting go.
//
// This is the failure `make dev` produced over and over: the outgoing server
// drains gracefully, the replacement finds the port busy, gives up, and leaves
// nothing listening at all. The next thing anybody sees is a page saying the
// gateway did not answer.

func bindTestApp(t *testing.T) *app.App {
	t.Helper()
	return &app.App{Log: zerolog.Nop()}
}

// TestATakenPortIsRecognisedOnThisPlatform is the assertion the waiting rests
// on, and it asks the OPERATING SYSTEM what a taken port looks like rather than
// naming a number.
//
// That distinction is the whole test. The code compared against POSIX's
// EADDRINUSE, which exists on Windows too and carries its POSIX value there
// while Windows reports WSAEADDRINUSE (10048). So the comparison compiled, ran,
// and never matched: a restart classified "the previous server is still
// draining" as fatal, gave up at once on a port that was about to free, and left
// nothing listening. A test naming the constant would have agreed with the bug.
func TestATakenPortIsRecognisedOnThisPlatform(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = held.Close() }()

	second, err := net.Listen("tcp", held.Addr().String())
	if err == nil {
		_ = second.Close()
		t.Fatal("this platform let two listeners take one port, so nothing here means what it says")
	}
	if !addressInUse(err) {
		t.Fatalf("a taken port is reported as %v and is not recognised, "+
			"so a restart gives up instead of waiting for the previous server", err)
	}
}

func TestListenWaitsForAPredecessorToLetGo(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := held.Addr().String()

	// The predecessor lets go shortly, the way a graceful shutdown does.
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = held.Close()
	}()

	start := time.Now()
	listener, err := listen(context.Background(), bindTestApp(t), addr)
	if err != nil {
		t.Fatalf("the replacement gave up on a port that was about to free: %v", err)
	}
	defer func() { _ = listener.Close() }()

	if time.Since(start) < 250*time.Millisecond {
		t.Error("the port was taken before the predecessor released it")
	}
	if listener.Addr().String() != addr {
		t.Errorf("bound %s, want %s", listener.Addr(), addr)
	}
}

func TestListenGivesUpWhenTheHolderNeverLeaves(t *testing.T) {
	// Waiting forever would be its own kind of silence. It says what to do.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = held.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	if _, err := listen(ctx, bindTestApp(t), held.Addr().String()); err == nil {
		t.Fatal("two servers took the same port")
	}
}

func TestAnAddressThatCannotBeBoundAtAllFailsAtOnce(t *testing.T) {
	// Only "in use" is worth waiting on. A port we may never have, or an address
	// that is not one, is not going to become available.
	start := time.Now()
	if _, err := listen(context.Background(), bindTestApp(t), "256.256.256.256:9"); err == nil {
		t.Fatal("an impossible address was bound")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("a hopeless address was retried as though it might free up")
	}
}
