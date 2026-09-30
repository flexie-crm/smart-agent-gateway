-- The seed a card's token is derived from.
--
-- A confirmation token used to be minted fresh on every DELIVERY of a card, and
-- there are three deliveries: the stream when the call parks, a socket push for
-- a detached agent, and the history read that restores the card on a reload.
-- Each new token replaced the last, so delivering a card twice silently killed
-- the one already on the person's screen. Approving then answered 410 and the
-- click did nothing, which on the approval path is the worst failure the
-- product has: the person is told nothing, and the action they allowed does not
-- happen.
--
-- It was not a mistake anybody made; it followed from the token never being
-- stored. Only its hash was, so a second delivery could not reproduce the first
-- token and had to mint another.
--
-- So the ROW keeps a seed instead, and the token is derived from it:
--
--     token = "sag_cf_" + base64(HMAC-SHA256(server secret, seed))
--
-- Every delivery derives the same token, and nothing can invalidate a card that
-- is on screen. The seed alone is useless: it is the server's secret that turns
-- it into a token, so reading this table still approves nothing, which is the
-- property the hash was there to give. The hash stays as it was and so does the
-- claim, which is what makes an approval single-use.
--
-- Empty on every row written before this: those keep the token they were minted
-- with, and the claim path is unchanged for them.

-- +goose Up
ALTER TABLE `agent_park_snapshots`
  ADD COLUMN `token_seed` varchar(64) NOT NULL DEFAULT '' AFTER `token_hash`;

-- +goose Down
ALTER TABLE `agent_park_snapshots` DROP COLUMN `token_seed`;
