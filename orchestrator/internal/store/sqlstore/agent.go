package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/store/sqldb"
)

type agentStore struct{ db *sqldb.DB }

// --- sessions ---------------------------------------------------------------

func (s *agentStore) CreateSession(ctx context.Context, sess *model.AgentSession) error {
	sess.StartedAt = time.Now().UTC()
	if sess.Status == "" {
		sess.Status = model.SessionRunning
	}
	if sess.UID == "" {
		uid, err := model.NewChatUID()
		if err != nil {
			return err
		}
		sess.UID = uid
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_sessions
		 (uid, workspace_id, user_id, agent_id, workflow_version_id, parent_session_id,
		  channel, external_key, title, status, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.UID, sess.WorkspaceID, sess.UserID, sess.AgentID, sess.WorkflowVersionID,
		sess.ParentSessionID, sess.Channel, nullString(sess.ExternalKey),
		nullString(sess.Title), sess.Status, sess.StartedAt)
	if err != nil {
		return wrapWriteErr("insert session", err)
	}
	sess.ID, err = res.LastInsertId()
	return err
}

func (s *agentStore) GetSession(ctx context.Context, workspaceID, id int64) (*model.AgentSession, error) {
	sess := &model.AgentSession{}
	var externalKey, title sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, uid, workspace_id, user_id, agent_id, workflow_version_id, parent_session_id,
		        channel, external_key, title, status, approval_mode, is_pinned, last_message_at,
		        message_count, started_at, completed_at
		 FROM agent_sessions WHERE id = ? AND workspace_id = ?`, id, workspaceID).
		Scan(&sess.ID, &sess.UID, &sess.WorkspaceID, &sess.UserID, &sess.AgentID,
			&sess.WorkflowVersionID, &sess.ParentSessionID, &sess.Channel, &externalKey,
			&title, &sess.Status, &sess.ApprovalMode, &sess.IsPinned, &sess.LastMessageAt, &sess.MessageCount,
			&sess.StartedAt, &sess.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	sess.ExternalKey, sess.Title = externalKey.String, title.String
	return sess, nil
}

// GetSessionByUID is how a request names a conversation. Nothing from outside
// the server ever arrives holding a row id.
func (s *agentStore) GetSessionByUID(ctx context.Context, workspaceID int64, uid string) (*model.AgentSession, error) {
	sess := &model.AgentSession{}
	var externalKey, title sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, uid, workspace_id, user_id, agent_id, workflow_version_id, parent_session_id,
		        channel, external_key, title, status, approval_mode, is_pinned, last_message_at,
		        message_count, started_at, completed_at
		 FROM agent_sessions WHERE uid = ? AND workspace_id = ?`, uid, workspaceID).
		Scan(&sess.ID, &sess.UID, &sess.WorkspaceID, &sess.UserID, &sess.AgentID,
			&sess.WorkflowVersionID, &sess.ParentSessionID, &sess.Channel, &externalKey,
			&title, &sess.Status, &sess.ApprovalMode, &sess.IsPinned, &sess.LastMessageAt, &sess.MessageCount,
			&sess.StartedAt, &sess.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get session by uid: %w", err)
	}
	sess.ExternalKey, sess.Title = externalKey.String, title.String
	return sess, nil
}

func (s *agentStore) SetSessionStatus(ctx context.Context, id int64, status string) error {
	var completedAt any
	switch status {
	case model.SessionCompleted, model.SessionFailed, model.SessionCancelled:
		completedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_sessions SET status = ?, completed_at = ? WHERE id = ?`,
		status, completedAt, id)
	if err != nil {
		return fmt.Errorf("set session status: %w", err)
	}
	return nil
}

// SetSessionApprovalMode switches a conversation between asking every time and
// auto-approving for the rest of the session.
func (s *agentStore) SetSessionApprovalMode(ctx context.Context, id int64, mode string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_sessions SET approval_mode = ? WHERE id = ?`, mode, id)
	if err != nil {
		return fmt.Errorf("set session approval mode: %w", err)
	}
	return nil
}

func (s *agentStore) SetSessionTitle(ctx context.Context, id int64, title string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agent_sessions SET title = ? WHERE id = ?`, title, id)
	if err != nil {
		return fmt.Errorf("set session title: %w", err)
	}
	return nil
}

// --- transcript ---------------------------------------------------------------

// SaveStep writes a step and everything it produced, in one transaction.
//
// It is an upsert on (session, seq), so the same step written twice (a
// checkpoint refining itself, a resumed turn replaying) lands in place rather
// than twice. That is what lets the checkpoint run on a timer without leaving
// a trail of half-written rows behind it.
func (s *agentStore) SaveStep(ctx context.Context, step *model.AgentStep) error {
	now := time.Now().UTC()
	if step.CreatedAt.IsZero() {
		step.CreatedAt = now
	}

	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO agent_steps
			  (session_id, seq, kind, vendor, model, agent_key, parent_tool_call_id,
			   is_partial, attachments, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON DUPLICATE KEY UPDATE
			   kind = VALUES(kind), vendor = VALUES(vendor), model = VALUES(model),
			   agent_key = VALUES(agent_key), parent_tool_call_id = VALUES(parent_tool_call_id),
			   is_partial = VALUES(is_partial), attachments = VALUES(attachments),
			   updated_at = VALUES(updated_at)`,
			step.SessionID, step.Seq, step.Kind, nullString(step.Vendor), nullString(step.Model),
			nullString(step.AgentKey), nullString(step.ParentToolCallID),
			step.Partial, attachmentsJSON(step.Attachments), step.CreatedAt, now); err != nil {
			return wrapWriteErr("save step", err)
		}

		// The insert id of an ON DUPLICATE KEY UPDATE is not the existing row's
		// id, so the step is read back rather than guessed at.
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM agent_steps WHERE session_id = ? AND seq = ?`,
			step.SessionID, step.Seq).Scan(&step.ID); err != nil {
			return fmt.Errorf("read step id: %w", err)
		}

		if err := saveStepText(ctx, tx, step); err != nil {
			return err
		}
		if err := saveStepReasoning(ctx, tx, step); err != nil {
			return err
		}
		return saveStepToolCalls(ctx, tx, step)
	})
}

func saveStepText(ctx context.Context, tx *sqldb.Tx, step *model.AgentStep) error {
	if !step.HasText() {
		// A step that produced no text has no message row. An assistant that
		// called a tool without narrating is normal, and an empty row would
		// pollute the search index and the timeline alike.
		return nil
	}
	role := model.RoleAssistant
	if step.Kind == model.StepUser {
		role = model.RoleUser
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_messages (step_id, session_id, role, content, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE content = VALUES(content)`,
		step.ID, step.SessionID, role, step.Text, step.CreatedAt); err != nil {
		return wrapWriteErr("save step text", err)
	}
	return nil
}

