CREATE TABLE `workflow_assignments` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `workflow_id` bigint(20) unsigned NOT NULL,
  `match_type` enum('group','role','user','channel') NOT NULL,
  `match_value` varchar(128) NOT NULL,
  `priority` int(11) NOT NULL DEFAULT 0,
  PRIMARY KEY (`id`),
  KEY `idx_wf` (`workflow_id`),
  KEY `idx_match` (`match_type`,`match_value`),
  CONSTRAINT `fk_assignment_workflow` FOREIGN KEY (`workflow_id`) REFERENCES `workflows` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
