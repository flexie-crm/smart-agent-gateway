CREATE TABLE `ai_skill_files` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `skill_id` bigint(20) unsigned NOT NULL,
  `version_id` bigint(20) unsigned NOT NULL,
  `path` varchar(512) NOT NULL,
  `file_type` enum('skill','reference','script','asset','other') NOT NULL,
  `mime_type` varchar(255) DEFAULT NULL,
  `text_content` longtext DEFAULT NULL,
  `binary_content` longblob DEFAULT NULL,
  `size_bytes` bigint(20) unsigned NOT NULL,
  `sha256` char(64) NOT NULL,
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_version_path` (`version_id`,`path`),
  KEY `fk_skill_file_skill` (`skill_id`),
  KEY `idx_skill_file_sha` (`sha256`),
  CONSTRAINT `fk_skill_file_skill` FOREIGN KEY (`skill_id`) REFERENCES `ai_skills` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_skill_file_version` FOREIGN KEY (`version_id`) REFERENCES `ai_skill_versions` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
