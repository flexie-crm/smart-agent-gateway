-- A turn is a RUN: a unit of work with a life of its own.
--
-- It used to execute on the HTTP request that asked for it, which meant the
-- browser tab owned the vendor call: close the tab and the answer was cancelled
-- mid-sentence and lost. A run outlives its reader. The request starts it and
-- listens to it; if the listener goes away the run carries on, keeps writing
-- steps to the transcript, and can be listened to again.
--
-- The row is the durable record of that work: what it belongs to, who asked for
-- it, and how it ended. It is also the seam for the queue: a worker claiming a
-- run later needs exactly this row, and nothing here assumes the process that
-- created it is the process that runs it.
--
-- The FRAMES a run emits are not stored here. They are the live stream, and
-- they are already durable in a better form: every step is written to the
-- transcript as it happens, and checkpointed while it is still arriving. A
-- second copy of the same words, one row per token, would buy nothing and cost
-- a write per token.

-- +goose Up
CREATE TABLE agent_runs (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  -- The identifier a client holds. The numeric id never leaves the server.
  uid VARCHAR(32) NOT NULL,
  workspace_id BIGINT UNSIGNED NOT NULL,
  session_id BIGINT UNSIGNED NOT NULL,
  user_id BIGINT UNSIGNED NOT NULL,
  -- interrupted is a process that died holding a run: the answer stops
  -- mid-sentence, and the conversation says so rather than pretending the
  -- silence was an answer.
  status ENUM('running','completed','failed','waiting_approval',
              'cancelled','interrupted') NOT NULL DEFAULT 'running',
  created_at DATETIME(3) NOT NULL,
  completed_at DATETIME(3) NULL,
  UNIQUE KEY uniq_uid (uid),
  INDEX idx_session (session_id, id),
  INDEX idx_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE IF EXISTS agent_runs;
