-- Who wrote this, on the knowledge base and on the agents.
--
-- A brain, a category and a document can all be created by a person OR by an
-- agent: `brain_write` is a real tool an agent holds (KB/32), and the memory
-- brain is written by the loop itself on a cadence. So the question "who wrote
-- this" has two kinds of answer and the rows could give neither.
--
-- Four columns on each, matching the pair the skills tables carry: the id of
-- the person and the name FROZEN beside it, for the creation and for the last
-- edit. Here BOTH pairs are needed, where a skill needs only the first, because
-- a document is edited: an agent corrects what a person wrote, a person tidies
-- up after an agent, and the row has to be able to say so.
--
-- The id is nullable and goes to NULL when the person is deleted; the name
-- survives them, because a record of who did something that disappears with
-- their account rewrites the history every time somebody leaves. The name is a
-- display string rather than strictly a person's name: an agent has no user row
-- at all, so for its writes the id is nothing and the name is what should be
-- printed.
--
-- The agents table gets the same pair for the same reason, one step removed: an
-- agent is configured by a person today, and an agent that writes its own
-- sub-agents is the shape this product is heading for (KB/27). The columns cost
-- nothing now and mean the answer exists when it is asked for.
--
-- Nothing is backfilled. Every row that exists was written before anybody was
-- recorded, and inventing an author for it would be worse than an empty one: a
-- blank reads as "before this was kept", and a guess reads as fact.

-- +goose Up
ALTER TABLE `brains`
  ADD COLUMN `created_by` bigint(20) unsigned DEFAULT NULL AFTER `is_locked`,
  ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '' AFTER `created_by`,
  ADD COLUMN `updated_by` bigint(20) unsigned DEFAULT NULL AFTER `created_by_name`,
  ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '' AFTER `updated_by`,
  ADD KEY `fk_brain_created_by` (`created_by`),
  ADD KEY `fk_brain_updated_by` (`updated_by`),
  ADD CONSTRAINT `fk_brain_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON DELETE SET NULL,
  ADD CONSTRAINT `fk_brain_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON DELETE SET NULL;

ALTER TABLE `brain_categories`
  ADD COLUMN `created_by` bigint(20) unsigned DEFAULT NULL AFTER `weight`,
  ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '' AFTER `created_by`,
  ADD COLUMN `updated_by` bigint(20) unsigned DEFAULT NULL AFTER `created_by_name`,
  ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '' AFTER `updated_by`,
  ADD KEY `fk_category_created_by` (`created_by`),
  ADD KEY `fk_category_updated_by` (`updated_by`),
  ADD CONSTRAINT `fk_category_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON DELETE SET NULL,
  ADD CONSTRAINT `fk_category_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON DELETE SET NULL;

ALTER TABLE `brain_documents`
  ADD COLUMN `created_by` bigint(20) unsigned DEFAULT NULL AFTER `weight`,
  ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '' AFTER `created_by`,
  ADD COLUMN `updated_by` bigint(20) unsigned DEFAULT NULL AFTER `created_by_name`,
  ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '' AFTER `updated_by`,
  ADD KEY `fk_document_created_by` (`created_by`),
  ADD KEY `fk_document_updated_by` (`updated_by`),
  ADD CONSTRAINT `fk_document_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON DELETE SET NULL,
  ADD CONSTRAINT `fk_document_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON DELETE SET NULL;

ALTER TABLE `agents`
  ADD COLUMN `created_by` bigint(20) unsigned DEFAULT NULL AFTER `delegation_mode`,
  ADD COLUMN `created_by_name` varchar(255) NOT NULL DEFAULT '' AFTER `created_by`,
  ADD COLUMN `updated_by` bigint(20) unsigned DEFAULT NULL AFTER `created_by_name`,
  ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '' AFTER `updated_by`,
  ADD KEY `fk_agent_created_by` (`created_by`),
  ADD KEY `fk_agent_updated_by` (`updated_by`),
  ADD CONSTRAINT `fk_agent_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON DELETE SET NULL,
  ADD CONSTRAINT `fk_agent_updated_by` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE `agents`
  DROP FOREIGN KEY `fk_agent_created_by`,
  DROP FOREIGN KEY `fk_agent_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;

ALTER TABLE `brain_documents`
  DROP FOREIGN KEY `fk_document_created_by`,
  DROP FOREIGN KEY `fk_document_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;

ALTER TABLE `brain_categories`
  DROP FOREIGN KEY `fk_category_created_by`,
  DROP FOREIGN KEY `fk_category_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;

ALTER TABLE `brains`
  DROP FOREIGN KEY `fk_brain_created_by`,
  DROP FOREIGN KEY `fk_brain_updated_by`,
  DROP COLUMN `created_by`,
  DROP COLUMN `created_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `updated_by_name`;
