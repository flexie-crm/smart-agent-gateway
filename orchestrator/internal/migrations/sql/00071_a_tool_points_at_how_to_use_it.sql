-- The skills that belong to a TOOL rather than to an agent.
--
-- A skill assigned to an agent is a procedure that agent holds: it is in the
-- prompt's map of them every turn, because it could be relevant to anything the
-- agent is asked. A skill assigned to a tool is different in kind. It is the
-- documentation for THAT tool: which paths an API has, what its error codes
-- mean, the order its endpoints must be called in. It is relevant exactly when
-- the tool is, and not otherwise.
--
-- So it is not in the prompt. It is named in the tool's own guide, which the
-- agent reads when it drills into the tool (tool_guide), and reaching it is a
-- second drill-down from there (load_skill). An agent holding a tool can read
-- the tool's skills for that reason: they came with the tool, exactly as its
-- base address and its policy did, and an administrator who granted the tool
-- granted the instructions for it.
--
-- One row per pair, the shape agent_skills already has. Cascading both ways: a
-- pointer to a deleted skill, or from a deleted tool, is a row nobody can reach.

-- +goose Up
CREATE TABLE `tool_skills` (
  `tool_id` bigint(20) unsigned NOT NULL,
  `skill_id` bigint(20) unsigned NOT NULL,
  PRIMARY KEY (`tool_id`,`skill_id`),
  KEY `idx_skill` (`skill_id`),
  CONSTRAINT `fk_tool_skill_tool` FOREIGN KEY (`tool_id`) REFERENCES `tools` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_tool_skill_skill` FOREIGN KEY (`skill_id`) REFERENCES `ai_skills` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE `tool_skills`;
