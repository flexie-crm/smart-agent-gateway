package repo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Which version is newer, decided numerically.
//
// As strings, "0.10.0" sorts BEFORE "0.9.0", so every installation would stop
// updating at exactly the moment a minor version reached double figures, and it
// would look like the update host had gone quiet rather than like a comparison
// being wrong.
func TestWhichVersionIsNewer(t *testing.T) {
	newer := []struct{ latest, current string }{
		{"0.2.0", "0.1.0"},
		{"0.10.0", "0.9.0"},
		{"1.0.0", "0.99.99"},
		{"0.1.10", "0.1.9"},
		{"0.1.1", "0.1.0"},
	}
	for _, c := range newer {
		if !newerThan(c.latest, c.current) {
			t.Errorf("%s should be newer than %s", c.latest, c.current)
		}
	}

	notNewer := []struct{ latest, current string }{
		{"0.1.0", "0.1.0"},   // the same
		{"0.1.0", "0.2.0"},   // older
		{"0.9.0", "0.10.0"},  // the string trap, the other way round
		{"garbage", "0.1.0"}, // unreadable sorts as zero, so it never pushes
	}
	for _, c := range notNewer {
		if newerThan(c.latest, c.current) {
			t.Errorf("%s should NOT be offered to %s", c.latest, c.current)
		}
	}
}

// A pre-release is compared on its numbers alone, because whether one beta is
// newer than another is a question this product does not have yet.
func TestAPreReleaseIsComparedOnItsNumbers(t *testing.T) {
	if !newerThan("0.2.0-beta.1", "0.1.0") {
		t.Error("a beta of a newer version should still be newer")
	}
	if newerThan("0.1.0-beta.2", "0.1.0") {
		t.Error("a beta of the SAME version is not an upgrade")
	}
}

// The path carries four parts and reaches the filesystem, so anything that is
// not a plain name is refused rather than cleaned up.
func TestAnUpdatePathCannotReachOutOfItsDirectory(t *testing.T) {
	refused := []string{
		"/updates/personal/darwin/x86_64",              // three parts
		"/updates/personal/darwin/x86_64/0.1.0/extra",  // five
		"/updates/../../etc/darwin/x86_64/0.1.0",       // dot-dot
		"/updates/personal/darwin/..%2f..%2fetc/0.1.0", // encoded, once decoded
		"/updates///0.1.0",                             // empty parts
	}
	for _, path := range refused {
		if _, _, _, _, ok := readUpdateRequest(path); ok {
			t.Errorf("%q was accepted", path)
		}
	}

	edition, target, arch, current, ok := readUpdateRequest("/updates/personal/darwin/x86_64/0.1.0")
	if !ok || edition != "personal" || target != "darwin" || arch != "x86_64" || current != "0.1.0" {
		t.Fatalf("a good path was read as %q %q %q %q (ok=%v)", edition, target, arch, current, ok)
	}
}

// Being current and being on a platform we do not publish for are the same
// answer, and neither is an error anybody should see.
func TestNothingToInstallIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dir: dir}

	// Nothing published at all.
	if got := ask(t, s, "/updates/personal/darwin/x86_64/0.1.0"); got.Code != http.StatusNoContent {
		t.Fatalf("with nothing published: got %d, want 204", got.Code)
	}

	writeManifest(t, dir, "personal-darwin-x86_64.json", release{
		Version:   "0.1.0",
		Signature: "sig",
		URL:       "/updates/personal/SAG-0.1.0.tar.gz",
	})
	// Published, and the same version this application is running.
	if got := ask(t, s, "/updates/personal/darwin/x86_64/0.1.0"); got.Code != http.StatusNoContent {
		t.Fatalf("when current: got %d, want 204", got.Code)
	}
}

