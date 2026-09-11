-- Deleting a conversation becomes a permission, without taking it from anybody.
--
-- Until now the chat routes required a valid token and nothing else: any person
-- who could sign in could delete their own conversations, and no role could say
-- otherwise. `chats:delete` is that decision, and enforcing it has an edge that
-- is easy to miss on the way in: a permission nobody holds is a capability
-- everybody just lost. Deploy it bare and every person on the system finds the
-- delete button gone, for a change that was meant to be about who may, not
-- about stopping everyone.
--
-- So every role that exists on the day of the upgrade is granted it. That is
-- the honest translation of what was true a moment before: everyone could.
-- Taking it away from somebody is then a deliberate act, made in the roles
-- screen by a person who meant it, which is the whole point of the permission.
--
-- New roles created afterwards start without it, like every other permission:
-- the backfill is about not changing what is, not about a new default.
--
-- Note what this does NOT reach. A person in no group, or in groups holding no
-- role, has no effective permissions at all and therefore cannot delete after
-- this runs. That is correct rather than an oversight: they hold no permission
-- of any kind, and the alternative is a grant that comes from nowhere and can
-- be revoked nowhere.

-- A role holding '*' is left alone. It already passes every check, so the row
-- would grant it nothing, and the roles screen would draw it as "Everything
-- (superuser)" with one further box ticked, which reads as though everything
-- were not quite everything.

-- +goose Up
INSERT IGNORE INTO `role_permissions` (`role_id`, `permission`)
  SELECT r.`id`, 'chats:delete' FROM `roles` r
  WHERE NOT EXISTS (
    SELECT 1 FROM `role_permissions` rp WHERE rp.`role_id` = r.`id` AND rp.`permission` = '*'
  );

-- +goose Down
-- Only the grant goes. A role that was edited afterwards to drop it is not
-- resurrected, and one that never had it is not disturbed: this deletes exactly
-- the permission this migration is about.
DELETE FROM `role_permissions` WHERE `permission` = 'chats:delete';
