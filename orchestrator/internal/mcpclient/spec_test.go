package mcpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The specification's discovery order, which exists because real servers put the
// same document in different places. A tenant under a path is the case a single
// guess gets wrong.
func TestWhereWeLookForAuthorizationMetadata(t *testing.T) {
	if got := authServerMetadataURLs("https://auth.example.com"); !sameList(got, []string{
		"https://auth.example.com/.well-known/oauth-authorization-server",
		"https://auth.example.com/.well-known/openid-configuration",
	}) {
		t.Fatalf("issuer with no path: %v", got)
	}

	// With a path, RFC 8414 INSERTS it after the well-known segment and OpenID
	// Connect Discovery appends it. The first three are the specification's, in
	// its order; the fourth is the pre-RFC-8414 appended OAuth form, tried last
	// because some servers still publish only that.
	if got := authServerMetadataURLs("https://auth.example.com/tenant1"); !sameList(got, []string{
		"https://auth.example.com/.well-known/oauth-authorization-server/tenant1",
		"https://auth.example.com/.well-known/openid-configuration/tenant1",
		"https://auth.example.com/tenant1/.well-known/openid-configuration",
		"https://auth.example.com/tenant1/.well-known/oauth-authorization-server",
	}) {
		t.Fatalf("issuer with a path: %v", got)
	}
}

func TestDiscoveryFindsATenantUnderAPath(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Only the path-inserted form exists, which is the one a root-only guess
	// would never reach.
	mux.HandleFunc("/.well-known/oauth-authorization-server/tenant1", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer":                           srv.URL + "/tenant1",
			"authorization_endpoint":           srv.URL + "/tenant1/authorize",
			"token_endpoint":                   srv.URL + "/tenant1/token",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{
			"resource":              srv.URL + "/mcp",
			"authorization_servers": []string{srv.URL + "/tenant1"},
		})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	d, err := Discover(context.Background(), srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if d.TokenEndpoint != srv.URL+"/tenant1/token" {
		t.Fatalf("token endpoint %q", d.TokenEndpoint)
	}
}

// "If code_challenge_methods_supported is absent, the authorization server does
// not support PKCE and MCP clients MUST refuse to proceed." Refused at
// discovery, so nobody is sent to a browser to consent to something we would
// then have to abandon.
func TestAServerWithoutPKCEIsRefusedBeforeAnybodyConsents(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
			// no code_challenge_methods_supported
		})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	_, err := Discover(context.Background(), srv.URL+"/mcp")
	if err == nil {
		t.Fatal("a server that does not support PKCE was accepted")
	}
	if !strings.Contains(err.Error(), "PKCE") {
		t.Fatalf("refused, but not for the reason a person needs: %v", err)
	}
}

// And a server offering only the deprecated plain method is not PKCE support
// either: the specification requires S256 when the client is capable, and we are.
func TestPlainIsNotPKCESupport(t *testing.T) {
	if supportsS256([]string{"plain"}) {
		t.Fatal("plain was accepted as PKCE support")
	}
	if !supportsS256([]string{"plain", "S256"}) {
		t.Fatal("S256 was missed when offered alongside plain")
	}
}

func sameList(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
