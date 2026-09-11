-- Brains used to have only view and edit, and edit covered creating and
-- deleting whole brains. The catalog now splits those out (brains:create,
-- brains:delete), with edit keeping everything inside a brain: categories,
-- documents, links.
--
-- A role that could curate brains yesterday keeps the full ability it had:
-- tightening the routes must not silently strip a capability from a running
-- deployment. Narrowing a specific role back down is an administrator's
-- decision, made in the roles form, not a side effect of an upgrade.

-- +goose Up
INSERT IGNORE INTO role_permissions (role_id, permission)
SELECT role_id, 'brains:create' FROM role_permissions WHERE permission = 'brains:edit';
INSERT IGNORE INTO role_permissions (role_id, permission)
SELECT role_id, 'brains:delete' FROM role_permissions WHERE permission = 'brains:edit';

-- +goose Down
DELETE FROM role_permissions WHERE permission IN ('brains:create', 'brains:delete');
