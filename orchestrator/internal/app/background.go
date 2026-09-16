package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/events"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/run"
	"flexie.io/sag/internal/store"
)

// The app's side of background delegation (Mode C, KB/27). The runtime decides
// to run an agent in the background; this owns the goroutine that actually
// runs it, records its result, and schedules the Gateway's completion turn. It is
// the app-layer owner the way the server owns the memory worker and the socket
// hub: created here, drained on shutdown, so no goroutine outlives the process.

// backgroundTerminalTimeout bounds the store writes that record how a background
// delegation ended and schedule its completion turn. They run on a context
// detached from the goroutine's own, so a shutdown that cancelled the agent
// still records the outcome rather than leaving the chip spinning.
const backgroundTerminalTimeout = 10 * time.Second

// backgroundInterrupted is the person-safe reason a completion turn relays when a
// restart killed a background agent mid-task. It names no technology: the
// person is told plainly that the task did not finish.
const backgroundInterrupted = "the service was interrupted before it could finish"

// backgroundManager owns the goroutines that run background-mode delegations.
// Each runs on the manager's base context; the manager tracks them so a clean
// shutdown drains them, and so a running one can be cancelled by delegation id.
type backgroundManager struct {
	app *App
	log zerolog.Logger

	baseCtx    context.Context
	baseCancel context.CancelFunc

	wg sync.WaitGroup

	mu       sync.Mutex
	running  map[int64]context.CancelFunc
	progress map[int64]*delegationProgress
	// quiescing is set when a restart begins: no new agent is started after it,
	// because starting one now guarantees interrupting it.
	quiescing bool
}

// noComputer is what a run with nobody in front of it carries where a computer
// would be.
//
// A background agent starts from a record and may run an hour after the person
// closed their laptop, so it has no computer to reach and is offered no tool
// that needs one. Reaching whichever machine happens to be awake would be the
// guess the device identity exists to remove; carrying the asking computer on
// the delegation is a column and a decision, and belongs with the first tool
// that actually needs it.
func noComputer() Computer { return Computer{} }

func newBackgroundManager(a *App, log zerolog.Logger) *backgroundManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &backgroundManager{
		app:        a,
		log:        log,
		baseCtx:    ctx,
		baseCancel: cancel,
		running:    make(map[int64]context.CancelFunc),
		progress:   make(map[int64]*delegationProgress),
	}
}

// launch runs a fresh background delegation: the agent's first leg.
func (b *backgroundManager) launch(bg agent.BackgroundDelegation) {
	b.notify(bg.DelegationID, model.DelegationRunning)
	b.spawn(bg.DelegationID, bg.Sub.BackgroundTimeout, func(ctx context.Context) {
		out := chat.NewStream(b.newProgressSink(bg.DelegationID))
		result, runErr := b.app.Agent.RunBackgroundAgent(ctx, bg, out)
		b.finish(ctx, bg, result, runErr)
	})
}

// resume continues a background agent from a card the person just answered.
// It clears the session's waiting state, re-launches the agent as a
// goroutine, and on finish records the outcome and schedules the completion turn,
// exactly as the first leg does.
func (b *backgroundManager) resume(snapshot *model.ParkSnapshot, approved bool) {
	sc, cancel := context.WithTimeout(context.Background(), backgroundTerminalTimeout)
	defer cancel()

	del, err := b.delegationOfPark(sc, snapshot)
	if err != nil {
		b.log.Error().Err(err).Int64("session_id", snapshot.SessionID).Msg("resume: delegation not found")
		return
	}
	bg := agent.BackgroundDelegation{
		DelegationID: del.ID,
		WorkspaceID:  snapshot.WorkspaceID,
		UserID:       snapshot.UserID,
		SessionID:    snapshot.SessionID,
		ModelID:      snapshot.ModelID,
	}
	sub, err := b.app.ResolveAgent(sc, snapshot.WorkspaceID, snapshot.UserID, noComputer(), snapshot.AgentKey)
	if err != nil {
		// The agent was removed while the card waited. Fail the delegation and
		// let the completion turn say so, rather than leave the chip spinning.
		b.log.Warn().Err(err).Str("agent", snapshot.AgentKey).Msg("resume: agent gone")
		b.settle(sc, bg, model.DelegationFailed, nil, "the agent that was working on this is no longer available")
		b.scheduleCompletion(sc, bg)
		return
	}

	// The person answered, so the session is no longer waiting on a card. Clear it
	// before the completion turn is scheduled, or the scheduler would hold that
	// turn forever behind a card that is already gone.
	if err := b.app.Store.Agent().SetSessionStatus(sc, snapshot.SessionID, model.SessionRunning); err != nil {
		b.log.Error().Err(err).Int64("session_id", snapshot.SessionID).Msg("resume: clear waiting status")
	}

	// The card is answered and the agent is working again: clear the waiting
	// marker and flip the chip back to running, keeping the running token total.
	prog := b.progressFor(del.ID)
	prog.setState("Working…", false)
	b.setProgress(del.ID, prog.snapshot())
	b.notify(del.ID, model.DelegationRunning)
	// The approval no longer waits: the dashboard's count moved (KB/30).
	b.app.Bus.Publish(events.ApprovalResolved{WorkspaceID: snapshot.WorkspaceID})

	br := agent.BackgroundResume{
		Snapshot: snapshot, Approved: approved, Sub: sub,
		WorkspaceID: bg.WorkspaceID, UserID: bg.UserID, SessionID: bg.SessionID, ModelID: bg.ModelID,
	}
	b.spawn(del.ID, sub.BackgroundTimeout, func(ctx context.Context) {
		out := chat.NewStream(b.newProgressSink(del.ID))
		result, runErr := b.app.Agent.ResumeBackgroundAgent(ctx, br, out)
		b.finish(ctx, bg, result, runErr)
	})
}

