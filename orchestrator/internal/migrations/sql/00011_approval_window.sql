-- How long a person has to answer a confirmation is a policy, not a constant.
--
-- Twenty-four hours suits an assistant that files a ticket. It is far too long
-- for one that moves money, where an approval redeemed tomorrow morning acts on
-- a world that has moved on; and far too short for a process where the approver
-- is a manager who is away until Monday. So it becomes another layer of the
-- configuration model (KB/15): a code default, a deployment default, and then
-- the agent or the workflow overriding it for the requests it governs.
--
-- Seconds, because a duration string in a column is a parser waiting to fail on
-- a row nobody validated.

-- +goose Up
ALTER TABLE agents
  ADD COLUMN approval_ttl_seconds INT UNSIGNED NULL AFTER reasoning;

-- +goose Down
ALTER TABLE agents DROP COLUMN approval_ttl_seconds;
