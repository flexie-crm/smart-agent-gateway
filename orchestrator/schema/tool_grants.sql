CREATE TABLE `tool_grants` (
  `tool_id` bigint(20) unsigned NOT NULL,
  `group_id` bigint(20) unsigned NOT NULL,
  PRIMARY KEY (`tool_id`,`group_id`),
  KEY `fk_grant_group` (`group_id`),
  CONSTRAINT `fk_grant_group` FOREIGN KEY (`group_id`) REFERENCES `user_groups_def` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_grant_tool` FOREIGN KEY (`tool_id`) REFERENCES `tools` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
