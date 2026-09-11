package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/store/sqldb"
)

// --- tools -------------------------------------------------------------------

type toolStore struct{ db *sqldb.DB }

const toolColumns = `id, workspace_id, name, kind, template, friendly_name, description,
	input_schema, risk, requires_approval, config, guide, status,
	mcp_server_id, remote_name, definition_hash, remote_missing, definition_changed_at`

// toolColumnsT is the same list qualified for a join. It is spelled out rather
// than derived, because deriving it would mean building a query string at
// runtime, and sqldb does not accept one: the rule holds even when the code
// doing it is ours.
const toolColumnsT = `t.id, t.workspace_id, t.name, t.kind, t.template, t.friendly_name, t.description,
	t.input_schema, t.risk, t.requires_approval, t.config, t.guide, t.status,
	t.mcp_server_id, t.remote_name, t.definition_hash, t.remote_missing, t.definition_changed_at`

// Sync reconciles the workspace with the tools the code offers.
//
// The split is the whole point: the INSERT carries everything, the UPDATE
// carries only the columns the code owns. What a tool is (its schema, its
// description, how dangerous it is) follows the deploy. What the admin decided
// about it (whether it is on, whether it needs a human, who may reach it)
// survives the deploy untouched.
func (s *toolStore) Sync(ctx context.Context, workspaceID int64, tools []*model.Tool) error {
	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		for _, t := range tools {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO tools
					(workspace_id, name, kind, friendly_name, description, input_schema,
					 risk, requires_approval, config, status)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, 'active')
				 ON DUPLICATE KEY UPDATE
					kind = VALUES(kind),
					friendly_name = VALUES(friendly_name),
					description = VALUES(description),
					input_schema = VALUES(input_schema),
					risk = VALUES(risk)`,
				workspaceID, t.Name, t.Kind, t.FriendlyName, t.Description,
				nullJSON(t.InputSchema), t.Risk, t.RequiresApproval); err != nil {
				return fmt.Errorf("sync tool %s: %w", t.Name, err)
			}
		}
		return nil
	})
}

func (s *toolStore) List(ctx context.Context, workspaceID int64) ([]*model.Tool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+toolColumns+` FROM tools WHERE workspace_id = ? ORDER BY name`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	tools, err := scanTools(rows)
	if err != nil {
		return nil, err
	}
	return tools, s.attachGrants(ctx, tools)
}

// ListForUser returns the tools this person may actually reach.
//
// A tool with no grants is open to the workspace, so a workspace that has
// never opened the admin UI still has working tools. The first grant is what
// makes the list exclusive, and from then on membership decides.
func (s *toolStore) ListForUser(ctx context.Context, workspaceID, userID int64) ([]*model.Tool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+toolColumnsT+`
		 FROM tools t
		 WHERE t.workspace_id = ?
		   AND t.status = 'active'
		   AND t.remote_missing = 0
		   AND (
			NOT EXISTS (SELECT 1 FROM tool_grants g WHERE g.tool_id = t.id)
			OR EXISTS (
				SELECT 1 FROM tool_grants g
				JOIN user_group_members m ON m.group_id = g.group_id
				WHERE g.tool_id = t.id AND m.user_id = ?
			)
		   )
		 ORDER BY t.name`, workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("list tools for user: %w", err)
	}
	return scanTools(rows)
}

func (s *toolStore) GetByID(ctx context.Context, workspaceID, id int64) (*model.Tool, error) {
	t, err := scanTool(s.db.QueryRowContext(ctx,
		`SELECT `+toolColumns+` FROM tools WHERE workspace_id = ? AND id = ?`, workspaceID, id))
	if err != nil {
		return nil, err
	}
	return t, s.attachGrants(ctx, []*model.Tool{t})
}

// Update writes the admin-owned columns and replaces the grants. The code-owned
// columns are absent on purpose: a description is not something an admin edits,
// it is something a deploy delivers.
func (s *toolStore) Update(ctx context.Context, t *model.Tool) error {
	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		if err := requireExists(ctx, tx, "update tool",
			`SELECT 1 FROM tools WHERE id = ? AND workspace_id = ?`, t.ID, t.WorkspaceID); err != nil {
			return err
		}
		// requires_approval is code-owned for a builtin (its floor) and set by
		// the MCP sync for a drifted remote tool; the caller decides whether this
		// write may touch it (only an MCP re-trust does). Admin-added confirmation
		// for a builtin or custom tool lives on the agent now (agent_confirm_tools).
		if _, err := tx.ExecContext(ctx,
			`UPDATE tools SET status = ?, requires_approval = ?, config = ?
			 WHERE id = ? AND workspace_id = ?`,
			t.Status, t.RequiresApproval, nullJSON(t.Config), t.ID, t.WorkspaceID); err != nil {
			return fmt.Errorf("update tool: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM tool_grants WHERE tool_id = ?`, t.ID); err != nil {
			return fmt.Errorf("clear tool grants: %w", err)
		}
		for _, groupID := range t.Grants {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO tool_grants (tool_id, group_id) VALUES (?, ?)`, t.ID, groupID); err != nil {
				return fmt.Errorf("grant tool: %w", err)
			}
		}
		return nil
	})
}

// statusOrActive defaults an unset status to active, so a caller that does not
// care gets a working tool.
// delegationModeOrAuto defaults an unset delegation mode to auto, so an older
// caller that never set one keeps leaving the choice to the Gateway.
func delegationModeOrAuto(mode string) string {
	if mode == "" {
		return model.DelegationModeAuto
	}
	return mode
}

func statusOrActive(status string) string {
	if status == "" {
		return model.StatusActive
	}
	return status
}

// CreateCustom inserts a custom tool row: a self-describing tool backed by data,
// not code. Sync never touches it (Sync only upserts the built-in tools it is
// given), so it lives until it is deleted.
func (s *toolStore) CreateCustom(ctx context.Context, t *model.Tool) error {
	// requires_approval defaults to 0: a custom tool is not an MCP projection, so
	// it has no drift lock, and admin-added confirmation now lives on the agent.
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tools
			(workspace_id, name, kind, template, friendly_name, description, input_schema,
			 risk, config, guide, status)
		 VALUES (?, ?, 'custom', ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.WorkspaceID, t.Name, t.Template, t.FriendlyName, t.Description, nullJSON(t.InputSchema),
		t.Risk, nullJSON(t.Config), nullString(t.Guide), statusOrActive(t.Status))
	if err != nil {
		return wrapWriteErr("create custom tool", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("create custom tool: %w", err)
	}
	t.ID = id
	return nil
}

// UpdateCustom rewrites a custom tool's own columns. It refuses anything that is
// not a custom tool, so it can never rewrite a built-in or an MCP row.
func (s *toolStore) UpdateCustom(ctx context.Context, t *model.Tool) error {
	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		// Existence is checked separately: MySQL reports zero affected rows for an
		// UPDATE whose new values equal the old, so an edit that seals the same
		// secrets and changes nothing must not read as "not found".
		if err := requireExists(ctx, tx, "update custom tool",
			`SELECT 1 FROM tools WHERE id = ? AND workspace_id = ? AND kind = 'custom'`,
			t.ID, t.WorkspaceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tools
			 SET friendly_name = ?, description = ?, input_schema = ?, risk = ?, config = ?, guide = ?
			 WHERE id = ? AND workspace_id = ? AND kind = 'custom'`,
			t.FriendlyName, t.Description, nullJSON(t.InputSchema), t.Risk, nullJSON(t.Config), nullString(t.Guide),
			t.ID, t.WorkspaceID); err != nil {
			return wrapWriteErr("update custom tool", err)
		}
		return nil
	})
}

