package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/tool"
)

// Mode B: many agents at once, on the workers, answered once.
//
// It is background's shape with a join. The Gateway asks for several things
// together, the call resolves at once with "started" and the turn ends; each
// task is run by whichever worker picks it up; and when the last of them is
// back the Gateway is brought round again with all of the results, exactly as a
// single background agent brings it round with one.
//
// What it is NOT is a loop over background delegations. The difference is the
// join: the Gateway is told once, when the fleet is complete, rather than N
// times as each finishes. That is the whole reason the fleet row exists.

// FleetRequest is what the loop hands the app to actually dispatch. The app
// owns the queue; this package owns deciding that a fleet should happen.
type FleetRequest struct {
	FleetID int64
	// Deadline is when this batch stops waiting: the longest of its members'
	// own configured time limits. Passed rather than looked up, because it was
	// just computed and the app should not have to read back what it was told.
	Deadline     time.Time
	WorkspaceID  int64
	UserID       int64
	SessionID    int64
	ModelID      int64
	ParentCallID string
	Members      []FleetMember
}

// FleetMember is one agent and the one thing it was asked to do.
type FleetMember struct {
	DelegationID int64
	Sub          AgentProfile
	Task         string
}

// fleetHandoff dispatches a batch and ends the turn.
type fleetHandoff struct{ r *Runner }

func (h fleetHandoff) Mode() string { return model.HandoffFleet }

func (h fleetHandoff) Run(ctx context.Context, d Delegation) (Outcome, error) {
	res, answer, terminal, err := h.r.delegateFleet(ctx, d)
	return Outcome{Result: res, Answer: answer, Terminal: terminal}, err
}

// maxFleetSize is how many agents this turn's Gateway may start at once: what
// its administrator set, or the code default when they set nothing.
//
// The number is not a fact about the software. Twenty agents is twenty model
// conversations at once, which is a bill and a rate limit as much as it is
// work, and what that is worth differs by deployment and by vendor. So it is a
// setting, and this is only where the setting meets the default.
func maxFleetSize(turn Turn) int {
	if turn.MaxFleetAgents > 0 {
		return turn.MaxFleetAgents
	}
	return model.DefaultMaxFleetAgents
}

