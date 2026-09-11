package api

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flexie.io/sag/internal/config"
)

// One process serves the console and the API it calls. The console is a page
// plus hashed assets; the API owns /v1. The seams between the two are what
// these tests pin down: a client route is the page, an asset is the asset,
// a misspelled endpoint is a JSON error, and nothing outside the console's
// directory can ever be read through it.

const indexHTML = "<!doctype html><title>sag console</title>"

func consoleEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	parent := t.TempDir()
	// A file OUTSIDE the console directory, to prove it stays unreachable.
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("not yours"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	dir := filepath.Join(parent, "console")
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatalf("make console dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(indexHTML), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app-abc123.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatalf("write asset: %v", err)
	}
	env := newTestEnv(t, func(cfg *config.Config) { cfg.ConsoleDir = dir })
	return env, dir
}

func (e *testEnv) get(path string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func TestConsoleServesThePage(t *testing.T) {
	env, _ := consoleEnv(t)

	for _, path := range []string{"/", "/brains", "/agents", "/some/deep/route"} {
		rec := env.get(path)
		env.expectStatus(rec, http.StatusOK)
		if rec.Body.String() != indexHTML {
			t.Fatalf("GET %s: got %q, want the console page", path, rec.Body.String())
		}
		// The page cannot carry a hash in its name, so it must never be
		// cached into staleness.
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("GET %s: Cache-Control = %q, want no-cache", path, got)
		}
	}
}

func TestConsoleServesHashedAssetsForever(t *testing.T) {
	env, _ := consoleEnv(t)

	rec := env.get("/assets/app-abc123.js")
	env.expectStatus(rec, http.StatusOK)
	if rec.Body.String() != "console.log(1)" {
		t.Fatalf("asset body = %q", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("asset Cache-Control = %q, want immutable", got)
	}
}

func TestConsoleNeverReadsOutsideItsDirectory(t *testing.T) {
	env, _ := consoleEnv(t)

	for _, path := range []string{"/../secret.txt", "/assets/../../secret.txt"} {
		req := httptest.NewRequest(http.MethodGet, "http://sag.test", nil)
		// Bypass NewRequest's own cleaning so the raw traversal reaches the
		// router, the way a hostile client would send it.
		req.URL.Path = path
		rec := httptest.NewRecorder()
		env.router.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), "not yours") {
			t.Fatalf("GET %s escaped the console directory", path)
		}
	}
}

func TestConsoleDoesNotShadowTheAPI(t *testing.T) {
	env, _ := consoleEnv(t)

	// A misspelled endpoint is an API error, never a happy little HTML page
	// handed to a client that asked for JSON.
	rec := env.get("/v1/no-such-endpoint")
	env.expectStatus(rec, http.StatusNotFound)
	if body := rec.Body.String(); !strings.Contains(body, "not_found") {
		t.Fatalf("API 404 body = %q, want a JSON error", body)
	}

	// And the health probe answers as itself.
	rec = env.get("/healthz")
	if strings.Contains(rec.Body.String(), "<!doctype") {
		t.Fatal("healthz served the console page")
	}
}

func TestNoConsoleConfiguredMeansNoConsole(t *testing.T) {
	env := newTestEnv(t)

	rec := env.get("/")
	env.expectStatus(rec, http.StatusNotFound)
}

// --- pages served by a build tool ------------------------------------------
//
// A page can be configured as an address instead of a directory, so that
// editing it is a reload rather than a rebuild. What these pin down is that it
// is the SAME deployment either way: one origin, the paths unchanged, and the
// page's live-reload channel carried through, because a page that loads but
// never reloads is the whole feature missing.

