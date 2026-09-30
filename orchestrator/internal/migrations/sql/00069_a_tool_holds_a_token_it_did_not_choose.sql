-- The tokens a custom tool was granted, which it cannot get back by itself.
--
-- WHY THIS IS NOT IN THE TOOL'S CONFIG, which is where every other setting of a
-- custom tool lives. That config is what an ADMINISTRATOR wrote: it is authored,
-- sealed on write, blanked when the form is reopened, and it changes when
-- somebody saves the form. A refresh token is none of those things. It arrives
-- from a third party mid-flight, it rotates without anybody pressing anything,
-- and a handler has to write it back. Putting it there would mean a tool
-- rewriting its own configuration behind the administrator who owns it.
--
-- WHY IT IS NEEDED AT ALL, when the client-credentials grant needed nothing.
-- There the access token is RE-OBTAINABLE at any moment from the client id and
-- secret already stored, so losing it costs one request and it lives in memory.
-- An authorization-code grant is the opposite: its refresh token was issued once
-- against a person's consent in a browser, and nothing in this process can get
-- another without sending them back through it. Lose it on a restart and the
-- connection is simply gone until somebody reconnects by hand.
--
-- One row per tool, because the grant is the tool's: two agents using one tool
-- use one connection to one service, exactly as they share its base address and
-- its key. Cascading on the tool, because a token for a tool that no longer
-- exists is a credential nobody can reach and nobody can revoke.
--
-- The same three columns mcp_servers already carries for the same reason
-- (oauth_access_token_enc, oauth_refresh_token_enc, oauth_token_expires_at),
-- and deliberately the same names: two places holding a granted token should be
-- recognisably the same thing, not two designs.
--
-- Sealed with the workspace's keyring like every other secret at rest, so what
-- is here is unreadable without it. The expiry is NOT sealed: it decides whether
-- to refresh before a call, it says nothing on its own, and encrypting it would
-- mean opening a credential to read a clock.

-- +goose Up
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

-- +goose Down
DROP TABLE `tool_credentials`;
