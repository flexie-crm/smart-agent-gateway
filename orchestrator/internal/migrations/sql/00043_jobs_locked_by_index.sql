-- Reading back what a claim just took.
--
-- Claiming is two statements: an UPDATE that takes up to N runnable rows under
-- a claim string, then a SELECT of the rows now carrying it. The second had no
-- index to use, so every successful claim scanned the whole table and sorted
-- it, to fetch the three rows it had just written.
--
-- That cost is invisible at first and grows with HISTORY rather than with queue
-- depth: nothing deletes a completed job, so after a few months of one title
-- job per conversation the scan is over a million rows that are all finished.
-- The shape of slowdown nobody can attribute later.

-- +goose Up
ALTER TABLE `jobs` ADD KEY `idx_locked_by` (`locked_by`);

-- +goose Down
ALTER TABLE `jobs` DROP KEY `idx_locked_by`;
