package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/events"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/queue"
)

// Watching an agent work (KB/27). The chat opens a background agent, or a whole
// fleet, from its chip: it reads what has happened so far with an ordinary
// request, and from then on it is told of every step as it happens.
//
// An agent tells of a step through its turn's StepChanged, and WHERE that news
// goes depends on where the agent runs, which the code that starts it knows:
//
//	in this process  a background agent, or a fleet member that reaches the
//	                 person's computer: straight onto the event bus
//	on a worker      over the queue, as a report, which this process hears and
//	                 puts on the same bus
//
// One listener on the bus pushes it to the socket, to the person whose chat it
// is and only while they have that agent open. Nobody watching is nothing sent.

const (
	// agentReportSubject is a worker's way of saying one of its agents moved.
	// It carries which step, never the step itself: the row is the record.
	agentReportSubject = queue.ReportSubject + ".agent"
	// agentReportGroup is the consumer the servers share, as for fleet reports.
	agentReportGroup = "agent-reports"
	// agentReportTimeout bounds telling the queue, which happens in the middle
	// of the agent's own loop.
	agentReportTimeout = 5 * time.Second
)

// AgentTopic is the socket channel one agent run is watched on.
func AgentTopic(delegationID int64) string {
	return "agent:" + strconv.FormatInt(delegationID, 10)
}

// FleetTopic is the socket channel a whole batch is watched on.
func FleetTopic(fleetID int64) string {
	return "fleet:" + strconv.FormatInt(fleetID, 10)
}

