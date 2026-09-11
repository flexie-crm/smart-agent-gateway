package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/tool"
)

// executeTool runs one tool the model asked for, or parks the turn if the tool
// needs a human first.
//
// The call already has a row: the step that requested it was committed before
// anything ran. So a tool does not create a record here, it RESOLVES one, which
// is why an interrupted turn leaves behind a call that says what it was going
// to do rather than nothing at all.
func (r *Runner) executeTool(
	ctx context.Context,
	turn Turn,
	resolved *provider.Resolved,
	call *model.ToolCall,
	handlers map[string]tool.Handler,
	byName map[string]tool.Schema,
	approved map[string]bool,
	out *chat.Stream,
	flag *memoryFlag,
) (tool.Result, error) {
	schema, known := byName[call.ToolName]
	handler, hasHandler := handlers[call.ToolName]
	if !known || !hasHandler {
		// The model invented a tool. Tell it so, in a form it can recover
		// from: this is a normal tool result, not a failed turn.
		return r.toolFailure(ctx, turn, call, schema, "unknown tool", out)
	}
	// FriendlyName was set when the call's row was created (from this same tool),
	// so it is already on the call here.

	// Pre-park validation: a tool that can refuse a call does so BEFORE any card,
	// so a person is never asked to approve something that would then fail
	// (approval must equal success, KB/02). A tool with no validator passes
	// straight through, and its handler validates its own arguments when it runs.
	schema, refusal := r.runValidator(ctx, turn, call, schema, turn.Tools.Validators)
	if refusal != nil {
		return r.finishToolResult(ctx, turn, call, schema, *refusal, 0, false, flag, out)
	}

	// A tool that needs approval stops the turn here, unless this very action
	// was already approved (the resumed turn replaying it), the conversation is
	// auto-approving, or a card has just been refused this turn.
	hash := actionHash(call.ToolName, call.Args)
	if schema.RequiresApproval && !approved[hash] && !turn.AutoApprove {
		if turn.noApprovals != "" {
			// There is no card to raise: the person just declined one, or this
			// run has nobody to ask. Either way the model is told why.
			return r.refuseApproval(ctx, turn, call, out)
		}
		return tool.Result{}, r.park(ctx, turn, resolved, call, schema, hash, nil, out)
	}

	r.log.Info().Str("tool", call.ToolName).Bool("async", schema.Async).
		Int64("session_id", turn.SessionID).Msg("running tool")

	started := time.Now()
	result, err := r.invokeHandler(ctx, turn, schema, call, handler, out)
	if err != nil {
		// The tool's own error text is never handed to the model verbatim: it
		// may carry internals. The model gets a plain statement it can work
		// with, and the real cause goes to the log and the audit row.
		r.log.Error().Err(err).Str("tool", call.ToolName).Int64("session_id", turn.SessionID).
			Msg("tool failed")
		return r.toolFailure(ctx, turn, call, schema, "the tool failed", out)
	}

	// The tool ran. Recording its outcome (success, or a failure it reported
	// through the result) is the same for every tool, and is shared with the
	// pre-park refusal above, so both tell the transcript the same kind of truth.
	return r.finishToolResult(ctx, turn, call, schema, result, time.Since(started), approved[hash], flag, out)
}

// finishToolResult records a call's outcome against its row, tells the client,
// and hands the result back to the loop. It is the one place a tool's result
// becomes a transcript row, whether the tool RAN or its pre-park check refused it
// before it could, so both tell the same truth and a bad-arguments refusal still
// teaches the workspace the correct usage.
func (r *Runner) finishToolResult(
	ctx context.Context,
	turn Turn,
	call *model.ToolCall,
	schema tool.Schema,
	result tool.Result,
	duration time.Duration,
	requestedApproval bool,
	flag *memoryFlag,
	out *chat.Stream,
) (tool.Result, error) {
	if result.Err == tool.ErrorBadArguments {
		flagToolMistake(flag, schema, result)
	}
	status, errText := model.ToolCallCompleted, ""
	if result.Failed() {
		status, errText = model.ToolCallFailed, resultErrorText(result)
	}
	r.resolveCall(ctx, turn, call, status, result.Content, errText, duration, requestedApproval)
	if err := r.emitToolDone(out, call, status, result.Content, duration, r.openableID(schema, call)); err != nil {
		return tool.Result{}, err
	}
	return result, nil
}

