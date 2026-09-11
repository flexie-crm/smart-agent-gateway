-- A model carries notes, not capability flags.
--
-- supports_tools / supports_streaming / supports_reasoning / privacy_level were
-- admin toggles that claimed to know what a model can do. There is no reliable
-- machine source for that (it varies per model even within a vendor, and it
-- turns over as models are released and withdrawn), so guessing it on the model
-- only misled the person setting one up.
--
-- Instead: tools and reasoning are chosen on the AGENT, and a model that cannot
-- honour them returns a vendor error the loop surfaces to the user as a normal
-- failure. Whether the data leaves the building is derived from the VENDOR (a
-- self-hosted/local endpoint), never stored here. What remains is a free-text
-- description for whoever curates the registry.

-- +goose Up
ALTER TABLE ai_models
  ADD COLUMN description TEXT DEFAULT NULL AFTER context_window,
  DROP COLUMN supports_tools,
  DROP COLUMN supports_streaming,
  DROP COLUMN supports_reasoning,
  DROP COLUMN privacy_level;

-- +goose Down
ALTER TABLE ai_models
  ADD COLUMN supports_tools TINYINT(1) NOT NULL DEFAULT 0 AFTER context_window,
  ADD COLUMN supports_streaming TINYINT(1) NOT NULL DEFAULT 1 AFTER supports_tools,
  ADD COLUMN supports_reasoning TINYINT(1) NOT NULL DEFAULT 0 AFTER supports_streaming,
  ADD COLUMN privacy_level ENUM('external_vendor','local') NOT NULL DEFAULT 'external_vendor' AFTER supports_reasoning,
  DROP COLUMN description;
