-- What a background agent needs to be STARTED AGAIN after a hard kill.
--
-- A graceful shutdown lets an agent finish (SAG_SHUTDOWN_GRACE), and covers a
-- deploy, a restart, a rebuild: the ordinary 99%. It cannot cover a SIGKILL, an
-- out-of-memory, a panic, or the machine going away. Until now the answer to
-- those was to settle the delegation as failed and have the Gateway tell the
-- person it did not finish, which is honest and useless: they have to ask again.
--
-- The park already proves the alternative works. A confirmation survives a
-- restart because everything needed to run the call again is written down, and
-- nothing in memory backs it. A background agent had all of that except the one
-- thing that matters, the INSTRUCTION it was given, which lived only in the
-- goroutine's arguments. So it is stored, and a killed agent can be started
-- again instead of mourned.
--
-- `attempts` is what stops that being a loop. A task that kills the process
-- would otherwise be restarted by every boot for ever, taking the process with
-- it each time. Past the cap it settles as failed and is narrated the old way.

-- +goose Up
ALTER TABLE `agent_delegations`
  ADD COLUMN `task` text DEFAULT NULL AFTER `mode`,
  ADD COLUMN `attempts` tinyint unsigned NOT NULL DEFAULT 1 AFTER `status`;

-- +goose Down
ALTER TABLE `agent_delegations`
  DROP COLUMN `task`,
  DROP COLUMN `attempts`;
