CREATE TABLE `agent_fleets` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `session_id` bigint(20) unsigned NOT NULL,
  `workspace_id` bigint(20) unsigned NOT NULL,
  `parent_tool_call_id` varchar(64) NOT NULL,
  `model_id` bigint(20) unsigned DEFAULT NULL,
  `size` smallint(5) unsigned NOT NULL,
  `status` enum('running','done','cancelled') NOT NULL DEFAULT 'running',
  `deadline` datetime(3) NOT NULL,
  `created_at` datetime(3) NOT NULL,
  `completed_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_session` (`session_id`),
  KEY `idx_status_deadline` (`status`,`deadline`),
  KEY `fk_fleet_model` (`model_id`),
  CONSTRAINT `fk_fleet_model` FOREIGN KEY (`model_id`) REFERENCES `ai_models` (`id`) ON DELETE SET NULL,
  CONSTRAINT `fk_fleet_session` FOREIGN KEY (`session_id`) REFERENCES `agent_sessions` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_fleet_workspace` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
