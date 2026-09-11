package app

import (
	"context"
	"strconv"
	"sync"
	"time"

	"flexie.io/sag/internal/state"
)

// What the master remembers about a batch it dispatched.
//
// The database is still the truth. This is the master's own live picture of
// work it started, and it exists so that hearing "one more is back" costs
// nothing: a number goes up, a chip moves on somebody's screen, and that is the
// whole of it. No query, no round trip, no reason for one report to wait behind
// another.
//
// The database is read ONCE per batch, when the last member is in and there is
// finally something to say: the results, the close, the Gateway's turn. Which
// is the only moment the answer is worth the read.
//
// Losing this map costs nothing that matters. A restart drops it, and the sweep
// finds the same batches by asking the database what it always knew; what is
// lost is the liveness of a chip on a page that is being reloaded anyway.

// fleetTrack is one dispatched batch, as the master watches it.
//
// Everything the chip needs is captured when the batch is dispatched, because
// that is the moment it is all in hand: who the agents are, which conversation
// they belong to, whose screen to push to. Reading it back per report would be
// the queries this exists to avoid.
type fleetTrack struct {
	FleetID     int64  `json:"fleet_id"`
	WorkspaceID int64  `json:"workspace_id"`
	UserID      int64  `json:"user_id"`
	SessionUID  string `json:"session_uid"`
	Name        string `json:"name"`
	Size        int    `json:"size"`
	// done is how many members are back, however they ended.
	Done int `json:"done"`
	// members is which delegations have reported, so the same member reporting
	// twice (at-least-once delivery) counts once. A count that can be pushed
	// past its own size is a chip that reads "4 of 3".
	Members map[int64]bool `json:"members"`
	// deadline is when this batch stops waiting. The TIMER that makes it exact
	// is not here: a timer belongs to the process that set it and cannot be
	// shared, so it lives beside this record rather than in it (see below).
	Deadline time.Time `json:"deadline"`
}

// fleetTracker is every batch this process has dispatched and not yet finished.
//
// The RECORD of a batch is state (internal/state): numbers, ids, which members
// have reported. It is kept in a store rather than a map here, so that the day
// there are two gateways the second one can answer about a batch the first
// dispatched, instead of saying it has never heard of it.
//
// The TIMER is not, and cannot be. A timer is a thing this process is holding;
// no store can carry one, and a second gateway learning that a deadline exists
// could not fire it. So the timers stay in a map here, owned by the process
// that dispatched the batch, and the record they belong to lives in the store.
// That split IS the design: what could be written down goes to the store, what
// is a live handle stays where it is held.
type fleetTracker struct {
	state state.Store
	mu    sync.Mutex
	// timers, by batch, for the batches THIS process dispatched.
	timers map[int64]*time.Timer
}

// howLongABatchIsRemembered bounds a record nobody Closed: a gateway that went
// down between dispatching a batch and hearing back leaves one behind, and a
// store with no expiry keeps it for ever. Longer than any batch is given.
const howLongABatchIsRemembered = 24 * time.Hour

func newFleetTracker(keep state.Store) *fleetTracker {
	return &fleetTracker{state: keep, timers: map[int64]*time.Timer{}}
}

// fleetKey is where one batch's record lives.
func fleetKey(fleetID int64) string {
	return "fleet/" + strconv.FormatInt(fleetID, 10)
}

// watch starts following a batch, and starts its clock. Called once, as the
// batch is dispatched.
//
// onExpire is called if the deadline arrives before every member is back. It
// runs on its own goroutine, and it will not be called for a batch that
// finished, because finishing stops the timer.
func (t *fleetTracker) watch(f *fleetTrack, onExpire func(fleetID int64)) {
	f.Members = map[int64]bool{}
	if err := state.Write(context.Background(), t.state, fleetKey(f.FleetID), *f, howLongABatchIsRemembered); err != nil {
		// The batch still runs; what is lost is the fast path, and every
		// report then falls back to the database, which is always right.
		return
	}
	t.mu.Lock()
	if !f.Deadline.IsZero() && onExpire != nil {
		id := f.FleetID
		t.timers[id] = time.AfterFunc(time.Until(f.Deadline), func() { onExpire(id) })
	}
	t.mu.Unlock()
}

