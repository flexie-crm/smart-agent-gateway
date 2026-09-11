-- OAuth 2.1 authorization-server tables (SAG as AS for the MCP surface and
-- the future REST API), mirroring the Flexie CRM oauth_* design: opaque
-- tokens stored only as SHA-256 hashes, bcrypt client secrets, refresh
-- rotation with family lineage. Divergence from the CRM: authorization
-- codes live in a DB table (single-use via atomic DELETE), not Redis.
-- Plus mcp_servers: outbound MCP-client connections (SAG as MCP client),
-- authenticated by API key or OAuth, both supported.

-- +goose Up
CREATE TABLE oauth_clients (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NULL,        -- NULL = platform-level (DCR)
  client_id VARCHAR(64) NOT NULL UNIQUE,
  client_secret_hash VARCHAR(255) NULL,     -- bcrypt; NULL for public clients
  client_type ENUM('public','confidential','service') NOT NULL,
  name VARCHAR(255) NOT NULL,
  redirect_uris JSON NOT NULL,
  grant_types JSON NOT NULL,
  scopes JSON NOT NULL,
  is_dcr TINYINT(1) NOT NULL DEFAULT 0,
  status ENUM('active','disabled') NOT NULL DEFAULT 'active',
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE oauth_auth_codes (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  code_hash CHAR(64) NOT NULL UNIQUE,       -- sha256 hex; single-use via DELETE
  client_pk BIGINT UNSIGNED NOT NULL,
  user_id BIGINT UNSIGNED NOT NULL,
  workspace_id BIGINT UNSIGNED NOT NULL,
  redirect_uri VARCHAR(512) NOT NULL,
  scopes JSON NOT NULL,
  code_challenge VARCHAR(128) NOT NULL,     -- PKCE S256 only
  expires_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  INDEX idx_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE oauth_access_tokens (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  token_hash CHAR(64) NOT NULL UNIQUE,
  client_pk BIGINT UNSIGNED NOT NULL,
  user_id BIGINT UNSIGNED NOT NULL,         -- tokens are always user-bound
  workspace_id BIGINT UNSIGNED NOT NULL,
  scopes JSON NOT NULL,
  audience VARCHAR(128) NULL,
  family_id CHAR(32) NULL,                  -- refresh family that minted it
  expires_at DATETIME(3) NOT NULL,
  revoked TINYINT(1) NOT NULL DEFAULT 0,
  created_at DATETIME(3) NOT NULL,
  INDEX idx_family (family_id),
  INDEX idx_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE oauth_refresh_tokens (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  token_hash CHAR(64) NOT NULL UNIQUE,
  client_pk BIGINT UNSIGNED NOT NULL,
  user_id BIGINT UNSIGNED NOT NULL,
  workspace_id BIGINT UNSIGNED NOT NULL,
  scopes JSON NOT NULL,
  family_id CHAR(32) NOT NULL,              -- rotation lineage
  parent_id BIGINT UNSIGNED NULL,
  used TINYINT(1) NOT NULL DEFAULT 0,       -- presenting a used token = theft
  revoked TINYINT(1) NOT NULL DEFAULT 0,
  expires_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  INDEX idx_family (family_id),
  INDEX idx_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE oauth_consents (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  user_id BIGINT UNSIGNED NOT NULL,
  client_pk BIGINT UNSIGNED NOT NULL,
  scopes JSON NOT NULL,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  UNIQUE KEY uniq_user_client (user_id, client_pk)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Outbound MCP connections: SAG as MCP *client* to external tool servers.
-- auth_type covers both styles: static API key/bearer, or OAuth where SAG
-- self-onboards at the remote AS (DCR) and holds rotating tokens.
CREATE TABLE mcp_servers (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(255) NOT NULL,
  url VARCHAR(512) NOT NULL,
  auth_type ENUM('none','api_key','oauth') NOT NULL DEFAULT 'none',
  api_key_enc VARBINARY(2048) NULL,
  oauth_client_id VARCHAR(255) NULL,        -- our client id at the remote AS
  oauth_client_secret_enc VARBINARY(2048) NULL,
  oauth_access_token_enc VARBINARY(4096) NULL,
  oauth_refresh_token_enc VARBINARY(4096) NULL,
  oauth_token_expires_at DATETIME(3) NULL,
  oauth_metadata JSON NULL,                 -- cached remote discovery document
  status ENUM('active','disabled') NOT NULL DEFAULT 'active',
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  UNIQUE KEY uniq_ws_name (workspace_id, name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE IF EXISTS mcp_servers;
DROP TABLE IF EXISTS oauth_consents;
DROP TABLE IF EXISTS oauth_refresh_tokens;
DROP TABLE IF EXISTS oauth_access_tokens;
DROP TABLE IF EXISTS oauth_auth_codes;
DROP TABLE IF EXISTS oauth_clients;
