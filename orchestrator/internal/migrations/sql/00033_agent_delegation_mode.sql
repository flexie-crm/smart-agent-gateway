-- A sub-agent can pin HOW the master runs it, rather than leaving it to the
-- master to infer from free-text instructions (KB/27). 'auto' (the default and
-- what every existing row gets) leaves the choice to the master; 'background'
-- pins Mode C (the master starts it and is brought back with the result);
-- 'inline' pins the synchronous modes (the master delegates and waits within the
-- turn). When a specialist is pinned, the subagent tool no longer offers a mode
-- for it, so the master cannot override the administrator's choice.

-- +goose Up
ALTER TABLE `agents`
  ADD COLUMN `delegation_mode` enum('auto','background','inline') NOT NULL DEFAULT 'auto' AFTER `status`;

-- +goose Down
ALTER TABLE `agents` DROP COLUMN `delegation_mode`;
