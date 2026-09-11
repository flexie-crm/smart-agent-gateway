-- Referential integrity: the database enforces the shape of the data.
--
-- Until now every relationship in this schema was a promise the application made
-- to itself. Deleting a conversation deleted its steps because a function
-- remembered to; deleting a workspace left its tools, agents and workflows
-- behind because nothing remembered at all. That is not integrity, it is
-- discipline, and discipline is exactly what an invariant should not depend on:
-- one forgotten DELETE, one new code path, one hand-run query, and the data is
-- quietly wrong in a way nothing detects.
--
-- So every edge in the graph gets a rule, and the rule says what the
-- relationship MEANS:
--
--   CASCADE   the child is PART OF the parent. A step is part of a conversation;
--             a conversation is part of a workspace. When the parent goes, the
--             child was never a separate thing to keep.
--
--   SET NULL  the child REFERS TO the parent and outlives it. A conversation
--             records which agent shaped it; deleting that agent must not delete
--             the conversation, it must only forget the attribution.
--
--   (none)    the row is a HISTORICAL RECORD of something that happened. An
--             audit line describes a past event and must survive the deletion of
--             everything it describes, or it is not an audit trail.
--
-- Orphans are deleted first, because a constraint cannot be added over data that
-- already violates it, and a row pointing at a parent that does not exist is
-- corruption whatever we call it.

-- +goose Up

-- Rows whose parent is already gone. On a schema that never had constraints,
-- these are the damage that had already been done.
DELETE m FROM user_group_members m LEFT JOIN users u ON u.id = m.user_id WHERE u.id IS NULL;
DELETE m FROM user_group_members m LEFT JOIN user_groups_def g ON g.id = m.group_id WHERE g.id IS NULL;
DELETE r FROM group_roles r LEFT JOIN user_groups_def g ON g.id = r.group_id WHERE g.id IS NULL;
DELETE r FROM group_roles r LEFT JOIN roles ro ON ro.id = r.role_id WHERE ro.id IS NULL;
DELETE p FROM role_permissions p LEFT JOIN roles r ON r.id = p.role_id WHERE r.id IS NULL;
DELETE t FROM agent_allowed_tools t LEFT JOIN agents a ON a.id = t.agent_id WHERE a.id IS NULL;
DELETE t FROM agent_allowed_tools t LEFT JOIN tools tl ON tl.id = t.tool_id WHERE tl.id IS NULL;
DELETE g FROM tool_grants g LEFT JOIN tools t ON t.id = g.tool_id WHERE t.id IS NULL;
DELETE g FROM tool_grants g LEFT JOIN user_groups_def ug ON ug.id = g.group_id WHERE ug.id IS NULL;
DELETE s FROM agent_steps s LEFT JOIN agent_sessions se ON se.id = s.session_id WHERE se.id IS NULL;
DELETE m FROM agent_messages m LEFT JOIN agent_steps s ON s.id = m.step_id WHERE s.id IS NULL;
DELETE r FROM agent_reasoning r LEFT JOIN agent_steps s ON s.id = r.step_id WHERE s.id IS NULL;
DELETE c FROM agent_tool_calls c LEFT JOIN agent_steps s ON s.id = c.step_id WHERE s.id IS NULL;
DELETE c FROM model_calls c LEFT JOIN agent_sessions s ON s.id = c.session_id WHERE s.id IS NULL;
DELETE p FROM agent_park_snapshots p LEFT JOIN agent_sessions s ON s.id = p.session_id WHERE s.id IS NULL;
DELETE r FROM agent_runs r LEFT JOIN agent_sessions s ON s.id = r.session_id WHERE s.id IS NULL;
DELETE v FROM workflow_versions v LEFT JOIN workflows w ON w.id = v.workflow_id WHERE w.id IS NULL;
DELETE a FROM workflow_assignments a LEFT JOIN workflows w ON w.id = a.workflow_id WHERE w.id IS NULL;

-- A reference that points nowhere is cleared rather than deleted: the row itself
-- is still real, it has simply lost an attribution it can live without.
UPDATE agent_sessions s LEFT JOIN agents a ON a.id = s.agent_id
  SET s.agent_id = NULL WHERE s.agent_id IS NOT NULL AND a.id IS NULL;
