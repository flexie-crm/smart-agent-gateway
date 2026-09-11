-- A join token belongs to the person who asked for it, lasts an hour, and is
-- spent when a machine uses it.
--
-- It used to be ONE row for the deployment, held indefinitely and read back
-- whenever somebody wanted to paste it onto another machine. Three things were
-- wrong with that, and they compound.
--
-- It never expired, so a token copied into a terminal in March still admitted a
-- machine in September. It was shared, so rotating it because one person's copy
-- had leaked took the token out from under everybody else mid-install, and
-- nothing recorded whose copy had leaked in the first place. And it was
-- reusable, so anybody who had ever seen it could add a machine to the fleet
-- forever, which is a machine that answers questions with the deployment's own
-- models.
--
-- One row per person fixes all three at once. Asking for a token UPDATES that
-- person's row rather than adding another, so nobody accumulates credentials
-- they have forgotten about; using it DELETES it; and there is a row again only
-- when they next ask. Two administrators adding machines at the same time each
-- hold their own and neither disturbs the other.
--
-- The token is still SEALED rather than hashed, and this is the one place the
-- usual reasoning does not apply: a machine proves it holds the token by signing
-- its request with it, and never sends it, so the server has to be able to get
-- the secret back to recompute that signature. A hash cannot be an HMAC key.
-- What replaces "we cannot read it" here is "there is almost never one to read":
-- an hour, one use.
--
-- The old row is dropped rather than migrated. Its token has no owner to give it
-- to, and the only thing that presented it was a machine, which no longer needs
-- it: a machine that has already joined re-enrols on its own key from now on.

-- +goose Up
CREATE TABLE `node_join_tokens` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint(20) unsigned NOT NULL,
  `token_enc` varbinary(2048) NOT NULL,
  `expires_at` datetime(3) NOT NULL,
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_join_token_user` (`user_id`),
  KEY `idx_join_token_expiry` (`expires_at`),
  CONSTRAINT `fk_join_token_user` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE `node_join_token`;

-- +goose Down
CREATE TABLE `node_join_token` (
  `id` tinyint(3) unsigned NOT NULL DEFAULT 1,
  `token_enc` varbinary(2048) NOT NULL,
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE `node_join_tokens`;
