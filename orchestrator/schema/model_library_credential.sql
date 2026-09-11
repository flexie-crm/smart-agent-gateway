CREATE TABLE `model_library_credential` (
  `id` tinyint(3) unsigned NOT NULL DEFAULT 1,
  `token_enc` varbinary(2048) NOT NULL,
  `updated_by` bigint(20) unsigned DEFAULT NULL,
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_model_library_credential_updated_by` (`updated_by`),
  CONSTRAINT `fk_model_library_credential_user` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
