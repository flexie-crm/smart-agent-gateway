CREATE TABLE `agent_reasoning` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `step_id` bigint(20) unsigned NOT NULL,
  `content` longtext NOT NULL,
  `vendor` varchar(64) DEFAULT NULL,
  `model` varchar(128) DEFAULT NULL,
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_step` (`step_id`),
  CONSTRAINT `fk_reasoning_step` FOREIGN KEY (`step_id`) REFERENCES `agent_steps` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
