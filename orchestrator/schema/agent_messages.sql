CREATE TABLE `agent_messages` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `step_id` bigint(20) unsigned NOT NULL,
  `session_id` bigint(20) unsigned NOT NULL,
  `role` enum('user','assistant') NOT NULL,
  `content` longtext DEFAULT NULL,
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_step` (`step_id`),
  KEY `idx_session` (`session_id`),
  FULLTEXT KEY `ft_content` (`content`),
  CONSTRAINT `fk_message_session` FOREIGN KEY (`session_id`) REFERENCES `agent_sessions` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_message_step` FOREIGN KEY (`step_id`) REFERENCES `agent_steps` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
