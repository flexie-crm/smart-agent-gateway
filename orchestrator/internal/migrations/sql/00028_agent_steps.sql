-- A delegation's inner steps become durable.
--
-- Until now a specialist's steps streamed live and were then thrown away: the
-- master's transcript kept only the `subagent` call and its result. That is
-- enough while a delegation cannot stop to ask a human. Once it can (sub-agent
-- park), resuming needs the specialist's own conversation back, and the park row
-- carries the prepared call, not the conversation, so the conversation has to
-- live in the transcript like the master's does.
--
-- A step with both columns NULL is a master step, on the conversation's own
-- timeline. A step with both set is a specialist's inner step: agent_key is
-- which specialist produced it, parent_tool_call_id is the master's `subagent`
-- call it runs under. The master's model transcript excludes the latter; the
-- specialist's loop is rebuilt from them by parent_tool_call_id.

-- +goose Up
ALTER TABLE agent_steps
  ADD COLUMN agent_key VARCHAR(64) DEFAULT NULL AFTER model,
  ADD COLUMN parent_tool_call_id VARCHAR(64) DEFAULT NULL AFTER agent_key,
  ADD KEY idx_parent_call (session_id, parent_tool_call_id);

-- +goose Down
ALTER TABLE agent_steps
  DROP KEY idx_parent_call,
  DROP COLUMN parent_tool_call_id,
  DROP COLUMN agent_key;
