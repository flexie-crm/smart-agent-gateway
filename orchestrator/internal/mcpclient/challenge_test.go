package mcpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The specification is explicit: an MCP client "MUST be able to parse
// WWW-Authenticate headers and respond appropriately to HTTP 401 Unauthorized
// responses". The header is how a server says where its metadata lives, and a
// client that guesses the path instead only works against servers that keep it
// where it guessed.

func TestParsingAChallenge(t *testing.T) {
	// The exact shape a real server answers with.
	got := parseChallenge(`Bearer resource_metadata="https://mcp.test/.well-known/oauth-protected-resource", scope="mcp", error="invalid_token"`)
	if got.ResourceMetadata != "https://mcp.test/.well-known/oauth-protected-resource" {
		t.Fatalf("resource metadata %q", got.ResourceMetadata)
	}
	if got.Scope != "mcp" {
		t.Fatalf("scope %q", got.Scope)
	}
}

func TestParsingAChallengeWithAwkwardValues(t *testing.T) {
	// A scope LIST contains a space, and other parameters contain commas, so
	// splitting on either would take the wrong thing.
	got := parseChallenge(`Bearer error="invalid_token", error_description="Token expired, please re-authorize", scope="mcp api profile", resource_metadata="https://host/.well-known/oauth-protected-resource/mcp"`)
	if got.Scope != "mcp api profile" {
		t.Fatalf("scope %q: a multi-scope value was cut", got.Scope)
	}
	if got.ResourceMetadata != "https://host/.well-known/oauth-protected-resource/mcp" {
		t.Fatalf("resource metadata %q", got.ResourceMetadata)
	}
}

func TestAChallengeWeCannotUseIsNotGuessedAt(t *testing.T) {
	for _, header := range []string{
		"",
		"Basic realm=\"something\"",
		"Bearer error=\"invalid_token\"",
	} {
		if got := parseChallenge(header); got.ResourceMetadata != "" {
			t.Fatalf("%q produced %q", header, got.ResourceMetadata)
		}
	}
}

// The point of the header: a server whose metadata is NOT where a client would
// guess is still discoverable, because it said where to look.
func TestDiscoveryFollowsTheHeaderToAnUnguessablePlace(t *testing.T) {
	var mux *http.ServeMux
	var srv *httptest.Server
	mux = http.NewServeMux()
	srv = httptest.NewServer(mux)
	defer srv.Close()

	// The document lives somewhere no client would try.
	mux.HandleFunc("/deep/somewhere/else/metadata.json", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{
			"resource":              srv.URL + "/mcp",
			"authorization_servers": []string{srv.URL + "/issuer"},
		})
	})
	mux.HandleFunc("/issuer/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer":                           srv.URL + "/issuer",
			"authorization_endpoint":           srv.URL + "/issuer/authorize",
			"token_endpoint":                   srv.URL + "/issuer/token",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate",
			`Bearer resource_metadata="`+srv.URL+`/deep/somewhere/else/metadata.json", scope="mcp"`)
		w.WriteHeader(http.StatusUnauthorized)
	})

	d, err := Discover(context.Background(), srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if d.AuthorizationEndpoint != srv.URL+"/issuer/authorize" {
		t.Fatalf("authorization endpoint %q: the header was not followed", d.AuthorizationEndpoint)
	}
	if d.Scope != "mcp" {
		t.Fatalf("scope %q: the challenge named one and it was dropped", d.Scope)
	}
}

// And a server that publishes nothing at all still connects the old way, so
// reading the header can never be the reason a connection fails.
func TestDiscoveryStillWorksWithoutAChallenge(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer":                           srv.URL,
			"authorization_endpoint":           srv.URL + "/authorize",
			"token_endpoint":                   srv.URL + "/token",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	d, err := Discover(context.Background(), srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if d.TokenEndpoint != srv.URL+"/token" {
		t.Fatalf("token endpoint %q", d.TokenEndpoint)
	}
}

func writeTestJSON(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for k, v := range body {
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(`"` + k + `":`)
		switch t := v.(type) {
		case string:
			b.WriteString(`"` + t + `"`)
		case []string:
			b.WriteByte('[')
			for i, s := range t {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(`"` + s + `"`)
			}
			b.WriteByte(']')
		}
	}
	b.WriteByte('}')
	_, _ = w.Write([]byte(b.String()))
}
