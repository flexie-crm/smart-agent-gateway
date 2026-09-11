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

// Delegation: the Gateway hands a task to an agent that runs its own loop
// with its own model, tools, and prompt, and returns a result. It is a single
// level deep, the same shape the CRM proved (KB/02): the agent never gets
// the delegate tool, so it cannot delegate again.

// agentFriendlyName is what the Gateway's delegation call shows as, in the
// chip and on the parked-delegation stub alike.
const agentFriendlyName = "Delegate to an agent"

// fleetFriendlyName is what a whole batch shows as. One name for the call, not
// one per member: the chat draws one chip for a fleet.
const fleetFriendlyName = "Run agents in parallel"

// AgentProfile is what one agent runs as. The app resolves it from the
// agent's own configuration; the runner runs it.
type AgentProfile struct {
	Key          string
	Name         string
	SystemPrompt string
	Tools        tool.Loadout
	ModelID      int64
	Reasoning    bool
	// Settings are this agent's own chosen values, nearest in the layered order.
	Settings model.Settings
	// DelegationMode pins how the Gateway must run this agent (auto|background|
	// inline, KB/27). A pinned mode overrides whatever mode the Gateway passed.
	DelegationMode string
	// MaxIterations bounds this agent's own tool loop. Zero falls back to the
	// code default.
	MaxIterations int
	// BackgroundTimeout bounds one working leg of this agent when it runs in
	// the background (KB/27). Zero falls back to the code default. The app owns the
	// goroutine and enforces it as a deadline on the run's context.
	BackgroundTimeout time.Duration
}

// AgentResolver resolves an agent by key. An unknown or inactive key is
// an error the loop turns into an observation the Gateway can react to, never a
// crash.
type AgentResolver func(ctx context.Context, agentKey string) (AgentProfile, error)

// BackgroundDelegation is everything the app needs to run one background-mode
// delegation as an owned goroutine (Mode C, KB/27): the resolved agent, the
// task, and the identity of the Gateway turn that started it, so the agent
// and the eventual completion turn run in the same session, on the same model.
type BackgroundDelegation struct {
	DelegationID int64
	Sub          AgentProfile
	Task         string
	ParentCallID string
	WorkspaceID  int64
	UserID       int64
	SessionID    int64
	ModelID      int64
}

// derefID reads an optional row id, zero when there is none.
func derefID(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}

// ErrBackgroundParked reports that a background agent stopped to ask a
// person: it is suspended, not finished. The app leaves the delegation running
// and a resume re-enters it (KB/27); it must not be completed or narrated.
var ErrBackgroundParked = errors.New("agent: background agent parked for approval")

// delegateArgs is what the model passes to the delegate tool.
type delegateArgs struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
	Mode  string `json:"mode"`
}

// fleetArgs is what the model passes to the fleet tool: the whole batch, in one
// call. A fleet is a separate tool rather than a mode of this one because the
// arguments are a different shape, and a schema that says "either these two
// fields or that list" is a schema a model gets wrong.
type fleetArgs struct {
	Tasks []FleetTask `json:"tasks"`
}

// delegate runs one delegate tool call. It records the call against the row the
// step already created (like any tool), and returns the result the Gateway
// reads. terminal reports that the agent's answer is the turn's answer, so
// the Gateway says nothing more.
func (r *Runner) delegate(
	ctx context.Context,
	turn Turn,
	call *model.ToolCall,
	out *chat.Stream,
) (result tool.Result, terminalAnswer string, terminal bool, err error) {
	started := time.Now()
	call.FriendlyName = agentFriendlyName

	if turn.Agent == nil {
		res, ferr := r.toolFailure(ctx, turn, call, tool.Schema{}, "no agent is available to delegate to", out)
		return res, "", false, ferr
	}

	var args delegateArgs
	if jsonErr := json.Unmarshal(call.Args, &args); jsonErr != nil || args.Task == "" || args.Agent == "" {
		res, ferr := r.toolFailure(ctx, turn, call, tool.Schema{}, "the delegation needs an agent and a task", out)
		return res, "", false, ferr
	}
	sub, resolveErr := turn.Agent(ctx, args.Agent)
	if resolveErr != nil {
		r.log.Warn().Err(resolveErr).Str("agent", args.Agent).Int64("session_id", turn.SessionID).
			Msg("unknown agent")
		res, ferr := r.toolFailure(ctx, turn, call, tool.Schema{},
			fmt.Sprintf("there is no agent named %q", args.Agent), out)
		return res, "", false, ferr
	}

	// The agent's pinned mode wins over whatever the Gateway passed: an
	// administrator who set it was giving an instruction, not a default (KB/27).
	mode := ResolveHandoff(args.Mode, sub.DelegationMode)

	// One lookup, and the loop does not know which mode it got. Every branch on
	// the mode used to live out here; a fourth one meant finding all of them.
	h, ok := r.handoffs[mode]
	if !ok {
		res, ferr := r.toolFailure(ctx, turn, call, tool.Schema{},
			fmt.Sprintf("this assistant cannot run an agent that way (%q)", mode), out)
		return res, "", false, ferr
	}
	outcome, runErr := h.Run(ctx, Delegation{
		Turn: turn, Call: call, Sub: sub, Task: args.Task, Out: out, Started: started,
	})
	return outcome.Result, outcome.Answer, outcome.Terminal, runErr
}