// Delete removes a custom tool. It refuses a non-custom tool: a built-in or a
// projected MCP tool is not something to delete by id.
func (s *toolStore) Delete(ctx context.Context, workspaceID, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM tools WHERE id = ? AND workspace_id = ? AND kind = 'custom'`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("delete custom tool: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListByMCPServer returns one connection's projection, the missing included:
// the console shows what drifted, so nothing about the registry mutates
// silently.
func (s *toolStore) ListByMCPServer(ctx context.Context, workspaceID, serverID int64) ([]*model.Tool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+toolColumns+` FROM tools
		 WHERE workspace_id = ? AND mcp_server_id = ? ORDER BY name`, workspaceID, serverID)
	if err != nil {
		return nil, fmt.Errorf("list mcp tools: %w", err)
	}
	tools, err := scanTools(rows)
	if err != nil {
		return nil, err
	}
	return tools, s.attachGrants(ctx, tools)
}

// SyncMCPTools reconciles one connection's projection with what the remote
// offered, with the drift semantics the projection promises:
//
//   - a NEW tool arrives granted to no agent: the allow-lists never heard of
//     it, so nothing gains a capability because a remote shipped one. That is
//     what makes it inert, and it needs no second answer;
//   - a tool the remote no longer offers is FLAGGED missing, never deleted:
//     one flaky sync against a rebooting server must not wipe an
//     administrator's grants;
//   - a tool whose definition hash changed keeps its grants and is STAMPED
//     with when it changed, which the console shows.
//
// It does not touch requires_approval. Whether a call pauses for a person is
// the agent's decision (its confirm set) and, for a dangerous built-in, the
// code's; a projected tool used to carry a third answer that the sync set and
// only a checkbox could clear. It read as a guard against a service redefining
// a tool under us and could not be one, because nothing syncs on its own: this
// runs when somebody presses refresh. What it reliably was is an onboarding
// step, in the same words as the agent setting that does work (migration 57).
//
// The stamp stays, and so does the check that refuses a PARKED approval whose
// tool changed while the card was waiting (agent/tools.go): that one is about a
// specific approval a specific person gave, and it is exact.
func (s *toolStore) SyncMCPTools(ctx context.Context, workspaceID, serverID int64, offered []*model.Tool) (model.MCPSyncResult, error) {
	result := model.MCPSyncResult{Offered: len(offered)}
	err := s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT `+toolColumns+` FROM tools
			 WHERE workspace_id = ? AND mcp_server_id = ? FOR UPDATE`, workspaceID, serverID)
		if err != nil {
			return fmt.Errorf("load projection: %w", err)
		}
		existing, err := scanTools(rows)
		if err != nil {
			return err
		}
		byRemote := make(map[string]*model.Tool, len(existing))
		for _, t := range existing {
			byRemote[t.RemoteName] = t
		}

		now := time.Now().UTC()
		seen := make(map[string]bool, len(offered))
		for _, t := range offered {
			seen[t.RemoteName] = true
			prev, known := byRemote[t.RemoteName]
			if !known {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO tools
						(workspace_id, name, kind, friendly_name, description, input_schema,
						 risk, requires_approval, config, status,
						 mcp_server_id, remote_name, definition_hash, remote_missing)
					 VALUES (?, ?, 'mcp', ?, ?, ?, ?, 0, NULL, 'active', ?, ?, ?, 0)`,
					workspaceID, t.Name, t.FriendlyName, t.Description, nullJSON(t.InputSchema),
					t.Risk, serverID, t.RemoteName, t.DefinitionHash); err != nil {
					return fmt.Errorf("project tool %s: %w", t.Name, err)
				}
				result.Added++
				continue
			}

			changed := prev.DefinitionHash != t.DefinitionHash
			if changed {
				result.Changed++
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE tools SET
					friendly_name = ?, description = ?, input_schema = ?,
					definition_hash = ?, remote_missing = 0,
					definition_changed_at = IF(?, ?, definition_changed_at)
				 WHERE id = ?`,
				t.FriendlyName, t.Description, nullJSON(t.InputSchema),
				t.DefinitionHash, changed, now, prev.ID); err != nil {
				return fmt.Errorf("refresh tool %s: %w", t.Name, err)
			}
		}

		for _, prev := range existing {
			if seen[prev.RemoteName] {
				continue
			}
			result.Missing++
			if prev.RemoteMissing {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE tools SET remote_missing = 1 WHERE id = ?`, prev.ID); err != nil {
				return fmt.Errorf("flag missing tool %s: %w", prev.Name, err)
			}
		}
		return nil
	})
	if err != nil {
		return model.MCPSyncResult{}, err
	}
	return result, nil
}

// attachGrants fills the grant lists for the given tools in one query, rather
// than one per tool.
func (s *toolStore) attachGrants(ctx context.Context, tools []*model.Tool) error {
	if len(tools) == 0 {
		return nil
	}
	byID := make(map[int64]*model.Tool, len(tools))
	for _, t := range tools {
		t.Grants = []int64{}
		byID[t.ID] = t
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT g.tool_id, g.group_id
		 FROM tool_grants g
		 JOIN tools t ON t.id = g.tool_id
		 WHERE t.workspace_id = ?`, tools[0].WorkspaceID)
	if err != nil {
		return fmt.Errorf("list tool grants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var toolID, groupID int64
		if err := rows.Scan(&toolID, &groupID); err != nil {
			return fmt.Errorf("scan tool grant: %w", err)
		}
		if t, ok := byID[toolID]; ok {
			t.Grants = append(t.Grants, groupID)
		}
	}
	return rows.Err()
}

func scanTools(rows *sql.Rows) ([]*model.Tool, error) {
	defer func() { _ = rows.Close() }()

	tools := []*model.Tool{}
	for rows.Next() {
		t, err := scanToolFields(rows.Scan)
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	return tools, rows.Err()
}

func scanTool(row *sql.Row) (*model.Tool, error) {
	return scanToolFields(row.Scan)
}

// scanToolFields reads one tool row through any Scan, so the row shape lives
// in exactly one place.
func scanToolFields(scan func(dest ...any) error) (*model.Tool, error) {
	t := &model.Tool{}
	var schema, config []byte
	var template, guide sql.NullString
	var serverID sql.NullInt64
	var remoteName, definitionHash sql.NullString
	var changedAt sql.NullTime
	err := scan(&t.ID, &t.WorkspaceID, &t.Name, &t.Kind, &template, &t.FriendlyName,
		&t.Description, &schema, &t.Risk, &t.RequiresApproval, &config, &guide, &t.Status,
		&serverID, &remoteName, &definitionHash, &t.RemoteMissing, &changedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan tool: %w", err)
	}
	t.InputSchema, t.Config = json.RawMessage(schema), json.RawMessage(config)
	t.Template = template.String
	t.Guide = guide.String
	if serverID.Valid {
		t.MCPServerID = &serverID.Int64
	}
	t.RemoteName, t.DefinitionHash = remoteName.String, definitionHash.String
	if changedAt.Valid {
		at := changedAt.Time
		t.DefinitionChangedAt = &at
	}
	t.Grants = []int64{}
	return t, nil
}

// --- agents ------------------------------------------------------------------

type agentConfigStore struct{ db *sqldb.DB }

const agentColumns = `id, workspace_id, agent_key, name, instructions, model_id,
	memory_brain_id, audio_model_id, reasoning, settings, approval_ttl_seconds, max_iterations, max_fleet_agents, background_timeout_seconds,
	status, delegation_mode, created_at, updated_at`

func (s *agentConfigStore) Create(ctx context.Context, a *model.Agent) error {
	now := time.Now().UTC()
	a.CreatedAt, a.UpdatedAt = now, now
	if a.Status == "" {
		a.Status = model.StatusActive
	}

	settings, err := a.Settings.Marshal()
	if err != nil {
		return fmt.Errorf("insert agent: %w", err)
	}

	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		// The key is internal. When the caller leaves it blank (a new agent),
		// derive a unique one from the name here, inside the transaction, so the
		// check and the insert cannot race a duplicate through.
		if a.Key == "" {
			key, err := uniqueAgentKey(ctx, tx, a.WorkspaceID, a.Name)
			if err != nil {
				return err
			}
			a.Key = key
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO agents (workspace_id, agent_key, name, instructions, model_id,
				memory_brain_id, audio_model_id, reasoning, settings, approval_ttl_seconds, max_iterations, max_fleet_agents, background_timeout_seconds,
				status, delegation_mode, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.WorkspaceID, a.Key, a.Name, a.Instructions, a.ModelID, a.MemoryBrainID, a.AudioModelID,
			a.Reasoning, nullBytes(settings), approvalSeconds(a.ApprovalTTL), nullableInt(a.MaxIterations),
			nullableInt(a.MaxFleetAgents), durationSeconds(a.BackgroundTimeout),
			a.Status, delegationModeOrAuto(a.DelegationMode), a.CreatedAt, a.UpdatedAt)
		if err != nil {
			return wrapWriteErr("insert agent", err)
		}
		if a.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		if err := setAgentTools(ctx, tx, a.WorkspaceID, a.ID, a.Tools); err != nil {
			return err
		}
		if err := setAgentConfirmTools(ctx, tx, a.WorkspaceID, a.ID, a.ConfirmTools); err != nil {
			return err
		}
		if err := setAgentBrains(ctx, tx, a.WorkspaceID, a.ID, a.Brains); err != nil {
			return err
		}
		return setGatewayFileRules(ctx, tx, a.WorkspaceID, a.ID, a.FileRules)
	})
}

func (s *agentConfigStore) GetByID(ctx context.Context, workspaceID, id int64) (*model.Agent, error) {
	a, err := scanAgent(s.db.QueryRowContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE workspace_id = ? AND id = ?`, workspaceID, id))
	if err != nil {
		return nil, err
	}
	return a, s.attachAgentTools(ctx, a)
}

func (s *agentConfigStore) GetByKey(ctx context.Context, workspaceID int64, key string) (*model.Agent, error) {
	a, err := scanAgent(s.db.QueryRowContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE workspace_id = ? AND agent_key = ?`, workspaceID, key))
	if err != nil {
		return nil, err
	}
	return a, s.attachAgentTools(ctx, a)
}