// runValidator runs a tool's pre-park check, if it has one. It returns the schema
// (annotated with any per-call card copy the check produced, so the person reads
// what will actually happen) and, when the check REFUSED the call, a result to
// return without ever showing a card. A tool with no validator passes straight
// through, exactly as before.
func (r *Runner) runValidator(
	ctx context.Context,
	turn Turn,
	call *model.ToolCall,
	schema tool.Schema,
	validators map[string]tool.Validator,
) (tool.Schema, *tool.Result) {
	validate, ok := validators[call.ToolName]
	if !ok {
		return schema, nil
	}
	v := validate(ctx, tool.Call{
		WorkspaceID: turn.WorkspaceID,
		UserID:      turn.UserID,
		SessionID:   turn.SessionID,
		Name:        call.ToolName,
		Args:        call.Args,
	})
	if !v.OK {
		return schema, &v.Result
	}
	if v.ApprovalTitle != "" {
		schema.ApprovalTitle = v.ApprovalTitle
	}
	if v.ApprovalPrompt != "" {
		schema.ApprovalPrompt = v.ApprovalPrompt
	}
	return schema, nil
}

// refuseApproval declines a would-be card during a post-rejection continuation:
// the person just said no, so the model does not get to raise another one. The
// model reads a normal tool result telling it to respond instead, and the call
// is recorded refused so nothing is left spinning.
func (r *Runner) refuseApproval(ctx context.Context, turn Turn, call *model.ToolCall, out *chat.Stream) (tool.Result, error) {
	payload, _ := json.Marshal(map[string]any{"success": false, "declined": true, "error": turn.noApprovals})
	r.resolveCall(ctx, turn, call, model.ToolCallRejected, payload, "", 0, false)
	if err := r.emitToolDone(out, call, model.ToolCallRejected, payload, 0,
		r.openableID(schemaOf(turn, call), call)); err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Content: payload, Err: tool.ErrorDenied}, nil
}

// resultErrorText pulls a short, model-safe reason out of a failed result's
// content for the transcript, falling back to the failure's classification.
func resultErrorText(result tool.Result) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(result.Content, &payload); err == nil && payload.Error != "" {
		return payload.Error
	}
	return string(result.Err)
}

// invokeHandler runs a tool handler, keeping the stream alive while it works. A
// tool marked async runs on its own goroutine under its own deadline (see
// asynctool.go); a sync tool runs inline. Both are the same to the caller: a
// result, or an error the caller turns into a tool failure.
func (r *Runner) invokeHandler(
	ctx context.Context,
	turn Turn,
	schema tool.Schema,
	call *model.ToolCall,
	handler tool.Handler,
	out *chat.Stream,
) (tool.Result, error) {
	run := func(runCtx context.Context) (tool.Result, error) {
		return handler(runCtx, tool.Call{
			WorkspaceID: turn.WorkspaceID,
			UserID:      turn.UserID,
			SessionID:   turn.SessionID,
			DeviceID:    turn.DeviceID,
			Name:        call.ToolName,
			Args:        call.Args,
		})
	}
	return runWithHeartbeat(ctx, out, func() (tool.Result, error) {
		if schema.Async {
			return runAsyncTool(ctx, asyncTimeout(schema), run)
		}
		return run(ctx)
	})
}

// resolveCall writes what a call did, against the row the step already created.
func (r *Runner) resolveCall(
	ctx context.Context,
	turn Turn,
	call *model.ToolCall,
	status string,
	result json.RawMessage,
	errorText string,
	duration time.Duration,
	requestedApproval bool,
) {
	now := time.Now().UTC()
	call.Status = status
	call.Result = result
	call.ErrorText = errorText
	call.DurationMS = duration.Milliseconds()
	call.RequestedApproval = call.RequestedApproval || requestedApproval
	call.CompletedAt = &now
	call.SessionID = turn.SessionID
	call.WorkspaceID = turn.WorkspaceID

	if err := r.store.Agent().ResolveToolCall(ctx, call); err != nil {
		r.log.Error().Err(err).Str("tool", call.ToolName).Msg("record tool call")
	}
}

