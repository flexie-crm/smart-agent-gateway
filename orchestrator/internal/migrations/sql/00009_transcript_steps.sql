-- The transcript is an ordered list of STEPS, not of messages.
--
-- One assistant turn is several steps: think, call tools, think again, call
-- more tools, then answer. Reasoning and tool calls are properties of a step,
-- so a single reasoning column and a single tool_call_id column on a message
-- row could hold exactly one of each, which is the case that never happens.
-- The old shape lost every intermediate step's text and reasoning, and stored
-- each tool result twice: once as a role=tool message and once on the tool
-- call row.
--
--   agent_steps        the spine: one row per step, in order
--     agent_messages     the text of the step, when it has any
--     agent_reasoning    what the model thought during the step
--     agent_tool_calls   what it called, with the arguments AND the result
--
-- A tool result is NOT a message. It belongs to the call that produced it, and
-- the model-facing role=tool message is synthesized when the transcript is
-- rebuilt. One copy, one owner.

-- +goose Up

DROP TABLE IF EXISTS tool_calls;
DROP TABLE IF EXISTS agent_messages;

CREATE TABLE agent_steps (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  session_id BIGINT UNSIGNED NOT NULL,
  -- Position in the conversation. The order IS the position.
  seq INT NOT NULL,
  kind ENUM('user','assistant') NOT NULL,
  -- Which model produced it. An assistant step can come from a different model
  -- than the one before it (a workflow changed, a specialist answered), so the
  -- attribution belongs on the step rather than on the conversation.
  vendor VARCHAR(64) NULL,
  model VARCHAR(128) NULL,
  -- is_partial marks a step still being streamed. The checkpoint writes it
  -- while the answer is arriving, and clears it when the step commits, so a
  -- process that dies mid-answer leaves the text it had rather than nothing.
  is_partial TINYINT(1) NOT NULL DEFAULT 0,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  UNIQUE KEY uniq_session_seq (session_id, seq),
  INDEX idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE agent_messages (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  step_id BIGINT UNSIGNED NOT NULL,
  -- Denormalised so the sidebar's search can reach a conversation's text in one
  -- join instead of two.
  session_id BIGINT UNSIGNED NOT NULL,
  role ENUM('user','assistant') NOT NULL,
  content LONGTEXT NULL,
  created_at DATETIME(3) NOT NULL,
  -- A step has at most one message: its text.
  UNIQUE KEY uniq_step (step_id),
  INDEX idx_session (session_id),
  FULLTEXT KEY ft_content (content)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE agent_reasoning (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  step_id BIGINT UNSIGNED NOT NULL,
  content LONGTEXT NOT NULL,
  -- The model that did the thinking, which can differ from the one that spoke.
  vendor VARCHAR(64) NULL,
  model VARCHAR(128) NULL,
  created_at DATETIME(3) NOT NULL,
  UNIQUE KEY uniq_step (step_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE agent_tool_calls (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  step_id BIGINT UNSIGNED NOT NULL,
  session_id BIGINT UNSIGNED NOT NULL,
  workspace_id BIGINT UNSIGNED NOT NULL,
  -- The vendor's own id. It is what pairs a call with its result, and every
  -- vendor validates that pairing.
  tool_call_id VARCHAR(64) NOT NULL,
  tool_name VARCHAR(64) NOT NULL,
  friendly_name VARCHAR(255) NULL,
  -- The arguments and the result live together, because they are two halves of
  -- one fact. There is no separate role=tool row holding a second copy.
  args JSON NULL,
  result JSON NULL,
  status ENUM('approval_required','running','completed','failed','rejected') NOT NULL,
  error_text TEXT NULL,
  duration_ms INT NULL,
  -- Parallel calls in one step, in the order they ran.
  execution_order INT NOT NULL DEFAULT 0,
  -- Sticky once set: a call that went through a confirmation card says so
  -- forever, including after the approved call re-persists its real result.
  requested_approval TINYINT(1) NOT NULL DEFAULT 0,
  created_at DATETIME(3) NOT NULL,
  completed_at DATETIME(3) NULL,
  UNIQUE KEY uniq_session_call (session_id, tool_call_id),
  INDEX idx_step (step_id),
  INDEX idx_ws_status (workspace_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- The park snapshot stops carrying a copy of the transcript.
--
-- It held the whole model-facing conversation as JSON, which is a frozen
-- duplicate of rows that already exist and can silently drift from them. What
-- a parked turn actually needs is the PREPARED CALL: the tool, the arguments,
-- the model, and the hash of the exact action a person is being asked to
-- approve. That is what we will run, validated before the card was shown, so
-- approval equals success. The conversation around it is rebuilt from the
-- steps, which is also what re-resolves the profile and the permissions live.
ALTER TABLE agent_park_snapshots DROP COLUMN transcript;

-- +goose Down
ALTER TABLE agent_park_snapshots ADD COLUMN transcript JSON NOT NULL;

DROP TABLE IF EXISTS agent_tool_calls;
DROP TABLE IF EXISTS agent_reasoning;
DROP TABLE IF EXISTS agent_messages;
DROP TABLE IF EXISTS agent_steps;

CREATE TABLE agent_messages (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  session_id BIGINT UNSIGNED NOT NULL,
  seq INT NOT NULL,
  role ENUM('system','user','assistant','tool') NOT NULL,
  content LONGTEXT NULL,
  reasoning LONGTEXT NULL,
  tool_call_id VARCHAR(64) NULL,
  metadata JSON NULL,
  created_at DATETIME(3) NOT NULL,
  UNIQUE KEY uniq_session_seq_role (session_id, seq, role),
  FULLTEXT KEY ft_content (content)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE tool_calls (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  session_id BIGINT UNSIGNED NOT NULL,
  seq INT NOT NULL,
  tool_call_id VARCHAR(64) NOT NULL,
  tool_name VARCHAR(64) NOT NULL,
  status ENUM('pending','approval_required','running','completed',
              'failed','rejected') NOT NULL,
  input JSON NULL,
  output JSON NULL,
  error_text TEXT NULL,
  requested_approval TINYINT(1) NOT NULL DEFAULT 0,
  duration_ms INT NULL,
  created_at DATETIME(3) NOT NULL,
  completed_at DATETIME(3) NULL,
  UNIQUE KEY uniq_session_call (session_id, tool_call_id),
  INDEX idx_ws_status (workspace_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
