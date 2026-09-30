-- Who made it, on everything somebody DECIDED.
--
-- The rule, so the next table needs no judgement call: a row gets these columns
-- when somebody decided it. Configuration, and content somebody authored.
--
-- What is deliberately left out, with the reason per group:
--
--   * a RECORD OF WHAT HAPPENED (a turn, a step, a tool call, a delegation, a
--     job, a model call, an attachment, a memory note). Those already carry the
--     user_id of the person it happened for, and "who created this tool call"
--     could only ever repeat it.
--   * a JOIN TABLE (agent_allowed_tools, agent_brains, agent_confirm_tools,
--     gateway_file_rules, brain_document_links, group_roles, role_permissions,
--     tool_grants, user_group_members, workspace_members, workflow_assignments).
--     Every one is rewritten wholesale when its parent is saved, so the parent's
--     updated_by already says who did it; four columns on a two-column table
--     would be noise that can go stale.
--   * a CREDENTIAL or a protocol artefact (the oauth tokens, codes and consents,
--     a user session, a node join token, the node authority, the model library
--     credential). Minted, not authored.
--   * user_settings, where the author is the key.
--   * audit_logs, which is an event log with its own actor enum and a different
--     question.
--
-- Two columns where a row cannot be edited, four where it can. An updated pair
-- on an immutable row is two columns nothing ever writes, and a promise the
-- data does not keep: workflow_versions gets the first pair only.
--
-- The id is nullable and goes to NULL when the person is deleted; the name is
-- FROZEN at the write and survives them, because a record of who did something
-- that disappears with their account rewrites the history every time somebody
-- leaves. The name is a display string rather than strictly a person's name: an
-- agent has no user row at all, so for its writes the id is nothing and the name
-- is what should be printed.
--
-- WORKFLOWS ALREADY HAD A created_by, and it was the broken version of this: a
-- bare NOT NULL bigint with no foreign key and no name, so deleting the person
-- left a row pointing at an id that no longer exists with nothing to print. It
-- becomes nullable here, gets the key it never had, and gets a name. Its
-- existing values are KEPT: they are real user ids, and the ones that still
-- resolve go on resolving.
--
-- Nothing else is backfilled. Every row that exists was written before any of
-- this was kept, and a blank reads as "before this was recorded" while a guess
-- would read as fact.

-- +goose Up

-- The dangling ids first. A workflow whose author was deleted points at nobody,
-- and the foreign key below would refuse the row: an id that resolves to no
-- user is not a record of anything, so it becomes the NULL it should have been.
-- +goose StatementBegin
UPDATE `workflows` w SET w.`created_by` = NULL
  WHERE w.`created_by` IS NOT NULL
    AND NOT EXISTS (SELECT 1 FROM `users` u WHERE u.`id` = w.`created_by`);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE `workflow_versions` v SET v.`created_by` = NULL
  WHERE v.`created_by` IS NOT NULL
    AND NOT EXISTS (SELECT 1 FROM `users` u WHERE u.`id` = v.`created_by`);
-- +goose StatementEnd

ALTER TABLE `users` ADD COLUMN `created_by` bigint unsigned NULL, ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '', ADD COLUMN `updated_by` bigint unsigned NULL, ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '', ADD INDEX `fk_user_created_by` (`created_by`), ADD INDEX `fk_user_updated_by` (`updated_by`), ADD CONSTRAINT `fk_user_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL, ADD CONSTRAINT `fk_user_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL;
ALTER TABLE `ai_models` ADD COLUMN `created_by` bigint unsigned NULL, ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '', ADD COLUMN `updated_by` bigint unsigned NULL, ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '', ADD INDEX `fk_aimodel_created_by` (`created_by`), ADD INDEX `fk_aimodel_updated_by` (`updated_by`), ADD CONSTRAINT `fk_aimodel_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL, ADD CONSTRAINT `fk_aimodel_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL;
ALTER TABLE `ai_vendors` ADD COLUMN `created_by` bigint unsigned NULL, ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '', ADD COLUMN `updated_by` bigint unsigned NULL, ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '', ADD INDEX `fk_vendor_created_by` (`created_by`), ADD INDEX `fk_vendor_updated_by` (`updated_by`), ADD CONSTRAINT `fk_vendor_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL, ADD CONSTRAINT `fk_vendor_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL;
ALTER TABLE `inference_nodes` ADD COLUMN `created_by` bigint unsigned NULL, ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '', ADD COLUMN `updated_by` bigint unsigned NULL, ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '', ADD INDEX `fk_node_created_by` (`created_by`), ADD INDEX `fk_node_updated_by` (`updated_by`), ADD CONSTRAINT `fk_node_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL, ADD CONSTRAINT `fk_node_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL;
ALTER TABLE `mcp_servers` ADD COLUMN `created_by` bigint unsigned NULL, ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '', ADD COLUMN `updated_by` bigint unsigned NULL, ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '', ADD INDEX `fk_mcp_server_created_by` (`created_by`), ADD INDEX `fk_mcp_server_updated_by` (`updated_by`), ADD CONSTRAINT `fk_mcp_server_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL, ADD CONSTRAINT `fk_mcp_server_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL;
ALTER TABLE `mcp_settings` ADD COLUMN `created_by` bigint unsigned NULL, ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '', ADD COLUMN `updated_by` bigint unsigned NULL, ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '', ADD INDEX `fk_mcp_setting_created_by` (`created_by`), ADD INDEX `fk_mcp_setting_updated_by` (`updated_by`), ADD CONSTRAINT `fk_mcp_setting_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL, ADD CONSTRAINT `fk_mcp_setting_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL;
ALTER TABLE `oauth_clients` ADD COLUMN `created_by` bigint unsigned NULL, ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '', ADD COLUMN `updated_by` bigint unsigned NULL, ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '', ADD INDEX `fk_oauth_client_created_by` (`created_by`), ADD INDEX `fk_oauth_client_updated_by` (`updated_by`), ADD CONSTRAINT `fk_oauth_client_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL, ADD CONSTRAINT `fk_oauth_client_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL;
ALTER TABLE `roles` ADD COLUMN `created_by` bigint unsigned NULL, ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '', ADD COLUMN `updated_by` bigint unsigned NULL, ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '', ADD INDEX `fk_role_created_by` (`created_by`), ADD INDEX `fk_role_updated_by` (`updated_by`), ADD CONSTRAINT `fk_role_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL, ADD CONSTRAINT `fk_role_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL;
ALTER TABLE `tools` ADD COLUMN `created_by` bigint unsigned NULL, ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '', ADD COLUMN `updated_by` bigint unsigned NULL, ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '', ADD INDEX `fk_tool_created_by` (`created_by`), ADD INDEX `fk_tool_updated_by` (`updated_by`), ADD CONSTRAINT `fk_tool_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL, ADD CONSTRAINT `fk_tool_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL;
ALTER TABLE `user_groups_def` ADD COLUMN `created_by` bigint unsigned NULL, ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '', ADD COLUMN `updated_by` bigint unsigned NULL, ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '', ADD INDEX `fk_group_created_by` (`created_by`), ADD INDEX `fk_group_updated_by` (`updated_by`), ADD CONSTRAINT `fk_group_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL, ADD CONSTRAINT `fk_group_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL;
ALTER TABLE `workspaces` ADD COLUMN `created_by` bigint unsigned NULL, ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '', ADD COLUMN `updated_by` bigint unsigned NULL, ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '', ADD INDEX `fk_workspace_created_by` (`created_by`), ADD INDEX `fk_workspace_updated_by` (`updated_by`), ADD CONSTRAINT `fk_workspace_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL, ADD CONSTRAINT `fk_workspace_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL;

