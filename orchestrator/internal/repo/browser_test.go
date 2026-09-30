package repo

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// What a desktop application asks for on its first run: the browser it was
// built against, by version, by platform.
func TestTheBrowserIsServedByVersionAndPlatform(t *testing.T) {
	dir := t.TempDir()
	writeBrowser(t, dir, "149.0.7827.55", "chrome-headless-shell-mac-arm64.zip", "PK\x03\x04zip")

	answer := fetchThrough(t, dir, "/browser/149.0.7827.55/chrome-headless-shell-mac-arm64.zip")
	if answer.Code != http.StatusOK {
		t.Fatalf("the browser answered %d, and an application on its first run has nothing to fetch",
			answer.Code)
	}
	if got := answer.Body.String(); got != "PK\x03\x04zip" {
		t.Errorf("served %q", got)
	}
}

// A version nobody mirrored is a plain 404, which is what the downloader is
// built to report. It must not fall through to anything.
func TestAVersionThatWasNeverMirroredIsNotFound(t *testing.T) {
	dir := t.TempDir()
	writeBrowser(t, dir, "149.0.7827.55", "chrome-headless-shell-mac-arm64.zip", "PK\x03\x04zip")

	if code := fetchThrough(t, dir, "/browser/150.0.0.0/chrome-headless-shell-mac-arm64.zip").Code; code != http.StatusNotFound {
		t.Errorf("a version that is not published answered %d", code)
	}
	if code := fetchThrough(t, dir, "/browser/149.0.7827.55/chrome-headless-shell-linux64.zip").Code; code != http.StatusNotFound {
		t.Errorf("a platform that is not published answered %d", code)
	}
}

// The prefix is rooted at its OWN subtree, not one level up.
//
// This is the lesson the desktop and updates handlers already carry, written
// down in `served`: `%2e%2e` survives routing and is decoded into r.URL.Path
// afterwards, so a handler rooted at the whole download directory served
// everything in it through any of its prefixes. Everything published here is
// public, so nothing leaked, but the confinement was a level looser than the
// comments claimed. This keeps it true for the browser the day something lands
// in that directory which is not for everybody.
func TestTheBrowserPrefixCannotReachOutOfItsOwnDirectory(t *testing.T) {
	dir := t.TempDir()
	writeBrowser(t, dir, "149.0.7827.55", "chrome-headless-shell-mac-arm64.zip", "PK\x03\x04zip")
	// A file next to the browser directory, which this prefix must not serve.
	if err := os.WriteFile(filepath.Join(dir, "builds.txt"), []byte("not yours"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/browser/%2e%2e/builds.txt",
		"/browser/../builds.txt",
		"/browser/..%2fbuilds.txt",
	} {
		answer := fetchThrough(t, dir, path)
		if answer.Code == http.StatusOK && answer.Body.String() == "not yours" {
			t.Errorf("%s reached out of the browser directory and served a file beside it", path)
		}
	}
}

// Asking for the bare directory is a 404 rather than a listing, the same as
// every other prefix here. The page is the index; a generated listing beside it
// would be a second answer to the same question.
func TestTheBrowserDirectoryIsNotBrowsable(t *testing.T) {
	dir := t.TempDir()
	writeBrowser(t, dir, "149.0.7827.55", "chrome-headless-shell-mac-arm64.zip", "PK\x03\x04zip")

	for _, path := range []string{"/browser/", "/browser/149.0.7827.55/"} {
		if code := fetchThrough(t, dir, path).Code; code != http.StatusNotFound {
			t.Errorf("%s answered %d rather than refusing to list itself", path, code)
		}
	}
}

// fetchThrough serves one request against a server rooted at dir, THROUGH THE
// WHOLE MUX rather than by calling a handler, because routing is half of what
// these assertions are about: which prefix wins, and what it is rooted at.
func fetchThrough(t *testing.T, dir, path string) *httptest.ResponseRecorder {
	t.Helper()
	s, err := New(dir, false)
	if err != nil {
		t.Fatalf("the server would not start on %s: %v", dir, err)
	}
	answer := httptest.NewRecorder()
	s.Handler().ServeHTTP(answer, httptest.NewRequest("GET", path, nil))
	return answer
}

func writeBrowser(t *testing.T, dir, version, name, body string) {
	t.Helper()
	at := filepath.Join(dir, browserDir, version)
	if err := os.MkdirAll(at, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(at, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
