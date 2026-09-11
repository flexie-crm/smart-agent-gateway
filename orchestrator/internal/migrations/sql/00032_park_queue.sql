-- Several background specialists (KB/27) can reach an approval-gated tool at the
-- same moment. Rather than flood the person with a card each, the server shows
-- one at a time: the first park is live (a card), the rest wait as 'queued', and
-- each is released to 'pending' when the one before it is answered. This adds the
-- 'queued' state the sequencing needs; a park is never created queued unless a
-- live one already exists, so existing rows are unaffected.

-- +goose Up
ALTER TABLE `agent_park_snapshots`
  MODIFY COLUMN `status` enum('queued','pending','approved','rejected','expired') NOT NULL DEFAULT 'pending';

-- +goose Down
ALTER TABLE `agent_park_snapshots`
  MODIFY COLUMN `status` enum('pending','approved','rejected','expired') NOT NULL DEFAULT 'pending';
