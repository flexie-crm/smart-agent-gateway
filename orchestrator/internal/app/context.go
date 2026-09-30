package app

import (
	"context"
	"time"

	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
)

// How full a conversation is, as the chat shows it above the message box.
//
// Measured inside every turn with the trim's own measure (provider.RequestChars)
// and kept on the conversation, so it is there the moment the conversation is
// opened; pushed over the socket after every step, so it moves while the
// assistant works. The percentage is of the room the model's window gives, which
// is the room the trim works in: past 100 the oldest messages are being dropped,
// and compacting is the better way to make room.

// ContextMeter is what the chat is told about how full a conversation is.
type ContextMeter struct {
	// Percent is nil when there is nothing to measure against: a conversation
	// never measured, or a model with no context window set.
	Percent *int `json:"percent,omitempty"`
	// WarnAt is the percentage from which the chat shows the meter.
	WarnAt int `json:"warn_at"`
	// Compacting says a summary is being written: the chat is frozen until it
	// is done, and a reload has to know that as much as the tab that asked.
	Compacting bool `json:"compacting"`
	// Failed says why the last compaction did not happen, when one did not.
	Failed string `json:"failed,omitempty"`
}

// contextPush is the socket message: the meter, and which conversation it is.
type contextPush struct {
	ChatUID string `json:"chat_uid"`
	ContextMeter
}

// contextWarnAt is the configured threshold, or the default for a config that
// did not set one.
func (a *App) contextWarnAt() int {
	if a.Config != nil && a.Config.ContextWarnPercent > 0 {
		return a.Config.ContextWarnPercent
	}
	return config.DefaultContextWarnPercent
}

// ContextMeterFor is how full a conversation is right now: its last measurement
// against its model's window as that is set now, and whether it is being
// compacted.
func (a *App) ContextMeterFor(ctx context.Context, workspaceID, sessionID int64) (ContextMeter, error) {
	meter := ContextMeter{WarnAt: a.contextWarnAt(), Compacting: a.Runs.Holding(sessionID)}
	use, err := a.Store.Agent().ContextUse(ctx, sessionID)
	if err != nil {
		return meter, err
	}
	if use.ModelID == 0 {
		return meter, nil
	}
	m, err := a.Store.AIModels().GetByID(ctx, workspaceID, use.ModelID)
	if err != nil {
		// A model that cannot be read has no window to measure against.
		return meter, nil
	}
	if percent, ok := provider.Fullness(m, use.Chars); ok {
		meter.Percent = &percent
	}
	return meter, nil
}

// recordContextUse keeps a turn's measurement and tells the person's tabs. It
// is the run manager's OnContext listener, called from inside the turn, so it
// is quick and never fails the turn: a meter that did not move is a small loss.
func (a *App) recordContextUse(workspaceID, userID, sessionID int64, m *model.AIModel, chars, baseChars int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	use := model.ContextUse{ModelID: m.ID, Chars: chars, BaseChars: baseChars}
	if err := a.Store.Agent().SetContextUse(ctx, sessionID, use); err != nil {
		a.Log.Warn().Err(err).Int64("session_id", sessionID).Msg("record how full the conversation is")
		return
	}
	a.announceContext(ctx, workspaceID, userID, sessionID, "")
}

// announceContext pushes how full a conversation is to every tab its owner has
// open, read back from what is stored so every push says the same thing a
// reload would.
func (a *App) announceContext(ctx context.Context, workspaceID, userID, sessionID int64, failed string) {
	if a.WS == nil {
		return
	}
	session, err := a.Store.Agent().GetSession(ctx, workspaceID, sessionID)
	if err != nil {
		return
	}
	meter, err := a.ContextMeterFor(ctx, workspaceID, sessionID)
	if err != nil {
		a.Log.Warn().Err(err).Int64("session_id", sessionID).Msg("read how full the conversation is")
		return
	}
	meter.Failed = failed
	a.WS.Notify(workspaceID, userID, map[string]any{
		"type":    "context",
		"payload": contextPush{ChatUID: session.UID, ContextMeter: meter},
	})
}
