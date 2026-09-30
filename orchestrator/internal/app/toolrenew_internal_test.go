package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tools/apitool"
	"flexie.io/sag/internal/tools/template"
)

// A person's sign-in is renewed once, from the stored refresh token, however
// many calls or bindings of the tool ask at the same moment, against a server
// that treats a refresh token presented twice as theft.

// rotatingServer rotates refresh tokens: each one works once, and presenting a
// spent one is treated as theft, so the whole family is revoked.
type rotatingServer struct {
	// takes is how long a token request takes, which holds the window open
	// in which two renewals can overlap.
	takes     time.Duration
	mu        sync.Mutex
	current   string
	spent     map[string]bool
	revoked   bool
	issued    int
	presented []string
}

func (s *rotatingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	time.Sleep(s.takes)
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	rt := r.PostForm.Get("refresh_token")
	s.presented = append(s.presented, rt)
	if s.revoked || s.spent[rt] {
		s.revoked = true
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Refresh token reuse detected — all tokens in the family have been revoked."}`))
		return
	}
	if rt != s.current {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"unknown refresh token"}`))
		return
	}
	s.spent[rt] = true
	s.issued++
	s.current = fmt.Sprintf("rt-%d", s.issued+1)
	_, _ = fmt.Fprintf(w, `{"access_token":"at-%d","refresh_token":%q,"token_type":"Bearer","expires_in":3600}`, s.issued+1, s.current)
}

func (s *rotatingServer) saw() ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.presented...), s.revoked
}

// oneTool is the store as far as renewing a sign-in reaches into it: read the
// tool, write its config. Anything else it touched would be a nil panic, which
// is the point: nothing here is quietly stubbed away.
type oneTool struct {
	store.Store
	tools *oneToolRow
}

func (s *oneTool) Tools() store.ToolStore { return s.tools }

type oneToolRow struct {
	store.ToolStore
	mu  sync.Mutex
	row model.Tool
}

func (m *oneToolRow) GetByID(context.Context, int64, int64) (*model.Tool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.row
	return &row, nil
}

func (m *oneToolRow) SetConfig(_ context.Context, _, _ int64, config json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.row.Config = append(json.RawMessage(nil), config...)
	return nil
}

// signedInWithAnExpiredToken is a connected tool whose access token has run
// out, holding refresh token rt-1, and the server that issued it.
func signedInWithAnExpiredToken(t *testing.T) (*App, *oneToolRow, *rotatingServer) {
	t.Helper()
	server := &rotatingServer{current: "rt-1", spent: map[string]bool{}}
	auth := httptest.NewServer(server)
	t.Cleanup(auth.Close)

	inst, err := apitool.New(nil).Build(template.Input{
		Alias: "service", Variant: apitool.AuthAuthorizationCode,
		Settings: map[string]any{
			"base_url":           "https://api.service.test",
			"auth.authorize_url": auth.URL + "/authorize",
			"auth.token_url":     auth.URL + "/token",
			"auth.client_id":     "client-abc",
			"auth.client_secret": "shhh",
			"policy.mode":        apitool.Denylist,
		},
	})
	if err != nil {
		t.Fatalf("build the tool: %v", err)
	}
	tools := &oneToolRow{row: model.Tool{ID: 962, WorkspaceID: 1, Template: apitool.TemplateName, Config: inst.Config}}
	a := testApp(t)
	a.Store = &oneTool{tools: tools}

	expired := time.Now().UTC().Add(-time.Hour)
	if err := a.writeGrant(context.Background(), 1, 962, inst.Config, apitool.Grant{
		AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: &expired,
	}); err != nil {
		t.Fatalf("install the sign-in: %v", err)
	}
	return a, tools, server
}

// bindForATurn is the grant exactly as bindCustom builds it for a turn: the
// stored config, opened, as it is at that moment.
func bindForATurn(t *testing.T, a *App, tools *oneToolRow) *toolGrant {
	t.Helper()
	row, _ := tools.GetByID(context.Background(), 1, 962)
	opened, err := a.openConfigSecrets(row.Config)
	if err != nil {
		t.Fatalf("open the tool: %v", err)
	}
	return &toolGrant{app: a, toolID: row.ID, workspaceID: row.WorkspaceID, settings: settingsOf(opened)}
}

// storedGrant is the sign-in as the store holds it now.
func storedGrant(t *testing.T, a *App, tools *oneToolRow) apitool.Grant {
	t.Helper()
	row, _ := tools.GetByID(context.Background(), 1, 962)
	opened, err := a.openConfigSecrets(row.Config)
	if err != nil {
		t.Fatalf("open the tool: %v", err)
	}
	return settingsOf(opened).Grant
}

// Two calls in one turn: one renewal, and the second call uses what it produced.
func TestTwoCallsInOneTurnRenewTheSignInOnce(t *testing.T) {
	a, tools, server := signedInWithAnExpiredToken(t)
	ctx := context.Background()

	turn := bindForATurn(t, a, tools)
	first, err := turn.Token(ctx)
	if err != nil {
		t.Fatalf("the first call could not renew the sign-in: %v", err)
	}
	second, err := turn.Token(ctx)
	presented, revoked := server.saw()
	if err != nil {
		t.Fatalf("the second call in the same turn failed (refresh tokens presented %v, revoked %v): %v",
			presented, revoked, err)
	}
	if revoked {
		t.Fatalf("the service revoked the sign-in: refresh tokens presented %v", presented)
	}
	if len(presented) != 1 || presented[0] != "rt-1" {
		t.Fatalf("one expired token should take one renewal with rt-1, the service saw %v", presented)
	}
	if first != "at-2" || second != "at-2" {
		t.Fatalf("both calls should use the renewed token at-2, got %q and %q", first, second)
	}
	if held := storedGrant(t, a, tools); held.AccessToken != "at-2" || held.RefreshToken != "rt-2" {
		t.Fatalf("the renewal was not kept: %+v", held)
	}
}

// Two bindings of the same tool, which is what the Gateway and one of its agents
// are when both hold it in one turn, or two conversations at once: both were
// bound before either renewed. The second must not renew with the refresh token
// the first already spent.
func TestASecondBindingDoesNotSpendTheSameRefreshTokenAgain(t *testing.T) {
	a, tools, server := signedInWithAnExpiredToken(t)
	ctx := context.Background()

	gateway, agent := bindForATurn(t, a, tools), bindForATurn(t, a, tools)
	if _, err := gateway.Token(ctx); err != nil {
		t.Fatalf("the first binding could not renew: %v", err)
	}
	token, err := agent.Token(ctx)
	presented, revoked := server.saw()
	if err != nil || revoked {
		t.Fatalf("the second binding broke the sign-in (presented %v, revoked %v): %v", presented, revoked, err)
	}
	if len(presented) != 1 || token != "at-2" {
		t.Fatalf("the second binding should use the renewal already made: presented %v, got %q", presented, token)
	}
}

// Many bindings at the same moment, which is a fleet of agents that all hold the
// tool reaching the expiry together. One renewal, and everybody uses it.
func TestBindingsRenewingAtOnceRenewOnce(t *testing.T) {
	a, tools, server := signedInWithAnExpiredToken(t)
	server.takes = 50 * time.Millisecond
	ctx := context.Background()

	const fleet = 8
	bindings := make([]*toolGrant, fleet)
	for i := range bindings {
		bindings[i] = bindForATurn(t, a, tools)
	}
	start := make(chan struct{})
	got := make([]string, fleet)
	errs := make([]error, fleet)
	var wg sync.WaitGroup
	for i := range bindings {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got[i], errs[i] = bindings[i].Token(ctx)
		}(i)
	}
	close(start)
	wg.Wait()

	presented, revoked := server.saw()
	if revoked || len(presented) != 1 {
		t.Fatalf("%d bindings at once should renew once, the service saw %v (revoked %v)", fleet, presented, revoked)
	}
	for i := range got {
		if errs[i] != nil || got[i] != "at-2" {
			t.Fatalf("binding %d got %q, %v", i, got[i], errs[i])
		}
	}
}

// A token the service refuses before its expiry is renewed, once.
func TestARefusedTokenIsRenewedBeforeItExpires(t *testing.T) {
	a, tools, server := signedInWithAnExpiredToken(t)
	ctx := context.Background()
	turn := bindForATurn(t, a, tools)
	if _, err := turn.Token(ctx); err != nil { // at-2, good for an hour
		t.Fatalf("renew: %v", err)
	}

	token, err := turn.Renew(ctx, "at-2") // and the service refuses it anyway
	presented, _ := server.saw()
	if err != nil || token != "at-3" {
		t.Fatalf("a refused token should be renewed to at-3, got %q, %v", token, err)
	}
	if len(presented) != 2 || presented[1] != "rt-2" {
		t.Fatalf("the renewal after a refusal should use the current refresh token rt-2, the service saw %v", presented)
	}
}

// A refusal of a token somebody has ALREADY replaced renews nothing: it hands
// back the replacement. Otherwise two calls refused at once would renew twice.
func TestARefusalOfAReplacedTokenRenewsNothing(t *testing.T) {
	a, tools, server := signedInWithAnExpiredToken(t)
	ctx := context.Background()
	stale, fresh := bindForATurn(t, a, tools), bindForATurn(t, a, tools)
	if _, err := fresh.Token(ctx); err != nil { // renewed to at-2 elsewhere
		t.Fatalf("renew: %v", err)
	}

	token, err := stale.Renew(ctx, "at-1") // the stale binding was refused at-1
	presented, _ := server.saw()
	if err != nil || token != "at-2" {
		t.Fatalf("the replacement at-2 should come back, got %q, %v", token, err)
	}
	if len(presented) != 1 {
		t.Fatalf("nothing should have been renewed a second time, the service saw %v", presented)
	}
}
