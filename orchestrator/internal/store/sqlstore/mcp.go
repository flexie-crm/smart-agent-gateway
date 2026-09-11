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

// The MCP client's connection registry: the third-party tool servers this
// workspace consumes. Secrets arrive sealed and are stored sealed; opening
// them is the app layer's job, never a query's.

type mcpServerStore struct{ db *sqldb.DB }

const mcpColumns = `id, workspace_id, name, url, auth_type,
	api_key_enc, oauth_client_id, oauth_client_secret_enc,
	oauth_access_token_enc, oauth_refresh_token_enc, oauth_token_expires_at, oauth_metadata,
	status, tool_prefix, last_synced_at, last_error, created_at, updated_at`

func (s *mcpServerStore) Create(ctx context.Context, m *model.MCPServer) error {
	now := time.Now().UTC()
	m.CreatedAt, m.UpdatedAt = now, now
	if m.Status == "" {
		m.Status = model.StatusActive
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO mcp_servers
			(workspace_id, name, url, auth_type, api_key_enc,
			 oauth_client_id, oauth_client_secret_enc, status, tool_prefix, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.WorkspaceID, m.Name, m.URL, m.AuthType, nullBytes(m.APIKey),
		m.OAuthClientID, nullBytes(m.OAuthClientSecret), m.Status, m.ToolPrefix,
		m.CreatedAt, m.UpdatedAt)
	if err != nil {
		return wrapWriteErr("insert mcp server", err)
	}
	m.ID, err = res.LastInsertId()
	return err
}

func (s *mcpServerStore) GetByID(ctx context.Context, workspaceID, id int64) (*model.MCPServer, error) {
	return scanMCPServer(s.db.QueryRowContext(ctx,
		`SELECT `+mcpColumns+` FROM mcp_servers WHERE id = ? AND workspace_id = ?`, id, workspaceID))
}

func (s *mcpServerStore) List(ctx context.Context, workspaceID int64) ([]*model.MCPServer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+mcpColumns+` FROM mcp_servers WHERE workspace_id = ? ORDER BY name`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	servers := []*model.MCPServer{}
	for rows.Next() {
		m, err := scanMCPServerFields(rows.Scan)
		if err != nil {
			return nil, err
		}
		servers = append(servers, m)
	}
	return servers, rows.Err()
}

// Update writes the admin-owned columns. A nil SECRET means "leave the stored
// one alone": editing a name must never wipe a key, the same rule vendors
// follow. The client id is not a secret, so it is written plainly and can be
// cleared, which is what lets somebody move a connection from a hand-registered
// client back to one the service issues itself. The tool_prefix is absent on purpose: it is fixed at creation, and
// the grants hanging off the projected tool names depend on that.
func (s *mcpServerStore) Update(ctx context.Context, m *model.MCPServer) error {
	if err := requireExists(ctx, s.db, "update mcp server",
		`SELECT 1 FROM mcp_servers WHERE id = ? AND workspace_id = ?`, m.ID, m.WorkspaceID); err != nil {
		return err
	}
	m.UpdatedAt = time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE mcp_servers SET
			name = ?, url = ?, auth_type = ?, status = ?,
			api_key_enc = COALESCE(?, api_key_enc),
			oauth_client_id = ?,
			oauth_client_secret_enc = COALESCE(?, oauth_client_secret_enc),
			updated_at = ?
		 WHERE id = ? AND workspace_id = ?`,
		m.Name, m.URL, m.AuthType, m.Status,
		nullBytes(m.APIKey), m.OAuthClientID, nullBytes(m.OAuthClientSecret), m.UpdatedAt,
		m.ID, m.WorkspaceID); err != nil {
		return wrapWriteErr("update mcp server", err)
	}
	return nil
}

// Delete removes the connection; the database cascades take its projected
// tools, their grants included.
func (s *mcpServerStore) Delete(ctx context.Context, workspaceID, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM mcp_servers WHERE id = ? AND workspace_id = ?`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("delete mcp server: %w", err)
	}
	return requireAffected(res, "delete mcp server")
}

