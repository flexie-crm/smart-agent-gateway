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

type attachmentStore struct{ db *sqldb.DB }

const attachmentColumns = `id, public_id, workspace_id, user_id, file_name, file_type,
	size_bytes, extraction, extracted_at, created_at`

func (s *attachmentStore) Create(ctx context.Context, a *model.Attachment) error {
	a.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO attachments (public_id, workspace_id, user_id, file_name, file_type,
			size_bytes, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.PublicID, a.WorkspaceID, a.UserID, a.FileName, a.FileType, a.SizeBytes, a.CreatedAt)
	if err != nil {
		return wrapWriteErr("insert attachment", err)
	}
	a.ID, err = res.LastInsertId()
	return err
}

// ByPublicID finds an attachment the way a caller refers to one, scoped to the
// person who uploaded it. Another user's attachment does not merely fail to
// load: as far as this is concerned it does not exist.
func (s *attachmentStore) ByPublicID(ctx context.Context, workspaceID, userID int64, publicID string) (*model.Attachment, error) {
	return s.scan(s.db.QueryRowContext(ctx,
		`SELECT `+attachmentColumns+` FROM attachments
		 WHERE public_id = ? AND workspace_id = ? AND user_id = ?`,
		publicID, workspaceID, userID))
}

func (s *attachmentStore) scan(row *sql.Row) (*model.Attachment, error) {
	a := &model.Attachment{}
	var extraction sql.NullString
	var extractedAt sql.NullTime
	err := row.Scan(&a.ID, &a.PublicID, &a.WorkspaceID, &a.UserID, &a.FileName, &a.FileType,
		&a.SizeBytes, &extraction, &extractedAt, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read attachment: %w", err)
	}
	a.Extraction = extraction.String
	if extractedAt.Valid {
		a.ExtractedAt = &extractedAt.Time
	}
	return a, nil
}

// ByID is the same lookup without the uploader check. It is used when
// rebuilding a conversation, where the message the file hangs off was already
// established as this person's.
func (s *attachmentStore) ByID(ctx context.Context, workspaceID int64, publicID string) (*model.Attachment, error) {
	return s.scan(s.db.QueryRowContext(ctx,
		`SELECT `+attachmentColumns+` FROM attachments WHERE public_id = ? AND workspace_id = ?`,
		publicID, workspaceID))
}

// SaveExtraction records what the routed model made of the file, so the same
// file is never read twice.
func (s *attachmentStore) SaveExtraction(ctx context.Context, id int64, text string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE attachments SET extraction = ?, extracted_at = ? WHERE id = ?`,
		text, time.Now().UTC(), id); err != nil {
		return fmt.Errorf("save extraction: %w", err)
	}
	return nil
}
