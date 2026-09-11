-- What reads what a person uploads.
--
-- A screenshot, a PDF, a spreadsheet, a recording: none of them is a prompt, so
-- something has to turn each one into text before the Gateway can think about
-- it. This is where an administrator says what does that.
--
-- Files and audio are kept apart because they are different questions with
-- different answers. Reading a file means handing it to a model along with a
-- prompt; reading audio means transcribing it, which is a different kind of
-- model and a different call. Putting them in one list would have made the
-- form ask one question and mean two.
--
-- FILES are a LIST OF RULES rather than a fixed set of kinds, because how
-- finely you want to split them is a business question, not ours. One rule
-- covering everything is one model for all files; several rules is a cheap
-- model for screenshots and an expensive one for contracts. Rules are tried in
-- `position` order and the first match wins, so a rule with an empty
-- `file_types` is the catch-all and belongs last.
--
-- AUDIO is one model, so it is a column rather than a table.
--
-- Both hang off the Gateway (the agents row keyed 'default'), because this is
-- the Gateway's configuration: an agent it delegates to is handed the text that
-- came out of this and never the file.

-- +goose Up
CREATE TABLE `gateway_file_rules` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `agent_id` bigint(20) unsigned NOT NULL,
  `position` int(10) unsigned NOT NULL,
  -- A comma-separated list of file types, lower case, without dots
  -- ("pdf,docx,xlsx"). Empty means every type: the catch-all.
  `file_types` varchar(255) NOT NULL,
  `model_id` bigint(20) unsigned NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_agent_position` (`agent_id`,`position`),
  KEY `idx_file_rule_model` (`model_id`),
  CONSTRAINT `fk_file_rule_agent` FOREIGN KEY (`agent_id`) REFERENCES `agents` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_file_rule_model` FOREIGN KEY (`model_id`) REFERENCES `ai_models` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE `agents`
  ADD COLUMN `audio_model_id` bigint(20) unsigned DEFAULT NULL AFTER `memory_brain_id`,
  ADD KEY `fk_agent_audio_model` (`audio_model_id`),
  ADD CONSTRAINT `fk_agent_audio_model` FOREIGN KEY (`audio_model_id`)
    REFERENCES `ai_models` (`id`) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE `agents`
  DROP FOREIGN KEY `fk_agent_audio_model`,
  DROP KEY `fk_agent_audio_model`,
  DROP COLUMN `audio_model_id`;

DROP TABLE `gateway_file_rules`;
