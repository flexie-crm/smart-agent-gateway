package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/queue"
	"flexie.io/sag/internal/store"
)

// A fleet, from the Gateway's one call to the Gateway's one answer (Mode B,
// KB/27 and KB/35).
//
// Three processes touch it and none of them holds it:
//
//	the Gateway   writes a fleet row and one delegation per member, enqueues a
//	              job each, and ends its turn saying it has started them
//	a worker      claims a job, runs that one agent, writes its result on the
//	              delegation, and rings the bell
//	the server    hears the bell, ASKS THE DATABASE how many are back, and when
//	              the last one is, wakes the Gateway once with all of it
//
// The counting is the part worth being careful about, and the care is all in
// not being clever: nobody keeps a tally. The worker knows nothing about
// fleets; the server counts rows. Two members finishing in the same instant
// both see a full count, and the conditional update in CloseFleet decides which
// of them tells the Gateway. A report that never arrives costs nothing either,
// because the sweep asks the same question on a timer: the message is a
// doorbell, the database is the record.

const (
	// fleetReportSubject is the way back from a worker: one member is done.
	// It carries no result (that is on the delegation row), only the fact.
	fleetReportSubject = queue.ReportSubject + ".fleet"
	// fleetReportGroup is the durable consumer the servers share, so a report
	// is handled once however many are listening.
	fleetReportGroup = "fleet-reports"

	// fleetCancelSubject carries "stop this fleet" to every worker at once.
	//
	// It is broadcast rather than queued, because the instruction is about
	// whichever process happens to be running the work and nobody knows which
	// that is. A worker holding none of it does nothing; a worker holding two
	// members of it stops both.
	fleetCancelSubject = queue.OpsSubject + ".fleet.cancel"

	// fleetSweepEvery is how often the join runs without being told to. It is
	// the backstop for a lost report and the enforcer of the deadline, so it is
	// frequent enough to be unnoticeable and rare enough to be free.
	fleetSweepEvery = 30 * time.Second

	// fleetWriteTimeout bounds the detached writes this file makes. They happen
	// after the turn that asked for them has ended, so they carry their own
	// deadline rather than inheriting a context that is already dying.
	fleetWriteTimeout = 30 * time.Second
)

// fleetJobPayload is what a worker is told, and it is deliberately two numbers.
// Everything else about the work (which agent, which conversation, what it was
// asked to do) is on the delegation row, which is the source of truth; a
// payload that repeated any of it would be a second copy that can disagree.
//
// The fleet id is here rather than only on the row because it is what a CANCEL
// names. A worker that has been told to stop a fleet has to know, without going
// to the database, whether any of what it is running belongs to it.
type fleetJobPayload struct {
	DelegationID int64 `json:"delegation_id"`
	FleetID      int64 `json:"fleet_id"`
}

// fleetCancel is the instruction. One number: the batch to stop.
type fleetCancel struct {
	FleetID int64 `json:"fleet_id"`
}

// fleetReport is the bell. Same reasoning: the fleet id is enough to go and
// look, and the delegation id is there so a log line says which member.
//
// Parked is the one thing the row cannot say. A member that stopped to ask is
// still `running` (a park is not terminal), so a report that only named the
// fleet would be indistinguishable from one that changed nothing, and the card
// would sit in the database until somebody reloaded. It says which of the two
// happened; what happened is still read from the database.
type fleetReport struct {
	FleetID      int64 `json:"fleet_id"`
	DelegationID int64 `json:"delegation_id"`
	Parked       bool  `json:"parked,omitempty"`
}

// fleetResumeJob carries a person's answer back to a worker.
//
// The SNAPSHOT travels, not its id, and that is the one place in this file
// where the database is not the thing consulted. Answering a card CLAIMS the
// park atomically (two clicks cannot run the action twice), so by the time this
// job exists the row is spent: the snapshot is no longer something a worker
// could go and read, and it is exactly what model.Job.Payload is for.
type fleetResumeJob struct {
	DelegationID int64               `json:"delegation_id"`
	FleetID      int64               `json:"fleet_id"`
	Approved     bool                `json:"approved"`
	Snapshot     *model.ParkSnapshot `json:"snapshot"`
}

