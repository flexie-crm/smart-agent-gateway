package api

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/skill"
)

// Skills over HTTP.
//
// Two things are worth testing at this level and are not testable below it: a
// multipart upload really arriving as a readable archive, and a refusal
// reaching the sender as something they can act on. The package reader has its
// own tests for what it refuses; these are about whether the refusal survives
// the trip.

const testManifest = `---
name: pdf-processing
description: Extract, inspect and transform PDF files.
license: Apache-2.0
metadata:
  title: PDF Toolkit
---

# PDF processing

Read the document first.

## Extracting

Run the script.
`

// packZip builds an archive in memory: path to contents.
func packZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	w := zip.NewWriter(buf)
	for path, body := range files {
		out, err := w.Create(path)
		if err != nil {
			t.Fatalf("pack %s: %v", path, err)
		}
		if _, err := out.Write([]byte(body)); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	return buf.Bytes()
}

func samplePackage(t *testing.T) []byte {
	t.Helper()
	return packZip(t, map[string]string{
		"pdf-processing/" + skill.Manifest:     testManifest,
		"pdf-processing/scripts/extract.py":    "import sys\nprint(sys.argv)\n",
		"pdf-processing/references/formats.md": "# Formats\n\nPDF 1.7.\n",
		"pdf-processing/assets/template.docx":  "PK\x03\x04\x00binary",
	})
}

// updatedPackage is the same skill with a changed file, which is what makes it
// a new VERSION rather than a new skill: the identity is the name, and the
// version is the bytes.
func updatedPackage(t *testing.T, script string) []byte {
	t.Helper()
	return packZip(t, map[string]string{
		"pdf-processing/" + skill.Manifest:  testManifest,
		"pdf-processing/scripts/extract.py": script,
	})
}

// namedPackage is a different skill, for the batches: a skill IS (workspace,
// name), so two archives with one name are two versions of one skill, which is
// not what "several skills at once" means.
func namedPackage(t *testing.T, name string) []byte {
	t.Helper()
	return packZip(t, map[string]string{
		name + "/" + skill.Manifest: fmt.Sprintf(
			"---\nname: %s\ndescription: A skill called %s.\n---\n\n# %s\n\nDo the thing.\n",
			name, name, name),
		name + "/scripts/run.sh": "#!/bin/sh\necho " + name + "\n",
	})
}

// upload posts one archive the way a browser does, as multipart form data.
func (e *testEnv) upload(token, field string, archive []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.uploadAll(token, field, archive)
}

