-- Which computer the parked call was going to run on.
--
-- A park is the prepared call, frozen: what will run, unchanged, the moment
-- somebody says yes. For a tool that runs on the person's own machine, WHERE it
-- runs is part of that, and it was the one part not written down. Two things
-- went wrong for want of it, both measured:
--
--   The card could not be drawn again. A reload rebuilds it from the park by
--   resolving the tool's schema live, and it resolved with no computer, so a
--   tool that runs on somebody's machine was not in the loadout and there was
--   no schema to draw from. A terminal waiting for approval simply vanished on
--   a refresh, and the park sat unresolved until it expired. A tool needing no
--   computer (an API request) came back fine, which is how the two were told
--   apart.
--
--   The approved call ran on whoever's computer answered. The resume took the
--   device from the REQUEST, so approving from a second machine ran the command
--   there instead of on the one the call was prepared for. Approval must equal
--   success, and where it runs was part of what was approved.
--
-- Empty for a call with no computer behind it, which is most of them.

-- +goose Up
ALTER TABLE `agent_park_snapshots`
  ADD COLUMN `device_id` varchar(64) NOT NULL DEFAULT '' AFTER `model_id`;

-- +goose Down
ALTER TABLE `agent_park_snapshots` DROP COLUMN `device_id`;