func (s *agentConfigStore) List(ctx context.Context, workspaceID int64) ([]*model.Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE workspace_id = ? ORDER BY agent_key`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	agents := []*model.Agent{}
	for rows.Next() {
		a, err := scanAgentRow(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, a := range agents {
		if err := s.attachAgentTools(ctx, a); err != nil {
			return nil, err
		}
	}
	return agents, nil
}

func (s *agentConfigStore) Update(ctx context.Context, a *model.Agent) error {
	settings, err := a.Settings.Marshal()
	if err != nil {
		return fmt.Errorf("update agent: %w", err)
	}
	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		if err := requireExists(ctx, tx, "update agent",
			`SELECT 1 FROM agents WHERE id = ? AND workspace_id = ?`, a.ID, a.WorkspaceID); err != nil {
			return err
		}
		a.UpdatedAt = time.Now().UTC()
		if _, err := tx.ExecContext(ctx,
			`UPDATE agents SET agent_key = ?, name = ?, instructions = ?, model_id = ?,
				memory_brain_id = ?, audio_model_id = ?, reasoning = ?, settings = ?, approval_ttl_seconds = ?, max_iterations = ?,
				max_fleet_agents = ?,
				background_timeout_seconds = ?, status = ?, delegation_mode = ?, updated_at = ?
			 WHERE id = ? AND workspace_id = ?`,
			a.Key, a.Name, a.Instructions, a.ModelID, a.MemoryBrainID, a.AudioModelID, a.Reasoning,
			nullBytes(settings), approvalSeconds(a.ApprovalTTL), nullableInt(a.MaxIterations), nullableInt(a.MaxFleetAgents),
			durationSeconds(a.BackgroundTimeout),
			a.Status, delegationModeOrAuto(a.DelegationMode), a.UpdatedAt,
			a.ID, a.WorkspaceID); err != nil {
			return wrapWriteErr("update agent", err)
		}
		if err := setAgentTools(ctx, tx, a.WorkspaceID, a.ID, a.Tools); err != nil {
			return err
		}
		if err := setAgentConfirmTools(ctx, tx, a.WorkspaceID, a.ID, a.ConfirmTools); err != nil {
			return err
		}
		if err := setAgentBrains(ctx, tx, a.WorkspaceID, a.ID, a.Brains); err != nil {
			return err
		}
		return setGatewayFileRules(ctx, tx, a.WorkspaceID, a.ID, a.FileRules)
	})
}

func (s *agentConfigStore) Delete(ctx context.Context, workspaceID, id int64) error {
	// The allowed-tool rows go with it. The CONVERSATIONS it shaped do not:
	// they merely forget which agent shaped them (the session's agent_id becomes
	// null), because deleting an assistant must not delete what people said to
	// it.
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM agents WHERE id = ? AND workspace_id = ?`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	return requireAffected(res, "delete agent")
}