// StartFleet dispatches a batch the Gateway asked for. It is the turn's
// StartFleet seam, and it runs while the turn is ending, so every write it makes
// is detached with its own deadline.
func (a *App) StartFleet(ctx context.Context, req agent.FleetRequest) {
	wc, cancel := context.WithTimeout(context.WithoutCancel(ctx), fleetWriteTimeout)
	defer cancel()

	// Start WATCHING before dispatching, so a member that finishes immediately
	// is counted rather than arriving before anybody was listening. Everything
	// the chip will need is captured here, where it is all in hand: reading it
	// back per report is exactly the queries this exists to avoid.
	track := &fleetTrack{
		FleetID:     req.FleetID,
		WorkspaceID: req.WorkspaceID,
		UserID:      req.UserID,
		SessionUID:  a.sessionUID(wc, req.WorkspaceID, req.SessionID),
		Name:        fleetTrackName(req.Members),
		Size:        len(req.Members),
		Deadline:    req.Deadline,
	}
	// Say it is running BEFORE anything runs.
	//
	// The batch's chip used to be sent by the first member REPORTING, which is
	// invisibly wrong: with quick agents the first report is immediate and the
	// chip looks like it appeared on dispatch, but a batch whose agents all take
	// a while showed nothing at all until it was over. Somebody asks for thirteen
	// agents and watches an empty column, which is the one moment the rail exists
	// for. Built and pushed here, before watch, so nothing else can be holding
	// this track yet and `done` cannot be anything but zero.
	a.pushChip(track.chip(model.DelegationRunning), req.WorkspaceID, req.UserID)
	a.fleetTracker.watch(track, a.onFleetOutOfTime)
	a.event().Int64("fleet", req.FleetID).Int("agents", len(req.Members)).Msg("fleet dispatched")

	if err := a.enqueueFleetMembers(wc, req); err != nil {
		a.Log.Error().Err(err).Int64("fleet_id", req.FleetID).Msg("dispatch fleet")
		// Nothing can run, so the batch is settled now rather than left claiming
		// to be working: the Gateway is told what did not happen instead of
		// waiting for the deadline.
		for _, m := range req.Members {
			a.settleFleetMember(wc, m.DelegationID, req.FleetID, model.DelegationFailed, nil,
				"this task could not be started")
		}
	}
}

// enqueueFleetMembers writes the batch's jobs in ONE statement, and then rings
// the bells.
//
// Rows before signals, always: a signal for a row that is not committed is a
// worker looking for work that does not exist (KB/05), while a row with no
// signal is merely slow, because the worker's sweep finds it.
func (a *App) enqueueFleetMembers(ctx context.Context, req agent.FleetRequest) error {
	if a.Queue == nil {
		return errors.New("no queue is connected")
	}
	subject := queue.SubjectRoot + "." + queue.CapabilityDefault + "." + model.JobKindAgentRun
	jobs := make([]*model.Job, len(req.Members))
	for i, m := range req.Members {
		payload, err := json.Marshal(fleetJobPayload{DelegationID: m.DelegationID, FleetID: req.FleetID})
		if err != nil {
			return fmt.Errorf("encode fleet job: %w", err)
		}
		jobs[i] = &model.Job{
			WorkspaceID: req.WorkspaceID,
			Kind:        model.JobKindAgentRun,
			Subject:     subject,
			Payload:     payload,
			// One retry, matching what a background agent gets after a hard kill
			// (model.MaxDelegationAttempts). An agent that ran and failed on its
			// own terms has already written its outcome and will not be retried;
			// this covers the worker process dying under it.
			MaxAttempts: model.MaxDelegationAttempts,
		}
	}
	if err := a.Store.Jobs().EnqueueMany(ctx, jobs); err != nil {
		return fmt.Errorf("enqueue fleet jobs: %w", err)
	}

	for i, job := range jobs {
		a.event().Int64("fleet", req.FleetID).Int64("del", req.Members[i].DelegationID).
			Str("agent", req.Members[i].Sub.Key).Msg("dispatching member")
		if err := a.Queue.Enqueue(ctx, job.Subject, queue.Task{
			ID: job.ID, Kind: job.Kind, WorkspaceID: job.WorkspaceID, Payload: job.Payload,
		}); err != nil {
			// The rows are committed, so the work is findable: a lost signal
			// costs the sweep's interval, not the job.
			a.Log.Warn().Err(err).Str("job", job.ID).Msg("fleet job signal not sent")
		}
	}
	return nil
}

// RunFleetMemberJob is the worker's handler for one member: run that agent, and
// write down what it said.
//
// It is idempotent on the delegation, because delivery is at-least-once and a
// job can be reclaimed after the work was done but before the bell was rung: a
// member that is already terminal is not run again, it is only reported.
func (a *App) RunFleetMemberJob(ctx context.Context, job *model.Job) error {
	var p fleetJobPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		// A job we cannot read cannot be retried into readability. Failing it
		// permanently is honest; the delegation it named is settled by the
		// fleet's deadline.
		a.Log.Error().Err(err).Str("job", job.ID).Msg("unreadable fleet job")
		return nil
	}
	del, err := a.Store.Agent().GetDelegation(ctx, p.DelegationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The conversation was deleted while this waited. Nothing to run.
			return nil
		}
		return fmt.Errorf("load fleet member: %w", err)
	}
	if del.FleetID == nil {
		a.Log.Error().Int64("delegation_id", del.ID).Msg("fleet job for a delegation with no fleet")
		return nil
	}
	fleetID := *del.FleetID

	if del.Terminal() {
		// Already run. What is missing is the bell.
		a.event().Int64("fleet", fleetID).Int64("del", del.ID).Msg("worker member already finished, reporting")
		a.reportFleetMember(ctx, fleetID, del.ID)
		return nil
	}

	session, err := a.Store.Agent().GetSession(ctx, del.WorkspaceID, del.SessionID)
	if err != nil {
		return fmt.Errorf("load fleet member session: %w", err)
	}
	// Resolved LIVE, like every other path that starts an agent from a row: one
	// an administrator removed or revoked while this waited is not quietly run.
	sub, err := a.ResolveAgent(ctx, del.WorkspaceID, session.UserID, noComputer(), del.AgentKey)
	if err != nil {
		a.Log.Warn().Err(err).Str("agent", del.AgentKey).Msg("fleet member agent unavailable")
		a.settleFleetMember(ctx, del.ID, fleetID, model.DelegationFailed, nil,
			"the agent assigned this task is no longer available")
		return nil
	}

	runCtx, cancel := context.WithTimeout(ctx, memberTimeout(sub))
	defer cancel()
	// Registered BEFORE it starts, so a cancel that arrives a millisecond into
	// the run finds it. Released on the way out, so the map holds what is live.
	defer a.fleets.add(fleetID, del.ID, cancel)()

	// Nothing is streaming this. A member runs with no reader: there is no live
	// request behind it and no socket on this process, so its frames go nowhere
	// and its progress is the delegation row the server reads. What the person
	// sees is the fleet's chip, which counts members rather than tokens.
	a.event().Int64("fleet", fleetID).Int64("del", del.ID).Str("agent", del.AgentKey).
		Str("job", job.ID).Msg("worker running agent")

	out := chat.NewStream(discardFrames{})
	result, runErr := a.Agent.RunFleetMember(runCtx, agent.BackgroundDelegation{
		DelegationID: del.ID,
		Sub:          sub,
		Task:         del.Task,
		ParentCallID: del.ParentToolCallID,
		WorkspaceID:  del.WorkspaceID,
		UserID:       session.UserID,
		SessionID:    del.SessionID,
	}, out)

	a.settleFleetRun(ctx, runCtx, del, fleetID, result, runErr)
	// The agent ran, whatever it decided. The job is finished with either way:
	// retrying it would run the same work a second time on the same money.
	return nil
}

