-- Custom tools: tools defined as data, not code.
--
-- A built-in tool is a Schema+Handler pair in the code. A CUSTOM tool is a row
-- in the tools table (kind='custom'), self-describing like any other tool
-- (friendly_name, description, input_schema all already live here), but backed
-- by data rather than a class:
--
--   - a NATIVE custom tool is instantiated from a code TEMPLATE (e.g. the query
--     template) with a chosen driver and settings; `template` names the
--     template, and `config` (the existing JSON column) holds all of its
--     configuration, including the connection auth;
--   - a WORKFLOW custom tool (later) has `template` NULL and is answered by a
--     workflow.
--
-- Because a custom tool is a tools row, it gets grants, status, and approval
-- from the same machinery every tool uses, and each instance is granted on its
-- own (query_analytics but not query_hr), the same way an MCP-projected tool is.
-- The one new column is `template`: which recipe an instance came from. Its
-- settings, auth included, live in the config column as JSON.

-- +goose Up
ALTER TABLE tools
  ADD COLUMN template VARCHAR(64) DEFAULT NULL AFTER kind;

-- +goose Down
ALTER TABLE tools
  DROP COLUMN template;