// setAgentTools replaces the agent's tool list. The names are resolved against
// the workspace's own tools, so an agent cannot be pointed at a tool from
// another workspace, and a name that does not exist is simply not stored: the
// allow-list is configuration, and configuration may name a tool a deploy has
// not delivered yet.
//
// A nil list means the agent has no opinion and inherits the layer below, so
// there is nothing to write. That is why nil and empty differ here.
func setAgentTools(ctx context.Context, tx *sqldb.Tx, workspaceID, agentID int64, names []string) error {
	if names == nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM agent_allowed_tools WHERE agent_id = ?`, agentID); err != nil {
		return fmt.Errorf("clear agent tools: %w", err)
	}
	for _, name := range names {
		if _, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO agent_allowed_tools (agent_id, tool_id)
			 SELECT ?, id FROM tools WHERE workspace_id = ? AND name = ?`,
			agentID, workspaceID, name); err != nil {
			return fmt.Errorf("allow tool %s: %w", name, err)
		}
	}
	return nil
}

// setAgentConfirmTools replaces the agent's set of tools that require
// confirmation. It mirrors setAgentTools (same workspace-scoped name
// resolution, same nil-means-inherit rule) into its own join table. It does
// not itself check that a confirmed tool is one the agent is allowed: that is
// the API's validation, and the resolver only honours confirmation on a tool
// the agent actually loaded, so a stray row is inert rather than dangerous.
func setAgentConfirmTools(ctx context.Context, tx *sqldb.Tx, workspaceID, agentID int64, names []string) error {
	if names == nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM agent_confirm_tools WHERE agent_id = ?`, agentID); err != nil {
		return fmt.Errorf("clear agent confirm tools: %w", err)
	}
	for _, name := range names {
		if _, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO agent_confirm_tools (agent_id, tool_id)
			 SELECT ?, id FROM tools WHERE workspace_id = ? AND name = ?`,
			agentID, workspaceID, name); err != nil {
			return fmt.Errorf("confirm tool %s: %w", name, err)
		}
	}
	return nil
}