// settleFleetRun records how one leg of a member ended. Both legs come here, the
// first run and a resumed one, because they end in exactly the same five ways
// and the fleet cannot tell them apart.
func (a *App) settleFleetRun(ctx, runCtx context.Context, del *model.AgentDelegation, fleetID int64, result string, runErr error) {
	outcome := "done"
	switch {
	case errors.Is(runErr, agent.ErrBackgroundParked):
		outcome = "waiting for approval"
	case runErr != nil:
		outcome = "failed"
	}
	a.event().Int64("fleet", fleetID).Int64("del", del.ID).Str("outcome", outcome).
		Msg("worker agent finished")

	switch {
	case errors.Is(runErr, agent.ErrBackgroundParked):
		// Suspended, not finished. The card is a durable row this worker has
		// already written; what it cannot do is put it on somebody's screen, so
		// it says so and the server, which owns the sockets, surfaces it. The
		// delegation stays running and the batch keeps waiting for it.
		a.reportFleetPark(ctx, fleetID, del.ID)
	case runErr != nil && errors.Is(runCtx.Err(), context.Canceled):
		// Stopped on instruction. The rows were settled by whoever asked for it,
		// before the instruction was sent, so there is nothing to write and
		// nothing to report: saying anything here would be this process's guess
		// about a decision another one already recorded.
		a.Log.Info().Int64("delegation_id", del.ID).Msg("fleet member stopped on instruction")
	case runErr != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded):
		a.settleFleetMember(ctx, del.ID, fleetID, model.DelegationFailed, nil,
			"this task ran past its time limit and was stopped before it returned a result")
	case runErr != nil:
		a.Log.Error().Err(runErr).Int64("delegation_id", del.ID).Msg("fleet member failed")
		a.settleFleetMember(ctx, del.ID, fleetID, model.DelegationFailed, nil,
			"this task could not be completed")
	default:
		payload, _ := json.Marshal(map[string]string{"result": result})
		a.settleFleetMember(ctx, del.ID, fleetID, model.DelegationDone, payload, "")
	}
}

// settleFleetMember writes a member's outcome and rings the bell. The write is
// detached: the job's context may already be unwinding, and a result nobody
// records is a fleet that waits for its deadline.
func (a *App) settleFleetMember(ctx context.Context, delegationID, fleetID int64, status string, result json.RawMessage, errText string) {
	wc, cancel := context.WithTimeout(context.WithoutCancel(ctx), fleetWriteTimeout)
	defer cancel()

	if err := a.Store.Agent().CompleteDelegation(wc, delegationID, status, result, errText); err != nil {
		// Already terminal (a cancel got here first, or a duplicate delivery).
		// The bell is still rung: whoever settled it may not have.
		a.Log.Debug().Err(err).Int64("delegation_id", delegationID).Msg("fleet member already resolved")
	}
	a.reportFleetMember(wc, fleetID, delegationID)
}

// reportFleetPark tells the server a member is waiting on a person, so the card
// it has already written reaches a screen.
//
// UNLIKE a finished member, this one is not covered by the sweep: the sweep asks
// "is this batch complete", and a parked member's answer is no. A dropped bell
// here costs the card its live appearance, and it comes back on the next reload
// of the conversation, which is the same floor a background agent's card has.
func (a *App) reportFleetPark(ctx context.Context, fleetID, delegationID int64) {
	a.event().Int64("fleet", fleetID).Int64("del", delegationID).Str("kind", "parked").
		Msg("worker reporting")
	a.publishFleetReport(ctx, fleetReport{FleetID: fleetID, DelegationID: delegationID, Parked: true})
}

