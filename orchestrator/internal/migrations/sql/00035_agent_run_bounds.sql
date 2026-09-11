-- Two safety bounds an administrator can set per agent, both optional (NULL uses
-- the code default). max_iterations bounds an agent's tool loop, the number of
-- model round-trips it may take before it must stop (default 100, main agent and
-- sub-agents alike). background_timeout_seconds bounds one working leg of a
-- background specialist (Mode C, KB/27): if its goroutine runs past this, the AI
-- stream is cancelled, the goroutine is torn down cleanly, and the master is told
-- it timed out with no result (default 600s, ten minutes; meaningful only for a
-- sub-agent, which is the only thing that runs in the background).

-- +goose Up
ALTER TABLE `agents`
  ADD COLUMN `max_iterations` int(10) unsigned DEFAULT NULL AFTER `approval_ttl_seconds`,
  ADD COLUMN `background_timeout_seconds` int(10) unsigned DEFAULT NULL AFTER `max_iterations`;

-- +goose Down
ALTER TABLE `agents`
  DROP COLUMN `background_timeout_seconds`,
  DROP COLUMN `max_iterations`;
