-- Who may see the thinking and the tool calls behind an answer.
--
-- An assistant's answer is one thing; how it got there is another. A tool call
-- carries what was sent and what came back (a query and its rows, a command and
-- what it printed), which is the most revealing thing in a conversation and the
-- most useful. An organisation may reasonably decide who sees it, so it becomes
-- a permission rather than something everybody has by default.
--
-- And every role that exists on the day of the upgrade is granted both, for the
-- reason chats:delete was (migration 42): a permission nobody holds is a
-- capability everybody just lost. Deploy this bare and every person on the
-- system finds the reasoning and the tool calls gone from their conversations,
-- for a change that was meant to be about who may see them, not about stopping
-- everyone.
--
-- Roles created afterwards start without them, like every other permission: the
-- backfill is about not changing what is, not about a new default.
--
-- A role holding '*' is left alone. It passes every check already, and the
-- roles screen would draw it as "Everything (superuser)" with two further boxes
-- ticked, which reads as though everything were not quite everything.

-- +goose Up
INSERT IGNORE INTO `role_permissions` (`role_id`, `permission`)
  SELECT r.`id`, p.`permission` FROM `roles` r
  CROSS JOIN (SELECT 'chats:see_reasoning' AS `permission`
              UNION ALL SELECT 'chats:see_tools') p
  WHERE NOT EXISTS (
    SELECT 1 FROM `role_permissions` rp WHERE rp.`role_id` = r.`id` AND rp.`permission` = '*'
  );

-- +goose Down
DELETE FROM `role_permissions` WHERE `permission` IN ('chats:see_reasoning', 'chats:see_tools');