// spawn runs work as an owned goroutine keyed by delegation id: its cancel is
// tracked so it can be stopped, and it is on a WaitGroup so shutdown waits for it.
// timeout bounds this working leg: past it the run's context hits its deadline,
// the AI stream unwinds, and finish() sees context.DeadlineExceeded and tells the
// Gateway it timed out. An operator cancel goes through the tracked cancel (a plain
// Canceled), which finish() keeps silent, so the two are distinguishable (KB/27).
func (b *backgroundManager) spawn(delegationID int64, timeout time.Duration, work func(context.Context)) {
	if timeout <= 0 {
		timeout = model.DefaultBackgroundTimeout
	}
	ctx, cancel := context.WithCancel(b.baseCtx)
	runCtx, timeoutCancel := context.WithTimeout(ctx, timeout)

	b.mu.Lock()
	if b.quiescing {
		// A restart has begun. Starting now would guarantee interrupting it, and
		// the caller has already been refused at the tool, so this is the floor
		// rather than the message.
		b.mu.Unlock()
		timeoutCancel()
		cancel()
		return
	}
	b.running[delegationID] = cancel
	b.mu.Unlock()

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() {
			b.mu.Lock()
			delete(b.running, delegationID)
			b.mu.Unlock()
			timeoutCancel()
			cancel()
		}()
		work(runCtx)
	}()
}

// finish records how an agent leg ended and, unless it was cancelled or is
// merely suspended for approval, schedules the Gateway's completion turn.
func (b *backgroundManager) finish(ctx context.Context, bg agent.BackgroundDelegation, result string, runErr error) {
	// The agent stopped to ask a person. It is suspended, not finished: the
	// park is durable and a resume re-enters it. Leave the record running, mark it
	// waiting on the durable record (so a reload also shows it), and flip the chip.
	if errors.Is(runErr, agent.ErrBackgroundParked) {
		b.markParked(ctx, bg)
		return
	}

	// Past the parked branch every outcome is terminal (timeout, cancel, done,
	// failed): the chip is about to go away, so its running-progress accumulator
	// has nothing left to show. One defer covers every terminal return.
	defer b.dropProgress(bg.DelegationID)

	// The terminal writes must survive the goroutine's own context dying (a
	// shutdown cancels the base context), so they run detached with a fresh
	// deadline.
	wc, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundTerminalTimeout)
	defer cancel()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// The agent ran past its background time limit (KB/27). Unlike an
		// operator cancel, this DOES get a completion turn: the Gateway must tell
		// the person it timed out and returned nothing, not go silent.
		if b.settle(wc, bg, model.DelegationFailed, nil,
			"the background task ran past its time limit and was stopped before it returned a result") {
			b.notify(bg.DelegationID, model.DelegationFailed)
			b.scheduleCompletion(wc, bg)
		}
		return
	}

	if ctx.Err() != nil {
		// Cancelled by an operator (or shutdown): mark it and run no completion
		// turn. The one-way lifecycle guard makes a concurrent cancel a no-op.
		if err := b.app.Store.Agent().CompleteDelegation(wc, bg.DelegationID, model.DelegationCancelled, nil, ""); err != nil {
			b.log.Debug().Err(err).Int64("delegation_id", bg.DelegationID).Msg("cancel already recorded")
		}
		b.notify(bg.DelegationID, model.DelegationCancelled)
		return
	}

	status, payload, errText := model.DelegationDone, json.RawMessage(nil), ""
	if runErr != nil {
		b.log.Error().Err(runErr).Int64("delegation_id", bg.DelegationID).Msg("background agent failed")
		status, errText = model.DelegationFailed, "the background task could not be completed"
	} else {
		payload, _ = json.Marshal(map[string]string{"result": result})
	}

	if !b.settle(wc, bg, status, payload, errText) {
		// Lost the race to a cancel, or already terminal: nothing to narrate.
		return
	}
	b.notify(bg.DelegationID, status)
	// Both a done and a failed delegation get a completion turn: on success the
	// Gateway narrates the result, on failure it tells the person it could not be
	// finished rather than leave the conversation silent.
	b.scheduleCompletion(wc, bg)
}

