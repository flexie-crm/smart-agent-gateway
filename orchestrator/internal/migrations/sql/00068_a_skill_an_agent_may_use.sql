-- Which skills an agent may use.
--
-- The third thing an agent is assigned, beside its tools (`agent_allowed_tools`)
-- and its knowledge (`agent_brains`), and deliberately the same shape as both: a
-- join table with nothing on it but the two ids. What an assignment MEANS lives
-- in the resolver, not here.
--
-- Two columns and no more. There is no per-assignment setting (no read-only, no
-- "scripts allowed here but not there") because a skill is a package somebody
-- imported as a whole, and a decision about what may be done with it belongs to
-- the skill rather than to each agent that holds it. A column here would be the
-- same decision written once per agent, and they would disagree.
--
-- EMPTY MEANS NONE, which is the rule the brains follow and the opposite of the
-- one a permission catalogue follows. The brain tools are registered with an
-- empty allow-list and rebound per turn over exactly what was assigned
-- (tools.BindBrains), so a brain nobody assigned is one no agent can reach; the
-- skill tools will be scoped the same way. An agent with no row here reaches no
-- skill, which is what an administrator who has assigned nothing has said.
--
-- The Gateway is an agent (agent 1), so it is assigned skills through this table
-- exactly as its agents are. Nothing here knows which one it is.
--
-- Both sides cascade. A deleted agent takes its assignments; a deleted skill
-- takes every agent's assignment of it, because an assignment to a skill that
-- does not exist is a row that can only ever be filtered out on read.

-- +goose Up
CREATE TABLE `agent_skills` (
  `agent_id` bigint(20) unsigned NOT NULL,
  `skill_id` bigint(20) unsigned NOT NULL,
  PRIMARY KEY (`agent_id`,`skill_id`),
  KEY `idx_skill` (`skill_id`),
  CONSTRAINT `fk_agent_skill_agent` FOREIGN KEY (`agent_id`) REFERENCES `agents` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_agent_skill_skill` FOREIGN KEY (`skill_id`) REFERENCES `ai_skills` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE `agent_skills`;
