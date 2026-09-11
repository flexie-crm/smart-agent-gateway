package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// TTLs mirror the CRM defaults (oauthAccessTokenTtl 60 min, refresh 30 d,
// code 60 s); they become workspace settings in a later phase.
const (
	AccessTokenTTL  = time.Hour
	RefreshTokenTTL = 30 * 24 * time.Hour
	AuthCodeTTL     = 60 * time.Second
)

// KnownScopes, "mcp" only, exactly like the CRM today; "api" is reserved
// for the REST API cutover.
var KnownScopes = []string{model.ScopeMCP}

var supportedGrantTypes = []string{"authorization_code", "refresh_token"}

type Service struct {
	store  store.OAuthStore
	issuer string // external base URL
}

func NewService(st store.OAuthStore, issuer string) *Service {
	return &Service{store: st, issuer: strings.TrimRight(issuer, "/")}
}

func (s *Service) Issuer() string { return s.issuer }

// --- Dynamic Client Registration (RFC 7591) --------------------------------

type DCRRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	Scope                   string   `json:"scope"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

type DCRResponse struct {
	ClientID                string   `json:"client_id"`
	ClientSecret            string   `json:"client_secret,omitempty"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	Scope                   string   `json:"scope"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// RegisterClient self-onboards an MCP client (Claude, Cursor, ...) with no
// admin step. It registers identity only, whether a user may authorize it
// is decided at the authorize endpoint, per user, per request.
func (s *Service) RegisterClient(ctx context.Context, req DCRRequest) (*DCRResponse, error) {
	if req.ClientName == "" {
		return nil, protoErr(http.StatusBadRequest, "invalid_client_metadata", "client_name is required")
	}
	if len(req.RedirectURIs) == 0 {
		return nil, protoErr(http.StatusBadRequest, "invalid_redirect_uri", "at least one redirect_uri is required")
	}
	for _, u := range req.RedirectURIs {
		if !validRedirectURI(u) {
			return nil, protoErr(http.StatusBadRequest, "invalid_redirect_uri",
				"redirect_uris must be https or loopback http")
		}
	}

	grants := req.GrantTypes
	if len(grants) == 0 {
		grants = []string{"authorization_code", "refresh_token"}
	}
	for _, g := range grants {
		if !slices.Contains(supportedGrantTypes, g) {
			return nil, protoErr(http.StatusBadRequest, "invalid_client_metadata",
				fmt.Sprintf("unsupported grant_type %q", g))
		}
	}

	scopes := splitScope(req.Scope)
	if len(scopes) == 0 {
		scopes = []string{model.ScopeMCP}
	}
	if !scopesSubset(scopes, KnownScopes) {
		return nil, protoErr(http.StatusBadRequest, "invalid_client_metadata", "unknown scope requested")
	}

	authMethod := req.TokenEndpointAuthMethod
	if authMethod == "" {
		authMethod = "none"
	}

	clientID, err := newSecret(prefixClientID)
	if err != nil {
		return nil, fmt.Errorf("mint client id: %w", err)
	}

	client := &model.OAuthClient{
		ClientID:     clientID,
		ClientType:   model.OAuthClientPublic,
		Name:         req.ClientName,
		RedirectURIs: req.RedirectURIs,
		GrantTypes:   grants,
		Scopes:       scopes,
		IsDCR:        true,
	}

	var clientSecret string
	switch authMethod {
	case "none":
		// public client, PKCE-only
	case "client_secret_basic", "client_secret_post":
		clientSecret, err = newSecret(prefixClientSecret)
		if err != nil {
			return nil, fmt.Errorf("mint client secret: %w", err)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(clientSecret), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("hash client secret: %w", err)
		}
		client.ClientType = model.OAuthClientConfidential
		client.ClientSecretHash = string(hash)
	default:
		return nil, protoErr(http.StatusBadRequest, "invalid_client_metadata",
			"unsupported token_endpoint_auth_method")
	}

	if err := s.store.CreateClient(ctx, client); err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}
	return &DCRResponse{
		ClientID:                client.ClientID,
		ClientSecret:            clientSecret, // shown exactly once
		ClientIDIssuedAt:        client.CreatedAt.Unix(),
		ClientName:              client.Name,
		RedirectURIs:            client.RedirectURIs,
		GrantTypes:              client.GrantTypes,
		Scope:                   joinScope(client.Scopes),
		TokenEndpointAuthMethod: authMethod,
	}, nil
}

// --- Authorize --------------------------------------------------------------

type AuthorizeParams struct {
	ClientID            string
	RedirectURI         string
	ResponseType        string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// AuthorizeRequest is a validated authorize request. FatalErr means the
// client/redirect pair could not be trusted, the handler must render an
// error page and NEVER redirect. RedirectErr is safe to send back to the
// client's redirect_uri as error parameters.
type AuthorizeRequest struct {
	Client *model.OAuthClient
	Scopes []string
}

// ValidateAuthorize enforces every invariant before any user interaction:
// client active, exact redirect_uri match, response_type=code, PKCE S256
// present, scope subset of the client's grant.
func (s *Service) ValidateAuthorize(ctx context.Context, p AuthorizeParams) (*AuthorizeRequest, *Error, *Error) {
	client, err := s.store.GetClientByClientID(ctx, p.ClientID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, protoErr(http.StatusBadRequest, "invalid_request", "unknown client"), nil
	}
	if err != nil {
		return nil, protoErr(http.StatusInternalServerError, "server_error", "temporary failure"), nil
	}
	if client.Status != model.StatusActive || client.ClientType == model.OAuthClientService {
		return nil, protoErr(http.StatusBadRequest, "invalid_request", "client is not authorized"), nil
	}
	if !slices.Contains(client.RedirectURIs, p.RedirectURI) {
		// Exact match only, and an unknown redirect_uri is fatal:
		// redirecting to it would be an open redirect.
		return nil, protoErr(http.StatusBadRequest, "invalid_request", "redirect_uri mismatch"), nil
	}

	// From here errors are safe to deliver via redirect.
	if p.ResponseType != "code" {
		return nil, nil, protoErr(http.StatusFound, "unsupported_response_type", "only code is supported")
	}
	if !slices.Contains(client.GrantTypes, "authorization_code") {
		return nil, nil, protoErr(http.StatusFound, "unauthorized_client", "client cannot use authorization_code")
	}
	if p.CodeChallengeMethod != "S256" || p.CodeChallenge == "" {
		return nil, nil, protoErr(http.StatusFound, "invalid_request", "PKCE with S256 is required")
	}
	scopes := splitScope(p.Scope)
	if len(scopes) == 0 {
		scopes = client.Scopes
	}
	if !scopesSubset(scopes, client.Scopes) {
		return nil, nil, protoErr(http.StatusFound, "invalid_scope", "scope exceeds client grant")
	}
	return &AuthorizeRequest{Client: client, Scopes: scopes}, nil, nil
}

// HasConsent reports whether a remembered consent already covers the
// requested scopes, letting the handler skip the consent screen.
func (s *Service) HasConsent(ctx context.Context, userID int64, clientPK int64, scopes []string) bool {
	consent, err := s.store.GetConsent(ctx, userID, clientPK)
	if err != nil {
		return false
	}
	return scopesSubset(scopes, consent.Scopes)
}

func (s *Service) RememberConsent(ctx context.Context, userID, clientPK int64, scopes []string) error {
	return s.store.UpsertConsent(ctx, &model.OAuthConsent{UserID: userID, ClientPK: clientPK, Scopes: scopes})
}

// IssueAuthCode mints a single-use code bound to client + redirect + PKCE +
// user + scope (every binding is re-verified at exchange).
func (s *Service) IssueAuthCode(ctx context.Context, req *AuthorizeRequest, p AuthorizeParams, userID, workspaceID int64) (string, error) {
	code, err := newSecret(prefixAuthCode)
	if err != nil {
		return "", fmt.Errorf("mint code: %w", err)
	}
	err = s.store.InsertAuthCode(ctx, &model.OAuthAuthCode{
		CodeHash:      hashToken(code),
		ClientPK:      req.Client.ID,
		UserID:        userID,
		WorkspaceID:   workspaceID,
		RedirectURI:   p.RedirectURI,
		Scopes:        req.Scopes,
		CodeChallenge: p.CodeChallenge,
		ExpiresAt:     time.Now().UTC().Add(AuthCodeTTL),
	})
	if err != nil {
		return "", fmt.Errorf("store code: %w", err)
	}
	return code, nil
}

// --- Token endpoint ------------------------------------------------------------

type TokenParams struct {
	GrantType    string
	Code         string
	RedirectURI  string
	CodeVerifier string
	RefreshToken string
	Scope        string
	ClientID     string
	ClientSecret string
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope"`
}

