package apitool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
)

// authServer is an authorization server that counts how often it is asked, so
// the caching claims can be checked at the only place they are visible.
type authServer struct {
	*httptest.Server
	asked  atomic.Int64
	basic  atomic.Value // the Authorization header it saw
	form   atomic.Value // the body it saw
	expiry int64
	issue  atomic.Int64 // which token it is up to, so a refetch is visible
}

func newAuthServer(t *testing.T, expiresIn int64) *authServer {
	t.Helper()
	a := &authServer{expiry: expiresIn}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.asked.Add(1)
		a.basic.Store(r.Header.Get("Authorization"))
		buf := make([]byte, 2048)
		n, _ := r.Body.Read(buf)
		a.form.Store(string(buf[:n]))
		n64 := a.issue.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"granted-%d","token_type":"Bearer","expires_in":%d}`, n64, a.expiry)
	}))
	t.Cleanup(a.Close)
	return a
}

func granted(t *testing.T, tokenURL, credentials string) Settings {
	t.Helper()
	cfg, err := New(nil).(apiTemplate).Config(AuthClientCredentials, map[string]any{
		"base_url":           "https://replaced.example.com",
		"auth.token_url":     tokenURL,
		"auth.client_id":     "client-abc",
		"auth.client_secret": "shhh-do-not-leak",
		"auth.scope":         "invoices.read",
		"auth.credentials":   credentials,
		"policy.mode":        Denylist,
		"policy.verbs":       "",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	s, err := Parse(cfg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return s
}

// A token is fetched, attached as a bearer, and the client's own credentials go
// where the administrator said.
func TestAGrantedTokenIsFetchedAndAttached(t *testing.T) {
	auth := newAuthServer(t, 3600)
	api, seen := serve(t, 200, "application/json", `{"ok":true}`)

	s := granted(t, auth.URL, CredentialsInHeader)
	s.BaseURL = api.URL

	res, err := Handler(s, nil, newTokens(nil), nil, nil)(context.Background(),
		tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/things"}`)})
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	if res.Failed() {
		t.Fatalf("the call failed: %s", res.Content)
	}
	if auth.asked.Load() != 1 {
		t.Fatalf("the authorization server was asked %d times", auth.asked.Load())
	}
	if seen.auth != "Bearer granted-1" {
		t.Fatalf("the API saw %q", seen.auth)
	}
	// The client's credentials in the header, and the scope asked for.
	if got, _ := auth.basic.Load().(string); !strings.HasPrefix(got, "Basic ") {
		t.Fatalf("the client credentials were not in the header: %q", got)
	}
	if body, _ := auth.form.Load().(string); !strings.Contains(body, "grant_type=client_credentials") ||
		!strings.Contains(body, "scope=invoices.read") {
		t.Fatalf("the token request was wrong: %q", body)
	}
	// And the secret is in none of what comes back.
	if strings.Contains(string(res.Content), "shhh-do-not-leak") {
		t.Fatalf("the client secret reached the model: %s", res.Content)
	}
}

// The other placement, because services differ and getting it wrong is a 401
// with no explanation.
func TestTheClientCredentialsCanGoInTheBody(t *testing.T) {
	auth := newAuthServer(t, 3600)
	api, _ := serve(t, 200, "application/json", `{}`)

	s := granted(t, auth.URL, CredentialsInBody)
	s.BaseURL = api.URL

	if _, err := Handler(s, nil, newTokens(nil), nil, nil)(context.Background(),
		tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/x"}`)}); err != nil {
		t.Fatalf("errored: %v", err)
	}
	body, _ := auth.form.Load().(string)
	if !strings.Contains(body, "client_id=client-abc") || !strings.Contains(body, "client_secret=shhh-do-not-leak") {
		t.Fatalf("the credentials did not go in the body: %q", body)
	}
	if got, _ := auth.basic.Load().(string); got != "" {
		t.Fatalf("they were ALSO sent in the header: %q", got)
	}
}