func (r *Runner) delegateFleet(ctx context.Context, d Delegation) (tool.Result, string, bool, error) {
	turn, call, out, started := d.Turn, d.Call, d.Out, d.Started

	if turn.StartFleet == nil {
		// Nothing on this turn can dispatch a fleet (a channel that does not
		// offer it, or a build with no worker). Refuse cleanly rather than
		// half-start one.
		res, ferr := r.toolFailure(ctx, turn, call, tool.Schema{}, "running a fleet of agents is not available here", out)
		return res, "", false, ferr
	}

	members, refusal := r.fleetMembers(ctx, d)
	if refusal != "" {
		res, ferr := r.toolFailure(ctx, turn, call, tool.Schema{}, refusal, out)
		return res, "", false, ferr
	}

	fleet := &model.AgentFleet{
		SessionID:        turn.SessionID,
		WorkspaceID:      turn.WorkspaceID,
		ParentToolCallID: call.ToolCallID,
		// The model this Gateway is on, written down, so the completion turn
		// runs on the same one even if this process is gone by then.
		ModelID: turn.ModelID,
		Size:    len(members),
		// How long the batch may work: the longest of what its own members were
		// configured to be allowed. Computed here, where the agents are resolved
		// and their settings are in hand, rather than guessed at by the sweep.
		Deadline: time.Now().UTC().Add(model.FleetDeadline(memberTimeouts(members))),
	}
	if err := r.store.Agent().CreateFleet(ctx, fleet); err != nil {
		r.log.Error().Err(err).Int64("session_id", turn.SessionID).Msg("create fleet")
		res, ferr := r.toolFailure(ctx, turn, call, tool.Schema{}, "the agents could not be started", out)
		return res, "", false, ferr
	}

	// A member is an ordinary delegation carrying the fleet's id. That is what
	// gives it the chip, the history, the cancel path and the boot recovery
	// without any of them learning what a fleet is.
	//
	// Written in ONE statement, however many there are: a thousand agents is a
	// thousand rows, and a round trip each would make starting a batch slower
	// than running it.
	rows := make([]*model.AgentDelegation, len(members))
	for i := range members {
		rows[i] = &model.AgentDelegation{
			SessionID:        turn.SessionID,
			WorkspaceID:      turn.WorkspaceID,
			ParentToolCallID: call.ToolCallID,
			AgentKey:         members[i].Sub.Key,
			Mode:             model.HandoffFleet,
			Task:             members[i].Task,
			Status:           model.DelegationRunning,
		}
	}
	if err := r.store.Agent().CreateFleetMembers(ctx, fleet.ID, rows); err != nil {
		r.log.Error().Err(err).Int64("fleet_id", fleet.ID).Msg("create fleet members")
		res, ferr := r.toolFailure(ctx, turn, call, tool.Schema{}, "the agents could not be started", out)
		return res, "", false, ferr
	}
	for i := range members {
		members[i].DelegationID = rows[i].ID
	}

	// One bracket for the whole fleet, not one per member: the chat draws ONE
	// chip for a fleet, denser rather than repeated, and twenty brackets would
	// be twenty rows saying almost the same thing.
	if err := out.Write(chat.Frame{
		Type: chat.FrameAgentStart,
		Message: chat.AgentMessage{
			Agent: members[0].Sub.Key,
			Name:  fleetName(members),
			Mode:  model.HandoffFleet,
		},
	}); err != nil {
		return tool.Result{}, "", false, err
	}
	if err := out.Write(chat.Frame{Type: chat.FrameAgentEnd}); err != nil {
		return tool.Result{}, "", false, err
	}

	// The same shape as a background start, and the same warning about the id:
	// it is ours, and a model that quotes it tells somebody a number that means
	// nothing outside this process.
	payload, _ := json.Marshal(map[string]any{
		"success":           true,
		"fleet":             true,
		"agents":            len(members),
		"internal_fleet_id": fleet.ID,
		"internal_id_notice": "For your own use only. Never show this number to the person; say what the agents are doing. " +
			"It names the BATCH, not one agent: stopping them all is done with this number as the fleet id.",
		"message": fmt.Sprintf("Started %d agents on their tasks. They run in parallel; when the last one finishes you are brought back automatically with all of their results.",
			len(members)),
	})
	duration := time.Since(started)
	// Resolved BEFORE anything is dispatched, for the same reason the background
	// path is: the members run in other processes, and a fast one can be back
	// and reported before this returns. The completion turn then writes the
	// results onto a call this was about to overwrite with "started".
	r.resolveCall(ctx, turn, call, model.ToolCallCompleted, payload, "", duration, false)
	if err := r.emitToolDone(out, call, model.ToolCallCompleted, payload, duration, ourOwnWiring); err != nil {
		return tool.Result{}, "", false, err
	}

	turn.StartFleet(ctx, FleetRequest{
		FleetID:      fleet.ID,
		Deadline:     fleet.Deadline,
		WorkspaceID:  turn.WorkspaceID,
		UserID:       turn.UserID,
		SessionID:    turn.SessionID,
		ModelID:      turn.ModelID,
		ParentCallID: call.ToolCallID,
		Members:      members,
	})
	return tool.Result{Content: payload}, "", false, nil
}

// memberTimeouts is what each agent in the batch was configured to be allowed.
func memberTimeouts(members []FleetMember) []time.Duration {
	out := make([]time.Duration, len(members))
	for i, m := range members {
		out[i] = m.Sub.BackgroundTimeout
	}
	return out
}

