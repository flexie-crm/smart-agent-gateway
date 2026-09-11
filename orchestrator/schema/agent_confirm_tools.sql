CREATE TABLE `agent_confirm_tools` (
  `agent_id` bigint(20) unsigned NOT NULL,
  `tool_id` bigint(20) unsigned NOT NULL,
  PRIMARY KEY (`agent_id`,`tool_id`),
  KEY `fk_confirm_tool` (`tool_id`),
  CONSTRAINT `fk_confirm_agent` FOREIGN KEY (`agent_id`) REFERENCES `agents` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_confirm_tool` FOREIGN KEY (`tool_id`) REFERENCES `tools` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
