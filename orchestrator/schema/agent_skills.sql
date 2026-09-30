CREATE TABLE `agent_skills` (
  `agent_id` bigint(20) unsigned NOT NULL,
  `skill_id` bigint(20) unsigned NOT NULL,
  PRIMARY KEY (`agent_id`,`skill_id`),
  KEY `idx_skill` (`skill_id`),
  CONSTRAINT `fk_agent_skill_agent` FOREIGN KEY (`agent_id`) REFERENCES `agents` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_agent_skill_skill` FOREIGN KEY (`skill_id`) REFERENCES `ai_skills` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