// fleetMembers turns what the model asked for into resolved agents, or says in
// one sentence why there is no fleet. The sentence is what the Gateway reads,
// so it is written for a reader who has to decide what to do next.
//
// Everything is resolved BEFORE anything is written. A fleet that creates
// nineteen rows and then discovers the twentieth agent does not exist is a fleet
// somebody has to clean up; refusing the whole call costs the Gateway one retry
// and leaves nothing behind.
func (r *Runner) fleetMembers(ctx context.Context, d Delegation) ([]FleetMember, string) {
	tasks := d.Tasks
	if len(tasks) == 0 {
		// A single agent reached here through a pinned mode rather than the
		// fleet tool. It is a fleet of one, and it costs nothing to allow.
		if d.Task == "" || d.Sub.Key == "" {
			return nil, "the fleet needs at least one agent and task"
		}
		return []FleetMember{{Sub: d.Sub, Task: d.Task}}, ""
	}
	if limit := maxFleetSize(d.Turn); len(tasks) > limit {
		return nil, fmt.Sprintf("a fleet runs at most %d agents at once; ask for fewer, or in rounds", limit)
	}

	members := make([]FleetMember, 0, len(tasks))
	for _, t := range tasks {
		if t.Agent == "" || t.Task == "" {
			return nil, "every task in a fleet needs an agent and a task"
		}
		sub, err := d.Turn.Agent(ctx, t.Agent)
		if err != nil {
			r.log.Warn().Err(err).Str("agent", t.Agent).Int64("session_id", d.Turn.SessionID).
				Msg("unknown agent in fleet")
			return nil, fmt.Sprintf("there is no agent named %q", t.Agent)
		}
		// An agent an administrator pinned to a synchronous mode was given an
		// instruction to run where the Gateway waits for it, and a fleet member
		// runs somewhere the Gateway cannot wait. Say so rather than silently
		// running it the other way.
		if !Detached(ResolveHandoff(model.HandoffFleet, sub.DelegationMode)) {
			return nil, fmt.Sprintf("%q cannot run in a fleet; delegate to it on its own", t.Agent)
		}
		members = append(members, FleetMember{Sub: sub, Task: t.Task})
	}
	return members, ""
}

// fleetName is what the chip is called. One kind of agent reads as that agent;
// a mixed fleet reads as a fleet, because listing five names in a chip is a
// list nobody needs while it is still running.
func fleetName(members []FleetMember) string {
	first := members[0].Sub.Name
	for _, m := range members[1:] {
		if m.Sub.Name != first {
			return fmt.Sprintf("%d agents", len(members))
		}
	}
	if len(members) == 1 {
		return first
	}
	return fmt.Sprintf("%d × %s", len(members), first)
}

// RunFleetMember runs one member of a fleet, on a worker.
//
// It is RunBackgroundAgent with one difference: the mode, so the transcript and
// the chip say what this is.
//
// It CAN stop to ask. A member reaching an approval-gated tool parks exactly as
// a background agent does (ErrBackgroundParked: suspended, not finished), and
// the card is a durable row, which is why a worker with no socket can raise one.
// An agent in a batch may reach for a great many tools and some of them are
// dangerous; refusing them all was a way of avoiding the question rather than
// answering it. The person answers one card at a time, and "approve, and stop
// asking" is there for when they have seen enough.
func (r *Runner) RunFleetMember(ctx context.Context, bg BackgroundDelegation, out *chat.Stream) (string, error) {
	turn := Turn{
		WorkspaceID:  bg.WorkspaceID,
		UserID:       bg.UserID,
		SessionID:    bg.SessionID,
		ModelID:      bg.ModelID,
		AutoApprove:  r.sessionAutoApproves(ctx, bg.WorkspaceID, bg.SessionID),
		DelegationID: bg.DelegationID,
	}
	answer, err := r.runAgent(ctx, turn, bg.Sub, bg.Task, model.HandoffFleet, bg.ParentCallID, out)
	if errors.Is(err, errParked) {
		return "", ErrBackgroundParked
	}
	return answer, err
}

// ResumeFleetMember continues a member from the card the person answered. It is
// ResumeBackgroundAgent unchanged, which is the point: the resume reads the
// mode off the snapshot, so a fleet member and a background agent come back the
// same way. What differs is WHERE it runs, and that is the app's problem.
func (r *Runner) ResumeFleetMember(ctx context.Context, br BackgroundResume, out *chat.Stream) (string, error) {
	return r.ResumeBackgroundAgent(ctx, br, out)
}

