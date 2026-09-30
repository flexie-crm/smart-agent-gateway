-- Skills: a procedure somebody wrote down, imported as a package and kept here.
--
-- The interchange format is the Agent Skills directory (a `SKILL.md` with YAML
-- frontmatter, plus `scripts/`, `references/` and `assets/`), so a skill written
-- for another compatible implementation imports unchanged. After the import the
-- database is the canonical store: the whole package is in here, byte for byte,
-- and any version can be written back out as the same directory.
--
-- Four tables, and the reason each one exists:
--
--   ai_skills           the identity. One row per skill per workspace, and the
--                       pointer at whichever version is live.
--   ai_skill_versions   a version is IMMUTABLE. An updated upload never edits
--                       the active version, it adds one beside it, which is what
--                       makes "which version answered that question" a fact
--                       rather than a guess.
--   ai_skill_files      the package. Text in `text_content`, raw bytes in
--                       `binary_content`, the other column NULL, and `path`
--                       keeping the original directory structure so the export
--                       needs nothing else.
--   ai_skill_sections   a DERIVED search index over the text files, split on
--                       headings. It can always be rebuilt from the files, and
--                       an export must never read it.
--
-- WHO made it, and who last touched it.
--
-- Two columns per row: the id of the person, and the name FROZEN beside it. The
-- id is nullable and goes to NULL when the person is deleted; the name survives
-- them, because a record of who did something that disappears with their
-- account rewrites the history every time somebody leaves. The name is a
-- display string rather than strictly a person's name, because a version the
-- system writes itself was made by an agent or a model and that is what should
-- be printed. Which of the two it was needs no column: `source` already says
-- whether a version was imported, edited by hand, or learned.
--
-- CREATED only, on both tables, and no updated pair on either. A version cannot
-- be changed at all, so a column recording who changed it would always equal
-- the other one and one day would not. And a skill's own row holds nothing a
-- person edits: its title and description belong to the version, its status is
-- a switch, and which version is live is a pointer. What CHANGED is always a
-- version, and the version says who made it.
--
-- The FILES and the SECTIONS carry neither. They belong to a version, which
-- says who imported it, and they are written once with it; repeating that on
-- every one of a thousand files would repeat the version's answer a thousand
-- times.
--
-- The children carry `skill_id` as well as `version_id`, which is the shape
-- `brain_documents` already uses (`brain_id` beside `category_id`): it takes a
-- join out of every workspace-scoped read, and out of the search that stage 2
-- is built on. It cannot drift, because a version is immutable and its files are
-- written once, in the transaction that made it.
--
-- The title and the description live on the VERSION, not on the skill, because
-- they are properties of the PACKAGE: they arrive in its frontmatter and change
-- when it does. On the skill they were written by whatever was imported LAST,
-- which is wrong the moment a rollback makes an earlier version live again: the
-- skill would be running version 2 and describing version 3. Found by doing
-- exactly that. So there is one copy, on the version that owns it, and the
-- skill's answer reads it through the join it already makes to its live version;
-- there is nothing to keep in step and nothing that can disagree.
--
-- `name` is the HANDLE, and `title` is the name for a person. The format has
-- only the first: `name` is max 64 characters of lowercase alphanumerics and
-- hyphens and MUST equal the package's directory name, which makes a list of
-- skills read like a directory listing. The specification's own answer to a
-- property it does not define is the `metadata` map ("clients can use this to
-- store additional properties not defined by the Agent Skills spec"), so an
-- author writes `metadata.title`; failing that the import takes the manifest's
-- own first heading, and failing both the column is empty and callers print the
-- handle (model.Skill.Label). Nothing is derived from the handle itself: a title
-- made by title-casing `pdf-processing` reads "Pdf processing", which is a name
-- the author never wrote.
--
-- Two more things about ai_skills worth stating, because they look like
-- duplication and are not:
--
-- `active_version_id` and `ai_skill_versions.status` are two records of one
-- fact, and two records of one fact can disagree. Exactly ONE store method moves
-- either of them, and it writes both inside one transaction (sqlstore.activate).
-- Nothing else may set them. The alternative, deriving "active" from the status
-- column alone, costs a scan on every search of every skill and still needs a
-- rule saying only one version may hold it.
--
-- `package_sha256` is unique PER SKILL. Uploading the identical package twice is
-- the same version, not a second one, so a re-import is idempotent: the store
-- finds the existing row and returns it rather than growing a version history
-- out of somebody pressing the button again.
--
-- The foreign keys between ai_skills and ai_skill_versions point BOTH ways, so
-- the tables are created without that second one and it goes on at the end. On
-- the declared side (schema/) it sits in the CREATE, because the schema differ
-- builds those files with foreign key checks lifted (internal/schemasync).

-- +goose Up
CREATE TABLE `ai_skills` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `workspace_id` bigint(20) unsigned NOT NULL,
  `name` varchar(64) NOT NULL,
  `created_by` bigint(20) unsigned DEFAULT NULL,
  `created_by_name` varchar(255) NOT NULL DEFAULT '',
  `active_version_id` bigint(20) unsigned DEFAULT NULL,
  `status` enum('draft','active','disabled','archived') NOT NULL DEFAULT 'draft',
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_ws_skill` (`workspace_id`,`name`),
  KEY `fk_skill_active_version` (`active_version_id`),
  KEY `fk_skill_created_by` (`created_by`),
  CONSTRAINT `fk_skill_workspace` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_skill_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE `ai_skill_versions` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `skill_id` bigint(20) unsigned NOT NULL,
  `parent_version_id` bigint(20) unsigned DEFAULT NULL,
  `version_number` int(10) unsigned NOT NULL,
  `source` enum('imported','manual','learned') NOT NULL,
  `status` enum('draft','active','rejected','archived') NOT NULL DEFAULT 'draft',
  `created_by` bigint(20) unsigned DEFAULT NULL,
  `created_by_name` varchar(255) NOT NULL DEFAULT '',
  `title` varchar(255) NOT NULL DEFAULT '',
  `description` varchar(1024) NOT NULL DEFAULT '',
  `change_summary` text DEFAULT NULL,
  `package_sha256` char(64) NOT NULL,
  `validated_at` datetime(3) DEFAULT NULL,
  `activated_at` datetime(3) DEFAULT NULL,
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_skill_version` (`skill_id`,`version_number`),
  UNIQUE KEY `uniq_skill_package` (`skill_id`,`package_sha256`),
  KEY `fk_version_parent` (`parent_version_id`),
  KEY `fk_version_created_by` (`created_by`),
  CONSTRAINT `fk_version_skill` FOREIGN KEY (`skill_id`) REFERENCES `ai_skills` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_version_created_by` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON DELETE SET NULL,
  CONSTRAINT `fk_version_parent` FOREIGN KEY (`parent_version_id`) REFERENCES `ai_skill_versions` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE `ai_skills`
  ADD CONSTRAINT `fk_skill_active_version` FOREIGN KEY (`active_version_id`) REFERENCES `ai_skill_versions` (`id`) ON DELETE SET NULL;

CREATE TABLE `ai_skill_files` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `skill_id` bigint(20) unsigned NOT NULL,
  `version_id` bigint(20) unsigned NOT NULL,
  `path` varchar(512) NOT NULL,
  `file_type` enum('skill','reference','script','asset','other') NOT NULL,
  `mime_type` varchar(255) DEFAULT NULL,
  `text_content` longtext DEFAULT NULL,
  `binary_content` longblob DEFAULT NULL,
  `size_bytes` bigint(20) unsigned NOT NULL,
  `sha256` char(64) NOT NULL,
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_version_path` (`version_id`,`path`),
  KEY `fk_skill_file_skill` (`skill_id`),
  KEY `idx_skill_file_sha` (`sha256`),
  CONSTRAINT `fk_skill_file_skill` FOREIGN KEY (`skill_id`) REFERENCES `ai_skills` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_skill_file_version` FOREIGN KEY (`version_id`) REFERENCES `ai_skill_versions` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE `ai_skill_sections` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `skill_id` bigint(20) unsigned NOT NULL,
  `version_id` bigint(20) unsigned NOT NULL,
  `file_id` bigint(20) unsigned NOT NULL,
  `heading` varchar(500) DEFAULT NULL,
  `section_path` varchar(1500) DEFAULT NULL,
  `body` longtext NOT NULL,
  `line_start` int(10) unsigned DEFAULT NULL,
  `line_end` int(10) unsigned DEFAULT NULL,
  `sequence_no` int(10) unsigned NOT NULL,
  `sha256` char(64) NOT NULL,
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_version_sequence` (`version_id`,`sequence_no`),
  KEY `fk_section_skill` (`skill_id`),
  KEY `fk_section_file` (`file_id`),
  FULLTEXT KEY `ft_skill_section` (`heading`,`section_path`,`body`),
  CONSTRAINT `fk_section_skill` FOREIGN KEY (`skill_id`) REFERENCES `ai_skills` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_section_version` FOREIGN KEY (`version_id`) REFERENCES `ai_skill_versions` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_section_file` FOREIGN KEY (`file_id`) REFERENCES `ai_skill_files` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Managing skills is a new area, and it goes to whoever could already curate
-- what the agent knows: a skill and a brain are the same kind of decision, one
-- written as a procedure and one as a document. Same reasoning as migration 50
-- for machines, which went to whoever could already configure where models come
-- from. A role holding '*' passes every check already and is left alone.
-- +goose StatementBegin
INSERT INTO `role_permissions` (`role_id`, `permission`)
SELECT DISTINCT rp.`role_id`, s.`permission`
FROM `role_permissions` rp
CROSS JOIN (
  SELECT 'skills:view' AS `permission`
  UNION ALL SELECT 'skills:create'
  UNION ALL SELECT 'skills:edit'
  UNION ALL SELECT 'skills:delete'
) s
WHERE rp.`permission` = 'brains:create'
  AND NOT EXISTS (
    SELECT 1 FROM `role_permissions` existing
    WHERE existing.`role_id` = rp.`role_id` AND existing.`permission` = s.`permission`
  );
-- +goose StatementEnd

-- +goose Down
DELETE FROM `role_permissions`
WHERE `permission` IN ('skills:view', 'skills:create', 'skills:edit', 'skills:delete');

DROP TABLE `ai_skill_sections`;
DROP TABLE `ai_skill_files`;
ALTER TABLE `ai_skills` DROP FOREIGN KEY `fk_skill_active_version`;
DROP TABLE `ai_skill_versions`;
DROP TABLE `ai_skills`;
