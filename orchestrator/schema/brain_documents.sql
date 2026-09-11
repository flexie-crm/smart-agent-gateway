CREATE TABLE `brain_documents` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `brain_id` bigint(20) unsigned NOT NULL,
  `category_id` bigint(20) unsigned NOT NULL,
  `title` varchar(255) NOT NULL,
  `content` mediumtext DEFAULT NULL,
  `weight` int(11) NOT NULL DEFAULT 0,
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_category_title` (`category_id`,`title`),
  KEY `idx_brain` (`brain_id`),
  KEY `idx_category_weight` (`category_id`,`weight`),
  FULLTEXT KEY `ft_brain_document` (`title`,`content`),
  CONSTRAINT `fk_document_brain` FOREIGN KEY (`brain_id`) REFERENCES `brains` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_document_category` FOREIGN KEY (`category_id`) REFERENCES `brain_categories` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
