package oauthclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// What real services require beyond the protocol, and neither is optional.
//
// Google's own documentation: "Set the value to offline if your application
// needs to refresh access tokens when the user is not present at the browser.
// This value instructs the Google authorization server to return a refresh
// token and an access token the first time that your application exchanges an
// authorization code for tokens." A connection made without it works for an
// hour and then cannot be renewed at all, which is the worst shape of failure
// there is: it works when it is set up and stops later.
func TestAServicesOwnParametersReachTheSignIn(t *testing.T) {
	d := &Discovery{AuthorizationEndpoint: "https://accounts.example.com/o/oauth2/v2/auth"}
	extra := url.Values{}
	extra.Set("access_type", "offline")
	extra.Set("audience", "https://api.example.com")
	extra.Set("prompt", "consent")

	where := AuthorizeURL(d, "client-1", "https://sag.test/cb", "state-1", "verifier-1", extra)
	asked := query(t, where)

	for name, want := range map[string]string{
		"access_type": "offline",
		"audience":    "https://api.example.com",
		"prompt":      "consent",
	} {
		if asked.Get(name) != want {
			t.Fatalf("%s did not reach the sign-in: %s", name, where)
		}
	}
	// And the protocol's own parameters are untouched.
	for _, required := range []string{"response_type", "client_id", "redirect_uri", "state", "code_challenge"} {
		if asked.Get(required) == "" {
			t.Fatalf("%q went missing: %s", required, where)
		}
	}
	if asked.Get("code_challenge_method") != "S256" {
		t.Fatalf("PKCE stopped being asked for: %s", where)
	}
}

// A typed parameter cannot overwrite what makes the round trip work.
//
// Not hypothetical: these are typed by hand, so a clashing name is a matter of
// when. Overwriting the state breaks the callback; overwriting the challenge
// breaks PKCE, which is the only protection a public client has.
func TestATypedParameterCannotBreakTheRoundTrip(t *testing.T) {
	d := &Discovery{AuthorizationEndpoint: "https://accounts.example.com/auth"}
	extra := url.Values{}
	extra.Set("state", "not-ours")
	extra.Set("code_challenge", "not-ours")
	extra.Set("redirect_uri", "https://somewhere.else.test/steal")
	extra.Set("client_id", "another-client")

	where := AuthorizeURL(d, "client-1", "https://sag.test/cb", "state-1", "verifier-1", extra)
	asked := query(t, where)

	if asked.Get("state") != "state-1" {
		t.Fatalf("the state was overwritten, so the callback cannot be opened: %s", where)
	}
	if asked.Get("redirect_uri") != "https://sag.test/cb" {
		t.Fatalf("the redirect was overwritten: %s", where)
	}
	if asked.Get("client_id") != "client-1" {
		t.Fatalf("the client was overwritten: %s", where)
	}
	if asked.Get("code_challenge") == "not-ours" {
		t.Fatalf("PKCE was overwritten, which is the one protection a public client has: %s", where)
	}
	// Set, not added: a duplicated parameter is refused outright by some servers.
	if len(asked["state"]) != 1 || len(asked["client_id"]) != 1 {
		t.Fatalf("a parameter was duplicated rather than replaced: %v", asked)
	}
}

// Where the client's own credentials go on the token request, which services
// genuinely differ on. Google's documentation recommends the body; the OAuth
// default is the header, and some servers accept only that.
func TestTheClientProvesItselfWhereTheServiceWantsIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		how      ClientAuth
		wantAuth bool
		wantBody bool
	}{
		{"the header, which is the default", ClientSecretBasic, true, false},
		{"the body, which Google recommends", ClientSecretPost, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sawAuth bool
			var form url.Values
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sawAuth = r.Header.Get("Authorization") != ""
				_ = r.ParseForm()
				form = r.PostForm
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600}`))
			}))
			defer server.Close()

			if _, err := Exchange(context.Background(), &Discovery{TokenEndpoint: server.URL},
				"client-1", "the-secret", "https://sag.test/cb", "code-1", "verifier-1", tc.how); err != nil {
				t.Fatalf("exchange: %v", err)
			}
			if sawAuth != tc.wantAuth {
				t.Fatalf("Authorization header present = %v, wanted %v", sawAuth, tc.wantAuth)
			}
			if got := form.Get("client_secret") != ""; got != tc.wantBody {
				t.Fatalf("client_secret in the body = %v, wanted %v (form: %v)", got, tc.wantBody, form)
			}
			// The client id travels either way, which every server tolerates
			// and some expect.
			if form.Get("client_id") != "client-1" {
				t.Fatalf("the client id did not travel: %v", form)
			}
		})
	}
}

// A public client has no secret, so nothing is sent in either place. An empty
// Basic credential is a 401 at services that read it.
func TestAPublicClientSendsNoSecretAnywhere(t *testing.T) {
	var sawAuth bool
	var form url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization") != ""
		_ = r.ParseForm()
		form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600}`))
	}))
	defer server.Close()

	for _, how := range []ClientAuth{ClientSecretBasic, ClientSecretPost} {
		if _, err := Exchange(context.Background(), &Discovery{TokenEndpoint: server.URL},
			"client-1", "", "https://sag.test/cb", "code-1", "verifier-1", how); err != nil {
			t.Fatalf("exchange with no secret (%s): %v", how, err)
		}
		if sawAuth {
			t.Fatalf("an empty Basic credential was sent for a public client (%s)", how)
		}
		if _, present := form["client_secret"]; present {
			t.Fatalf("an empty client_secret was sent in the body (%s): %v", how, form)
		}
		// PKCE is what a public client proves itself with, so it must be there.
		if form.Get("code_verifier") != "verifier-1" {
			t.Fatalf("the verifier did not travel (%s): %v", how, form)
		}
	}
	_ = strings.TrimSpace("")
}