// A token is held, so a conversation of twenty calls is one token request.
func TestATokenIsHeldRatherThanFetchedEveryCall(t *testing.T) {
	auth := newAuthServer(t, 3600)
	api, _ := serve(t, 200, "application/json", `{}`)
	s := granted(t, auth.URL, CredentialsInHeader)
	s.BaseURL = api.URL

	handle := Handler(s, nil, newTokens(nil), nil, nil)
	for i := 0; i < 5; i++ {
		if _, err := handle(context.Background(),
			tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/x"}`)}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := auth.asked.Load(); got != 1 {
		t.Fatalf("five calls made %d token requests", got)
	}
}

// And replaced before it expires, rather than after a call has failed.
//
// The server issues a token that is already inside the replacement window, so
// the second call must fetch again: a token that expires in flight is a failure
// somebody has to understand.
func TestATokenIsReplacedBeforeItExpires(t *testing.T) {
	auth := newAuthServer(t, 10) // ten seconds, well inside the one-minute margin
	api, seen := serve(t, 200, "application/json", `{}`)
	s := granted(t, auth.URL, CredentialsInHeader)
	s.BaseURL = api.URL

	handle := Handler(s, nil, newTokens(nil), nil, nil)
	for i := 0; i < 2; i++ {
		if _, err := handle(context.Background(),
			tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/x"}`)}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := auth.asked.Load(); got != 2 {
		t.Fatalf("a token expiring in 10s was reused: %d token requests", got)
	}
	if seen.auth != "Bearer granted-2" {
		t.Fatalf("the second call used %q, not the fresh token", seen.auth)
	}
	// The control for the window: a long-lived token is NOT refetched, so the
	// test above is about expiry and not about the cache being broken.
	long := newAuthServer(t, 3600)
	s2 := granted(t, long.URL, CredentialsInHeader)
	s2.BaseURL = api.URL
	handle2 := Handler(s2, nil, newTokens(nil), nil, nil)
	for i := 0; i < 2; i++ {
		_, _ = handle2(context.Background(), tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/x"}`)})
	}
	if got := long.asked.Load(); got != 1 {
		t.Fatalf("a one-hour token was refetched: %d token requests", got)
	}
}

// Twenty calls at once make ONE token request.
//
// A fleet of agents starting together is the real case, and without a lock per
// entry each would see an empty cache and ask, which is twenty requests an
// authorization server may well rate-limit.
func TestConcurrentCallsMakeOneTokenRequest(t *testing.T) {
	auth := newAuthServer(t, 3600)
	api, _ := serve(t, 200, "application/json", `{}`)
	s := granted(t, auth.URL, CredentialsInHeader)
	s.BaseURL = api.URL

	handle := Handler(s, nil, newTokens(nil), nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = handle(context.Background(), tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/x"}`)})
		}()
	}
	wg.Wait()
	if got := auth.asked.Load(); got != 1 {
		t.Fatalf("twenty concurrent calls made %d token requests", got)
	}
}

// An authorization server that refuses says so in ITS OWN words, and the
// secret is not in the message.
func TestARefusedGrantIsReportedInTheServersOwnWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_scope","error_description":"this client may not have invoices.read"}`))
	}))
	t.Cleanup(srv.Close)

	s := granted(t, srv.URL, CredentialsInHeader)
	s.BaseURL = "https://unused.example.com"

	res, err := Handler(s, nil, newTokens(nil), nil, nil)(context.Background(),
		tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/x"}`)})
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	if !res.Failed() {
		t.Fatal("a refused grant was not reported")
	}
	if !strings.Contains(string(res.Content), "may not have invoices.read") {
		t.Fatalf("the server's own reason was dropped: %s", res.Content)
	}
	if strings.Contains(string(res.Content), "shhh-do-not-leak") {
		t.Fatalf("the client secret is in the failure: %s", res.Content)
	}
}

// A token type this tool cannot send is refused rather than half-supported.
func TestATokenTypeWeCannotSendIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"x","token_type":"DPoP","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)
	s := granted(t, srv.URL, CredentialsInHeader)

	_, _, err := fetch(context.Background(), &http.Client{}, s.Auth)
	if err == nil {
		t.Fatal("a DPoP token was accepted, which this tool cannot send")
	}
	if !strings.Contains(err.Error(), "bearer") {
		t.Fatalf("the refusal does not say what we send: %v", err)
	}
}

// Two tools with the same credentials share one token; different scopes do not.
func TestTheCacheKeyIsTheTokenRequestsOwnIdentity(t *testing.T) {
	base := Auth{Kind: AuthClientCredentials, TokenURL: "https://t.test/token", ClientID: "a", ClientSecret: "s", Scope: "read"}
	same := base
	if keyOf(base, "") != keyOf(same, "") {
		t.Error("the same credentials did not share a key")
	}
	for _, different := range []Auth{
		{Kind: AuthClientCredentials, TokenURL: "https://t.test/token", ClientID: "a", ClientSecret: "s", Scope: "write"},
		{Kind: AuthClientCredentials, TokenURL: "https://t.test/token", ClientID: "b", ClientSecret: "s", Scope: "read"},
		// A ROTATED secret must not hand back the token the old one bought.
		{Kind: AuthClientCredentials, TokenURL: "https://t.test/token", ClientID: "a", ClientSecret: "rotated", Scope: "read"},
		{Kind: AuthClientCredentials, TokenURL: "https://other.test/token", ClientID: "a", ClientSecret: "s", Scope: "read"},
	} {
		if keyOf(base, "") == keyOf(different, "") {
			t.Errorf("a different token request shared the key: %+v", different)
		}
	}
	// And the secret is not IN the key, which could reach a log.
	if strings.Contains(keyOf(base, ""), "s") && len(keyOf(base, "")) < 20 {
		t.Error("the key looks like it carries the secret")
	}
}

