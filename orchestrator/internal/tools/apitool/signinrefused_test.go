package apitool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"flexie.io/sag/internal/tool"
)

// A person's sign-in that the API refuses with a 401 is renewed and tried once
// more, as the other granted kinds are; the Test button's stored sign-in is not.

// acceptsOnly is an API that takes one bearer token and refuses every other.
func acceptsOnly(t *testing.T, good string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+good {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
			return
		}
		_, _ = w.Write([]byte(`{"total":1752}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sent...)
	}
}

// renewableGrant hands out a stale token, and a good one when renewed.
type renewableGrant struct {
	held, renewed string
	mu            sync.Mutex
	refused       []string
}

func (g *renewableGrant) Token(context.Context) (string, error) { return g.held, nil }

func (g *renewableGrant) Renew(_ context.Context, refused string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refused = append(g.refused, refused)
	return g.renewed, nil
}

func signedInCall(t *testing.T, srv *httptest.Server, grant interface {
	Token(context.Context) (string, error)
	Renew(context.Context, string) (string, error)
}) map[string]any {
	t.Helper()
	s := settings(t, srv.URL, Auth{
		Kind: AuthAuthorizationCode, ClientID: "client-abc", ClientSecret: "shhh",
		AuthorizeURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token",
	}, Policy{Mode: Denylist})
	res, err := Handler(s, nil, nil, grant, nil)(context.Background(),
		tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/contacts"}`)})
	if err != nil {
		t.Fatalf("the tool errored: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Content, &out); err != nil {
		t.Fatalf("the result is not readable: %v (%s)", err, res.Content)
	}
	return out
}

func TestARefusedSignInIsRenewedAndTriedOnce(t *testing.T) {
	srv, sent := acceptsOnly(t, "at-2")
	grant := &renewableGrant{held: "at-1", renewed: "at-2"}

	out := signedInCall(t, srv, grant)

	if got := sent(); len(got) != 2 || got[0] != "Bearer at-1" || got[1] != "Bearer at-2" {
		t.Fatalf("the API should see the refused token and then the renewed one, it saw %v", got)
	}
	if len(grant.refused) != 1 || grant.refused[0] != "at-1" {
		t.Fatalf("the grant should be told which token was refused: %v", grant.refused)
	}
	if status, _ := out["status"].(float64); status != 200 {
		t.Fatalf("the retried call should answer 200, got %v: %v", out["status"], out)
	}
}

// The Test button's grant proves the stored sign-in as it is and never renews:
// it hands the same token back, so there is no second request and the 401 is
// the answer, as it always was.
func TestTheTestButtonsRefusalStands(t *testing.T) {
	srv, sent := acceptsOnly(t, "at-2")

	out := signedInCall(t, srv, storedToken("at-1"))

	if got := sent(); len(got) != 1 {
		t.Fatalf("nothing better was offered, so the call should not be repeated: %v", got)
	}
	if status, _ := out["status"].(float64); status != http.StatusUnauthorized {
		t.Fatalf("the refusal should stand, got %v", out["status"])
	}
}
