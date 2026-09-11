-- How many agents the Gateway may start in one batch.
--
-- It was a constant in the code, chosen by whoever wrote it, and the number is
-- not a fact about the software: twenty model conversations at once is a bill
-- and a rate limit, and what that is worth differs per deployment and per
-- vendor. So it belongs beside the other bounds an administrator already sets
-- on the Gateway (its approval window, its tool-loop cap), and for the same
-- reason they do.
--
-- Nullable: empty means the code default, exactly as the columns beside it do.
-- Only the Gateway reads it, because only the Gateway starts a batch.

-- +goose Up
ALTER TABLE `agents`
  ADD COLUMN `max_fleet_agents` smallint(5) unsigned DEFAULT NULL AFTER `max_iterations`;

-- +goose Down
ALTER TABLE `agents` DROP COLUMN `max_fleet_agents`;
