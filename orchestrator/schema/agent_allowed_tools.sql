CREATE TABLE `agent_allowed_tools` (
  `agent_id` bigint(20) unsigned NOT NULL,
  `tool_id` bigint(20) unsigned NOT NULL,
  PRIMARY KEY (`agent_id`,`tool_id`),
  KEY `fk_allowed_tool` (`tool_id`),
  CONSTRAINT `fk_allowed_agent` FOREIGN KEY (`agent_id`) REFERENCES `agents` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_allowed_tool` FOREIGN KEY (`tool_id`) REFERENCES `tools` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
