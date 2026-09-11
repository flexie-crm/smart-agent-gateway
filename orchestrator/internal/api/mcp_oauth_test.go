package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"flexie.io/sag/internal/model"
)

// The OAuth half of the MCP client, end to end against a fake authorization
// server: discovery, dynamic registration, the PKCE code exchange through our
// own callback, and the silent refresh when the token ages out. The fake
// verifies what a real server would: the registered client, the code, and
// that the PKCE verifier matches the challenge from the authorize URL.

type fakeAuthServer struct {
	ts     *httptest.Server
	remote *fakeRemote

	mu            sync.Mutex
	clientID      string
	clientSecret  string
	registrations int
	issuedCodes   map[string]bool
	accessToken   string
	refreshToken  string
	refreshes     int
	// expiresIn controls how long issued tokens claim to live; short values
	// force the gateway's refresh path.
	expiresIn int64
	// expectedChallenge is set by the test from the parsed authorize URL, so
	// the token endpoint can verify the PKCE verifier like a real server.
	expectedChallenge string
	// denyReason, when set, makes the token endpoint refuse a code exchange
	// with this human-readable reason, the way a server refuses a user who is
	// authenticated but not entitled.
	denyReason string
}

func newFakeAuthServer(t *testing.T, remote *fakeRemote) *fakeAuthServer {
	t.Helper()
	f := &fakeAuthServer{
		remote:       remote,
		issuedCodes:  map[string]bool{"valid-code": true},
		accessToken:  "token-1",
		refreshToken: "refresh-1",
		expiresIn:    3600,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 f.ts.URL,
			"authorization_endpoint": f.ts.URL + "/authorize",
			// Without this a client must refuse to proceed: it is the only way
			// PKCE support can be established (MCP authorization, Authorization
			// Code Protection).
			"code_challenge_methods_supported": []string{"S256"},
			"token_endpoint":                   f.ts.URL + "/token",
			"registration_endpoint":            f.ts.URL + "/register",
			"scopes_supported":                 []string{"mcp"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.registrations++
		f.clientID = "issued-client"
		// A fresh secret per registration, so a reconnect that re-registers can
		// be told apart from one that reused a stale credential.
		f.clientSecret = fmt.Sprintf("issued-secret-%d", f.registrations)
		secret := f.clientSecret
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id": "issued-client", "client_secret": secret, "scope": "mcp",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) { f.token(t, w, r) })
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)

	// The resource server honours whatever token the AS most recently issued,
	// and its public metadata points discovery at this AS.
	remote.mu.Lock()
	remote.key = f.accessToken
	remote.authIssuer = f.ts.URL
	remote.mu.Unlock()
	return f
}

func (f *fakeAuthServer) token(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	refuse := func(reason string) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": reason})
	}
	// Like the CRM: a confidential client authenticates with HTTP Basic,
	// and the form-body variant is refused.
	basicID, basicSecret, hasBasic := r.BasicAuth()
	if !hasBasic || basicID != f.clientID || basicSecret != f.clientSecret {
		refuse("Invalid client credentials.")
		return
	}
	if r.PostFormValue("client_id") != f.clientID {
		refuse("unknown client")
		return
	}

	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		if f.denyReason != "" {
			refuse(f.denyReason)
			return
		}
		if !f.issuedCodes[r.PostFormValue("code")] {
			refuse("unknown code")
			return
		}
		delete(f.issuedCodes, r.PostFormValue("code"))
		// PKCE: the verifier must hash to the challenge the authorize URL
		// carried. The test stored it on the fake from the parsed URL.
		challenge := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(challenge[:]) != f.expectedChallenge {
			refuse("verifier does not match challenge")
			return
		}
	case "refresh_token":
		if r.PostFormValue("refresh_token") != f.refreshToken {
			refuse("unknown refresh token")
			return
		}
		f.refreshes++
		f.accessToken = "token-2"
		f.refreshToken = "refresh-2"
	default:
		refuse("unsupported grant")
		return
	}

	// The resource server follows the newest token.
	f.remote.mu.Lock()
	f.remote.key = f.accessToken
	f.remote.mu.Unlock()

	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  f.accessToken,
		"refresh_token": f.refreshToken,
		"expires_in":    f.expiresIn,
		"token_type":    "Bearer",
	})
}

