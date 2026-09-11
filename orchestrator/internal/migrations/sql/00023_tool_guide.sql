-- A custom tool's own AI guide, editable per instance.
--
-- A built-in tool's deep guide lives in code. A native custom tool starts from
-- its template's guide, but an administrator may refine it for their instance
-- (the tables this database has, the way this API is used), so the guide is a
-- column on the tool, prefilled from the template and then the tool's own.
-- Empty means "use the template's".

-- +goose Up
ALTER TABLE tools
  ADD COLUMN guide MEDIUMTEXT DEFAULT NULL AFTER config;

-- +goose Down
ALTER TABLE tools
  DROP COLUMN guide;