func (s *Service) Token(ctx context.Context, p TokenParams) (*TokenResponse, error) {
	client, err := s.authenticateClient(ctx, p.ClientID, p.ClientSecret)
	if err != nil {
		return nil, err
	}
	switch p.GrantType {
	case "authorization_code":
		return s.exchangeCode(ctx, client, p)
	case "refresh_token":
		return s.refresh(ctx, client, p)
	default:
		// client_credentials deliberately unsupported: SAG tokens are
		// always user-bound (CRM invariant); machine access is the
		// service-token path, minted by an admin, in a later phase.
		return nil, protoErr(http.StatusBadRequest, "unsupported_grant_type", "unsupported grant_type")
	}
}

func (s *Service) authenticateClient(ctx context.Context, clientID, clientSecret string) (*model.OAuthClient, error) {
	if clientID == "" {
		return nil, protoErr(http.StatusUnauthorized, "invalid_client", "client authentication failed")
	}
	client, err := s.store.GetClientByClientID(ctx, clientID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, protoErr(http.StatusUnauthorized, "invalid_client", "client authentication failed")
	}
	if err != nil {
		return nil, fmt.Errorf("load client: %w", err)
	}
	if client.Status != model.StatusActive || client.ClientType == model.OAuthClientService {
		return nil, protoErr(http.StatusUnauthorized, "invalid_client", "client authentication failed")
	}
	switch client.ClientType {
	case model.OAuthClientConfidential:
		if bcrypt.CompareHashAndPassword([]byte(client.ClientSecretHash), []byte(clientSecret)) != nil {
			return nil, protoErr(http.StatusUnauthorized, "invalid_client", "client authentication failed")
		}
	case model.OAuthClientPublic:
		if clientSecret != "" {
			return nil, protoErr(http.StatusUnauthorized, "invalid_client", "public clients have no secret")
		}
	}
	return client, nil
}

