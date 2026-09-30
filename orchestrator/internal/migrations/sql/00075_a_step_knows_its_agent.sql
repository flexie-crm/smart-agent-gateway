-- Which agent run an agent's step belongs to.
--
-- An agent's step was attributed by the agent's key and the Gateway call that
-- started it. That is exact for a background agent, which has a call of its
-- own, and not for a fleet: its members are started by ONE call, so the same
-- agent given six tasks in one batch writes six conversations under one key and
-- one parent, interleaved, and nothing on the rows says which is which. The chat
-- now opens a single agent's work, so the step has to say whose it is.
--
-- Nullable, because the Gateway's own steps and a synchronous agent's belong to
-- no delegation. SET NULL rather than CASCADE: a step is part of what was said
-- in the conversation, and losing the record of who ran it is no reason to lose
-- what it said.
--
-- The rows already written are filled in only where the answer is certain: a
-- key and a parent call that belong to exactly one delegation. That is every
-- background agent, and every fleet member whose agent ran once in its batch.
-- The rest stay NULL, because any choice among the members would be a guess.

-- +goose Up
ALTER TABLE `agent_steps`
  ADD COLUMN `delegation_id` bigint(20) unsigned DEFAULT NULL AFTER `parent_tool_call_id`,
  ADD KEY `fk_step_delegation` (`delegation_id`),
  ADD CONSTRAINT `fk_step_delegation` FOREIGN KEY (`delegation_id`) REFERENCES `agent_delegations` (`id`) ON DELETE SET NULL;

UPDATE `agent_steps` s
JOIN (
  SELECT MIN(`id`) AS `id`, `session_id`, `parent_tool_call_id`, `agent_key`
  FROM `agent_delegations`
  GROUP BY `session_id`, `parent_tool_call_id`, `agent_key`
  HAVING COUNT(*) = 1
) d ON d.`session_id` = s.`session_id`
   AND d.`parent_tool_call_id` = s.`parent_tool_call_id`
   AND d.`agent_key` = s.`agent_key`
SET s.`delegation_id` = d.`id`;

-- +goose Down
ALTER TABLE `agent_steps`
  DROP FOREIGN KEY `fk_step_delegation`,
  DROP KEY `fk_step_delegation`,
  DROP COLUMN `delegation_id`;
