-- Refresh-token reuse detection (OAuth 2.0 Security BCP). family_id ties every
-- session in a rotation chain together (a login and the refreshes descended from
-- it), so replaying a spent token can revoke the whole family and sever a thief's
-- rotated chain instead of leaving it live. Rows created before this migration
-- keep the empty family and start a real one on their next rotation; RevokeFamily
-- treats the empty family as no-op, so a legacy session's reuse fails without a
-- too-broad sweep.

-- +goose Up
ALTER TABLE `user_sessions`
  ADD COLUMN `family_id` varchar(64) NOT NULL DEFAULT '' AFTER `token_hash`,
  ADD KEY `idx_family` (`family_id`);

-- +goose Down
ALTER TABLE `user_sessions`
  DROP KEY `idx_family`,
  DROP COLUMN `family_id`;