// markParked is everything that happens on THIS process when a detached agent
// stops to ask: the chip flips to waiting, the dashboard's count moves, and the
// card is pushed whole to the person if it is the live one.
//
// None of it is the park itself, which is a durable row the agent already
// wrote. That is why it can be called for a fleet member that parked on a
// WORKER: the row is already there, and what is missing is a screen, which only
// a process holding sockets can supply.
func (b *backgroundManager) markParked(ctx context.Context, bg agent.BackgroundDelegation) {
	prog := b.progressFor(bg.DelegationID)
	prog.setState("Waiting for your approval", true)
	b.setProgress(bg.DelegationID, prog.snapshot())
	b.notify(bg.DelegationID, model.SessionWaitingApproval)
	// An approval is now waiting: the live dashboard's count moved (KB/30). A
	// detached park is off the run-manager lane, so it does not fire the turn
	// events; this is how the dashboard learns of it.
	b.app.Bus.Publish(events.ApprovalPending{WorkspaceID: bg.WorkspaceID})
	// Push the card itself, whole, when this park is the live one; a park queued
	// behind another waits its turn (KB/27). Detached from the caller's context,
	// like the other terminal writes here.
	sc, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundTerminalTimeout)
	defer cancel()
	b.surfaceIfLive(sc, bg)
}

// markResumed is markParked undone: the person answered, so the chip goes back
// to working and the dashboard's waiting count comes down. It keeps the running
// token total, because the agent is the same agent carrying on.
func (b *backgroundManager) markResumed(ctx context.Context, delegationID, workspaceID int64) {
	prog := b.progressFor(delegationID)
	prog.setState("Working…", false)
	b.setProgress(delegationID, prog.snapshot())
	b.notify(delegationID, model.DelegationRunning)
	b.app.Bus.Publish(events.ApprovalResolved{WorkspaceID: workspaceID})
	_ = ctx
}

// settle writes a delegation's terminal status, reporting whether this call was
// the one that resolved it (false when a concurrent cancel got there first).
func (b *backgroundManager) settle(ctx context.Context, bg agent.BackgroundDelegation, status string, result json.RawMessage, errText string) bool {
	if err := b.app.Store.Agent().CompleteDelegation(ctx, bg.DelegationID, status, result, errText); err != nil {
		b.log.Debug().Err(err).Int64("delegation_id", bg.DelegationID).Msg("delegation already resolved")
		return false
	}
	return true
}

// delegationOfPark is which agent raised a card. It is the ONE answer to that
// question, and everything that needs it asks here.
//
// The card says so directly when it can. It did not always: a card used to be
// matched by the Gateway `delegate` call it was raised under, which is exact for
// a background agent because each has a call of its own, and WRONG for a fleet,
// whose members share one. Every path that guessed this way picked an arbitrary
// member of the batch: the wrong chip moved, the wrong agent was resumed, and
// "approve all" re-launched one member thirteen times.
//
// The fallback is for cards written before a card carried its delegation, and it
// is only ever right for the delegation that has a parent call to itself.
func (b *backgroundManager) delegationOfPark(ctx context.Context, park *model.ParkSnapshot) (*model.AgentDelegation, error) {
	if park.DelegationID != nil {
		return b.app.Store.Agent().GetDelegation(ctx, *park.DelegationID)
	}
	running, err := b.app.Store.Agent().RunningDelegations(ctx, park.SessionID)
	if err != nil {
		return nil, err
	}
	for _, d := range running {
		if d.ParentToolCallID == park.ParentToolCallID {
			return d, nil
		}
	}
	return nil, store.ErrNotFound
}

// scheduleCompletion builds the Gateway's completion turn and queues it on the
// session's one turn lane. It resolves the profile live (like any turn), on the
// model the Gateway ran, and carries the seams a completion may itself need:
// delegate again in the background, or park for its own approval.
func (b *backgroundManager) scheduleCompletion(ctx context.Context, bg agent.BackgroundDelegation) {
	session, err := b.app.Store.Agent().GetSession(ctx, bg.WorkspaceID, bg.SessionID)
	if err != nil {
		b.log.Error().Err(err).Int64("session_id", bg.SessionID).Msg("load session for completion turn")
		return
	}
	profile, loadout, err := b.app.Resolve(ctx, ProfileRequest{
		WorkspaceID:      bg.WorkspaceID,
		UserID:           bg.UserID,
		Channel:          model.ChannelChat,
		PreferredModelID: bg.ModelID,
		SessionID:        bg.SessionID,
	})
	if err != nil {
		b.log.Error().Err(err).Int64("session_id", bg.SessionID).Msg("resolve profile for completion turn")
		return
	}
	if profile.ModelID == 0 {
		b.log.Error().Int64("session_id", bg.SessionID).Msg("completion turn has no model to run on")
		return
	}

	b.app.Runs.Schedule(agent.Turn{
		WorkspaceID:           bg.WorkspaceID,
		UserID:                bg.UserID,
		SessionID:             bg.SessionID,
		ModelID:               profile.ModelID,
		SystemPrompt:          profile.SystemPrompt,
		Tools:                 loadout,
		Reasoning:             profile.Reasoning,
		Settings:              profile.Settings,
		ApprovalTTL:           profile.ApprovalTTL,
		MaxIterations:         profile.MaxIterations,
		MaxFleetAgents:        profile.MaxFleetAgents,
		AutoApprove:           session.ApprovalMode == model.ApprovalAuto,
		Agent:                 b.app.AgentResolver(bg.WorkspaceID, bg.UserID, noComputer()),
		StartBackground:       b.app.StartBackground,
		StartFleet:            b.app.StartFleet,
		CompletedDelegationID: bg.DelegationID,
	})
}

