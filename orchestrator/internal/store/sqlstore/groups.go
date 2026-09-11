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

type groupStore struct{ db *sqldb.DB }

func (s *groupStore) Create(ctx context.Context, g *model.Group) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO user_groups_def (workspace_id, name) VALUES (?, ?)`, g.WorkspaceID, g.Name)
	if err != nil {
		return wrapWriteErr("insert group", err)
	}
	g.ID, err = res.LastInsertId()
	return err
}

func (s *groupStore) GetByID(ctx context.Context, workspaceID, id int64) (*model.Group, error) {
	g := &model.Group{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, workspace_id, name FROM user_groups_def WHERE id = ? AND workspace_id = ?`,
		id, workspaceID).Scan(&g.ID, &g.WorkspaceID, &g.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get group: %w", err)
	}
	return g, nil
}

func (s *groupStore) List(ctx context.Context, workspaceID int64) ([]*model.Group, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, workspace_id, name FROM user_groups_def WHERE workspace_id = ? ORDER BY id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanGroups(rows)
}

func (s *groupStore) Update(ctx context.Context, g *model.Group) error {
	if err := requireExists(ctx, s.db, "update group",
		`SELECT 1 FROM user_groups_def WHERE id = ? AND workspace_id = ?`, g.ID, g.WorkspaceID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE user_groups_def SET name = ? WHERE id = ? AND workspace_id = ?`,
		g.Name, g.ID, g.WorkspaceID)
	if err != nil {
		return wrapWriteErr("update group", err)
	}
	return nil
}

// Delete removes the group with its memberships and role assignments, so no
// orphan rows can grant permissions after the group is gone.
func (s *groupStore) Delete(ctx context.Context, workspaceID, id int64) error {
	// The memberships, the role assignments and the tool grants go with it. A
	// membership is not a thing in its own right: it is the fact that a user and
	// a group both exist, and the database knows that.
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM user_groups_def WHERE id = ? AND workspace_id = ?`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	return requireAffected(res, "delete group")
}

// AddMember is idempotent: re-adding an existing member is not an error.
//
// A group belongs to a workspace, so its members must be people who may act in
// that workspace. The INSERT ... SELECT is the check: it inserts a row only if
// the person is a member of the group's workspace, and a request to put an
// outsider in a group matches nothing and is refused rather than quietly
// granting them the group's roles.
func (s *groupStore) AddMember(ctx context.Context, groupID, userID int64) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT IGNORE INTO user_group_members (user_id, group_id)
		 SELECT ?, g.id FROM user_groups_def g
		 JOIN workspace_members m ON m.workspace_id = g.workspace_id AND m.user_id = ?
		 WHERE g.id = ?`, userID, userID, groupID)
	if err != nil {
		return fmt.Errorf("add group member: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("add group member: %w", err)
	}
	if n == 0 {
		// Either the row is already there, or the person does not belong in
		// this workspace. Only the second is an error, so ask.
		var one int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM user_group_members WHERE user_id = ? AND group_id = ?`,
			userID, groupID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("add group member: %w", store.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("add group member: %w", err)
		}
	}
	return nil
}

func (s *groupStore) RemoveMember(ctx context.Context, groupID, userID int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM user_group_members WHERE user_id = ? AND group_id = ?`, userID, groupID)
	if err != nil {
		return fmt.Errorf("remove group member: %w", err)
	}
	return requireAffected(res, "remove group member")
}

func (s *groupStore) ListMembers(ctx context.Context, groupID int64) ([]*model.User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT u.id, u.email, u.name, u.password_hash, u.status, u.created_at, u.updated_at
		 FROM users u
		 JOIN user_group_members ugm ON ugm.user_id = u.id
		 WHERE ugm.group_id = ? ORDER BY u.id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list group members: %w", err)
	}
	defer func() { _ = rows.Close() }()

	users := []*model.User{}
	for rows.Next() {
		u := &model.User{}
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash,
			&u.Status, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// SetForUser makes the person's memberships among THIS workspace's groups
// exactly the given list, in one transaction: a save that names a group from
// another workspace leaves the memberships they had, not half of the new list.
// The rule that a member must be able to act in the group's workspace holds
// here the same way it does in AddMember.
func (s *groupStore) SetForUser(ctx context.Context, workspaceID, userID int64, groupIDs []int64) error {
	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		if err := requireExists(ctx, tx, "set user groups",
			`SELECT 1 FROM workspace_members WHERE workspace_id = ? AND user_id = ?`,
			workspaceID, userID); err != nil {
			return err
		}
		for _, groupID := range groupIDs {
			if err := requireExists(ctx, tx, "set user groups",
				`SELECT 1 FROM user_groups_def WHERE id = ? AND workspace_id = ?`,
				groupID, workspaceID); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE ugm FROM user_group_members ugm
			 JOIN user_groups_def g ON g.id = ugm.group_id
			 WHERE ugm.user_id = ? AND g.workspace_id = ?`, userID, workspaceID); err != nil {
			return fmt.Errorf("clear user groups: %w", err)
		}
		for _, groupID := range groupIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT IGNORE INTO user_group_members (user_id, group_id) VALUES (?, ?)`,
				userID, groupID); err != nil {
				return wrapWriteErr("add user group", err)
			}
		}
		return nil
	})
}

func (s *groupStore) ListForUser(ctx context.Context, userID int64) ([]*model.Group, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT g.id, g.workspace_id, g.name
		 FROM user_groups_def g
		 JOIN user_group_members ugm ON ugm.group_id = g.id
		 WHERE ugm.user_id = ? ORDER BY g.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list groups for user: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanGroups(rows)
}

func (s *groupStore) AssignRole(ctx context.Context, groupID, roleID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT IGNORE INTO group_roles (group_id, role_id) VALUES (?, ?)`, groupID, roleID)
	if err != nil {
		return fmt.Errorf("assign role: %w", err)
	}
	return nil
}

func (s *groupStore) UnassignRole(ctx context.Context, groupID, roleID int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM group_roles WHERE group_id = ? AND role_id = ?`, groupID, roleID)
	if err != nil {
		return fmt.Errorf("unassign role: %w", err)
	}
	return requireAffected(res, "unassign role")
}

func (s *groupStore) ListRoles(ctx context.Context, groupID int64) ([]*model.Role, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id, r.workspace_id, r.name
		 FROM roles r
		 JOIN group_roles gr ON gr.role_id = r.id
		 WHERE gr.group_id = ? ORDER BY r.id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list group roles: %w", err)
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
		perms, err := loadRolePermissions(ctx, s.db, r.ID)
		if err != nil {
			return nil, err
		}
		r.Permissions = perms
	}
	return roles, nil
}

func scanGroups(rows *sql.Rows) ([]*model.Group, error) {
	groups := []*model.Group{}
	for rows.Next() {
		g := &model.Group{}
		if err := rows.Scan(&g.ID, &g.WorkspaceID, &g.Name); err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}
