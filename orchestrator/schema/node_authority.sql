CREATE TABLE `node_authority` (
  `id` tinyint(3) unsigned NOT NULL DEFAULT 1,
  `key_enc` varbinary(2048) NOT NULL,
  `cert_pem` text NOT NULL,
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
