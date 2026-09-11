-- What the assistant remembers, in two scopes.
--
-- ai_user_memory is a person's context: who they are, how they like their
-- answers, facts worth carrying between conversations. One row per person per
-- workspace.
--
-- ai_workspace_memory is the workspace's own working notes: the assistant's
-- technical memory about how to use its tools and what it got wrong last time,
-- so the loop can improve itself. One row per workspace.
--
-- Both are curated by the assistant through the `remember` tool and read back
-- into its prompt every turn. Two tables, not one nullable column, because a
-- NULL in a unique key is not unique in MySQL, and "one note per workspace" has
-- to actually be one.

-- +goose Up
CREATE TABLE ai_user_memory (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  workspace_id BIGINT UNSIGNED NOT NULL,
  user_id BIGINT UNSIGNED NOT NULL,
  content MEDIUMTEXT NOT NULL,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uniq_user_memory (workspace_id, user_id),
  KEY fk_user_memory_user (user_id),
  CONSTRAINT fk_user_memory_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces (id) ON DELETE CASCADE,
  CONSTRAINT fk_user_memory_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE ai_workspace_memory (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  workspace_id BIGINT UNSIGNED NOT NULL,
  content MEDIUMTEXT NOT NULL,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uniq_workspace_memory (workspace_id),
  CONSTRAINT fk_workspace_memory_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE ai_workspace_memory;
DROP TABLE ai_user_memory;
