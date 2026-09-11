package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/store/sqldb"
)

// The durable job queue.
//
// Two properties are worth reading the statements for, because everything else
// here is bookkeeping.
//
// CLAIMING IS ONE STATEMENT. The UPDATE below is the claim: it matches only
// pending, runnable rows, and InnoDB takes a row lock as it scans, so two
// workers racing for the same job cannot both win. There is no SELECT-then-
// UPDATE window to lose, and no transaction to hold open across it.
//
// AND THE CLAIM IS UNIQUE PER CALL, not per worker. `locked_by` is
// "<instance>#<token>", so the SELECT that reads back what the UPDATE just took
// cannot pick up a row a sibling goroutine on the same node claimed a
// microsecond earlier. That is the whole reason the token exists; the instance
// half is there so the reclaim scan and a person reading the table can still
// see which node holds what.
type jobStore struct{ db *sqldb.DB }

func (s *jobStore) Enqueue(ctx context.Context, j *model.Job) error {
	if j.ID == "" {
		id, err := model.NewJobUID()
		if err != nil {
			return fmt.Errorf("mint job id: %w", err)
		}
		j.ID = id
	}
	// The status is not the caller's to choose. A row enqueued as anything but
	// pending matches neither Claim (which wants pending) nor Reclaim (which
	// wants running with a lock), so it is committed, counted, and will never
	// run: a model that never downloads with nothing anywhere to explain why.
	j.Status = model.JobPending
	j.Attempts = 0
	if j.MaxAttempts == 0 {
		j.MaxAttempts = model.DefaultJobAttempts
	}
	// Both times come from the DATABASE unless the caller asked for a later one.
	// One clock decides what is runnable and what is stale, so a node whose
	// clock is a minute fast cannot claim work before its time.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (id, workspace_id, kind, subject, payload, status,
		                   attempts, max_attempts, run_after, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 0, ?, COALESCE(?, UTC_TIMESTAMP(3)), UTC_TIMESTAMP(3))`,
		j.ID, j.WorkspaceID, j.Kind, j.Subject, nullJSONBytes(j.Payload), j.Status,
		j.MaxAttempts, nullTime(j.RunAfter))
	if err != nil {
		return wrapWriteErr("enqueue job", err)
	}
	return nil
}

// EnqueueMany writes a batch of jobs with a multi-row insert.
//
// The shape a dump uses: one INSERT carrying many rows, rather than a round trip
// each. A batch of a thousand agents is a thousand jobs, and writing them one at
// a time would make dispatching a batch slower than running it.
//
// In chunks, because a prepared statement is cached by its text and a statement
// per batch size would be a cache with no end to it. A hundred rows a chunk
// makes a thousand jobs ten statements.
//
// Ids are minted here, so nothing has to be read back: a job's id is ours to
// choose and is also the key a handler dedupes on.
func (s *jobStore) EnqueueMany(ctx context.Context, jobs []*model.Job) error {
	if len(jobs) == 0 {
		return nil
	}
	for _, j := range jobs {
		if j.ID == "" {
			id, err := model.NewJobUID()
			if err != nil {
				return fmt.Errorf("mint job id: %w", err)
			}
			j.ID = id
		}
		// The status is not the caller's to choose, for the same reason it is
		// not in Enqueue: a row that is not pending will never be claimed.
		j.Status = model.JobPending
		j.Attempts = 0
		if j.MaxAttempts == 0 {
			j.MaxAttempts = model.DefaultJobAttempts
		}
	}

	// Both times come from the DATABASE, as in Enqueue: one clock decides what
	// is runnable.
	const insert sqldb.Statement = `INSERT INTO jobs
	  (id, workspace_id, kind, subject, payload, status, attempts, max_attempts,
	   run_after, created_at)
	 VALUES `
	const cols = 10

	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		// The database's clock, read ONCE and bound into every row. Still one
		// clock deciding what is runnable, which is the property that matters:
		// a node whose clock is a minute fast cannot claim work before its time.
		var now time.Time
		if err := tx.QueryRowContext(ctx, `SELECT UTC_TIMESTAMP(3)`).Scan(&now); err != nil {
			return fmt.Errorf("read the database clock: %w", err)
		}
		for start := 0; start < len(jobs); start += sqldb.RowChunk {
			end := start + sqldb.RowChunk
			if end > len(jobs) {
				end = len(jobs)
			}
			chunk := jobs[start:end]
			args := make([]any, 0, len(chunk)*cols)
			for _, j := range chunk {
				args = append(args, j.ID, j.WorkspaceID, j.Kind, j.Subject,
					nullJSONBytes(j.Payload), j.Status, 0, j.MaxAttempts, now, now)
			}
			if _, err := tx.ExecContext(ctx, insert.Rows(len(chunk), cols), args...); err != nil {
				return wrapWriteErr("enqueue jobs", err)
			}
		}
		return nil
	})
}

func (s *jobStore) Claim(ctx context.Context, claim, subjectPrefix string, limit int) ([]*model.Job, error) {
	if limit <= 0 {
		return nil, nil
	}
	// Oldest first, so a job that has waited does not starve behind a stream of
	// new ones. Attempts is incremented HERE rather than on failure: a handler
	// that panics or a worker that is killed has still had its go, and counting
	// only clean failures is how a job that kills its worker gets retried
	// forever.
	// LEFT(subject, n) = prefix, not `LIKE prefix%`. LIKE reads `_` and `%` in a
	// capability name as wildcards, so a node started to serve "model_host"
	// would silently claim "model-host" work it has no handler for, fail it to
	// death, and the machine that could have run it would never see the row. A
	// byte-range would have been the other way to say it, and it is wrong here:
	// this column has a collation, so `<` is not byte order.
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs
		    SET status = ?, locked_by = ?, locked_at = UTC_TIMESTAMP(3), attempts = attempts + 1
		  WHERE status = ? AND run_after <= UTC_TIMESTAMP(3)
		    AND LEFT(subject, ?) = ?
		  ORDER BY run_after
		  LIMIT ?`,
		model.JobRunning, claim, model.JobPending,
		utf8.RuneCountInString(subjectPrefix), subjectPrefix, limit)
	if err != nil {
		return nil, wrapWriteErr("claim jobs", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, workspace_id, kind, subject, payload, status, attempts,
		        max_attempts, last_error, run_after, locked_by, locked_at,
		        created_at, completed_at
		   FROM jobs WHERE locked_by = ? AND status = ? ORDER BY run_after`,
		claim, model.JobRunning)
	if err != nil {
		// The claim committed and the caller will never learn which rows it
		// took, so it cannot hand them back. Undo it here, on a context that
		// does not share the failure, or the work sits `running` under a claim
		// nobody holds until the stale window expires (the same detached-context
		// rule background delegations settle under, KB/27).
		s.abandon(context.WithoutCancel(ctx), claim)
		return nil, fmt.Errorf("read claimed jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []*model.Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			s.abandon(context.WithoutCancel(ctx), claim)
			return nil, err
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		s.abandon(context.WithoutCancel(ctx), claim)
		return nil, err
	}
	return out, nil
}

// abandon puts back everything a claim took, attempt included: the delivery
// never happened, so it must not be counted against the job.
func (s *jobStore) abandon(ctx context.Context, claim string) {
	_, _ = s.db.ExecContext(ctx,
		`UPDATE jobs
		    SET status = ?, locked_by = NULL, locked_at = NULL,
		        attempts = GREATEST(attempts - 1, 0)
		  WHERE locked_by = ? AND status = ?`,
		model.JobPending, claim, model.JobRunning)
}

// Heartbeat pushes the lease out. Losing the job is a thing the worker must be
// TOLD, rather than left to discover by having its Complete refused an hour
// later, so this reports ErrNotFound when the claim no longer holds the row.
//
// What it must not do is confuse that with having nothing to change. An UPDATE
// reports rows CHANGED, not rows matched, and locked_at is a datetime(3): two
// heartbeats inside the same millisecond write the same value, nothing changes,
// and reading that as "you have lost the job" tells a worker holding a perfectly
// good claim to stop, which for a model pull means abandoning a download that
// was going to succeed. So zero is not an answer on its own: it is a question,
// and it is asked. Only on that path, which is rare enough to be free.
func (s *jobStore) Heartbeat(ctx context.Context, id, claim string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET locked_at = UTC_TIMESTAMP(3)
		  WHERE id = ? AND locked_by = ? AND status = ?`,
		id, claim, model.JobRunning)
	if err != nil {
		return wrapWriteErr("heartbeat job", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var held int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE id = ? AND locked_by = ? AND status = ?`,
		id, claim, model.JobRunning).Scan(&held); err != nil {
		return fmt.Errorf("heartbeat job: %w", err)
	}
	if held == 0 {
		return fmt.Errorf("heartbeat job: %w", store.ErrNotFound)
	}
	return nil
}

func (s *jobStore) Complete(ctx context.Context, id, claim string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, completed_at = UTC_TIMESTAMP(3), locked_by = NULL, locked_at = NULL
		  WHERE id = ? AND locked_by = ?`,
		model.JobCompleted, id, claim)
	if err != nil {
		return wrapWriteErr("complete job", err)
	}
	// Scoped to the claim, so a worker whose job was reclaimed out from under it
	// (it stalled long enough to look dead) cannot mark somebody else's run
	// finished. It learns it has lost the job instead.
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("complete job: %w", store.ErrNotFound)
	}
	return nil
}

