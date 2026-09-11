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

// settingStore keeps one person's preferences: a key, and text under it.
//
// The store deliberately knows nothing about what a value means. "workspace"
// holds an id, tomorrow's "sidebar" may hold a JSON object, and neither costs a
// migration. What it does enforce is whose setting it is: every statement here
// is keyed by user_id, so one person cannot read or overwrite another's.
type settingStore struct{ db *sqldb.DB }

func (s *settingStore) Get(ctx context.Context, userID int64, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		"SELECT `value` FROM user_settings WHERE user_id = ? AND `key` = ?", userID, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get setting: %w", err)
	}
	return value, nil
}

func (s *settingStore) All(ctx context.Context, userID int64) ([]*model.UserSetting, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT `key`, `value`, updated_at FROM user_settings WHERE user_id = ? ORDER BY `key`", userID)
	if err != nil {
		return nil, fmt.Errorf("list settings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	settings := []*model.UserSetting{}
	for rows.Next() {
		v := &model.UserSetting{}
		if err := rows.Scan(&v.Key, &v.Value, &v.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		settings = append(settings, v)
	}
	return settings, rows.Err()
}

// Set writes the value, whether or not there was one. A preference has no
// history worth keeping: the last thing the person chose is the whole truth.
func (s *settingStore) Set(ctx context.Context, userID int64, key, value string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO user_settings (user_id, `key`, `value`, created_at, updated_at)\n"+
			"VALUES (?, ?, ?, ?, ?)\n"+
			"ON DUPLICATE KEY UPDATE `value` = VALUES(`value`), updated_at = VALUES(updated_at)",
		userID, key, value, now, now)
	if err != nil {
		return wrapWriteErr("set setting", err)
	}
	return nil
}

// Delete forgets the preference. Deleting one that was never set is not an
// error: the caller asked for it to be gone, and it is gone.
func (s *settingStore) Delete(ctx context.Context, userID int64, key string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM user_settings WHERE user_id = ? AND `key` = ?", userID, key)
	if err != nil {
		return fmt.Errorf("delete setting: %w", err)
	}
	return nil
}