func (r *Runner) emitToolDone(
	out *chat.Stream,
	call *model.ToolCall,
	status string,
	result json.RawMessage,
	duration time.Duration,
	openable string,
) error {
	return out.Write(chat.Frame{
		Type: chat.FrameTool,
		Message: chat.ToolMessage{
			Name:              call.ToolName,
			FriendlyName:      friendlyOrName(call),
			Done:              true,
			Status:            status,
			DurationMS:        duration.Milliseconds(),
			Input:             call.Args,
			Output:            result,
			RequestedApproval: call.RequestedApproval,
			ID:                openable,
		},
	})
}

// ourOwnWiring is what a call site passes when there is nothing for a person to
// open: delegation, the fleet, the loop's own acknowledgements. Named rather
// than written as "" so a reader of the call sees the decision instead of an
// empty argument.
const ourOwnWiring = ""

// schemaOf is this turn's schema for a call, or the zero value when the tool is
// gone. The zero value is not internal, so a call whose tool has been revoked
// stays openable: what it was sent and what it answered are still recorded, and
// they are exactly what somebody asking "what happened here" wants.
func schemaOf(turn Turn, call *model.ToolCall) tool.Schema {
	schema, _ := turn.Tools.Schema(call.ToolName)
	return schema
}

// openableID is the id the chat opens a finished call by, or nothing.
//
// The same rule the history answer applies, applied here so a row behaves the
// same live as it does after a reload: our own wiring is not opened, and
// neither is a call with no row to read back. It is answered from the SCHEMA
// this turn actually ran, which is the complete answer: the loop's own tools
// are built per turn and are in no registry to ask.
func (r *Runner) openableID(schema tool.Schema, call *model.ToolCall) string {
	if call.ID == 0 || schema.Kind == tool.KindInternal {
		return ""
	}
	return strconv.FormatInt(call.ID, 10)
}

func friendlyOrName(call *model.ToolCall) string {
	if call.FriendlyName != "" {
		return call.FriendlyName
	}
	return call.ToolName
}

// runWithHeartbeat keeps the stream alive while a tool works. Without it a
// slow tool looks exactly like a dead connection, and clients (and proxies)
// hang up on what is really a working turn.
//
// The writer is safe for concurrent use, which is why the ticker can write
// while the tool runs.
func runWithHeartbeat(ctx context.Context, out *chat.Stream, work func() (tool.Result, error)) (tool.Result, error) {
	done := make(chan struct{})
	defer close(done)

	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := out.Heartbeat(); err != nil {
					return
				}
			}
		}
	}()

	return work()
}

// toolFailure reports a failed tool to both the client and the model. The
// model sees a normal result and can apologise or try something else, which
// is far better than the turn dying.
//
// The payload handed to the model is also the one stored as the call's result,
// so the transcript a later turn reads is exactly the transcript this turn saw.
func (r *Runner) toolFailure(
	ctx context.Context,
	turn Turn,
	call *model.ToolCall,
	schema tool.Schema,
	reason string,
	out *chat.Stream,
) (tool.Result, error) {
	payload, err := json.Marshal(map[string]any{"success": false, "error": reason})
	if err != nil {
		return tool.Result{}, err
	}
	if call.FriendlyName == "" {
		call.FriendlyName = schema.FriendlyName
	}
	r.resolveCall(ctx, turn, call, model.ToolCallFailed, payload, reason, 0, false)

	if err := r.emitToolDone(out, call, model.ToolCallFailed, nil, 0, r.openableID(schema, call)); err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Content: payload}, nil
}

