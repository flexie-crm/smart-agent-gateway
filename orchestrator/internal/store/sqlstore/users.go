package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/store/sqldb"
)

// The driver error numbers we have something to say about.
const (
	mysqlDuplicateEntry  = 1062 // a unique key was violated
	mysqlRowIsReferenced = 1451 // a parent still has children that forbid its removal
	mysqlNoReferencedRow = 1452 // a child points at a parent that does not exist
)

// wrapWriteErr turns what the database refused into what the caller can say
// about it, so an integrity rule reaches the user as an answer rather than as a
// 500 with a driver code in it.
//
//   - a duplicate key is a conflict (409);
//   - a child pointing at a parent that is not there is a request naming
//     something that does not exist (404), not a server fault: an admin saving
//     an agent against a model somebody deleted a moment ago is the ordinary
//     case, not a bug;
//   - a parent that cannot be removed because something still depends on it is
//     in use (409).
func wrapWriteErr(op string, err error) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case mysqlDuplicateEntry:
			return fmt.Errorf("%s: %w", op, store.ErrConflict)
		case mysqlNoReferencedRow:
			return fmt.Errorf("%s: %w", op, store.ErrNotFound)
		case mysqlRowIsReferenced:
			return fmt.Errorf("%s: %w", op, store.ErrInUse)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

type workspaceStore struct{ db *sqldb.DB }

func (s *workspaceStore) Create(ctx context.Context, w *model.Workspace) error {
	now := time.Now().UTC()
	w.CreatedAt, w.UpdatedAt = now, now
	if w.Status == "" {
		w.Status = model.StatusActive
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO workspaces (slug, name, description, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		w.Slug, w.Name, w.Description, w.Status, w.CreatedAt, w.UpdatedAt)
	if err != nil {
		return wrapWriteErr("insert workspace", err)
	}
	w.ID, err = res.LastInsertId()
	return err
}

func (s *workspaceStore) GetByID(ctx context.Context, id int64) (*model.Workspace, error) {
	return scanWorkspace(s.db.QueryRowContext(ctx,
		`SELECT id, slug, name, description, status, created_at, updated_at FROM workspaces WHERE id = ?`, id))
}

func (s *workspaceStore) GetBySlug(ctx context.Context, slug string) (*model.Workspace, error) {
	return scanWorkspace(s.db.QueryRowContext(ctx,
		`SELECT id, slug, name, description, status, created_at, updated_at FROM workspaces WHERE slug = ?`, slug))
}

func (s *workspaceStore) List(ctx context.Context) ([]*model.Workspace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, slug, name, description, status, created_at, updated_at FROM workspaces ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()

	workspaces := []*model.Workspace{}
	for rows.Next() {
		w := &model.Workspace{}
		if err := rows.Scan(&w.ID, &w.Slug, &w.Name, &w.Description, &w.Status, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		workspaces = append(workspaces, w)
	}
	return workspaces, rows.Err()
}

func (s *workspaceStore) Update(ctx context.Context, w *model.Workspace) error {
	if err := requireExists(ctx, s.db, "update workspace",
		`SELECT 1 FROM workspaces WHERE id = ?`, w.ID); err != nil {
		return err
	}
	w.UpdatedAt = time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE workspaces SET slug = ?, name = ?, description = ?, status = ?, updated_at = ? WHERE id = ?`,
		w.Slug, w.Name, w.Description, w.Status, w.UpdatedAt, w.ID); err != nil {
		return wrapWriteErr("update workspace", err)
	}
	return nil
}

// Delete removes the workspace. The database's own cascades take everything
// scoped to it: memberships, groups, roles, vendors, models, agents,
// workflows, brains, and every conversation.
func (s *workspaceStore) Delete(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, id)
	if err != nil {
		return wrapWriteErr("delete workspace", err)
	}
	return requireAffected(res, "delete workspace")
}

// AddMember is idempotent: re-adding an existing member is not an error.
func (s *workspaceStore) AddMember(ctx context.Context, workspaceID, userID int64) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT IGNORE INTO workspace_members (workspace_id, user_id, created_at) VALUES (?, ?, ?)`,
		workspaceID, userID, time.Now().UTC()); err != nil {
		return wrapWriteErr("add workspace member", err)
	}
	return nil
}

// ListForUser returns the workspaces this person may act in, and only the
// active ones: a suspended workspace is not somewhere to switch to.
func (s *workspaceStore) ListForUser(ctx context.Context, userID int64) ([]*model.Workspace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT w.id, w.slug, w.name, w.description, w.status, w.created_at, w.updated_at
		 FROM workspaces w
		 JOIN workspace_members m ON m.workspace_id = w.id
		 WHERE m.user_id = ? AND w.status = 'active'
		 ORDER BY w.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list workspaces for user: %w", err)
	}
	defer func() { _ = rows.Close() }()

	workspaces := []*model.Workspace{}
	for rows.Next() {
		w := &model.Workspace{}
		if err := rows.Scan(&w.ID, &w.Slug, &w.Name, &w.Description, &w.Status, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		workspaces = append(workspaces, w)
	}
	return workspaces, rows.Err()
}

// IsMember answers the only question a workspace switch asks. The status of the
// workspace is part of the answer: membership of a suspended workspace grants
// nothing.
func (s *workspaceStore) IsMember(ctx context.Context, workspaceID, userID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM workspace_members m
		 JOIN workspaces w ON w.id = m.workspace_id
		 WHERE m.workspace_id = ? AND m.user_id = ? AND w.status = 'active'`,
		workspaceID, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check membership: %w", err)
	}
	return true, nil
}

// SetMembers makes the person's memberships exactly the given list. It runs in
// one transaction, so a save that names a workspace that does not exist leaves
// the memberships they had, not half of the ones they asked for.
func (s *workspaceStore) SetMembers(ctx context.Context, userID int64, workspaceIDs []int64) error {
	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM workspace_members WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("clear memberships: %w", err)
		}
		now := time.Now().UTC()
		for _, id := range workspaceIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO workspace_members (workspace_id, user_id, created_at) VALUES (?, ?, ?)`,
				id, userID, now); err != nil {
				return wrapWriteErr("add membership", err)
			}
		}
		return nil
	})
}

