-- The MCP client: SAG connecting OUT to third-party tool servers.
--
-- A connection's remote tools are PROJECTED into the tools table (kind
-- 'mcp'), where the existing governance applies unchanged: status, approval,
-- grants, agent allow-lists. The projection needs to know which connection
-- offers a tool, what the remote calls it, and a hash of its definition, so
-- a remote that adds, renames, or quietly rewrites a tool is a visible
-- event, never a silent one.
--
-- The connection itself gains a tool_prefix (the immutable namespace its
-- tools live under, so a renamed connection does not orphan its grants), and
-- the bookkeeping of its last sync. A parked confirmation records the
-- definition hash of the moment it parked: approving a card must not run an
-- action the remote redefined while the card sat waiting.

-- +goose Up
ALTER TABLE tools
  MODIFY name VARCHAR(191) NOT NULL,
  MODIFY kind ENUM('builtin','custom','client','internal','mcp') NOT NULL,
  ADD COLUMN mcp_server_id BIGINT UNSIGNED DEFAULT NULL,
  ADD COLUMN remote_name VARCHAR(255) DEFAULT NULL,
  ADD COLUMN definition_hash CHAR(64) DEFAULT NULL,
  ADD COLUMN remote_missing TINYINT(1) NOT NULL DEFAULT 0,
  ADD COLUMN definition_changed_at DATETIME(3) DEFAULT NULL,
  ADD KEY idx_tool_mcp_server (mcp_server_id),
  ADD CONSTRAINT fk_tool_mcp_server FOREIGN KEY (mcp_server_id) REFERENCES mcp_servers (id) ON DELETE CASCADE;

ALTER TABLE mcp_servers
  ADD COLUMN tool_prefix VARCHAR(64) NOT NULL AFTER status,
  ADD COLUMN last_synced_at DATETIME(3) DEFAULT NULL AFTER tool_prefix,
  ADD COLUMN last_error TEXT DEFAULT NULL AFTER last_synced_at;

-- The table is empty in every known deployment (no code consumed it before
-- this feature), so backfilling the prefix from the name is formality; the
-- unique key is the real point.
UPDATE mcp_servers SET tool_prefix = LOWER(REPLACE(name, ' ', '-')) WHERE tool_prefix = '';
ALTER TABLE mcp_servers
  ADD UNIQUE KEY uniq_ws_prefix (workspace_id, tool_prefix);

ALTER TABLE agent_park_snapshots
  MODIFY tool_name VARCHAR(191) NOT NULL,
  ADD COLUMN definition_hash CHAR(64) DEFAULT NULL AFTER action_hash;

-- +goose Down
ALTER TABLE agent_park_snapshots
  DROP COLUMN definition_hash,
  MODIFY tool_name VARCHAR(64) NOT NULL;
ALTER TABLE mcp_servers
  DROP KEY uniq_ws_prefix,
  DROP COLUMN last_error,
  DROP COLUMN last_synced_at,
  DROP COLUMN tool_prefix;
DELETE FROM tools WHERE kind = 'mcp';
ALTER TABLE tools
  DROP FOREIGN KEY fk_tool_mcp_server,
  DROP KEY idx_tool_mcp_server,
  DROP COLUMN definition_changed_at,
  DROP COLUMN remote_missing,
  DROP COLUMN definition_hash,
  DROP COLUMN remote_name,
  DROP COLUMN mcp_server_id,
  MODIFY kind ENUM('builtin','custom','client','internal') NOT NULL,
  MODIFY name VARCHAR(64) NOT NULL;
