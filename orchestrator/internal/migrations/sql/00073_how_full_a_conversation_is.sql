-- How much of its model's context window a conversation takes, as last
-- measured, so the chat can show it the moment the conversation is opened.
--
-- Measured the way the trim measures (KB/10): characters of what is sent, the
-- messages and the tools together. `context_chars` is the whole of it,
-- `context_base_chars` the part that does not come from the conversation (the
-- system prompt and the tools), which is what a compaction needs to say how
-- full the conversation is once its summary replaces what it covers.
--
-- The MODEL is kept rather than the percentage, so the percentage is worked out
-- against the window as it is set when somebody looks, not as it was set when
-- the turn ran. Removing the model forgets which one it was, and the chat then
-- shows nothing rather than a number against a window that no longer exists.

-- +goose Up
ALTER TABLE `agent_sessions`
  ADD COLUMN `context_model_id` bigint(20) unsigned DEFAULT NULL,
  ADD COLUMN `context_chars` int(11) NOT NULL DEFAULT 0,
  ADD COLUMN `context_base_chars` int(11) NOT NULL DEFAULT 0,
  ADD KEY `fk_session_context_model` (`context_model_id`),
  ADD CONSTRAINT `fk_session_context_model` FOREIGN KEY (`context_model_id`) REFERENCES `ai_models` (`id`) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE `agent_sessions`
  DROP FOREIGN KEY `fk_session_context_model`,
  DROP KEY `fk_session_context_model`,
  DROP COLUMN `context_base_chars`,
  DROP COLUMN `context_chars`,
  DROP COLUMN `context_model_id`;
