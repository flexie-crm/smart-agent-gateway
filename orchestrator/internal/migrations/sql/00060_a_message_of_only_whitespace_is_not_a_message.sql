-- Rows that hold nothing, written because "\n\n" is not the empty string.
--
-- A model about to call a tool commonly emits two newlines as its text.
-- `HasText` asked `Text != ""`, which is true of that, so a message row was
-- written holding only whitespace: 49 of them in one installation. The
-- transcript then served each as a message, every reader had to know to skip
-- them, and the chat drew each as a zero-height element that still cost the
-- timeline's gap above and below it, which is a visible band of nothing between
-- two tool rows.
--
-- HasText now trims, so no new ones are written. These are the ones already
-- there, and they are deleted rather than left, because a reader that has to
-- know which stored messages are not really messages is the defect, not the
-- rendering.
--
-- Deletes nothing anybody wrote: the step, its reasoning and its tool calls are
-- untouched, and a step whose only content was whitespace said nothing that a
-- person could have meant. The step row stays, so a turn keeps its shape and
-- its tool calls stay attached to it.

-- +goose Up
DELETE FROM `agent_messages` WHERE TRIM(BOTH '\r' FROM TRIM(BOTH '\n' FROM TRIM(BOTH '\t' FROM TRIM(`content`)))) = '';

-- +goose Down
-- Nothing to restore: the rows held no content to put back.
SELECT 1;
