-- A skill's own name and description, and an index to find one by.
--
-- The skill row held a handle and nothing else a person reads. The name and the
-- description on screen were the LIVE VERSION's, reached through a join, and
-- migration 61 said so in as many words: "a skill's own row holds nothing a
-- person edits". That is no longer true, and this is the migration that changes
-- it, so the reasoning is recorded here rather than left contradicting itself
-- two files apart.
--
-- Two things forced it, one of them structural:
--
--   1. THERE WAS NO WAY TO SEARCH FOR A SKILL. A full-text index is built over
--      columns of one table, and the description was not a column of any table
--      that a search would start from. `ai_skill_sections` has carried a
--      full-text index since 61 and nothing has ever queried it; the passages
--      are the deep content, and finding a skill by what it is called is a
--      different question from finding a passage by what it says.
--
--   2. A SKILL'S NAME WAS THE PACKAGE'S TO CHOOSE, AND NOBODY ELSE'S. The
--      title comes out of `metadata.title` (or the manifest's first heading),
--      which is the author's, and an administrator who wanted the thing called
--      something else in their own console had nowhere to write it.
--
-- What 61 was avoiding is real and is still avoided. Its worry was a skill
-- named after whatever was imported LAST: roll back to version 2 and the row
-- still described version 5. These columns are not a copy of the live version,
-- they are the SKILL'S OWN pair, and a rollback does not touch them. Every
-- version keeps its own immutable title and description, so the record of what
-- each package said is unchanged and is what the version history shows.
--
-- The import rule that follows from that, in `findOrCreate`: a new version's
-- title and description are taken only when the skill's current pair still
-- equals the version that was live, which is the proof that nobody has edited
-- them. Once somebody has, an import leaves their words alone. No column
-- records "edited": the old version already answers it.
--
-- And the row becomes editable, so it gets the `updated_by` pair migration 63
-- puts on everything a person can change: the id, which goes to NULL when they
-- are deleted, and the name FROZEN beside it, which has to survive them.

-- +goose Up

-- Four columns, then the constraint, then the backfill, then the index: four
-- statements rather than one, and not for readability. Adding the FIRST
-- full-text index to an InnoDB table REBUILDS it (the table gains the hidden
-- FTS_DOC_ID), and a rebuild is where `ADD INDEX fk_x` in the same statement as
-- `ADD CONSTRAINT fk_x` fails with "Duplicate key name" (migration 63 hit
-- exactly that, and only on a table with rows, so it passed on a fresh database
-- and failed the moment a test seeded one). So the index under the foreign key
-- is left to the constraint, which names it after itself and lands on the shape
-- the declared schema asks for.
ALTER TABLE `ai_skills`
  ADD COLUMN `title` varchar(255) NOT NULL DEFAULT '' AFTER `name`,
  ADD COLUMN `description` varchar(1024) NOT NULL DEFAULT '' AFTER `title`,
  ADD COLUMN `updated_by` bigint(20) unsigned DEFAULT NULL AFTER `created_by_name`,
  ADD COLUMN `updated_by_name` varchar(255) NOT NULL DEFAULT '' AFTER `updated_by`;

ALTER TABLE `ai_skills`
  ADD CONSTRAINT `fk_skill_updated_by` FOREIGN KEY (`updated_by`)
    REFERENCES `users` (`id`) ON UPDATE RESTRICT ON DELETE SET NULL;

-- Seeded from the version that is live, which is exactly what the join used to
-- return, so nothing on any screen changes when this runs. A skill with no live
-- version keeps two empty strings, which is what the join gave it too.
UPDATE `ai_skills` s
  JOIN `ai_skill_versions` v ON v.id = s.active_version_id
   SET s.title = v.title,
       s.description = v.description;

-- Last, so it is built over the rows the backfill just wrote rather than over
-- empty columns. `name` is in it as well as the two new ones: the handle is
-- what an author addresses the skill by and is the one word somebody who has
-- read a SKILL.md will type.
ALTER TABLE `ai_skills`
  ADD FULLTEXT KEY `ft_skill` (`name`,`title`,`description`);

-- +goose Down

-- The columns go and the join comes back. Nothing is lost that was not either
-- a copy of a version's title (still on the version) or something somebody
-- typed here, and a down step cannot keep the second without the column to keep
-- it in.
ALTER TABLE `ai_skills` DROP FOREIGN KEY `fk_skill_updated_by`;

ALTER TABLE `ai_skills`
  DROP KEY `ft_skill`,
  DROP COLUMN `updated_by_name`,
  DROP COLUMN `updated_by`,
  DROP COLUMN `description`,
  DROP COLUMN `title`;
