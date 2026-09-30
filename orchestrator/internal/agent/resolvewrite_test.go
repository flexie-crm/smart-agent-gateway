package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// fakeStore is the agent store as far as these tests reach into it. A resolved
// call is refused once its context has ended, as the real database refuses it.
// Anything else it is asked for is a nil panic, so a write a test stopped
// covering fails loudly.
type fakeStore struct {
	store.Store
	agent *fakeAgent
}

func (s *fakeStore) Agent() store.AgentStore { return s.agent }

type fakeAgent struct {
	store.AgentStore
	mu      sync.Mutex
	next    int
	written []model.ToolCall
	refused int
}

func (a *fakeAgent) NextSeq(context.Context, int64) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next++
	return a.next, nil
}

func (a *fakeAgent) SaveStep(context.Context, *model.AgentStep) error        { return nil }
func (a *fakeAgent) RecordModelCall(context.Context, *model.ModelCall) error { return nil }

func (a *fakeAgent) ResolveToolCall(ctx context.Context, call *model.ToolCall) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		a.refused++
		return err
	}
	a.written = append(a.written, *call)
	return nil
}

func fakeRunner() (*Runner, *fakeAgent) {
	ag := &fakeAgent{}
	return &Runner{store: &fakeStore{agent: ag}, log: zerolog.Nop()}, ag
}

// A tool call's result is written even when its turn has already been stopped.
func TestACallsResultIsWrittenAfterItsTurnIsStopped(t *testing.T) {
	r, ag := fakeRunner()
	stopped, stop := context.WithCancel(context.Background())
	stop()

	result := json.RawMessage(`{"success":true,"output":"deployed"}`)
	r.resolveCall(stopped, Turn{SessionID: 19, WorkspaceID: 1},
		&model.ToolCall{ToolCallID: "c1", ToolName: "terminal", Status: model.ToolCallRunning},
		model.ToolCallCompleted, result, "", 27*time.Second, false)

	if ag.refused != 0 || len(ag.written) != 1 {
		t.Fatalf("the write was lost with the turn: written=%d refused=%d", len(ag.written), ag.refused)
	}
	got := ag.written[0]
	if got.Status != model.ToolCallCompleted || string(got.Result) != string(result) ||
		got.DurationMS != 27000 || got.CompletedAt == nil || got.SessionID != 19 {
		t.Fatalf("the row was written wrongly: %+v", got)
	}
}

func TestALiveTurnWritesItsCallsResult(t *testing.T) {
	r, ag := fakeRunner()
	r.resolveCall(context.Background(), Turn{SessionID: 19, WorkspaceID: 1},
		&model.ToolCall{ToolCallID: "c2", ToolName: "read_file", Status: model.ToolCallRunning},
		model.ToolCallCompleted, json.RawMessage(`{"success":true}`), "", time.Second, false)

	if ag.refused != 0 || len(ag.written) != 1 || ag.written[0].Status != model.ToolCallCompleted {
		t.Fatalf("an ordinary write changed: written=%+v refused=%d", ag.written, ag.refused)
	}
}
