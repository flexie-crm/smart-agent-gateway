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

type sessionStore struct{ db *sqldb.DB }

func (s *sessionStore) Create(ctx context.Context, us *model.UserSession) error {
	us.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO user_sessions
		 (token_hash, family_id, user_id, workspace_id, user_agent, ip, used, revoked, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		us.TokenHash, us.FamilyID, us.UserID, us.WorkspaceID, nullString(us.UserAgent), nullString(us.IP),
		us.Used, us.Revoked, us.ExpiresAt, us.CreatedAt)
	if err != nil {
		return wrapWriteErr("insert user session", err)
	}
	us.ID, err = res.LastInsertId()
	return err
}

func (s *sessionStore) GetByHash(ctx context.Context, hash string) (*model.UserSession, error) {
	us := &model.UserSession{}
	var agent, ip sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, token_hash, family_id, user_id, workspace_id, user_agent, ip, used, used_at, revoked, expires_at, created_at
		 FROM user_sessions WHERE token_hash = ?`, hash).
		Scan(&us.ID, &us.TokenHash, &us.FamilyID, &us.UserID, &us.WorkspaceID, &agent, &ip,
			&us.Used, &us.UsedAt, &us.Revoked, &us.ExpiresAt, &us.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user session: %w", err)
	}
	us.UserAgent, us.IP = agent.String, ip.String
	return us, nil
}

// ClaimReuseGrace consumes the ONE recovery a spent token is allowed, and
// reports whether this caller got it.
//
// Atomic and single-use, exactly like MarkUsed and for the same reason. Without
// it the grace is unbounded: eight tabs racing one cookie would each read
// "used, and recently" and each be handed a session. Clearing used_at is what
// spends it, so a ninth attempt finds nothing to claim and falls through to
// reuse detection, where it belongs.
func (s *sessionStore) ClaimReuseGrace(ctx context.Context, id int64, notBefore time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE user_sessions SET used_at = NULL
		 WHERE id = ? AND used = 1 AND revoked = 0 AND used_at IS NOT NULL AND used_at >= ?`,
		id, notBefore)
	if err != nil {
		return false, fmt.Errorf("claim reuse grace: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim reuse grace rows: %w", err)
	}
	return n == 1, nil
}

// RevokeUnusedInFamily kills every token in a family that has not been spent.
//
// It is how a lost rotation is cleaned up: when a client presents a token it
// already spent, the successor we issued is one nobody holds, so it is revoked
// before a fresh one takes its place. Exactly one live token per family, which
// is the property rotation exists to keep.
func (s *sessionStore) RevokeUnusedInFamily(ctx context.Context, familyID string) error {
	if familyID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE user_sessions SET revoked = 1 WHERE family_id = ? AND used = 0 AND revoked = 0`,
		familyID)
	if err != nil {
		return fmt.Errorf("revoke unused sessions in family: %w", err)
	}
	return nil
}

// MarkUsed consumes a refresh session atomically. It flips used to 1 ONLY if it
// was still 0, and reports store.ErrNotFound when the row was already used. This
// is the single-use gate: two concurrent refreshes of the same token both pass
// the earlier used==0 read, but only the one whose UPDATE actually flips the row
// wins; the loser gets ErrNotFound and is refused, so a refresh token can never
// be redeemed into two sessions. A plain `SET used = 1 WHERE id = ?` would be
// idempotent and let both racers through.
func (s *sessionStore) MarkUsed(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE user_sessions SET used = 1, used_at = ? WHERE id = ? AND used = 0`,
		time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("mark session used: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark session used rows: %w", err)
	}
	if n != 1 {
		return store.ErrNotFound
	}
	return nil
}

// ValidateAccess reports whether an access token's session is still good to act
// on: the session exists for this user and is not revoked, the user is active,
// and the user is still a member of the workspace the token speaks for and that
// workspace is active. It is the live re-check that makes a short-lived access
// token actually revocable, so a logout, password change, disable, or lost
// membership takes effect on the next request instead of at token expiry. The
// membership and workspace-status conditions mirror IsMember.
func (s *sessionStore) ValidateAccess(ctx context.Context, sessionID, userID, workspaceID int64) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1
			FROM user_sessions se
			JOIN users u ON u.id = se.user_id
			JOIN workspace_members wm ON wm.user_id = se.user_id
			JOIN workspaces w ON w.id = wm.workspace_id
			WHERE se.id = ? AND se.user_id = ? AND se.revoked = 0
			  AND u.status = 'active'
			  AND wm.workspace_id = ? AND w.status = 'active'
		)`,
		sessionID, userID, workspaceID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("validate access session: %w", err)
	}
	return ok, nil
}

func (s *sessionStore) Revoke(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE user_sessions SET revoked = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// RevokeFamily revokes every live session in a rotation family. It is the
// containment for refresh-token reuse: replaying a spent token severs the whole
// chain, the thief's and the victim's alike. An empty family (a pre-detection
// row) revokes nothing, so a legacy session's reuse just fails without a
// too-broad sweep.
func (s *sessionStore) RevokeFamily(ctx context.Context, familyID string) error {
	if familyID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE user_sessions SET revoked = 1 WHERE family_id = ? AND revoked = 0`, familyID)
	if err != nil {
		return fmt.Errorf("revoke session family: %w", err)
	}
	return nil
}

func (s *sessionStore) RevokeAllForUser(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE user_sessions SET revoked = 1 WHERE user_id = ? AND revoked = 0`, userID)
	if err != nil {
		return fmt.Errorf("revoke user sessions: %w", err)
	}
	return nil
}

func nullString(v string) sql.NullString {
	return sql.NullString{String: v, Valid: v != ""}
}
