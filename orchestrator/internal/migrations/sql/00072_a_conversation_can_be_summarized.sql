-- A conversation's summary, standing in for the part of it the model no
-- longer needs to read word for word.
--
-- A long conversation is sent whole on every turn, tool calls and all, and the
-- only thing that ever shortens it is the trim that keeps a request under the
-- model's context window. So a conversation the person asks to compact gets a
-- summary written by the model, and from then on the next turn is built from
-- the NEWEST summary plus every step after the one it covers, instead of from
-- every step there is. The steps themselves stay exactly where they are: the
-- chat shows what was said, and this is only what the model is shown.
--
-- `through_seq` is the last step the summary covers, which is what makes it
-- usable at all: the steps after it are still sent in full, so a summary written
-- five turns ago plus those five turns is the whole conversation. A new summary
-- starts from the previous one, so they never need reading together, and the
-- newest is the only one read. The older ones stay as the record of what the
-- model was told, and go with the conversation when it is deleted.
--
-- `vendor` and `model` are the model that wrote it, frozen as text the way a
-- step records who answered it, so it survives the model being removed. And who
-- asked for it, the pair every table that holds a decision carries.

-- +goose Up
CREATE TABLE `agent_compactions` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `session_id` bigint(20) unsigned NOT NULL,
  `through_seq` int(11) NOT NULL,
  `summary` longtext NOT NULL,
  `vendor` varchar(64) DEFAULT NULL,
  `model` varchar(128) DEFAULT NULL,
  `created_by` bigint(20) unsigned DEFAULT NULL,
  `created_by_name` varchar(255) NOT NULL DEFAULT '',
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_session` (`session_id`,`id`),
  KEY `fk_compaction_created_by` (`created_by`),
  CONSTRAINT `fk_compaction_session` FOREIGN KEY (`session_id`) REFERENCES `agent_sessions` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_compaction_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE `agent_compactions`;