// delegateFleetCall runs one fleet tool call. It is the second door into the
// same handoff: this one carries a list, the pinned-mode door carries one agent,
// and past this point neither is distinguishable from the other.
func (r *Runner) delegateFleetCall(
	ctx context.Context,
	turn Turn,
	call *model.ToolCall,
	out *chat.Stream,
) (result tool.Result, terminalAnswer string, terminal bool, err error) {
	started := time.Now()
	call.FriendlyName = fleetFriendlyName

	if turn.Agent == nil {
		res, ferr := r.toolFailure(ctx, turn, call, tool.Schema{}, "no agent is available to delegate to", out)
		return res, "", false, ferr
	}

	var args fleetArgs
	if jsonErr := json.Unmarshal(call.Args, &args); jsonErr != nil || len(args.Tasks) == 0 {
		res, ferr := r.toolFailure(ctx, turn, call, tool.Schema{}, "a fleet needs a list of agents and their tasks", out)
		return res, "", false, ferr
	}

	h, ok := r.handoffs[model.HandoffFleet]
	if !ok {
		res, ferr := r.toolFailure(ctx, turn, call, tool.Schema{}, "this assistant cannot run a fleet of agents", out)
		return res, "", false, ferr
	}
	outcome, runErr := h.Run(ctx, Delegation{
		Turn: turn, Call: call, Tasks: args.Tasks, Out: out, Started: started,
	})
	return outcome.Result, outcome.Answer, outcome.Terminal, runErr
}

// synchronousHandoff runs the agent HERE and waits: the Gateway reads its
// answer as the tool result (continue), or the answer is the turn's (terminal).
type synchronousHandoff struct {
	r    *Runner
	mode string
}

func (h synchronousHandoff) Mode() string { return h.mode }

func (h synchronousHandoff) Run(ctx context.Context, d Delegation) (Outcome, error) {
	result, answer, terminal, err := h.r.delegateInline(ctx, d, h.mode)
	return Outcome{Result: result, Answer: answer, Terminal: terminal}, err
}

// backgroundHandoff hands the agent to the app to run as a goroutine and
// resolves the Gateway's call at once with "started", so the turn ends (KB/27).
type backgroundHandoff struct{ r *Runner }

func (h backgroundHandoff) Mode() string { return model.HandoffBackground }

func (h backgroundHandoff) Run(ctx context.Context, d Delegation) (Outcome, error) {
	result, answer, terminal, err := h.r.delegateBackground(ctx, d.Turn, d.Call, d.Sub, d.Task, d.Started, d.Out)
	return Outcome{Result: result, Answer: answer, Terminal: terminal}, err
}

// delegateInline is the synchronous body, unchanged: it was the tail of
// delegate before the modes became things.
func (r *Runner) delegateInline(
	ctx context.Context,
	d Delegation,
	mode string,
) (result tool.Result, terminalAnswer string, terminal bool, err error) {
	turn, call, sub, out, started := d.Turn, d.Call, d.Sub, d.Out, d.Started

	if err := out.Write(chat.Frame{
		Type:    chat.FrameAgentStart,
		Message: chat.AgentMessage{Agent: sub.Key, Name: sub.Name, Mode: mode},
	}); err != nil {
		return tool.Result{}, "", false, err
	}
	answer, runErr := r.runAgent(ctx, turn, sub, d.Task, mode, call.ToolCallID, out)
	if errors.Is(runErr, errParked) {
		// The agent stopped to ask a person. The turn is parked and the card
		// is the final frame: the delegation is suspended, not finished, so no
		// agent_end and no result. It resumes back into this same delegation.
		return tool.Result{}, "", false, runErr
	}
	if endErr := out.Write(chat.Frame{Type: chat.FrameAgentEnd}); endErr != nil {
		return tool.Result{}, "", false, endErr
	}
	if runErr != nil {
		r.log.Error().Err(runErr).Str("agent", sub.Key).Int64("session_id", turn.SessionID).
			Msg("delegation failed")
		res, ferr := r.toolFailure(ctx, turn, call, tool.Schema{}, "the agent could not finish the task", out)
		return res, "", false, ferr
	}

	payload, _ := json.Marshal(map[string]any{"success": true, "agent": sub.Key, "result": answer})
	duration := time.Since(started)
	r.resolveCall(ctx, turn, call, model.ToolCallCompleted, payload, "", duration, false)
	if err := r.emitToolDone(out, call, model.ToolCallCompleted, payload, duration, ourOwnWiring); err != nil {
		return tool.Result{}, "", false, err
	}
	return tool.Result{Content: payload}, answer, mode == model.HandoffTerminal, nil
}

// delegateBackground records a background delegation and hands the agent to
// the app to run as a goroutine. It resolves the Gateway's `delegate` call right
// away with a "started" result, so the Gateway narrates that it has begun and the
// turn ends normally (KB/27); the real work is tracked on the delegation record.
func (r *Runner) delegateBackground(
	ctx context.Context,
	turn Turn,
	call *model.ToolCall,
	sub AgentProfile,
	task string,
	started time.Time,
	out *chat.Stream,
) (tool.Result, string, bool, error) {
	if turn.StartBackground == nil {
		// Nothing on this turn can own a background goroutine (a channel that does
		// not offer it). Refuse cleanly rather than half-start it.
		res, ferr := r.toolFailure(ctx, turn, call, tool.Schema{}, "background delegation is not available here", out)
		return res, "", false, ferr
	}

	del := &model.AgentDelegation{
		SessionID:        turn.SessionID,
		WorkspaceID:      turn.WorkspaceID,
		ParentToolCallID: call.ToolCallID,
		AgentKey:         sub.Key,
		Mode:             model.HandoffBackground,
		// The instruction, written down. It is the one thing a killed agent
		// cannot be started again without: everything else it needs is already
		// on this row, and the task used to live only in this goroutine's
		// arguments, so it died with the process.
		Task:   task,
		Status: model.DelegationRunning,
	}
	if err := r.store.Agent().CreateDelegation(ctx, del); err != nil {
		r.log.Error().Err(err).Int64("session_id", turn.SessionID).Msg("create background delegation")
		res, ferr := r.toolFailure(ctx, turn, call, tool.Schema{}, "the background task could not be started", out)
		return res, "", false, ferr
	}

	// Bracket the delegation so a client can raise a chip for it. A background
	// agent runs on its own stream, so nothing is nested here: the bracket
	// only announces that a background delegation began, carrying the mode so the
	// chip knows it is a long-running one, not an inline hand-off.
	if err := out.Write(chat.Frame{
		Type:    chat.FrameAgentStart,
		Message: chat.AgentMessage{Agent: sub.Key, Name: sub.Name, Mode: model.HandoffBackground},
	}); err != nil {
		return tool.Result{}, "", false, err
	}
	if err := out.Write(chat.Frame{Type: chat.FrameAgentEnd}); err != nil {
		return tool.Result{}, "", false, err
	}

	// The id is INTERNAL and the payload says so where the model will read it.
	// It used to arrive inside a quotable sentence ("... (running id 258)"), and
	// the Gateway did exactly what that invites: it told the person it was
	// "waiting on #257, #258, #261 and #262". Those numbers mean nothing to
	// anybody outside this process. The model still needs the id, to ask after
	// one agent or to stop it, so it is kept and labelled rather than removed,
	// and the model is told what to say INSTEAD: what the agent is doing.
	payload, _ := json.Marshal(map[string]any{
		"success":            true,
		"background":         true,
		"agent":              sub.Key,
		"internal_id":        del.ID,
		"internal_id_notice": "For your own use only. Never show this number to the person and never refer to an agent by it; say what the agent is doing.",
		"message": fmt.Sprintf("Started your %s agent in the background. It is now running on its own; when it finishes you are brought back automatically with its result recorded here.",
			sub.Name),
	})
	duration := time.Since(started)
	// Resolved BEFORE the goroutine is handed off, and the order matters. The
	// call is "started" the moment we have committed to starting it, and a
	// background agent can park for approval in its first few milliseconds: with
	// the goroutine launched first, there is a window where a card is already
	// waiting on a conversation whose transcript still says the delegation is
	// running, which is exactly the state a reload cannot make sense of.
	r.resolveCall(ctx, turn, call, model.ToolCallCompleted, payload, "", duration, false)
	if err := r.emitToolDone(out, call, model.ToolCallCompleted, payload, duration, ourOwnWiring); err != nil {
		return tool.Result{}, "", false, err
	}

	turn.StartBackground(ctx, BackgroundDelegation{
		DelegationID: del.ID,
		Sub:          sub,
		Task:         task,
		ParentCallID: call.ToolCallID,
		WorkspaceID:  turn.WorkspaceID,
		UserID:       turn.UserID,
		SessionID:    turn.SessionID,
		ModelID:      turn.ModelID,
	})
	return tool.Result{Content: payload}, "", false, nil
}

