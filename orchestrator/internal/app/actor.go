package app

import (
	"context"
	"fmt"

	"flexie.io/sag/internal/model"
)

// Acting resolves the person behind a request into the record frozen beside
// whatever they are about to write.
//
// The name is read HERE, at the moment of the write, rather than joined at read
// time, which is the whole point of freezing it: a person can be renamed or
// deleted, and the answer to "who did this" must not change when they are. The
// id is kept too, so a live person is still linked to their work.
//
// A failure is returned rather than swallowed. The request was authenticated
// against this user a moment ago (requireAuth re-checks the session live), so a
// row that cannot be read is a real fault and not a missing name.
func (a *App) Acting(ctx context.Context, userID int64) (model.Actor, error) {
	user, err := a.Store.Users().GetByID(ctx, userID)
	if err != nil {
		return model.Actor{}, fmt.Errorf("read who is acting: %w", err)
	}
	return model.Actor{UserID: user.ID, Name: user.Name}, nil
}