// uniqueAgentKey derives a stable, unique key from an agent's name. It slugifies
// the name, refuses to mint the reserved "default" (that key is the Gateway's
// alone), and appends -2, -3, ... until the key is free in the workspace. The
// caller runs it inside the create transaction, so the free key it finds is
// still free at insert.
func uniqueAgentKey(ctx context.Context, tx *sqldb.Tx, workspaceID int64, name string) (string, error) {
	base := model.Slugify(name)
	if base == "" {
		base = "agent"
	}
	if base == model.DefaultAgentKey {
		base = "agent-" + base
	}
	key := base
	for i := 2; ; i++ {
		var one int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM agents WHERE workspace_id = ? AND agent_key = ?`,
			workspaceID, key).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return key, nil
		}
		if err != nil {
			return "", fmt.Errorf("check agent key: %w", err)
		}
		key = fmt.Sprintf("%s-%d", base, i)
	}
}

// setAgentBrains replaces the brains an agent may read. The INSERT ... SELECT is
// the workspace check, so a brain from another workspace is silently not
// written rather than trusted. A nil slice means no opinion and writes nothing.
func setAgentBrains(ctx context.Context, tx *sqldb.Tx, workspaceID, agentID int64, brainIDs []int64) error {
	if brainIDs == nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM agent_brains WHERE agent_id = ?`, agentID); err != nil {
		return fmt.Errorf("clear agent brains: %w", err)
	}
	for _, brainID := range brainIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO agent_brains (agent_id, brain_id)
			 SELECT ?, id FROM brains WHERE id = ? AND workspace_id = ?`,
			agentID, brainID, workspaceID); err != nil {
			return fmt.Errorf("assign brain %d: %w", brainID, err)
		}
	}
	return nil
}

// setGatewayFileRules replaces the Gateway's file routing. The INSERT ...
// SELECT is the workspace check, so a model from another workspace is silently
// not written rather than trusted, exactly as a brain is.
//
// Order is the rule: they are tried in the order they were given and the first
// match wins, so `position` is the index rather than anything the caller
// chooses. A nil slice means no opinion and writes nothing; an empty slice
// clears the lot, which is how an administrator stops accepting files.
func setGatewayFileRules(ctx context.Context, tx *sqldb.Tx, workspaceID, agentID int64, rules []model.FileRule) error {
	if rules == nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM gateway_file_rules WHERE agent_id = ?`, agentID); err != nil {
		return fmt.Errorf("clear file rules: %w", err)
	}
	for i, rule := range rules {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO gateway_file_rules (agent_id, position, file_types, model_id)
			 SELECT ?, ?, ?, id FROM ai_models WHERE id = ? AND workspace_id = ?`,
			agentID, i, strings.Join(rule.Types, ","), rule.ModelID, workspaceID); err != nil {
			return fmt.Errorf("write file rule %d: %w", i, err)
		}
	}
	return nil
}

// attachAgentTools fills what a loaded agent carries beyond its own row: the
// tools it may call, which of those it must confirm, and the brains it may
// read. They are read together so a caller never has half an agent.
func (s *agentConfigStore) attachAgentTools(ctx context.Context, a *model.Agent) error {
	if err := s.attachTools(ctx, a); err != nil {
		return err
	}
	if err := s.attachConfirmTools(ctx, a); err != nil {
		return err
	}
	if err := s.attachBrains(ctx, a); err != nil {
		return err
	}
	return s.attachFileRules(ctx, a)
}

// attachFileRules fills the Gateway's file routing, in the order it is applied.
// Ids and type lists, not models: the form prefills a picker with them, and a
// turn resolves the real model when it has a file to read.
func (s *agentConfigStore) attachFileRules(ctx context.Context, a *model.Agent) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT file_types, model_id FROM gateway_file_rules WHERE agent_id = ? ORDER BY position`, a.ID)
	if err != nil {
		return fmt.Errorf("list file rules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []model.FileRule{}
	for rows.Next() {
		var types string
		var id int64
		if err := rows.Scan(&types, &id); err != nil {
			return fmt.Errorf("scan file rule: %w", err)
		}
		rule := model.FileRule{ModelID: id, Types: []string{}}
		if types != "" {
			rule.Types = strings.Split(types, ",")
		}
		out = append(out, rule)
	}
	a.FileRules = out
	return rows.Err()
}

// attachBrains fills the ids of the brains this agent may read. Ids, not the
// brains themselves: the config form prefills a picker with them, while the tool
// loadout reads the full brains through BrainStore.AgentBrains.
func (s *agentConfigStore) attachBrains(ctx context.Context, a *model.Agent) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT brain_id FROM agent_brains WHERE agent_id = ? ORDER BY brain_id`, a.ID)
	if err != nil {
		return fmt.Errorf("list agent brains: %w", err)
	}
	defer func() { _ = rows.Close() }()

	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan agent brain: %w", err)
		}
		ids = append(ids, id)
	}
	a.Brains = ids
	return rows.Err()
}

func (s *agentConfigStore) attachTools(ctx context.Context, a *model.Agent) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.name FROM agent_allowed_tools at
		 JOIN tools t ON t.id = at.tool_id
		 WHERE at.agent_id = ? ORDER BY t.name`, a.ID)
	if err != nil {
		return fmt.Errorf("list agent tools: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// A configured agent's tools are EXPLICIT: the set as stored is the set it
	// gets, and no rows means it deliberately has none, not "inherit the code
	// defaults". So this returns an empty, non-nil slice for zero rows. The
	// resolver honours it (applyDefaultAgent overrides on non-nil), which is why
	// a main agent with nothing selected reaches the model with nothing selected,
	// rather than the default tools leaking in through a nil read.
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan agent tool: %w", err)
		}
		names = append(names, name)
	}
	a.Tools = names
	return rows.Err()
}