// RunBackgroundAgent runs a background delegation's agent to a
// technical result, off any chat stream. Its steps still persist to the
// transcript (attributed to the delegation), but its prose and reasoning go to a
// discard stream: an agent never streams to the person, and the Gateway
// narrates at completion. A park unwinds to ErrBackgroundParked, so the app can
// suspend the delegation rather than mistake it for a failure.
func (r *Runner) RunBackgroundAgent(ctx context.Context, bg BackgroundDelegation, out *chat.Stream) (string, error) {
	turn := Turn{
		WorkspaceID:  bg.WorkspaceID,
		UserID:       bg.UserID,
		SessionID:    bg.SessionID,
		ModelID:      bg.ModelID,
		AutoApprove:  r.sessionAutoApproves(ctx, bg.WorkspaceID, bg.SessionID),
		DelegationID: bg.DelegationID,
	}
	answer, err := r.runAgent(ctx, turn, bg.Sub, bg.Task, model.HandoffBackground, bg.ParentCallID, out)
	if errors.Is(err, errParked) {
		return "", ErrBackgroundParked
	}
	return answer, err
}

// BackgroundResume carries a person's answer to a card a background agent
// raised, so the app can re-enter that agent off the stream (KB/27).
type BackgroundResume struct {
	Snapshot    *model.ParkSnapshot
	Approved    bool
	Sub         AgentProfile
	WorkspaceID int64
	UserID      int64
	SessionID   int64
	ModelID     int64
}

// backgroundRejected is the result a background delegation ends with when the
// person declines the action it needed. The Gateway's completion turn reads it and
// tells the person the task could not be finished, rather than the chip hanging.
const backgroundRejected = "The person declined the action this task needed, so it could not be completed."

// ResumeBackgroundAgent continues a background agent from the card it
// raised. On approval it runs the validated tool and carries the agent on to
// a result; on refusal the delegation ends with a person-safe reason the Gateway
// narrates. A second approval-gated tool re-parks (ErrBackgroundParked). It runs
// off any chat stream, exactly as the first leg did, and never touches the
// Gateway's `delegate` call, which is already resolved "started".
func (r *Runner) ResumeBackgroundAgent(ctx context.Context, br BackgroundResume, out *chat.Stream) (string, error) {
	turn := Turn{
		WorkspaceID: br.WorkspaceID, UserID: br.UserID, SessionID: br.SessionID, ModelID: br.ModelID,
		AutoApprove: r.sessionAutoApproves(ctx, br.WorkspaceID, br.SessionID),
		Resume:      &Resume{Snapshot: br.Snapshot, Approved: br.Approved},
		// Carried through the resume, so a SECOND card this agent raises is
		// stamped with the same member as the first.
		DelegationID: derefID(br.Snapshot.DelegationID),
	}
	subByName := runnable(br.Sub.Tools)

	// Resolve the agent's model before the parked call runs: an approved
	// action can change the world, and the agent must continue on the model it
	// started on. A refusal does not run the agent, so a resolution error only
	// matters on approval.
	subResolved, resolveErr := r.resolveAgentModel(ctx, turn, br.Sub)

	// Finish the call the person answered, against the agent's own row and
	// handlers, writing its result into the transcript.
	if err := r.completeParkedTool(ctx, turn, br.Sub.Tools.Handlers, subByName, out); err != nil {
		return "", err
	}

	if !br.Approved {
		// The delegation ends here, as the synchronous reject does (option A): the
		// Gateway's completion turn reads this and tells the person it was declined.
		return backgroundRejected, nil
	}
	if resolveErr != nil {
		return "", resolveErr
	}

	messages, err := r.agentTranscript(ctx, turn, br.Sub, br.Snapshot.ParentToolCallID, br.Snapshot.HandoffMode)
	if err != nil {
		return "", err
	}
	answer, err := r.agentLoop(ctx, turn, br.Sub, subResolved, messages, br.Snapshot.ParentToolCallID, br.Snapshot.HandoffMode, out)
	if errors.Is(err, errParked) {
		return "", ErrBackgroundParked
	}
	return answer, err
}

