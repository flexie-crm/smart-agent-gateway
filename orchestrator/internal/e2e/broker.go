package e2e

import (
	"context"
	"fmt"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/worker"
)

// A broker and a worker, inside the harness process.
//
// A fleet (Mode B) is the one flow that is not one process talking to itself:
// the Gateway writes rows and puts jobs on a queue, a WORKER claims them and
// runs the agents, and the server hears reports and wakes the Gateway. Testing
// it with a fake queue would test the fake, and the two bugs this harness was
// built after were both in the seams a fake would have papered over: a report
// that reached nobody, and a completion turn nobody was told to listen to.
//
// So the harness runs the real broker and the real worker pool, in-process. It
// is exactly the production wiring with the process boundary collapsed, which
// leaves the boundary itself (two OS processes, two machines) as the only thing
// it does not prove. Everything above it is real: real JetStream, real job rows,
// real claims, real reports.

// StartBroker brings up an embedded broker on a port the OS picks, connects the
// app to it, and hands back the shutdown.
//
// The port is chosen rather than fixed so a developer with a broker of their own
// already running is not fighting it for 4222, and so two harnesses can run at
// once.
func StartBroker(ctx context.Context, a *app.App, storeDir string) (func(), error) {
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // an unused port, chosen by the OS
		JetStream: true,
		StoreDir:  storeDir,
		NoLog:     true,
		NoSigs:    true,
	})
	if err != nil {
		return nil, fmt.Errorf("build the e2e broker: %w", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		srv.Shutdown()
		return nil, fmt.Errorf("the e2e broker did not come up")
	}

	a.Config.NATSURL = srv.ClientURL()
	if err := a.ConnectQueue(ctx); err != nil {
		srv.Shutdown()
		return nil, fmt.Errorf("connect the e2e queue: %w", err)
	}
	return func() {
		if a.Queue != nil {
			_ = a.Queue.Close()
		}
		srv.Shutdown()
	}, nil
}

// StartWorker runs the real worker pool in this process, on the same app.
//
// Sized so it is never the thing being measured. Playwright runs the spec files
// in parallel, so several batches can be in flight at once, and a pool too small
// to hold them turns "the card did not arrive" into a queue that had not got
// round to the agent yet: a real failure and a harness artefact reading exactly
// the same. It stops when ctx is cancelled.
func StartWorker(ctx context.Context, a *app.App) {
	pool := worker.New(a.Store, a.Queue, a.Log, "default", 16)
	pool.Handle(model.JobKindAgentRun, a.RunFleetMemberJob)
	pool.Handle(model.JobKindAgentResume, a.RunFleetResumeJob)

	// The instruction channel, exactly as a worker process listens on it, so a
	// cancelled batch is stopped here rather than merely marked.
	go a.RunFleetOps(ctx)
	go func() {
		if err := pool.Run(ctx, "default"); err != nil {
			a.Log.Error().Err(err).Msg("e2e worker pool stopped")
		}
	}()
}