// cancel stops a background delegation. A running one has its goroutine's context
// cancelled, so it unwinds and finish() records it cancelled with no completion
// turn. A delegation not currently on a goroutine (suspended for approval, or
// already between states) is marked cancelled directly, so its chip goes
// terminal. It reports whether there was something to cancel.
func (b *backgroundManager) cancel(delegationID int64) bool {
	b.mu.Lock()
	stop, running := b.running[delegationID]
	b.mu.Unlock()
	if running {
		stop()
		return true
	}

	ctx, c := context.WithTimeout(context.Background(), backgroundTerminalTimeout)
	defer c()
	// Read it before settling it, because the parks it left open are found by
	// the `delegate` call it was raised under and that is on the row.
	del, derr := b.app.Store.Agent().GetDelegation(ctx, delegationID)
	if err := b.app.Store.Agent().CompleteDelegation(ctx, delegationID, model.DelegationCancelled, nil, ""); err != nil {
		b.log.Debug().Err(err).Int64("delegation_id", delegationID).Msg("cancel: nothing to stop")
		return false
	}
	// A cancelled agent's cards go with it. Settling only the row left the card
	// on screen for something that no longer exists: answering it would resume an
	// agent the person had just stopped, and leaving it unanswered kept the
	// conversation waiting on a decision that could not matter. Same rule as boot
	// recovery, and scoped the same way, to what this agent raised and nothing
	// else on the conversation.
	if derr == nil {
		b.releaseOrphanedParks(ctx, del.WorkspaceID, del.SessionID, del.ParentToolCallID)
	}
	b.notify(delegationID, model.DelegationCancelled)
	return true
}

// notify pushes a delegation's state to the person's tabs, so a chip updates
// without a reload (KB/27). It is fire-and-forget on its own short context: a
// missed push is cosmetic, the durable record is the truth, and the chip is
// rebuilt from history on the next load.
func (b *backgroundManager) notify(delegationID int64, status string) {
	if b.app.WS == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTerminalTimeout)
	defer cancel()

	del, err := b.app.Store.Agent().GetDelegation(ctx, delegationID)
	if err != nil {
		return
	}
	if del.FleetID != nil {
		// A member of a batch has no chip of its own. The batch has ONE, and it
		// counts members, so a member changing state moves that count rather
		// than putting a thirteenth card in the column beside it.
		//
		// From MEMORY, because this is called once per member and the numbers
		// are already known. Reading them back per member is how answering
		// thirteen cards at once became a hundred and forty database round
		// trips before the last agent was even told to carry on.
		if progress, ok := b.app.fleetTracker.snapshot(*del.FleetID); ok {
			b.app.pushChip(progress.chip(model.DelegationRunning), progress.WorkspaceID, progress.UserID)
			return
		}
		// Not dispatched by this process (a restart). Slow and always right.
		if fleet, ferr := b.app.Store.Agent().Fleet(ctx, *del.FleetID); ferr == nil {
			b.app.notifyFleet(ctx, fleet)
		}
		return
	}
	session, err := b.app.Store.Agent().GetSession(ctx, del.WorkspaceID, del.SessionID)
	if err != nil {
		return
	}
	name := ""
	if ag, err := b.app.Store.Agents().GetByKey(ctx, del.WorkspaceID, del.AgentKey); err == nil {
		name = ag.Name
	}
	// The caller's status wins over the record's, because one of the states a
	// chip shows is not a status at all: an agent parked for approval is still
	// "running" on the row, and this is the call that says so.
	chip := AgentChip(del, name)
	chip.Status = status
	chip.ChatUID = session.UID
	b.app.WS.Notify(del.WorkspaceID, session.UserID, map[string]any{
		"type":    "delegation",
		"payload": chip,
	})
}