// attachConfirmTools fills the set of the agent's tools that require
// confirmation. Unlike Tools there is no inherit semantics to preserve, so an
// agent with no rows simply confirms nothing: the field is the set as stored.
func (s *agentConfigStore) attachConfirmTools(ctx context.Context, a *model.Agent) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.name FROM agent_confirm_tools ct
		 JOIN tools t ON t.id = ct.tool_id
		 WHERE ct.agent_id = ? ORDER BY t.name`, a.ID)
	if err != nil {
		return fmt.Errorf("list agent confirm tools: %w", err)
	}
	defer func() { _ = rows.Close() }()

	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan agent confirm tool: %w", err)
		}
		names = append(names, name)
	}
	a.ConfirmTools = names
	return rows.Err()
}

// approvalSeconds stores a window as whole seconds, or NULL when the agent has
// no opinion and inherits the deployment's.
func approvalSeconds(ttl *time.Duration) any {
	if ttl == nil {
		return nil
	}
	return int64(ttl.Seconds())
}

// durationSeconds stores an optional duration as whole seconds, NULL when unset.
func durationSeconds(d *time.Duration) any {
	if d == nil {
		return nil
	}
	return int64(d.Seconds())
}

// nullableInt stores an optional int, NULL when unset.
func nullableInt(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}

func scanAgent(row *sql.Row) (*model.Agent, error) {
	a := &model.Agent{}
	var instructions sql.NullString
	var modelID, memoryBrainID, audioModelID, approvalTTL, maxIter, maxFleet, bgTimeout sql.NullInt64
	var settings []byte
	err := row.Scan(&a.ID, &a.WorkspaceID, &a.Key, &a.Name, &instructions, &modelID,
		&memoryBrainID, &audioModelID, &a.Reasoning, &settings, &approvalTTL, &maxIter, &maxFleet, &bgTimeout, &a.Status, &a.DelegationMode, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan agent: %w", err)
	}
	a.Settings = model.ParseSettings(settings)
	fillAgent(a, instructions, modelID, memoryBrainID, audioModelID, approvalTTL, maxIter, maxFleet, bgTimeout)
	return a, nil
}

func scanAgentRow(rows *sql.Rows) (*model.Agent, error) {
	a := &model.Agent{}
	var instructions sql.NullString
	var modelID, memoryBrainID, audioModelID, approvalTTL, maxIter, maxFleet, bgTimeout sql.NullInt64
	var settings []byte
	if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.Key, &a.Name, &instructions, &modelID,
		&memoryBrainID, &audioModelID, &a.Reasoning, &settings, &approvalTTL, &maxIter, &maxFleet, &bgTimeout, &a.Status, &a.DelegationMode, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, fmt.Errorf("scan agent: %w", err)
	}
	a.Settings = model.ParseSettings(settings)
	fillAgent(a, instructions, modelID, memoryBrainID, audioModelID, approvalTTL, maxIter, maxFleet, bgTimeout)
	return a, nil
}

func fillAgent(a *model.Agent, instructions sql.NullString, modelID, memoryBrainID, audioModelID, approvalTTL, maxIter, maxFleet, bgTimeout sql.NullInt64) {
	a.Instructions = instructions.String
	if modelID.Valid {
		a.ModelID = &modelID.Int64
	}
	if memoryBrainID.Valid {
		a.MemoryBrainID = &memoryBrainID.Int64
	}
	if audioModelID.Valid {
		a.AudioModelID = &audioModelID.Int64
	}
	if approvalTTL.Valid {
		ttl := time.Duration(approvalTTL.Int64) * time.Second
		a.ApprovalTTL = &ttl
	}
	if maxIter.Valid {
		n := int(maxIter.Int64)
		a.MaxIterations = &n
	}
	if maxFleet.Valid {
		n := int(maxFleet.Int64)
		a.MaxFleetAgents = &n
	}
	if bgTimeout.Valid {
		d := time.Duration(bgTimeout.Int64) * time.Second
		a.BackgroundTimeout = &d
	}
}

// --- workflows ---------------------------------------------------------------

type workflowStore struct{ db *sqldb.DB }

const workflowColumns = `id, workspace_id, name, status, created_by, created_at, updated_at`

func (s *workflowStore) Create(ctx context.Context, w *model.Workflow) error {
	now := time.Now().UTC()
	w.CreatedAt, w.UpdatedAt = now, now
	if w.Status == "" {
		w.Status = model.WorkflowDraft
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO workflows (workspace_id, name, status, created_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		w.WorkspaceID, w.Name, w.Status, w.CreatedBy, w.CreatedAt, w.UpdatedAt)
	if err != nil {
		return wrapWriteErr("insert workflow", err)
	}
	w.ID, err = res.LastInsertId()
	return err
}

func (s *workflowStore) GetByID(ctx context.Context, workspaceID, id int64) (*model.Workflow, error) {
	w := &model.Workflow{}
	err := s.db.QueryRowContext(ctx,
		`SELECT `+workflowColumns+` FROM workflows WHERE workspace_id = ? AND id = ?`,
		workspaceID, id).
		Scan(&w.ID, &w.WorkspaceID, &w.Name, &w.Status, &w.CreatedBy, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan workflow: %w", err)
	}
	return w, nil
}

func (s *workflowStore) List(ctx context.Context, workspaceID int64) ([]*model.Workflow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+workflowColumns+` FROM workflows WHERE workspace_id = ? ORDER BY id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	workflows := []*model.Workflow{}
	for rows.Next() {
		w := &model.Workflow{}
		if err := rows.Scan(&w.ID, &w.WorkspaceID, &w.Name, &w.Status,
			&w.CreatedBy, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan workflow: %w", err)
		}
		workflows = append(workflows, w)
	}
	return workflows, rows.Err()
}