// reportFleetMember tells whoever is joining that one member is back. It is
// best-effort on purpose: the sweep asks the same question on a timer, so a
// dropped bell is a slower answer and never a lost one.
func (a *App) reportFleetMember(ctx context.Context, fleetID, delegationID int64) {
	a.event().Int64("fleet", fleetID).Int64("del", delegationID).Str("kind", "done").
		Msg("worker reporting")
	a.publishFleetReport(ctx, fleetReport{FleetID: fleetID, DelegationID: delegationID})
}

func (a *App) publishFleetReport(ctx context.Context, report fleetReport) {
	fleetID, delegationID := report.FleetID, report.DelegationID
	if a.Queue == nil {
		return
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return
	}
	// The id is the member, so a report published twice for one member (a
	// retried publish) is deduplicated by the broker rather than counted twice.
	// Counting is done from rows anyway; this only saves the work.
	// The id includes WHICH report this is, because one member legitimately
	// reports twice: once when it stops to ask, and once when it is done. One id
	// for both and the broker would swallow the second as a duplicate.
	kind := "done"
	if report.Parked {
		kind = "parked"
	}
	err = a.Queue.Enqueue(ctx, fleetReportSubject, queue.Task{
		ID:          "fleet-" + strconv.FormatInt(fleetID, 10) + "-" + strconv.FormatInt(delegationID, 10) + "-" + kind,
		Kind:        model.JobKindAgentRun,
		Payload:     payload,
		WorkspaceID: 0,
	})
	if err != nil {
		a.Log.Warn().Err(err).Int64("fleet_id", fleetID).Msg("report fleet member")
	}
}

// ResumeDetached answers a card raised by an agent running away from the turn
// that started it, and sends it back to WHERE THAT AGENT LIVES.
//
// One door, because there are two answers and two callers: a person clicking
// approve, and "approve all" letting the queue through. They used to disagree,
// and the disagreement was invisible: the queue drain always chose the
// in-process one, so a batch approved wholesale ran its members in the wrong
// process, on the wrong rows.
func (a *App) ResumeDetached(snapshot *model.ParkSnapshot, approved bool) {
	if snapshot.HandoffMode == model.HandoffFleet {
		a.ResumeFleetMember(snapshot, approved)
		return
	}
	a.ResumeBackground(snapshot, approved)
}

// ResumeFleetMember carries a person's answer back to a worker.
//
// The agent that raised the card is gone: its goroutine ended when it parked, in
// a process this one cannot reach. So the answer becomes a job, and whichever
// worker is free re-enters the agent from the snapshot, exactly as the server
// re-launches a background agent in itself.
//
// What stays HERE is everything about the conversation, because the conversation
// is this process's: clearing the waiting status, moving the chip, and telling
// the dashboard the approval is answered. A worker owns the work; the server
// owns the person.
func (a *App) ResumeFleetMember(snapshot *model.ParkSnapshot, approved bool) {
	ctx, cancel := context.WithTimeout(context.Background(), fleetWriteTimeout)
	defer cancel()

	del, err := a.fleetMemberOfPark(ctx, snapshot)
	if err != nil {
		a.Log.Error().Err(err).Int64("session_id", snapshot.SessionID).
			Str("agent", snapshot.AgentKey).Msg("resume: fleet member not found")
		return
	}

	// The person answered, so the conversation is no longer waiting on a card.
	// Cleared BEFORE the job is enqueued, or a completion turn scheduled the
	// moment this member finishes would be held behind a wait already over.
	if err := a.Store.Agent().SetSessionStatus(ctx, snapshot.SessionID, model.SessionRunning); err != nil {
		a.Log.Error().Err(err).Int64("session_id", snapshot.SessionID).Msg("resume: clear waiting status")
	}
	a.bg.markResumed(ctx, del.ID, snapshot.WorkspaceID)

	if err := a.enqueueFleetResume(ctx, del, *del.FleetID, snapshot, approved); err != nil {
		a.Log.Error().Err(err).Int64("delegation_id", del.ID).Msg("resume: enqueue fleet member")
		// Nothing will re-enter this agent, so it is settled here rather than
		// left running until the batch's Deadline: an answered card that leads
		// nowhere is worse than a refusal, because the person believes it went.
		a.settleFleetMember(ctx, del.ID, *del.FleetID, model.DelegationFailed, nil,
			"this task could not be continued after your decision")
	}
}

// fleetMemberOfPark is which member raised this card. The card says so: a
// batch's members share one parent call and can be the same agent twice, so it
// is the delegation id on the snapshot or it is a guess.
func (a *App) fleetMemberOfPark(ctx context.Context, snapshot *model.ParkSnapshot) (*model.AgentDelegation, error) {
	if snapshot.DelegationID == nil {
		return nil, store.ErrNotFound
	}
	del, err := a.Store.Agent().GetDelegation(ctx, *snapshot.DelegationID)
	if err != nil {
		return nil, err
	}
	if del.FleetID == nil || del.SessionID != snapshot.SessionID {
		return nil, store.ErrNotFound
	}
	return del, nil
}

