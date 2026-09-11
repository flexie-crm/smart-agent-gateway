package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/store/sqldb"
)

type oauthStore struct{ db *sqldb.DB }

func jsonList(v []string) ([]byte, error) {
	if v == nil {
		v = []string{}
	}
	return json.Marshal(v)
}

func scanJSONList(raw []byte, dest *[]string) error {
	if len(raw) == 0 {
		*dest = []string{}
		return nil
	}
	return json.Unmarshal(raw, dest)
}

// --- Clients ---------------------------------------------------------------

func (s *oauthStore) CreateClient(ctx context.Context, c *model.OAuthClient) error {
	now := time.Now().UTC()
	c.CreatedAt, c.UpdatedAt = now, now
	if c.Status == "" {
		c.Status = model.StatusActive
	}
	redirects, err := jsonList(c.RedirectURIs)
	if err != nil {
		return fmt.Errorf("marshal redirect uris: %w", err)
	}
	grants, err := jsonList(c.GrantTypes)
	if err != nil {
		return fmt.Errorf("marshal grant types: %w", err)
	}
	scopes, err := jsonList(c.Scopes)
	if err != nil {
		return fmt.Errorf("marshal scopes: %w", err)
	}
	var secret sql.NullString
	if c.ClientSecretHash != "" {
		secret = sql.NullString{String: c.ClientSecretHash, Valid: true}
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO oauth_clients
		 (workspace_id, client_id, client_secret_hash, client_type, name,
		  redirect_uris, grant_types, scopes, is_dcr, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.WorkspaceID, c.ClientID, secret, c.ClientType, c.Name,
		redirects, grants, scopes, c.IsDCR, c.Status, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert oauth client: %w", err)
	}
	c.ID, err = res.LastInsertId()
	return err
}

func (s *oauthStore) GetClientByClientID(ctx context.Context, clientID string) (*model.OAuthClient, error) {
	c := &model.OAuthClient{}
	var (
		secret                   sql.NullString
		redirects, grants, scope []byte
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, workspace_id, client_id, client_secret_hash, client_type, name,
		        redirect_uris, grant_types, scopes, is_dcr, status, created_at, updated_at
		 FROM oauth_clients WHERE client_id = ?`, clientID).
		Scan(&c.ID, &c.WorkspaceID, &c.ClientID, &secret, &c.ClientType, &c.Name,
			&redirects, &grants, &scope, &c.IsDCR, &c.Status, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get oauth client: %w", err)
	}
	c.ClientSecretHash = secret.String
	if err := scanJSONList(redirects, &c.RedirectURIs); err != nil {
		return nil, fmt.Errorf("decode redirect uris: %w", err)
	}
	if err := scanJSONList(grants, &c.GrantTypes); err != nil {
		return nil, fmt.Errorf("decode grant types: %w", err)
	}
	if err := scanJSONList(scope, &c.Scopes); err != nil {
		return nil, fmt.Errorf("decode scopes: %w", err)
	}
	return c, nil
}

// scanClientRow reads one client row; the shape lives in one place.
func scanClientRow(scan func(dest ...any) error) (*model.OAuthClient, error) {
	c := &model.OAuthClient{}
	var (
		secret                   sql.NullString
		redirects, grants, scope []byte
	)
	err := scan(&c.ID, &c.WorkspaceID, &c.ClientID, &secret, &c.ClientType, &c.Name,
		&redirects, &grants, &scope, &c.IsDCR, &c.Status, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan oauth client: %w", err)
	}
	c.ClientSecretHash = secret.String
	if err := scanJSONList(redirects, &c.RedirectURIs); err != nil {
		return nil, fmt.Errorf("decode redirect uris: %w", err)
	}
	if err := scanJSONList(grants, &c.GrantTypes); err != nil {
		return nil, fmt.Errorf("decode grant types: %w", err)
	}
	if err := scanJSONList(scope, &c.Scopes); err != nil {
		return nil, fmt.Errorf("decode scopes: %w", err)
	}
	return c, nil
}

// ListClients returns the workspace's clients plus the platform-level ones
// (DCR registrations carry no workspace): both concern the same console.
func (s *oauthStore) ListClients(ctx context.Context, workspaceID int64) ([]*model.OAuthClient, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, workspace_id, client_id, client_secret_hash, client_type, name,
		        redirect_uris, grant_types, scopes, is_dcr, status, created_at, updated_at
		 FROM oauth_clients
		 WHERE workspace_id = ? OR workspace_id IS NULL
		 ORDER BY created_at DESC`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list oauth clients: %w", err)
	}
	defer func() { _ = rows.Close() }()

	clients := []*model.OAuthClient{}
	for rows.Next() {
		c, err := scanClientRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		clients = append(clients, c)
	}
	return clients, rows.Err()
}

// GetClientForWorkspace resolves a client the workspace may manage: its own,
// or a platform-level (DCR) one.
func (s *oauthStore) GetClientForWorkspace(ctx context.Context, workspaceID, id int64) (*model.OAuthClient, error) {
	return scanClientRow(s.db.QueryRowContext(ctx,
		`SELECT id, workspace_id, client_id, client_secret_hash, client_type, name,
		        redirect_uris, grant_types, scopes, is_dcr, status, created_at, updated_at
		 FROM oauth_clients
		 WHERE id = ? AND (workspace_id = ? OR workspace_id IS NULL)`, id, workspaceID).Scan)
}

// UpdateClient writes the admin-owned fields. The client_id is immutable;
// the secret hash is set to exactly what the caller resolved (a type toggle
// clears it or mints a fresh one, and that decision is the API layer's).
func (s *oauthStore) UpdateClient(ctx context.Context, c *model.OAuthClient) error {
	redirects, err := jsonList(c.RedirectURIs)
	if err != nil {
		return fmt.Errorf("marshal redirect uris: %w", err)
	}
	grants, err := jsonList(c.GrantTypes)
	if err != nil {
		return fmt.Errorf("marshal grant types: %w", err)
	}
	scopes, err := jsonList(c.Scopes)
	if err != nil {
		return fmt.Errorf("marshal scopes: %w", err)
	}
	var secret sql.NullString
	if c.ClientSecretHash != "" {
		secret = sql.NullString{String: c.ClientSecretHash, Valid: true}
	}
	c.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE oauth_clients SET
			name = ?, client_type = ?, client_secret_hash = ?, redirect_uris = ?,
			grant_types = ?, scopes = ?, status = ?, updated_at = ?
		 WHERE id = ?`,
		c.Name, c.ClientType, secret, redirects, grants, scopes, c.Status, c.UpdatedAt, c.ID)
	if err != nil {
		return wrapWriteErr("update oauth client", err)
	}
	_ = res
	return nil
}

// DeleteClient removes the client; the database cascades take every token it
// owns, which is the service token's kill switch.
func (s *oauthStore) DeleteClient(ctx context.Context, workspaceID, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM oauth_clients WHERE id = ? AND (workspace_id = ? OR workspace_id IS NULL)`,
		id, workspaceID)
	if err != nil {
		return fmt.Errorf("delete oauth client: %w", err)
	}
	return requireAffected(res, "delete oauth client")
}

