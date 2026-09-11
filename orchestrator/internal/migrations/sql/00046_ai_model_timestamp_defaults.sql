-- `ALTER COLUMN ... DROP DEFAULT` does not drop CURRENT_TIMESTAMP.
--
-- Migration 4 added ai_models.created_at and updated_at with a
-- DEFAULT CURRENT_TIMESTAMP(3) to backfill the rows that predated them, and
-- then dropped the default, because the application supplies both values and a
-- column that fills itself in is a column that hides a write nobody made.
--
-- On MariaDB 11.6 the second statement does nothing. A datetime default of
-- CURRENT_TIMESTAMP is an auto-initialise attribute rather than an ordinary
-- default, and DROP DEFAULT leaves it exactly where it was, silently and
-- successfully. MODIFY COLUMN, which restates the column whole, does remove it.
--
-- Nothing broke, which is why it went unnoticed for forty-two migrations: the
-- application always wrote both columns, so the default never fired. What it
-- did was make the migrations produce a schema we do not declare, and the test
-- that asserts those two agree is the thing that caught it.

-- +goose Up
ALTER TABLE `ai_models`
  MODIFY COLUMN `created_at` datetime(3) NOT NULL,
  MODIFY COLUMN `updated_at` datetime(3) NOT NULL;

-- +goose Down
ALTER TABLE `ai_models`
  MODIFY COLUMN `created_at` datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  MODIFY COLUMN `updated_at` datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3);