// record counts one member as back and says where the batch now stands.
//
// complete is true exactly once per batch, for the caller that brought the last
// member in. Everyone else gets false and has nothing to do beyond moving the
// chip, which is the point: only one report in a batch ever touches the
// database, and only the last one.
//
// known is false when this process never dispatched the batch (a restart, or
// another server did). The caller then falls back to asking the database, which
// is slower and always right.
func (t *fleetTracker) record(fleetID, delegationID int64) (snapshot fleetTrack, known, complete bool) {
	// The whole read-change-write under this tracker's lock, which is what
	// makes "complete exactly once" true when two reports arrive together:
	// without it both read seven, both write eight, and the batch never
	// finishes. The lock is enough while there is one gateway. Two of them
	// would need the store to do this in one operation, and that is the day to
	// add it rather than today.
	t.mu.Lock()
	defer t.mu.Unlock()

	f, found, err := state.Read[fleetTrack](context.Background(), t.state, fleetKey(fleetID))
	if err != nil || !found {
		// Never dispatched here (a restart, or another server did). The caller
		// falls back to the database, which is slower and always right.
		return fleetTrack{}, false, false
	}
	if f.Members == nil {
		f.Members = map[int64]bool{}
	}
	if !f.Members[delegationID] {
		f.Members[delegationID] = true
		f.Done++
	}
	complete = f.Done >= f.Size
	if complete {
		// Handed over: whoever got `complete` owns finishing this batch, and
		// the record goes, so a duplicate report finds nothing to claim. The
		// clock stops with it, so nothing expires a batch already home.
		t.stopTimerLocked(fleetID)
		_ = t.state.Delete(context.Background(), fleetKey(fleetID))
		return f, true, true
	}
	if err := state.Write(context.Background(), t.state, fleetKey(fleetID), f, howLongABatchIsRemembered); err != nil {
		return fleetTrack{}, false, false
	}
	return f, true, false
}

// snapshot is where a batch stands, without touching it. For anything that has
// to draw the chip outside a report: the numbers are already here.
func (t *fleetTracker) snapshot(fleetID int64) (fleetTrack, bool) {
	f, found, err := state.Read[fleetTrack](context.Background(), t.state, fleetKey(fleetID))
	if err != nil || !found {
		return fleetTrack{}, false
	}
	return f, true
}

// forget drops a batch this process is no longer following (it was cancelled,
// or the sweep closed it).
func (t *fleetTracker) forget(fleetID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopTimerLocked(fleetID)
	_ = t.state.Delete(context.Background(), fleetKey(fleetID))
}

// expire claims a batch because its time is up, and reports whether this call
// was the one that claimed it. False means it finished, or was cancelled,
// between the timer firing and this running.
func (t *fleetTracker) expire(fleetID int64) bool {
	// Claimed under the same lock completion takes, so a timer firing at the
	// moment the last member reports does not give up on a batch that is home.
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, found, err := state.Read[fleetTrack](context.Background(), t.state, fleetKey(fleetID)); err != nil || !found {
		return false
	}
	t.stopTimerLocked(fleetID)
	_ = t.state.Delete(context.Background(), fleetKey(fleetID))
	return true
}

// stopTimerLocked ends this process's clock for a batch, if it set one. Held
// with the tracker's lock.
func (t *fleetTracker) stopTimerLocked(fleetID int64) {
	if timer, ok := t.timers[fleetID]; ok {
		timer.Stop()
		delete(t.timers, fleetID)
	}
}

// chip is the batch as the browser draws it, built entirely from memory.
func (f fleetTrack) chip(status string) Chip {
	return Chip{
		ID:      FleetChipID(f.FleetID),
		Kind:    ChipKindFleet,
		Name:    f.Name,
		Status:  status,
		Agents:  f.Size,
		Done:    f.Done,
		ChatUID: f.SessionUID,
	}
}

// String names a batch in a log line the way somebody reading it thinks of it.
func (f fleetTrack) String() string {
	return "fleet " + strconv.FormatInt(f.FleetID, 10) + " (" +
		strconv.Itoa(f.Done) + "/" + strconv.Itoa(f.Size) + ")"
}
