-- The model the Gateway was running on when it started a fleet.
--
-- A fleet is answered by a turn nobody asked for: the last member reports, and
-- the server starts a Gateway turn to narrate every result. That turn has to run
-- on the SAME model the conversation was using, and until now the only copy of
-- which model that was lived in the goroutine that started the batch.
--
-- A fleet outlives that goroutine on purpose (its members run on workers, and it
-- can be completed after a restart), so the one fact it could not be woken from
-- the database without now lives on the row, like the task does on a delegation.
--
-- Nullable, because a fleet started before this column existed has no answer and
-- must fall back to the profile's model rather than to zero.

-- +goose Up
ALTER TABLE `agent_fleets`
  ADD COLUMN `model_id` bigint(20) unsigned DEFAULT NULL AFTER `parent_tool_call_id`,
  ADD CONSTRAINT `fk_fleet_model` FOREIGN KEY (`model_id`) REFERENCES `ai_models` (`id`) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE `agent_fleets`
  DROP FOREIGN KEY `fk_fleet_model`,
  DROP COLUMN `model_id`;
