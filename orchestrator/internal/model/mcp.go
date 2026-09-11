package model

import (
	"encoding/json"
	"time"
)

// MCPServer is a connection to a third-party tool server: SAG as the client,
// the inverse of the MCP surface we expose. Its remote tools are projected
// into the tools table (kind 'mcp'), where the existing governance applies:
// status, approval, grants, and the agents' allow-lists.
type MCPServer struct {
	ID          int64
	WorkspaceID int64
	Name        string
	URL         string

	// AuthType picks how the gateway authenticates: nothing, a static key
	// sent as a bearer header, or OAuth with tokens the gateway keeps fresh.
	AuthType string

	// The secrets are sealed (envelope encryption, like vendor credentials)
	// and never leave the server: the API reports only whether one is stored.
	APIKey            []byte
	OAuthClientID     string
	OAuthClientSecret []byte
	OAuthAccessToken  []byte
	OAuthRefreshToken []byte
	OAuthTokenExpires *time.Time
	// OAuthMetadata caches the remote's discovery documents (authorization
	// server metadata and our registered client), so every refresh does not
	// re-walk discovery.
	OAuthMetadata json.RawMessage

	Status string

	// ToolPrefix is the immutable namespace this connection's tools live
	// under (<prefix>.<remote name>). Fixed at creation, like a vendor's
	// key: renaming the connection must not orphan the grants and approval
	// decisions hanging off its tool names.
	ToolPrefix string

	LastSyncedAt *time.Time
	LastError    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// HasAPIKey reports whether a key is stored, without revealing it.
func (s *MCPServer) HasAPIKey() bool { return len(s.APIKey) > 0 }

// Connected reports whether the OAuth dance has completed: tokens are held.
func (s *MCPServer) Connected() bool { return len(s.OAuthAccessToken) > 0 }

const (
	MCPAuthNone   = "none"
	MCPAuthAPIKey = "api_key"
	MCPAuthOAuth  = "oauth"
)

// KnownMCPAuthTypes is what the API validates against.
var KnownMCPAuthTypes = []string{MCPAuthNone, MCPAuthAPIKey, MCPAuthOAuth}

// MCPSyncResult says what a sync did, so the console can show a diff instead
// of a silently mutated registry.
type MCPSyncResult struct {
	Offered int `json:"offered"`
	Added   int `json:"added"`
	Changed int `json:"changed"`
	Missing int `json:"missing"`
	// Skipped counts remote tools whose names cannot be projected (too long
	// for the registry even after prefixing). Silent truncation would read
	// as "covered everything", so it is counted and logged.
	Skipped int `json:"skipped"`
}

// MCPSettings is our MCP server's configuration for one workspace (CRM
// parity: the mcpToolConfig / mcpBrainConfig pair). The row's ABSENCE means
// unconfigured, and unconfigured exposes the whole catalog, the CRM's own
// default; an admin who saves once takes explicit control.
type MCPSettings struct {
	WorkspaceID int64 `json:"-"`
	// ToolConfig is an object map {"<tool name>": {"enabled": bool}}.
	ToolConfig json.RawMessage `json:"tool_config"`
	// BrainConfig is an array of brain ids the brain tool may reach for MCP
	// callers. Stored now; consumed when the brains agent-tool lands.
	BrainConfig json.RawMessage `json:"brain_config"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// MCPToolEnabled reads one tool's switch from a ToolConfig map.
type MCPToolSwitch struct {
	Enabled bool `json:"enabled"`
}
