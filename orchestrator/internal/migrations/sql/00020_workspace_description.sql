-- A workspace says what it is for, in a person's words.
--
-- The slug is a handle the system derives and keys the tenant on; nobody
-- creating a workspace should have to know the word "slug" exists. The
-- description is the field they actually fill in: what this partition is about.
-- It is optional and free text, empty by default so existing rows need no
-- backfill.

-- +goose Up
ALTER TABLE workspaces ADD COLUMN description VARCHAR(1024) NOT NULL DEFAULT '' AFTER name;

-- +goose Down
ALTER TABLE workspaces DROP COLUMN description;