func (s *jobStore) Fail(ctx context.Context, id, claim, reason string, runAfter time.Time) error {
	// One statement decides between "try again later" and "leave it dead",
	// because two would need a transaction to stop a reclaim landing between
	// them. attempts was already incremented at claim time, so this compares
	// what has been spent against what was allowed.
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs
		    SET status = IF(attempts >= max_attempts, ?, ?),
		        last_error = ?,
		        run_after = ?,
		        completed_at = IF(attempts >= max_attempts, UTC_TIMESTAMP(3), NULL),
		        locked_by = NULL,
		        locked_at = NULL
		  WHERE id = ? AND locked_by = ?`,
		model.JobDead, model.JobPending, truncateError(reason), runAfter,
		id, claim)
	if err != nil {
		return wrapWriteErr("fail job", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("fail job: %w", store.ErrNotFound)
	}
	return nil
}

func (s *jobStore) Release(ctx context.Context, id, claim string) error {
	// The attempt is given back as well. Being asked to stop is not a failure,
	// and a worker restarted three times during a deploy must not exhaust a
	// job's attempts without ever having run it.
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs
		    SET status = ?, locked_by = NULL, locked_at = NULL,
		        attempts = GREATEST(attempts - 1, 0)
		  WHERE id = ? AND locked_by = ? AND status = ?`,
		model.JobPending, id, claim, model.JobRunning)
	if err != nil {
		return wrapWriteErr("release job", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("release job: %w", store.ErrNotFound)
	}
	return nil
}

