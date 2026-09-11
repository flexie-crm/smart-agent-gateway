CREATE TABLE `brain_categories` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `brain_id` bigint(20) unsigned NOT NULL,
  `name` varchar(120) NOT NULL,
  `description` text DEFAULT NULL,
  `weight` int(11) NOT NULL DEFAULT 0,
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_brain_weight` (`brain_id`,`weight`),
  CONSTRAINT `fk_category_brain` FOREIGN KEY (`brain_id`) REFERENCES `brains` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
