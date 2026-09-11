-- Rename every projected MCP tool to a name a model will accept.
--
-- They were minted as `<prefix>.<remote name>`, and a dot is not in the
-- alphabet the model APIs allow for a tool name (`^[a-zA-Z0-9_-]+$`). Because
-- the tools go up as one array, a single connection made every tool of every
-- agent that carried one unusable: a 400 on every turn.
--
-- The new name is recomputed from the two columns it was built from rather than
-- edited in place, so a remote name that itself held a dot is corrected too
-- instead of keeping one and losing the other.
--
-- Nothing else has to move. Grants (`agent_allowed_tools`, `agent_confirm_tools`)
-- point at `tools.id`. The transcript tables that DO store a tool name
-- (`agent_tool_calls`, `agent_park_snapshots`) are deliberately left alone: they
-- record what was called at the time, and rewriting history to look like it
-- worked is not a migration.

-- +goose Up
UPDATE tools t
  JOIN mcp_servers s ON s.id = t.mcp_server_id
SET t.name = CONCAT(
  REGEXP_REPLACE(s.tool_prefix, '[^a-zA-Z0-9_-]', '_'),
  '_',
  REGEXP_REPLACE(t.remote_name, '[^a-zA-Z0-9_-]', '_')
)
WHERE t.kind = 'mcp'
  AND t.remote_name IS NOT NULL
  -- Only rows that are actually wrong, so running this twice changes nothing.
  AND t.name <> CONCAT(
    REGEXP_REPLACE(s.tool_prefix, '[^a-zA-Z0-9_-]', '_'),
    '_',
    REGEXP_REPLACE(t.remote_name, '[^a-zA-Z0-9_-]', '_')
  )
  -- A name too long to be usable cannot be repaired by renaming, and a guess
  -- at a shorter one could collide with a real tool. Left as it is; the next
  -- sync reports it as one that could not be projected.
  AND CHAR_LENGTH(CONCAT(s.tool_prefix, '_', t.remote_name)) <= 64;

-- +goose Down
UPDATE tools t
  JOIN mcp_servers s ON s.id = t.mcp_server_id
SET t.name = CONCAT(s.tool_prefix, '.', t.remote_name)
WHERE t.kind = 'mcp'
  AND t.remote_name IS NOT NULL
  AND t.name <> CONCAT(s.tool_prefix, '.', t.remote_name);
