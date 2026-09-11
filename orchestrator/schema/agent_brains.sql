CREATE TABLE `agent_brains` (
  `agent_id` bigint(20) unsigned NOT NULL,
  `brain_id` bigint(20) unsigned NOT NULL,
  PRIMARY KEY (`agent_id`,`brain_id`),
  KEY `idx_brain` (`brain_id`),
  CONSTRAINT `fk_agent_brain_agent` FOREIGN KEY (`agent_id`) REFERENCES `agents` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_agent_brain_brain` FOREIGN KEY (`brain_id`) REFERENCES `brains` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