func (s *workflowStore) Update(ctx context.Context, w *model.Workflow) error {
	if err := requireExists(ctx, s.db, "update workflow",
		`SELECT 1 FROM workflows WHERE id = ? AND workspace_id = ?`, w.ID, w.WorkspaceID); err != nil {
		return err
	}
	w.UpdatedAt = time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE workflows SET name = ?, status = ?, updated_at = ?
		 WHERE id = ? AND workspace_id = ?`,
		w.Name, w.Status, w.UpdatedAt, w.ID, w.WorkspaceID); err != nil {
		return wrapWriteErr("update workflow", err)
	}
	return nil
}

func (s *workflowStore) Delete(ctx context.Context, workspaceID, id int64) error {
	// The versions and the conditions go with it: neither is a thing that means
	// anything without the workflow it belongs to.
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM workflows WHERE id = ? AND workspace_id = ?`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("delete workflow: %w", err)
	}
	return requireAffected(res, "delete workflow")
}

// CreateVersion appends a version, numbering it under the workflow's own row
// lock. Two administrators saving at the same instant get version 4 and
// version 5, never two version 4s: the number is derived inside the
// transaction that holds the workflow, not read beforehand.
func (s *workflowStore) CreateVersion(ctx context.Context, workspaceID int64, v *model.WorkflowVersion) error {
	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		var locked int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM workflows WHERE id = ? AND workspace_id = ? FOR UPDATE`,
			v.WorkflowID, workspaceID).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock workflow: %w", err)
		}

		var next sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT MAX(version) FROM workflow_versions WHERE workflow_id = ?`,
			v.WorkflowID).Scan(&next); err != nil {
			return fmt.Errorf("next workflow version: %w", err)
		}
		v.Version = int(next.Int64) + 1
		v.CreatedAt = time.Now().UTC()

		res, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_versions (workflow_id, version, definition, is_published, created_by, created_at)
			 VALUES (?, ?, ?, 0, ?, ?)`,
			v.WorkflowID, v.Version, []byte(v.Definition), v.CreatedBy, v.CreatedAt)
		if err != nil {
			return wrapWriteErr("insert workflow version", err)
		}
		v.ID, err = res.LastInsertId()
		v.IsPublished = false
		return err
	})
}

func (s *workflowStore) ListVersions(ctx context.Context, workspaceID, workflowID int64) ([]*model.WorkflowVersion, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT v.id, v.workflow_id, v.version, v.definition, v.is_published, v.created_by, v.created_at
		 FROM workflow_versions v
		 JOIN workflows w ON w.id = v.workflow_id
		 WHERE w.workspace_id = ? AND v.workflow_id = ?
		 ORDER BY v.version DESC`, workspaceID, workflowID)
	if err != nil {
		return nil, fmt.Errorf("list workflow versions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	versions := []*model.WorkflowVersion{}
	for rows.Next() {
		v := &model.WorkflowVersion{}
		var definition []byte
		if err := rows.Scan(&v.ID, &v.WorkflowID, &v.Version, &definition,
			&v.IsPublished, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan workflow version: %w", err)
		}
		v.Definition = json.RawMessage(definition)
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

// Publish swaps the live version. Demote-then-promote runs in one transaction,
// so there is never an instant with two published versions of one workflow, nor
// an instant with none: a turn that lands mid-publish sees one or the other,
// and both are complete configurations.
func (s *workflowStore) Publish(ctx context.Context, workspaceID, workflowID, versionID int64) error {
	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		if err := requireExists(ctx, tx, "publish workflow",
			`SELECT 1 FROM workflow_versions v
			 JOIN workflows w ON w.id = v.workflow_id
			 WHERE v.id = ? AND v.workflow_id = ? AND w.workspace_id = ?`,
			versionID, workflowID, workspaceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE workflow_versions SET is_published = 0 WHERE workflow_id = ?`, workflowID); err != nil {
			return fmt.Errorf("demote workflow versions: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE workflow_versions SET is_published = 1 WHERE id = ?`, versionID); err != nil {
			return fmt.Errorf("publish workflow version: %w", err)
		}
		// A workflow with a published version is published: the two states
		// cannot drift apart, because nothing else is allowed to set them.
		if _, err := tx.ExecContext(ctx,
			`UPDATE workflows SET status = ?, updated_at = ? WHERE id = ? AND workspace_id = ?`,
			model.WorkflowPublished, time.Now().UTC(), workflowID, workspaceID); err != nil {
			return fmt.Errorf("publish workflow: %w", err)
		}
		return nil
	})
}

func (s *workflowStore) SetAssignments(ctx context.Context, workspaceID, workflowID int64, assignments []model.WorkflowAssignment) error {
	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		if err := requireExists(ctx, tx, "set assignments",
			`SELECT 1 FROM workflows WHERE id = ? AND workspace_id = ?`,
			workflowID, workspaceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM workflow_assignments WHERE workflow_id = ?`, workflowID); err != nil {
			return fmt.Errorf("clear assignments: %w", err)
		}
		for _, a := range assignments {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO workflow_assignments (workflow_id, match_type, match_value, priority)
				 VALUES (?, ?, ?, ?)`,
				workflowID, a.MatchType, a.MatchValue, a.Priority); err != nil {
				return fmt.Errorf("insert assignment: %w", err)
			}
		}
		return nil
	})
}

