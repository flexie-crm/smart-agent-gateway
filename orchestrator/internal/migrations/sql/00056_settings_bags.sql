-- One place to keep a setting nobody had thought of yet.
--
-- Vendors keep inventing attributes that are neither standard nor optional.
-- OpenAI's newest models take a reasoning EFFORT; Anthropic has its own version
-- of the same idea; there will be more, and they will not agree with each other.
-- Adding a column for each is a migration per vendor whim, and a schema that
-- records which company shipped what in which quarter.
--
-- So: one JSON column per configurable thing, holding an object of chosen
-- values. `tools` has done exactly this since it was built (its `config`), and
-- the inference node's settings form works the same way, so this is an existing
-- pattern applied consistently rather than a new idea.
--
-- THE RULE THAT KEEPS IT FROM BECOMING A SWAMP: the CODE declares which keys are
-- legal, and this column only stores the values chosen for them. A dialect says
-- "this vendor's models take reasoning_effort, one of low, medium, high, max,
-- and medium if nobody says"; the row holds {"reasoning_effort":"high"}. A key
-- nothing declares is IGNORED rather than obeyed, so a value left behind by a
-- removed feature cannot quietly steer anything, and there is always a form to
-- render and always an answer to "what does this mean".
--
-- Four levels, because a setting belongs wherever the decision is made, and they
-- resolve most specific first: agent, then model, then vendor, then the default
-- the code declares.

-- +goose Up
ALTER TABLE `ai_models`
  ADD COLUMN `settings` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL
  CHECK (json_valid(`settings`));

ALTER TABLE `ai_vendors`
  ADD COLUMN `settings` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL
  CHECK (json_valid(`settings`));

ALTER TABLE `agents`
  ADD COLUMN `settings` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL
  CHECK (json_valid(`settings`));

ALTER TABLE `inference_nodes`
  ADD COLUMN `settings` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL
  CHECK (json_valid(`settings`));

-- +goose Down
ALTER TABLE `inference_nodes` DROP COLUMN `settings`;
ALTER TABLE `agents` DROP COLUMN `settings`;
ALTER TABLE `ai_vendors` DROP COLUMN `settings`;
ALTER TABLE `ai_models` DROP COLUMN `settings`;