func (s *workspaceStore) AllMemberships(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, workspace_id FROM workspace_members ORDER BY user_id, workspace_id`)
	if err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	defer func() { _ = rows.Close() }()

	memberships := map[int64][]int64{}
	for rows.Next() {
		var userID, workspaceID int64
		if err := rows.Scan(&userID, &workspaceID); err != nil {
			return nil, fmt.Errorf("scan membership: %w", err)
		}
		memberships[userID] = append(memberships[userID], workspaceID)
	}
	return memberships, rows.Err()
}

func scanWorkspace(row *sql.Row) (*model.Workspace, error) {
	w := &model.Workspace{}
	err := row.Scan(&w.ID, &w.Slug, &w.Name, &w.Description, &w.Status, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan workspace: %w", err)
	}
	return w, nil
}

type userStore struct{ db *sqldb.DB }

const userColumns = `id, email, name, password_hash, status, created_at, updated_at`

func (s *userStore) Create(ctx context.Context, u *model.User) error {
	now := time.Now().UTC()
	u.CreatedAt, u.UpdatedAt = now, now
	if u.Status == "" {
		u.Status = model.StatusActive
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (email, name, password_hash, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		u.Email, u.Name, u.PasswordHash, u.Status, u.CreatedAt, u.UpdatedAt)
	if err != nil {
		return wrapWriteErr("insert user", err)
	}
	u.ID, err = res.LastInsertId()
	return err
}

func (s *userStore) GetByEmail(ctx context.Context, email string) (*model.User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE email = ?`, email))
}

func (s *userStore) GetByID(ctx context.Context, id int64) (*model.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

func (s *userStore) List(ctx context.Context) ([]*model.User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userColumns+` FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	users := []*model.User{}
	for rows.Next() {
		u := &model.User{}
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash,
			&u.Status, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *userStore) Update(ctx context.Context, u *model.User) error {
	if err := requireExists(ctx, s.db, "update user",
		`SELECT 1 FROM users WHERE id = ?`, u.ID); err != nil {
		return err
	}
	u.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET email = ?, name = ?, status = ?, updated_at = ? WHERE id = ?`,
		u.Email, u.Name, u.Status, u.UpdatedAt, u.ID)
	if err != nil {
		return wrapWriteErr("update user", err)
	}
	return nil
}

func (s *userStore) UpdatePassword(ctx context.Context, userID int64, passwordHash string) error {
	if err := requireExists(ctx, s.db, "update password",
		`SELECT 1 FROM users WHERE id = ?`, userID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		passwordHash, time.Now().UTC(), userID)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	return nil
}

// Delete removes the user. Their memberships, sessions, conversations and
// parked approvals go with them, because the database says a person's things
// belong to that person. Their access tokens are revoked by the caller as well:
// a deleted user's live token must die immediately rather than at expiry, and
// that is a policy, not an integrity rule.
func (s *userStore) Delete(ctx context.Context, userID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return requireAffected(res, "delete user")
}

func (s *userStore) EffectivePermissions(ctx context.Context, userID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT rp.permission
		 FROM user_group_members ugm
		 JOIN group_roles gr ON gr.group_id = ugm.group_id
		 JOIN role_permissions rp ON rp.role_id = gr.role_id
		 WHERE ugm.user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("effective permissions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	perms := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan permission: %w", err)
		}
		perms = append(perms, p)
	}
	return perms, rows.Err()
}

func scanUser(row *sql.Row) (*model.User, error) {
	u := &model.User{}
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash,
		&u.Status, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	return u, nil
}

// requireAffected turns a write that matched nothing (wrong id, or an id
// from another workspace) into ErrNotFound instead of silent success. It is
// only valid for DELETE and for writes that always change a value: MySQL
// reports zero affected rows for an UPDATE whose new values equal the old
// ones, so an UPDATE must verify existence with requireExists instead.
func requireAffected(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, q sqldb.Statement, args ...any) *sql.Row
}

// requireExists reports ErrNotFound when the scoped row is absent. UPDATE
// paths call it because RowsAffected cannot distinguish "row missing" from
// "row unchanged" on MySQL.
func requireExists(ctx context.Context, q rowQuerier, op string, query sqldb.Statement, args ...any) error {
	var one int
	err := q.QueryRowContext(ctx, query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}