// runCompletion is a server-initiated turn that delivers a finished background
// delegation to the Gateway. The Gateway's transcript already holds the `agent`
// call resolved as "started"; this appends the agent's result as a framed
// system event and runs the Gateway loop, which decides what happens next: narrate
// the answer, act on it with its own tools, or delegate again (KB/27).
func (r *Runner) runCompletion(
	ctx context.Context,
	turn Turn,
	resolved *provider.Resolved,
	handlers map[string]tool.Handler,
	tools []provider.ToolDef,
	byName map[string]tool.Schema,
	flag *memoryFlag,
	out *chat.Stream,
) (answer, []provider.Message, error) {
	del, err := r.store.Agent().GetDelegation(ctx, turn.CompletedDelegationID)
	if err != nil {
		r.log.Error().Err(err).Int64("delegation_id", turn.CompletedDelegationID).Msg("load completed delegation")
		return answer{}, nil, r.fail(ctx, turn, out, "A background task could not be delivered.")
	}

	// Persist the agent's real result ONTO the Gateway's own delegate tool
	// call, replacing the "started" placeholder, so the Gateway's transcript reads
	// call -> result and it has every finished agent's result in its own
	// context. This is the same write the synchronous resume path uses.
	r.resolveDelegationCall(ctx, turn, del)

	messages, err := r.buildTranscript(ctx, turn)
	if err != nil {
		r.log.Error().Err(err).Msg("build transcript")
		return answer{}, nil, r.fail(ctx, turn, out, "Your conversation could not be loaded.")
	}

	// A completion turn has no prompt of its own, so give the Gateway a minimal
	// wake-up: the bare fact that one agent finished. It carries NO result
	// (that is on the tool call above), no instruction, and nothing about other
	// agents. The Gateway decides its next move from its own roster.
	name := r.agentName(ctx, turn, del.AgentKey)
	messages = append(messages, provider.Message{
		Role:    provider.RoleUser,
		Content: completionWake(name, del.ID),
	})

	reply, err := r.loop(ctx, turn, resolved, messages, tools, handlers, byName, out, flag)
	return reply, messages, err
}

// resolveDelegationCall writes a finished background delegation's real result
// onto the Gateway's OWN delegate tool call (del.ParentToolCallID), replacing the
// "started" placeholder, so the Gateway's transcript carries the result the way a
// synchronous delegation does and it can combine straight from its own context.
func (r *Runner) resolveDelegationCall(ctx context.Context, turn Turn, del *model.AgentDelegation) {
	parent := &model.ToolCall{
		SessionID: turn.SessionID, WorkspaceID: turn.WorkspaceID,
		ToolCallID: del.ParentToolCallID, ToolName: model.DelegateToolName,
		FriendlyName: agentFriendlyName, RequestedApproval: true,
	}
	if del.Status == model.DelegationDone {
		result := delegationResult(del.AgentKey, resultText(del.Result), true)
		r.resolveCall(ctx, turn, parent, model.ToolCallCompleted, result, "", 0, true)
		return
	}
	errText := del.ErrorText
	if errText == "" {
		errText = "the background task could not be completed"
	}
	r.resolveCall(ctx, turn, parent, model.ToolCallFailed, delegationResult(del.AgentKey, "", false), errText, 0, true)
}

// CompletionWakeMarker is the bracketed lead-in every server-initiated completion
// turn carries. It is the one reliable signal that a turn is a completion (a
// background agent finishing), so anything that must recognize one keys off
// this constant rather than a copied literal that could silently drift.
const CompletionWakeMarker = "[A background agent finished]"

// completionWake is the bare fact a completion turn gives the Gateway: one of its
// background agents has finished. It states ONLY that, and NO more: no result
// (that is on the agent's own tool call now), no instruction on what to do
// next, and nothing about any other agent. An agent is one-way execution
// and never dictates the Gateway's next move; what to do with a finished agent
// is the Gateway's own decision, governed by its roster (combine once all it
// started have finished, read from this conversation, never poll).
func completionWake(name string, id int64) string {
	return fmt.Sprintf(
		CompletionWakeMarker+" Your %s agent (#%d) has finished. Its result is now recorded with it in the conversation above.",
		name, id)
}

// resultText pulls the agent's answer out of the delegation's stored
// result, which the app writes as {"result": "..."}. It falls back to the raw
// JSON so a differently shaped result is still shown rather than dropped.
func resultText(raw json.RawMessage) string {
	var payload struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && payload.Result != "" {
		return payload.Result
	}
	return string(raw)
}

// agentName is the agent's friendly name for the completion message,
// resolved live so the Gateway names it the way the person saw it. It falls back
// to the key if the agent was removed while the task ran.
func (r *Runner) agentName(ctx context.Context, turn Turn, key string) string {
	if turn.Agent != nil {
		if sub, err := turn.Agent(ctx, key); err == nil && sub.Name != "" {
			return sub.Name
		}
	}
	return key
}

// sessionAutoApproves reads, live, whether a conversation runs approval-gated
// tools without a card. A background agent must honour the same setting the
// person sees: if the session is auto-approving, the agent does not park.
// Read fresh (not snapshotted at delegation start), so flipping the setting takes
// effect on the next run leg, exactly like every other live re-check.
func (r *Runner) sessionAutoApproves(ctx context.Context, workspaceID, sessionID int64) bool {
	sess, err := r.store.Agent().GetSession(ctx, workspaceID, sessionID)
	if err != nil {
		return false
	}
	return sess.ApprovalMode == model.ApprovalAuto
}

