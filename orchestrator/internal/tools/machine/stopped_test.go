package machine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/link"
	"flexie.io/sag/internal/tool"
)

// What is recorded when a call to somebody's own computer does not come back: a
// stopped turn says the turn was stopped, and never blames the computer or
// shows a context error.

type failingMachine struct{ err error }

func (f failingMachine) Call(context.Context, int64, int64, string, string, json.RawMessage, string) (link.Result, error) {
	return link.Result{}, f.err
}
func (f failingMachine) Runs(int64, int64, string) map[string]int { return nil }

func told(t *testing.T, failure error) string {
	t.Helper()
	schema := tool.Schema{Name: "terminal", FriendlyName: "Terminal"}
	res, err := Dispatch(failingMachine{err: failure}, schema, schema.Name)(context.Background(),
		tool.Call{WorkspaceID: 1, UserID: 7, DeviceID: "the-laptop"})
	if err != nil {
		t.Fatalf("the dispatcher errored instead of answering: %v", err)
	}
	return string(res.Content)
}

func TestAStoppedTurnDoesNotBlameTheComputer(t *testing.T) {
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded} {
		said := told(t, failure)
		if !strings.Contains(said, "this turn was stopped before your computer answered") {
			t.Fatalf("a stopped turn was recorded as %s", said)
		}
		if strings.Contains(strings.ToLower(said), "context") || strings.Contains(said, "could not do that") {
			t.Fatalf("a stopped turn blamed the computer or leaked the plumbing: %s", said)
		}
	}
}

// The failures that ARE about the computer keep their words.
func TestTheComputerIsStillNamedWhenItIsTheComputer(t *testing.T) {
	if said := told(t, link.ErrNoMachine); !strings.Contains(said, "not connected on this computer") {
		t.Fatalf("an application that is not running was recorded as %s", said)
	}
	if said := told(t, link.ErrTimeout); !strings.Contains(said, "did not answer in time") {
		t.Fatalf("a computer that did not answer was recorded as %s", said)
	}
}