// surfaceCardFor builds a background agent's approval card and pushes it
// WHOLE over the socket, so the client renders it directly with no API round-trip
// (KB/27). The card's token is minted here (the original was never stored, only
// hashed) and the park is rebound to it, exactly once per surfacing, so nothing
// races a second read to rotate the token out from under the person. Called only
// when a card becomes live: the first park of a batch, or a released queued one.
func (b *backgroundManager) surfaceCardFor(ctx context.Context, park *model.ParkSnapshot) {
	if b.app.WS == nil {
		return
	}
	// With no live socket in this workspace there is nobody to push the card to, so
	// do not mint and rebind a fresh token for a push that will be dropped. A park
	// delivered only on the next history reload keeps its token until that read
	// binds one, so a live surface and a history read can never race to rotate the
	// token out from under each other.
	if b.app.WS.Connected(park.WorkspaceID).Connections == 0 {
		// Nobody is here to be asked. The card is a durable row and the next
		// time the conversation is opened it is read from there, so nothing is
		// lost; what is lost is the immediacy, and an agent waiting on a person
		// who is not there is worth a line in the log.
		b.log.Info().Int64("park", park.ID).Int64("session", park.SessionID).
			Msg("approval waiting, nobody connected to show it to")
		return
	}
	// Every way out of here from this point on is a card the person will not
	// see until they reload, so every one of them says so. They were silent, and
	// a card that simply never appeared was indistinguishable from an agent that
	// never asked for one.
	session, err := b.app.Store.Agent().GetSession(ctx, park.WorkspaceID, park.SessionID)
	if err != nil {
		b.log.Error().Err(err).Int64("park", park.ID).Msg("card not shown: cannot read the conversation")
		return
	}
	// A detached park is always an agent's; its tool is resolved live, so a tool
	// revoked while it waited yields no card, matching the resume's re-check.
	sub, err := b.app.ResolveAgent(ctx, park.WorkspaceID, session.UserID, noComputer(), park.AgentKey)
	if err != nil {
		b.log.Error().Err(err).Int64("park", park.ID).Str("agent", park.AgentKey).
			Msg("card not shown: the agent could not be resolved")
		return
	}
	schema, ok := sub.Tools.Schema(park.ToolName)
	if !ok {
		b.log.Warn().Int64("park", park.ID).Str("tool", park.ToolName).
			Msg("card not shown: the agent no longer has that ability")
		return
	}
	token, tokenHash, err := agent.NewToken()
	if err != nil {
		b.log.Error().Err(err).Int64("park", park.ID).Msg("card not shown: no token")
		return
	}
	if err := b.app.Store.Agent().RotateParkToken(ctx, park.ID, tokenHash); err != nil {
		b.log.Error().Err(err).Int64("park", park.ID).Msg("card not shown: token could not be bound")
		return
	}
	b.app.WS.Notify(park.WorkspaceID, session.UserID, map[string]any{
		"type": "card",
		"payload": map[string]any{
			"chat_uid": session.UID,
			"id":       "confirm_" + park.ToolCallID,
			"confirmation": map[string]any{
				"token":       token,
				"title":       schema.ConfirmTitle(),
				"description": schema.ConfirmDescription(),
				"severity":    string(schema.Risk),
				"details":     nullableJSON(park.ToolArgs),
				"status":      "pending",
			},
		},
	})
}

// surfaceIfLive pushes this delegation's card only if its park is the live one
// (the first to park), so the queued ones stay off the person's screen until
// their turn. It is the card half of a background park; the chip half is notify.
func (b *backgroundManager) surfaceIfLive(ctx context.Context, bg agent.BackgroundDelegation) {
	pending, err := b.app.Store.Agent().PendingPark(ctx, bg.SessionID)
	if err != nil {
		return // nothing live (this park is queued behind another, or none)
	}
	del, err := b.app.Store.Agent().GetDelegation(ctx, bg.DelegationID)
	if err != nil {
		return
	}
	// The live card is this agent's, or it is somebody else's and this one is
	// queued behind it. Compared by DELEGATION: a batch's members share a parent
	// call, so comparing that made every member believe the live card was its own.
	if pending.DelegationID != nil {
		if *pending.DelegationID == del.ID {
			b.surfaceCardFor(ctx, pending)
		}
		return
	}
	if pending.ParentToolCallID == del.ParentToolCallID {
		b.surfaceCardFor(ctx, pending)
	}
}

// nullableJSON returns raw JSON as-is, or nil so it serialises as null rather
// than an empty string a client cannot parse.
func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

// progressSink turns a background agent's tool-lifecycle frames into chip
// progress: what it is doing now and a step count, written to the delegation's
// durable record and pushed to the chip. It drops the agent's prose and
// reasoning (never shown, KB/27), so nothing leaks; the Gateway narrates at
// completion, and the Gateway can read this progress through its own tool.
// delegationProgress is everything a background delegation's chip shows,
// accumulated across the WHOLE delegation. The backgroundManager owns one per
// running delegation and keeps it across a park and resume (which each recreate
// the streaming sink), so an agent that stops for approval and starts again
// keeps its running token total rather than resetting to zero.
//
// Every writer publishes the WHOLE record, because the stored progress is
// replaced, not merged (UpdateDelegationProgress): a token update from the
// stream and a "waiting for approval" flag from the park path would otherwise
// clobber each other. One owner, one full write, no lost fields.
type delegationProgress struct {
	mu       sync.Mutex
	activity string
	step     int
	inTok    int64
	outTok   int64
	cost     float64
	waiting  bool
}

// fold applies one of an agent's live stream frames, reporting whether
// anything worth publishing changed. It keeps a tool starting (the visible
// activity and a step) and a usage frame (this call's exact token spend and
// cost); prose, reasoning and results are dropped and return false so nothing
// the agent says leaks to the chip (KB/27) and a no-op frame does not
// re-publish. Pure over its own state, so the accumulation is unit-tested.
func (p *delegationProgress) fold(f chat.Frame) bool {
	switch f.Type {
	case chat.FrameToolPreparing:
		tm, ok := f.Message.(chat.ToolMessage)
		if !ok {
			return false
		}
		activity := tm.Narration
		if activity == "" {
			activity = tm.FriendlyName
		}
		p.mu.Lock()
		p.step++
		p.activity = activity
		p.mu.Unlock()
		return true
	case chat.FrameUsage:
		um, ok := f.Message.(chat.UsageMessage)
		if !ok {
			return false
		}
		p.mu.Lock()
		p.inTok += um.InputTokens
		p.outTok += um.OutputTokens
		p.cost += um.Cost
		p.mu.Unlock()
		return true
	default:
		return false
	}
}

// setState sets what the delegation is doing and whether it is waiting on a
// person, leaving the accumulated token spend untouched. The park and resume
// paths use it so flipping to "waiting for approval" and back never drops the
// running total.
func (p *delegationProgress) setState(activity string, waiting bool) {
	p.mu.Lock()
	p.activity = activity
	p.waiting = waiting
	p.mu.Unlock()
}

