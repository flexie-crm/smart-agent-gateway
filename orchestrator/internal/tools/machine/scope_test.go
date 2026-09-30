package machine

import (
	"context"
	"encoding/json"
	"testing"

	"flexie.io/sag/internal/link"
	"flexie.io/sag/internal/tool"
)

// heard records what reached the far side, so a test can assert what was sent
// rather than what we meant to send.
type heard struct{ args json.RawMessage }

func (h *heard) Call(_ context.Context, _, _ int64, _, _ string, args json.RawMessage, _ string) (link.Result, error) {
	h.args = args
	return link.Result{OK: true, Content: json.RawMessage(`{}`)}, nil
}

func (h *heard) Runs(int64, int64, string) map[string]int { return nil }

// scopeOf reads the namespace that was actually sent.
func scopeOf(t *testing.T, h *heard) int64 {
	t.Helper()
	var sent struct {
		Conversation int64 `json:"conversation"`
	}
	if err := json.Unmarshal(h.args, &sent); err != nil {
		t.Fatalf("what was sent could not be read: %v\n%s", err, h.args)
	}
	return sent.Conversation
}

// Two agents of one conversation reach two different sets of terminals.
//
// EVERY agent, in every mode: an agent is its own worker, with its own folder
// and its own exported variables, and has no business inheriting somebody
// else's. The identity is the one the product already mints per running agent
// (tool.OwnerOfAgent), which is what the SSH tool has always keyed on.
//
// Measured before this existed: two fleet members asked to run one command each
// landed on one shell, and the second was refused with "this conversation's
// terminal is busy with" the FIRST one's command, quoted. They are separated by
// what is sent, so that is what this asserts.
func TestTwoAgentsOfOneConversationGetDifferentTerminals(t *testing.T) {
	schema := terminalSchema()
	args := json.RawMessage(`{"command":"pwd"}`)

	first := &heard{}
	if _, err := Dispatch(first, schema, schema.Name)(context.Background(), tool.Call{
		SessionID: 42, DeviceID: "the-laptop", Owner: tool.OwnerOfAgent(), Args: args,
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	second := &heard{}
	if _, err := Dispatch(second, schema, schema.Name)(context.Background(), tool.Call{
		SessionID: 42, DeviceID: "the-laptop", Owner: tool.OwnerOfAgent(), Args: args,
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	a, b := scopeOf(t, first), scopeOf(t, second)
	if a == b {
		t.Fatalf("two agents of one conversation were sent the same terminal namespace (%d)", a)
	}
}

// The Gateway's own calls are unchanged: its terminals are its conversation's,
// which is what makes the shell theirs rather than everybody's.
func TestTheGatewayKeepsItsConversationsTerminals(t *testing.T) {
	schema := terminalSchema()
	h := &heard{}
	if _, err := Dispatch(h, schema, schema.Name)(context.Background(), tool.Call{
		SessionID: 42, DeviceID: "the-laptop", Owner: tool.OwnerOfSession(42), Args: json.RawMessage(`{"command":"pwd"}`),
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := scopeOf(t, h); got != 42 {
		t.Fatalf("the Gateway's terminal namespace changed: %d, want the conversation 42", got)
	}
}

// And an agent's namespace can never be a conversation's: a conversation id and
// an agent's number are both positive, so one must be moved out of the other's
// space.
func TestAnAgentsNamespaceCannotBeAConversations(t *testing.T) {
	schema := terminalSchema()
	h := &heard{}
	if _, err := Dispatch(h, schema, schema.Name)(context.Background(), tool.Call{
		SessionID: 7, DeviceID: "the-laptop", Owner: tool.OwnerOfAgent(), Args: json.RawMessage(`{"command":"pwd"}`),
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := scopeOf(t, h); got == 7 {
		t.Fatal("delegation 7 was sent conversation 7's terminals: the two spaces overlap")
	}
}
