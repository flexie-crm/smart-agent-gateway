// Package run detaches a turn from whoever asked for it.
//
// A turn used to execute on the HTTP request that started it, which meant the
// browser tab owned the vendor call: close it, and the answer was cancelled
// mid-sentence and thrown away. That is a constraint of request-scoped
// runtimes, and it is precisely the constraint this project exists to leave
// behind.
//
// So a turn is a RUN. The request starts it and then merely LISTENS to it. The
// run keeps its own context, streams from the vendor whether or not anyone is
// attached, writes every step to the database as it goes, and remembers the
// frames it emitted. A reader who disconnects and comes back gets everything
// they missed and then rejoins the live stream, mid-sentence if that is where
// it is.
//
// Because nothing is holding the run open on the reader's behalf, a reader who
// leaves cannot stop it either. Stopping is now an explicit act (Cancel), which
// is the honest trade: a turn that costs money should end because someone said
// so, not because a network hiccup happened to look like a decision.
package run

import (
	"context"
	"sync"

	"flexie.io/sag/internal/chat"
)

// subscriberBuffer is how many frames a reader may fall behind by.
//
// A reader that cannot keep up is dropped rather than allowed to stall the
// vendor stream, and dropping is safe: the log holds everything, so the reader
// reattaches and replays from where it was. Backpressure from a slow browser
// must never reach the model.
const subscriberBuffer = 512

// Run is one turn, executing.
type Run struct {
	ID          int64
	UID         string
	SessionID   int64
	UserID      int64
	WorkspaceID int64

	mu     sync.Mutex
	log    []chat.Frame
	subs   map[int]chan chat.Frame
	nextID int
	done   bool
	cancel context.CancelFunc
}

func newRun(id int64, uid string, sessionID, userID, workspaceID int64, cancel context.CancelFunc) *Run {
	return &Run{
		ID: id, UID: uid,
		SessionID: sessionID, UserID: userID, WorkspaceID: workspaceID,
		subs:   make(map[int]chan chat.Frame),
		cancel: cancel,
	}
}

// Write records a frame and hands it to everyone listening. It is the
// chat.Sink the runtime writes into, and it never blocks on a reader.
func (r *Run) Write(frame chat.Frame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return nil
	}

	frame.Index = len(r.log)
	r.log = append(r.log, frame)

	for id, sub := range r.subs {
		select {
		case sub <- frame:
		default:
			// This reader is too far behind. Drop it rather than stall the
			// turn: it can reattach and replay from the log, which lost
			// nothing.
			close(sub)
			delete(r.subs, id)
		}
	}
	if frame.Final {
		r.finish()
	}
	return nil
}

// Subscribe returns everything after the given index, then the live frames.
//
// after is the index the reader last saw. A reader starting fresh passes -1 and
// gets the run from the beginning, which is what a reloaded page wants: the
// answer so far, then the rest of it as it arrives.
//
// The returned channel closes when the run ends or the reader is dropped. The
// cancel function detaches without ending the run: a reader leaving is not a
// decision to stop the work.
func (r *Run) Subscribe(after int) (<-chan chat.Frame, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()

	backlog := []chat.Frame{}
	if after < len(r.log)-1 {
		start := after + 1
		if start < 0 {
			start = 0
		}
		backlog = r.log[start:]
	}

	// The backlog is delivered through the same channel as the live frames, so
	// a reader cannot observe the seam between what it missed and what is
	// happening now.
	sub := make(chan chat.Frame, len(backlog)+subscriberBuffer)
	for _, frame := range backlog {
		sub <- frame
	}

	if r.done {
		// Nothing more is coming. The reader gets the whole run and the channel
		// closes, so a page reloaded after the answer finished still renders it
		// without waiting.
		close(sub)
		return sub, func() {}
	}

	id := r.nextID
	r.nextID++
	r.subs[id] = sub

	return sub, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if sub, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(sub)
		}
	}
}

// Cancel ends the run. It is what a person pressing stop means, and it is the
// only thing that means it: a reader disconnecting does not.
func (r *Run) Cancel() { r.cancel() }

// Done reports whether the run has finished.
func (r *Run) Done() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

// Frames returns the run's log so far.
func (r *Run) Frames() []chat.Frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]chat.Frame(nil), r.log...)
}

// finish closes the run to writers and readers alike. The caller holds the lock.
func (r *Run) finish() {
	if r.done {
		return
	}
	r.done = true
	for id, sub := range r.subs {
		close(sub)
		delete(r.subs, id)
	}
}
