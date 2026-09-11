-- A fleet: N agents asked to do N things, and one answer when they are all back.
--
-- The members are ordinary `agent_delegations` rows with a `fleet_id`, not a new
-- kind of thing. That is the whole point of the shape: a fleet member is still a
-- delegation, so it keeps the chip in the chat, the history on reload, the
-- cancel path and the boot recovery, none of which had to learn a new concept.
--
-- What the fleet row itself holds is only what a delegation cannot: how many
-- were asked for, and whether somebody has already told the Gateway about it.
--
-- There is deliberately NO counter of how many have come back. The count is a
-- COUNT: the terminal members are in the delegations table, so asking is exact,
-- cannot drift, and survives a restart with no bookkeeping to rebuild. A
-- decrementing column would be a second copy of a fact the database already has.
--
-- `status` is what stops two reports arriving together from both deciding they
-- were last: closing the fleet is a conditional update and exactly one wins.

-- +goose Up
CREATE TABLE `agent_fleets` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `session_id` bigint(20) unsigned NOT NULL,
  `workspace_id` bigint(20) unsigned NOT NULL,
  `parent_tool_call_id` varchar(64) NOT NULL,
  `size` smallint(5) unsigned NOT NULL,
  `status` enum('running','done','cancelled') NOT NULL DEFAULT 'running',
  `deadline` datetime(3) NOT NULL,
  `created_at` datetime(3) NOT NULL,
  `completed_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_session` (`session_id`),
  KEY `idx_status_deadline` (`status`,`deadline`),
  CONSTRAINT `fk_fleet_session` FOREIGN KEY (`session_id`) REFERENCES `agent_sessions` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_fleet_workspace` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE `agent_delegations`
  ADD COLUMN `fleet_id` bigint(20) unsigned DEFAULT NULL AFTER `mode`,
  ADD KEY `idx_fleet` (`fleet_id`,`status`),
  ADD CONSTRAINT `fk_delegation_fleet` FOREIGN KEY (`fleet_id`) REFERENCES `agent_fleets` (`id`) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE `agent_delegations`
  DROP FOREIGN KEY `fk_delegation_fleet`,
  DROP KEY `idx_fleet`,
  DROP COLUMN `fleet_id`;
DROP TABLE `agent_fleets`;