func TestMCPOAuthConnectAndRefresh(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	remote := newFakeRemote(t, "placeholder-until-issued")
	remote.offer(map[string]string{"echo": "echoes"})
	auth := newFakeAuthServer(t, remote)

	// Issued tokens age out immediately, so the second errand must refresh.
	auth.mu.Lock()
	auth.expiresIn = 30
	auth.mu.Unlock()

	rec := env.do(http.MethodPost, "/v1/mcp-servers", token, map[string]any{
		"name": "Remote", "url": remote.ts.URL, "auth_type": model.MCPAuthOAuth,
	})
	env.expectStatus(rec, http.StatusCreated)
	var connection mcpServerBody
	env.decode(rec, &connection)

	// Begin: discovery + registration happen, and the authorize URL carries
	// PKCE and our sealed state.
	rec = env.do(http.MethodPost, "/v1/mcp-servers/"+itoa(connection.ID)+"/connect", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var begun connectResponse
	env.decode(rec, &begun)
	authorize, err := url.Parse(begun.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize url: %v", err)
	}
	q := authorize.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Fatalf("the authorize url is not a PKCE request: %s", begun.AuthorizeURL)
	}
	// The scope registration granted travels on the authorize request: real
	// servers (the CRM's among them) refuse a request that names none.
	if q.Get("scope") != "mcp" {
		t.Fatalf("the authorize url does not carry the granted scope: %s", begun.AuthorizeURL)
	}
	auth.mu.Lock()
	auth.expectedChallenge = q.Get("code_challenge")
	auth.mu.Unlock()

	// The person consents; the AS sends them back to our callback. The
	// exchange runs, the tokens are sealed, and the FIRST SYNC happens: the
	// dance ends with tools on the table.
	rec = env.do(http.MethodGet,
		"/connect/mcp/callback?state="+url.QueryEscape(q.Get("state"))+"&code=valid-code", "", nil)
	env.expectStatus(rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "Connected to Remote") {
		t.Fatalf("the callback page does not say what happened: %s", rec.Body.String())
	}
	if tools := env.connectionTools(token, connection.ID); len(tools) != 1 {
		t.Fatalf("the first sync did not project the tools: %d", len(tools))
	}

	// A tampered state is nobody's connection.
	rec = env.do(http.MethodGet, "/connect/mcp/callback?state=forged&code=valid-code", "", nil)
	env.expectStatus(rec, http.StatusBadRequest)

	// Every token in this test is born nearly expired (expires_in=30 sits
	// under the freshness bar), so each errand against the remote refreshes
	// exactly once and then works: one for the first sync inside the
	// callback, one for this manual sync. A number above that would mean a
	// refresh storm; below it, a stale token being trusted.
	result := env.syncConnection(token, connection.ID)
	if result.Offered != 1 {
		t.Fatalf("the refreshed sync did not reach the remote: %+v", result)
	}
	auth.mu.Lock()
	refreshes := auth.refreshes
	auth.mu.Unlock()
	if refreshes != 2 {
		t.Fatalf("expected one refresh per errand (2), got %d", refreshes)
	}
}

// Starting the connection again (the failure page invites exactly this) must
// recover on its own when the server offers dynamic registration: it registers
// a fresh client rather than trust a stored secret that may be gone or wrong.
// The regression: BeginMCPConnect reused the existing client and rewrote its
// credentials with an empty secret, so a second attempt could never complete.
func TestMCPOAuthReconnectReRegisters(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	remote := newFakeRemote(t, "placeholder-until-issued")
	remote.offer(map[string]string{"echo": "echoes"})
	auth := newFakeAuthServer(t, remote)

	rec := env.do(http.MethodPost, "/v1/mcp-servers", token, map[string]any{
		"name": "Remote", "url": remote.ts.URL, "auth_type": model.MCPAuthOAuth,
	})
	env.expectStatus(rec, http.StatusCreated)
	var connection mcpServerBody
	env.decode(rec, &connection)

	// First connect: registration issues the confidential client and its secret.
	env.expectStatus(env.do(http.MethodPost,
		"/v1/mcp-servers/"+itoa(connection.ID)+"/connect", token, nil), http.StatusOK)

	// Reconnect: because the server offers registration, this mints a fresh
	// client. The authorize URL from this attempt carries the PKCE verifier the
	// callback will complete against.
	rec = env.do(http.MethodPost, "/v1/mcp-servers/"+itoa(connection.ID)+"/connect", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var begun connectResponse
	env.decode(rec, &begun)

	auth.mu.Lock()
	registrations := auth.registrations
	auth.mu.Unlock()
	if registrations != 2 {
		t.Fatalf("a reconnect must register a fresh client, saw %d registrations", registrations)
	}

	authorize, err := url.Parse(begun.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize url: %v", err)
	}
	q := authorize.Query()
	auth.mu.Lock()
	auth.expectedChallenge = q.Get("code_challenge")
	auth.mu.Unlock()

	// The exchange authenticates with the secret from the reconnect's fresh
	// registration. The old code reused a client whose secret it had erased, so
	// the token endpoint refused and the callback reported failure.
	rec = env.do(http.MethodGet,
		"/connect/mcp/callback?state="+url.QueryEscape(q.Get("state"))+"&code=valid-code", "", nil)
	env.expectStatus(rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "Connected to Remote") {
		t.Fatalf("the reconnect did not complete: %s", rec.Body.String())
	}
}

// When the remote refuses the token exchange with a reason (authenticated, but
// not entitled), the callback page shows that reason. The generic "start again"
// would send the person back to retry something no retry can fix.
func TestMCPOAuthCallbackShowsRemoteReason(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	remote := newFakeRemote(t, "placeholder-until-issued")
	remote.offer(map[string]string{"echo": "echoes"})
	auth := newFakeAuthServer(t, remote)

	rec := env.do(http.MethodPost, "/v1/mcp-servers", token, map[string]any{
		"name": "Remote", "url": remote.ts.URL, "auth_type": model.MCPAuthOAuth,
	})
	env.expectStatus(rec, http.StatusCreated)
	var connection mcpServerBody
	env.decode(rec, &connection)

	rec = env.do(http.MethodPost, "/v1/mcp-servers/"+itoa(connection.ID)+"/connect", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var begun connectResponse
	env.decode(rec, &begun)
	authorize, err := url.Parse(begun.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize url: %v", err)
	}
	q := authorize.Query()

	const reason = "The user is not eligible for MCP access."
	auth.mu.Lock()
	auth.expectedChallenge = q.Get("code_challenge")
	auth.denyReason = reason
	auth.mu.Unlock()

	rec = env.do(http.MethodGet,
		"/connect/mcp/callback?state="+url.QueryEscape(q.Get("state"))+"&code=valid-code", "", nil)
	env.expectStatus(rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), reason) {
		t.Fatalf("the callback hid the remote's reason: %s", rec.Body.String())
	}
}
