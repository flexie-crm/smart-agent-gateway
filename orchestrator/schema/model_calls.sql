CREATE TABLE `model_calls` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `workspace_id` bigint(20) unsigned NOT NULL,
  `session_id` bigint(20) unsigned NOT NULL,
  `model_id` bigint(20) unsigned NOT NULL,
  `input_tokens` bigint(20) NOT NULL DEFAULT 0,
  `output_tokens` bigint(20) NOT NULL DEFAULT 0,
  `duration_ms` int(11) DEFAULT NULL,
  `status` enum('completed','failed') NOT NULL,
  `error_text` text DEFAULT NULL,
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_ws_created` (`workspace_id`,`created_at`),
  KEY `idx_session` (`session_id`),
  CONSTRAINT `fk_model_call_session` FOREIGN KEY (`session_id`) REFERENCES `agent_sessions` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_model_call_workspace` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
