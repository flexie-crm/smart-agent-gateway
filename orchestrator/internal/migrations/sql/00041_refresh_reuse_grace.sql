-- WHEN a refresh token was spent, so a replay can be told from a theft.
--
-- Rotation is single-use: refreshing marks the old token used and issues a new
-- one, and presenting a used token again revokes the whole family as a stolen
-- chain (OAuth 2.0 BCP reuse detection). That is right, and it has a blind spot
-- with teeth: the server can spend a token and the client can never receive its
-- replacement. The response dies in flight, the process is killed mid-write, a
-- phone loses signal. The browser still holds the old cookie, presents it, and
-- is treated as an attacker for a failure that was ours.
--
-- It is not theoretical. A development rebuild takes about twenty seconds, and
-- anyone whose page refreshed into that window was signed out of a session that
-- was in no way compromised, with no way back: the family was already revoked.
--
-- A timestamp is all that was missing. A replay seconds after the rotation, from
-- the same browser, is a retry; the same replay an hour later, or from a
-- different browser, is what reuse detection is for.

-- +goose Up
ALTER TABLE `user_sessions`
  ADD COLUMN `used_at` datetime(3) DEFAULT NULL AFTER `used`;

-- +goose Down
ALTER TABLE `user_sessions` DROP COLUMN `used_at`;
