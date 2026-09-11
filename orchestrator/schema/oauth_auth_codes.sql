CREATE TABLE `oauth_auth_codes` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `code_hash` char(64) NOT NULL,
  `client_pk` bigint(20) unsigned NOT NULL,
  `user_id` bigint(20) unsigned NOT NULL,
  `workspace_id` bigint(20) unsigned NOT NULL,
  `redirect_uri` varchar(512) NOT NULL,
  `scopes` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`scopes`)),
  `code_challenge` varchar(128) NOT NULL,
  `expires_at` datetime(3) NOT NULL,
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `code_hash` (`code_hash`),
  KEY `idx_expires` (`expires_at`),
  KEY `fk_auth_code_user` (`user_id`),
  KEY `fk_auth_code_workspace` (`workspace_id`),
  CONSTRAINT `fk_auth_code_user` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_auth_code_workspace` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
