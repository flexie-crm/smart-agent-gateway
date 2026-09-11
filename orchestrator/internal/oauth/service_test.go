package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// fakeOAuthStore is an in-memory store.OAuthStore for exercising the
// service logic without a database.
type fakeOAuthStore struct {
	mu       sync.Mutex
	nextID   int64
	clients  map[string]*model.OAuthClient
	codes    map[string]*model.OAuthAuthCode
	access   map[string]*model.OAuthAccessToken
	refresh  map[string]*model.OAuthRefreshToken
	consents map[[2]int64]*model.OAuthConsent
}

func newFakeStore() *fakeOAuthStore {
	return &fakeOAuthStore{
		clients:  map[string]*model.OAuthClient{},
		codes:    map[string]*model.OAuthAuthCode{},
		access:   map[string]*model.OAuthAccessToken{},
		refresh:  map[string]*model.OAuthRefreshToken{},
		consents: map[[2]int64]*model.OAuthConsent{},
	}
}

func (f *fakeOAuthStore) id() int64 { f.nextID++; return f.nextID }

func (f *fakeOAuthStore) CreateClient(_ context.Context, c *model.OAuthClient) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c.ID = f.id()
	if c.Status == "" {
		c.Status = model.StatusActive
	}
	f.clients[c.ClientID] = c
	return nil
}

func (f *fakeOAuthStore) GetClientByClientID(_ context.Context, clientID string) (*model.OAuthClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.clients[clientID]; ok {
		return c, nil
	}
	return nil, store.ErrNotFound
}

