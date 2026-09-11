-- +goose Up

-- An agent carries the reasoning flag, because reasoning is a property of the
-- agent rather than of the request: it is a configuration decision about how
-- this assistant thinks, not something the caller toggles per message.
ALTER TABLE agents
  ADD COLUMN reasoning TINYINT(1) NOT NULL DEFAULT 0 AFTER model_id;

-- An agent may leave the model unset. The layered configuration model means
-- each layer overrides only the fields it has an opinion about: an agent that
-- exists to supply a system prompt and a tool set should not be forced to pin
-- a model it does not care about.
ALTER TABLE agents
  MODIFY COLUMN model_id BIGINT UNSIGNED NULL;

-- +goose Down
ALTER TABLE agents MODIFY COLUMN model_id BIGINT UNSIGNED NOT NULL;
ALTER TABLE agents DROP COLUMN reasoning;
