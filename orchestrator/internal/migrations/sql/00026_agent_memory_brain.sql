-- An agent's own long-term memory brain.
--
-- An agent may READ a set of brains (agent_brains, already here) and may keep
-- ONE of them as its long-term memory: a brain it writes back to off the stream
-- as it learns. That brain must be unlocked (writable), which the API enforces;
-- the column only holds the choice.
--
-- It is optional, for every agent including the main one: an agent with no
-- memory brain simply keeps no long-term memory. ON DELETE SET NULL, because
-- deleting a brain must forget the agents that pointed at it, not delete them.

-- +goose Up
ALTER TABLE agents
  ADD COLUMN memory_brain_id BIGINT UNSIGNED DEFAULT NULL AFTER model_id,
  ADD KEY fk_agent_memory_brain (memory_brain_id),
  ADD CONSTRAINT fk_agent_memory_brain FOREIGN KEY (memory_brain_id) REFERENCES brains (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE agents
  DROP FOREIGN KEY fk_agent_memory_brain,
  DROP COLUMN memory_brain_id;
