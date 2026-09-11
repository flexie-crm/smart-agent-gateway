-- A step's seq (its position in the transcript) was allocated with SELECT
-- MAX(seq)+1, a read two writers can both win. With async delegation (KB/27) a
-- background specialist runs on its own goroutine, concurrently with the master's
-- turn and off the run manager's one-turn-at-a-time lane. Both would read the same
-- MAX and write the same seq; SaveStep upserts on (session, seq), so the two
-- different steps collapsed into one row (the master's text carrying the
-- specialist's tool calls, stamped as the master). Seq allocation moves to an
-- atomic per-session counter, handed out under a row lock, so every step, master
-- or specialist, gets its own seq and no two collide.

-- +goose Up
ALTER TABLE `agent_sessions`
  ADD COLUMN `next_seq` int(11) NOT NULL DEFAULT 0 AFTER `message_count`;

-- Seed the counter past the highest seq any existing session already used, so the
-- next step continues the transcript instead of colliding with a written row.
UPDATE `agent_sessions` s
  SET s.next_seq = COALESCE(
    (SELECT MAX(st.seq) + 1 FROM `agent_steps` st WHERE st.session_id = s.id),
    0
  );

-- +goose Down
ALTER TABLE `agent_sessions` DROP COLUMN `next_seq`;
