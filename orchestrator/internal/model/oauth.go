package model

import "time"

// OAuth client types. A service client is inert at every AS endpoint; it
// only owns a long-lived user-bound token minted at creation (CRM pattern,
// implemented in a later phase).
const (
	OAuthClientPublic       = "public"
	OAuthClientConfidential = "confidential"
	OAuthClientService      = "service"
)

// Known scopes. "mcp" is the only scope issued today; "api" is reserved for
// the future REST API cutover (same staging as the CRM).
const (
	ScopeMCP = "mcp"
)

type OAuthClient struct {
	ID               int64
	WorkspaceID      *int64 // nil = platform-level (DCR-registered)
	ClientID         string
	ClientSecretHash string // bcrypt; empty for public clients
	ClientType       string
	Name             string
	RedirectURIs     []string
	GrantTypes       []string
	Scopes           []string
	IsDCR            bool
	Status           string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type OAuthAuthCode struct {
	ID            int64
	CodeHash      string
	ClientPK      int64
	UserID        int64
	WorkspaceID   int64
	RedirectURI   string
	Scopes        []string
	CodeChallenge string
	ExpiresAt     time.Time
	CreatedAt     time.Time
}

type OAuthAccessToken struct {
	ID          int64
	TokenHash   string
	ClientPK    int64
	UserID      int64
	WorkspaceID int64
	Scopes      []string
	Audience    string
	FamilyID    string // empty when not minted through a refresh family
	ExpiresAt   time.Time
	Revoked     bool
	CreatedAt   time.Time
}

type OAuthRefreshToken struct {
	ID          int64
	TokenHash   string
	ClientPK    int64
	UserID      int64
	WorkspaceID int64
	Scopes      []string
	FamilyID    string
	ParentID    *int64
	Used        bool
	Revoked     bool
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

type OAuthConsent struct {
	ID        int64
	UserID    int64
	ClientPK  int64
	Scopes    []string
	CreatedAt time.Time
	UpdatedAt time.Time
}