// The grant's secret is sealed, and its form and seal list agree.
func TestTheGrantsSecretIsSealed(t *testing.T) {
	tpl := New(nil)
	sealed := tpl.SecretPaths(AuthClientCredentials)
	var found bool
	for _, path := range sealed {
		if path == "auth.client_secret" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the client secret is not sealed: %v", sealed)
	}
	// Offered on the form, so an administrator can choose it at all.
	found = false
	for _, v := range tpl.Variants() {
		if v.Key == AuthClientCredentials {
			found = true
		}
	}
	if !found {
		t.Fatal("the grant is not offered as a variant, so nobody can pick it")
	}
	if _, err := tpl.Fields(AuthClientCredentials); err != nil {
		t.Fatalf("it has no form: %v", err)
	}
}

// A configuration that could not work is refused before it is saved.
func TestABrokenGrantIsRefusedAtSaveTime(t *testing.T) {
	for _, c := range []struct {
		name     string
		settings map[string]any
	}{
		{"no token address", map[string]any{"base_url": "https://api.test", "auth.client_id": "a", "auth.client_secret": "s", "policy.mode": Denylist}},
		{"no client id", map[string]any{"base_url": "https://api.test", "auth.token_url": "https://t.test/token", "auth.client_secret": "s", "policy.mode": Denylist}},
		{"no secret", map[string]any{"base_url": "https://api.test", "auth.token_url": "https://t.test/token", "auth.client_id": "a", "policy.mode": Denylist}},
		{"a token address that is not one", map[string]any{"base_url": "https://api.test", "auth.token_url": "not a url", "auth.client_id": "a", "auth.client_secret": "s", "policy.mode": Denylist}},
		{"credentials sent nowhere sensible", map[string]any{"base_url": "https://api.test", "auth.token_url": "https://t.test/token", "auth.client_id": "a", "auth.client_secret": "s", "auth.credentials": "cookie", "policy.mode": Denylist}},
	} {
		if _, err := New(nil).Build(template.Input{Alias: "x", Variant: AuthClientCredentials, Settings: c.settings}); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
	// The control: correct is accepted.
	if _, err := New(nil).Build(template.Input{
		Alias: "x", Variant: AuthClientCredentials,
		Settings: map[string]any{
			"base_url": "https://api.test", "auth.token_url": "https://t.test/token",
			"auth.client_id": "a", "auth.client_secret": "s", "policy.mode": Denylist,
		},
	}); err != nil {
		t.Fatalf("a correct grant was refused: %v", err)
	}
}

// A token that has been rotated or revoked is replaced on the spot.
//
// Without this, an API that starts answering 401 keeps being asked with the
// same dead token until it expires, which for a one-hour token is an hour of
// failure over something a single request fixes. Bounded to 401 because that
// means the request was REFUSED rather than performed, so there is nothing to
// do twice, which is what makes it safe on a POST.
func TestARevokedTokenIsReplacedRatherThanFailingForAnHour(t *testing.T) {
	auth := newAuthServer(t, 3600)

	// The API refuses the first token and accepts the second, which is what a
	// rotation looks like from here.
	var seenTokens []string
	var mu sync.Mutex
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		seenTokens = append(seenTokens, token)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if token == "granted-1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"token revoked"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(api.Close)

	s := granted(t, auth.URL, CredentialsInHeader)
	s.BaseURL = api.URL

	res, err := Handler(s, nil, newTokens(nil), nil, nil)(context.Background(),
		tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/things"}`)})
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	var out map[string]any
	_ = json.Unmarshal(res.Content, &out)
	if out["status"] != float64(200) {
		t.Fatalf("the retry did not recover: status %#v, body %s", out["status"], res.Content)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seenTokens) != 2 || seenTokens[0] != "granted-1" || seenTokens[1] != "granted-2" {
		t.Fatalf("the API saw %v, want the dead token then a fresh one", seenTokens)
	}
	if auth.asked.Load() != 2 {
		t.Fatalf("the authorization server was asked %d times", auth.asked.Load())
	}
}

// And it does NOT loop: a 401 that survives a fresh token is the answer.
//
// The dangerous shape here is retrying for ever against an API that always
// refuses, so this asserts exactly two attempts and no more.
func TestAPersistent401IsAnsweredRatherThanRetriedForEver(t *testing.T) {
	auth := newAuthServer(t, 3600)
	var attempts atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	t.Cleanup(api.Close)

	s := granted(t, auth.URL, CredentialsInHeader)
	s.BaseURL = api.URL

	res, err := Handler(s, nil, newTokens(nil), nil, nil)(context.Background(),
		tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/things"}`)})
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	if res.Failed() {
		t.Fatalf("a 401 was reported as a broken tool: %s", res.Content)
	}
	var out map[string]any
	_ = json.Unmarshal(res.Content, &out)
	if out["status"] != float64(401) {
		t.Fatalf("the 401 did not reach the model: %#v", out["status"])
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("the API was called %d times, want exactly two", got)
	}
}

// A body survives the retry.
//
// The request is replayed, so this is the assertion that a POST is not quietly
// turned into an empty one. It passes because of GetBody in net/http rather
// than because of anything here (measured: removing an explicit rewind changed
// nothing), which is exactly why the assertion is worth keeping: it holds the
// behaviour whether the stdlib or we are the reason for it.
func TestABodySurvivesTheRetry(t *testing.T) {
	auth := newAuthServer(t, 3600)
	var bodies []string
	var mu sync.Mutex
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 2048)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		bodies = append(bodies, string(buf[:n]))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.Header.Get("Authorization"), "granted-1") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(api.Close)

	s := granted(t, auth.URL, CredentialsInHeader)
	s.BaseURL = api.URL
	s.Policy = Policy{Mode: Allowlist, Verbs: []string{"POST"}}

	if _, err := Handler(s, nil, newTokens(nil), nil, nil)(context.Background(),
		tool.Call{Args: json.RawMessage(`{"method":"POST","path":"/things","body":{"name":"kept"}}`)}); err != nil {
		t.Fatalf("errored: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("the API saw %d bodies", len(bodies))
	}
	for i, body := range bodies {
		if !strings.Contains(body, `"name":"kept"`) {
			t.Fatalf("attempt %d sent %q, so the body was lost on the replay", i+1, body)
		}
	}
}

// When a server refuses in a shape we do not recognise, hand over what it
// actually said.
//
// "the authorization server answered 400" is where a diagnosis stops. The
// reason is almost always in the body, and the body was discarded exactly when
// it was the only thing left: an OAuth error in another shape, an HTML login
// page because the address is not a token endpoint, a proxy's own words.
func TestAnUnrecognisedRefusalHandsOverWhatTheServerSaid(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{
			name:   "an HTML page, which means the address is not a token endpoint",
			status: 400,
			body:   "<html>\n  <body>\n    <h1>Sign in to continue</h1>\n  </body>\n</html>",
			want:   "<html> <body> <h1>Sign in to continue</h1> </body> </html>",
		},
		{
			name:   "an error in some other shape",
			status: 400,
			body:   `{"message":"grant type not enabled for this application"}`,
			want:   `{"message":"grant type not enabled for this application"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			_, _, err := fetch(context.Background(), server.Client(), Auth{
				Kind: AuthClientCredentials, TokenURL: server.URL,
				ClientID: "an-app", ClientSecret: "a-secret",
			})
			if err == nil {
				t.Fatal("a refusal was read as a token")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the server's own words are missing:\n  got:  %v\n  want: %s", err, tc.want)
			}
		})
	}

	// An empty body says what that usually means, since there is nothing to quote.
	quiet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer quiet.Close()
	_, _, err := fetch(context.Background(), quiet.Client(), Auth{
		Kind: AuthClientCredentials, TokenURL: quiet.URL, ClientID: "a", ClientSecret: "b",
	})
	if err == nil || !strings.Contains(err.Error(), "not a token endpoint") {
		t.Fatalf("an empty refusal says nothing useful: %v", err)
	}

	// And the tool's own credentials never travel in a body we quote back.
	leaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad client: a-very-secret-value"))
	}))
	defer leaky.Close()
	_, _, err = fetch(context.Background(), leaky.Client(), Auth{
		Kind: AuthClientCredentials, TokenURL: leaky.URL,
		ClientID: "an-app", ClientSecret: "a-very-secret-value",
	})
	if err == nil {
		t.Fatal("a refusal was read as a token")
	}
	if strings.Contains(err.Error(), "a-very-secret-value") {
		t.Fatalf("the client secret was quoted back out of the server's body: %v", err)
	}
}