func (s *Service) exchangeCode(ctx context.Context, client *model.OAuthClient, p TokenParams) (*TokenResponse, error) {
	if !slices.Contains(client.GrantTypes, "authorization_code") {
		return nil, protoErr(http.StatusBadRequest, "unauthorized_client", "grant not allowed for client")
	}
	if p.Code == "" || p.CodeVerifier == "" {
		return nil, protoErr(http.StatusBadRequest, "invalid_request", "code and code_verifier are required")
	}
	code, err := s.store.ConsumeAuthCode(ctx, hashToken(p.Code))
	if errors.Is(err, store.ErrNotFound) {
		return nil, protoErr(http.StatusBadRequest, "invalid_grant", "invalid or expired code")
	}
	if err != nil {
		return nil, fmt.Errorf("consume code: %w", err)
	}
	// The code is destroyed at this point regardless of the outcome,
	// single use even on failure paths.
	if time.Now().UTC().After(code.ExpiresAt) ||
		code.ClientPK != client.ID ||
		code.RedirectURI != p.RedirectURI ||
		!verifyPKCES256(p.CodeVerifier, code.CodeChallenge) {
		return nil, protoErr(http.StatusBadRequest, "invalid_grant", "invalid or expired code")
	}
	return s.mintTokenPair(ctx, client, code.UserID, code.WorkspaceID, code.Scopes)
}

func (s *Service) refresh(ctx context.Context, client *model.OAuthClient, p TokenParams) (*TokenResponse, error) {
	if !slices.Contains(client.GrantTypes, "refresh_token") {
		return nil, protoErr(http.StatusBadRequest, "unauthorized_client", "grant not allowed for client")
	}
	if p.RefreshToken == "" {
		return nil, protoErr(http.StatusBadRequest, "invalid_request", "refresh_token is required")
	}
	rt, err := s.store.GetRefreshTokenByHash(ctx, hashToken(p.RefreshToken))
	if errors.Is(err, store.ErrNotFound) {
		return nil, protoErr(http.StatusBadRequest, "invalid_grant", "invalid refresh token")
	}
	if err != nil {
		return nil, fmt.Errorf("load refresh token: %w", err)
	}
	if rt.ClientPK != client.ID || rt.Revoked || time.Now().UTC().After(rt.ExpiresAt) {
		return nil, protoErr(http.StatusBadRequest, "invalid_grant", "invalid refresh token")
	}
	if rt.Used {
		// Replay of a consumed token, the theft signature. Kill the
		// whole family and every access token it minted.
		if err := s.store.RevokeFamily(ctx, rt.FamilyID); err != nil {
			return nil, fmt.Errorf("revoke family: %w", err)
		}
		return nil, protoErr(http.StatusBadRequest, "invalid_grant", "invalid refresh token")
	}

	scopes := rt.Scopes
	if requested := splitScope(p.Scope); len(requested) > 0 {
		// Scopes may narrow on refresh, never widen.
		if !scopesSubset(requested, rt.Scopes) {
			return nil, protoErr(http.StatusBadRequest, "invalid_scope", "scope exceeds original grant")
		}
		scopes = requested
	}

	if err := s.store.MarkRefreshTokenUsed(ctx, rt.ID); err != nil {
		return nil, fmt.Errorf("mark used: %w", err)
	}
	return s.mintTokenPairInFamily(ctx, client, rt.UserID, rt.WorkspaceID, scopes, rt.FamilyID, &rt.ID)
}

