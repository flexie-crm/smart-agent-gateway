package app

import (
	"context"
	"encoding/json"
	"sync"

	"flexie.io/sag/internal/queue"
)

// Stopping a fleet that is running somewhere else.
//
// Cancelling a background agent is a function call: the goroutine is in this
// process and the manager holds its cancel. A fleet member is not. It is on a
// worker, in another process, quite possibly on another machine, and the only
// thing the two share is the database and the broker.
//
// Marking the rows is not enough on its own. It stops the ANSWER (nothing is
// narrated, the conversation is not held), but the agent keeps working, keeps
// calling a model, and keeps costing money for an answer nobody will read. So
// the instruction has to reach the process actually doing it.
//
// It goes over the broker, broadcast rather than queued: a job is done once so
// it goes to one worker, an instruction is about whoever happens to be doing
// something so it goes to all of them. Each worker looks at what IT is running,
// and a worker holding none of that fleet does nothing.
//
// The database is still the record. The broadcast is how it becomes true
// sooner: a worker that never hears it finishes its member and finds the row
// already terminal, so its write is a no-op and nothing resurrects.

// fleetRuns is what a worker is running, so it can be told to stop.
//
// Keyed by FLEET first, because that is what a cancel names, and by delegation
// under it because one process can hold several members of one batch.
type fleetRuns struct {
	mu      sync.Mutex
	running map[int64]map[int64]context.CancelFunc
}

func newFleetRuns() *fleetRuns {
	return &fleetRuns{running: map[int64]map[int64]context.CancelFunc{}}
}

// add records a member this process is running and hands back the way to forget
// it. The caller defers the release, so the map holds what is live and nothing
// else.
func (f *fleetRuns) add(fleetID, delegationID int64, cancel context.CancelFunc) func() {
	f.mu.Lock()
	if f.running[fleetID] == nil {
		f.running[fleetID] = map[int64]context.CancelFunc{}
	}
	f.running[fleetID][delegationID] = cancel
	f.mu.Unlock()

	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.running[fleetID], delegationID)
		if len(f.running[fleetID]) == 0 {
			delete(f.running, fleetID)
		}
	}
}

// cancel stops every member of a fleet this process is running, and reports how
// many there were. Zero is the ordinary answer: most workers hold none of any
// given batch.
func (f *fleetRuns) cancel(fleetID int64) int {
	f.mu.Lock()
	members := f.running[fleetID]
	stops := make([]context.CancelFunc, 0, len(members))
	for _, stop := range members {
		stops = append(stops, stop)
	}
	f.mu.Unlock()

	// Outside the lock: a cancel unwinds a goroutine that takes the lock on its
	// way out, and holding it here would be waiting for what we just stopped.
	for _, stop := range stops {
		stop()
	}
	return len(stops)
}

// RunFleetOps listens for instructions about work this process is doing. The
// worker owns it; the server, which runs no fleet members, has nothing to stop.
func (a *App) RunFleetOps(ctx context.Context) {
	if a.Queue == nil {
		return
	}
	sub, err := a.Queue.Broadcast(ctx, fleetCancelSubject)
	if err != nil {
		a.Log.Error().Err(err).Msg("listen for fleet cancellations")
		return
	}
	defer func() { _ = sub.Close() }()

	// Live, like every other listener: an instruction to stop reaches a worker
	// as it is sent, because the work it is about is running right now.
	for {
		select {
		case <-ctx.Done():
			return
		case task, ok := <-sub.Tasks():
			if !ok {
				return
			}
			var op fleetCancel
			if err := json.Unmarshal(task.Payload, &op); err != nil || op.FleetID == 0 {
				continue
			}
			if n := a.fleets.cancel(op.FleetID); n > 0 {
				a.Log.Info().Int64("fleet_id", op.FleetID).Int("agents", n).
					Msg("stopped fleet members running here")
			}
		}
	}
}

// publishFleetCancel tells every worker to stop this batch.
//
// Best-effort, and the rows are already marked before it is called. A worker
// that never hears it does the work and finds the row terminal when it tries to
// write: wasteful, and correct. This is what turns "correct eventually" into
// "stopped now".
func (a *App) publishFleetCancel(ctx context.Context, fleetID int64) {
	if a.Queue == nil {
		return
	}
	payload, err := json.Marshal(fleetCancel{FleetID: fleetID})
	if err != nil {
		return
	}
	// No dedup id: two cancels for one fleet are the same instruction twice, and
	// the second finds nothing left to stop. Suppressing it would risk
	// suppressing the one that actually reaches a worker.
	if err := a.Queue.Enqueue(ctx, fleetCancelSubject, queue.Task{Kind: fleetCancelKind, Payload: payload}); err != nil {
		a.Log.Warn().Err(err).Int64("fleet_id", fleetID).Msg("publish fleet cancellation")
	}
}

// fleetCancelKind names the instruction on the wire. It is not a job kind: no
// handler is looked up by it, and nothing claims a row because of it.
const fleetCancelKind = "fleet.cancel"
