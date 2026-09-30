package agent

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
)

// runValidator is the seam that keeps "approval must equal success": it runs a
// tool's pre-check BEFORE the loop decides to park, so a card is only ever shown
// for a call that will run. These are its three outcomes.

func TestRunValidatorPassesThroughWithoutAValidator(t *testing.T) {
	r := &Runner{log: zerolog.Nop()}
	schema := tool.Schema{Name: "brain_write", ApprovalTitle: "generic"}
	call := &model.ToolCall{ToolName: "brain_write"}

	got, refusal := r.runValidator(context.Background(), Turn{}, call, schema, nil)
	if refusal != nil {
		t.Fatalf("a tool with no validator was refused: %+v", refusal)
	}
	if got.ApprovalTitle != "generic" {
		t.Fatalf("the schema was altered without a validator: %+v", got)
	}
}

func TestRunValidatorRefusesBeforeTheCard(t *testing.T) {
	r := &Runner{log: zerolog.Nop()}
	schema := tool.Schema{Name: "brain_write"}
	call := &model.ToolCall{ToolName: "brain_write"}

	// A validator that refuses: this call must never reach a person.
	validators := map[string]tool.Validator{
		"brain_write": func(context.Context, tool.Call) tool.Validation {
			return tool.Validation{OK: false, Result: tool.Result{Err: tool.ErrorBadArguments}}
		},
	}
	_, refusal := r.runValidator(context.Background(), Turn{}, call, schema, validators)
	if refusal == nil {
		t.Fatal("a refusing validator did not stop the call before the card")
	}
	if refusal.Err != tool.ErrorBadArguments {
		t.Fatalf("the refusal lost its classification (so it would not teach): %v", refusal.Err)
	}
}

func TestRunValidatorAnnotatesTheCard(t *testing.T) {
	r := &Runner{log: zerolog.Nop()}
	schema := tool.Schema{Name: "brain_write", ApprovalTitle: "generic"}
	call := &model.ToolCall{ToolName: "brain_write"}

	validators := map[string]tool.Validator{
		"brain_write": func(context.Context, tool.Call) tool.Validation {
			return tool.Validation{OK: true, ApprovalTitle: "Save 'Refunds' to Support Playbook / Billing"}
		},
	}
	got, refusal := r.runValidator(context.Background(), Turn{}, call, schema, validators)
	if refusal != nil {
		t.Fatalf("a valid call was refused: %+v", refusal)
	}
	if got.ApprovalTitle != "Save 'Refunds' to Support Playbook / Billing" {
		t.Fatalf("the per-call card copy did not override the generic title: %q", got.ApprovalTitle)
	}
}

// Every tool call carries the computer the turn came from.
//
// This is one function because it was two, and the second one forgot this
// field. A delegated agent's terminal was refused with "this conversation is
// not in one" while the Gateway's own file tools worked in the same
// conversation seconds apart: same person, same machine, same turn, and the
// only difference was which of two places assembled the call.
func TestEveryToolCallCarriesTheComputerItCameFrom(t *testing.T) {
	turn := Turn{WorkspaceID: 7, UserID: 3, SessionID: 42, DeviceID: "device-abc"}
	call := &model.ToolCall{ToolName: "terminal", Args: []byte(`{"command":"pwd"}`)}

	got := callFor(turn, call)
	if got.DeviceID != "device-abc" {
		t.Errorf("the call lost the computer it came from: %+v", got)
	}
	// The rest of it too, so a field cannot go missing here the way the device
	// did there.
	if got.WorkspaceID != 7 || got.UserID != 3 || got.SessionID != 42 {
		t.Errorf("the call lost who was asking: %+v", got)
	}
	if got.Name != "terminal" || string(got.Args) != `{"command":"pwd"}` {
		t.Errorf("the call lost what was asked: %+v", got)
	}
}

// A validator is handed the same call the handler will be, device included. A
// validator that judged a different value than the one that runs is
// approval-equals-success broken by an omission.
func TestAValidatorSeesTheSameCallTheHandlerWill(t *testing.T) {
	r := &Runner{log: zerolog.Nop()}
	call := &model.ToolCall{ToolName: "write_file"}

	var seen tool.Call
	validators := map[string]tool.Validator{
		"write_file": func(_ context.Context, c tool.Call) tool.Validation {
			seen = c
			return tool.Validation{OK: true}
		},
	}
	r.runValidator(context.Background(), Turn{DeviceID: "device-abc", UserID: 3}, call,
		tool.Schema{Name: "write_file"}, validators)

	if seen.DeviceID != "device-abc" {
		t.Fatalf("the validator was shown a call with no computer on it: %+v", seen)
	}
}

// Every detached agent runs on a turn that knows which computer it may act on,
// whatever mode it was started in.
//
// This is one constructor because it was three, and two of them forgot the
// device. A fleet member was the one that showed it: its loadout WAS resolved
// with the computer, so the terminal was offered to it, and its turn was not,
// so calling the terminal came back "this conversation is not in one". Offered
// and then refused is worse than absent, because the agent spends its run
// trying.
func TestADetachedTurnKnowsWhichComputerItMayActOn(t *testing.T) {
	bg := BackgroundDelegation{
		DelegationID: 9, WorkspaceID: 7, UserID: 3, SessionID: 42,
		ModelID: 5, DeviceID: "device-abc",
	}
	got := detachedTurn(bg, false)
	if got.DeviceID != "device-abc" {
		t.Errorf("the detached turn has no computer on it: %+v", got)
	}
	// And the rest, so nothing else can go missing here the way the device did.
	if got.WorkspaceID != 7 || got.UserID != 3 || got.SessionID != 42 ||
		got.ModelID != 5 || got.DelegationID != 9 {
		t.Errorf("the detached turn lost part of its identity: %+v", got)
	}
}