// SetOAuthClient records what the remote's authorization server issued us at
// registration, plus the discovery documents, so a refresh never re-walks
// discovery.
func (s *mcpServerStore) SetOAuthClient(ctx context.Context, id int64, clientID string, clientSecret []byte, metadata json.RawMessage) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE mcp_servers SET oauth_client_id = ?, oauth_client_secret_enc = ?, oauth_metadata = ?, updated_at = ?
		 WHERE id = ?`,
		clientID, nullBytes(clientSecret), nullJSON(metadata), time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("set oauth client: %w", err)
	}
	return requireAffected(res, "set oauth client")
}

func (s *mcpServerStore) SetOAuthMetadata(ctx context.Context, id int64, metadata json.RawMessage) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE mcp_servers SET oauth_metadata = ?, updated_at = ? WHERE id = ?`,
		nullJSON(metadata), time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("set oauth metadata: %w", err)
	}
	return requireAffected(res, "set oauth metadata")
}

// SetTokens writes a token pair the gateway obtained or refreshed. These are
// gateway-owned columns: no admin edit path touches them.
func (s *mcpServerStore) SetTokens(ctx context.Context, id int64, access, refresh []byte, expires *time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE mcp_servers SET
			oauth_access_token_enc = ?, oauth_refresh_token_enc = ?, oauth_token_expires_at = ?, updated_at = ?
		 WHERE id = ?`,
		nullBytes(access), nullBytes(refresh), expires, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("set tokens: %w", err)
	}
	return requireAffected(res, "set tokens")
}

// SetSyncState records what the last sync did, error included: a connection
// that cannot be reached must say so where an administrator looks.
func (s *mcpServerStore) SetSyncState(ctx context.Context, id int64, syncedAt time.Time, lastError string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE mcp_servers SET last_synced_at = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		syncedAt, lastError, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("set sync state: %w", err)
	}
	return requireAffected(res, "set sync state")
}

func scanMCPServer(row *sql.Row) (*model.MCPServer, error) {
	return scanMCPServerFields(row.Scan)
}

func scanMCPServerFields(scan func(dest ...any) error) (*model.MCPServer, error) {
	m := &model.MCPServer{}
	var apiKey, clientSecret, access, refresh []byte
	var clientID sql.NullString
	var expires, syncedAt sql.NullTime
	var metadata []byte
	var lastError sql.NullString
	err := scan(&m.ID, &m.WorkspaceID, &m.Name, &m.URL, &m.AuthType,
		&apiKey, &clientID, &clientSecret,
		&access, &refresh, &expires, &metadata,
		&m.Status, &m.ToolPrefix, &syncedAt, &lastError, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan mcp server: %w", err)
	}
	m.APIKey, m.OAuthClientSecret = apiKey, clientSecret
	m.OAuthAccessToken, m.OAuthRefreshToken = access, refresh
	m.OAuthClientID = clientID.String
	m.OAuthMetadata = json.RawMessage(metadata)
	m.LastError = lastError.String
	if expires.Valid {
		at := expires.Time
		m.OAuthTokenExpires = &at
	}
	if syncedAt.Valid {
		at := syncedAt.Time
		m.LastSyncedAt = &at
	}
	return m, nil
}

// --- our MCP server's settings -----------------------------------------------------

// GetSettings reads the workspace's MCP server configuration. ErrNotFound is
// a real answer: unconfigured, whole catalog exposed (CRM parity).
func (s *mcpServerStore) GetSettings(ctx context.Context, workspaceID int64) (*model.MCPSettings, error) {
	m := &model.MCPSettings{WorkspaceID: workspaceID}
	var toolConfig, brainConfig []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT tool_config, brain_config, updated_at FROM mcp_settings WHERE workspace_id = ?`,
		workspaceID).Scan(&toolConfig, &brainConfig, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get mcp settings: %w", err)
	}
	m.ToolConfig = json.RawMessage(toolConfig)
	m.BrainConfig = json.RawMessage(brainConfig)
	return m, nil
}

// PutSettings writes the configuration wholesale.
func (s *mcpServerStore) PutSettings(ctx context.Context, m *model.MCPSettings) error {
	m.UpdatedAt = time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO mcp_settings (workspace_id, tool_config, brain_config, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
			tool_config = VALUES(tool_config),
			brain_config = VALUES(brain_config),
			updated_at = VALUES(updated_at)`,
		m.WorkspaceID, string(m.ToolConfig), string(m.BrainConfig), m.UpdatedAt); err != nil {
		return wrapWriteErr("put mcp settings", err)
	}
	return nil
}
