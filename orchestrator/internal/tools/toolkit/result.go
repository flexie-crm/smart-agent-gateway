// Package toolkit holds what the built-in tools share: how a result is shaped,
// how a permission is checked, and the guards (SSRF, error sanitising) a tool
// that reaches the outside world needs. Each tool lives in its own package
// under internal/tools and leans on this one, so the tools stay small and say
// the same things the same way.
package toolkit

import (
	"encoding/json"
	"fmt"

	"flexie.io/sag/internal/tool"
)

// Success is a completed call. The payload is whatever the tool wants the model
// to see, shaped by the tool; toolkit only marshals it.
func Success(payload any) (tool.Result, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return tool.Result{}, fmt.Errorf("encode tool result: %w", err)
	}
	return tool.Result{Content: raw}, nil
}

// BadArguments is the model calling the tool wrong: a missing field, a bad
// enum, a malformed value. It is the one failure the loop learns from, so it is
// classified apart from the rest. The reason is model-safe text the model reads
// and corrects; never put an internal error in it.
func BadArguments(reason string) (tool.Result, error) {
	return errorResult(tool.ErrorBadArguments, reason, false)
}

// Denied is a permission refusal, handed to the model as a normal result so it
// explains the refusal rather than the turn dying.
func Denied() (tool.Result, error) {
	return errorResult(tool.ErrorDenied, "you do not have permission to do this", false)
}

// Blocked is a policy refusal (a forbidden target, an SSRF-guarded host). It is
// not retryable: the model should report it, not try again.
func Blocked(reason string) (tool.Result, error) {
	return errorResult(tool.ErrorBlocked, reason, false)
}

// Transient is a passing failure (a timeout, a connection reset). The model may
// retry, ideally with different parameters, so the result says so.
func Transient(reason string) (tool.Result, error) {
	return errorResult(tool.ErrorTransient, reason, true)
}

// Failed is a generic failure with no more specific classification.
func Failed(reason string) (tool.Result, error) {
	return errorResult(tool.ErrorFailed, reason, false)
}

// FieldSuccess and FieldError are what every tool's result calls the two things
// this package puts in it. They are exported because the chat filters them out
// of what a person reads (a red row already says it failed, and the error has
// its own line), and a filter that spelled them itself would keep working while
// meaning nothing the day this shape changes.
const (
	FieldSuccess = "success"
	FieldError   = "error"
)

func errorResult(kind tool.ErrorKind, reason string, retry bool) (tool.Result, error) {
	payload := map[string]any{FieldSuccess: false, FieldError: reason}
	if retry {
		payload["retry"] = true
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return tool.Result{}, fmt.Errorf("encode tool error: %w", err)
	}
	return tool.Result{Content: raw, Err: kind}, nil
}