ALTER TABLE `workflow_versions` MODIFY COLUMN `created_by` bigint unsigned NULL, ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '', ADD CONSTRAINT `fk_workflow_version_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL;
ALTER TABLE `workflows` MODIFY COLUMN `created_by` bigint unsigned NULL, ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '', ADD COLUMN `updated_by` bigint unsigned NULL, ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '', ADD CONSTRAINT `fk_workflow_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL, ADD CONSTRAINT `fk_workflow_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL;


-- The two statements above carry a MODIFY, which makes MariaDB REBUILD the
-- table, and a rebuild is where an explicit index collides with the one the
-- foreign key creates for itself: ADD INDEX `fk_x` followed by ADD CONSTRAINT
-- `fk_x` in the same rebuild is "Duplicate key name". So the index is left to
-- the constraint, which names it after itself and lands on the same shape the
-- declared schema asks for. It only bites when the table HAS ROWS (an empty one
-- takes the instant path), which is why it passed on a fresh database and
-- failed the moment a test seeded a workflow.

-- +goose Down

-- The columns go, and `created_by` stays NULLABLE on the two workflow tables.
--
-- That is the one asymmetry here, and it is deliberate: the up step turned
-- dangling ids into NULLs, and a down step cannot invent the user ids it
-- cleared. Restoring NOT NULL would mean writing a number that points at
-- nobody, which is exactly the state this migration exists to end. Rolling
-- back gives you the old columns without a constraint that was wrong; running
-- up again is safe, because MODIFY onto the same type is a no-op.

ALTER TABLE `workflow_versions`
  DROP FOREIGN KEY `fk_workflow_version_created_by`,
  DROP COLUMN `created_by_name`;

ALTER TABLE `workflows`
  DROP FOREIGN KEY `fk_workflow_created_by`,
  DROP FOREIGN KEY `fk_workflow_updated_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;

ALTER TABLE `inference_nodes`
  DROP FOREIGN KEY `fk_node_created_by`,
  DROP FOREIGN KEY `fk_node_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;

ALTER TABLE `oauth_clients`
  DROP FOREIGN KEY `fk_oauth_client_created_by`,
  DROP FOREIGN KEY `fk_oauth_client_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;

ALTER TABLE `mcp_settings`
  DROP FOREIGN KEY `fk_mcp_setting_created_by`,
  DROP FOREIGN KEY `fk_mcp_setting_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;

ALTER TABLE `mcp_servers`
  DROP FOREIGN KEY `fk_mcp_server_created_by`,
  DROP FOREIGN KEY `fk_mcp_server_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;

ALTER TABLE `tools`
  DROP FOREIGN KEY `fk_tool_created_by`,
  DROP FOREIGN KEY `fk_tool_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;

ALTER TABLE `ai_models`
  DROP FOREIGN KEY `fk_aimodel_created_by`,
  DROP FOREIGN KEY `fk_aimodel_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;

ALTER TABLE `ai_vendors`
  DROP FOREIGN KEY `fk_vendor_created_by`,
  DROP FOREIGN KEY `fk_vendor_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;

ALTER TABLE `roles`
  DROP FOREIGN KEY `fk_role_created_by`,
  DROP FOREIGN KEY `fk_role_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;

ALTER TABLE `user_groups_def`
  DROP FOREIGN KEY `fk_group_created_by`,
  DROP FOREIGN KEY `fk_group_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;

ALTER TABLE `users`
  DROP FOREIGN KEY `fk_user_created_by`,
  DROP FOREIGN KEY `fk_user_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;

ALTER TABLE `workspaces`
  DROP FOREIGN KEY `fk_workspace_created_by`,
  DROP FOREIGN KEY `fk_workspace_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;
