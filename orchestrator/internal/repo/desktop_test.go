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

// An edition with nothing published says "coming soon" rather than offering a
// link that answers 404. The page decides that from the disk, so the day an
// installer is uploaded is the day the button appears, with nothing to
// remember.
func TestAnEditionWithNoInstallerIsNotOffered(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dir: dir}
	if got := s.installerFor("personal"); got != nil {
		t.Fatalf("offered %+v with nothing published", got)
	}

	page := render(t, dir)
	if strings.Contains(page, "/desktop/SAG-Personal.dmg") {
		t.Error("the page links to an installer that is not there")
	}
	// And it still gives that reader something to do, rather than a dead end.
	if !strings.Contains(page, "Tell me when it is ready") {
		t.Error("the page offers nothing to somebody who cannot download yet")
	}
}

// And when there is one, the page offers it with the version from the MANIFEST
// rather than from the filename, because the published name carries no version
// and a filename is a thing somebody types.
func TestAnInstallerIsOfferedWithTheVersionTheManifestGives(t *testing.T) {
	dir := t.TempDir()
	writeInstaller(t, dir, "SAG-Personal.dmg", 3*1024*1024)
	writeManifest(t, dir, "personal-darwin-x86_64.json", release{
		Version: "0.1.5", Signature: "sig", URL: "/updates/personal/x.tar.gz",
	})

	s := &Server{dir: dir}
	got := s.installerFor("personal")
	if got == nil {
		t.Fatal("nothing offered")
	}
	if got.Version != "0.1.5" {
		t.Errorf("version %q, want 0.1.5", got.Version)
	}
	if got.Size != "3 MB" {
		t.Errorf("size %q, want 3 MB", got.Size)
	}
	if got.URL != "/desktop/SAG-Personal.dmg" {
		t.Errorf("url %q", got.URL)
	}

	page := render(t, dir)
	if !strings.Contains(page, "/desktop/SAG-Personal.dmg") {
		t.Error("the page does not link the installer")
	}
	// Named in the footer, for whoever needs to know which build they have. Not
	// under the download button: to a stranger deciding whether to try this,
	// "0.1.5" reads as "not finished", and the first draft said it twice above
	// the fold.
	if !strings.Contains(page, "Personal 0.1.5") {
		t.Error("the page never names the version it is serving")
	}
}

// An installer with no manifest beside it is still offered, without a version.
// Saying nothing about the version is better than saying a wrong one, and a
// download that works is better than a page that hides it over a missing file.
func TestAnInstallerWithNoManifestIsStillOffered(t *testing.T) {
	dir := t.TempDir()
	writeInstaller(t, dir, "SAG-Personal.dmg", 1024*1024)
	got := (&Server{dir: dir}).installerFor("personal")
	if got == nil {
		t.Fatal("nothing offered")
	}
	if got.Version != "" {
		t.Errorf("invented a version: %q", got.Version)
	}
}

// The file the page links to must actually be downloadable at that address.
//
// This is the same failure the update manifest once had, arrived at from the
// other side: a correct file on disk, served under a path nothing routed. So it
// goes through the whole Handler rather than to a function.
func TestTheInstallerThePageLinksToCanBeDownloaded(t *testing.T) {
	dir := t.TempDir()
	writeInstaller(t, dir, "SAG-Personal.dmg", 2048)

	s, err := New(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()

	page := httptest.NewRecorder()
	asked := httptest.NewRequest(http.MethodGet, "/", nil)
	asked.Host = "sag-repo.example"
	h.ServeHTTP(page, asked)

	link := (&Server{dir: dir}).installerFor("personal").URL
	if !strings.Contains(page.Body.String(), link) {
		t.Fatalf("the page does not link %s", link)
	}

	got := httptest.NewRecorder()
	h.ServeHTTP(got, httptest.NewRequest(http.MethodGet, link, nil))
	if got.Code != http.StatusOK {
		t.Fatalf("%s answered %d, so the page offers a download that does not work", link, got.Code)
	}
	if got.Body.Len() != 2048 {
		t.Fatalf("got %d bytes, want 2048", got.Body.Len())
	}
}

// A path under the installers cannot climb out of the directory.
func TestTheInstallerPathCannotClimbOut(t *testing.T) {
	dir := t.TempDir()
	writeInstaller(t, dir, "SAG-Personal.dmg", 16)
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/desktop/../secret.txt", "/desktop/%2e%2e/secret.txt", "/desktop/"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "no") {
			t.Errorf("%q reached outside the directory", path)
		}
	}
}

// The drawings are in the binary, and an empty embed is a silent way to ship a
// page with a hole where the product is.
func TestThePageCarriesItsIllustrations(t *testing.T) {
	for _, name := range []string{"chat", "console"} {
		if len(art[name]) < 2000 {
			t.Errorf("%s.svg is %d bytes, which is not a drawing", name, len(art[name]))
		}
	}
	page := render(t, t.TempDir())
	if strings.Count(page, "</svg>") < 2 {
		t.Error("the page does not carry both drawings inline")
	}
}

func render(t *testing.T, dir string) string {
	t.Helper()
	s, err := New(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "sag-repo.example"
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("the page answered %d", w.Code)
	}
	return w.Body.String()
}

func writeInstaller(t *testing.T, dir, name string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, desktopDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, desktopDir, name), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

var _ = json.Marshal