func (s *jobStore) Reclaim(ctx context.Context, staleAfter, retryAfter time.Duration) (int64, error) {
	// A claim older than the cutoff belongs to a worker that is not coming
	// back. The attempt it took is NOT given back: it did have its go, and a
	// job that reliably kills whatever picks it up must run out of attempts
	// rather than circulate forever.
	//
	// The cutoff is inclusive, which matters only at the edge and only for a
	// caller passing zero: the column is datetime(3), so a claim and a reclaim
	// in the same millisecond are the same instant, and "everything claimed" has
	// to include the one claimed just now or it means nothing.
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs
		    SET status = IF(attempts >= max_attempts, ?, ?),
		        last_error = ?,
		        locked_by = NULL, locked_at = NULL,
		        run_after = UTC_TIMESTAMP(3) + INTERVAL ? MICROSECOND,
		        completed_at = IF(attempts >= max_attempts, UTC_TIMESTAMP(3), NULL)
		  WHERE status = ? AND locked_at <= UTC_TIMESTAMP(3) - INTERVAL ? MICROSECOND`,
		model.JobDead, model.JobPending,
		"The worker running this stopped before it finished.",
		retryAfter.Microseconds(), model.JobRunning, staleAfter.Microseconds())
	if err != nil {
		return 0, wrapWriteErr("reclaim jobs", err)
	}
	return res.RowsAffected()
}

func (s *jobStore) Get(ctx context.Context, id string) (*model.Job, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, workspace_id, kind, subject, payload, status, attempts,
		        max_attempts, last_error, run_after, locked_by, locked_at,
		        created_at, completed_at
		   FROM jobs WHERE id = ?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return j, err
}

type scanner interface{ Scan(dest ...any) error }

func scanJob(row scanner) (*model.Job, error) {
	var (
		j         model.Job
		payload   []byte
		lastError sql.NullString
		lockedBy  sql.NullString
		lockedAt  sql.NullTime
		completed sql.NullTime
	)
	err := row.Scan(&j.ID, &j.WorkspaceID, &j.Kind, &j.Subject, &payload, &j.Status,
		&j.Attempts, &j.MaxAttempts, &lastError, &j.RunAfter, &lockedBy, &lockedAt,
		&j.CreatedAt, &completed)
	if err != nil {
		return nil, err
	}
	j.Payload = payload
	j.LastError = lastError.String
	j.LockedBy = lockedBy.String
	if lockedAt.Valid {
		j.LockedAt = &lockedAt.Time
	}
	if completed.Valid {
		j.CompletedAt = &completed.Time
	}
	return &j, nil
}

// nullJSONBytes keeps an empty payload out of the column, which has a
// json_valid CHECK: an empty string is not valid JSON, and a job with nothing
// to say should store nothing rather than "{}".
func nullJSONBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// truncateError bounds what a vendor or a driver can write into the row. The
// column is TEXT, and a runaway message is a row nobody can read anyway.
//
// It cuts on a RUNE boundary. Cutting on a byte leaves half a character, which
// the connection rejects outright in strict mode, so the whole UPDATE fails:
// the reason is lost, the backoff is never written, and the job sits running
// under a claim nobody holds. The failure path failing is worse than a long
// message, and it only happens once the message is not English.
func truncateError(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// nullTime binds a zero time as NULL, so the statement can fall back to the
// database's own clock rather than to one this process guessed.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