func (s *workflowStore) ListAssignments(ctx context.Context, workspaceID, workflowID int64) ([]model.WorkflowAssignment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, a.workflow_id, a.match_type, a.match_value, a.priority
		 FROM workflow_assignments a
		 JOIN workflows w ON w.id = a.workflow_id
		 WHERE w.workspace_id = ? AND a.workflow_id = ?
		 ORDER BY a.id`, workspaceID, workflowID)
	if err != nil {
		return nil, fmt.Errorf("list assignments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	assignments := []model.WorkflowAssignment{}
	for rows.Next() {
		var a model.WorkflowAssignment
		if err := rows.Scan(&a.ID, &a.WorkflowID, &a.MatchType, &a.MatchValue, &a.Priority); err != nil {
			return nil, fmt.Errorf("scan assignment: %w", err)
		}
		assignments = append(assignments, a)
	}
	return assignments, rows.Err()
}

// Candidates loads every workflow that could shape a turn: published, with a
// published version, and with at least one condition. It runs on every message,
// so it is one query and one pass, and the matching itself happens in Go where
// the rules are readable.
func (s *workflowStore) Candidates(ctx context.Context, workspaceID int64) ([]*model.WorkflowCandidate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT w.id, v.id, v.definition, a.id, a.match_type, a.match_value, a.priority
		 FROM workflows w
		 JOIN workflow_versions v ON v.workflow_id = w.id AND v.is_published = 1
		 JOIN workflow_assignments a ON a.workflow_id = w.id
		 WHERE w.workspace_id = ? AND w.status = ?
		 ORDER BY w.id, a.id`, workspaceID, model.WorkflowPublished)
	if err != nil {
		return nil, fmt.Errorf("list workflow candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	candidates := []*model.WorkflowCandidate{}
	byWorkflow := map[int64]*model.WorkflowCandidate{}
	for rows.Next() {
		var workflowID, versionID int64
		var definition []byte
		var a model.WorkflowAssignment
		if err := rows.Scan(&workflowID, &versionID, &definition,
			&a.ID, &a.MatchType, &a.MatchValue, &a.Priority); err != nil {
			return nil, fmt.Errorf("scan workflow candidate: %w", err)
		}
		a.WorkflowID = workflowID

		c, ok := byWorkflow[workflowID]
		if !ok {
			c = &model.WorkflowCandidate{
				WorkflowID: workflowID,
				VersionID:  versionID,
				Definition: json.RawMessage(definition),
			}
			byWorkflow[workflowID] = c
			candidates = append(candidates, c)
		}
		c.Assignments = append(c.Assignments, a)
	}
	return candidates, rows.Err()
}