// park freezes the turn and asks the user to approve.
//
// The PREPARED CALL is written down first, then the card is shown. That order
// is the whole contract: what runs on approval is exactly what was validated
// and displayed, never something a client sends afterwards, so an approved
// action is always redeemable (KB/02, park-and-resume).
//
// The park row carries no copy of the conversation. The transcript is already
// stored, step by step, and the resumed turn rebuilds it from there: a frozen
// duplicate could only drift from the truth.
// parkedDelegation addresses the delegation a parked call belongs to. It is nil
// for a Gateway park, and set when the call that stopped the turn is a
// agent's, so the resume knows to re-enter that delegation.
type parkedDelegation struct {
	Key          string
	ParentCallID string
	Mode         string
	// ID is the delegation row this agent is running as, when it has one. It is
	// what makes a card belong to ONE member of a batch: a fleet's members share
	// a parent call, so without it an answered card could re-enter the wrong one.
	ID int64
	// Detached marks a park raised inside a delegation that runs AWAY from the
	// turn that asked for it (background, or a fleet member on a worker). Two
	// things follow from it, and both are about the turn already being over.
	//
	// Its `delegate` call is already resolved as "started", so this park must not
	// re-stub it as waiting: the chip is a completed hand-off and the card stands
	// on its own. And its card WAITS ITS TURN, because several detached agents
	// can stop to ask at the same moment and the person is shown one at a time.
	Detached bool
}

func (r *Runner) park(
	ctx context.Context,
	turn Turn,
	resolved *provider.Resolved,
	call *model.ToolCall,
	schema tool.Schema,
	hash string,
	deleg *parkedDelegation,
	out *chat.Stream,
) error {
	token, tokenHash, err := newToken()
	if err != nil {
		return err
	}

	expiresAt := time.Now().UTC().Add(turn.approvalTTL())
	park := &model.ParkSnapshot{
		TokenHash:   tokenHash,
		WorkspaceID: turn.WorkspaceID,
		SessionID:   turn.SessionID,
		UserID:      turn.UserID,
		ModelID:     resolved.Model.ID,
		ToolName:    call.ToolName,
		ToolCallID:  call.ToolCallID,
		ToolArgs:    call.Args,
		ActionHash:  hash,
		// For a projected MCP tool: what the tool WAS when this card was
		// shown. The resume compares before running.
		DefinitionHash: schema.DefinitionHash,
		ExpiresAt:      expiresAt,
	}
	if deleg != nil {
		// This call is an agent's. The snapshot records which delegation, so
		// the resume re-enters it rather than running the call in the Gateway's
		// loop, and it pins the GATEWAY's model (not the agent's, which
		// `resolved` is here): the transcript belongs to the Gateway's model, and
		// the agent's model is re-derived from its own configuration.
		park.ModelID = turn.ModelID
		park.AgentKey = deleg.Key
		park.ParentToolCallID = deleg.ParentCallID
		park.HandoffMode = deleg.Mode
		if deleg.ID != 0 {
			park.DelegationID = &deleg.ID
		}
	}
	// A detached agent's card waits its turn: if the conversation already has a
	// live card, this one is queued and shown only when that one is answered
	// (KB/27), so several agents asking at once do not flood the person with a
	// card each. Everything else (the Gateway, a synchronous agent) parks the one
	// card there can be.
	if deleg != nil && deleg.Detached {
		if _, err := r.store.Agent().CreateParkSequenced(ctx, park); err != nil {
			return err
		}
	} else if err := r.store.Agent().CreatePark(ctx, park); err != nil {
		return err
	}

	// The call stops being "running" and becomes "waiting on a person", so a
	// reloaded conversation shows the pending card instead of a tool that is
	// mysteriously still spinning.
	call.RequestedApproval = true
	r.resolveCall(ctx, turn, call, model.ToolCallApprovalRequired, nil, "", 0, true)

	if deleg != nil && !deleg.Detached {
		// The Gateway's `delegate` call really ran, up to the point the agent
		// stopped to ask. Stub it as waiting-on-approval too, or a reloaded
		// conversation shows the delegation chip spinning forever while the card
		// sits on screen. It is resolved for real when the delegation finishes.
		//
		// A BACKGROUND delegation is the exception: its Gateway turn already ended
		// and its `delegate` call is already resolved "started", so re-stubbing it
		// would make a settled chip spin again. The card stands alone.
		parent := &model.ToolCall{
			SessionID: turn.SessionID, WorkspaceID: turn.WorkspaceID,
			ToolCallID: deleg.ParentCallID, ToolName: model.DelegateToolName,
			FriendlyName: agentFriendlyName,
		}
		parent.RequestedApproval = true
		r.resolveCall(ctx, turn, parent, model.ToolCallApprovalRequired, nil, "", 0, true)
	}

	if err := r.store.Agent().SetSessionStatus(ctx, turn.SessionID, model.SessionWaitingApproval); err != nil {
		r.log.Error().Err(err).Msg("mark session waiting")
	}

	// The card is the final frame: the stream closes and the connection is
	// released. The user may answer in a minute or tomorrow.
	if err := out.Write(chat.Frame{
		Type:  chat.FrameConfirmRequest,
		Final: true,
		Message: chat.ConfirmRequest{
			Token:       token,
			Title:       schema.ConfirmTitle(),
			Description: schema.ConfirmDescription(),
			Severity:    string(schema.Risk),
			Details:     call.Args,
			ExpiresAt:   expiresAt.Unix(),
		},
	}); err != nil {
		return err
	}
	return errParked
}

