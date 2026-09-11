// Package worker is the entry wiring for jobs mode. It starts no HTTP
// stack; it attaches queue consumers for the capability subjects this node
// serves (see config.WorkerCapabilities) and the scheduler scan loop.
package worker

import (
	"context"
	"fmt"
	"strings"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
)

// Run is the worker process: a pool of goroutines per capability it serves,
// each asking the queue for work and claiming rows from the database.
//
// One process, N goroutines, and a script starting M processes when a machine
// wants more than one address space (KB/35). The goroutines of one process share
// its connections, which is the whole reason the default shape is one process
// with many goroutines rather than many processes with one each.
func Run(ctx context.Context, a *app.App) error {
	caps := capabilities(a.Config.WorkerCapabilities)
	if len(caps) == 0 {
		return fmt.Errorf("worker: no capabilities configured")
	}
	if a.Queue == nil {
		return fmt.Errorf("worker: no queue connected")
	}

	a.Log.Info().Strs("capabilities", caps).Int("goroutines", a.Config.WorkerConcurrency).
		Msg("worker started")

	// Instructions about work already in flight, which no pool can carry: a job
	// is handed to one worker, but "stop that batch" is about whichever process
	// happens to be running it, so it is broadcast and every worker listens.
	go a.RunFleetOps(ctx)

	// One pool per capability, because a pool's subject prefix IS its
	// capability: what a node picks up is decided by how it was started, not by
	// a list somebody keeps in the database.
	errs := make(chan error, len(caps))
	for _, capability := range caps {
		pool := New(a.Store, a.Queue, a.Log, capability, a.Config.WorkerConcurrency)
		// Everything this build knows how to do, named in one place. A kind with
		// no handler here is not an error: another node may serve it.
		pool.Handle(model.JobKindAgentRun, a.RunFleetMemberJob)
		pool.Handle(model.JobKindAgentResume, a.RunFleetResumeJob)
		pool.Handle(model.JobKindModelPull, a.RunModelPullJob)

		go func(capability string, pool *Pool) {
			errs <- pool.Run(ctx, capability)
		}(capability, pool)
	}

	select {
	case <-ctx.Done():
		a.Log.Info().Msg("worker stopping")
		return nil
	case err := <-errs:
		return err
	}
}

// capabilities reads the configured list, dropping the blanks a comma-separated
// setting collects as somebody edits it.
func capabilities(configured string) []string {
	out := []string{}
	for _, c := range strings.Split(configured, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}
