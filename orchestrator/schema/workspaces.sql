CREATE TABLE `workspaces` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `slug` varchar(64) NOT NULL,
  `name` varchar(255) NOT NULL,
  `description` varchar(1024) NOT NULL DEFAULT '',
  `status` enum('active','suspended') NOT NULL DEFAULT 'active',
  `created_by` bigint(20) unsigned DEFAULT NULL,
  `created_by_name` varchar(255) NOT NULL DEFAULT '',
  `updated_by` bigint(20) unsigned DEFAULT NULL,
  `updated_by_name` varchar(255) NOT NULL DEFAULT '',
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `fk_workspace_created_by` (`created_by`),
  KEY `fk_workspace_updated_by` (`updated_by`),
  UNIQUE KEY `slug` (`slug`),
  CONSTRAINT `fk_workspace_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON DELETE SET NULL,
  CONSTRAINT `fk_workspace_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