// snapshot is the whole progress record the chip and the Gateway's status tool
// read: what the agent is doing now, whether it waits on approval, how many
// actions in, and its running token spend split into input and output with a
// total and a dollar cost. Zero values are left out so the chip shows a field
// only once there is something to say.
func (p *delegationProgress) snapshot() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := map[string]any{"step": p.step}
	if p.activity != "" {
		m["activity"] = p.activity
	}
	if p.waiting {
		m["waiting"] = true
	}
	if total := p.inTok + p.outTok; total > 0 {
		m["tokens"] = total
		m["tokens_in"] = p.inTok
		m["tokens_out"] = p.outTok
	}
	if p.cost > 0 {
		m["cost"] = p.cost
	}
	return m
}

// progressSink folds a background agent's live stream into its delegation's
// progress and pushes the chip on each change. It holds the shared per-delegation
// accumulator (so a resumed agent continues the same totals) and does the IO
// the accumulator must not: persist the record and notify the chip.
type progressSink struct {
	bg           *backgroundManager
	delegationID int64
	prog         *delegationProgress
}

func (b *backgroundManager) newProgressSink(delegationID int64) *progressSink {
	return &progressSink{bg: b, delegationID: delegationID, prog: b.progressFor(delegationID)}
}

func (s *progressSink) Write(f chat.Frame) error {
	if !s.prog.fold(f) {
		return nil
	}
	s.bg.setProgress(s.delegationID, s.prog.snapshot())
	s.bg.notify(s.delegationID, model.DelegationRunning)
	return nil
}

// progressFor returns a delegation's progress accumulator, creating it on first
// use. The same object lives across a park and resume, so the running token
// total survives an agent stopping for approval and starting again. (A
// process restart loses it, but boot recovery settles a mid-flight delegation as
// failed rather than resuming it, KB/27, so there is nothing to continue.)
func (b *backgroundManager) progressFor(delegationID int64) *delegationProgress {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.progress[delegationID]
	if p == nil {
		p = &delegationProgress{}
		b.progress[delegationID] = p
	}
	return p
}

// dropProgress forgets a finished delegation's accumulator: a terminal
// delegation's chip is gone, so its running total has nothing left to show.
func (b *backgroundManager) dropProgress(delegationID int64) {
	b.mu.Lock()
	delete(b.progress, delegationID)
	b.mu.Unlock()
}

// setProgress writes a delegation's small progress JSON (what it is doing, its
// step count, whether it is waiting on approval), which the chip and the Gateway's
// status tool read. Only writes while the delegation is still running.
func (b *backgroundManager) setProgress(delegationID int64, progress map[string]any) {
	raw, err := json.Marshal(progress)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTerminalTimeout)
	defer cancel()
	if err := b.app.Store.Agent().UpdateDelegationProgress(ctx, delegationID, raw); err != nil {
		b.log.Debug().Err(err).Int64("delegation_id", delegationID).Msg("update progress")
	}
}

