package skills

// One install of one version, however many agents ask for it.
//
// The Gateway starts a fleet, twenty agents hold the same skill, none of them
// has it, and twenty calls reach this tool within a second of each other. Each
// one asks the computer to run a script, each one is told the version is not
// there, and without anything here each one would read the whole package out of
// the database, encode it, and push its own copy down the link. Twenty times
// the work for one directory.
//
// So the first to hear "not there" installs it and the rest WAIT for that
// install, then carry on and run their script. Which is ordinary concurrency
// control and nothing to do with agents: the same shape as any cache that must
// be filled once.
//
// WHY HERE AND NOT IN THE CLIENT. The gateway is the half that knows twenty
// agents are asking about one machine; the client sees twenty unrelated calls.
// Coordinating where the knowledge is means the duplicate work never happens,
// rather than happening and being discarded on arrival.
//
// WHY NOT A CACHE OF WHAT A MACHINE HOLDS. Remembering "this computer has
// version 19" would skip even the first question, and would be wrong the moment
// somebody deletes the folder, with nothing to notice. Asking is one cheap round
// trip and it is self-correcting. What is held here is only what is HAPPENING,
// which cannot go stale: it ends.

import (
	"context"
	"errors"
	"sync"
	"time"
)

// How long a waiter gives the install it is waiting for.
//
// A ceiling rather than an expectation. The install itself is bounded by the
// machine being there rather than by a clock (KB/39), which is right for work
// somebody is waiting on and means a leader whose laptop has gone to sleep could
// otherwise hold twenty agents for as long as their turns live.
//
// Two minutes against a measurement rather than a feeling: a package of 303
// files crossing the real link to a real disk took 65-85ms over three runs
// (internal/app/skillcollision_test.go prints it). So this is more than a
// thousand times what the work costs, which is the right shape for a ceiling:
// it is never reached by an install that is merely slow, only by one that is
// not coming back.
//
// A value rather than a constant so the test that proves the ceiling exists can
// lower it: two minutes is right for the product and would make that test one
// nobody runs.
var waitForInstall = 2 * time.Minute

// installs is which (device, version) is being put on a computer right now.
//
// Keyed by the VERSION and not the skill, because the version is what the
// client's directory is named after and what an install is about. Keyed on the
// skill, an agent needing version 20 would wait for version 19's install and
// still be missing when it finished, which is a thing that happens when
// somebody re-imports a package mid-fleet.
//
// It holds channels, so it is not in internal/state: that package keeps facts
// which could be written down and read back, and says so. A live handle stays
// with the process that can use it.
type installs struct {
	mu      sync.Mutex
	running map[installKey]*installing
}

type installKey struct {
	workspaceID int64
	userID      int64
	deviceID    string
	versionID   int64
}

// installing is one install in flight: how to wait for it, and what it came to.
type installing struct {
	done chan struct{}
	err  error
}

// The one gate for this process. Process-wide because it is a fact about a
// MACHINE rather than about a turn, and two turns are exactly what it exists to
// hold apart.
var gate = &installs{running: map[installKey]*installing{}}

// once runs put exactly once per key, however many callers arrive.
//
// The first caller runs it. Anybody arriving while it runs waits for the same
// answer and does not run it again. Whatever it came to, the key is RELEASED:
// an install that failed is not remembered as impossible, so the next turn tries
// again rather than the whole fleet being told for ever that a package it never
// really tried is broken.
func (i *installs) once(ctx context.Context, key installKey, put func() error) error {
	i.mu.Lock()
	if flight, found := i.running[key]; found {
		i.mu.Unlock()
		return flight.wait(ctx)
	}
	flight := &installing{done: make(chan struct{})}
	i.running[key] = flight
	i.mu.Unlock()

	// The leader. Released on every path, including a panic: a key left behind
	// would hold every later caller until this process restarted.
	//
	// BOTH STEPS UNDER ONE LOCK, which is what makes this arrangement provable
	// rather than something to reason about. Released as two separate steps
	// there is an order to get right and a window between them: a caller
	// arriving inside it either finds a flight that is already finished (fine,
	// it reads the answer off a closed channel) or finds nothing and installs
	// again for a version that has just landed (a waste). Holding the lock
	// across both means no such moment exists. A caller either takes the lock
	// first and sees a flight it is guaranteed to get an answer from, or takes
	// it after and sees none, which is correct because the install really is
	// finished. Nothing waits while holding this lock (wait runs after the
	// unlock above), so closing a channel under it cannot deadlock.
	defer func() {
		i.mu.Lock()
		close(flight.done)
		delete(i.running, key)
		i.mu.Unlock()
	}()
	flight.err = put()
	return flight.err
}

// wait blocks until the install this caller is waiting on is finished, and
// answers with what it came to.
//
// Three ways out, and each is a different answer. It finished: take its result.
// This caller's own turn was cancelled or timed out: stop waiting, because
// nobody is listening any more. The ceiling: say so plainly, and let the caller
// try the script anyway, because the likeliest reason to be here is an install
// that finished while something else went wrong.
func (f *installing) wait(ctx context.Context) error {
	waited, stop := context.WithTimeout(ctx, waitForInstall)
	defer stop()
	select {
	case <-f.done:
		return f.err
	case <-waited.Done():
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errStillInstalling
	}
}

// errStillInstalling is the one outcome a caller treats specially: it has not
// been told the install failed, only that it has not finished in time.
var errStillInstalling = errors.New("the skill is still being put on this computer")
