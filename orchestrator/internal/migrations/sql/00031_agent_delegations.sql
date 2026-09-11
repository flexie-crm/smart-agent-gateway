-- A delegation is a durable record, so a background one can be tracked.
--
-- Synchronous delegation (continue/terminal) finishes inside the turn and needs
-- nothing here. A background delegation (Mode C, KB/27) outlives the turn: the
-- master starts a specialist as a goroutine, ends the turn, and the person keeps
-- chatting. This row is the source of truth the chip reads and the completion
-- turn resolves, and it makes the delegation mode first-class for every mode.
--
-- parent_tool_call_id is the master's `subagent` call this delegation runs under,
-- the same handle the transcript and the resume already use (KB/11).

-- +goose Up
CREATE TABLE `agent_delegations` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `session_id` bigint(20) unsigned NOT NULL,
  `workspace_id` bigint(20) unsigned NOT NULL,
  `parent_tool_call_id` varchar(64) NOT NULL,
  `agent_key` varchar(64) NOT NULL,
  `mode` varchar(16) NOT NULL,
  `status` enum('running','done','failed','cancelled') NOT NULL DEFAULT 'running',
  `progress` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL CHECK (json_valid(`progress`)),
  `result` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL CHECK (json_valid(`result`)),
  `error_text` text DEFAULT NULL,
  `created_at` datetime(3) NOT NULL,
  `completed_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_session` (`session_id`),
  KEY `idx_session_status` (`session_id`,`status`),
  KEY `fk_delegation_workspace` (`workspace_id`),
  CONSTRAINT `fk_delegation_session` FOREIGN KEY (`session_id`) REFERENCES `agent_sessions` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_delegation_workspace` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE `agent_delegations`;