func (s *Service) mintTokenPair(ctx context.Context, client *model.OAuthClient, userID, workspaceID int64, scopes []string) (*TokenResponse, error) {
	family, err := newFamilyID()
	if err != nil {
		return nil, err
	}
	return s.mintTokenPairInFamily(ctx, client, userID, workspaceID, scopes, family, nil)
}

func (s *Service) mintTokenPairInFamily(ctx context.Context, client *model.OAuthClient, userID, workspaceID int64, scopes []string, familyID string, parentID *int64) (*TokenResponse, error) {
	accessToken, err := newSecret(prefixAccessToken)
	if err != nil {
		return nil, err
	}
	audience := ""
	if slices.Contains(scopes, model.ScopeMCP) {
		audience = model.ScopeMCP
	}
	now := time.Now().UTC()
	if err := s.store.InsertAccessToken(ctx, &model.OAuthAccessToken{
		TokenHash:   hashToken(accessToken),
		ClientPK:    client.ID,
		UserID:      userID,
		WorkspaceID: workspaceID,
		Scopes:      scopes,
		Audience:    audience,
		FamilyID:    familyID,
		ExpiresAt:   now.Add(AccessTokenTTL),
	}); err != nil {
		return nil, fmt.Errorf("store access token: %w", err)
	}

	resp := &TokenResponse{
		AccessToken: accessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int64(AccessTokenTTL.Seconds()),
		Scope:       joinScope(scopes),
	}

	if slices.Contains(client.GrantTypes, "refresh_token") {
		refreshToken, err := newSecret(prefixRefreshToken)
		if err != nil {
			return nil, err
		}
		if err := s.store.InsertRefreshToken(ctx, &model.OAuthRefreshToken{
			TokenHash:   hashToken(refreshToken),
			ClientPK:    client.ID,
			UserID:      userID,
			WorkspaceID: workspaceID,
			Scopes:      scopes,
			FamilyID:    familyID,
			ParentID:    parentID,
			ExpiresAt:   now.Add(RefreshTokenTTL),
		}); err != nil {
			return nil, fmt.Errorf("store refresh token: %w", err)
		}
		resp.RefreshToken = refreshToken
	}
	return resp, nil
}

// --- Revocation (RFC 7009) & introspection (RFC 7662) --------------------------

// Revoke invalidates a token presented by its owning client. Per RFC 7009
// the endpoint answers 200 even for unknown tokens, only failed client
// authentication is an error.
func (s *Service) Revoke(ctx context.Context, clientID, clientSecret, token string) error {
	client, err := s.authenticateClient(ctx, clientID, clientSecret)
	if err != nil {
		return err
	}
	hash := hashToken(token)

	if at, err := s.store.GetAccessTokenByHash(ctx, hash); err == nil {
		if at.ClientPK == client.ID {
			return s.store.RevokeAccessToken(ctx, at.ID)
		}
		return nil
	}
	if rt, err := s.store.GetRefreshTokenByHash(ctx, hash); err == nil {
		if rt.ClientPK == client.ID {
			// Revoking a refresh token kills its whole family, incl.
			// access tokens it minted (RFC 7009 SHOULD).
			return s.store.RevokeFamily(ctx, rt.FamilyID)
		}
	}
	return nil
}

type Introspection struct {
	Active      bool   `json:"active"`
	Scope       string `json:"scope,omitempty"`
	ClientID    string `json:"client_id,omitempty"`
	UserID      int64  `json:"user_id,omitempty"`
	WorkspaceID int64  `json:"workspace_id,omitempty"`
	Audience    string `json:"aud,omitempty"`
	ExpiresAt   int64  `json:"exp,omitempty"`
}