UPDATE agent_sessions s LEFT JOIN workflow_versions v ON v.id = s.workflow_version_id
  SET s.workflow_version_id = NULL WHERE s.workflow_version_id IS NOT NULL AND v.id IS NULL;
UPDATE agents a LEFT JOIN ai_models m ON m.id = a.model_id
  SET a.model_id = NULL WHERE a.model_id IS NOT NULL AND m.id IS NULL;

-- Identity ---------------------------------------------------------------------
ALTER TABLE users
  ADD CONSTRAINT fk_users_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

ALTER TABLE user_groups_def
  ADD CONSTRAINT fk_groups_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

ALTER TABLE roles
  ADD CONSTRAINT fk_roles_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

-- A membership is not a thing in its own right: it is the fact that these two
-- exist. Remove either and the fact is gone.
ALTER TABLE user_group_members
  ADD CONSTRAINT fk_member_group FOREIGN KEY (group_id)
    REFERENCES user_groups_def (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_member_user FOREIGN KEY (user_id)
    REFERENCES users (id) ON DELETE CASCADE;

ALTER TABLE group_roles
  ADD CONSTRAINT fk_group_role_group FOREIGN KEY (group_id)
    REFERENCES user_groups_def (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_group_role_role FOREIGN KEY (role_id)
    REFERENCES roles (id) ON DELETE CASCADE;

ALTER TABLE role_permissions
  ADD CONSTRAINT fk_role_permission_role FOREIGN KEY (role_id)
    REFERENCES roles (id) ON DELETE CASCADE;

ALTER TABLE user_sessions
  ADD CONSTRAINT fk_user_session_user FOREIGN KEY (user_id)
    REFERENCES users (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_user_session_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

-- Vendors and models -----------------------------------------------------------
ALTER TABLE ai_vendors
  ADD CONSTRAINT fk_vendor_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

-- A model belongs to its vendor: without the account it is a name that cannot be
-- called. Refusing to delete a vendor that still has models is a POLICY, and it
-- lives in the store, which says so in words a person can read (ErrInUse). This
-- constraint is about integrity, not policy: it makes an orphaned model
-- impossible even when someone deletes a row by hand.
ALTER TABLE ai_models
  ADD CONSTRAINT fk_model_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_model_vendor FOREIGN KEY (vendor_id)
    REFERENCES ai_vendors (id) ON DELETE CASCADE;

-- Configuration ----------------------------------------------------------------
ALTER TABLE tools
  ADD CONSTRAINT fk_tool_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

ALTER TABLE tool_grants
  ADD CONSTRAINT fk_grant_tool FOREIGN KEY (tool_id)
    REFERENCES tools (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_grant_group FOREIGN KEY (group_id)
    REFERENCES user_groups_def (id) ON DELETE CASCADE;

-- An agent that pinned a deleted model loses the pin, not its existence: it
-- falls back to the layer below, which is exactly what a null model_id means.
ALTER TABLE agents
  ADD CONSTRAINT fk_agent_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_agent_model FOREIGN KEY (model_id)
    REFERENCES ai_models (id) ON DELETE SET NULL;

ALTER TABLE agent_allowed_tools
  ADD CONSTRAINT fk_allowed_agent FOREIGN KEY (agent_id)
    REFERENCES agents (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_allowed_tool FOREIGN KEY (tool_id)
    REFERENCES tools (id) ON DELETE CASCADE;

ALTER TABLE workflows
  ADD CONSTRAINT fk_workflow_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

ALTER TABLE workflow_versions
  ADD CONSTRAINT fk_version_workflow FOREIGN KEY (workflow_id)
    REFERENCES workflows (id) ON DELETE CASCADE;

ALTER TABLE workflow_assignments
  ADD CONSTRAINT fk_assignment_workflow FOREIGN KEY (workflow_id)
    REFERENCES workflows (id) ON DELETE CASCADE;

-- Conversations ----------------------------------------------------------------
--
-- A session refers to the agent and the workflow version that shaped it, and it
-- outlives both: deleting an agent must not delete the conversations it once
-- answered, it must only stop claiming they were shaped by something that no
-- longer exists. A sub-agent session, on the other hand, is PART OF its parent.
ALTER TABLE agent_sessions
  ADD CONSTRAINT fk_session_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_session_user FOREIGN KEY (user_id)
    REFERENCES users (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_session_agent FOREIGN KEY (agent_id)
    REFERENCES agents (id) ON DELETE SET NULL,
  ADD CONSTRAINT fk_session_version FOREIGN KEY (workflow_version_id)
    REFERENCES workflow_versions (id) ON DELETE SET NULL,
  ADD CONSTRAINT fk_session_parent FOREIGN KEY (parent_session_id)
    REFERENCES agent_sessions (id) ON DELETE CASCADE;

-- The transcript is part of the conversation, all the way down. This is what
-- replaces the hand-written cascade the store used to perform, and it cannot be
-- forgotten by a code path nobody thought of.
ALTER TABLE agent_steps
  ADD CONSTRAINT fk_step_session FOREIGN KEY (session_id)
    REFERENCES agent_sessions (id) ON DELETE CASCADE;

ALTER TABLE agent_messages
  ADD CONSTRAINT fk_message_step FOREIGN KEY (step_id)
    REFERENCES agent_steps (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_message_session FOREIGN KEY (session_id)
    REFERENCES agent_sessions (id) ON DELETE CASCADE;

ALTER TABLE agent_reasoning
  ADD CONSTRAINT fk_reasoning_step FOREIGN KEY (step_id)
    REFERENCES agent_steps (id) ON DELETE CASCADE;

ALTER TABLE agent_tool_calls
  ADD CONSTRAINT fk_call_step FOREIGN KEY (step_id)
    REFERENCES agent_steps (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_call_session FOREIGN KEY (session_id)
    REFERENCES agent_sessions (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_call_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

-- A parked approval is meaningless without the conversation it belongs to.
ALTER TABLE agent_park_snapshots
  ADD CONSTRAINT fk_park_session FOREIGN KEY (session_id)
    REFERENCES agent_sessions (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_park_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_park_user FOREIGN KEY (user_id)
    REFERENCES users (id) ON DELETE CASCADE;

ALTER TABLE agent_runs
  ADD CONSTRAINT fk_run_session FOREIGN KEY (session_id)
    REFERENCES agent_sessions (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_run_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_run_user FOREIGN KEY (user_id)
    REFERENCES users (id) ON DELETE CASCADE;

-- model_calls is operational telemetry ABOUT a conversation: what this turn cost.
-- It belongs to the conversation and goes with it. The immutable ledger that must
-- outlive everything is audit_logs, and that is why audit_logs has no foreign
-- keys at all: a record of what happened cannot depend on the continued existence
-- of what it happened to.
--
-- model_id has no constraint on purpose. Cost history that forgets which model
-- ran is not cost history, so a deleted model must not erase the attribution, and
-- nor may it prevent the deletion.
ALTER TABLE model_calls
  ADD CONSTRAINT fk_model_call_session FOREIGN KEY (session_id)
    REFERENCES agent_sessions (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_model_call_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

-- OAuth ------------------------------------------------------------------------
ALTER TABLE oauth_clients
  ADD CONSTRAINT fk_oauth_client_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

ALTER TABLE oauth_auth_codes
  ADD CONSTRAINT fk_auth_code_user FOREIGN KEY (user_id)
    REFERENCES users (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_auth_code_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

ALTER TABLE oauth_access_tokens
  ADD CONSTRAINT fk_access_token_user FOREIGN KEY (user_id)
    REFERENCES users (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_access_token_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

ALTER TABLE oauth_refresh_tokens
  ADD CONSTRAINT fk_refresh_token_user FOREIGN KEY (user_id)
    REFERENCES users (id) ON DELETE CASCADE,
  ADD CONSTRAINT fk_refresh_token_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

ALTER TABLE oauth_consents
  ADD CONSTRAINT fk_consent_user FOREIGN KEY (user_id)
    REFERENCES users (id) ON DELETE CASCADE;

ALTER TABLE mcp_servers
  ADD CONSTRAINT fk_mcp_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

ALTER TABLE jobs
  ADD CONSTRAINT fk_job_workspace FOREIGN KEY (workspace_id)
    REFERENCES workspaces (id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE jobs DROP FOREIGN KEY fk_job_workspace;
ALTER TABLE mcp_servers DROP FOREIGN KEY fk_mcp_workspace;
ALTER TABLE oauth_consents DROP FOREIGN KEY fk_consent_user;
ALTER TABLE oauth_refresh_tokens DROP FOREIGN KEY fk_refresh_token_user,
  DROP FOREIGN KEY fk_refresh_token_workspace;
ALTER TABLE oauth_access_tokens DROP FOREIGN KEY fk_access_token_user,
  DROP FOREIGN KEY fk_access_token_workspace;
ALTER TABLE oauth_auth_codes DROP FOREIGN KEY fk_auth_code_user,
  DROP FOREIGN KEY fk_auth_code_workspace;
ALTER TABLE oauth_clients DROP FOREIGN KEY fk_oauth_client_workspace;
ALTER TABLE model_calls DROP FOREIGN KEY fk_model_call_session,
  DROP FOREIGN KEY fk_model_call_workspace;
ALTER TABLE agent_runs DROP FOREIGN KEY fk_run_session,
  DROP FOREIGN KEY fk_run_workspace, DROP FOREIGN KEY fk_run_user;
ALTER TABLE agent_park_snapshots DROP FOREIGN KEY fk_park_session,
  DROP FOREIGN KEY fk_park_workspace, DROP FOREIGN KEY fk_park_user;
ALTER TABLE agent_tool_calls DROP FOREIGN KEY fk_call_step,
  DROP FOREIGN KEY fk_call_session, DROP FOREIGN KEY fk_call_workspace;
ALTER TABLE agent_reasoning DROP FOREIGN KEY fk_reasoning_step;
ALTER TABLE agent_messages DROP FOREIGN KEY fk_message_step,
  DROP FOREIGN KEY fk_message_session;
ALTER TABLE agent_steps DROP FOREIGN KEY fk_step_session;
ALTER TABLE agent_sessions DROP FOREIGN KEY fk_session_workspace,
  DROP FOREIGN KEY fk_session_user, DROP FOREIGN KEY fk_session_agent,
  DROP FOREIGN KEY fk_session_version, DROP FOREIGN KEY fk_session_parent;
ALTER TABLE workflow_assignments DROP FOREIGN KEY fk_assignment_workflow;
ALTER TABLE workflow_versions DROP FOREIGN KEY fk_version_workflow;
ALTER TABLE workflows DROP FOREIGN KEY fk_workflow_workspace;
ALTER TABLE agent_allowed_tools DROP FOREIGN KEY fk_allowed_agent,
  DROP FOREIGN KEY fk_allowed_tool;
ALTER TABLE agents DROP FOREIGN KEY fk_agent_workspace, DROP FOREIGN KEY fk_agent_model;
ALTER TABLE tool_grants DROP FOREIGN KEY fk_grant_tool, DROP FOREIGN KEY fk_grant_group;
ALTER TABLE tools DROP FOREIGN KEY fk_tool_workspace;
ALTER TABLE ai_models DROP FOREIGN KEY fk_model_workspace, DROP FOREIGN KEY fk_model_vendor;
ALTER TABLE ai_vendors DROP FOREIGN KEY fk_vendor_workspace;
ALTER TABLE user_sessions DROP FOREIGN KEY fk_user_session_user,
  DROP FOREIGN KEY fk_user_session_workspace;
ALTER TABLE role_permissions DROP FOREIGN KEY fk_role_permission_role;
ALTER TABLE group_roles DROP FOREIGN KEY fk_group_role_group,
  DROP FOREIGN KEY fk_group_role_role;
ALTER TABLE user_group_members DROP FOREIGN KEY fk_member_group,
  DROP FOREIGN KEY fk_member_user;
ALTER TABLE roles DROP FOREIGN KEY fk_roles_workspace;
ALTER TABLE user_groups_def DROP FOREIGN KEY fk_groups_workspace;
ALTER TABLE users DROP FOREIGN KEY fk_users_workspace;
