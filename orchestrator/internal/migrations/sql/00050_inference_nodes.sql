-- Machines that add themselves, and belong to the platform rather than to a
-- workspace.
--
-- A machine running our inference engine used to be an `ai_vendors` row, which
-- made it a thing a workspace owned. That is the wrong shape: a GPU box is
-- infrastructure. It is bought once, racked once, and every workspace on the
-- deployment may have models on it. What IS a workspace's is the model row that
-- routes to one, because that is a decision about what that workspace's agents
-- may use.
--
-- So the machine gets a table of its own with no `workspace_id`, and the vendor
-- row becomes a POINTER at it: one per workspace that has been given a model
-- from that machine, carrying no address and no key of its own. Both live on the
-- machine, stored once, so there is nothing to keep in step and nothing to leak
-- twice. Deleting a machine takes its pointers with it, and their models with
-- those, which is the truth: the weights are gone.
--
-- `node_id` on the machine is minted on the machine's own disk and sent with
-- every join, so a container rescheduled onto a different address updates its
-- row instead of leaving a dead one behind and a duplicate beside it.
--
-- The join token is ONE for the deployment, for the same reason the machines
-- are: a person adding a box is adding it to the platform. It is sealed rather
-- than hashed, because an administrator has to read it back to paste it onto the
-- next machine, the way a swarm lets you ask for its join token whenever you
-- need one. A show-once token for a thing whose whole purpose is to be pasted
-- onto machine after machine is a token that ends up written down somewhere
-- worse.

-- +goose Up
CREATE TABLE `inference_nodes` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `node_id` varchar(64) NOT NULL,
  `name` varchar(255) NOT NULL,
  `base_url` varchar(512) NOT NULL,
  `key_enc` varbinary(2048) NOT NULL,
  `version` varchar(32) NOT NULL DEFAULT '',
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_node_id` (`node_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- One row, ever. The primary key is a constant so a second insert is a
-- duplicate-key error rather than a second token nobody knows about.
CREATE TABLE `node_join_token` (
  `id` tinyint(3) unsigned NOT NULL DEFAULT 1,
  `token_enc` varbinary(2048) NOT NULL,
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE `ai_vendors`
  ADD COLUMN `node_id` bigint(20) unsigned DEFAULT NULL AFTER `base_url`,
  ADD UNIQUE KEY `uniq_ws_node` (`workspace_id`, `node_id`),
  ADD CONSTRAINT `fk_vendor_node` FOREIGN KEY (`node_id`) REFERENCES `inference_nodes` (`id`) ON DELETE CASCADE;

-- Managing machines is a new area, and a permission nobody holds is a
-- capability everybody just lost. So every role that could already configure
-- where models come from gets it, which is the same reasoning migration 42 used
-- for `chats:delete`.
-- +goose StatementBegin
INSERT INTO `role_permissions` (`role_id`, `permission`)
SELECT DISTINCT rp.`role_id`, m.`permission`
FROM `role_permissions` rp
CROSS JOIN (
  SELECT 'machines:view' AS `permission`
  UNION ALL SELECT 'machines:create'
  UNION ALL SELECT 'machines:edit'
  UNION ALL SELECT 'machines:delete'
) m
WHERE rp.`permission` = 'vendors:create'
  AND NOT EXISTS (
    SELECT 1 FROM `role_permissions` existing
    WHERE existing.`role_id` = rp.`role_id` AND existing.`permission` = m.`permission`
  );
-- +goose StatementEnd

-- +goose Down
DELETE FROM `role_permissions`
WHERE `permission` IN ('machines:view', 'machines:create', 'machines:edit', 'machines:delete');

ALTER TABLE `ai_vendors`
  DROP FOREIGN KEY `fk_vendor_node`,
  DROP INDEX `uniq_ws_node`,
  DROP COLUMN `node_id`;

DROP TABLE `node_join_token`;
DROP TABLE `inference_nodes`;
