package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"flexie.io/sag/internal/tool"
)

// An async tool that answers within its budget returns its result unchanged.
func TestRunAsyncToolReturnsResult(t *testing.T) {
	want := tool.Result{Content: json.RawMessage(`{"ok":true}`)}
	got, err := runAsyncTool(context.Background(), time.Second, func(context.Context) (tool.Result, error) {
		return want, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got.Content) != string(want.Content) {
		t.Fatalf("result not passed through: %s", got.Content)
	}
}

// An async tool that runs past its deadline is cancelled and reported as a
// deadline error, which the caller turns into an ordinary tool failure the model
// can retry.
func TestRunAsyncToolTimesOut(t *testing.T) {
	started := make(chan struct{})
	_, err := runAsyncTool(context.Background(), 20*time.Millisecond, func(ctx context.Context) (tool.Result, error) {
		close(started)
		<-ctx.Done() // a well-behaved tool observes the deadline
		return tool.Result{}, ctx.Err()
	})
	<-started
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline error, got %v", err)
	}
}

// A tool's declared timeout is honoured; a tool that declared none gets the
// default rather than an instant, zero-duration deadline.
func TestAsyncTimeout(t *testing.T) {
	if got := asyncTimeout(tool.Schema{AsyncTimeout: 5 * time.Second}); got != 5*time.Second {
		t.Fatalf("declared timeout ignored: %v", got)
	}
	if got := asyncTimeout(tool.Schema{}); got != defaultAsyncTimeout {
		t.Fatalf("missing timeout did not fall back to the default: %v", got)
	}
}
