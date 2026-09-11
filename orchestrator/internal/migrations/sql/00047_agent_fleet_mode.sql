-- An agent can be pinned to run in a fleet.
--
-- The mode was added everywhere else in the same pass that built fleets: the
-- Agents screen offers it, the API accepts it, and the runtime resolves it. The
-- column did not learn it, so saving an agent with it failed at the database
-- with a message nobody upstream could explain, which is what an enum does when
-- the code that writes it has moved on.
--
-- Widening an enum is additive: no row holds the new value yet, and nothing
-- reads a value that is not there.

-- +goose Up
ALTER TABLE `agents`
  MODIFY COLUMN `delegation_mode` enum('auto','background','inline','fleet') NOT NULL DEFAULT 'auto';

-- +goose Down
-- Anything pinned to the new mode goes back to letting the Gateway choose,
-- because the column is about to stop being able to say it.
UPDATE `agents` SET `delegation_mode` = 'auto' WHERE `delegation_mode` = 'fleet';
ALTER TABLE `agents`
  MODIFY COLUMN `delegation_mode` enum('auto','background','inline') NOT NULL DEFAULT 'auto';
