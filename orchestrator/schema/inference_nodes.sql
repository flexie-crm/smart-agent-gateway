CREATE TABLE `inference_nodes` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `node_id` varchar(64) NOT NULL,
  `name` varchar(255) NOT NULL,
  `base_url` varchar(512) NOT NULL,
  `key_enc` varbinary(2048) NOT NULL,
  `version` varchar(32) NOT NULL DEFAULT '',
  `cert_expires_at` datetime(3) DEFAULT NULL,
  `pinned_cert` text DEFAULT NULL,
  `created_at` datetime(3) NOT NULL,
  `settings` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL CHECK (json_valid(`settings`)),
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_node_id` (`node_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
