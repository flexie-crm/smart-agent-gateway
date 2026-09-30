package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/run"
	"flexie.io/sag/internal/store"
)

// ErrChatBusy is a conversation with something still working in it: a turn
// being answered, a card waiting on the person, an agent in the background, or
// a compaction already under way.
var ErrChatBusy = errors.New("app: the conversation is still working")

// ErrCompactNeedsModel is a conversation with no model to write its summary:
// nothing pinned one and the person did not choose one.
var ErrCompactNeedsModel = errors.New("app: no model to write the summary")

// compactFailed is what the person is told when a summary could not be written.
// The conversation is exactly as it was, which is the useful half of it.
const compactFailed = "The conversation could not be compacted, and is unchanged. Try again in a moment."

// StartCompaction summarizes a person's conversation in the background, so its
// next turn is built from the summary and what comes after it rather than from
// every step.
//
// It takes the conversation's lane first (run.Manager.Hold), under the lock a
// turn claims its lane with, so a compaction and a turn can never run at once:
// nothing can be said into the conversation until the summary is written, and
// a reload is told it is being compacted. Then it runs detached from the
// request that asked, so closing the tab or reloading does not stop it.
//
// Refused, before anything starts, while anything in the conversation is still
// working. A background agent finishing writes its result onto the call that
// started it, which is a step the summary would already cover, so the model
// would be told an agent had finished and never see what it found. A card
// waiting on the person is the same: the call it would run sits inside what was
// summarized.
func (a *App) StartCompaction(ctx context.Context, workspaceID, userID int64, uid string, preferredModelID int64) error {
	session, err := a.Store.Agent().GetSessionByUID(ctx, workspaceID, uid)
	if err != nil {
		return err
	}
	// Somebody else's conversation does not exist, the rule every chat route
	// keeps.
	if session.UserID != userID {
		return store.ErrNotFound
	}

	release, err := a.Runs.Hold(session.ID)
	switch {
	case errors.Is(err, run.ErrBusy), errors.Is(err, run.ErrHeld):
		return ErrChatBusy
	case err != nil:
		return err
	}
	started := false
	defer func() {
		if !started {
			release()
		}
	}()

	if session.Status == model.SessionWaitingApproval {
		return ErrChatBusy
	}
	running, err := a.Store.Agent().RunningDelegations(ctx, session.ID)
	if err != nil {
		return fmt.Errorf("read what is still running: %w", err)
	}
	if len(running) > 0 {
		return ErrChatBusy
	}

	// The model the next turn would run on, decided the way a turn decides it,
	// so the summary is written by the model that will read it.
	profile, err := a.ResolveProfile(ctx, ProfileRequest{
		WorkspaceID:      workspaceID,
		UserID:           userID,
		Channel:          model.ChannelChat,
		PreferredModelID: preferredModelID,
	})
	if err != nil {
		return fmt.Errorf("resolve the assistant: %w", err)
	}
	if profile.ModelID == 0 {
		return ErrCompactNeedsModel
	}
	// Answered now rather than from the background, so a click with nothing to
	// compact is told so at once instead of freezing the chat to find out.
	if err := a.Agent.CheckCompactable(ctx, session.ID); err != nil {
		return err
	}
	by, err := a.Acting(ctx, userID)
	if err != nil {
		return err
	}

	started = true
	req := agent.CompactRequest{
		WorkspaceID:     workspaceID,
		SessionID:       session.ID,
		ModelID:         profile.ModelID,
		By:              by,
		ReadAttachments: a.AttachmentText,
	}
	// Every tab the person has open freezes now, not only the one that asked.
	a.announceContext(ctx, workspaceID, userID, session.ID, "")
	a.compactions.run(func(ctx context.Context) {
		a.compactInBackground(ctx, workspaceID, userID, req, release)
	})
	return nil
}

// compactInBackground writes the summary, records how full the conversation is
// with it, gives the lane back and tells the person's tabs, in that order: the
// chat unfreezes on the last of those, and by then a message sent into it is
// accepted.
func (a *App) compactInBackground(ctx context.Context, workspaceID, userID int64, req agent.CompactRequest, release func()) {
	compacted, err := a.Agent.Compact(ctx, req)
	if err != nil {
		release()
		a.Log.Error().Err(err).Int64("session_id", req.SessionID).Msg("compact a conversation")
		failed := compactFailed
		if errors.Is(err, agent.ErrNothingToCompact) {
			failed = "Nothing new has been said since this conversation was last compacted."
		}
		a.announceAfter(workspaceID, userID, req.SessionID, failed)
		return
	}

	// How full it is now: what does not come from the conversation (the system
	// prompt and the tools, from the last measurement) plus the summary, which
	// is all of the conversation the next turn will read. The next turn
	// measures it again for real.
	record, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	use, err := a.Store.Agent().ContextUse(record, req.SessionID)
	if err == nil {
		err = a.Store.Agent().SetContextUse(record, req.SessionID, model.ContextUse{
			ModelID:   req.ModelID,
			Chars:     use.BaseChars + agent.SummaryChars(compacted.Summary),
			BaseChars: use.BaseChars,
		})
	}
	cancel()
	if err != nil {
		a.Log.Warn().Err(err).Int64("session_id", req.SessionID).Msg("record how full a compacted conversation is")
	}
	release()
	a.announceAfter(workspaceID, userID, req.SessionID, "")
}

// announceAfter tells the tabs how a compaction ended, on a context of its own:
// the one the work ran on may be the one that was cancelled.
func (a *App) announceAfter(workspaceID, userID, sessionID int64, failed string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.announceContext(ctx, workspaceID, userID, sessionID, failed)
}

// compactions owns the summaries being written in the background, so a
// shutdown can wait for them and, past its grace, end them, like every other
// goroutine the server starts.
type compactions struct {
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

func newCompactions() *compactions {
	ctx, cancel := context.WithCancel(context.Background())
	return &compactions{ctx: ctx, cancel: cancel}
}

func (c *compactions) run(work func(context.Context)) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		work(c.ctx)
	}()
}

// drain waits for the ones under way, and reports whether they finished before
// ctx did.
func (c *compactions) drain(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// stop ends whatever is still being written and waits for it to give its lane
// back. A summary cut short is not kept, so the conversation is as it was.
func (c *compactions) stop() {
	c.cancel()
	c.wg.Wait()
}
