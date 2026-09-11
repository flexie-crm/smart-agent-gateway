CREATE TABLE `brain_document_links` (
  `document_id` bigint(20) unsigned NOT NULL,
  `related_id` bigint(20) unsigned NOT NULL,
  PRIMARY KEY (`document_id`,`related_id`),
  KEY `idx_related` (`related_id`),
  CONSTRAINT `fk_link_document` FOREIGN KEY (`document_id`) REFERENCES `brain_documents` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_link_related` FOREIGN KEY (`related_id`) REFERENCES `brain_documents` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
