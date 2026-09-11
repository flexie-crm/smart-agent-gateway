-- A park snapshot can point at the delegation it belongs to.
--
-- A specialist that holds an approval-gated tool must be able to stop the whole
-- turn and ask, exactly as the master does. The snapshot still carries the
-- prepared call, not the conversation (KB/16); it only gains the ADDRESS of the
-- delegation so the resume knows to re-enter it: which specialist, the master's
-- `subagent` call it runs under, and the handoff mode that decides who speaks
-- when the delegation finishes. All three NULL is a master park, unchanged.

-- +goose Up
ALTER TABLE agent_park_snapshots
  ADD COLUMN agent_key VARCHAR(64) DEFAULT NULL AFTER definition_hash,
  ADD COLUMN parent_tool_call_id VARCHAR(64) DEFAULT NULL AFTER agent_key,
  ADD COLUMN handoff_mode VARCHAR(16) DEFAULT NULL AFTER parent_tool_call_id;

-- +goose Down
ALTER TABLE agent_park_snapshots
  DROP COLUMN handoff_mode,
  DROP COLUMN parent_tool_call_id,
  DROP COLUMN agent_key;