// recoverInterrupted closes out the background delegations a dead process left
// running and, for each, fires the Gateway's completion turn so the person is told
// the task did not finish (KB/27). A background agent is an in-process
// goroutine, so a restart kills it while its row still says running; without this
// the chip would spin forever on a reload and the result would never be narrated.
// Called once at boot, before anything can legitimately be running, so every
// running row is a ghost of the last shutdown.
func (b *backgroundManager) recoverInterrupted(ctx context.Context) (int, error) {
	running, err := b.app.Store.Agent().AllRunningDelegations(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range running {
		if d.FleetID != nil {
			// NOT OURS. A fleet member runs on a worker, in another process
			// that this server's restart did not touch, so "nothing can
			// legitimately be running yet" is false for it. Settling it here
			// would kill live work on a machine we did not restart, and fire a
			// completion turn for a batch still being worked on.
			//
			// What covers a member whose worker really did die is the job's own
			// lease (reclaimed and run again) and the fleet's deadline, both of
			// which belong to the process that owns the work.
			continue
		}
		session, err := b.app.Store.Agent().GetSession(ctx, d.WorkspaceID, d.SessionID)
		if err != nil {
			b.log.Error().Err(err).Int64("delegation_id", d.ID).Msg("recover: load session")
			continue
		}
		bg := agent.BackgroundDelegation{
			DelegationID: d.ID,
			WorkspaceID:  d.WorkspaceID,
			UserID:       session.UserID,
			SessionID:    d.SessionID,
			// The delegation stores no model; the completion turn resolves the
			// profile's default, which is the Gateway's model in the ordinary case.
		}
		// START IT AGAIN if it can be. A graceful shutdown lets an agent finish;
		// this path is what is left when the process was KILLED, and settling the
		// row is honest but useless, because the person has to ask again for work
		// they already asked for. Everything the agent needs is on the row (the
		// agent it was, the task it was given, the conversation it belongs to),
		// so it can simply be started, exactly as a parked card is replayed from
		// its snapshot. The restart is atomic and capped: a task that kills the
		// process must not be restarted by every boot for ever.
		if d.Resumable() {
			restarted, err := b.app.Store.Agent().RestartDelegation(ctx, d.ID)
			if err != nil {
				b.log.Error().Err(err).Int64("delegation_id", d.ID).Msg("recover: restart delegation")
			} else if restarted {
				if b.relaunch(ctx, d, session.UserID) {
					b.log.Info().Int64("delegation_id", d.ID).Int("attempt", d.Attempts+1).
						Msg("background agent restarted after an interrupted shutdown")
					n++
					continue
				}
			}
		}
		if !b.settle(ctx, bg, model.DelegationFailed, nil, backgroundInterrupted) {
			continue
		}
		// The agent is gone, so anything it was waiting on has to go with it.
		// A park left behind here is a card offering to resume a goroutine that
		// no longer exists, and the session status left behind is worse: it
		// blocks every completion turn the conversation will ever queue, so a
		// chat that had three agents parked at the moment of a restart simply
		// never spoke again. Settling the record is not enough; what the record
		// was BLOCKING has to be released too.
		b.releaseOrphanedParks(ctx, d.WorkspaceID, d.SessionID, d.ParentToolCallID)
		b.notify(d.ID, model.DelegationFailed)
		b.scheduleCompletion(ctx, bg)
		n++
	}
	return n, nil
}

// relaunch starts an interrupted background agent again from its record, and
// reports whether it went. It resolves the agent LIVE, the way every other path
// does, so one an administrator removed or revoked while we were down is not
// quietly brought back: that is a refusal, and the caller settles it instead.
func (b *backgroundManager) relaunch(ctx context.Context, d *model.AgentDelegation, userID int64) bool {
	sub, err := b.app.ResolveAgent(ctx, d.WorkspaceID, userID, noComputer(), d.AgentKey)
	if err != nil {
		b.log.Warn().Err(err).Str("agent", d.AgentKey).Msg("recover: agent no longer available")
		return false
	}
	// Its cards go with the leg that died: the goroutine that would have been
	// resumed by an answer is gone, and the new leg raises its own if it needs to.
	b.releaseOrphanedParks(ctx, d.WorkspaceID, d.SessionID, d.ParentToolCallID)

	prog := b.progressFor(d.ID)
	prog.setState("Starting again", false)
	b.setProgress(d.ID, prog.snapshot())
	b.launch(agent.BackgroundDelegation{
		DelegationID: d.ID,
		Sub:          sub,
		Task:         d.Task,
		ParentCallID: d.ParentToolCallID,
		WorkspaceID:  d.WorkspaceID,
		UserID:       userID,
		SessionID:    d.SessionID,
	})
	return true
}

// releaseOrphanedParks clears the approvals THIS dead agent was waiting on (a
// restart, KB/27), and only those. They are rejected rather than approved:
// nobody agreed to any of them, and an unanswered card is not consent. The
// session's waiting status is then cleared IF nothing is left waiting, because
// waiting for an answer that can no longer be given is how a conversation stops
// for good (`run.Manager` will not deliver anything while a card is pending).
//
// SCOPED TO THE DEAD AGENT, by the `delegate` call it was raised under. A
// session's open parks are not all this agent's: the Gateway parks on the same
// conversation whenever IT calls an approval-gated tool, and that card is fully
// durable across a restart (nothing in memory backs it, the resume re-resolves
// the profile and runs the turn again). Closing every park on the session
// destroyed a card the person could still have answered, and left the tool call
// behind it reading "approval required" for good, because rejecting a park
// writes no result: only a resume does that. So a park is this agent's or it is
// left exactly where it is.
func (b *backgroundManager) releaseOrphanedParks(ctx context.Context, workspaceID, sessionID int64, parentCallID string) {
	mine := func(p *model.ParkSnapshot) bool {
		return p.HandoffMode == model.HandoffBackground && p.ParentToolCallID == parentCallID
	}
	// EVERY park this agent left open, live card and queue alike: they are the
	// same thing to an agent that is gone. Anybody else's is left exactly where
	// it is, still on screen and still answerable.
	open, err := b.app.Store.Agent().OpenParks(ctx, sessionID)
	if err != nil {
		b.log.Error().Err(err).Int64("session_id", sessionID).Msg("recover: read open parks")
		return
	}
	for _, park := range open {
		if !mine(park) {
			continue
		}
		if err := b.app.Store.Agent().ResolveParkByID(ctx, park.ID, model.ParkRejected); err != nil {
			b.log.Error().Err(err).Int64("park_id", park.ID).Msg("recover: release orphaned park")
		}
	}

	// Only now, and only if the conversation is genuinely waiting on nothing. A
	// card still on screen is a card somebody can still answer, and clearing the
	// status under it would let a completion turn interrupt the decision.
	if _, err := b.app.Store.Agent().PendingPark(ctx, sessionID); err == nil {
		return
	}
	session, err := b.app.Store.Agent().GetSession(ctx, workspaceID, sessionID)
	if err != nil || session.Status != model.SessionWaitingApproval {
		return
	}
	if err := b.app.Store.Agent().SetSessionStatus(ctx, sessionID, model.SessionCompleted); err != nil {
		b.log.Error().Err(err).Int64("session_id", sessionID).Msg("recover: clear waiting status")
	}
}

// releaseNextCard promotes a conversation's next queued approval card to live and
// surfaces it, so answering one background card brings up the next (KB/27). The
// resume that calls this cleared the session's waiting status for the agent
// it let through, so this re-marks it waiting when a card follows, and pushes the
// delegation's waiting state so the client fetches the newly live card. A no-op
// when the queue is empty.
func (b *backgroundManager) releaseNextCard(sessionID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTerminalTimeout)
	defer cancel()

	park, err := b.app.Store.Agent().ReleaseNextQueuedPark(ctx, sessionID)
	if err != nil {
		return // the queue is empty (ErrNotFound), or a read error: nothing to surface
	}
	if err := b.app.Store.Agent().SetSessionStatus(ctx, sessionID, model.SessionWaitingApproval); err != nil {
		b.log.Error().Err(err).Int64("session_id", sessionID).Msg("release: mark session waiting")
	}
	del, err := b.delegationOfPark(ctx, park)
	if err != nil {
		return
	}
	prog := b.progressFor(del.ID)
	prog.setState("Waiting for your approval", true)
	b.setProgress(del.ID, prog.snapshot())
	b.notify(del.ID, model.SessionWaitingApproval)
	// Push the newly live card whole, so the next one appears with no API round-trip.
	b.surfaceCardFor(ctx, park)
}

