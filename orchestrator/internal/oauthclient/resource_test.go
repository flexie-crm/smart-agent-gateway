package oauthclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The resource indicator is sent when there is one, and NOT sent when there is
// not. An empty value is not the same as silence: a server that validates the
// parameter reads "resource=" as an invalid resource and refuses, so a service
// configured by hand (which has no resource to name) would be unable to
// connect, while every MCP connection must go on sending its endpoint.
func TestTheResourceIsNamedOnlyWhenThereIsOne(t *testing.T) {
	withResource := &Discovery{
		AuthorizationEndpoint: "https://service.example.com/authorize",
		Resource:              "https://service.example.com/mcp",
	}
	where := AuthorizeURL(withResource, "client-1", "https://sag.example.com/cb", "state-1", "verifier-1", nil)
	asked := query(t, where)
	if asked.Get("resource") != "https://service.example.com/mcp" {
		t.Fatalf("an MCP connection stopped naming its resource: %s", where)
	}

	noResource := &Discovery{AuthorizationEndpoint: "https://service.example.com/authorize"}
	where = AuthorizeURL(noResource, "client-1", "https://sag.example.com/cb", "state-1", "verifier-1", nil)
	asked = query(t, where)
	if _, present := asked["resource"]; present {
		t.Fatalf("a service with no resource was sent an empty one: %s", where)
	}
	// And the rest of the request is untouched by that.
	for _, required := range []string{"response_type", "client_id", "redirect_uri", "state", "code_challenge"} {
		if asked.Get(required) == "" {
			t.Fatalf("%q went missing from the authorize request: %s", required, where)
		}
	}
	if asked.Get("code_challenge_method") != "S256" {
		t.Fatalf("PKCE stopped being asked for with S256: %s", where)
	}
}

// The same rule on the token endpoint, where the form is read by the server
// rather than by a person.
func TestTheTokenRequestOmitsAnEmptyResource(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resource string
		want     bool
	}{
		{"an MCP connection names its endpoint", "https://service.example.com/mcp", true},
		{"a hand configured service names nothing", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got url.Values
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				got = r.PostForm
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600}`))
			}))
			defer server.Close()

			d := &Discovery{TokenEndpoint: server.URL, Resource: tc.resource}
			if _, err := Exchange(context.Background(), d, "client-1", "", "https://sag.example.com/cb", "code-1", "verifier-1", ClientSecretBasic); err != nil {
				t.Fatalf("the exchange failed: %v", err)
			}
			if _, present := got["resource"]; present != tc.want {
				t.Fatalf("resource present = %v, wanted %v (form: %v)", present, tc.want, got)
			}
			if tc.want && got.Get("resource") != tc.resource {
				t.Fatalf("the resource was sent as %q, not %q", got.Get("resource"), tc.resource)
			}
			// The exchange itself still carries what it must.
			if got.Get("grant_type") != "authorization_code" || got.Get("code_verifier") != "verifier-1" {
				t.Fatalf("the exchange lost part of itself: %v", got)
			}
		})
	}
}

func query(t *testing.T, where string) url.Values {
	t.Helper()
	_, raw, found := strings.Cut(where, "?")
	if !found {
		t.Fatalf("the authorize address carries no parameters at all: %s", where)
	}
	asked, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("the authorize address is not readable: %v", err)
	}
	return asked
}
