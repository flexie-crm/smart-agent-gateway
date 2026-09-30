CREATE TABLE `agent_compactions` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `session_id` bigint(20) unsigned NOT NULL,
  `through_seq` int(11) NOT NULL,
  `summary` longtext NOT NULL,
  `vendor` varchar(64) DEFAULT NULL,
  `model` varchar(128) DEFAULT NULL,
  `created_by` bigint(20) unsigned DEFAULT NULL,
  `created_by_name` varchar(255) NOT NULL DEFAULT '',
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_session` (`session_id`,`id`),
  KEY `fk_compaction_created_by` (`created_by`),
  CONSTRAINT `fk_compaction_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON DELETE SET NULL,
  CONSTRAINT `fk_compaction_session` FOREIGN KEY (`session_id`) REFERENCES `agent_sessions` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
