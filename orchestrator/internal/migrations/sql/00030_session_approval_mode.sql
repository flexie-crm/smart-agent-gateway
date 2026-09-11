-- A conversation can auto-approve for the rest of the session.
--
-- Manual is the floor and the default: every approval-gated action asks. A
-- person who trusts what they are doing can switch a session to automatic, and
-- then the master AND every sub-agent it starts run those actions without a
-- card, for that session only. It is the person's explicit, reversible choice,
-- scoped to the one conversation, never a global setting.

-- +goose Up
ALTER TABLE agent_sessions
  ADD COLUMN approval_mode ENUM('manual','auto') NOT NULL DEFAULT 'manual' AFTER status;

-- +goose Down
ALTER TABLE agent_sessions
  DROP COLUMN approval_mode;