// runAgent runs the agent's own tool loop to an answer, streaming what
// it produces so a person watching sees the work. In continue mode the frames
// are tagged as an agent's, so the client shows them apart and never mistakes
// the inner answer for the turn's; in terminal mode the agent's answer IS
// the turn's answer, so its frames read as the answer.
func (r *Runner) runAgent(
	ctx context.Context,
	turn Turn,
	sub AgentProfile,
	task string,
	mode string,
	parentCallID string,
	out *chat.Stream,
) (string, error) {
	resolved, err := r.resolveAgentModel(ctx, turn, sub)
	if err != nil {
		return "", err
	}

	// The task the Gateway handed over is the first turn of the agent's inner
	// conversation. It is persisted as a user step of this delegation so that a
	// resume rebuilds the whole inner conversation from the transcript, the park
	// snapshot carrying none of it (KB/16).
	if err := r.saveAgentStep(ctx, turn, &model.AgentStep{
		Kind: model.StepUser, Text: task,
	}, sub.Key, parentCallID); err != nil {
		return "", err
	}
	messages := []provider.Message{
		{Role: provider.RoleSystem, Content: agentSystem(sub.SystemPrompt, mode)},
		{Role: provider.RoleUser, Content: task},
	}
	return r.agentLoop(ctx, turn, sub, resolved, messages, parentCallID, mode, out)
}

// agentSystem wraps an agent's own prompt with the ROLE that matches
// the handoff, because the two modes have DIFFERENT audiences (CRM Agent):
//
//   - continue: the agent reports back to the GATEWAY (a system), not the
//     person, and its prose is not shown; it must return the COMPLETE technical
//     result (exact ids, values, errors) for the Gateway to narrate.
//   - terminal: the agent's answer IS the person-facing response, so it must
//     be clean business language under the same rules the Gateway follows.
func agentSystem(base, mode string) string {
	if mode == model.HandoffTerminal {
		return base + "\n\nROLE: your answer is shown directly to the person as the final response. " +
			"Work silently while your tools run and present ONLY the finished result, in plain business language. " +
			"Never expose raw ids, JSON, payloads, URLs, or internal steps; the same communication rules the main agent follows apply to you."
	}
	return base + "\n\nROLE: you are reporting back to the main agent (a system), NOT to the person, and your reply is NOT shown to them. " +
		"Return a concise but COMPLETE technical result the main agent can act on: the exact ids, names, field values, counts, links, and any errors, verbatim. " +
		"Do NOT translate to business language or omit detail; the main agent turns your result into the person-facing answer."
}

// resolveAgentModel resolves the model an agent runs on: its own, or the
// Gateway's when it pins none.
func (r *Runner) resolveAgentModel(ctx context.Context, turn Turn, sub AgentProfile) (*provider.Resolved, error) {
	modelID := sub.ModelID
	if modelID == 0 {
		modelID = turn.ModelID
	}
	resolved, err := r.gateway.Resolve(ctx, turn.WorkspaceID, modelID)
	if err != nil {
		return nil, fmt.Errorf("resolve agent model: %w", err)
	}
	return resolved, nil
}

// agentLoop runs the agent's tool loop from a set of messages to an
// answer, streaming and persisting each step. It is entered fresh for a new
// delegation, and re-entered with the conversation rebuilt from the transcript
// when a parked delegation resumes.
func (r *Runner) agentLoop(
	ctx context.Context,
	turn Turn,
	sub AgentProfile,
	resolved *provider.Resolved,
	messages []provider.Message,
	parentCallID string,
	mode string,
	out *chat.Stream,
) (string, error) {
	tagged := mode != model.HandoffTerminal
	tools := toolDefs(sub.Tools.Schemas)
	byName := runnable(sub.Tools)

	limit := sub.MaxIterations
	if limit <= 0 {
		limit = model.DefaultMaxIterations
	}
	for iteration := 0; iteration < limit; iteration++ {
		req := resolved.Prepare(provider.GenerateRequest{
			Messages:  messages,
			Tools:     tools,
			Reasoning: sub.Reasoning,
			Settings:  sub.Settings,
		})
		// An agent's step takes its own place on the conversation's timeline,
		// right after the Gateway's `delegate` call, so an interrupted delegation
		// can be rebuilt from the transcript.
		seq, err := r.store.Agent().NextSeq(ctx, turn.SessionID)
		if err != nil {
			return "", fmt.Errorf("agent next seq: %w", err)
		}
		step, unknown, err := r.streamAgentStep(ctx, turn, resolved, req, byName, tagged, seq, out)
		if err != nil {
			return "", err
		}
		// Attributed to this delegation and persisted BEFORE its tools run, the
		// same order the Gateway uses: the request the agent made is a fact
		// the moment it made it, and each call is resolved in place as it runs. A
		// step of nothing but hallucinated calls has no words and no real tool
		// rows, so it is not written down: as far as the transcript is concerned
		// it never happened.
		if step.HasText() || step.Reasoning != "" || len(step.ToolCalls) > 0 {
			if err := r.saveAgentStep(ctx, turn, step, sub.Key, parentCallID); err != nil {
				return "", err
			}
		}
		if len(step.ToolCalls) == 0 && len(unknown) == 0 {
			return step.Text, nil
		}
		messages = append(messages, assistantMessageWithUnknown(step, unknown))
		for _, u := range unknown {
			messages = append(messages, provider.Message{
				Role:       provider.RoleTool,
				ToolCallID: u.ID,
				Content:    unavailableToolNote(u.Name),
			})
		}
		for _, sc := range step.ToolCalls {
			// An agent's approval-gated tool stops the WHOLE turn and asks,
			// exactly as the Gateway's does. The park points back at this
			// delegation (agent, parent call, mode) so the resume re-enters
			// it rather than running the call in the Gateway's loop. errParked
			// unwinds through delegate and the Gateway loop to Run, which has
			// already closed the stream with the card.
			schema := byName[sc.ToolName]

			// Pre-park validation on the agent path too: a tool that can refuse
			// a call does so BEFORE any card (approval must equal success), so a write
			// that would fail is a mistake the agent corrects, not a card a
			// person answers. A tool with no validator passes straight through.
			schema, refusal := r.runValidator(ctx, turn, sc, schema, sub.Tools.Validators)
			if refusal != nil {
				r.resolveCall(ctx, turn, sc, model.ToolCallFailed, refusal.Content, resultErrorText(*refusal), 0, false)
				if err := r.emitDelegateToolDone(out, sc, byName, model.ToolCallFailed, refusal.Content, 0); err != nil {
					return "", err
				}
				messages = append(messages, provider.Message{
					Role: provider.RoleTool, ToolCallID: sc.ToolCallID, Content: string(refusal.Content),
				})
				continue
			}

			// A background agent decides to park many steps into a long run, so
			// the approval mode is re-read LIVE here, not snapshotted at run start:
			// if the person switched to auto-approve while it was working, the very
			// next tool runs without a card.
			autoApprove := turn.AutoApprove
			if !autoApprove && Detached(mode) {
				autoApprove = r.sessionAutoApproves(ctx, turn.WorkspaceID, turn.SessionID)
			}
			if schema.RequiresApproval && !autoApprove {
				if turn.noApprovals != "" {
					// There is no card to raise (one was just refused this turn,
					// or this run has nobody to ask). The agent gets a refusal
					// result and carries on to report back.
					res, err := r.refuseApproval(ctx, turn, sc, out)
					if err != nil {
						return "", err
					}
					messages = append(messages, provider.Message{
						Role: provider.RoleTool, ToolCallID: sc.ToolCallID, Content: string(res.Content),
					})
					continue
				}
				return "", r.park(ctx, turn, resolved, sc, schema, actionHash(sc.ToolName, sc.Args),
					&parkedDelegation{
						Key: sub.Key, ParentCallID: parentCallID, Mode: mode,
						Detached: Detached(mode), ID: turn.DelegationID,
					}, out)
			}
			content, err := r.runAgentTool(ctx, turn, sub, byName, sc, out)
			if err != nil {
				return "", err
			}
			messages = append(messages, provider.Message{
				Role: provider.RoleTool, ToolCallID: sc.ToolCallID, Content: content,
			})
		}
	}
	return "I was not able to finish this task.", nil
}

