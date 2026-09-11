package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flexie.io/sag/internal/config"
)

// A desktop has no proxy, so one process serves the API, the console and the
// chat. Two single-page apps behind one origin is exactly the arrangement where
// one silently answers for the other, and these pin the seams.

const chatIndexHTML = "<!doctype html><title>sag chat</title>"

func bothAppsEnv(t *testing.T) *testEnv {
	t.Helper()
	parent := t.TempDir()

	console := filepath.Join(parent, "console")
	if err := os.MkdirAll(filepath.Join(console, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(console, "index.html"), []byte(indexHTML), 0o600); err != nil {
		t.Fatal(err)
	}

	chat := filepath.Join(parent, "chat")
	if err := os.MkdirAll(filepath.Join(chat, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chat, "index.html"), []byte(chatIndexHTML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chat, "assets", "chat-def456.js"), []byte("chat(1)"), 0o600); err != nil {
		t.Fatal(err)
	}

	return newTestEnv(t, func(cfg *config.Config) {
		cfg.ConsoleDir = console
		cfg.ChatDir = chat
	})
}

func TestEachApplicationAnswersOnItsOwnPath(t *testing.T) {
	env := bothAppsEnv(t)

	if res := env.get("/"); !strings.Contains(res.Body.String(), "sag console") {
		t.Fatalf("the root should be the console, got %q", res.Body.String())
	}
	// The console owns a catch-all, so the question is whether a more specific
	// route survives beside it. It does, and independently of declaration order:
	// chi matches on specificity, which was checked by swapping the two mounts
	// and finding these tests still pass.
	if res := env.get("/chat/"); !strings.Contains(res.Body.String(), "sag chat") {
		t.Fatalf("/chat/ should be the chat, got %q", res.Body.String())
	}
}

func TestAChatClientRouteLoadsTheChatAndNotTheConsole(t *testing.T) {
	env := bothAppsEnv(t)
	// A path only the chat's router knows about. It is not a file, so it must
	// come back as the chat's page rather than a 404 or the console.
	res := env.get("/chat/c/some-conversation-id")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), "sag chat") {
		t.Fatalf("a chat route loaded the wrong application: %q", res.Body.String())
	}
}

func TestTheAddressPeopleTypeReachesTheChat(t *testing.T) {
	env := bothAppsEnv(t)
	// Without the redirect this falls through to the console's catch-all and
	// quietly loads the wrong application, which looks like the chat is broken.
	res := env.get("/chat")
	if res.Code != http.StatusMovedPermanently {
		t.Fatalf("status %d, want a redirect to /chat/", res.Code)
	}
	if loc := res.Header().Get("Location"); loc != "/chat/" {
		t.Fatalf("redirected to %q, want /chat/", loc)
	}
}

func TestChatAssetsAreCachedAndItsPageIsNot(t *testing.T) {
	env := bothAppsEnv(t)

	res := env.get("/chat/assets/chat-def456.js")
	if res.Code != http.StatusOK {
		t.Fatalf("asset status %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), "chat(1)") {
		t.Fatalf("wrong asset body: %q", res.Body.String())
	}
	// A hashed name cannot be served stale, so it is cached hard.
	if cc := res.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("asset Cache-Control is %q", cc)
	}
	// The page's name carries no hash, so a cached copy would load assets that
	// no longer exist after an update.
	page := env.get("/chat/")
	if cc := page.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("page Cache-Control is %q, want no-cache", cc)
	}
}

func TestTheApiStillAnswersAsTheApi(t *testing.T) {
	env := bothAppsEnv(t)
	// With two catch-alls in the tree, the risk is that a mistyped endpoint
	// starts returning HTML to something that asked for JSON.
	res := env.get("/v1/no-such-endpoint")
	if res.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", res.Code)
	}
	if body := res.Body.String(); strings.Contains(body, "<!doctype") {
		t.Fatalf("an API path returned a page: %q", body)
	}
}

func TestWithNoChatConfiguredTheConsoleKeepsEverything(t *testing.T) {
	// The server deployment has no chat directory: it serves the console at the
	// root and the chat lives behind its own name. Adding the chat mount must
	// not have changed that.
	env, _ := consoleEnv(t)
	res := env.get("/chat/")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), "sag console") {
		t.Fatalf("with no chat configured this path belongs to the console, got %q", res.Body.String())
	}
}
