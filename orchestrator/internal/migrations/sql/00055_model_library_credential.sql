-- One credential for the model library, held by the deployment.
--
-- Some weights are published under terms somebody has to accept before they can
-- be downloaded. Until now the only way to hold the credential that proves the
-- acceptance was `SAG_NODE_HUB_TOKEN` in a machine's environment, which means
-- editing a unit file and restarting a node to add a model, and means the
-- personal edition (where there is no unit file and no terminal) could not do it
-- at all: the screen said the machine had no credentials and there was nowhere
-- to put any.
--
-- ONE ROW, not one per machine, and that is a decision worth writing down. The
-- credential belongs to a PERSON'S account with the library, not to a box: the
-- same token is what every machine would use, and storing it per machine means
-- pasting it again for every GPU racked and rotating it in as many places. So it
-- sits here, sealed, and is handed to each machine over the connection we
-- already authenticate both ends of.
--
-- The same shape as `node_authority` above it, for the same reason: a deployment
-- has exactly one, and a table with one row states that better than a settings
-- key nothing constrains.
--
-- `updated_by` is who put it there. Kept because this is a credential belonging
-- to a named human being rather than to the installation, so the person who has
-- to rotate it when it expires is a fact worth having, and it is nullable
-- because a person can be deleted while their token is still in use.
--
-- What is NOT here is any record of which models it was used for, or what it can
-- reach. That is the library's business and it changes without telling us: a
-- copy of it here would be a second answer that goes stale, and the honest way
-- to find out is to ask, which is what Test does.

-- +goose Up
CREATE TABLE `model_library_credential` (
  `id` tinyint(3) unsigned NOT NULL DEFAULT 1,
  `token_enc` varbinary(2048) NOT NULL,
  `updated_by` bigint(20) unsigned DEFAULT NULL,
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_model_library_credential_updated_by` (`updated_by`),
  CONSTRAINT `fk_model_library_credential_user` FOREIGN KEY (`updated_by`)
    REFERENCES `users` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE `model_library_credential`;
