-- Confirmation park-and-resume.
--
-- When a tool needs approval, the turn stops and everything needed to finish
-- it later is written here. The stream then CLOSES: no connection is held
-- open waiting for a human. Approval arrives as a fresh request carrying the
-- token, and the runtime rebuilds the turn from this row.
--
-- The snapshot lives in the database rather than a cache because an approval
-- that silently evaporates is worse than one that is refused: the user clicks
-- approve and nothing happens. A row with an expiry can say "this expired",
-- and it survives a restart of the process that parked it.

-- +goose Up
CREATE TABLE agent_park_snapshots (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  token_hash CHAR(64) NOT NULL UNIQUE,   -- sha256 of the token handed to the client
  workspace_id BIGINT UNSIGNED NOT NULL,
  session_id BIGINT UNSIGNED NOT NULL,
  user_id BIGINT UNSIGNED NOT NULL,      -- only this user may resolve it
  agent_id BIGINT UNSIGNED NULL,
  model_id BIGINT UNSIGNED NOT NULL,
  tool_name VARCHAR(64) NOT NULL,
  tool_call_id VARCHAR(64) NOT NULL,
  tool_args JSON NOT NULL,
  -- The model-visible transcript at the moment of parking, so the resumed
  -- turn continues from exactly where it stopped.
  transcript JSON NOT NULL,
  -- action_hash is hash(tool_name + canonical args): the same action must not
  -- ask twice within one turn, and an approval is consumed once.
  action_hash CHAR(64) NOT NULL,
  status ENUM('pending','approved','rejected','expired') NOT NULL DEFAULT 'pending',
  expires_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  resolved_at DATETIME(3) NULL,
  INDEX idx_session (session_id),
  INDEX idx_expires (expires_at),
  INDEX idx_ws_status (workspace_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE IF EXISTS agent_park_snapshots;
