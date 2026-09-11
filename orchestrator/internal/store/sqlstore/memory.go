package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"flexie.io/sag/internal/store/sqldb"
)

// memoryStore holds what the assistant remembers. It knows nothing about what a
// memory means, only whose it is: every statement is keyed by workspace (and,
// for a person's memory, by user), so one scope can never read or overwrite
// another's. A memory that was never written reads as empty, because a fresh
// person and a fresh workspace simply have nothing to remember yet.
type memoryStore struct{ db *sqldb.DB }

func (s *memoryStore) UserMemory(ctx context.Context, workspaceID, userID int64) (string, error) {
	return content(s.db.QueryRowContext(ctx,
		"SELECT content FROM ai_user_memory WHERE workspace_id = ? AND user_id = ?",
		workspaceID, userID))
}

func (s *memoryStore) SetUserMemory(ctx context.Context, workspaceID, userID int64, text string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO ai_user_memory (workspace_id, user_id, content, created_at, updated_at)\n"+
			"VALUES (?, ?, ?, ?, ?)\n"+
			"ON DUPLICATE KEY UPDATE content = VALUES(content), updated_at = VALUES(updated_at)",
		workspaceID, userID, text, now, now)
	if err != nil {
		return wrapWriteErr("set user memory", err)
	}
	return nil
}

func (s *memoryStore) WorkspaceMemory(ctx context.Context, workspaceID int64) (string, error) {
	return content(s.db.QueryRowContext(ctx,
		"SELECT content FROM ai_workspace_memory WHERE workspace_id = ?",
		workspaceID))
}

func (s *memoryStore) SetWorkspaceMemory(ctx context.Context, workspaceID int64, text string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO ai_workspace_memory (workspace_id, content, created_at, updated_at)\n"+
			"VALUES (?, ?, ?, ?)\n"+
			"ON DUPLICATE KEY UPDATE content = VALUES(content), updated_at = VALUES(updated_at)",
		workspaceID, text, now, now)
	if err != nil {
		return wrapWriteErr("set workspace memory", err)
	}
	return nil
}

// content scans a single content column, treating a missing row as empty: a
// scope with no memory yet is not an error.
func content(row *sql.Row) (string, error) {
	var text string
	err := row.Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get memory: %w", err)
	}
	return text, nil
}
