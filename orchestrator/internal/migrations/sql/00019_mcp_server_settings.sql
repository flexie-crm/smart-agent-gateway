-- Our MCP server's configuration (CRM parity: mcpToolConfig/mcpBrainConfig),
-- one row per workspace. No row means unconfigured, and unconfigured means
-- the whole catalog is exposed, the CRM's own default.

-- +goose Up
CREATE TABLE mcp_settings (
  workspace_id BIGINT UNSIGNED NOT NULL,
  tool_config LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(tool_config)),
  brain_config LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(brain_config)),
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (workspace_id),
  CONSTRAINT fk_mcp_settings_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Deleting an OAuth client must take its tokens with it: that cascade IS
-- the service token's kill switch (delete the client, the machine loses its
-- key), and both token tables lacked the constraint.
ALTER TABLE oauth_access_tokens
  ADD KEY idx_access_token_client (client_pk),
  ADD CONSTRAINT fk_access_token_client FOREIGN KEY (client_pk) REFERENCES oauth_clients (id) ON DELETE CASCADE;
ALTER TABLE oauth_refresh_tokens
  ADD KEY idx_refresh_token_client (client_pk),
  ADD CONSTRAINT fk_refresh_token_client FOREIGN KEY (client_pk) REFERENCES oauth_clients (id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE oauth_refresh_tokens DROP FOREIGN KEY fk_refresh_token_client, DROP KEY idx_refresh_token_client;
ALTER TABLE oauth_access_tokens DROP FOREIGN KEY fk_access_token_client, DROP KEY idx_access_token_client;
DROP TABLE mcp_settings;
