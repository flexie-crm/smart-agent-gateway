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
	if got := s.installerFor("personal", "mac"); got != nil {
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
	got := s.installerFor("personal", "mac")
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
	got := (&Server{dir: dir}).installerFor("personal", "mac")
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

	link := (&Server{dir: dir}).installerFor("personal", "mac").URL
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

// Windows is offered when its installer is on disk, and not before.
//
// The two platforms are separate files at separate fixed names, and the page
// decides each from what is actually there: a Mac build published without a
// Windows one must not turn the Windows button into a link that 404s, which is
// the whole reason this reads the disk instead of a constant.
func TestWindowsIsOfferedOnlyWhenItsInstallerIsThere(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "desktop"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Server{dir: dir}

	if got := s.installerFor("personal", "windows"); got != nil {
		t.Fatalf("offered a Windows download with nothing on disk: %+v", got)
	}

	// A Mac build alone must not make Windows appear: they are different files.
	if err := os.WriteFile(filepath.Join(dir, "desktop", "SAG-Personal.dmg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.installerFor("personal", "windows"); got != nil {
		t.Fatalf("a Mac build made Windows appear: %+v", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "desktop", "SAG-Personal.exe"), make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	got := s.installerFor("personal", "windows")
	if got == nil {
		t.Fatal("the installer is on disk and was not offered")
	}
	if got.URL != "/desktop/SAG-Personal.exe" {
		t.Errorf("URL was %q", got.URL)
	}
	// No Windows manifest is published yet, so it says nothing about a version
	// rather than reading one out of a filename.
	if got.Version != "" {
		t.Errorf("named a version with no manifest to read it from: %q", got.Version)
	}
}

// The page has to say there are TWO products, name both, and say who makes them.
//
// It is tested because it is the kind of thing that reads fine to whoever wrote
// it and is wrong to everybody else: the page sold "Private AI for your company"
// while the only thing anybody could download was the personal edition, so a
// visitor either downloaded the wrong expectation or left.
func TestThePageNamesBothEditionsAndWhoMakesThem(t *testing.T) {
	dir := t.TempDir()
	writeInstaller(t, dir, "SAG-Personal.dmg", 3*1024*1024)
	writeManifest(t, dir, "personal-darwin-x86_64.json", release{Version: "0.1.8"})
	page := render(t, dir)

	for _, want := range []string{
		"Flexie SAG Personal",
		"Flexie SAG Enterprise",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page never names %q", want)
		}
	}

	// Enterprise is a conversation, not a download, and it goes to sales rather
	// than to the company's front page: somebody ready to talk should not have to
	// go and find the address.
	if !strings.Contains(page, "mailto:sales@flexie.io") {
		t.Error("there is no way to reach sales about the enterprise edition")
	}

	// Who makes it, linked. A visitor arriving from a search has no other way to
	// know, and it must not live only in the footer.
	if !strings.Contains(page, "A product by") || !strings.Contains(page, `href="https://flexie.io/"`) {
		t.Error("the page does not say it is a product by Flexie CRM, linked")
	}
	hero := page[:strings.Index(page, `<section id="get">`)]
	if !strings.Contains(hero, "A product by") {
		t.Error("the attribution is below the fold; it belongs where somebody lands")
	}

	// And the positioning: a record-keeping system is not the same as one that
	// acts, which is the whole argument for this product existing.
	if !strings.Contains(hero, "CRM or an ERP") {
		t.Error("the page does not say what this is FOR, next to what a company already runs")
	}
	// And the headline has to name the thing. "Does the work" could be a
	// dishwasher; what a visitor needs in the first line is what this IS and
	// what it reaches.
	if !strings.Contains(hero, "AI agent") {
		t.Error("the headline never says what the product is")
	}
}
