// Package state keeps the small facts a running system needs between requests:
// how far a batch has got, what was cached a few minutes ago.
//
// One place, so that the day these have to be shared between two gateways,
// there is one thing to change instead of a map in every package. A key, a
// value, and how long to keep it, which is what any store of this kind takes.
//
// WHAT BELONGS HERE: values that could be written down and read back somewhere
// else. A count, a status, a snapshot.
//
// WHAT NEVER DOES: a live handle. A websocket, a cancel function, an open SSH
// connection, an HTTP client. Those are not facts about the system, they are
// the system, held by the one process that can use them, and no store can carry
// one.
//
// Every place this system holds something in memory, and which it is, so that
// nobody has to survey them again:
//
//	fleetTracker record .......... a fact ......... here (internal/app)
//	fleetTracker timers .......... a handle ....... stays: a timer belongs to
//	                                               the process that set it
//	fleetRuns .................... handles ........ stays: cancel functions
//	backgroundManager.running .... handles ........ stays: cancel functions
//	backgroundManager.progress ... an accumulator . stays: a pointer callers
//	                                               fold stream frames into,
//	                                               and its durable copy is
//	                                               already a database column
//	link.Registry machines ....... handles ........ stays: websockets, and the
//	                                               ask/drop closures over them
//	sshtool pool ................. handles ........ stays: open connections
//	nodeauthority control ........ handles ........ stays: HTTP clients
//	query guard cache ............ a built object . stays: a compiled guard is
//	                                               not something bytes can
//	                                               carry; what it was built
//	                                               FROM could be, the day that
//	                                               is worth a rebuild per read
//
// The list is shorter than it looks because most of what a running system holds
// turns out to be handles. That is the useful finding: a shared store changes
// less than it seems, and the things it cannot change are the things that make
// a second gateway a real piece of work rather than a configuration change.
package state

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Store is somewhere to keep a value under a key, for a while.
type Store interface {
	// Get reads a value. The second result is whether there was one; a value
	// past its time is the same as no value.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set writes one. A ttl of zero keeps it until something removes it.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Delete removes a value, whether or not it was there.
	Delete(ctx context.Context, key string) error
}

// Read gets a value and reads it as JSON. A function rather than a method
// because a method cannot be generic.
func Read[T any](ctx context.Context, store Store, key string) (T, bool, error) {
	var value T
	raw, found, err := store.Get(ctx, key)
	if err != nil || !found {
		return value, false, err
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, false, fmt.Errorf("read %q: %w", key, err)
	}
	return value, true, nil
}

// Write puts a value in as JSON.
func Write[T any](ctx context.Context, store Store, key string, value T, ttl time.Duration) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("write %q: %w", key, err)
	}
	return store.Set(ctx, key, raw, ttl)
}
