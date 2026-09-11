-- Public identifiers are opaque.
--
-- A conversation's row id is a fine primary key and a terrible public
-- identifier: it tells anyone holding one how many conversations exist, and it
-- invites walking the range to find someone else's. Authorization stops them
-- reading it, but they should not be able to ask the question in the first
-- place.
--
-- So the API speaks in `uid`, and the numeric id stays inside: joins remain
-- fast, and nothing about the shape of the data leaks out.

-- +goose Up
ALTER TABLE agent_sessions ADD COLUMN uid VARCHAR(32) NULL;

-- Existing rows get one, so nothing is left unaddressable. A hash of the id is
-- not random, but these are development rows and the column is about to be
-- unique-indexed either way.
UPDATE agent_sessions
SET uid = CONCAT('ch_', LEFT(SHA2(CONCAT('sag', id, started_at), 256), 22))
WHERE uid IS NULL;

ALTER TABLE agent_sessions
  MODIFY COLUMN uid VARCHAR(32) NOT NULL,
  ADD UNIQUE KEY uniq_uid (uid);

-- +goose Down
ALTER TABLE agent_sessions DROP INDEX uniq_uid, DROP COLUMN uid;
