CREATE TABLE `agent_steps` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `session_id` bigint(20) unsigned NOT NULL,
  `seq` int(11) NOT NULL,
  `kind` enum('user','assistant') NOT NULL,
  `vendor` varchar(64) DEFAULT NULL,
  `model` varchar(128) DEFAULT NULL,
  `agent_key` varchar(64) DEFAULT NULL,
  `parent_tool_call_id` varchar(64) DEFAULT NULL,
  `is_partial` tinyint(1) NOT NULL DEFAULT 0,
  `attachments` json DEFAULT NULL,
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_session_seq` (`session_id`,`seq`),
  KEY `idx_session` (`session_id`),
  KEY `idx_parent_call` (`session_id`,`parent_tool_call_id`),
  CONSTRAINT `fk_step_session` FOREIGN KEY (`session_id`) REFERENCES `agent_sessions` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
