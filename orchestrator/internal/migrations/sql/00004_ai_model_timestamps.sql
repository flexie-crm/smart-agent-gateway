-- ai_models shipped without audit timestamps while every other table has
-- them. Backfill existing rows from the current time so the columns can be
-- NOT NULL like everywhere else.

-- +goose Up
ALTER TABLE ai_models
  ADD COLUMN created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  ADD COLUMN updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3);

-- The application always supplies both values; the defaults exist only to
-- backfill rows that predate this migration.
ALTER TABLE ai_models
  ALTER COLUMN created_at DROP DEFAULT,
  ALTER COLUMN updated_at DROP DEFAULT;

-- +goose Down
ALTER TABLE ai_models DROP COLUMN created_at, DROP COLUMN updated_at;
