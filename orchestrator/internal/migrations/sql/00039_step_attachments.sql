-- The files a person sent with a message, on the message.
--
-- Their ids, not their contents. The account of what each file holds already
-- lives on the attachment itself, written the one time it was read, so a
-- conversation that goes on for twenty turns still calls the reading model
-- once: every turn after the first composes the same account out of the same
-- stored row.
--
-- Ids rather than the composed text for two reasons. The person's own message
-- stays their own words, so the chat shows what they typed rather than what we
-- said to the model about it. And a reloaded conversation can still show the
-- file chips, because it knows which files were sent.

-- +goose Up
ALTER TABLE `agent_steps`
  ADD COLUMN `attachments` json DEFAULT NULL AFTER `is_partial`;

-- +goose Down
ALTER TABLE `agent_steps` DROP COLUMN `attachments`;