func (a *App) enqueueFleetResume(ctx context.Context, del *model.AgentDelegation, fleetID int64, snapshot *model.ParkSnapshot, approved bool) error {
	if a.Queue == nil {
		return errors.New("no queue is connected")
	}
	payload, err := json.Marshal(fleetResumeJob{
		DelegationID: del.ID, FleetID: fleetID, Approved: approved, Snapshot: snapshot,
	})
	if err != nil {
		return fmt.Errorf("encode fleet resume: %w", err)
	}
	job := &model.Job{
		WorkspaceID: del.WorkspaceID,
		Kind:        model.JobKindAgentResume,
		Subject:     queue.SubjectRoot + "." + queue.CapabilityDefault + "." + model.JobKindAgentResume,
		Payload:     payload,
		// ONE attempt. The approved action may already have run by the time
		// anything could go wrong, and running it a second time because a worker
		// died mid-write is the one outcome an approval must never produce.
		MaxAttempts: 1,
	}
	if err := a.Store.Jobs().Enqueue(ctx, job); err != nil {
		return fmt.Errorf("enqueue fleet resume: %w", err)
	}
	return a.Queue.Enqueue(ctx, job.Subject, queue.Task{
		ID: job.ID, Kind: job.Kind, WorkspaceID: job.WorkspaceID, Payload: job.Payload,
	})
}

// RunFleetResumeJob is the worker's half of an answered card: re-enter the agent
// where it stopped, and finish the member as any other run of it would.
func (a *App) RunFleetResumeJob(ctx context.Context, job *model.Job) error {
	var p fleetResumeJob
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.Snapshot == nil {
		a.Log.Error().Err(err).Str("job", job.ID).Msg("unreadable fleet resume job")
		return nil
	}
	del, err := a.Store.Agent().GetDelegation(ctx, p.DelegationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // the conversation was deleted while the card waited
		}
		return fmt.Errorf("load fleet member: %w", err)
	}
	if del.Terminal() {
		// Cancelled, or out of time, while the card was on screen. The decision
		// arrived too late to matter, and re-entering would restart work the
		// batch has already accounted for.
		a.reportFleetMember(ctx, p.FleetID, del.ID)
		return nil
	}

	sub, err := a.ResolveAgent(ctx, del.WorkspaceID, p.Snapshot.UserID, noComputer(), del.AgentKey)
	if err != nil {
		a.Log.Warn().Err(err).Str("agent", del.AgentKey).Msg("resume: agent no longer available")
		a.settleFleetMember(ctx, del.ID, p.FleetID, model.DelegationFailed, nil,
			"the agent working on this is no longer available")
		return nil
	}

	runCtx, cancel := context.WithTimeout(ctx, memberTimeout(sub))
	defer cancel()
	defer a.fleets.add(p.FleetID, del.ID, cancel)()

	out := chat.NewStream(discardFrames{})
	result, runErr := a.Agent.ResumeFleetMember(runCtx, agent.BackgroundResume{
		Snapshot: p.Snapshot, Approved: p.Approved, Sub: sub,
		WorkspaceID: del.WorkspaceID, UserID: p.Snapshot.UserID,
		SessionID: del.SessionID, ModelID: p.Snapshot.ModelID,
	}, out)
	a.settleFleetRun(ctx, runCtx, del, p.FleetID, result, runErr)
	return nil
}

// sessionUID is the conversation's public id, read once when a batch is
// dispatched so no report ever has to look it up.
func (a *App) sessionUID(ctx context.Context, workspaceID, sessionID int64) string {
	session, err := a.Store.Agent().GetSession(ctx, workspaceID, sessionID)
	if err != nil {
		return ""
	}
	return session.UID
}

// fleetTrackName is what the chip is called, decided at dispatch from the
// agents themselves: one kind reads as that agent, a mixed batch as a count.
func fleetTrackName(members []agent.FleetMember) string {
	if len(members) == 0 {
		return "agents"
	}
	name := members[0].Sub.Name
	if name == "" {
		name = members[0].Sub.Key
	}
	for _, m := range members[1:] {
		if m.Sub.Name != members[0].Sub.Name {
			return fmt.Sprintf("%d agents", len(members))
		}
	}
	if len(members) == 1 {
		return name
	}
	return fmt.Sprintf("%d × %s", len(members), name)
}

// memberTimeout is how long one working leg of a member gets: what its agent was
// configured with, or the code default. It is the SAME number the batch's
// deadline is built from, so a member cannot outlive the batch waiting for it.
func memberTimeout(sub agent.AgentProfile) time.Duration {
	if sub.BackgroundTimeout > 0 {
		return sub.BackgroundTimeout
	}
	return model.DefaultBackgroundTimeout
}

// RunFleetJoin is the server's half: hear the bells, and keep asking anyway.
//
// Two goroutines, because they answer two different questions. The subscription
// reacts to a member finishing, which is the fast path and the ordinary one. The
// sweep asks, on a timer, whether any fleet is complete without anybody having
// said so, and whether any has run past its deadline. Neither is a fallback for
// the other being broken: the first makes it prompt, the second makes it true.
func (a *App) RunFleetJoin(ctx context.Context) {
	if a.Queue == nil {
		a.Log.Warn().Msg("no queue connected: fleets are not available on this server")
		return
	}
	go a.readFleetReports(ctx)
	a.sweepFleetsUntil(ctx)
}