// drainApprovalQueue resumes every queued background agent as approved,
// without ever showing its card. It is what "approve all" does after switching
// the conversation to auto: the cards waiting in line are all let through, and
// anything that parks later auto-approves live (KB/27).
func (b *backgroundManager) drainApprovalQueue(sessionID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTerminalTimeout)
	defer cancel()

	// ONE write for the whole batch: the person said yes to all of them, so all
	// of them are answered together rather than one round trip at a time.
	queued, err := b.app.Store.Agent().ResolveQueuedParks(ctx, sessionID, model.ParkApproved)
	if err != nil {
		b.log.Error().Err(err).Int64("session_id", sessionID).Msg("drain: resolve queued parks")
		return
	}

	// Then signal them ALL, at once. They are independent (a different card, a
	// different agent, a different row), so there is nothing to be in a line
	// for, and one after another meant the last agent of a batch of thirteen
	// was told to carry on long after the first.
	var letting sync.WaitGroup
	for _, park := range queued {
		letting.Add(1)
		go func(park *model.ParkSnapshot) {
			defer letting.Done()
			// THROUGH THE SAME DOOR the person's own click goes through,
			// because where the agent lives decides how it is re-entered.
			b.app.ResumeDetached(park, true)
		}(park)
	}
	letting.Wait()
}

// cancelSession stops every background delegation a conversation still has in
// flight, and reports how many. It is what deleting a chat calls, so a removed
// conversation does not leave agents running against records that are about
// to disappear.
func (b *backgroundManager) cancelSession(sessionID int64) int {
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTerminalTimeout)
	defer cancel()

	running, err := b.app.Store.Agent().RunningDelegations(ctx, sessionID)
	if err != nil {
		return 0
	}
	n := 0
	for _, d := range running {
		if d.FleetID != nil {
			// A fleet member is stopped through its fleet, once, rather than one
			// row at a time: the batch has to be closed too, or the join would
			// wake the Gateway about a conversation that no longer exists.
			continue
		}
		if b.cancel(d.ID) {
			n++
		}
	}
	// Every batch this conversation still has running goes with it.
	fleets, err := b.app.Store.Agent().SessionFleets(ctx, sessionID)
	if err != nil {
		return n
	}
	for _, f := range fleets {
		if f.Status != model.FleetRunning {
			continue
		}
		if b.app.CancelFleet(ctx, f.WorkspaceID, f.ID) {
			n += f.Size
		}
	}
	return n
}

// notifyServerTurn tells the person's tabs a server-initiated turn is streaming,
// so they attach and hear it. It is the run manager's OnServerTurn callback: a
// completion turn (KB/27) has no request behind it, so nothing would otherwise
// draw the person to it.
func (a *App) notifyServerTurn(r *run.Run) {
	if a.WS == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTerminalTimeout)
	defer cancel()

	session, err := a.Store.Agent().GetSession(ctx, r.WorkspaceID, r.SessionID)
	if err != nil {
		return
	}
	a.WS.Notify(r.WorkspaceID, r.UserID, map[string]any{
		"type":    "turn",
		"payload": map[string]any{"chat_uid": session.UID, "run": r.UID},
	})
}

// shutdown stops every running background delegation and waits for the
// goroutines to settle, so a process going down does not abandon them mid-write.
// quiesce stops new background agents starting. The ones already running are
// left alone: they are what a graceful shutdown is trying to protect.
func (b *backgroundManager) quiesce() {
	b.mu.Lock()
	b.quiescing = true
	b.mu.Unlock()
}

// drain waits for the agents still working, and reports whether they all
// finished. It cancels nothing.
func (b *backgroundManager) drain(ctx context.Context) bool {
	for {
		b.mu.Lock()
		n := len(b.running)
		b.mu.Unlock()
		if n == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// shutdown cancels whatever is still running and waits for it to unwind. The
// END of a graceful shutdown: quiesce and drain come first, and by the time
// this runs the only agents left are the ones that would not finish in the time
// we were willing to give them.
func (b *backgroundManager) shutdown() {
	b.baseCancel()
	b.wg.Wait()
}
