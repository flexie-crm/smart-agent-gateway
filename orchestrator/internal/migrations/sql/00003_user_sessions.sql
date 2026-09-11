-- First-party login sessions for the app and /v1 API: short-lived JWT
-- access tokens are stateless, refresh tokens are rows here so logout and
-- revocation are real. Same discipline as the OAuth tables: only the
-- SHA-256 hash of the token is stored, and rotation makes each refresh
-- token single use.

-- +goose Up
CREATE TABLE user_sessions (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  token_hash CHAR(64) NOT NULL UNIQUE,
  user_id BIGINT UNSIGNED NOT NULL,
  workspace_id BIGINT UNSIGNED NOT NULL,
  user_agent VARCHAR(255) NULL,
  ip VARCHAR(45) NULL,
  used TINYINT(1) NOT NULL DEFAULT 0,      -- rotated away, replay is rejected
  revoked TINYINT(1) NOT NULL DEFAULT 0,   -- logout or admin revocation
  expires_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  INDEX idx_user (user_id),
  INDEX idx_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE IF EXISTS user_sessions;
