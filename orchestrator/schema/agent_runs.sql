CREATE TABLE `agent_runs` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `uid` varchar(32) NOT NULL,
  `workspace_id` bigint(20) unsigned NOT NULL,
  `session_id` bigint(20) unsigned NOT NULL,
  `user_id` bigint(20) unsigned NOT NULL,
  `status` enum('running','completed','failed','waiting_approval','cancelled','interrupted') NOT NULL DEFAULT 'running',
  `created_at` datetime(3) NOT NULL,
  `completed_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_uid` (`uid`),
  KEY `idx_session` (`session_id`,`id`),
  KEY `idx_status` (`status`),
  KEY `fk_run_workspace` (`workspace_id`),
  KEY `fk_run_user` (`user_id`),
  CONSTRAINT `fk_run_session` FOREIGN KEY (`session_id`) REFERENCES `agent_sessions` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_run_user` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_run_workspace` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
