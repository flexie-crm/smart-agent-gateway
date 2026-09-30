-- Which computer a delegated agent may act on.
--
-- An agent works on the person's behalf, so what they can reach it can reach,
-- and for a synchronous delegation that already held: the device travels on the
-- turn. A background or fleet agent had nothing. It was resolved with no
-- computer at all, which does not merely break its calls, it takes the tools
-- away: machine.Offers answers nil for an empty device and the loadout then
-- drops every tool that runs on somebody's machine, so the agent was never told
-- they existed.
--
-- It has to be a COLUMN rather than a value passed along, because a background
-- delegation outlives the process. Recovery restarts it from this row after a
-- crash, with no request to ask, and the row's own note said the task was "the
-- one thing a killed agent cannot be started again without: everything else it
-- needs is already on this row". The device is the second thing, and this is
-- what makes that sentence true again.
--
-- Empty for every row that exists, which is exactly what those runs had.

-- +goose Up
ALTER TABLE `agent_delegations`
  ADD COLUMN `device_id` varchar(64) NOT NULL DEFAULT '' AFTER `mode`;

-- +goose Down
ALTER TABLE `agent_delegations` DROP COLUMN `device_id`;
