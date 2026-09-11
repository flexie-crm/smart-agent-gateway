-- The chat list and its search.
--
-- Search covers BOTH the title and what was actually said: people look for a
-- conversation by what it was about, and the title is only a summary of that.
-- Two FULLTEXT indexes rather than one, because the two live in different
-- tables and a conversation matches if either does.
--
-- last_message_at and message_count are stored rather than derived, so listing
-- the sidebar is one indexed read instead of a COUNT and a MAX per row.

-- +goose Up
ALTER TABLE agent_sessions
  ADD COLUMN is_pinned TINYINT(1) NOT NULL DEFAULT 0,
  ADD COLUMN last_message_at DATETIME(3) NULL,
  ADD COLUMN message_count INT NOT NULL DEFAULT 0;

-- The sidebar reads one user's chats, newest first, pinned on top.
CREATE INDEX idx_chat_list ON agent_sessions (workspace_id, user_id, is_pinned, last_message_at);

-- Search by title.
ALTER TABLE agent_sessions ADD FULLTEXT INDEX ft_title (title);

-- Search by what was said. A conversation is found by its content even when
-- its title never mentioned the thing you remember.
ALTER TABLE agent_messages ADD FULLTEXT INDEX ft_content (content);

-- +goose Down
ALTER TABLE agent_messages DROP INDEX ft_content;
ALTER TABLE agent_sessions DROP INDEX ft_title;
DROP INDEX idx_chat_list ON agent_sessions;
ALTER TABLE agent_sessions
  DROP COLUMN is_pinned,
  DROP COLUMN last_message_at,
  DROP COLUMN message_count;