func (a *App) readFleetReports(ctx context.Context) {
	sub, err := a.Queue.SubscribeAll(ctx, fleetReportSubject, fleetReportGroup)
	if err != nil {
		a.Log.Error().Err(err).Msg("subscribe to fleet reports")
		return
	}
	defer func() { _ = sub.Close() }()

	// Sitting on a live connection, and handling what arrives IN PARALLEL.
	//
	// Both halves matter. Live, because a report is the moment a batch might be
	// complete, and a conversation waiting on it should be answered then and not
	// at somebody's next look. In parallel, because this process is not doing
	// the work: a hundred workers finishing in the same second are a hundred
	// things to react to at once, and handling them one after another would move
	// the queue out of the broker and into this goroutine, where one slow
	// reaction holds up every other batch on the machine.
	//
	// Unbounded on purpose. Each reaction is a handful of reads and a socket
	// push, the count is bounded by how many agents are actually running, and
	// the real limit is the database pool, which is a queue that already exists
	// and is sized for it. A semaphore here would be a second one, tuned by
	// guesswork, in front of the first.
	var reacting sync.WaitGroup
	defer reacting.Wait()

	for {
		select {
		case <-ctx.Done():
			return
		case task, ok := <-sub.Tasks():
			if !ok {
				return // the subscription is over
			}
			var report fleetReport
			if err := json.Unmarshal(task.Payload, &report); err != nil {
				continue
			}
			reacting.Add(1)
			go func() {
				defer reacting.Done()
				a.onFleetReport(ctx, report)
			}()
		}
	}
}

func (a *App) sweepFleetsUntil(ctx context.Context) {
	ticker := time.NewTicker(fleetSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.sweepFleets(ctx)
		}
	}
}

// sweepFleets closes what nobody told us about, and gives up on what has run out
// of time.
func (a *App) sweepFleets(ctx context.Context) {
	settled, err := a.Store.Agent().SettledFleets(ctx)
	if err != nil {
		a.Log.Error().Err(err).Msg("scan settled fleets")
	}
	for _, f := range settled {
		a.closeFleet(ctx, f, model.FleetDone)
	}

	overdue, err := a.Store.Agent().OverdueFleets(ctx)
	if err != nil {
		a.Log.Error().Err(err).Msg("scan overdue fleets")
		return
	}
	for _, f := range overdue {
		// What has not come back by now is recorded as not having. The fleet is
		// then answered with what DID come back, which is the whole reason a
		// deadline exists: nineteen results and one that hung should read as
		// nineteen results and a note, not as a conversation waiting forever.
		n, err := a.Store.Agent().AbandonFleetMembers(ctx, f.ID, fleetOutOfTime)
		if err != nil {
			a.Log.Error().Err(err).Int64("fleet_id", f.ID).Msg("abandon overdue fleet members")
			continue
		}
		if n > 0 {
			a.Log.Warn().Int64("fleet_id", f.ID).Int("agents", n).Msg("fleet members ran out of time")
		}
		a.closeFleet(ctx, f, model.FleetDone)
	}
}

// fleetOutOfTime is what a member that never came back is recorded as. It is
// written where the Gateway will read it, so it says what happened rather than
// only that something did.
const fleetOutOfTime = "this task did not finish before the batch ran out of time"

// onFleetReport is what a bell means: one member of a batch has something to
// say. Two things it can be, and only one of them is ever expensive.
//
// A member STOPPED TO ASK. Its card is already a row; what it needs is a
// screen, and this process has one.
//
// A member IS BACK. The counter this process has been keeping goes up, the chip
// on somebody's screen moves, and that is all: no query, nothing to wait for,
// nothing for the next report to queue behind. Only when the count says the
// batch is COMPLETE does the database get read, and then it is read once, for
// the thing it is finally worth reading for: the results, the close, and the
// Gateway's turn.
func (a *App) onFleetReport(ctx context.Context, report fleetReport) {
	if report.Parked {
		a.onFleetMemberParked(ctx, report)
		return
	}

	progress, known, complete := a.fleetTracker.record(report.FleetID, report.DelegationID)
	if !known {
		// This process never dispatched it: a restart, or another server did.
		// Fall back to asking the database, which is slower and always right.
		a.event().Int64("fleet", report.FleetID).Msg("master report, batch not tracked here")
		a.closeFleetFromStore(ctx, report.FleetID)
		return
	}

	a.event().Int64("fleet", report.FleetID).Int64("del", report.DelegationID).
		Str("progress", progress.String()).Bool("complete", complete).Msg("master report")

	// Straight to the screen from memory. This is the ordinary case, it happens
	// once per member, and it costs a socket write.
	a.pushChip(progress.chip(model.DelegationRunning), progress.WorkspaceID, progress.UserID)
	if !complete {
		return
	}

	// The last one. NOW the database is worth reading.
	a.event().Int64("fleet", report.FleetID).Msg("master batch complete, closing")
	a.closeFleetFromStore(ctx, report.FleetID)
}