// completeParkedTool runs (or refuses) the call a person just answered about,
// and writes the outcome against the row the card was showing.
//
// The action executed is the one written down when the turn parked, never
// anything a client sends now: approving a card must not be a way to run a
// different action. The transcript is rebuilt afterwards, by the caller, so the
// model simply sees a conversation in which this call has an answer.
func (r *Runner) completeParkedTool(
	ctx context.Context,
	turn Turn,
	handlers map[string]tool.Handler,
	byName map[string]tool.Schema,
	out *chat.Stream,
) error {
	snapshot := turn.Resume.Snapshot
	call := &model.ToolCall{
		SessionID:         turn.SessionID,
		WorkspaceID:       turn.WorkspaceID,
		ToolCallID:        snapshot.ToolCallID,
		ToolName:          snapshot.ToolName,
		FriendlyName:      friendlyName(byName, snapshot.ToolName),
		Args:              snapshot.ToolArgs,
		RequestedApproval: true,
	}

	decision := model.ParkRejected
	if turn.Resume.Approved {
		decision = model.ParkApproved
	}
	if err := out.Write(chat.Frame{
		Type:    chat.FrameConfirmResolved,
		Message: chat.ConfirmResolved{Token: turn.Resume.Token, Status: decision},
	}); err != nil {
		return err
	}

	if !turn.Resume.Approved {
		// A rejection is a normal tool result, not an error: the model reads
		// it and explains itself instead of the turn dying.
		payload, err := json.Marshal(map[string]any{
			"success":  false,
			"rejected": true,
			"error":    "The user did not approve this action.",
		})
		if err != nil {
			return err
		}
		r.resolveCall(ctx, turn, call, model.ToolCallRejected, payload, "", 0, true)
		return r.emitToolDone(out, call, model.ToolCallRejected, payload, 0,
			r.openableID(byName[call.ToolName], call))
	}

	handler, ok := handlers[call.ToolName]
	if !ok {
		// The tool went away between the card and the approval: a deploy, or an
		// administrator revoking it. The approval was real, so say what happened
		// rather than pretending it ran.
		_, err := r.toolFailure(ctx, turn, call, byName[call.ToolName],
			"the tool is no longer available", out)
		return err
	}
	if snapshot.DefinitionHash != "" && byName[call.ToolName].DefinitionHash != snapshot.DefinitionHash {
		// The remote redefined the tool while the card sat waiting. Nobody
		// approved the new semantics, so the approval is stale: approval must
		// equal success, and success here would be running something unseen.
		_, err := r.toolFailure(ctx, turn, call, byName[call.ToolName],
			"the external service changed this action while approval was pending; please ask again", out)
		return err
	}

	started := time.Now()
	result, err := r.invokeHandler(ctx, turn, byName[call.ToolName], call, handler, out)
	if err != nil {
		r.log.Error().Err(err).Str("tool", call.ToolName).Msg("approved tool failed")
		_, ferr := r.toolFailure(ctx, turn, call, byName[call.ToolName], "the tool failed", out)
		return ferr
	}

	duration := time.Since(started)
	r.resolveCall(ctx, turn, call, model.ToolCallCompleted, result.Content, "", duration, true)
	return r.emitToolDone(out, call, model.ToolCallCompleted, result.Content, duration,
		r.openableID(byName[call.ToolName], call))
}
