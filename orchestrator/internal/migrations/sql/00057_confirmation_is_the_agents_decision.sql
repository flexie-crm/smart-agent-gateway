-- A projected tool stops carrying an approval bit of its own.
--
-- Whether a call pauses for a person is decided on the AGENT (its confirm set),
-- and for a dangerous built-in by the code. A tool coming from an MCP
-- connection carried a third answer: the sync switched approval ON for every
-- tool it had not seen before, and back ON whenever a remote changed a
-- definition, and a checkbox on the tool was the only way to clear it.
--
-- It was described as a guard against a service redefining a tool under us, and
-- it cannot be one: nothing syncs on its own. A sync happens when somebody
-- presses refresh or finishes an OAuth connection, so the "guard" fires
-- whenever an administrator happens to look, which is not a guarantee anybody
-- can rely on. What it reliably was instead is an onboarding step: connect a
-- service, then tick a box on each of its tools before any of them can answer
-- without interrupting somebody, in words ("ask a person first") that a person
-- reads as the agent setting of the same name.
--
-- So it goes, and the switch in the console goes with it. The tools table keeps
-- the column, because a built-in still mirrors the code's own floor into it.
--
-- This clears a decision somebody may have made deliberately: an MCP tool held
-- for approval stops being held. The replacement is the agent's confirm set,
-- which is where every other tool's pause already lives, and it is not clearing
-- a grant: who may reach the tool is untouched.

-- +goose Up
UPDATE `tools` SET `requires_approval` = 0 WHERE `kind` = 'mcp';

-- +goose Down
-- The old default restored, not the individual decisions: which tools an
-- administrator had trusted is not recorded anywhere once cleared, and holding
-- them all again is what a fresh sync would have done under the old rule.
UPDATE `tools` SET `requires_approval` = 1 WHERE `kind` = 'mcp';