// And when there IS one, the answer carries what the updater needs, with the
// address filled in from the host that was asked.
func TestAnAvailableVersionIsDescribedInFull(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dir: dir}
	writeManifest(t, dir, "personal-darwin-x86_64.json", release{
		Version:   "0.2.0",
		Signature: "a-real-signature",
		URL:       "/updates/personal/SAG-0.2.0.tar.gz",
	})

	got := ask(t, s, "/updates/personal/darwin/x86_64/0.1.0")
	if got.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", got.Code)
	}
	var out release
	if err := json.Unmarshal(got.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Version != "0.2.0" || out.Signature != "a-real-signature" {
		t.Fatalf("the release was not described: %+v", out)
	}
	// Stored as a path, served as an address, so a manifest written on a build
	// machine does not have to know what this host is called.
	if out.URL != "https://sag-repo.example/updates/personal/SAG-0.2.0.tar.gz" {
		t.Fatalf("the address was not filled in: %q", out.URL)
	}
}

// A manifest naming a version but no signature is not a release. Serving it
// would have every installation download something it must then refuse.
func TestAnIncompleteManifestPublishesNothing(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dir: dir}
	writeManifest(t, dir, "personal-darwin-x86_64.json", release{Version: "0.2.0"})

	if got := ask(t, s, "/updates/personal/darwin/x86_64/0.1.0"); got.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204 for a manifest with no signature", got.Code)
	}
}

// The archive a manifest names must actually be downloadable at that address.
//
// It was not. Everything under /updates/ went to the manifest handler, which
// reads a four-part path, so the archive (two parts) was read as a malformed
// question and answered 404: every installation would have been told a new
// version existed and then failed to fetch it. Which is precisely the failure
// the manifest is written by the build to prevent, arrived at from the other
// side.
//
// Routed through the whole Handler rather than to a function, because what
// broke was the ROUTING and a test that called handleUpdate directly would have
// passed throughout.
func TestTheArchiveAManifestNamesCanBeDownloaded(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "updates", "personal"), 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "updates", "personal", "SAG-Personal-0.2.0.app.tar.gz")
	if err := os.WriteFile(archive, []byte("not really a tarball"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, dir, "personal-darwin-x86_64.json", release{
		Version:   "0.2.0",
		Signature: "sig",
		URL:       "/updates/personal/SAG-Personal-0.2.0.app.tar.gz",
	})

	s, err := New(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()

	// Ask as an older installation would, then fetch exactly what it is told.
	asked := httptest.NewRequest(http.MethodGet, "/updates/personal/darwin/x86_64/0.1.0", nil)
	asked.Host = "sag-repo.example"
	answer := httptest.NewRecorder()
	handler.ServeHTTP(answer, asked)
	if answer.Code != http.StatusOK {
		t.Fatalf("no update offered: %d", answer.Code)
	}
	var rel release
	if err := json.Unmarshal(answer.Body.Bytes(), &rel); err != nil {
		t.Fatal(err)
	}

	fetched := httptest.NewRecorder()
	handler.ServeHTTP(fetched, httptest.NewRequest(http.MethodGet, "/updates/personal/SAG-Personal-0.2.0.app.tar.gz", nil))
	if fetched.Code != http.StatusOK {
		t.Fatalf("the archive at %s answered %d, so the update it announced cannot be installed",
			rel.URL, fetched.Code)
	}
	if fetched.Body.String() != "not really a tarball" {
		t.Fatalf("the wrong bytes came back: %q", fetched.Body.String())
	}
}

func ask(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = "sag-repo.example"
	w := httptest.NewRecorder()
	s.handleUpdate(w, r)
	return w
}

func writeManifest(t *testing.T, dir, name string, rel release) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "updates"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "updates", name), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The same escape the installer path had, on the prefix that predates it. One
// handler rooted at the whole directory served BOTH, and `%2e%2e` survives
// routing to be decoded into r.URL.Path afterwards, so this answered 200 with a
// file from outside updates/.
func TestAnUpdatePathCannotClimbOutOfItsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "updates", "personal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "install.sh"), []byte("NOT-VIA-UPDATES"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/updates/%2e%2e/install.sh", "/updates/../install.sh"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if strings.Contains(w.Body.String(), "NOT-VIA-UPDATES") {
			t.Errorf("%q read a file from outside updates/", path)
		}
	}
}