// onFleetOutOfTime is a batch that waited as long as its agents were given.
//
// The wait is the LONGEST of its members' own configured limits, so by now
// anything still running has already outlived what an administrator said it may
// take. The conversation is answered with everything that did come back, and
// what did not is written down as not having: nineteen results and a note beats
// a conversation waiting on the twentieth forever.
//
// What it does NOT do is overwrite a member that already recorded its own
// failure. AbandonFleetMembers only touches rows still claiming to be running,
// so an agent that failed for a reason of its own keeps that reason and only a
// silent straggler gets the timeout.
func (a *App) onFleetOutOfTime(fleetID int64) {
	if !a.fleetTracker.expire(fleetID) {
		return // it finished, or was cancelled, while the timer was going off
	}
	ctx, cancel := context.WithTimeout(context.Background(), fleetWriteTimeout)
	defer cancel()

	fleet, err := a.Store.Agent().Fleet(ctx, fleetID)
	if err != nil || fleet.Status != model.FleetRunning {
		return
	}
	// A batch with a card on screen is NOT out of time: it is waiting on a
	// person, a card stays answerable for a day, and somebody at lunch must not
	// cost their colleague the answers the other agents came back with. The
	// sweep will take it when the card is answered and the work really stalls.
	if _, perr := a.Store.Agent().PendingPark(ctx, fleet.SessionID); perr == nil {
		a.event().Int64("fleet", fleetID).Msg("master batch out of time, but waiting on a person")
		return
	}

	n, err := a.Store.Agent().AbandonFleetMembers(ctx, fleetID, fleetOutOfTime)
	if err != nil {
		a.Log.Error().Err(err).Int64("fleet_id", fleetID).Msg("abandon overdue fleet members")
		return
	}
	a.event().Int64("fleet", fleetID).Int("gave_up_on", n).Msg("master batch out of time")
	if n > 0 {
		a.Log.Warn().Int64("fleet_id", fleetID).Int("agents", n).Msg("fleet members ran out of time")
	}
	a.closeFleet(ctx, fleet, model.FleetDone)
}

// onFleetMemberParked puts a card a worker wrote onto somebody's screen.
func (a *App) onFleetMemberParked(ctx context.Context, report fleetReport) {
	fleet, err := a.Store.Agent().Fleet(ctx, report.FleetID)
	if err != nil {
		return
	}
	a.event().Int64("fleet", report.FleetID).Int64("del", report.DelegationID).
		Msg("master report, member waiting for approval")
	a.bg.markParked(ctx, agent.BackgroundDelegation{
		DelegationID: report.DelegationID,
		WorkspaceID:  fleet.WorkspaceID,
		SessionID:    fleet.SessionID,
	})
}

// closeFleetFromStore reads the batch and finishes it if it is finished. It is
// the slow path, and it runs once per batch.
func (a *App) closeFleetFromStore(ctx context.Context, fleetID int64) {
	fleet, err := a.Store.Agent().Fleet(ctx, fleetID)
	if err != nil {
		return
	}
	a.notifyFleet(ctx, fleet)
	if fleet.Status != model.FleetRunning {
		return
	}
	a.closeFleet(ctx, fleet, model.FleetDone)
}

// pushChip sends one chip to a person's tabs. No reads: everything on it was
// already known.
func (a *App) pushChip(chip Chip, workspaceID, userID int64) {
	if a.WS == nil {
		return
	}
	a.WS.Notify(workspaceID, userID, map[string]any{"type": "delegation", "payload": chip})
}

// closeFleet finishes a fleet if it is finished, and wakes the Gateway if THIS
// call was the one that closed it.
//
// The conditional update is the whole of the concurrency design. Two members
// reporting in the same instant both read a full tally; exactly one of them gets
// true back, and only that one schedules a turn. Nothing is locked and nothing
// is coordinated between processes.
func (a *App) closeFleet(ctx context.Context, fleet *model.AgentFleet, status string) {
	done, size, err := a.Store.Agent().FleetTally(ctx, fleet.ID)
	if err != nil {
		a.Log.Error().Err(err).Int64("fleet_id", fleet.ID).Msg("tally fleet")
		return
	}
	if done < size {
		return
	}
	won, err := a.Store.Agent().CloseFleet(ctx, fleet.ID, status)
	if err != nil {
		a.Log.Error().Err(err).Int64("fleet_id", fleet.ID).Msg("close fleet")
		return
	}
	if !won {
		return // somebody else is telling the Gateway
	}
	a.Log.Info().Int64("fleet_id", fleet.ID).Int("agents", size).Msg("fleet finished")
	a.event().Int64("fleet", fleet.ID).Msg("master batch closed, waking the Gateway")
	// Followed to its end: nothing left to count.
	a.fleetTracker.forget(fleet.ID)
	a.notifyFleet(ctx, fleet)
	a.scheduleFleetCompletion(ctx, fleet)
}

