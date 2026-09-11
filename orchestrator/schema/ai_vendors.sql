CREATE TABLE `ai_vendors` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `workspace_id` bigint(20) unsigned NOT NULL,
  `vendor_key` varchar(64) NOT NULL,
  `name` varchar(255) NOT NULL,
  `base_url` varchar(512) DEFAULT NULL,
  `node_id` bigint(20) unsigned DEFAULT NULL,
  `credentials_enc` varbinary(2048) DEFAULT NULL,
  `status` enum('active','disabled') NOT NULL DEFAULT 'active',
  `created_at` datetime(3) NOT NULL,
  `settings` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL CHECK (json_valid(`settings`)),
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_ws_node` (`workspace_id`,`node_id`),
  KEY `idx_ws` (`workspace_id`),
  KEY `fk_vendor_node` (`node_id`),
  CONSTRAINT `fk_vendor_node` FOREIGN KEY (`node_id`) REFERENCES `inference_nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_vendor_workspace` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
