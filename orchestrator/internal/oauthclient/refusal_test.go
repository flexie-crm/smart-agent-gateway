package oauthclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A token endpoint that refuses has to come back as what it SAID, whatever
// shape it said it in.
//
// This is the real failure that prompted it. A service whose 404 handler
// answers {"error":{...}} produced:
//
//	read token answer (404): json: cannot unmarshal object into Go struct
//	field .error of type string
//
// which names neither the status that mattered nor the address that was wrong,
// because the body was decoded into a struct BEFORE the status was looked at
// and a decode failure was returned as the error.
func TestARefusalComesBackAsWhatTheServiceSaid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{
			name:   "the shape that broke it: error is an object, not a string",
			status: 404,
			body:   `{"error":{"code":404,"message":"No route found for POST /oauth/tokens"}}`,
			want:   []string{"404", "No route found", "token address is wrong"},
		},
		{
			name:   "an ordinary OAuth refusal, in the service's own words",
			status: 400,
			body:   `{"error":"unsupported_grant_type","error_description":"Unsupported or missing grant_type."}`,
			want:   []string{"400", "Unsupported or missing grant_type"},
		},
		{
			name:   "an HTML page, because the address is not an endpoint at all",
			status: 404,
			body:   "<html><body><h1>Not Found</h1></body></html>",
			want:   []string{"404", "Not Found", "token address is wrong"},
		},
		{
			name:   "nothing at all",
			status: 500,
			body:   "",
			want:   []string{"500"},
		},
		{
			name:   "a 200 that carries no token is still a failure",
			status: 200,
			body:   `{"message":"your application is pending review"}`,
			want:   []string{"pending review"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			d := &Discovery{TokenEndpoint: server.URL}
			_, err := Exchange(context.Background(), d, "client-1", "secret", "https://sag.test/cb", "code-1", "verifier-1", ClientSecretBasic)
			if err == nil {
				t.Fatal("a refusal was read as a token")
			}
			// Never a Go type error: that is the failure this closes.
			if strings.Contains(err.Error(), "cannot unmarshal") || strings.Contains(err.Error(), "json:") {
				t.Fatalf("the reader's own trouble was reported instead of the service's: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the message is missing %q:\n  %v", want, err)
				}
			}
		})
	}
}

// An enormous body does not become the message.
func TestAnEnormousRefusalIsTrimmed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>" + strings.Repeat("a wall of proxy text ", 400) + "</html>"))
	}))
	defer server.Close()

	_, err := Exchange(context.Background(), &Discovery{TokenEndpoint: server.URL},
		"client-1", "secret", "https://sag.test/cb", "code-1", "verifier-1", ClientSecretBasic)
	if err == nil {
		t.Fatal("a refusal was read as a token")
	}
	if len(err.Error()) > 400 {
		t.Fatalf("the whole page became the message (%d characters)", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("the status went missing: %v", err)
	}
}