// --- Authorization codes -----------------------------------------------------

func (s *oauthStore) InsertAuthCode(ctx context.Context, c *model.OAuthAuthCode) error {
	c.CreatedAt = time.Now().UTC()
	scopes, err := jsonList(c.Scopes)
	if err != nil {
		return fmt.Errorf("marshal scopes: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO oauth_auth_codes
		 (code_hash, client_pk, user_id, workspace_id, redirect_uri, scopes,
		  code_challenge, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CodeHash, c.ClientPK, c.UserID, c.WorkspaceID, c.RedirectURI, scopes,
		c.CodeChallenge, c.ExpiresAt, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert auth code: %w", err)
	}
	c.ID, err = res.LastInsertId()
	return err
}

func (s *oauthStore) ConsumeAuthCode(ctx context.Context, codeHash string) (*model.OAuthAuthCode, error) {
	c := &model.OAuthAuthCode{}

	err := s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		var scopes []byte
		err := tx.QueryRowContext(ctx,
			`SELECT id, code_hash, client_pk, user_id, workspace_id, redirect_uri, scopes,
			        code_challenge, expires_at, created_at
			 FROM oauth_auth_codes WHERE code_hash = ? FOR UPDATE`, codeHash).
			Scan(&c.ID, &c.CodeHash, &c.ClientPK, &c.UserID, &c.WorkspaceID, &c.RedirectURI,
				&scopes, &c.CodeChallenge, &c.ExpiresAt, &c.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("select auth code: %w", err)
		}
		if err := scanJSONList(scopes, &c.Scopes); err != nil {
			return fmt.Errorf("decode scopes: %w", err)
		}

		res, err := tx.ExecContext(ctx, `DELETE FROM oauth_auth_codes WHERE id = ?`, c.ID)
		if err != nil {
			return fmt.Errorf("delete auth code: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return store.ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// --- Access tokens ------------------------------------------------------------

func (s *oauthStore) InsertAccessToken(ctx context.Context, t *model.OAuthAccessToken) error {
	t.CreatedAt = time.Now().UTC()
	scopes, err := jsonList(t.Scopes)
	if err != nil {
		return fmt.Errorf("marshal scopes: %w", err)
	}
	var family sql.NullString
	if t.FamilyID != "" {
		family = sql.NullString{String: t.FamilyID, Valid: true}
	}
	var audience sql.NullString
	if t.Audience != "" {
		audience = sql.NullString{String: t.Audience, Valid: true}
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO oauth_access_tokens
		 (token_hash, client_pk, user_id, workspace_id, scopes, audience, family_id,
		  expires_at, revoked, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.TokenHash, t.ClientPK, t.UserID, t.WorkspaceID, scopes, audience, family,
		t.ExpiresAt, t.Revoked, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert access token: %w", err)
	}
	t.ID, err = res.LastInsertId()
	return err
}

func (s *oauthStore) GetAccessTokenByHash(ctx context.Context, hash string) (*model.OAuthAccessToken, error) {
	t := &model.OAuthAccessToken{}
	var (
		scopes           []byte
		audience, family sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, token_hash, client_pk, user_id, workspace_id, scopes, audience,
		        family_id, expires_at, revoked, created_at
		 FROM oauth_access_tokens WHERE token_hash = ?`, hash).
		Scan(&t.ID, &t.TokenHash, &t.ClientPK, &t.UserID, &t.WorkspaceID, &scopes,
			&audience, &family, &t.ExpiresAt, &t.Revoked, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}
	t.Audience, t.FamilyID = audience.String, family.String
	if err := scanJSONList(scopes, &t.Scopes); err != nil {
		return nil, fmt.Errorf("decode scopes: %w", err)
	}
	return t, nil
}

func (s *oauthStore) RevokeAccessToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE oauth_access_tokens SET revoked = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("revoke access token: %w", err)
	}
	return nil
}

// --- Refresh tokens --------------------------------------------------------------

func (s *oauthStore) InsertRefreshToken(ctx context.Context, t *model.OAuthRefreshToken) error {
	t.CreatedAt = time.Now().UTC()
	scopes, err := jsonList(t.Scopes)
	if err != nil {
		return fmt.Errorf("marshal scopes: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO oauth_refresh_tokens
		 (token_hash, client_pk, user_id, workspace_id, scopes, family_id, parent_id,
		  used, revoked, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.TokenHash, t.ClientPK, t.UserID, t.WorkspaceID, scopes, t.FamilyID, t.ParentID,
		t.Used, t.Revoked, t.ExpiresAt, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert refresh token: %w", err)
	}
	t.ID, err = res.LastInsertId()
	return err
}

func (s *oauthStore) GetRefreshTokenByHash(ctx context.Context, hash string) (*model.OAuthRefreshToken, error) {
	t := &model.OAuthRefreshToken{}
	var scopes []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT id, token_hash, client_pk, user_id, workspace_id, scopes, family_id,
		        parent_id, used, revoked, expires_at, created_at
		 FROM oauth_refresh_tokens WHERE token_hash = ?`, hash).
		Scan(&t.ID, &t.TokenHash, &t.ClientPK, &t.UserID, &t.WorkspaceID, &scopes,
			&t.FamilyID, &t.ParentID, &t.Used, &t.Revoked, &t.ExpiresAt, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get refresh token: %w", err)
	}
	if err := scanJSONList(scopes, &t.Scopes); err != nil {
		return nil, fmt.Errorf("decode scopes: %w", err)
	}
	return t, nil
}

func (s *oauthStore) MarkRefreshTokenUsed(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE oauth_refresh_tokens SET used = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("mark refresh used: %w", err)
	}
	return nil
}

func (s *oauthStore) RevokeFamily(ctx context.Context, familyID string) error {
	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE oauth_refresh_tokens SET revoked = 1 WHERE family_id = ?`, familyID); err != nil {
			return fmt.Errorf("revoke refresh family: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE oauth_access_tokens SET revoked = 1 WHERE family_id = ?`, familyID); err != nil {
			return fmt.Errorf("revoke access family: %w", err)
		}
		return nil
	})
}

// --- Consents ---------------------------------------------------------------------

func (s *oauthStore) GetConsent(ctx context.Context, userID, clientPK int64) (*model.OAuthConsent, error) {
	c := &model.OAuthConsent{}
	var scopes []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, client_pk, scopes, created_at, updated_at
		 FROM oauth_consents WHERE user_id = ? AND client_pk = ?`, userID, clientPK).
		Scan(&c.ID, &c.UserID, &c.ClientPK, &scopes, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get consent: %w", err)
	}
	if err := scanJSONList(scopes, &c.Scopes); err != nil {
		return nil, fmt.Errorf("decode scopes: %w", err)
	}
	return c, nil
}

func (s *oauthStore) UpsertConsent(ctx context.Context, c *model.OAuthConsent) error {
	now := time.Now().UTC()
	scopes, err := jsonList(c.Scopes)
	if err != nil {
		return fmt.Errorf("marshal scopes: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO oauth_consents (user_id, client_pk, scopes, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE scopes = VALUES(scopes), updated_at = VALUES(updated_at)`,
		c.UserID, c.ClientPK, scopes, now, now)
	if err != nil {
		return fmt.Errorf("upsert consent: %w", err)
	}
	return nil
}