func saveStepReasoning(ctx context.Context, tx *sqldb.Tx, step *model.AgentStep) error {
	if step.Reasoning == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_reasoning (step_id, content, vendor, model, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE content = VALUES(content)`,
		step.ID, step.Reasoning, nullString(step.Vendor), nullString(step.Model),
		step.CreatedAt); err != nil {
		return wrapWriteErr("save step reasoning", err)
	}
	return nil
}

func saveStepToolCalls(ctx context.Context, tx *sqldb.Tx, step *model.AgentStep) error {
	for i, call := range step.ToolCalls {
		call.StepID, call.SessionID = step.ID, step.SessionID
		if call.ExecutionOrder == 0 {
			call.ExecutionOrder = i
		}
		if call.CreatedAt.IsZero() {
			call.CreatedAt = step.CreatedAt
		}
		if err := upsertToolCall(ctx, tx, call); err != nil {
			return err
		}
	}
	return nil
}

// upsertToolCall is idempotent on (session, tool call id). A resumed turn
// replays the same call and must refine the row rather than add a second.
//
// Two columns are deliberately NOT overwritten on an existing row:
//
//   - step_id, because a call belongs to the step that made it, permanently. A
//     turn approved an hour later resolves the call without knowing or caring
//     which step produced it, and moving the row would tear it out of the
//     conversation it happened in.
//   - requested_approval, which is OR-ed: a call that once went through a
//     confirmation card says so forever, even though the approved run that
//     follows never asks again. That is the audit trail.
func upsertToolCall(ctx context.Context, tx *sqldb.Tx, c *model.ToolCall) error {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO agent_tool_calls
		  (step_id, session_id, workspace_id, tool_call_id, tool_name, friendly_name,
		   args, result, status, error_text, duration_ms, execution_order,
		   requested_approval, created_at, completed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		   result = VALUES(result), status = VALUES(status),
		   error_text = VALUES(error_text), duration_ms = VALUES(duration_ms),
		   requested_approval = requested_approval OR VALUES(requested_approval),
		   completed_at = VALUES(completed_at)`,
		c.StepID, c.SessionID, c.WorkspaceID, c.ToolCallID, c.ToolName,
		nullString(c.FriendlyName), nullJSON(c.Args), nullJSON(c.Result), c.Status,
		nullString(c.ErrorText), c.DurationMS, c.ExecutionOrder,
		c.RequestedApproval, c.CreatedAt, c.CompletedAt)
	if err != nil {
		return wrapWriteErr("save tool call", err)
	}
	if c.ID == 0 {
		c.ID, _ = res.LastInsertId()
	}
	return nil
}

// MarkToolCallsInterrupted closes out every tool call still claiming to be
// running. Called once at boot: nothing can legitimately be running in a process
// that has just started, so a row that says it is, is a ghost of the last
// shutdown, and the transcript was showing a spinner beside a tool nothing is
// doing (and for calls whose turn parked, would never do). Returns how many.
func (s *agentStore) MarkToolCallsInterrupted(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_tool_calls SET status = ?, error_text = ?, completed_at = ?
		 WHERE status = ?`,
		model.ToolCallFailed, "the service was interrupted before it could finish",
		time.Now().UTC(), model.ToolCallRunning)
	if err != nil {
		return 0, fmt.Errorf("mark interrupted tool calls: %w", err)
	}
	return res.RowsAffected()
}

// ResolveToolCall records what a call finally did. It is how a parked call
// stops being pending: the approved run writes its result here, against the
// row the card was showing.
func (s *agentStore) ResolveToolCall(ctx context.Context, c *model.ToolCall) error {
	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		// The row's id comes back with the check that it exists, in the same
		// query the check already costs, because the caller needs it: a
		// resolved call is handed to the chat, which opens it by its row.
		//
		// It is read rather than taken from the upsert. What LastInsertId
		// reports after an INSERT ... ON DUPLICATE KEY UPDATE that UPDATED a
		// row is server behaviour, not a contract: this one answers with the
		// row's id, which is why the id was right before this existed, and it
		// is not something to build on.
		id, err := existingToolCallID(ctx, tx, c.SessionID, c.ToolCallID)
		if err != nil {
			return err
		}
		if c.ID == 0 {
			c.ID = id
		}
		return upsertToolCall(ctx, tx, c)
	})
}

// existingToolCallID is the row for this call in this conversation, and the
// check that there is one.
func existingToolCallID(ctx context.Context, tx *sqldb.Tx, sessionID int64, toolCallID string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM agent_tool_calls WHERE session_id = ? AND tool_call_id = ?`,
		sessionID, toolCallID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("resolve tool call: %w", store.ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("resolve tool call: %w", err)
	}
	return id, nil
}