// resumeDelegation continues a delegation whose agent stopped to ask a
// person. It finishes the parked call with the agent's own handlers,
// rebuilds the agent's inner conversation from the transcript, and carries
// it on: on approval the agent's loop runs; on rejection it gets one
// no-tools pass to acknowledge and the delegation ends (option A). The Gateway's
// `delegate` row is then resolved with the outcome, and the handoff decides who
// has the last word: in terminal mode the agent's answer is the turn's; in
// continue mode the Gateway reads the result and speaks.
func (r *Runner) resumeDelegation(
	ctx context.Context,
	turn Turn,
	gatewayResolved *provider.Resolved,
	handlers map[string]tool.Handler,
	tools []provider.ToolDef,
	byName map[string]tool.Schema,
	flag *memoryFlag,
	out *chat.Stream,
) (answer, error) {
	snap := turn.Resume.Snapshot
	if turn.Agent == nil {
		return answer{}, r.fail(ctx, turn, out, "This delegation can no longer be resumed.")
	}
	sub, err := turn.Agent(ctx, snap.AgentKey)
	if err != nil {
		// The agent was removed or disabled while the card waited. There is
		// nothing to run; say so rather than leave a chip spinning.
		r.log.Warn().Err(err).Str("agent", snap.AgentKey).Int64("session_id", turn.SessionID).
			Msg("resumed delegation's agent is gone")
		return answer{}, r.fail(ctx, turn, out, "The agent that was working on this is no longer available.")
	}
	subByName := runnable(sub.Tools)

	// Resolve the agent's model up FRONT, before the parked call runs:
	// completing that call may change the world (an approved action can disable
	// the very model the agent runs on), and the agent must continue on
	// the model it started on, resolved while that was still true. A rejection
	// does not run the agent, so a resolution error only matters on approve.
	subResolved, resolveErr := r.resolveAgentModel(ctx, turn, sub)

	// Re-open the delegation on the resumed stream, so its frames are grouped as
	// the agent's again.
	if err := out.Write(chat.Frame{
		Type:    chat.FrameAgentStart,
		Message: chat.AgentMessage{Agent: sub.Key, Name: sub.Name, Mode: snap.HandoffMode},
	}); err != nil {
		return answer{}, err
	}

	// Finish the call the person just answered, against the agent's own row
	// and with the agent's own handlers (the tool is the agent's, not
	// the Gateway's), writing its result into the transcript.
	if err := r.completeParkedTool(ctx, turn, sub.Tools.Handlers, subByName, out); err != nil {
		return answer{}, err
	}

	if !turn.Resume.Approved {
		// The person refused the agent's action. End the delegation, then
		// hand control BACK to the Gateway to respond, rather than going silent:
		// it reads the {rejected} delegation result and acknowledges, takes a
		// different path, or asks how to proceed. The Gateway runs WITH its tools
		// (so DeepSeek does not spill raw protocol), but noApprovals stops it
		// from re-delegating the refused task into a new card, it must talk to the
		// person.
		if err := r.endRefusedDelegation(ctx, turn, sub.Key, snap.ParentToolCallID, out); err != nil {
			return answer{}, err
		}
		gatewayMessages, err := r.buildTranscript(ctx, turn)
		if err != nil {
			return answer{}, r.fail(ctx, turn, out, "Your conversation could not be loaded.")
		}
		turn.noApprovals = approvalsAfterRejection
		return r.loop(ctx, turn, gatewayResolved, gatewayMessages, tools, handlers, byName, out, flag)
	}

	// APPROVED: rebuild the agent's inner conversation from its durable
	// steps, now including the just-run call, and carry on where it stopped.
	if resolveErr != nil {
		return answer{}, r.fail(ctx, turn, out, "This assistant is not available right now.")
	}
	messages, err := r.agentTranscript(ctx, turn, sub, snap.ParentToolCallID, snap.HandoffMode)
	if err != nil {
		return answer{}, r.fail(ctx, turn, out, "Your conversation could not be loaded.")
	}
	subAnswer, err := r.agentLoop(ctx, turn, sub, subResolved, messages, snap.ParentToolCallID, snap.HandoffMode, out)
	if err != nil {
		// errParked (a second card inside the resumed delegation) unwinds to Run,
		// which leaves the turn open. Any other error is a real failure.
		return answer{}, err
	}

	// The delegation is over. Close it and resolve the Gateway's `delegate` row
	// with the agent's result, so the Gateway's transcript is whole.
	if err := out.Write(chat.Frame{Type: chat.FrameAgentEnd}); err != nil {
		return answer{}, err
	}
	result := delegationResult(sub.Key, subAnswer, true)
	parent := &model.ToolCall{
		SessionID: turn.SessionID, WorkspaceID: turn.WorkspaceID,
		ToolCallID: snap.ParentToolCallID, ToolName: model.DelegateToolName,
		FriendlyName: agentFriendlyName, RequestedApproval: true,
	}
	r.resolveCall(ctx, turn, parent, model.ToolCallCompleted, result, "", 0, true)
	if err := r.emitToolDone(out, parent, model.ToolCallCompleted, result, 0, ourOwnWiring); err != nil {
		return answer{}, err
	}

	if snap.HandoffMode == model.HandoffTerminal {
		// The agent's answer is the turn's answer; the Gateway adds nothing.
		return answer{Content: subAnswer}, nil
	}
	// Continue mode: the Gateway reads the delegation's result (now on its row)
	// and speaks. Its transcript rebuilds with the delegate call resolved.
	gatewayMessages, err := r.buildTranscript(ctx, turn)
	if err != nil {
		return answer{}, r.fail(ctx, turn, out, "Your conversation could not be loaded.")
	}
	return r.loop(ctx, turn, gatewayResolved, gatewayMessages, tools, handlers, byName, out, flag)
}

