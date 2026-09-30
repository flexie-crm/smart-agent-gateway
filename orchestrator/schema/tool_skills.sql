-- The skills that belong to a TOOL rather than to an agent: the documentation
-- for using it (an API's paths, its error codes, the order to call things in).
--
-- Not in the prompt, unlike agent_skills. It is named in the tool's own guide,
-- which an agent reads when it drills into the tool, and reaching it is a
-- second drill-down from there. An agent holding the tool can read them:
-- whoever granted the tool granted the instructions for it.

CREATE TABLE `tool_skills` (
  `tool_id` bigint(20) unsigned NOT NULL,
  `skill_id` bigint(20) unsigned NOT NULL,
  PRIMARY KEY (`tool_id`,`skill_id`),
  KEY `idx_skill` (`skill_id`),
  CONSTRAINT `fk_tool_skill_tool` FOREIGN KEY (`tool_id`) REFERENCES `tools` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_tool_skill_skill` FOREIGN KEY (`skill_id`) REFERENCES `ai_skills` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