// ToolCall reads one call, for somebody opening it in the chat.
//
// Scoped by workspace like every read here: an id from another workspace is not
// refused, it does not exist, so nothing can be learned by asking.
func (s *agentStore) ToolCall(ctx context.Context, workspaceID, id int64) (*model.ToolCall, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, step_id, session_id, workspace_id, tool_call_id, tool_name,
		        friendly_name, args, result, status, error_text, duration_ms,
		        execution_order, requested_approval
		 FROM agent_tool_calls WHERE workspace_id = ? AND id = ?`, workspaceID, id)

	var call model.ToolCall
	var friendly, errorText sql.NullString
	var args, result []byte
	err := row.Scan(&call.ID, &call.StepID, &call.SessionID, &call.WorkspaceID,
		&call.ToolCallID, &call.ToolName, &friendly, &args, &result, &call.Status,
		&errorText, &call.DurationMS, &call.ExecutionOrder, &call.RequestedApproval)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read tool call: %w", err)
	}
	call.FriendlyName = friendly.String
	call.ErrorText = errorText.String
	call.Args = json.RawMessage(args)
	call.Result = json.RawMessage(result)
	return &call, nil
}

// Transcript loads the conversation as ordered steps, each carrying what it
// produced. It is the single read path: the model-facing history, the chat
// timeline, and the resumed turn are all built from it, so they cannot
// disagree about what happened.
// TranscriptPage is what a person is shown: the newest part of a conversation,
// and older parts when they scroll back for them.
//
// Counted in ROWS rather than steps, because a row is what costs: a step with
// eleven tool calls is twelve things on the screen and twelve things in the
// DOM. A long conversation was sent whole on every open, which is a payload,
// a memory cost, and about half the time it takes to type a character into the
// composer with the thing on screen.
//
// before is the seq to read backwards from, or zero for the newest. It returns
// the steps in the order they happened, and whether there is more behind them.
func (s *agentStore) TranscriptPage(ctx context.Context, sessionID int64, before, rows int) ([]*model.AgentStep, bool, error) {
	if rows <= 0 {
		rows = model.HistoryRows
	}
	if before <= 0 {
		before = math.MaxInt32
	}
	// How far back the budget reaches. One cheap pass over the sizes, newest
	// first, rather than reading a conversation to find out how big it is.
	const sizes sqldb.Statement = `SELECT s.seq,
		        (SELECT COUNT(*) FROM agent_messages m WHERE m.step_id = s.id) AS said,
		        (SELECT COUNT(*) FROM agent_tool_calls c WHERE c.step_id = s.id) AS called
		 FROM agent_steps s
		 WHERE s.session_id = ? AND s.seq < ?
		 ORDER BY s.seq DESC
		 LIMIT ?`
	// One step beyond the budget, so "is there more" is answered by what was
	// read rather than by a second query.
	sized, err := s.db.QueryContext(ctx, sizes, sessionID, before, rows+1)
	if err != nil {
		return nil, false, fmt.Errorf("size the transcript: %w", err)
	}
	defer func() { _ = sized.Close() }()

	// found, not "from is non-zero": a conversation's first step is seq ZERO,
	// so a sentinel of zero threw away every page that reached the beginning,
	// which is every short conversation.
	from, counted, more, found := 0, 0, false, false
	for sized.Next() {
		var seq, said, called int
		if err := sized.Scan(&seq, &said, &called); err != nil {
			return nil, false, fmt.Errorf("scan a step's size: %w", err)
		}
		if counted > 0 && counted+said+called > rows {
			// This step would take it over. Everything from here back is the
			// next page, and a step is never split across two.
			more = true
			break
		}
		counted += said + called
		from, found = seq, true
	}
	if err := sized.Err(); err != nil {
		return nil, false, err
	}
	if !found {
		return []*model.AgentStep{}, false, nil
	}

	steps, err := s.transcript(ctx, sessionID, from, before)
	return steps, more, err
}

// Transcript is the WHOLE conversation, which is what the model is given: it is
// trimmed to the model's context window rather than to a number of rows, and
// trimming it here would be deciding what the assistant may remember.
func (s *agentStore) Transcript(ctx context.Context, sessionID int64) ([]*model.AgentStep, error) {
	return s.transcript(ctx, sessionID, 0, math.MaxInt32)
}

// transcript reads the steps of one conversation between two seq numbers, with
// their tool calls. from is inclusive, before is exclusive.
func (s *agentStore) transcript(ctx context.Context, sessionID int64, from, before int) ([]*model.AgentStep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.session_id, s.seq, s.kind, s.vendor, s.model,
		        s.agent_key, s.parent_tool_call_id, s.is_partial, s.attachments,
		        s.created_at, m.content, r.content
		 FROM agent_steps s
		 LEFT JOIN agent_messages m ON m.step_id = s.id
		 LEFT JOIN agent_reasoning r ON r.step_id = s.id
		 WHERE s.session_id = ? AND s.seq >= ? AND s.seq < ?
		 ORDER BY s.seq`, sessionID, from, before)
	if err != nil {
		return nil, fmt.Errorf("load transcript: %w", err)
	}
	defer func() { _ = rows.Close() }()

	steps := []*model.AgentStep{}
	byID := map[int64]*model.AgentStep{}
	for rows.Next() {
		step := &model.AgentStep{}
		var vendor, modelName, agentKey, parentCall, text, reasoning, attachments sql.NullString
		if err := rows.Scan(&step.ID, &step.SessionID, &step.Seq, &step.Kind, &vendor,
			&modelName, &agentKey, &parentCall, &step.Partial, &attachments, &step.CreatedAt,
			&text, &reasoning); err != nil {
			return nil, fmt.Errorf("scan step: %w", err)
		}
		step.Vendor, step.Model = vendor.String, modelName.String
		step.AgentKey, step.ParentToolCallID = agentKey.String, parentCall.String
		step.Text, step.Reasoning = text.String, reasoning.String
		step.Attachments = attachmentsOf(attachments)
		steps = append(steps, step)
		byID[step.ID] = step
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return steps, nil
	}

	calls, err := s.db.QueryContext(ctx,
		`SELECT id, step_id, session_id, workspace_id, tool_call_id, tool_name,
		        friendly_name, args, result, status, error_text, duration_ms,
		        execution_order, requested_approval, created_at, completed_at
		 FROM agent_tool_calls
		 WHERE session_id = ? AND step_id IN (
		     SELECT id FROM agent_steps WHERE session_id = ? AND seq >= ? AND seq < ?
		 )
		 ORDER BY step_id, execution_order, id`, sessionID, sessionID, from, before)
	if err != nil {
		return nil, fmt.Errorf("load tool calls: %w", err)
	}
	defer func() { _ = calls.Close() }()

	for calls.Next() {
		c := &model.ToolCall{}
		var friendly, errText sql.NullString
		var args, result []byte
		var duration sql.NullInt64
		if err := calls.Scan(&c.ID, &c.StepID, &c.SessionID, &c.WorkspaceID, &c.ToolCallID,
			&c.ToolName, &friendly, &args, &result, &c.Status, &errText, &duration,
			&c.ExecutionOrder, &c.RequestedApproval, &c.CreatedAt, &c.CompletedAt); err != nil {
			return nil, fmt.Errorf("scan tool call: %w", err)
		}
		c.FriendlyName, c.ErrorText = friendly.String, errText.String
		c.Args, c.Result = json.RawMessage(args), json.RawMessage(result)
		c.DurationMS = duration.Int64
		if step, ok := byID[c.StepID]; ok {
			step.ToolCalls = append(step.ToolCalls, c)
		}
	}
	return steps, calls.Err()
}

// NextSeq hands out the next step position for a session, atomically. A
// background agent (KB/27) runs on its own goroutine, concurrently with the
// Gateway's turn, and both write steps to the same session: allocating from
// MAX(seq)+1 let both read the same value and land two different steps on one
// (session, seq) row, which SaveStep's upsert then merged. The counter lives on
// the session row and is handed out under a row lock, so concurrent callers
// serialize on it and every step gets a seq of its own.
func (s *agentStore) NextSeq(ctx context.Context, sessionID int64) (int, error) {
	var seq int64
	err := s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		// One atomic increment: LAST_INSERT_ID(next_seq) records the current value
		// (the seq we hand out) while the statement writes next_seq+1, all under the
		// row's own brief lock. No separate SELECT ... FOR UPDATE, so the lock window
		// is a single statement rather than a round-trip, which is what keeps many
		// concurrent writers on one session from piling up on it.
		res, err := tx.ExecContext(ctx,
			`UPDATE agent_sessions SET next_seq = LAST_INSERT_ID(next_seq) + 1 WHERE id = ?`, sessionID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return sql.ErrNoRows // no such session: a bad id, not a silent stale value
		}
		return tx.QueryRowContext(ctx, `SELECT LAST_INSERT_ID()`).Scan(&seq)
	})
	if err != nil {
		return 0, fmt.Errorf("next seq: %w", err)
	}
	return int(seq), nil
}