// endRefusedDelegation closes a delegation the person refused: it marks the
// Gateway's `delegate` row rejected (so the chip shows a refusal, not a spinner)
// and ends the bracket. Nothing else runs, and the turn stops.
func (r *Runner) endRefusedDelegation(ctx context.Context, turn Turn, subKey, parentCallID string, out *chat.Stream) error {
	if err := out.Write(chat.Frame{Type: chat.FrameAgentEnd}); err != nil {
		return err
	}
	// The delegation itself ran and returned a result (that the person declined
	// its action), so its chip is COMPLETED, not a red rejection: the person
	// refused the agent's tool, not the act of delegating. The Gateway reads
	// this {rejected} result and responds.
	rejected := delegationResult(subKey, "", false)
	parent := &model.ToolCall{
		SessionID: turn.SessionID, WorkspaceID: turn.WorkspaceID,
		ToolCallID: parentCallID, ToolName: model.DelegateToolName,
		FriendlyName: agentFriendlyName, RequestedApproval: true,
	}
	r.resolveCall(ctx, turn, parent, model.ToolCallCompleted, rejected, "", 0, true)
	return r.emitToolDone(out, parent, model.ToolCallCompleted, rejected, 0, ourOwnWiring)
}

// agentTranscript rebuilds one delegation's inner conversation from the
// durable transcript: the agent's own system prompt, then its steps, in
// order, including the call the resume just resolved.
func (r *Runner) agentTranscript(ctx context.Context, turn Turn, sub AgentProfile, parentCallID, mode string) ([]provider.Message, error) {
	steps, err := r.store.Agent().Transcript(ctx, turn.SessionID)
	if err != nil {
		return nil, err
	}
	inner := steps[:0:0]
	for _, s := range steps {
		if s.AgentKey == sub.Key && s.ParentToolCallID == parentCallID {
			inner = append(inner, s)
		}
	}
	return conversation(agentSystem(sub.SystemPrompt, mode), inner, ""), nil
}

// delegationResult is what the Gateway reads back from a delegation: the
// agent's answer, and whether the person let it finish. A rejection is
// success:false with rejected:true, so the Gateway knows why it stopped and can
// take another path rather than assume the work is done.
func delegationResult(agentKey, result string, approved bool) json.RawMessage {
	payload := map[string]any{"success": approved, "agent": agentKey, "result": result}
	if !approved {
		payload["rejected"] = true
	}
	b, _ := json.Marshal(payload)
	return b
}

// saveAgentStep attributes a step to a delegation and persists it. It
// allocates a place on the conversation's timeline unless the caller already
// did (the assistant step needs its seq before it can stream).
func (r *Runner) saveAgentStep(ctx context.Context, turn Turn, step *model.AgentStep, key, parent string) error {
	if step.Seq == 0 {
		seq, err := r.store.Agent().NextSeq(ctx, turn.SessionID)
		if err != nil {
			return fmt.Errorf("agent step seq: %w", err)
		}
		step.Seq = seq
	}
	step.SessionID = turn.SessionID
	step.AgentKey = key
	step.ParentToolCallID = parent
	if err := r.store.Agent().SaveStep(ctx, step); err != nil {
		return fmt.Errorf("save agent step: %w", err)
	}
	return nil
}

