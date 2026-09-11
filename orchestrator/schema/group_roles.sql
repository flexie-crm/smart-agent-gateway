CREATE TABLE `group_roles` (
  `group_id` bigint(20) unsigned NOT NULL,
  `role_id` bigint(20) unsigned NOT NULL,
  PRIMARY KEY (`group_id`,`role_id`),
  KEY `fk_group_role_role` (`role_id`),
  CONSTRAINT `fk_group_role_group` FOREIGN KEY (`group_id`) REFERENCES `user_groups_def` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_group_role_role` FOREIGN KEY (`role_id`) REFERENCES `roles` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
