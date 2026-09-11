-- The authority that lets a machine sit on a public address.
--
-- Migration 50 gave machines a table and a join token, on the premise KB/35
-- wrote down: a machine lives inside one network, so plain HTTP and a sealed key
-- are enough. Putting one on a public address breaks that premise. A key sent
-- over plain HTTP on the open internet is a key anybody in the path can read,
-- and every later call carries it.
--
-- So the two ends authenticate each other with certificates, and this is where
-- the thing that issues them lives. One row, like the join token, for the same
-- reason: it belongs to the deployment. The key is sealed with the keyring; the
-- certificate is not, because it is public by construction, handed to every
-- machine that joins and to anybody who asks.
--
-- Deliberately NOT a public authority. A public one vouches that somebody
-- controls a DOMAIN, which is meaningless between two machines you own and
-- address by IP, and it would put a renewal dependency on a third party into the
-- path between us and our own hardware. What has to be true here is that the
-- machine on the other end is the machine that joined, and that is exactly what
-- a private authority can say.
--
-- `cert_expires_at` on the machine is the leaf we issued it, kept so the console
-- can say when it needs renewing. It is nullable because a machine that joined
-- before this migration has no certificate until it comes back.

-- +goose Up
CREATE TABLE `node_authority` (
  `id` tinyint(3) unsigned NOT NULL DEFAULT 1,
  `key_enc` varbinary(2048) NOT NULL,
  `cert_pem` text NOT NULL,
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE `inference_nodes`
  ADD COLUMN `cert_expires_at` datetime(3) DEFAULT NULL AFTER `version`;

-- +goose Down
ALTER TABLE `inference_nodes`
  DROP COLUMN `cert_expires_at`;

DROP TABLE `node_authority`;
