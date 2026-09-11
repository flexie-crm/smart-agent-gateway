-- What somebody uploaded, and what was made of it.
--
-- The row carries the name as it was given (for showing back) and the type
-- (for routing), but NOT a path. The bytes live under the file's own public id,
-- so nothing a person typed ever reaches the filesystem: a name like
-- "../../etc/passwd" is a string in a column and nothing more.
--
-- `extraction` is what the routed model made of the file: the text the Gateway
-- actually reads. It is stored rather than recomputed because re-reading the
-- same PDF on every turn would be slow, expensive, and give a different answer
-- each time.
--
-- An attachment belongs to the person who uploaded it, not to a conversation:
-- it is uploaded before the message that carries it exists.

-- +goose Up
CREATE TABLE `attachments` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `public_id` varchar(64) NOT NULL,
  `workspace_id` bigint(20) unsigned NOT NULL,
  `user_id` bigint(20) unsigned NOT NULL,
  `file_name` varchar(255) NOT NULL,
  `file_type` varchar(32) NOT NULL,
  `size_bytes` bigint(20) unsigned NOT NULL,
  `extraction` mediumtext DEFAULT NULL,
  `extracted_at` datetime(3) DEFAULT NULL,
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_attachment_public_id` (`public_id`),
  KEY `idx_attachment_owner` (`workspace_id`,`user_id`),
  CONSTRAINT `fk_attachment_workspace` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_attachment_user` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE `attachments`;
