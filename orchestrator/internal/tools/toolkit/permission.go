package toolkit

import (
	"context"
	"errors"
)

// Authorizer resolves a person's live permissions. Tools take it as a
// dependency so a tool call can never be more powerful than the person who
// triggered it: authorization is re-checked on every call, never assumed from
// the fact that the tool was loaded.
type Authorizer interface {
	Authorize(ctx context.Context, userID int64, permission string) (bool, error)
}

// ErrDenied is what Require returns when the person may not do this. A tool
// turns it into a Denied() result, which the model reads and explains, rather
// than a failed turn.
var ErrDenied = errors.New("permission denied")

// Require enforces the caller's live permission. It is the checkpoint every
// state-changing tool passes before it acts.
func Require(ctx context.Context, auth Authorizer, userID int64, permission string) error {
	allowed, err := auth.Authorize(ctx, userID, permission)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrDenied
	}
	return nil
}
