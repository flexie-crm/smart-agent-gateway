-- The table that held a token a PERSON granted to a custom tool, removed with
-- the grant that needed it.
--
-- It existed for one variant of the API tool: the OAuth authorization-code
-- flow, where somebody signs in at the service in a browser and the refresh
-- token that comes back cannot be re-obtained without sending them through it
-- again. That is what made a table necessary, where the client-credentials
-- grant needs none: there the access token is re-obtainable at any moment from
-- the client id and secret already in the tool's config, so it lives in memory.
--
-- That variant is gone. What remains is the client-credentials grant, which
-- writes nothing, so this table has no writer and never will while that is
-- true. Kept empty it would be schema drift: a declared table nothing reaches.
--
-- Dropping it destroys nothing in practice (no deployment ever connected one),
-- and the Down brings it back exactly as migration 69 wrote it, so this is
-- reversible rather than one way.

-- +goose Up
DROP TABLE IF EXISTS `tool_credentials`;

-- +goose Down
CREATE TABLE `tool_credentials` (
  `tool_id` bigint(20) unsigned NOT NULL,
  `workspace_id` bigint(20) unsigned NOT NULL,
  `access_token_enc` varbinary(4096) DEFAULT NULL,
  `refresh_token_enc` varbinary(4096) DEFAULT NULL,
  `token_expires_at` datetime(3) DEFAULT NULL,
  `connected_at` datetime(3) DEFAULT NULL,
  `connected_by` bigint(20) unsigned DEFAULT NULL,
  `connected_by_name` varchar(255) DEFAULT NULL,
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`tool_id`),
  KEY `idx_workspace` (`workspace_id`),
  CONSTRAINT `fk_tool_credential_tool` FOREIGN KEY (`tool_id`) REFERENCES `tools` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_tool_credential_workspace` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_tool_credential_user` FOREIGN KEY (`connected_by`) REFERENCES `users` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
