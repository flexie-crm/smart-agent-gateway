package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"flexie.io/sag/internal/state"
)

// What a person says while their conversation is still answering.
//
// A conversation answers one thing at a time, and that used to mean anything
// typed mid-answer was refused: the server said "this conversation is already
// answering" and the chat dropped it without a word. But somebody typing while
// the assistant works is usually correcting what they asked for, and making them
// wait for an answer they have already changed their mind about is the wrong end
// of the trade.
//
// So it is kept here, briefly, and the turn picks it up at its next step, which
// is the same place it picks up a tool result. The model then sees it before
// deciding what to do next, and drifts to what it has just been told.
//
// WHERE IT LIVES, and why it is not somewhere more obvious:
//
//   - Not on the event bus. That bus is documented as carrying "reactions, never
//     obligations", best-effort, dropping under load, explicitly not for control
//     flow (KB/30). A person's words reaching the model is an obligation, and a
//     dropped one is the exact bug being fixed here.
//   - Not a field on the running turn. It is a VALUE, not a live handle: bytes
//     under a key, which is precisely what internal/state is for, and it is the
//     half of this that a second gateway could one day read.
//
// The lock is this file's own, because the store has no atomic append and two
// people (or one person typing twice) would otherwise read the same list and
// write back over each other. It is honest about the boundary: the day this runs
// on two gateways, the store needs the append rather than this mutex.
type Said struct {
	store state.Store
	mu    sync.Mutex
}

// heldFor is how long words wait to be picked up.
//
// Generous next to a turn, which is seconds, and short enough that something
// nobody ever collected does not sit there. Anything still here when a turn ends
// is started as a turn of its own rather than left to expire.
const heldFor = 10 * time.Minute

func newSaid(store state.Store) *Said { return &Said{store: store} }

func saidKey(sessionID int64) string { return fmt.Sprintf("chat:said:%d", sessionID) }

// Add puts something at the end of what this conversation has been told.
func (s *Said) Add(ctx context.Context, sessionID int64, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	waiting, _, err := state.Read[[]string](ctx, s.store, saidKey(sessionID))
	if err != nil {
		return err
	}
	return state.Write(ctx, s.store, saidKey(sessionID), append(waiting, text), heldFor)
}

// Take returns everything said since it was last called, and forgets it. Empty
// when nobody has said anything, which is almost always.
func (s *Said) Take(ctx context.Context, sessionID int64) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	waiting, found, err := state.Read[[]string](ctx, s.store, saidKey(sessionID))
	if err != nil || !found || len(waiting) == 0 {
		return nil
	}
	if err := s.store.Delete(ctx, saidKey(sessionID)); err != nil {
		// Taken but not cleared would say the same thing twice, which is worse
		// than not saying it at all.
		return nil
	}
	return waiting
}
