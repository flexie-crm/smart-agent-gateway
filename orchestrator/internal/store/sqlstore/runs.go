package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/store/sqldb"
)

type runStore struct{ db *sqldb.DB }

const runColumns = `id, uid, workspace_id, session_id, user_id, status, created_at, completed_at`

func (s *runStore) Create(ctx context.Context, r *model.Run) error {
	r.CreatedAt = time.Now().UTC()
	if r.Status == "" {
		r.Status = model.RunRunning
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_runs (uid, workspace_id, session_id, user_id, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.UID, r.WorkspaceID, r.SessionID, r.UserID, r.Status, r.CreatedAt)
	if err != nil {
		return wrapWriteErr("insert run", err)
	}
	r.ID, err = res.LastInsertId()
	return err
}

// SetStatus closes a run out. completed_at is set for every terminal status,
// because "when did this stop" is a question worth answering for a failure just
// as much as for a success.
func (s *runStore) SetStatus(ctx context.Context, id int64, status string) error {
	var completedAt any
	if status != model.RunRunning {
		completedAt = time.Now().UTC()
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE agent_runs SET status = ?, completed_at = ? WHERE id = ?`,
		status, completedAt, id); err != nil {
		return fmt.Errorf("set run status: %w", err)
	}
	return nil
}

func (s *runStore) GetByUID(ctx context.Context, workspaceID int64, uid string) (*model.Run, error) {
	r := &model.Run{}
	err := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM agent_runs WHERE workspace_id = ? AND uid = ?`,
		workspaceID, uid).
		Scan(&r.ID, &r.UID, &r.WorkspaceID, &r.SessionID, &r.UserID, &r.Status,
			&r.CreatedAt, &r.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan run: %w", err)
	}
	return r, nil
}

// MarkInterrupted closes out the runs a dead process left behind.
//
// A run whose process died is not running, and a row that says otherwise is a
// lie that never expires. It is called at boot, before this process starts
// serving: nothing else can be running yet, so every row that claims to be is
// a ghost.
func (s *runStore) MarkInterrupted(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_runs SET status = ?, completed_at = ? WHERE status = ?`,
		model.RunInterrupted, time.Now().UTC(), model.RunRunning)
	if err != nil {
		return 0, fmt.Errorf("mark interrupted runs: %w", err)
	}
	return res.RowsAffected()
}