// uploadAll posts several, every one under the same field name, which is how a
// browser sends a multiple file input.
func (e *testEnv) uploadAll(token, field string, archives ...[]byte) *httptest.ResponseRecorder {
	e.t.Helper()
	body := &bytes.Buffer{}
	form := multipart.NewWriter(body)
	for i, archive := range archives {
		// Named per part, so the answer can be checked to be about the right
		// one: the order of the results is the order of the parts.
		part, err := form.CreateFormFile(field, fmt.Sprintf("package-%d.zip", i+1))
		if err != nil {
			e.t.Fatalf("form file: %v", err)
		}
		if _, err := part.Write(archive); err != nil {
			e.t.Fatalf("write the archive: %v", err)
		}
	}
	if err := form.Close(); err != nil {
		e.t.Fatalf("close the form: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/skills", body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func TestAPackageIsImportedThroughTheAPI(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.upload(token, "file", samplePackage(t))
	env.expectStatus(rec, http.StatusOK)

	var body importResponse
	env.decode(rec, &body)
	if len(body.Results) != 1 {
		t.Fatalf("one archive came back as %d results", len(body.Results))
	}
	imported := body.Results[0]
	if imported.Status != app.SkillImported {
		t.Errorf("the first import reported %q, want %q", imported.Status, app.SkillImported)
	}
	// The name the sender used, echoed back: with one archive it is redundant,
	// and with forty it is the only way to say which was refused.
	if imported.File != "package-1.zip" {
		t.Errorf("the result is about %q, not the file that was sent", imported.File)
	}
	if imported.Skill.Name != "pdf-processing" {
		t.Errorf("name = %q", imported.Skill.Name)
	}
	// The name for a person, which the format has no field for: the author put
	// it in metadata, which is where the specification says to put it.
	if imported.Skill.Title != "PDF Toolkit" {
		t.Errorf("title = %q, want the one the author declared", imported.Skill.Title)
	}
	if imported.Skill.Files != 4 {
		t.Errorf("files = %d, want 4", imported.Skill.Files)
	}
	if imported.Skill.Sections == 0 {
		t.Error("nothing was parsed into passages")
	}
	// Who imported it, resolved from the request and frozen into the row. The
	// name comes from the user record, not from the token, because a token
	// carries an id and a history has to carry a name.
	// createUser names a person by their email, so that is the name to expect.
	if imported.Skill.CreatedByName != "admin@acme.test" {
		t.Errorf("created by %q, want the person's own name", imported.Skill.CreatedByName)
	}
	if imported.Skill.CreatedBy == 0 {
		t.Error("the import is not linked to the person who made it")
	}
	if imported.Skill.Version.CreatedByName != imported.Skill.CreatedByName {
		t.Errorf("the version says %q and the skill says %q",
			imported.Skill.Version.CreatedByName, imported.Skill.CreatedByName)
	}

	// The same bytes again are the same version, and the answer says so rather
	// than reporting a successful import that did not happen.
	again := env.upload(token, "file", samplePackage(t))
	env.expectStatus(again, http.StatusOK)
	var secondBody importResponse
	env.decode(again, &secondBody)
	second := secondBody.Results[0]
	if second.Status != app.SkillUnchanged {
		t.Errorf("re-uploading the identical package reported %q, want %q",
			second.Status, app.SkillUnchanged)
	}
	if second.Skill.Versions != 1 {
		t.Errorf("versions = %d after re-uploading the same package", second.Skill.Versions)
	}
}

// The screen in one answer, and the file route that the screen's clicks use.
func TestTheSkillsScreenIsOneRequest(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	env.upload(token, "file", samplePackage(t))

	rec := env.do(http.MethodGet, "/v1/skills/view", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var view app.SkillOverview
	env.decode(rec, &view)

	if len(view.Skills) != 1 || view.Skill == nil {
		t.Fatalf("the view holds %d skills and %v selected", len(view.Skills), view.Skill)
	}
	// The selection comes back RESOLVED: asking for nothing opens the live
	// version of the first skill, so a client never has to work out a default.
	if view.VersionID != view.Skill.ActiveVersionID {
		t.Errorf("the view opened version %d, not the live one (%d)", view.VersionID, view.Skill.ActiveVersionID)
	}
	if len(view.Versions) != 1 {
		t.Errorf("versions = %d", len(view.Versions))
	}
	if len(view.Files) != 4 {
		t.Fatalf("files = %d", len(view.Files))
	}
	if len(view.Sections) == 0 {
		t.Fatal("no passages")
	}
	// The manifest rides on the screen's answer, with its text, because opening
	// it is not a click anybody should have to make: it is the default state of
	// the screen, and the screen is one request.
	if view.Manifest == nil {
		t.Fatal("the view carries no manifest")
	}
	if view.Manifest.Path != skill.Manifest {
		t.Errorf("the manifest is %q", view.Manifest.Path)
	}
	if view.Manifest.Text != testManifest {
		t.Error("the manifest did not arrive exactly as it was uploaded")
	}

	// A file LIST carries no content, and says which files are bytes, so a
	// screen knows what to offer without loading anything.
	var manifestID, assetID int64
	for _, f := range view.Files {
		if f.Text != "" {
			t.Errorf("%s arrived with its content in a listing", f.Path)
		}
		switch f.Path {
		case skill.Manifest:
			manifestID = f.ID
			if f.Binary {
				t.Error("the manifest is reported as bytes")
			}
		case "assets/template.docx":
			assetID = f.ID
			if !f.Binary {
				t.Error("the asset is not reported as bytes")
			}
		}
	}

	// One file, with its text.
	rec = env.do(http.MethodGet, fmt.Sprintf("/v1/skill-files/%d", manifestID), token, nil)
	env.expectStatus(rec, http.StatusOK)
	var manifest model.SkillFile
	env.decode(rec, &manifest)
	if manifest.Text != testManifest {
		t.Error("the manifest did not come back exactly as it was uploaded")
	}

	// The bytes come back as a download, never inline: this origin serves the
	// console, and a package is content somebody else wrote.
	rec = env.do(http.MethodGet, fmt.Sprintf("/v1/skill-files/%d/download", assetID), token, nil)
	env.expectStatus(rec, http.StatusOK)
	if got := rec.Body.String(); got != "PK\x03\x04\x00binary" {
		t.Errorf("the download is %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != "attachment" {
		t.Errorf("Content-Disposition = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want the one that renders nothing", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}

// A refused package has to reach the sender as the reason, on the field they
// can act on. "Import failed" tells somebody with a bad archive nothing.
func TestARefusedPackageSaysWhatIsWrongWithIt(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	tests := []struct {
		name    string
		archive []byte
		says    string
	}{
		{
			name:    "not an archive at all",
			archive: []byte("this is not a zip"),
			says:    "not a zip archive",
		},
		{
			name: "no manifest in it",
			archive: packZip(t, map[string]string{
				"pdf-processing/README.md": "# nothing",
			}),
			says: "no " + skill.Manifest,
		},
		{
			name: "a path climbing out of the package",
			archive: packZip(t, map[string]string{
				"pdf-processing/" + skill.Manifest: testManifest,
				"pdf-processing/../escape.txt":     "out",
			}),
			says: "points outside the package",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.upload(token, "file", tc.archive)
			// The REQUEST was fine: it carried an archive and we read it. What
			// is wrong is the archive, and that is reported per archive,
			// because in a batch of ten it has to be.
			env.expectStatus(rec, http.StatusOK)

			var body importResponse
			env.decode(rec, &body)
			if len(body.Results) != 1 {
				t.Fatalf("one archive came back as %d results", len(body.Results))
			}
			refused := body.Results[0]
			if refused.Status != app.SkillRefused {
				t.Fatalf("status = %q, want %q", refused.Status, app.SkillRefused)
			}
			if !strings.Contains(refused.Reason, tc.says) {
				t.Errorf("the refusal is %q, and does not mention %q", refused.Reason, tc.says)
			}
			if refused.Skill != nil {
				t.Error("a refused archive came back with a skill on it")
			}
		})
	}

	// Nothing was stored by any of it.
	rec := env.do(http.MethodGet, "/v1/skills/view", token, nil)
	var view app.SkillOverview
	env.decode(rec, &view)
	if len(view.Skills) != 0 {
		t.Errorf("a refused import left %d skills behind", len(view.Skills))
	}
}

func TestAnUploadWithNoFileIsRefused(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// The right shape, the wrong field name: a client sending the file under
	// another name must be told to choose one, not handed a server error.
	rec := env.upload(token, "package", samplePackage(t))
	env.expectStatus(rec, http.StatusBadRequest)
}

// Every route states the permission it needs, and importing is not the same
// right as reading.
func TestSkillRoutesAreGatedSeparately(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	admin, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	env.upload(admin, "file", samplePackage(t))

	// Somebody who may look and no more.
	env.createUser("reader@acme.test", "dev-Passw0rd!", model.PermSkillsView)
	reader, _ := env.login("reader@acme.test", "dev-Passw0rd!")

	env.expectStatus(env.do(http.MethodGet, "/v1/skills/view", reader, nil), http.StatusOK)
	env.expectStatus(env.upload(reader, "file", samplePackage(t)), http.StatusForbidden)

	rec := env.do(http.MethodGet, "/v1/skills/view", admin, nil)
	var view app.SkillOverview
	env.decode(rec, &view)

	// The one edit route, carrying everything the skill's form decides: the
	// words, the switch, and which version is live.
	env.expectStatus(env.do(http.MethodPut,
		fmt.Sprintf("/v1/skills/%d", view.Skill.ID), reader,
		map[string]any{
			"status": "disabled", "title": "Theirs", "description": "",
			"version_id": view.VersionID,
		}), http.StatusForbidden)
	env.expectStatus(env.do(http.MethodDelete,
		fmt.Sprintf("/v1/skills/%d", view.Skill.ID), reader, nil), http.StatusForbidden)

	// And the skill is untouched by any of the refusals.
	rec = env.do(http.MethodGet, "/v1/skills/view", admin, nil)
	var after app.SkillOverview
	env.decode(rec, &after)
	if len(after.Skills) != 1 || after.Skill.Status != model.SkillActive {
		t.Errorf("the refused writes changed something: %+v", after.Skill)
	}
}

// Enabling and disabling is the administrator's decision, and only those two:
// draft and archived are states the system moves a skill through.
func TestOnlyTheTwoStatusesAnAdministratorChoosesAreAccepted(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	env.upload(token, "file", samplePackage(t))

	rec := env.do(http.MethodGet, "/v1/skills/view", token, nil)
	var view app.SkillOverview
	env.decode(rec, &view)
	id := view.Skill.ID

	env.expectFields(env.do(http.MethodPut, fmt.Sprintf("/v1/skills/%d", id), token,
		map[string]any{"status": "archived", "title": "", "description": ""}),
		http.StatusBadRequest, "invalid_request",
		map[string]string{"status": "A skill is either active or disabled."})

	rec = env.do(http.MethodPut, fmt.Sprintf("/v1/skills/%d", id), token,
		map[string]any{"status": "disabled", "title": "", "description": ""})
	env.expectStatus(rec, http.StatusOK)
	var disabled model.Skill
	env.decode(rec, &disabled)
	if disabled.Status != model.SkillDisabled {
		t.Errorf("status = %q", disabled.Status)
	}
}

// The form is one request, and rolling back is one of the things it decides.
//
// The two used to be separate routes, so a console saving both had to send two
// and could land one. Here the rename and the rollback go together, and the
// answer carries both: what it is called, and what it is now running.
func TestTheSkillFormSavesTheWordsAndTheVersionTogether(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// Two versions of one skill, so there is an earlier one to go back to.
	env.upload(token, "file", samplePackage(t))
	env.upload(token, "file", updatedPackage(t, "print(2)\n"))

	rec := env.do(http.MethodGet, "/v1/skills/view", token, nil)
	var view app.SkillOverview
	env.decode(rec, &view)
	if len(view.Versions) != 2 {
		t.Fatalf("want two versions to choose between, got %d", len(view.Versions))
	}
	// Versions come back newest first, so the second is the one to roll back to.
	earlier := view.Versions[1].ID
	if earlier == view.Skill.ActiveVersionID {
		t.Fatalf("version %d is already live, so this proves nothing", earlier)
	}

	rec = env.do(http.MethodPut, fmt.Sprintf("/v1/skills/%d", view.Skill.ID), token,
		map[string]any{
			"title":       "Invoice tooling",
			"description": "What we use it for.",
			"status":      model.SkillActive,
			"version_id":  earlier,
		})
	env.expectStatus(rec, http.StatusOK)

	var saved model.Skill
	env.decode(rec, &saved)
	if saved.ActiveVersionID != earlier {
		t.Errorf("live version = %d, want %d: the rollback did not land",
			saved.ActiveVersionID, earlier)
	}
	// And the words are the ones typed, not the ones the earlier package
	// carried: a person who names a skill in the same save keeps their name.
	if saved.Title != "Invoice tooling" {
		t.Errorf("title = %q, want the one that was typed", saved.Title)
	}
	if saved.UpdatedByName == "" {
		t.Error("nothing recorded who changed it")
	}
}

// A batch: what the drop zone sends when somebody drags in a folder of packages.
//
// The behaviour under test is that one bad archive costs only itself. The
// control for it is on the last line: put the good ones on their own and the
// count is the same, so the two that landed landed because they were good and
// not because the batch happened to succeed.
func TestABatchImportsWhatItCanAndSkipsTheRest(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.uploadAll(token, "file",
		namedPackage(t, "pdf-processing"),                           // 1: lands
		[]byte("this is not a zip"),                                 // 2: skipped
		namedPackage(t, "spreadsheet-cleanup"),                      // 3: lands, after the bad one
		namedPackage(t, "pdf-processing"),                           // 4: the same bytes as 1
		packZip(t, map[string]string{"a/README.md": "no manifest"}), // 5: skipped
	)
	env.expectStatus(rec, http.StatusOK)

	var body importResponse
	env.decode(rec, &body)
	if len(body.Results) != 5 {
		t.Fatalf("five archives came back as %d results", len(body.Results))
	}

	// In the order they were sent, which is what lets a person match a refusal
	// to the file on their own disk.
	want := []string{
		app.SkillImported, app.SkillRefused, app.SkillImported,
		app.SkillUnchanged, app.SkillRefused,
	}
	for i, status := range want {
		got := body.Results[i]
		if got.Status != status {
			t.Errorf("result %d is %q, want %q (reason %q)", i+1, got.Status, status, got.Reason)
		}
		if got.File != fmt.Sprintf("package-%d.zip", i+1) {
			t.Errorf("result %d is about %q", i+1, got.File)
		}
		if status == app.SkillRefused && got.Reason == "" {
			t.Errorf("result %d was skipped without saying why", i+1)
		}
		if status != app.SkillRefused && got.Skill == nil {
			t.Errorf("result %d landed without saying what it landed as", i+1)
		}
	}

	// The third archive came AFTER the refusal, so this is the assertion that
	// the loop kept going rather than stopping at the first bad one.
	if body.Results[2].Skill.Name != "spreadsheet-cleanup" {
		t.Errorf("the archive after the refused one imported as %q", body.Results[2].Skill.Name)
	}

	// And the database holds exactly the two, each with one version: the
	// duplicate added nothing, and neither refusal left anything behind.
	view := env.do(http.MethodGet, "/v1/skills/view", token, nil)
	var screen app.SkillOverview
	env.decode(view, &screen)
	if len(screen.Skills) != 2 {
		names := make([]string, 0, len(screen.Skills))
		for _, s := range screen.Skills {
			names = append(names, s.Name)
		}
		t.Fatalf("the batch left %d skills behind: %v", len(screen.Skills), names)
	}
	for _, stored := range screen.Skills {
		if stored.Versions != 1 {
			t.Errorf("%s has %d versions after one import each", stored.Name, stored.Versions)
		}
	}
}

// The ceiling on the count, and the proof that it is refused BEFORE the loop:
// nothing at all is imported, not the first fifty of fifty-one.
func TestAnImportCarriesUpToTheAllowedNumberOfPackages(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	archives := make([][]byte, 0, skill.MaxPackages+1)
	for i := 0; i <= skill.MaxPackages; i++ {
		archives = append(archives, namedPackage(t, fmt.Sprintf("skill-%d", i)))
	}

	rec := env.uploadAll(token, "file", archives...)
	env.expectStatus(rec, http.StatusBadRequest)

	var refusal struct {
		Fields map[string]string `json:"fields"`
	}
	env.decode(rec, &refusal)
	if !strings.Contains(refusal.Fields["file"], strconv.Itoa(skill.MaxPackages)) {
		t.Errorf("the refusal is %q and does not say how many are allowed", refusal.Fields["file"])
	}

	view := env.do(http.MethodGet, "/v1/skills/view", token, nil)
	var screen app.SkillOverview
	env.decode(view, &screen)
	if len(screen.Skills) != 0 {
		t.Errorf("a refused batch imported %d of its packages anyway", len(screen.Skills))
	}

	// The control: one fewer and the same request works, so what was refused
	// was the count and not something else about the request.
	rec = env.uploadAll(token, "file", archives[:skill.MaxPackages]...)
	env.expectStatus(rec, http.StatusOK)
	var body importResponse
	env.decode(rec, &body)
	if len(body.Results) != skill.MaxPackages {
		t.Fatalf("the allowed number came back as %d results", len(body.Results))
	}
	for i, got := range body.Results {
		if got.Status != app.SkillImported {
			t.Fatalf("result %d of a good batch is %q (%s)", i+1, got.Status, got.Reason)
		}
	}
}

// A body over the ceiling is told what happened, rather than being told it was
// not a file at all.
//
// The archives are never built: the body is a multipart prologue followed by a
// stream of zeroes, because the point is the size of the request and nothing
// past the ceiling is ever read.
func TestAnImportOverTheSizeCeilingSaysSo(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	prologue := &bytes.Buffer{}
	form := multipart.NewWriter(prologue)
	if _, err := form.CreateFormFile("file", "enormous.zip"); err != nil {
		t.Fatalf("form file: %v", err)
	}
	// Past the ceiling and its slack, so the reader stops inside the part.
	oversize := io.MultiReader(
		bytes.NewReader(prologue.Bytes()),
		io.LimitReader(zeroes{}, skill.MaxBatchBytes+(2<<20)),
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/skills", oversize)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)

	env.expectStatus(rec, http.StatusBadRequest)
	var refusal struct {
		Fields map[string]string `json:"fields"`
	}
	env.decode(rec, &refusal)
	said := refusal.Fields["file"]
	if !strings.Contains(said, strconv.Itoa(skill.MaxBatchBytes>>20)) {
		t.Errorf("the refusal is %q and does not say what the ceiling is", said)
	}
	// The control: the generic message is what this test exists to rule out.
	if strings.Contains(said, "not an uploaded file") {
		t.Errorf("an oversize import was reported as not being a file: %q", said)
	}
}

// zeroes is an endless reader, used to make a request large without making one
// large in memory.
type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// Editing a file is a new version, not a change to one, and it is not live
// until somebody publishes it.
func TestEditingAFileWritesADraftNobodyIsUsingYet(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	env.upload(token, "file", samplePackage(t))

	rec := env.do(http.MethodGet, "/v1/skills/view", token, nil)
	var view app.SkillOverview
	env.decode(rec, &view)
	live := view.VersionID

	script := fileNamed(t, view.Files, "scripts/extract.py")
	rec = env.do(http.MethodPost, fmt.Sprintf("/v1/skills/%d/versions", view.Skill.ID), token,
		map[string]any{
			"from":  live,
			"edits": []map[string]any{{"path": script.Path, "text": "import sys\n\nprint('edited')\n"}},
		})
	env.expectStatus(rec, http.StatusCreated)
	var saved struct {
		Version model.SkillVersion `json:"version"`
		Changed []string           `json:"changed"`
	}
	env.decode(rec, &saved)
	if saved.Version.Status != model.SkillVersionDraft {
		t.Errorf("status = %q, want a draft", saved.Version.Status)
	}
	if saved.Version.Number != 2 || saved.Version.ParentVersionID != live {
		t.Errorf("version %d with parent %d, want 2 from %d",
			saved.Version.Number, saved.Version.ParentVersionID, live)
	}
	if len(saved.Changed) != 1 || saved.Changed[0] != "scripts/extract.py" {
		t.Errorf("changed = %v", saved.Changed)
	}

	// The screen still shows the live version, and the agent still gets the
	// script it was getting.
	rec = env.do(http.MethodGet, "/v1/skills/view", token, nil)
	var after app.SkillOverview
	env.decode(rec, &after)
	if after.Skill.ActiveVersionID != live {
		t.Fatalf("the skill moved to %d, want %d", after.Skill.ActiveVersionID, live)
	}
	if len(after.Versions) != 2 {
		t.Fatalf("%d versions, want two", len(after.Versions))
	}

	// Published through the one route that makes a version live.
	env.expectStatus(env.do(http.MethodPut, fmt.Sprintf("/v1/skills/%d", view.Skill.ID), token,
		map[string]any{
			"status": "active", "title": after.Skill.Title, "description": after.Skill.Description,
			"version_id": saved.Version.ID,
		}), http.StatusOK)

	rec = env.do(http.MethodGet, "/v1/skills/view", token, nil)
	var published app.SkillOverview
	env.decode(rec, &published)
	if published.Skill.ActiveVersionID != saved.Version.ID {
		t.Fatalf("after publishing, the skill points at %d, want %d",
			published.Skill.ActiveVersionID, saved.Version.ID)
	}
	// And the file an agent would now be handed is the edited one.
	edited := fileNamed(t, published.Files, "scripts/extract.py")
	rec = env.do(http.MethodGet, fmt.Sprintf("/v1/skill-files/%d", edited.ID), token, nil)
	var whole model.SkillFile
	env.decode(rec, &whole)
	if !strings.Contains(whole.Text, "edited") {
		t.Errorf("the published script says %q", whole.Text)
	}
}

// A draft is withdrawn, and withdrawing it leaves the live version alone.
func TestADraftCanBeDiscarded(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	env.upload(token, "file", samplePackage(t))

	rec := env.do(http.MethodGet, "/v1/skills/view", token, nil)
	var view app.SkillOverview
	env.decode(rec, &view)

	script := fileNamed(t, view.Files, "scripts/extract.py")
	rec = env.do(http.MethodPost, fmt.Sprintf("/v1/skills/%d/versions", view.Skill.ID), token,
		map[string]any{"edits": []map[string]any{{"path": script.Path, "text": "print('no')\n"}}})
	env.expectStatus(rec, http.StatusCreated)
	var saved struct {
		Version model.SkillVersion `json:"version"`
	}
	env.decode(rec, &saved)

	env.expectStatus(env.do(http.MethodDelete,
		fmt.Sprintf("/v1/skills/%d/versions/%d", view.Skill.ID, saved.Version.ID), token, nil),
		http.StatusNoContent)
	// The live version is untouched, and the draft is not offered again.
	rec = env.do(http.MethodGet, "/v1/skills/view", token, nil)
	var after app.SkillOverview
	env.decode(rec, &after)
	if after.Skill.ActiveVersionID != view.VersionID {
		t.Errorf("the live version moved to %d", after.Skill.ActiveVersionID)
	}
	for _, v := range after.Versions {
		if v.ID == saved.Version.ID && v.Status != model.SkillVersionRejected {
			t.Errorf("the discarded draft is %q", v.Status)
		}
	}
	// And the live version cannot be discarded through the same route.
	env.expectStatus(env.do(http.MethodDelete,
		fmt.Sprintf("/v1/skills/%d/versions/%d", view.Skill.ID, view.VersionID), token, nil),
		http.StatusNotFound)
}

// Writing a version is create; choosing which one is live is edit. Somebody
// with one and not the other can do exactly their half.
func TestWritingAVersionAndPublishingItAreDifferentRights(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	admin, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	env.upload(admin, "file", samplePackage(t))

	rec := env.do(http.MethodGet, "/v1/skills/view", admin, nil)
	var view app.SkillOverview
	env.decode(rec, &view)
	script := fileNamed(t, view.Files, "scripts/extract.py")

	// An author: may write a version, may not choose which one is live.
	env.createUser("author@acme.test", "dev-Passw0rd!", model.PermSkillsView, model.PermSkillsCreate)
	author, _ := env.login("author@acme.test", "dev-Passw0rd!")
	rec = env.do(http.MethodPost, fmt.Sprintf("/v1/skills/%d/versions", view.Skill.ID), author,
		map[string]any{"edits": []map[string]any{{"path": script.Path, "text": "print('theirs')\n"}}})
	env.expectStatus(rec, http.StatusCreated)
	var saved struct {
		Version model.SkillVersion `json:"version"`
	}
	env.decode(rec, &saved)
	env.expectStatus(env.do(http.MethodPut, fmt.Sprintf("/v1/skills/%d", view.Skill.ID), author,
		map[string]any{
			"status": "active", "title": view.Skill.Title, "description": view.Skill.Description,
			"version_id": saved.Version.ID,
		}), http.StatusForbidden)

	// A publisher: may choose which one is live, may not write one.
	env.createUser("publisher@acme.test", "dev-Passw0rd!", model.PermSkillsView, model.PermSkillsEdit)
	publisher, _ := env.login("publisher@acme.test", "dev-Passw0rd!")
	env.expectStatus(env.do(http.MethodPost, fmt.Sprintf("/v1/skills/%d/versions", view.Skill.ID),
		publisher, map[string]any{
			"edits": []map[string]any{{"path": script.Path, "text": "print('nope')\n"}},
		}), http.StatusForbidden)
	env.expectStatus(env.do(http.MethodPut, fmt.Sprintf("/v1/skills/%d", view.Skill.ID), publisher,
		map[string]any{
			"status": "active", "title": view.Skill.Title, "description": view.Skill.Description,
			"version_id": saved.Version.ID,
		}), http.StatusOK)
}

// An edit that the format refuses says which file and why, in the body, the
// same as a refused import.
func TestARefusedEditSaysWhy(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	env.upload(token, "file", samplePackage(t))

	rec := env.do(http.MethodGet, "/v1/skills/view", token, nil)
	var view app.SkillOverview
	env.decode(rec, &view)
	script := fileNamed(t, view.Files, "scripts/extract.py")

	for _, one := range []struct {
		name     string
		edits    []map[string]any
		mentions string
	}{
		{"a file that is not there", []map[string]any{
			{"path": "scripts/invented.py", "text": "print(1)\n"}}, "not in this version"},
		{"nothing at all", nil, "edited"},
		{"the same content", []map[string]any{
			{"path": script.Path, "text": scriptText(t, env, token, script.ID)}}, "nothing changed"},
	} {
		t.Run(one.name, func(t *testing.T) {
			body := map[string]any{"from": view.VersionID}
			if one.edits != nil {
				body["edits"] = one.edits
			}
			rec := env.do(http.MethodPost,
				fmt.Sprintf("/v1/skills/%d/versions", view.Skill.ID), token, body)
			env.expectStatus(rec, http.StatusBadRequest)
			// The reason rides on the `edits` field, which is where the form
			// shows it, and it names the problem rather than saying "invalid".
			var refused errorBody
			env.decode(rec, &refused)
			if !strings.Contains(refused.Fields["edits"], one.mentions) {
				t.Errorf("the reason does not mention %q: %s", one.mentions, rec.Body.String())
			}
		})
	}
	// And nothing was written by any of them.
	rec = env.do(http.MethodGet, "/v1/skills/view", token, nil)
	var after app.SkillOverview
	env.decode(rec, &after)
	if len(after.Versions) != 1 {
		t.Errorf("%d versions, want the one", len(after.Versions))
	}
}

func fileNamed(t *testing.T, files []*model.SkillFile, path string) *model.SkillFile {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("%s is not in the version", path)
	return nil
}

func scriptText(t *testing.T, env *testEnv, token string, fileID int64) string {
	t.Helper()
	rec := env.do(http.MethodGet, fmt.Sprintf("/v1/skill-files/%d", fileID), token, nil)
	var whole model.SkillFile
	env.decode(rec, &whole)
	return whole.Text
}

// An edit is applied to the version it was WRITTEN against, not to whatever is
// live when it arrives.
//
// Somebody opens version 1, starts editing, and while they are typing a
// colleague imports version 2. Their save must produce version 1 plus their
// change, which is a version they can look at and decide about, rather than
// version 2 with their change silently merged into it: the file they did not
// touch would then hold somebody else's edit and nobody would be told.
//
// It needs two versions whose files DIFFER to be able to come out the other
// way. With one version, or with two identical ones, ignoring `from` entirely
// makes no observable difference and the test proves nothing.
func TestAnEditIsAppliedToTheVersionItWasWrittenAgainst(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	env.upload(token, "file", samplePackage(t))
	rec := env.do(http.MethodGet, "/v1/skills/view", token, nil)
	var first app.SkillOverview
	env.decode(rec, &first)
	older := first.VersionID

	// A second version, live, whose script says something else.
	env.upload(token, "file", updatedPackage(t, "print('from version two')\n"))
	rec = env.do(http.MethodGet, "/v1/skills/view", token, nil)
	var second app.SkillOverview
	env.decode(rec, &second)
	if second.VersionID == older {
		t.Fatal("the second import did not become the live version")
	}

	// Edit the OLDER version, changing only its manifest.
	rec = env.do(http.MethodPost, fmt.Sprintf("/v1/skills/%d/versions", second.Skill.ID), token,
		map[string]any{
			"from": older,
			"edits": []map[string]any{{
				"path": skill.Manifest,
				"text": strings.Replace(testManifest, "# PDF processing",
					"# PDF processing, edited from version one", 1),
			}},
		})
	env.expectStatus(rec, http.StatusCreated)
	var saved struct {
		Version model.SkillVersion `json:"version"`
	}
	env.decode(rec, &saved)
	if saved.Version.ParentVersionID != older {
		t.Errorf("parent = %d, want the version it was written against (%d)",
			saved.Version.ParentVersionID, older)
	}

	// The proof: the draft's script is version ONE's, because that is what was
	// being edited. Version two's file was never in this package.
	rec = env.do(http.MethodGet,
		fmt.Sprintf("/v1/skills/view?skill=%d&version=%d", second.Skill.ID, saved.Version.ID),
		token, nil)
	var draft app.SkillOverview
	env.decode(rec, &draft)
	script := fileNamed(t, draft.Files, "scripts/extract.py")
	rec = env.do(http.MethodGet, fmt.Sprintf("/v1/skill-files/%d", script.ID), token, nil)
	var whole model.SkillFile
	env.decode(rec, &whole)
	if strings.Contains(whole.Text, "from version two") {
		t.Errorf("the draft carries version two's script, so the edit was applied to the live version:\n%s",
			whole.Text)
	}
	if !strings.Contains(whole.Text, "print(sys.argv)") {
		t.Errorf("the draft's script is neither version's:\n%s", whole.Text)
	}
	// And version one also had two files version two does not, which came with it.
	if len(draft.Files) != 4 {
		t.Errorf("the draft holds %d files, want version one's four", len(draft.Files))
	}
}
