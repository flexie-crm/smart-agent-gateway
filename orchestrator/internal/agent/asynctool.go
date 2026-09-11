package agent

import (
	"context"
	"time"

	"flexie.io/sag/internal/tool"
)

// Running a tool off the loop's goroutine.
//
// A tool marked async is one that can block on the outside world, a network
// request that sits waiting on a far end. The stream stays alive regardless
// (runWithHeartbeat pings while any tool runs), so async here buys two things:
// a per-tool deadline independent of the turn, and the isolation that lets the
// work move to a worker node later without the loop changing. The in-process
// runner below spawns a goroutine and awaits it; that is the seam where a
// queue-backed runner slots in, exactly as the memory queue does.

// defaultAsyncTimeout bounds an async tool that declared no timeout of its own.
const defaultAsyncTimeout = 90 * time.Second

// runAsyncTool runs a tool handler on its own goroutine under a fresh deadline
// derived from the turn's context, so a slow tool cannot outlive its budget and
// the handler (an HTTP client, say) is cancelled when the deadline passes. On
// timeout it returns the context error, which the caller reports as an ordinary
// tool failure the model can work around.
func runAsyncTool(ctx context.Context, timeout time.Duration, run func(context.Context) (tool.Result, error)) (tool.Result, error) {
	if timeout <= 0 {
		timeout = defaultAsyncTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type outcome struct {
		res tool.Result
		err error
	}
	// Buffered so the goroutine can always send and exit even if we have already
	// returned on the deadline: no leak, the handler just finishes into the void.
	ch := make(chan outcome, 1)
	go func() {
		res, err := run(ctx)
		ch <- outcome{res, err}
	}()

	select {
	case <-ctx.Done():
		return tool.Result{}, ctx.Err()
	case o := <-ch:
		return o.res, o.err
	}
}

// asyncTimeout is a schema's declared deadline, or the default.
func asyncTimeout(s tool.Schema) time.Duration {
	if s.AsyncTimeout > 0 {
		return s.AsyncTimeout
	}
	return defaultAsyncTimeout
}