// AgentRun is one agent run as the chat shows it when somebody opens it: who
// it is, how it stands, what it was asked, and why it stopped when it failed.
type AgentRun struct {
	ID          string `json:"id"`
	Agent       string `json:"agent"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	CreatedAt   int64  `json:"created_at"`
	CompletedAt int64  `json:"completed_at,omitempty"`
	Task        string `json:"task"`
	Error       string `json:"error,omitempty"`
	// Activity, Tokens and Cost are what it is doing and what it has spent so
	// far, the vendor's own counts added up over every call it made, read off
	// the same record its chip reads.
	Activity string  `json:"activity,omitempty"`
	Tokens   int64   `json:"tokens,omitempty"`
	Cost     float64 `json:"cost,omitempty"`
}

// AgentWork is one agent run opened: the run, and everything it has done so
// far in the shape the chat draws a conversation.
type AgentWork struct {
	Run      AgentRun      `json:"run"`
	Messages []ChatMessage `json:"messages"`
	// ResultID is the message that is what the agent handed back, once it
	// has. Its last words usually are, and the chat marks that message rather
	// than showing the same words twice.
	ResultID string `json:"result_id,omitempty"`
}

// agentRunOf is a delegation as a run. A parked agent's row still says
// running, so it reads "waiting for approval" here, as its chip does.
func agentRunOf(del *model.AgentDelegation, name string) AgentRun {
	if name == "" {
		name = del.AgentKey
	}
	status := del.Status
	if del.Waiting() {
		status = model.SessionWaitingApproval
	}
	run := AgentRun{
		ID:        strconv.FormatInt(del.ID, 10),
		Agent:     del.AgentKey,
		Name:      name,
		Status:    status,
		CreatedAt: del.CreatedAt.Unix(),
		Task:      del.Task,
		Error:     del.ErrorText,
	}
	if del.CompletedAt != nil {
		run.CompletedAt = del.CompletedAt.Unix()
	}
	spent := progressFrom(del.Progress)
	run.Activity, run.Tokens, run.Cost = spent.activity, spent.inTok+spent.outTok, spent.cost
	return run
}

// AgentWork reads one agent run as far as it has got, for the person it belongs
// to. The caller has already established that it is theirs.
func (a *App) AgentWork(ctx context.Context, userID int64, del *model.AgentDelegation) (AgentWork, error) {
	steps, err := a.Store.Agent().DelegationSteps(ctx, del.SessionID, del.ID)
	if err != nil {
		return AgentWork{}, err
	}
	show := a.MaySee(ctx, userID)
	work := AgentWork{
		Run:      agentRunOf(del, a.AgentNames(ctx, del.WorkspaceID)[del.AgentKey]),
		Messages: []ChatMessage{},
	}
	for _, step := range steps {
		if message, ok := a.agentMessage(ctx, del.WorkspaceID, step, show); ok {
			work.Messages = append(work.Messages, message)
		}
	}

	if del.Status != model.DelegationDone {
		return work, nil
	}
	result := agent.ResultText(del.Result)
	if result == "" {
		return work, nil
	}
	if n := len(work.Messages); n > 0 && work.Messages[n-1].Content == result {
		work.ResultID = work.Messages[n-1].ID
		return work, nil
	}
	// What it handed back is not its last words. That is an agent stopped by
	// its step limit, whose answer is written for the Gateway and never saved
	// as a step, and a fleet member whose steps were recorded before a step
	// named its run and cannot be told apart from its batch's. Either way the
	// answer is on the row, and it is what the person opened this to read.
	work.ResultID = "result-" + strconv.FormatInt(del.ID, 10)
	work.Messages = append(work.Messages, ChatMessage{
		ID: work.ResultID, Role: model.StepAssistant, Content: result,
	})
	return work, nil
}

// FleetRuns is every agent of a batch, in the order they were asked for.
func (a *App) FleetRuns(ctx context.Context, fleet *model.AgentFleet) ([]AgentRun, error) {
	members, err := a.Store.Agent().FleetMembers(ctx, fleet.ID)
	if err != nil {
		return nil, err
	}
	names := a.AgentNames(ctx, fleet.WorkspaceID)
	runs := make([]AgentRun, 0, len(members))
	for _, m := range members {
		runs = append(runs, agentRunOf(m, names[m.AgentKey]))
	}
	return runs, nil
}

// agentMessage is one of an agent's steps as the chat draws it. Its user step
// is the task it was given, which is shown as the task rather than as a
// message somebody sent.
func (a *App) agentMessage(ctx context.Context, workspaceID int64, step *model.AgentStep, show chat.Show) (ChatMessage, bool) {
	if step.Kind == model.StepUser {
		return ChatMessage{}, false
	}
	return a.ShownStep(ctx, workspaceID, step, show)
}

// stepsHere is how an agent running beside the listener tells of a step: on
// the bus, where the listener hears it.
func (a *App) stepsHere(workspaceID, userID, sessionID, delegationID, fleetID int64) func(stepID int64) {
	return func(stepID int64) {
		a.Bus.Publish(events.AgentStepChanged{
			WorkspaceID: workspaceID, UserID: userID, SessionID: sessionID,
			DelegationID: delegationID, StepID: stepID, FleetID: fleetID,
		})
	}
}

// agentReport is the news of a step, sent from a worker.
type agentReport struct {
	WorkspaceID  int64 `json:"workspace_id"`
	UserID       int64 `json:"user_id"`
	SessionID    int64 `json:"session_id"`
	DelegationID int64 `json:"delegation_id"`
	StepID       int64 `json:"step_id"`
	FleetID      int64 `json:"fleet_id,omitempty"`
}

// stepsFromWorker is how an agent running on a WORKER process tells of a
// step: over the queue, to the server holding the sockets. Best-effort, like the bus it
// ends up on: a lost report is a step that shows when the agent is opened
// again, never a step that is lost.
func (a *App) stepsFromWorker(workspaceID, userID, sessionID, delegationID, fleetID int64) func(stepID int64) {
	return func(stepID int64) {
		if a.Queue == nil {
			return
		}
		payload, err := json.Marshal(agentReport{
			WorkspaceID: workspaceID, UserID: userID, SessionID: sessionID,
			DelegationID: delegationID, StepID: stepID, FleetID: fleetID,
		})
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), agentReportTimeout)
		defer cancel()
		// Unique per telling. One step is told of several times (written, then
		// once per tool call that finishes), and the broker drops a message
		// whose id it has seen, which is for a retried publish and nothing else.
		id := fmt.Sprintf("agent-%d-%d-%d", delegationID, stepID, time.Now().UnixNano())
		if err := a.Queue.Enqueue(ctx, agentReportSubject, queue.Task{
			ID: id, Kind: model.JobKindAgentRun, WorkspaceID: workspaceID, Payload: payload,
		}); err != nil {
			a.Log.Warn().Err(err).Int64("delegation_id", delegationID).Msg("report an agent's step")
		}
	}
}

// WatchAgents starts the listener: it pushes what moved to whoever is
// watching, and hears the workers' reports. The server calls it once, like the
// live dashboard, and a worker never does.
//
// It marks this process first, before anything can run, and that mark is what
// decides how an agent tells of its steps (tellSteps): here, where the listener
// is, the bus; anywhere else, the queue. Deciding by process rather than by who
// started the agent matters in the desktop, where the worker loop runs inside
// this process: its agents are beside the listener, and the queue there is a
// channel that drops a message whenever its reader is busy.
func (a *App) WatchAgents(ctx context.Context) {
	a.watchingAgents.Store(true)
	go func() {
		a.Bus.Subscribe(events.KindAgentStepChanged, func(e events.Event) {
			if moved, ok := e.(events.AgentStepChanged); ok {
				a.pushAgentStep(ctx, moved)
			}
		})
		a.Bus.Subscribe(events.KindFleetChanged, func(e events.Event) {
			if moved, ok := e.(events.FleetChanged); ok {
				a.pushFleetRuns(ctx, moved)
			}
		})
	}()
	if a.Queue != nil {
		go a.readAgentReports(ctx)
	}
}

// tellSteps is how an agent running in this process tells of its steps: on the
// bus when the listener is here, over the queue when it is not. fleetID is the
// batch it is one of, zero for an agent on its own.
func (a *App) tellSteps(workspaceID, userID, sessionID, delegationID, fleetID int64) func(stepID int64) {
	if a.watchingAgents.Load() {
		return a.stepsHere(workspaceID, userID, sessionID, delegationID, fleetID)
	}
	return a.stepsFromWorker(workspaceID, userID, sessionID, delegationID, fleetID)
}

// readAgentReports puts what the workers report on the bus, in the order it
// arrives, so their agents reach the listener the same way this process's do.
func (a *App) readAgentReports(ctx context.Context) {
	sub, err := a.Queue.SubscribeAll(ctx, agentReportSubject, agentReportGroup)
	if err != nil {
		a.Log.Error().Err(err).Msg("subscribe to agent reports")
		return
	}
	defer func() { _ = sub.Close() }()
	for {
		select {
		case <-ctx.Done():
			return
		case task, ok := <-sub.Tasks():
			if !ok {
				return
			}
			var report agentReport
			if err := json.Unmarshal(task.Payload, &report); err != nil {
				continue
			}
			a.Bus.Publish(events.AgentStepChanged{
				WorkspaceID: report.WorkspaceID, UserID: report.UserID, SessionID: report.SessionID,
				DelegationID: report.DelegationID, StepID: report.StepID, FleetID: report.FleetID,
			})
		}
	}
}

// agentStepPush is a step pushed to somebody watching an agent.
type agentStepPush struct {
	Kind    string      `json:"kind"`
	Run     string      `json:"run"`
	Message ChatMessage `json:"message"`
}

// fleetPush is a batch pushed to somebody watching it: where every agent in it
// now stands.
type fleetPush struct {
	Kind  string     `json:"kind"`
	Fleet string     `json:"fleet"`
	Runs  []AgentRun `json:"runs"`
}

// pushAgentStep sends a step to the person watching its agent, and does no
// work at all when nobody is.
func (a *App) pushAgentStep(ctx context.Context, moved events.AgentStepChanged) {
	if a.WS == nil {
		return
	}
	if moved.FleetID != 0 {
		// A member's step is also what its batch has spent so far, written to
		// its row by the time the step is: whoever has the batch open sees the
		// totals move as the agents work, not only as each one finishes.
		a.pushFleetRuns(ctx, events.FleetChanged{
			WorkspaceID: moved.WorkspaceID, UserID: moved.UserID, FleetID: moved.FleetID,
		})
	}
	topic := AgentTopic(moved.DelegationID)
	if !a.WS.Watching(moved.WorkspaceID, moved.UserID, topic) {
		return
	}
	step, err := a.Store.Agent().Step(ctx, moved.SessionID, moved.StepID)
	if err != nil {
		return
	}
	message, ok := a.agentMessage(ctx, moved.WorkspaceID, step, a.MaySee(ctx, moved.UserID))
	if !ok {
		return
	}
	a.WS.BroadcastTo(moved.WorkspaceID, moved.UserID, topic, agentStepPush{
		Kind: "step", Run: strconv.FormatInt(moved.DelegationID, 10), Message: message,
	})
}

// pushFleetRuns sends a batch's agents to the person watching it.
func (a *App) pushFleetRuns(ctx context.Context, moved events.FleetChanged) {
	if a.WS == nil {
		return
	}
	topic := FleetTopic(moved.FleetID)
	if !a.WS.Watching(moved.WorkspaceID, moved.UserID, topic) {
		return
	}
	fleet, err := a.Store.Agent().Fleet(ctx, moved.FleetID)
	if err != nil {
		return
	}
	runs, err := a.FleetRuns(ctx, fleet)
	if err != nil {
		return
	}
	a.WS.BroadcastTo(moved.WorkspaceID, moved.UserID, topic, fleetPush{
		Kind: "fleet", Fleet: strconv.FormatInt(moved.FleetID, 10), Runs: runs,
	})
}