// The admin CRUD, minimally: the service tests only need the interface met.
func (f *fakeOAuthStore) ListClients(_ context.Context, _ int64) ([]*model.OAuthClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*model.OAuthClient{}
	for _, c := range f.clients {
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeOAuthStore) GetClientForWorkspace(_ context.Context, _ int64, id int64) (*model.OAuthClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.clients {
		if c.ID == id {
			return c, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeOAuthStore) UpdateClient(_ context.Context, c *model.OAuthClient) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clients[c.ClientID] = c
	return nil
}

func (f *fakeOAuthStore) DeleteClient(_ context.Context, _ int64, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for clientID, c := range f.clients {
		if c.ID == id {
			delete(f.clients, clientID)
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeOAuthStore) InsertAuthCode(_ context.Context, c *model.OAuthAuthCode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c.ID = f.id()
	f.codes[c.CodeHash] = c
	return nil
}

func (f *fakeOAuthStore) ConsumeAuthCode(_ context.Context, hash string) (*model.OAuthAuthCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.codes[hash]
	if !ok {
		return nil, store.ErrNotFound
	}
	delete(f.codes, hash)
	return c, nil
}

func (f *fakeOAuthStore) InsertAccessToken(_ context.Context, t *model.OAuthAccessToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t.ID = f.id()
	f.access[t.TokenHash] = t
	return nil
}

func (f *fakeOAuthStore) GetAccessTokenByHash(_ context.Context, hash string) (*model.OAuthAccessToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.access[hash]; ok {
		return t, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeOAuthStore) RevokeAccessToken(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.access {
		if t.ID == id {
			t.Revoked = true
		}
	}
	return nil
}

func (f *fakeOAuthStore) InsertRefreshToken(_ context.Context, t *model.OAuthRefreshToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t.ID = f.id()
	f.refresh[t.TokenHash] = t
	return nil
}

func (f *fakeOAuthStore) GetRefreshTokenByHash(_ context.Context, hash string) (*model.OAuthRefreshToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.refresh[hash]; ok {
		return t, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeOAuthStore) MarkRefreshTokenUsed(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.refresh {
		if t.ID == id {
			t.Used = true
		}
	}
	return nil
}

func (f *fakeOAuthStore) RevokeFamily(_ context.Context, familyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.refresh {
		if t.FamilyID == familyID {
			t.Revoked = true
		}
	}
	for _, t := range f.access {
		if t.FamilyID == familyID {
			t.Revoked = true
		}
	}
	return nil
}

func (f *fakeOAuthStore) GetConsent(_ context.Context, userID, clientPK int64) (*model.OAuthConsent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.consents[[2]int64{userID, clientPK}]; ok {
		return c, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeOAuthStore) UpsertConsent(_ context.Context, c *model.OAuthConsent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consents[[2]int64{c.UserID, c.ClientPK}] = c
	return nil
}

// --- helpers ----------------------------------------------------------------

const testVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk-verifier"

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func registerTestClient(t *testing.T, svc *Service) *DCRResponse {
	t.Helper()
	resp, err := svc.RegisterClient(context.Background(), DCRRequest{
		ClientName:   "Test MCP Client",
		RedirectURIs: []string{"https://client.example/callback"},
	})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}
	return resp
}

// runCodeFlow walks authorize → code → exchange and returns the tokens.
func runCodeFlow(t *testing.T, svc *Service, clientID string) *TokenResponse {
	t.Helper()
	ctx := context.Background()
	params := AuthorizeParams{
		ClientID:            clientID,
		RedirectURI:         "https://client.example/callback",
		ResponseType:        "code",
		Scope:               "mcp",
		CodeChallenge:       challengeFor(testVerifier),
		CodeChallengeMethod: "S256",
	}
	req, fatal, redirect := svc.ValidateAuthorize(ctx, params)
	if fatal != nil || redirect != nil {
		t.Fatalf("authorize failed: fatal=%v redirect=%v", fatal, redirect)
	}
	code, err := svc.IssueAuthCode(ctx, req, params, 7, 3)
	if err != nil {
		t.Fatalf("issue code: %v", err)
	}
	tokens, err := svc.Token(ctx, TokenParams{
		GrantType:    "authorization_code",
		Code:         code,
		RedirectURI:  params.RedirectURI,
		CodeVerifier: testVerifier,
		ClientID:     clientID,
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	return tokens
}

// --- tests ---------------------------------------------------------------------

func TestPKCEVerify(t *testing.T) {
	if !verifyPKCES256(testVerifier, challengeFor(testVerifier)) {
		t.Fatal("valid verifier rejected")
	}
	if verifyPKCES256(testVerifier+"x", challengeFor(testVerifier)) {
		t.Fatal("wrong verifier accepted")
	}
	if verifyPKCES256("short", challengeFor("short")) {
		t.Fatal("verifier below RFC 7636 minimum length accepted")
	}
}

func TestCodeFlowAndReplay(t *testing.T) {
	svc := NewService(newFakeStore(), "https://sag.example")
	client := registerTestClient(t, svc)
	ctx := context.Background()

	params := AuthorizeParams{
		ClientID:            client.ClientID,
		RedirectURI:         "https://client.example/callback",
		ResponseType:        "code",
		Scope:               "mcp",
		CodeChallenge:       challengeFor(testVerifier),
		CodeChallengeMethod: "S256",
	}
	req, fatal, redirect := svc.ValidateAuthorize(ctx, params)
	if fatal != nil || redirect != nil {
		t.Fatalf("authorize: fatal=%v redirect=%v", fatal, redirect)
	}
	code, err := svc.IssueAuthCode(ctx, req, params, 7, 3)
	if err != nil {
		t.Fatalf("issue code: %v", err)
	}

	exchange := TokenParams{
		GrantType: "authorization_code", Code: code,
		RedirectURI: params.RedirectURI, CodeVerifier: testVerifier,
		ClientID: client.ClientID,
	}
	tokens, err := svc.Token(ctx, exchange)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		t.Fatal("expected access + refresh tokens")
	}
	if at, err := svc.ValidateAccessToken(ctx, tokens.AccessToken); err != nil || at.UserID != 7 || at.WorkspaceID != 3 {
		t.Fatalf("access token invalid after mint: %v", err)
	}

	// Replaying the same code must fail, single use.
	if _, err := svc.Token(ctx, exchange); err == nil {
		t.Fatal("code replay accepted")
	}
}

func TestExchangeRejectsWrongVerifier(t *testing.T) {
	svc := NewService(newFakeStore(), "https://sag.example")
	client := registerTestClient(t, svc)
	ctx := context.Background()

	params := AuthorizeParams{
		ClientID: client.ClientID, RedirectURI: "https://client.example/callback",
		ResponseType: "code", Scope: "mcp",
		CodeChallenge: challengeFor(testVerifier), CodeChallengeMethod: "S256",
	}
	req, _, _ := svc.ValidateAuthorize(ctx, params)
	code, err := svc.IssueAuthCode(ctx, req, params, 7, 3)
	if err != nil {
		t.Fatalf("issue code: %v", err)
	}
	_, err = svc.Token(ctx, TokenParams{
		GrantType: "authorization_code", Code: code,
		RedirectURI:  params.RedirectURI,
		CodeVerifier: "wrong-verifier-wrong-verifier-wrong-verifier-wrong",
		ClientID:     client.ClientID,
	})
	var oe *Error
	if !errors.As(err, &oe) || oe.Code != "invalid_grant" {
		t.Fatalf("expected invalid_grant, got %v", err)
	}
}

func TestRefreshRotationAndReuseDetection(t *testing.T) {
	fake := newFakeStore()
	svc := NewService(fake, "https://sag.example")
	client := registerTestClient(t, svc)
	ctx := context.Background()

	first := runCodeFlow(t, svc, client.ClientID)

	// Rotate once, must succeed and mint a new pair.
	second, err := svc.Token(ctx, TokenParams{
		GrantType: "refresh_token", RefreshToken: first.RefreshToken,
		ClientID: client.ClientID,
	})
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh token was not rotated")
	}

	// Replaying the consumed token = theft → whole family revoked.
	if _, err := svc.Token(ctx, TokenParams{
		GrantType: "refresh_token", RefreshToken: first.RefreshToken,
		ClientID: client.ClientID,
	}); err == nil {
		t.Fatal("reused refresh token accepted")
	}
	if _, err := svc.Token(ctx, TokenParams{
		GrantType: "refresh_token", RefreshToken: second.RefreshToken,
		ClientID: client.ClientID,
	}); err == nil {
		t.Fatal("family member survived reuse detection")
	}
	if _, err := svc.ValidateAccessToken(ctx, second.AccessToken); err == nil {
		t.Fatal("family access token survived reuse detection")
	}
}

func TestRefreshScopeNeverWidens(t *testing.T) {
	svc := NewService(newFakeStore(), "https://sag.example")
	client := registerTestClient(t, svc)
	first := runCodeFlow(t, svc, client.ClientID)

	_, err := svc.Token(context.Background(), TokenParams{
		GrantType: "refresh_token", RefreshToken: first.RefreshToken,
		Scope: "mcp admin", ClientID: client.ClientID,
	})
	var oe *Error
	if !errors.As(err, &oe) || oe.Code != "invalid_scope" {
		t.Fatalf("expected invalid_scope, got %v", err)
	}
}

func TestAuthorizeRejectsUnknownRedirectFatally(t *testing.T) {
	svc := NewService(newFakeStore(), "https://sag.example")
	client := registerTestClient(t, svc)

	_, fatal, redirect := svc.ValidateAuthorize(context.Background(), AuthorizeParams{
		ClientID:            client.ClientID,
		RedirectURI:         "https://evil.example/callback",
		ResponseType:        "code",
		CodeChallenge:       challengeFor(testVerifier),
		CodeChallengeMethod: "S256",
	})
	if fatal == nil {
		t.Fatal("unknown redirect_uri must be a fatal error (never redirected)")
	}
	if redirect != nil {
		t.Fatal("unknown redirect_uri must not produce a redirectable error")
	}
}

func TestDCRRejectsInsecureRedirect(t *testing.T) {
	svc := NewService(newFakeStore(), "https://sag.example")
	_, err := svc.RegisterClient(context.Background(), DCRRequest{
		ClientName:   "Bad",
		RedirectURIs: []string{"http://attacker.example/cb"},
	})
	if err == nil {
		t.Fatal("non-loopback http redirect accepted")
	}
}