// Introspect implements RFC 7662, confidential clients only (CRM rule).
func (s *Service) Introspect(ctx context.Context, clientID, clientSecret, token string) (*Introspection, error) {
	client, err := s.authenticateClient(ctx, clientID, clientSecret)
	if err != nil {
		return nil, err
	}
	if client.ClientType != model.OAuthClientConfidential {
		return nil, protoErr(http.StatusUnauthorized, "invalid_client", "introspection requires a confidential client")
	}
	at, err := s.store.GetAccessTokenByHash(ctx, hashToken(token))
	if err != nil || at.Revoked || time.Now().UTC().After(at.ExpiresAt) {
		return &Introspection{Active: false}, nil
	}
	return &Introspection{
		Active:      true,
		Scope:       joinScope(at.Scopes),
		UserID:      at.UserID,
		WorkspaceID: at.WorkspaceID,
		Audience:    at.Audience,
		ExpiresAt:   at.ExpiresAt.Unix(),
	}, nil
}

// ValidateAccessToken is the resource-server entry point (MCP, future REST
// API). Scope possession is necessary, never sufficient, callers must run
// their live permission check after this.
func (s *Service) ValidateAccessToken(ctx context.Context, rawToken string) (*model.OAuthAccessToken, error) {
	if !strings.HasPrefix(rawToken, prefixAccessToken) {
		return nil, protoErr(http.StatusUnauthorized, "invalid_token", "unrecognized token")
	}
	at, err := s.store.GetAccessTokenByHash(ctx, hashToken(rawToken))
	if errors.Is(err, store.ErrNotFound) {
		return nil, protoErr(http.StatusUnauthorized, "invalid_token", "unknown token")
	}
	if err != nil {
		return nil, fmt.Errorf("load access token: %w", err)
	}
	if at.Revoked || time.Now().UTC().After(at.ExpiresAt) {
		return nil, protoErr(http.StatusUnauthorized, "invalid_token", "token expired or revoked")
	}
	return at, nil
}

// --- helpers -----------------------------------------------------------------------

func splitScope(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Fields(s)
}

func joinScope(scopes []string) string { return strings.Join(scopes, " ") }

func scopesSubset(requested, granted []string) bool {
	for _, r := range requested {
		if !slices.Contains(granted, r) {
			return false
		}
	}
	return true
}

// validRedirectURI: https anywhere, or http on loopback only (native apps).
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		host := u.Hostname()
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	default:
		return false
	}
}

// --- admin-created clients and service tokens (CRM ClientController parity) ------

// ServiceTokenTTL is deliberately a far-future point in time, not forever:
// every check compares a real expiry, so nothing needs a special "never"
// case (CRM parity: 99 years).
const ServiceTokenTTL = 99 * 365 * 24 * time.Hour

// NewClientID mints a public client identifier.
func NewClientID() (string, error) { return newSecret(prefixClientID) }

// NewClientSecret mints a client secret and its bcrypt hash. The plaintext
// is shown once and never stored.
func NewClientSecret() (secret, hash string, err error) {
	secret, err = newSecret(prefixClientSecret)
	if err != nil {
		return "", "", fmt.Errorf("mint client secret: %w", err)
	}
	raw, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", "", fmt.Errorf("hash client secret: %w", err)
	}
	return secret, string(raw), nil
}

// HashToken exposes the persisted form of a token to the resource side: a
// bearer arriving at the MCP surface is looked up by this hash and nothing
// else.
func HashToken(token string) string { return hashToken(token) }

// IssueServiceToken mints the one long-lived token a service client owns:
// user-bound machine access, the third client type. It acts as the person
// who created it and carries only the mcp scope; there is no refresh token,
// and rolling it means deleting the client and creating a new one. Every
// kill switch stays live: revoke the token, disable the client, disable the
// user, or take their permission away.
func (s *Service) IssueServiceToken(ctx context.Context, client *model.OAuthClient, userID, workspaceID int64) (string, error) {
	token, err := newSecret(prefixAccessToken)
	if err != nil {
		return "", fmt.Errorf("mint service token: %w", err)
	}
	if err := s.store.InsertAccessToken(ctx, &model.OAuthAccessToken{
		TokenHash:   hashToken(token),
		ClientPK:    client.ID,
		UserID:      userID,
		WorkspaceID: workspaceID,
		Scopes:      []string{model.ScopeMCP},
		Audience:    model.ScopeMCP,
		ExpiresAt:   time.Now().UTC().Add(ServiceTokenTTL),
	}); err != nil {
		return "", fmt.Errorf("store service token: %w", err)
	}
	return token, nil
}
