package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestAStopRequestEndsTheRun(t *testing.T) {
	state := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stopped := make(chan struct{})
	go watchForStopRequest(ctx, state, zerolog.Nop(), func() {
		close(stopped)
		cancel()
	})

	if err := os.WriteFile(filepath.Join(state, stopFileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway was asked to close and did not, so closing the window leaves it running")
	}

	// Spent as it was acted on. Left behind, it would stop the next gateway
	// started in this directory the moment that one was ready.
	if _, err := os.Stat(filepath.Join(state, stopFileName)); !os.IsNotExist(err) {
		t.Fatalf("the request is still there: %v", err)
	}
}

// TestARequestMeantForAPreviousRunDoesNotStopThisOne is why clearing comes
// first.
//
// The file outlives the process it was written for. A gateway that was ended
// some other way (the last resort taskkill, a power cut) leaves it on disk, and
// a launch that read it would create its database, apply every migration, seed
// itself, and then immediately quit. From the outside that is a first run
// crashing.
func TestARequestMeantForAPreviousRunDoesNotStopThisOne(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, stopFileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := clearStopRequest(state); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopped := make(chan struct{})
	go watchForStopRequest(ctx, state, zerolog.Nop(), func() { close(stopped) })

	select {
	case <-stopped:
		t.Fatal("a request left by a previous run closed this one")
	case <-time.After(10 * stopPollEvery):
	}
}

func TestClearingWhenThereIsNothingToClearIsNotAFailure(t *testing.T) {
	// The common case by far: every first run, and every run after a normal
	// close, has no request to clear.
	if err := clearStopRequest(t.TempDir()); err != nil {
		t.Fatalf("a directory with no request in it was reported as a problem: %v", err)
	}
}

func TestTheWatcherEndsWithTheRunItBelongsTo(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		watchForStopRequest(ctx, t.TempDir(), zerolog.Nop(), func() {})
		close(done)
	}()

	// Shutting down for any other reason (a signal, the supervisor giving up)
	// must take this with it rather than leave a goroutine polling a directory
	// nobody is going to write to.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher outlived the run")
	}
}
