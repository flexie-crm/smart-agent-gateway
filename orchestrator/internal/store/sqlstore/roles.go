package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/store/sqldb"
)

type roleStore struct{ db *sqldb.DB }

func (s *roleStore) Create(ctx context.Context, r *model.Role) error {
	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO roles (workspace_id, name) VALUES (?, ?)`, r.WorkspaceID, r.Name)
		if err != nil {
			return wrapWriteErr("insert role", err)
		}
		if r.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		return replacePermissions(ctx, tx, r.ID, r.Permissions)
	})
}

func (s *roleStore) GetByID(ctx context.Context, workspaceID, id int64) (*model.Role, error) {
	r := &model.Role{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, workspace_id, name FROM roles WHERE id = ? AND workspace_id = ?`,
		id, workspaceID).Scan(&r.ID, &r.WorkspaceID, &r.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get role: %w", err)
	}
	r.Permissions, err = loadRolePermissions(ctx, s.db, r.ID)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (s *roleStore) List(ctx context.Context, workspaceID int64) ([]*model.Role, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, workspace_id, name FROM roles WHERE workspace_id = ? ORDER BY id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	roles := []*model.Role{}
	for rows.Next() {
		r := &model.Role{}
		if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.Name); err != nil {
			return nil, fmt.Errorf("scan role: %w", err)
		}
		roles = append(roles, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range roles {
		if r.Permissions, err = loadRolePermissions(ctx, s.db, r.ID); err != nil {
			return nil, err
		}
	}
	return roles, nil
}

// Update replaces the name and the whole permission set in one transaction:
// a partially applied permission change must never be observable.
func (s *roleStore) Update(ctx context.Context, r *model.Role) error {
	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		// Existence is checked inside the transaction (and locked) because a
		// name-only no-op UPDATE reports zero affected rows on MySQL.
		if err := requireExists(ctx, tx, "update role",
			`SELECT 1 FROM roles WHERE id = ? AND workspace_id = ? FOR UPDATE`, r.ID, r.WorkspaceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE roles SET name = ? WHERE id = ? AND workspace_id = ?`, r.Name, r.ID, r.WorkspaceID); err != nil {
			return wrapWriteErr("update role", err)
		}
		return replacePermissions(ctx, tx, r.ID, r.Permissions)
	})
}

func (s *roleStore) Delete(ctx context.Context, workspaceID, id int64) error {
	// The permissions and the group assignments go with it, because the
	// database says so: a permission belongs to its role, and an assignment is
	// the fact that a role and a group both exist. Neither is a thing that can
	// outlive the role, and neither depends on this function remembering.
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM roles WHERE id = ? AND workspace_id = ?`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("delete role: %w", err)
	}
	return requireAffected(res, "delete role")
}

// execer is satisfied by both the pool and a transaction, so a helper can be
// used inside or outside one. Both are the safe wrapper: neither can be handed
// a query that was built at runtime.
type execer interface {
	ExecContext(ctx context.Context, q sqldb.Statement, args ...any) (sql.Result, error)
}

func replacePermissions(ctx context.Context, tx execer, roleID int64, perms []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM role_permissions WHERE role_id = ?`, roleID); err != nil {
		return fmt.Errorf("clear role permissions: %w", err)
	}
	for _, p := range perms {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO role_permissions (role_id, permission) VALUES (?, ?)`, roleID, p); err != nil {
			return fmt.Errorf("insert role permission: %w", err)
		}
	}
	return nil
}

type querier interface {
	QueryContext(ctx context.Context, q sqldb.Statement, args ...any) (*sql.Rows, error)
}

func loadRolePermissions(ctx context.Context, q querier, roleID int64) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT permission FROM role_permissions WHERE role_id = ? ORDER BY permission`, roleID)
	if err != nil {
		return nil, fmt.Errorf("load role permissions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	perms := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan role permission: %w", err)
		}
		perms = append(perms, p)
	}
	return perms, rows.Err()
}