// --- model calls ----------------------------------------------------------------

func (s *agentStore) RecordModelCall(ctx context.Context, c *model.ModelCall) error {
	c.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO model_calls
		 (workspace_id, session_id, model_id, input_tokens, output_tokens, duration_ms,
		  status, error_text, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.WorkspaceID, c.SessionID, c.ModelID, c.InputTokens, c.OutputTokens,
		c.DurationMS, c.Status, nullString(c.ErrorText), c.CreatedAt)
	if err != nil {
		return wrapWriteErr("record model call", err)
	}
	c.ID, err = res.LastInsertId()
	return err
}

// --- park snapshots ---------------------------------------------------------------

func (s *agentStore) CreatePark(ctx context.Context, p *model.ParkSnapshot) error {
	p.CreatedAt = time.Now().UTC()
	if p.Status == "" {
		p.Status = model.ParkPending
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_park_snapshots
		 (token_hash, workspace_id, session_id, user_id, agent_id, model_id, tool_name,
		  tool_call_id, tool_args, action_hash, definition_hash,
		  agent_key, parent_tool_call_id, delegation_id, handoff_mode, status, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.TokenHash, p.WorkspaceID, p.SessionID, p.UserID, p.AgentID, p.ModelID, p.ToolName,
		p.ToolCallID, p.ToolArgs, p.ActionHash, nullString(p.DefinitionHash),
		nullString(p.AgentKey), nullString(p.ParentToolCallID), p.DelegationID, nullString(p.HandoffMode),
		p.Status, p.ExpiresAt, p.CreatedAt)
	if err != nil {
		return wrapWriteErr("create park", err)
	}
	p.ID, err = res.LastInsertId()
	return err
}

// CreateParkSequenced creates a park that waits its turn: if the session already
// has a live (pending) card, this one is inserted 'queued' and reports live
// false; otherwise it is 'pending' (the live card) and reports live true. The
// check and the insert share a transaction that locks the session's live parks,
// so several agents parking at the same instant cannot each decide they are
// first. It is how background approval cards are shown one at a time (KB/27).
func (s *agentStore) CreateParkSequenced(ctx context.Context, p *model.ParkSnapshot) (bool, error) {
	p.CreatedAt = time.Now().UTC()
	live := false
	err := s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		var liveCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM agent_park_snapshots
			 WHERE session_id = ? AND status = ? FOR UPDATE`,
			p.SessionID, model.ParkPending).Scan(&liveCount); err != nil {
			return fmt.Errorf("count live parks: %w", err)
		}
		p.Status = model.ParkQueued
		if liveCount == 0 {
			p.Status = model.ParkPending
			live = true
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO agent_park_snapshots
			 (token_hash, workspace_id, session_id, user_id, agent_id, model_id, tool_name,
			  tool_call_id, tool_args, action_hash, definition_hash,
			  agent_key, parent_tool_call_id, delegation_id, handoff_mode, status, expires_at, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.TokenHash, p.WorkspaceID, p.SessionID, p.UserID, p.AgentID, p.ModelID, p.ToolName,
			p.ToolCallID, p.ToolArgs, p.ActionHash, nullString(p.DefinitionHash),
			nullString(p.AgentKey), nullString(p.ParentToolCallID), p.DelegationID, nullString(p.HandoffMode),
			p.Status, p.ExpiresAt, p.CreatedAt)
		if err != nil {
			return wrapWriteErr("create park", err)
		}
		p.ID, err = res.LastInsertId()
		return err
	})
	return live, err
}

// ReleaseNextQueuedPark promotes the oldest queued park of a session to live
// (pending) and returns it, so answering one card surfaces the next. ErrNotFound
// when the queue is empty. Locks the row it promotes so two resolves cannot
// release the same one.
func (s *agentStore) ReleaseNextQueuedPark(ctx context.Context, sessionID int64) (*model.ParkSnapshot, error) {
	p := &model.ParkSnapshot{}
	err := s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		var definitionHash, agentKey, parentCall, handoffMode sql.NullString
		var delegationID sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT id, token_hash, workspace_id, session_id, user_id, agent_id, model_id,
			        tool_name, tool_call_id, tool_args, action_hash, definition_hash,
			        agent_key, parent_tool_call_id, delegation_id, handoff_mode, status,
			        expires_at, created_at, resolved_at
			 FROM agent_park_snapshots
			 WHERE session_id = ? AND status = ? AND expires_at > ?
			 ORDER BY id ASC LIMIT 1 FOR UPDATE`,
			sessionID, model.ParkQueued, time.Now().UTC()).
			Scan(&p.ID, &p.TokenHash, &p.WorkspaceID, &p.SessionID, &p.UserID, &p.AgentID, &p.ModelID,
				&p.ToolName, &p.ToolCallID, &p.ToolArgs, &p.ActionHash, &definitionHash,
				&agentKey, &parentCall, &delegationID, &handoffMode, &p.Status,
				&p.ExpiresAt, &p.CreatedAt, &p.ResolvedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("select queued park: %w", err)
		}
		p.DefinitionHash = definitionHash.String
		p.AgentKey, p.ParentToolCallID, p.HandoffMode = agentKey.String, parentCall.String, handoffMode.String
		if delegationID.Valid {
			p.DelegationID = &delegationID.Int64
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE agent_park_snapshots SET status = ? WHERE id = ?`,
			model.ParkPending, p.ID); err != nil {
			return fmt.Errorf("release queued park: %w", err)
		}
		p.Status = model.ParkPending
		return nil
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// QueuedParks returns a session's queued (not yet live) parks, oldest first, so
// "approve all" can drain them. Expired ones are left out; they are dead.
// OpenParks returns every approval a session still has outstanding, live card
// and queue alike, oldest first. Recovery uses it to close out the ones a dead
// agent raised: it has to see BOTH, because a live card and a queued one are the
// same thing to whoever is gone, and one session can hold more than one live
// park (an agent's and the Gateway's are created by different paths).
func (s *agentStore) OpenParks(ctx context.Context, sessionID int64) ([]*model.ParkSnapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, token_hash, workspace_id, session_id, user_id, agent_id, model_id,
		        tool_name, tool_call_id, tool_args, action_hash, definition_hash,
		        agent_key, parent_tool_call_id, delegation_id, handoff_mode, status,
		        expires_at, created_at, resolved_at
		 FROM agent_park_snapshots
		 WHERE session_id = ? AND status IN (?, ?) AND expires_at > ?
		 ORDER BY id ASC`,
		sessionID, model.ParkPending, model.ParkQueued, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("open parks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*model.ParkSnapshot{}
	for rows.Next() {
		p := &model.ParkSnapshot{}
		var definitionHash, agentKey, parentCall, handoffMode sql.NullString
		var delegationID sql.NullInt64
		if err := rows.Scan(&p.ID, &p.TokenHash, &p.WorkspaceID, &p.SessionID, &p.UserID, &p.AgentID, &p.ModelID,
			&p.ToolName, &p.ToolCallID, &p.ToolArgs, &p.ActionHash, &definitionHash,
			&agentKey, &parentCall, &delegationID, &handoffMode, &p.Status,
			&p.ExpiresAt, &p.CreatedAt, &p.ResolvedAt); err != nil {
			return nil, err
		}
		p.DefinitionHash = definitionHash.String
		p.AgentKey, p.ParentToolCallID, p.HandoffMode = agentKey.String, parentCall.String, handoffMode.String
		if delegationID.Valid {
			p.DelegationID = &delegationID.Int64
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *agentStore) QueuedParks(ctx context.Context, sessionID int64) ([]*model.ParkSnapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, token_hash, workspace_id, session_id, user_id, agent_id, model_id,
		        tool_name, tool_call_id, tool_args, action_hash, definition_hash,
		        agent_key, parent_tool_call_id, delegation_id, handoff_mode, status,
		        expires_at, created_at, resolved_at
		 FROM agent_park_snapshots
		 WHERE session_id = ? AND status = ? AND expires_at > ?
		 ORDER BY id ASC`,
		sessionID, model.ParkQueued, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("queued parks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*model.ParkSnapshot{}
	for rows.Next() {
		p := &model.ParkSnapshot{}
		var definitionHash, agentKey, parentCall, handoffMode sql.NullString
		var delegationID sql.NullInt64
		if err := rows.Scan(&p.ID, &p.TokenHash, &p.WorkspaceID, &p.SessionID, &p.UserID, &p.AgentID, &p.ModelID,
			&p.ToolName, &p.ToolCallID, &p.ToolArgs, &p.ActionHash, &definitionHash,
			&agentKey, &parentCall, &delegationID, &handoffMode, &p.Status,
			&p.ExpiresAt, &p.CreatedAt, &p.ResolvedAt); err != nil {
			return nil, err
		}
		p.DefinitionHash = definitionHash.String
		p.AgentKey, p.ParentToolCallID, p.HandoffMode = agentKey.String, parentCall.String, handoffMode.String
		if delegationID.Valid {
			p.DelegationID = &delegationID.Int64
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ReopenPark puts a claimed park back the way it was, keeping its token hash so
// the card the person is still looking at goes on working.
//
// It is the compensation for a resume that CLAIMED an approval and then could
// not start the turn: the claim is single-use on purpose, so without this the
// approval is simply spent. The person had clicked Approve, the server answered
// "this conversation is already answering", and the card was gone for good on
// the next reload with its tool call left reading "approval required" forever.
// Nothing ran, so putting it back approves nothing and risks no double
// execution: it restores the question that was never actually answered.
func (s *agentStore) ReopenPark(ctx context.Context, parkID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_park_snapshots SET status = ?, resolved_at = NULL
		 WHERE id = ? AND status IN (?, ?)`,
		model.ParkPending, parkID, model.ParkApproved, model.ParkRejected)
	if err != nil {
		return fmt.Errorf("reopen park: %w", err)
	}
	return nil
}

// ClaimPark atomically takes a pending snapshot and marks it resolved, so the
// same approval can never be redeemed twice, even by two clicks at once. It
// returns ErrNotFound when the token is unknown, already used, or expired:
// the caller must not be able to tell those apart.
func (s *agentStore) ClaimPark(ctx context.Context, tokenHash, decision string) (*model.ParkSnapshot, error) {
	p := &model.ParkSnapshot{}

	err := s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		var definitionHash, agentKey, parentCall, handoffMode sql.NullString
		var delegationID sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT id, token_hash, workspace_id, session_id, user_id, agent_id, model_id,
			        tool_name, tool_call_id, tool_args, action_hash, definition_hash,
			        agent_key, parent_tool_call_id, delegation_id, handoff_mode, status,
			        expires_at, created_at, resolved_at
			 FROM agent_park_snapshots WHERE token_hash = ? FOR UPDATE`, tokenHash).
			Scan(&p.ID, &p.TokenHash, &p.WorkspaceID, &p.SessionID, &p.UserID, &p.AgentID, &p.ModelID,
				&p.ToolName, &p.ToolCallID, &p.ToolArgs, &p.ActionHash, &definitionHash,
				&agentKey, &parentCall, &delegationID, &handoffMode, &p.Status,
				&p.ExpiresAt, &p.CreatedAt, &p.ResolvedAt)
		p.DefinitionHash = definitionHash.String
		p.AgentKey, p.ParentToolCallID, p.HandoffMode = agentKey.String, parentCall.String, handoffMode.String
		if delegationID.Valid {
			p.DelegationID = &delegationID.Int64
		}
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("select park: %w", err)
		}

		now := time.Now().UTC()
		// Already answered. Unknown and already-used look the same from
		// outside, deliberately.
		if p.Status != model.ParkPending {
			return store.ErrNotFound
		}
		if now.After(p.ExpiresAt) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE agent_park_snapshots SET status = ?, resolved_at = ? WHERE id = ?`,
				model.ParkExpired, now, p.ID); err != nil {
				return fmt.Errorf("expire park: %w", err)
			}
			// The expiry is committed: the approval was real, it simply came
			// too late, and the row should say so.
			return store.ErrExpired
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE agent_park_snapshots SET status = ?, resolved_at = ? WHERE id = ?`,
			decision, now, p.ID); err != nil {
			return fmt.Errorf("claim park: %w", err)
		}
		p.Status, p.ResolvedAt = decision, &now
		return nil
	})

	// An expired snapshot still has to record its expiry, and Tx rolls back on
	// any error, so the update is applied here where it will commit.
	if errors.Is(err, store.ErrExpired) {
		if _, uerr := s.db.ExecContext(ctx,
			`UPDATE agent_park_snapshots SET status = ?, resolved_at = ?
			 WHERE token_hash = ? AND status = ?`,
			model.ParkExpired, time.Now().UTC(), tokenHash, model.ParkPending); uerr != nil {
			return nil, fmt.Errorf("expire park: %w", uerr)
		}
		return nil, store.ErrExpired
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ResolveParkByID marks a park resolved by its id, for the queued cards "approve
// all" drains without ever showing them. Unlike ClaimPark it needs no token
// (there was no card to click) and only touches a still-open park.
func (s *agentStore) ResolveParkByID(ctx context.Context, parkID int64, decision string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_park_snapshots SET status = ?, resolved_at = ?
		 WHERE id = ? AND status IN (?, ?)`,
		decision, time.Now().UTC(), parkID, model.ParkQueued, model.ParkPending)
	if err != nil {
		return wrapWriteErr("resolve park by id", err)
	}
	return nil
}

// ResolveQueuedParks answers every card a conversation still has waiting, in
// ONE statement, and returns them.
//
// It is what "approve, and stop asking" means: the person said yes to all of
// them, so all of them are answered at once. Doing it a card at a time was a
// write and a round trip each, and with thirteen agents waiting the last one
// was answered long after the first, which the person watched happen.
//
// They are returned because the caller has to act on each of them afterwards
// (re-entering the agent that raised it), and reading them back would be a
// second query for rows this already had in hand.
func (s *agentStore) ResolveQueuedParks(ctx context.Context, sessionID int64, decision string) ([]*model.ParkSnapshot, error) {
	var out []*model.ParkSnapshot
	err := s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, token_hash, workspace_id, session_id, user_id, agent_id, model_id,
			        tool_name, tool_call_id, tool_args, action_hash, definition_hash,
			        agent_key, parent_tool_call_id, delegation_id, handoff_mode, status,
			        expires_at, created_at, resolved_at
			   FROM agent_park_snapshots
			  WHERE session_id = ? AND status = ? AND expires_at > ?
			  ORDER BY id FOR UPDATE`, sessionID, model.ParkQueued, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("read queued parks: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			p := &model.ParkSnapshot{}
			var definitionHash, agentKey, parentCall, handoffMode sql.NullString
			var delegationID sql.NullInt64
			if err := rows.Scan(&p.ID, &p.TokenHash, &p.WorkspaceID, &p.SessionID, &p.UserID, &p.AgentID, &p.ModelID,
				&p.ToolName, &p.ToolCallID, &p.ToolArgs, &p.ActionHash, &definitionHash,
				&agentKey, &parentCall, &delegationID, &handoffMode, &p.Status,
				&p.ExpiresAt, &p.CreatedAt, &p.ResolvedAt); err != nil {
				return err
			}
			p.DefinitionHash = definitionHash.String
			p.AgentKey, p.ParentToolCallID, p.HandoffMode = agentKey.String, parentCall.String, handoffMode.String
			if delegationID.Valid {
				p.DelegationID = &delegationID.Int64
			}
			out = append(out, p)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out) == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE agent_park_snapshots SET status = ?, resolved_at = ?
			  WHERE session_id = ? AND status = ?`,
			decision, time.Now().UTC(), sessionID, model.ParkQueued); err != nil {
			return wrapWriteErr("resolve queued parks", err)
		}
		return nil
	})
	return out, err
}