// streamAgentStep streams one model call of an agent. Unlike the
// Gateway's streamStep it does not persist: an agent's inner steps are
// ephemeral in this first version, the Gateway's transcript keeps only the
// delegation and its result.
func (r *Runner) streamAgentStep(
	ctx context.Context,
	turn Turn,
	resolved *provider.Resolved,
	req provider.GenerateRequest,
	byName map[string]tool.Schema,
	tagged bool,
	seq int,
	out *chat.Stream,
) (*model.AgentStep, []provider.ToolCall, error) {
	started := time.Now()
	events, err := resolved.Provider.Stream(ctx, req)
	if err != nil {
		r.recordModelFailure(ctx, turn, resolved, started, err)
		return nil, nil, fmt.Errorf("stream agent: %w", err)
	}

	buf := &stepBuffer{}
	announced := map[string]bool{}
	// Tools the agent made up: names it was never handed. Like the Gateway's,
	// they never chip and never persist; the loop hands the model a "no such tool".
	var unknown []provider.ToolCall
	for event := range events {
		switch event.Kind {
		case provider.EventContentDelta:
			// An agent NEVER streams prose live, in EITHER mode (CRM Agent
			// handler). Its per-token text can carry raw ids and JSON that must not
			// reach the chat. In continue mode the Gateway narrates the answer; in
			// terminal mode the agent's finished answer is delivered as the
			// turn's result (Run's tail), not streamed here. Only the agent's
			// thinking and its tool chips flow live. The text is still accumulated,
			// because that is what the Gateway reads (continue) or what becomes the
			// result (terminal).
			buf.addContent(event.ContentDelta)
		case provider.EventReasoningDelta:
			buf.addReasoning(event.ReasoningDelta)
			if err := out.Write(chat.Frame{Type: chat.FrameReasoningDelta, Agent: tagged, Message: event.ReasoningDelta}); err != nil {
				return nil, nil, err
			}
		case provider.EventToolCall:
			if announced[event.ToolCall.ID] {
				break // one call id is handled once
			}
			announced[event.ToolCall.ID] = true
			// A tool the agent was not given is a hallucination, not an
			// action. Collect it for the correction and move on: no chip, no row.
			if _, ok := byName[event.ToolCall.Name]; !ok {
				unknown = append(unknown, *event.ToolCall)
				break
			}
			buf.addToolCall(*event.ToolCall)
			// The preparing chip carries the friendly name and narration, the
			// same as the Gateway's does. Without it the chip showed the raw
			// tool alias while running (e.g. "http_request") and only switched
			// to the friendly name once the call finished.
			if err := out.Write(chat.Frame{
				Type:  chat.FrameToolPreparing,
				Agent: true,
				Message: chat.ToolMessage{
					Name:         event.ToolCall.Name,
					FriendlyName: friendlyName(byName, event.ToolCall.Name),
					Narration:    narration(byName, event.ToolCall.Name),
				},
			}); err != nil {
				return nil, nil, err
			}
		case provider.EventUsage:
			if event.Usage != nil {
				r.recordModelCall(ctx, turn, resolved, started, *event.Usage)
				// The exact, vendor-reported spend of this one model call. A
				// background agent's chip accumulates it into a running
				// total; on the Gateway's stream a client ignores it. Emitted
				// once per model call, which is the finest granularity an exact
				// count exists at (vendors report usage only at a call's end).
				if err := out.Write(chat.Frame{
					Type:  chat.FrameUsage,
					Agent: tagged,
					Message: chat.UsageMessage{
						InputTokens:  event.Usage.InputTokens,
						OutputTokens: event.Usage.OutputTokens,
						Cost:         resolved.Model.CallCost(event.Usage.InputTokens, event.Usage.OutputTokens),
					},
				}); err != nil {
					return nil, nil, err
				}
			}
		case provider.EventError:
			r.recordModelFailure(ctx, turn, resolved, started, event.Err)
			return nil, nil, fmt.Errorf("agent stream: %w", event.Err)
		case provider.EventDone:
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return buf.step(turn, seq, resolved, byName, false), unknown, nil
}

// runAgentTool runs one of an agent's tools. An agent's loadout
// never contains an approval-gated tool (the app strips them for a delegation),
// so there is nothing to park here.
func (r *Runner) runAgentTool(
	ctx context.Context,
	turn Turn,
	sub AgentProfile,
	byName map[string]tool.Schema,
	call *model.ToolCall,
	out *chat.Stream,
) (string, error) {
	_, known := byName[call.ToolName]
	handler, ok := sub.Tools.Handlers[call.ToolName]
	if !known || !ok {
		fail := json.RawMessage(`{"success":false,"error":"unknown tool"}`)
		r.resolveCall(ctx, turn, call, model.ToolCallFailed, fail, "unknown tool", 0, false)
		return string(fail), r.emitDelegateToolDone(out, call, byName, model.ToolCallFailed, fail, 0)
	}

	started := time.Now()
	result, err := runWithHeartbeat(ctx, out, func() (tool.Result, error) {
		return handler(ctx, tool.Call{
			WorkspaceID: turn.WorkspaceID,
			UserID:      turn.UserID,
			SessionID:   turn.SessionID,
			Name:        call.ToolName,
			Args:        call.Args,
		})
	})
	if err != nil {
		r.log.Error().Err(err).Str("tool", call.ToolName).Str("agent", sub.Key).
			Int64("session_id", turn.SessionID).Msg("delegate tool failed")
		fail := json.RawMessage(`{"success":false,"error":"the tool failed"}`)
		r.resolveCall(ctx, turn, call, model.ToolCallFailed, fail, "the tool failed", time.Since(started), false)
		return string(fail), r.emitDelegateToolDone(out, call, byName, model.ToolCallFailed, fail, time.Since(started))
	}

	// The tool reports its own outcome through the result, exactly as it does for
	// the Gateway (a failed result is recorded failed), so the agent's steps
	// tell the same truth the Gateway's do.
	status, errText := model.ToolCallCompleted, ""
	if result.Failed() {
		status, errText = model.ToolCallFailed, resultErrorText(result)
	}
	duration := time.Since(started)
	r.resolveCall(ctx, turn, call, status, result.Content, errText, duration, false)
	return string(result.Content), r.emitDelegateToolDone(out, call, byName, status, result.Content, duration)
}

// emitDelegateToolDone streams an agent's finished tool call, tagged so a
// client shows it apart from the Gateway's work.
//
// It carries the id the row is opened by, under the same rule the Gateway's own
// calls go out with. An agent's call is a real tool doing real work for the
// person, it is persisted like any other, and `/chat/tool-call` already serves
// it: withholding the id here made the one row somebody most wants to read (what
// did the agent actually run?) the one row that could not be opened.
func (r *Runner) emitDelegateToolDone(
	out *chat.Stream,
	call *model.ToolCall,
	byName map[string]tool.Schema,
	status string,
	result json.RawMessage,
	duration time.Duration,
) error {
	return out.Write(chat.Frame{
		Type:  chat.FrameTool,
		Agent: true,
		Message: chat.ToolMessage{
			Name:         call.ToolName,
			FriendlyName: friendlyName(byName, call.ToolName),
			Done:         true,
			Status:       status,
			DurationMS:   duration.Milliseconds(),
			Input:        call.Args,
			Output:       result,
			ID:           r.openableID(byName[call.ToolName], call),
		},
	})
}
