package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/model"
)

// The console in development is served from its own origin and calls the API
// directly, so the browser asks permission first. That permission is an
// allowlist the deployment configures; it must never leak to origins that are
// not on it, and it must not exist at all when nothing is configured.

const consoleOrigin = "http://console.test"

func corsEnv(t *testing.T) *testEnv {
	return newTestEnv(t, func(cfg *config.Config) {
		cfg.AllowedOrigins = []string{consoleOrigin}
	})
}

func preflight(env *testEnv, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodOptions, "/v1/brains/view", nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	return rec
}

func TestCORSPreflightFromAllowedOrigin(t *testing.T) {
	env := corsEnv(t)

	// No bearer token on purpose: a preflight is the browser's question, and
	// the browser cannot attach the Authorization header to it. It has to be
	// answered before authentication, or the console can never make a request.
	rec := preflight(env, consoleOrigin)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != consoleOrigin {
		t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, consoleOrigin)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != http.MethodGet {
		t.Fatalf("Access-Control-Allow-Methods = %q, want %q", got, http.MethodGet)
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got != "7200" {
		t.Fatalf("Access-Control-Max-Age = %q, want 7200", got)
	}
	// Credentials are allowed so the browser may attach the HttpOnly refresh
	// cookie on the auth endpoints. This is only safe paired with an explicit
	// origin (asserted above), never "*".
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("Access-Control-Allow-Credentials = %q, want true", got)
	}
}

func TestCORSPreflightFromUnknownOriginGetsNothing(t *testing.T) {
	env := corsEnv(t)

	rec := preflight(env, "http://evil.test")

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("unknown origin was allowed: Access-Control-Allow-Origin = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "" {
		t.Fatalf("unknown origin got methods: Access-Control-Allow-Methods = %q", got)
	}
}

func TestCORSActualRequestFromAllowedOrigin(t *testing.T) {
	env := corsEnv(t)
	env.createUser("cors@acme.test", "pw-cors-1", model.PermBrainsView)
	token, _ := env.login("cors@acme.test", "pw-cors-1")

	req := httptest.NewRequest(http.MethodGet, "/v1/brains/view", nil)
	req.Header.Set("Origin", consoleOrigin)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)

	env.expectStatus(rec, http.StatusOK)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != consoleOrigin {
		t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, consoleOrigin)
	}
	// The header names the one origin that asked, so the response must say the
	// answer varies by origin or a shared cache would serve it to everyone.
	if got := rec.Header().Get("Vary"); got == "" {
		t.Fatal("expected a Vary header on a per-origin answer")
	}
}

func TestCORSActualRequestFromUnknownOriginGetsNoHeader(t *testing.T) {
	env := corsEnv(t)
	env.createUser("cors2@acme.test", "pw-cors-2", model.PermBrainsView)
	token, _ := env.login("cors2@acme.test", "pw-cors-2")

	req := httptest.NewRequest(http.MethodGet, "/v1/brains/view", nil)
	req.Header.Set("Origin", "http://evil.test")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)

	// The API still answers (the browser is the one refusing to hand the body
	// to the page), but no permission header goes with it.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("unknown origin was allowed: Access-Control-Allow-Origin = %q", got)
	}
}

func TestCORSAbsentWhenNotConfigured(t *testing.T) {
	env := newTestEnv(t)

	rec := preflight(env, consoleOrigin)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("CORS answered with no origins configured: %q", got)
	}
}