// PendingPark returns the one snapshot a session is still waiting on: pending,
// and not past its expiry. A session parks and waits on a single card at a time,
// so there is at most one. store.ErrNotFound when there is none.
func (s *agentStore) PendingPark(ctx context.Context, sessionID int64) (*model.ParkSnapshot, error) {
	p := &model.ParkSnapshot{}
	var definitionHash, agentKey, parentCall, handoffMode sql.NullString
	var delegationID sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, token_hash, workspace_id, session_id, user_id, agent_id, model_id,
		        tool_name, tool_call_id, tool_args, action_hash, definition_hash,
		        agent_key, parent_tool_call_id, delegation_id, handoff_mode, status,
		        expires_at, created_at, resolved_at
		 FROM agent_park_snapshots
		 WHERE session_id = ? AND status = ? AND expires_at > ?
		 ORDER BY id DESC LIMIT 1`,
		sessionID, model.ParkPending, time.Now().UTC()).
		Scan(&p.ID, &p.TokenHash, &p.WorkspaceID, &p.SessionID, &p.UserID, &p.AgentID, &p.ModelID,
			&p.ToolName, &p.ToolCallID, &p.ToolArgs, &p.ActionHash, &definitionHash,
			&agentKey, &parentCall, &delegationID, &handoffMode, &p.Status,
			&p.ExpiresAt, &p.CreatedAt, &p.ResolvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pending park: %w", err)
	}
	p.DefinitionHash = definitionHash.String
	p.AgentKey, p.ParentToolCallID, p.HandoffMode = agentKey.String, parentCall.String, handoffMode.String
	if delegationID.Valid {
		p.DelegationID = &delegationID.Int64
	}
	return p, nil
}

// RotateParkToken rebinds a pending snapshot to a fresh token hash. It touches
// only a still-pending row, so a rotation cannot revive a card the person has
// already answered.
func (s *agentStore) RotateParkToken(ctx context.Context, parkID int64, tokenHash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_park_snapshots SET token_hash = ? WHERE id = ? AND status = ?`,
		tokenHash, parkID, model.ParkPending)
	if err != nil {
		return wrapWriteErr("rotate park token", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

const delegationCols = `id, session_id, workspace_id, parent_tool_call_id, agent_key, mode, fleet_id,
	task, status, attempts, progress, result, error_text, created_at, completed_at`

func (s *agentStore) CreateDelegation(ctx context.Context, d *model.AgentDelegation) error {
	d.CreatedAt = time.Now().UTC()
	if d.Status == "" {
		d.Status = model.DelegationRunning
	}
	if d.Attempts == 0 {
		d.Attempts = 1
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_delegations
		 (session_id, workspace_id, parent_tool_call_id, agent_key, mode, fleet_id, task,
		  status, attempts, progress, result, error_text, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.SessionID, d.WorkspaceID, d.ParentToolCallID, d.AgentKey, d.Mode, d.FleetID,
		nullString(d.Task), d.Status, d.Attempts,
		nullJSON(d.Progress), nullJSON(d.Result), nullString(d.ErrorText), d.CreatedAt)
	if err != nil {
		return wrapWriteErr("create delegation", err)
	}
	d.ID, err = res.LastInsertId()
	return err
}

// RestartDelegation takes a delegation the process died under and puts it back
// to running for another go, counting the attempt. It is atomic and conditional
// on the row still being 'running', so two processes recovering at once cannot
// both claim it, and it refuses past the cap: whatever the task does may be what
// killed us, and a boot loop is worse than a task that did not finish.
func (s *agentStore) RestartDelegation(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_delegations SET attempts = attempts + 1, progress = NULL
		 WHERE id = ? AND status = ? AND attempts < ?`,
		id, model.DelegationRunning, model.MaxDelegationAttempts)
	if err != nil {
		return false, wrapWriteErr("restart delegation", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// UpdateDelegationProgress refreshes the chip's JSON, but only while the
// delegation is still running: a finished one is not overwritten by a late tick.
func (s *agentStore) UpdateDelegationProgress(ctx context.Context, id int64, progress json.RawMessage) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_delegations SET progress = ? WHERE id = ? AND status = ?`,
		nullJSON(progress), id, model.DelegationRunning)
	if err != nil {
		return wrapWriteErr("update delegation progress", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// CompleteDelegation resolves a running delegation to a terminal status. The
// running guard makes it a one-way door: a delegation cannot be completed twice,
// nor completed after it was cancelled.
func (s *agentStore) CompleteDelegation(ctx context.Context, id int64, status string, result json.RawMessage, errText string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_delegations SET status = ?, result = ?, error_text = ?, completed_at = ?
		 WHERE id = ? AND status = ?`,
		status, nullJSON(result), nullString(errText), time.Now().UTC(), id, model.DelegationRunning)
	if err != nil {
		return wrapWriteErr("complete delegation", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *agentStore) RunningDelegations(ctx context.Context, sessionID int64) ([]*model.AgentDelegation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+delegationCols+` FROM agent_delegations
		 WHERE session_id = ? AND status = ? ORDER BY id`,
		sessionID, model.DelegationRunning)
	if err != nil {
		return nil, fmt.Errorf("running delegations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*model.AgentDelegation{}
	for rows.Next() {
		d, err := scanDelegation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SessionDelegations returns every background delegation of a session, in order,
// whatever its status, so the Gateway's runtime-info tool can picture the whole
// set it started (how many running, finished, failed, cancelled).
func (s *agentStore) SessionDelegations(ctx context.Context, sessionID int64) ([]*model.AgentDelegation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+delegationCols+` FROM agent_delegations WHERE session_id = ? ORDER BY id`,
		sessionID)
	if err != nil {
		return nil, fmt.Errorf("session delegations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*model.AgentDelegation{}
	for rows.Next() {
		d, err := scanDelegation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *agentStore) GetDelegation(ctx context.Context, id int64) (*model.AgentDelegation, error) {
	d, err := scanDelegation(s.db.QueryRowContext(ctx,
		`SELECT `+delegationCols+` FROM agent_delegations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return d, err
}

// AllRunningDelegations returns every still-running background delegation across
// all sessions, ordered oldest first. It is the boot-recovery read (KB/27): a
// restart kills the goroutines but not their rows, so the app settles each of
// these and narrates that it did not finish.
func (s *agentStore) AllRunningDelegations(ctx context.Context) ([]*model.AgentDelegation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+delegationCols+` FROM agent_delegations WHERE status = ? ORDER BY id`,
		model.DelegationRunning)
	if err != nil {
		return nil, fmt.Errorf("all running delegations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*model.AgentDelegation{}
	for rows.Next() {
		d, err := scanDelegation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// scanDelegation reads one row, from a *sql.Row or a *sql.Rows.
func scanDelegation(sc interface{ Scan(...any) error }) (*model.AgentDelegation, error) {
	d := &model.AgentDelegation{}
	var progress, result []byte
	var errText, task sql.NullString
	var fleetID sql.NullInt64
	if err := sc.Scan(&d.ID, &d.SessionID, &d.WorkspaceID, &d.ParentToolCallID, &d.AgentKey,
		&d.Mode, &fleetID, &task, &d.Status, &d.Attempts, &progress, &result, &errText,
		&d.CreatedAt, &d.CompletedAt); err != nil {
		return nil, err
	}
	if fleetID.Valid {
		d.FleetID = &fleetID.Int64
	}
	d.Task = task.String
	d.Progress, d.Result, d.ErrorText = json.RawMessage(progress), json.RawMessage(result), errText.String
	return d, nil
}

func nullJSON(v []byte) any {
	if len(v) == 0 {
		return nil
	}
	return v
}

// attachmentsJSON is how the ids of the files sent with a message are stored.
// Nothing attached stores NULL rather than an empty array, so "no files" reads
// the same whether the row predates attachments or simply had none.
func attachmentsJSON(ids []string) any {
	if len(ids) == 0 {
		return nil
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return nil
	}
	return string(raw)
}

// attachmentsOf reads them back. Anything unreadable is treated as none: a step
// whose attachment list cannot be parsed is still a message somebody sent, and
// losing the chips is better than losing the turn.
func attachmentsOf(raw sql.NullString) []string {
	if !raw.Valid || raw.String == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw.String), &ids); err != nil {
		return nil
	}
	return ids
}

// --- fleets --------------------------------------------------------------------

func (s *agentStore) CreateFleet(ctx context.Context, f *model.AgentFleet) error {
	f.CreatedAt = time.Now().UTC()
	if f.Status == "" {
		f.Status = model.FleetRunning
	}
	if f.Deadline.IsZero() {
		// Nothing said how long, which means nobody resolved the members. One
		// default leg is the honest floor: it is what a single agent with no
		// setting of its own gets.
		f.Deadline = f.CreatedAt.Add(model.DefaultBackgroundTimeout)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_fleets
		 (session_id, workspace_id, parent_tool_call_id, model_id, size, status, deadline, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		f.SessionID, f.WorkspaceID, f.ParentToolCallID, nullID(f.ModelID), f.Size, f.Status,
		f.Deadline, f.CreatedAt)
	if err != nil {
		return wrapWriteErr("create fleet", err)
	}
	f.ID, err = res.LastInsertId()
	return err
}

func (s *agentStore) Fleet(ctx context.Context, id int64) (*model.AgentFleet, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+fleetCols+` FROM agent_fleets WHERE id = ?`, id)
	return scanFleet(row)
}

// FleetTally asks the database how many members have finished.
//
// Counting rather than decrementing a column is the whole point: the members
// are already there, so the answer cannot drift, two reports landing together
// both count correctly with no locking between them, and a restart mid-fleet
// has nothing to rebuild because nothing was being held.
func (s *agentStore) FleetTally(ctx context.Context, id int64) (int, int, error) {
	var done, size int
	err := s.db.QueryRowContext(ctx,
		`SELECT
		   (SELECT COUNT(*) FROM agent_delegations
		     WHERE fleet_id = ? AND status IN (?, ?, ?)),
		   (SELECT size FROM agent_fleets WHERE id = ?)`,
		id, model.DelegationDone, model.DelegationFailed, model.DelegationCancelled, id).
		Scan(&done, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, store.ErrNotFound
	}
	if err != nil {
		return 0, 0, fmt.Errorf("tally fleet: %w", err)
	}
	return done, size, nil
}

// CloseFleet is the conditional update that decides who tells the Gateway.
//
// Every member that finishes will see a full tally at about the same moment,
// and exactly one of them must act on it. The WHERE clause is the arbiter: the
// first caller changes the row, everybody else affects nothing and is told so.
func (s *agentStore) CloseFleet(ctx context.Context, id int64, status string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_fleets SET status = ?, completed_at = UTC_TIMESTAMP(3)
		  WHERE id = ? AND status = ?`,
		status, id, model.FleetRunning)
	if err != nil {
		return false, wrapWriteErr("close fleet", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// OverdueFleets returns what has run out of WORKING time.
//
// A batch with a card still waiting on a person is not overdue however long it
// has been, and the NOT EXISTS is that rule. The deadline bounds work; a card
// bounds itself (it expires on its own, after a day by default), and a person at
// lunch must not cost their colleague the answers the other agents came back
// with. Without this a batch could be written off while its card was still on
// screen, and answering it would then re-enter an agent whose row had already
// been settled.
func (s *agentStore) OverdueFleets(ctx context.Context) ([]*model.AgentFleet, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fleetCols+`
		   FROM agent_fleets f
		  WHERE f.status = ? AND f.deadline < UTC_TIMESTAMP(3)
		    AND NOT EXISTS (
		          SELECT 1 FROM agent_park_snapshots p
		           WHERE p.session_id = f.session_id
		             AND p.parent_tool_call_id = f.parent_tool_call_id
		             AND p.status IN (?, ?)
		             AND p.expires_at > UTC_TIMESTAMP(3))
		  ORDER BY f.deadline`, model.FleetRunning, model.ParkPending, model.ParkQueued)
	if err != nil {
		return nil, fmt.Errorf("list overdue fleets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []*model.AgentFleet{}
	for rows.Next() {
		f, err := scanFleet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *agentStore) SessionFleets(ctx context.Context, sessionID int64) ([]*model.AgentFleet, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fleetCols+` FROM agent_fleets WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session fleets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []*model.AgentFleet{}
	for rows.Next() {
		f, err := scanFleet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *agentStore) SettledFleets(ctx context.Context) ([]*model.AgentFleet, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fleetCols+`
		   FROM agent_fleets f
		  WHERE f.status = ?
		    AND (SELECT COUNT(*) FROM agent_delegations d
		          WHERE d.fleet_id = f.id AND d.status IN (?, ?, ?)) >= f.size
		  ORDER BY f.id`,
		model.FleetRunning, model.DelegationDone, model.DelegationFailed, model.DelegationCancelled)
	if err != nil {
		return nil, fmt.Errorf("list settled fleets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []*model.AgentFleet{}
	for rows.Next() {
		f, err := scanFleet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *agentStore) FleetMembers(ctx context.Context, fleetID int64) ([]*model.AgentDelegation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+delegationCols+` FROM agent_delegations WHERE fleet_id = ? ORDER BY id`, fleetID)
	if err != nil {
		return nil, fmt.Errorf("fleet members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*model.AgentDelegation{}
	for rows.Next() {
		d, err := scanDelegation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CreateFleetMembers writes a whole batch's delegations with a multi-row insert.
//
// A thousand agents is a thousand rows, and a round trip each would make
// starting a batch slower than running it. In chunks, so the prepared-statement
// cache holds a couple of shapes rather than one per batch size.
//
// The ids are read back rather than assumed. An auto-increment hands out a block
// for a multi-row insert, and whether that block is consecutive depends on a
// server setting; the rows are already there to be selected, so there is nothing
// to gain by guessing.
func (s *agentStore) CreateFleetMembers(ctx context.Context, fleetID int64, members []*model.AgentDelegation) error {
	if len(members) == 0 {
		return nil
	}
	now := time.Now().UTC()
	for _, m := range members {
		m.CreatedAt = now
		m.FleetID = &fleetID
		if m.Status == "" {
			m.Status = model.DelegationRunning
		}
		if m.Attempts == 0 {
			m.Attempts = 1
		}
	}

	const insert sqldb.Statement = `INSERT INTO agent_delegations
	  (session_id, workspace_id, parent_tool_call_id, agent_key, mode, fleet_id,
	   task, status, attempts, created_at)
	 VALUES `
	const cols = 10

	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		for start := 0; start < len(members); start += sqldb.RowChunk {
			end := start + sqldb.RowChunk
			if end > len(members) {
				end = len(members)
			}
			chunk := members[start:end]
			args := make([]any, 0, len(chunk)*cols)
			for _, m := range chunk {
				args = append(args, m.SessionID, m.WorkspaceID, m.ParentToolCallID,
					m.AgentKey, m.Mode, fleetID, nullString(m.Task), m.Status, m.Attempts, now)
			}
			if _, err := tx.ExecContext(ctx, insert.Rows(len(chunk), cols), args...); err != nil {
				return wrapWriteErr("create fleet members", err)
			}
		}

		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM agent_delegations WHERE fleet_id = ? ORDER BY id`, fleetID)
		if err != nil {
			return fmt.Errorf("read fleet member ids: %w", err)
		}
		defer func() { _ = rows.Close() }()
		i := 0
		for rows.Next() && i < len(members) {
			if err := rows.Scan(&members[i].ID); err != nil {
				return err
			}
			i++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if i != len(members) {
			return fmt.Errorf("create fleet members: wrote %d of %d", i, len(members))
		}
		return nil
	})
}

// AbandonFleetMembers settles what a deadline overtook.
//
// The status is `failed` rather than `cancelled` because nobody cancelled it:
// it was asked for, it was started, and it did not come back, which is a
// failure the Gateway should tell the person about. One statement, so a member
// finishing in the same instant either lands before this (and is left alone by
// the status guard) or after it (and finds itself already terminal).
func (s *agentStore) AbandonFleetMembers(ctx context.Context, fleetID int64, reason string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_delegations
		    SET status = ?, error_text = ?, completed_at = UTC_TIMESTAMP(3)
		  WHERE fleet_id = ? AND status = ?`,
		model.DelegationFailed, truncateError(reason), fleetID, model.DelegationRunning)
	if err != nil {
		return 0, wrapWriteErr("abandon fleet members", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// fleetCols is the one list of a fleet's columns, in the order scanFleet reads
// them, so a query and its scan cannot drift apart.
const fleetCols = `id, session_id, workspace_id, parent_tool_call_id, model_id,
	size, status, deadline, created_at, completed_at`

func scanFleet(row scanner) (*model.AgentFleet, error) {
	var (
		f         model.AgentFleet
		modelID   sql.NullInt64
		completed sql.NullTime
	)
	err := row.Scan(&f.ID, &f.SessionID, &f.WorkspaceID, &f.ParentToolCallID, &modelID,
		&f.Size, &f.Status, &f.Deadline, &f.CreatedAt, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan fleet: %w", err)
	}
	f.ModelID = modelID.Int64
	if completed.Valid {
		f.CompletedAt = &completed.Time
	}
	return &f, nil
}

// nullID writes a zero id as NULL. A foreign key of 0 points at nothing and
// would be refused; "not recorded" is what NULL is for.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