// scheduleFleetCompletion wakes the Gateway ONCE, with every result on the one
// call it made. It is the same server-initiated turn a single background agent
// gets (KB/27); the only difference is that it is the join.
func (a *App) scheduleFleetCompletion(ctx context.Context, fleet *model.AgentFleet) {
	session, err := a.Store.Agent().GetSession(ctx, fleet.WorkspaceID, fleet.SessionID)
	if err != nil {
		a.Log.Error().Err(err).Int64("session_id", fleet.SessionID).Msg("load session for fleet completion")
		return
	}
	profile, loadout, err := a.Resolve(ctx, ProfileRequest{
		WorkspaceID: fleet.WorkspaceID,
		UserID:      session.UserID,
		Channel:     model.ChannelChat,
		// The model the Gateway was on when it started the batch, kept on the
		// row so this turn runs on the same one even after a restart.
		PreferredModelID: fleet.ModelID,
		SessionID:        fleet.SessionID,
	})
	if err != nil {
		a.Log.Error().Err(err).Int64("session_id", fleet.SessionID).Msg("resolve profile for fleet completion")
		return
	}
	if profile.ModelID == 0 {
		a.Log.Error().Int64("session_id", fleet.SessionID).Msg("fleet completion has no model to run on")
		return
	}

	a.Runs.Schedule(agent.Turn{
		WorkspaceID:      fleet.WorkspaceID,
		UserID:           session.UserID,
		SessionID:        fleet.SessionID,
		ModelID:          profile.ModelID,
		SystemPrompt:     profile.SystemPrompt,
		Tools:            loadout,
		Reasoning:        profile.Reasoning,
		Settings:         profile.Settings,
		ApprovalTTL:      profile.ApprovalTTL,
		MaxIterations:    profile.MaxIterations,
		MaxFleetAgents:   profile.MaxFleetAgents,
		AutoApprove:      session.ApprovalMode == model.ApprovalAuto,
		Agent:            a.AgentResolver(fleet.WorkspaceID, session.UserID, noComputer()),
		StartBackground:  a.StartBackground,
		StartFleet:       a.StartFleet,
		CompletedFleetID: fleet.ID,
	})
}

// CancelFleet stops a batch, and reports whether there was one to stop.
//
// Two steps, in this order and for a reason. The ROWS are settled first, which
// is what makes it true: the fleet is closed, its members are terminal, nothing
// is narrated and the conversation is not held. Then the instruction goes to
// the workers, which is what makes it true SOON: a member is running in another
// process, and left to itself it would keep calling a model for an answer
// nobody will read.
//
// Settling first is what makes the broadcast safe to lose. A worker that never
// hears it finishes and finds the row already terminal, so its write is a no-op
// and nothing resurrects; it cost money, not correctness.
func (a *App) CancelFleet(ctx context.Context, workspaceID, fleetID int64) bool {
	fleet, err := a.Store.Agent().Fleet(ctx, fleetID)
	if err != nil || fleet.WorkspaceID != workspaceID || fleet.Status != model.FleetRunning {
		return false
	}
	if _, err := a.Store.Agent().AbandonFleetMembers(ctx, fleetID, fleetCancelled); err != nil {
		a.Log.Error().Err(err).Int64("fleet_id", fleetID).Msg("cancel fleet members")
		return false
	}
	won, err := a.Store.Agent().CloseFleet(ctx, fleetID, model.FleetCancelled)
	if err != nil {
		a.Log.Error().Err(err).Int64("fleet_id", fleetID).Msg("close cancelled fleet")
		return false
	}
	if won {
		a.notifyFleet(ctx, fleet)
	}
	a.fleetTracker.forget(fleetID)
	a.event().Int64("fleet", fleetID).Msg("master batch cancelled, telling the workers")
	// Told to every worker, not only the one we think has it: nobody here knows
	// which process picked up which member, and a batch of twenty may be spread
	// across all of them.
	a.publishFleetCancel(ctx, fleetID)
	// Cancelled means the person said stop, so there is no completion turn: the
	// Gateway does not narrate a result nobody asked for, exactly as a cancelled
	// background agent does not (KB/27).
	return won
}

const fleetCancelled = "this task was cancelled"

// discardFrames is the sink for a run with no reader. A fleet member's frames
// have nowhere to go: it runs on a worker, and the person's picture of it is
// the fleet's chip, which counts members.
type discardFrames struct{}

func (discardFrames) Write(chat.Frame) error { return nil }

// notifyFleet pushes the fleet's chip to the person's tabs.
//
// ONE chip for the whole batch, not one per agent (KB/27): twenty chips saying
// almost the same thing is a wall, and what somebody wants to know about a fleet
// is how many there are and how many are back. So the chip is dense rather than
// repeated, and every report moves its numbers.
//
// Fire-and-forget on a short context, like the delegation push it sits beside: a
// missed push is cosmetic, and the chip is rebuilt from history on the next load.
func (a *App) notifyFleet(ctx context.Context, fleet *model.AgentFleet) {
	if a.WS == nil {
		return
	}
	wc, cancel := context.WithTimeout(context.WithoutCancel(ctx), fleetWriteTimeout)
	defer cancel()

	session, err := a.Store.Agent().GetSession(wc, fleet.WorkspaceID, fleet.SessionID)
	if err != nil {
		return
	}
	members, err := a.Store.Agent().FleetMembers(wc, fleet.ID)
	if err != nil {
		return
	}
	// Read back rather than trusted: the caller may hold a row from before the
	// close, and a chip that says "running" about a finished fleet is the kind
	// of lie a reload silently corrects an hour later.
	current, err := a.Store.Agent().Fleet(wc, fleet.ID)
	if err != nil {
		return
	}
	chip := FleetChip(current, members, a.AgentNames(wc, fleet.WorkspaceID))
	chip.ChatUID = session.UID
	a.WS.Notify(fleet.WorkspaceID, session.UserID, map[string]any{
		"type":    "delegation",
		"payload": chip,
	})
}