// runFleetCompletion is the join: the Gateway is brought back once, with every
// agent's result on the single call it made.
func (r *Runner) runFleetCompletion(
	ctx context.Context,
	turn Turn,
	resolved *provider.Resolved,
	handlers map[string]tool.Handler,
	tools []provider.ToolDef,
	byName map[string]tool.Schema,
	flag *memoryFlag,
	out *chat.Stream,
) (answer, []provider.Message, error) {
	fleet, err := r.store.Agent().Fleet(ctx, turn.CompletedFleetID)
	if err != nil {
		r.log.Error().Err(err).Int64("fleet_id", turn.CompletedFleetID).Msg("load completed fleet")
		return answer{}, nil, r.fail(ctx, turn, out, "A batch of background tasks could not be delivered.")
	}
	members, err := r.store.Agent().FleetMembers(ctx, fleet.ID)
	if err != nil || len(members) == 0 {
		r.log.Error().Err(err).Int64("fleet_id", fleet.ID).Msg("load fleet members")
		return answer{}, nil, r.fail(ctx, turn, out, "A batch of background tasks could not be delivered.")
	}

	// Every result onto the ONE call the Gateway made, replacing the "started"
	// placeholder, so its own transcript reads call -> results and it can work
	// from its own context without asking anything.
	r.resolveFleetCall(ctx, turn, fleet, members)

	messages, err := r.buildTranscript(ctx, turn)
	if err != nil {
		r.log.Error().Err(err).Msg("build transcript")
		return answer{}, nil, r.fail(ctx, turn, out, "Your conversation could not be loaded.")
	}

	// The wake-up carries the bare fact and nothing else, exactly as the single
	// completion does: no results (they are on the call above), no instruction.
	messages = append(messages, provider.Message{
		Role:    provider.RoleUser,
		Content: fleetWake(len(members)),
	})

	reply, err := r.loop(ctx, turn, resolved, messages, tools, handlers, byName, out, flag)
	return reply, messages, err
}

// resolveFleetCall writes every member's outcome onto the Gateway's own fleet
// tool call, in the order they were asked for.
//
// A fleet is not all-or-nothing: three of five succeeding is three results and
// two stated failures, which is more use to the Gateway than one word saying it
// went wrong. It fails outright only when NOTHING came back.
func (r *Runner) resolveFleetCall(ctx context.Context, turn Turn, fleet *model.AgentFleet, members []*model.AgentDelegation) {
	parent := &model.ToolCall{
		SessionID: turn.SessionID, WorkspaceID: turn.WorkspaceID,
		ToolCallID: fleet.ParentToolCallID, ToolName: model.FleetToolName,
		FriendlyName: fleetFriendlyName, RequestedApproval: true,
	}

	results := make([]map[string]any, 0, len(members))
	succeeded := 0
	for _, m := range members {
		entry := map[string]any{
			"agent":   m.AgentKey,
			"task":    m.Task,
			"success": m.Status == model.DelegationDone,
		}
		if m.Status == model.DelegationDone {
			entry["result"] = resultText(m.Result)
			succeeded++
		} else {
			reason := m.ErrorText
			if reason == "" {
				reason = "the agent did not return a result"
			}
			entry["error"] = reason
		}
		results = append(results, entry)
	}

	payload, _ := json.Marshal(map[string]any{
		"success":   succeeded > 0,
		"agents":    len(members),
		"succeeded": succeeded,
		"results":   results,
	})
	if succeeded == 0 {
		r.resolveCall(ctx, turn, parent, model.ToolCallFailed, payload,
			"none of the agents returned a result", 0, true)
		return
	}
	r.resolveCall(ctx, turn, parent, model.ToolCallCompleted, payload, "", 0, true)
}

// FleetWakeMarker is the lead-in a fleet's completion turn carries, the
// counterpart of CompletionWakeMarker for a whole batch. Anything that has to
// recognize one keys off this constant rather than a copied literal.
const FleetWakeMarker = "[A batch of agents finished]"

// fleetWake states the one fact and no more: they are all back. What to do with
// the results is the Gateway's decision, read from its own transcript, exactly
// as it is for a single agent.
func fleetWake(n int) string {
	return fmt.Sprintf(
		FleetWakeMarker+" All %d agents you started together have finished. "+
			"Their results are recorded with the call in the conversation above.", n)
}
