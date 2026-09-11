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
