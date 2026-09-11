-- Which delegation a card belongs to.
--
-- Until now a card was matched back to its agent by the tool call the Gateway
-- made when it started it, and that was exact because every background
-- delegation has a `delegate` call of its own.
--
-- A fleet breaks it. Its members are started by ONE `delegate_fleet` call, so
-- twenty of them share one parent, and "which agent raised this card" stops
-- having an answer: with the same agent twice in a batch, answering a card could
-- re-enter the wrong member, running an approved action against a task nobody
-- approved it for.
--
-- So the card names its delegation. Nullable, because the Gateway's own cards
-- belong to no delegation and never did.

-- +goose Up
ALTER TABLE `agent_park_snapshots`
  ADD COLUMN `delegation_id` bigint(20) unsigned DEFAULT NULL AFTER `parent_tool_call_id`,
  ADD CONSTRAINT `fk_park_delegation` FOREIGN KEY (`delegation_id`) REFERENCES `agent_delegations` (`id`) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE `agent_park_snapshots`
  DROP FOREIGN KEY `fk_park_delegation`,
  DROP COLUMN `delegation_id`;