func TestAPathIsADirectoryAndAnAddressIsNot(t *testing.T) {
	// The default has to stay the default: every deployment configures a
	// directory, and one misread as an address would serve nothing at all.
	for _, location := range []string{
		"/var/www/console", "console/dist", "", "C:\\console",
		// Close enough to be worth stating: no scheme is not an address.
		"127.0.0.1:5173", "//127.0.0.1:5173",
	} {
		if _, ok := buildToolAddress(location); ok {
			t.Fatalf("%q was read as an address, want a directory", location)
		}
	}
	for _, location := range []string{"http://127.0.0.1:5173", "https://localhost:5174"} {
		if _, ok := buildToolAddress(location); !ok {
			t.Fatalf("%q was read as a directory, want an address", location)
		}
	}
}

func TestAPageServedByABuildToolKeepsItsPath(t *testing.T) {
	var asked []string
	tool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		_, _ = w.Write([]byte("from the build tool"))
	}))
	defer tool.Close()

	env := newTestEnv(t, func(cfg *config.Config) {
		cfg.ConsoleDir = tool.URL
		cfg.ChatDir = tool.URL
	})

	// The chat is built for a sub-path, so the build tool serves it there too.
	// Rewriting the path here would hand it requests it has no answer for.
	for _, path := range []string{"/", "/agents", "/chat/", "/chat/assets/index.js"} {
		rec := env.get(path)
		env.expectStatus(rec, http.StatusOK)
		if rec.Body.String() != "from the build tool" {
			t.Fatalf("GET %s: got %q", path, rec.Body.String())
		}
	}
	want := []string{"/", "/agents", "/chat/", "/chat/assets/index.js"}
	if strings.Join(asked, ",") != strings.Join(want, ",") {
		t.Fatalf("the build tool was asked for %v, want %v", asked, want)
	}

	// And the API still owns its own routes: proxying a page must not put a
	// build tool in front of the endpoints the page calls.
	rec := env.get("/v1/no-such-endpoint")
	env.expectStatus(rec, http.StatusNotFound)
	if !strings.Contains(rec.Body.String(), "not_found") {
		t.Fatalf("the API was proxied away: %q", rec.Body.String())
	}
}

func TestABuildToolThatIsNotUpYetSaysSo(t *testing.T) {
	// Nothing is listening here: the port is taken and released, so the address
	// is well formed and refuses connections.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := dead.URL
	dead.Close()

	env := newTestEnv(t, func(cfg *config.Config) { cfg.ConsoleDir = address })

	rec := env.get("/")
	env.expectStatus(rec, http.StatusBadGateway)
	if !strings.Contains(rec.Body.String(), address) {
		t.Fatalf("a page that is not being served yet did not say where: %q", rec.Body.String())
	}
}

// The page's live-reload channel is a WebSocket back to the origin it was
// loaded from, which is us. This drives a real connection through a real
// listener, because an upgrade cannot be proved against a recorder: the
// hijacking is the part that has to work.
func TestALiveReloadChannelSurvivesTheProxy(t *testing.T) {
	tool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			t.Errorf("the upgrade did not reach the build tool: %v", r.Header)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
		// Echo, so the test proves bytes flow after the switch rather than
		// only that the handshake was answered.
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			t.Errorf("read after upgrade: %v", err)
			return
		}
		_, _ = conn.Write(buf[:n])
	}))
	defer tool.Close()

	env := newTestEnv(t, func(cfg *config.Config) { cfg.ConsoleDir = tool.URL })
	gateway := httptest.NewServer(env.router)
	defer gateway.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(gateway.URL, "http://"))
	if err != nil {
		t.Fatalf("dial the gateway: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: sag.test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: EhqPRfCLQF2GRXfLYaZOaA==\r\n\r\n"))

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("the connection was not upgraded: %q", status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read the headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	if _, err := conn.Write([]byte("reload")); err != nil {
		t.Fatalf("write after the upgrade: %v", err)
	}
	echoed := make([]byte, 6)
	if _, err := io.ReadFull(reader, echoed); err != nil {
		t.Fatalf("read the echo: %v", err)
	}
	if string(echoed) != "reload" {
		t.Fatalf("echoed %q, want the bytes back", echoed)
	}
}
