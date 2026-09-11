package app

import (
	"context"
	"errors"
	"time"

	"flexie.io/sag/internal/store"
)

// zoneSetting is where a person's own time zone is kept.
//
// A setting rather than a column, because it is what a settings bag is for and
// because nobody administers it: it is not a preference somebody sets, it is
// what their computer says about where they are, remembered so that everything
// which builds a prompt can read it. A conversation resumed from a scheduler at
// three in the morning has no request to ask, and the person is still in the
// same place they were.
const zoneSetting = "time_zone"

// PersonZone is where somebody is, by IANA name, or nothing.
//
// Nothing is an ordinary answer, not a failure: an API caller, an MCP client and
// a person who has not opened the chat since this shipped all have no zone, and
// what follows from that is a prompt that says UTC and says so.
func (a *App) PersonZone(ctx context.Context, userID int64) string {
	if userID == 0 {
		return ""
	}
	zone, err := a.Store.Settings().Get(ctx, userID, zoneSetting)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			a.Log.Warn().Err(err).Int64("user_id", userID).Msg("read the person's time zone")
		}
		return ""
	}
	return zone
}

// RememberZone records where somebody is, when their chat has just said.
//
// Written only when it CHANGED, which is almost never: the read happens on
// every turn anyway to build the prompt, so the common case costs nothing extra
// and somebody who flies to another country is right on their next message.
//
// The zone is checked against the machine's own database before it is kept,
// and this is the security half rather than the tidy half: it arrives from a
// client and it goes into a system prompt, so anything that is not the name of
// a real place is refused rather than repeated back to the model as fact.
func (a *App) RememberZone(ctx context.Context, userID int64, zone string) {
	if userID == 0 || zone == "" || len(zone) > 64 {
		return
	}
	if _, err := time.LoadLocation(zone); err != nil {
		a.Log.Debug().Str("zone", zone).Msg("a chat said a time zone this machine does not know")
		return
	}
	if a.PersonZone(ctx, userID) == zone {
		return
	}
	if err := a.Store.Settings().Set(ctx, userID, zoneSetting, zone); err != nil {
		// Not being able to remember it is not a reason to refuse the turn: the
		// worst of it is a prompt that says UTC.
		a.Log.Warn().Err(err).Int64("user_id", userID).Msg("remember the person's time zone")
	}
}
