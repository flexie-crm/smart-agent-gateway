-- A person belongs to the company, not to one of its workspaces.
--
-- The database is the tenant (a real tenant gets its own), and a workspace is a
-- separation inside it: sales and support share a company and share people, but
-- not each other's agents, brains and conversations. So a user pinned to a
-- single workspace_id was the wrong shape. It made the login form ask for a
-- workspace, which nobody should have to type, and it made the same colleague
-- two accounts if they worked in two places.
--
-- Membership becomes what it always was: a relationship. workspace_members says
-- where a person may act, the switcher lists exactly those workspaces, and the
-- server refuses a switch to any other. The email is now unique across the
-- tenant, which is the identity people actually think they have.
--
-- Every existing user gets a membership of the workspace they were pinned to
-- BEFORE the column goes, so nobody is locked out by this migration.

-- +goose Up
CREATE TABLE workspace_members (
  workspace_id BIGINT UNSIGNED NOT NULL,
  user_id BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (workspace_id, user_id),
  KEY idx_member_user (user_id),
  CONSTRAINT fk_ws_member_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces (id) ON DELETE CASCADE,
  CONSTRAINT fk_ws_member_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT INTO workspace_members (workspace_id, user_id, created_at)
SELECT workspace_id, id, created_at FROM users;

ALTER TABLE users DROP FOREIGN KEY fk_users_workspace;
ALTER TABLE users DROP INDEX uniq_ws_email;
ALTER TABLE users ADD UNIQUE KEY uniq_email (email);
ALTER TABLE users DROP COLUMN workspace_id;

-- +goose Down
ALTER TABLE users ADD COLUMN workspace_id BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER id;

-- Coming back down, a person who joined several workspaces has to be pinned to
-- one: the earliest they belong to, which is the one they came from.
UPDATE users u
SET u.workspace_id = COALESCE(
  (SELECT MIN(m.workspace_id) FROM workspace_members m WHERE m.user_id = u.id), 0);

ALTER TABLE users DROP INDEX uniq_email;
ALTER TABLE users ALTER COLUMN workspace_id DROP DEFAULT;
ALTER TABLE users ADD UNIQUE KEY uniq_ws_email (workspace_id, email);
ALTER TABLE users ADD CONSTRAINT fk_users_workspace
  FOREIGN KEY (workspace_id) REFERENCES workspaces (id) ON DELETE CASCADE;

DROP TABLE workspace_members;
