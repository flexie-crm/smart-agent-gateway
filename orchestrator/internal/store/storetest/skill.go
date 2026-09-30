package storetest

import (
	"errors"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
)

// Skills: an imported package, kept whole and versioned.
//
// The two rules this suite exists to hold are both about what must NOT happen.
// A version is immutable, so an update may not touch the files of the version
// before it. And one version is live, recorded in two places, so no path may
// leave the pointer and the statuses disagreeing.
//
// The packages here are built by hand rather than read out of a zip: what the
// store does with a package is a different question from whether an archive was
// read correctly (internal/skill tests that), and a store test that needed a
// fixture archive would fail for two unrelated reasons.

func samplePackage(name, script string) *model.SkillPackage {
	return &model.SkillPackage{
		Name: name,
		// The name for a person, which the format has no field for: it is read
		// out of the package (internal/skill) and stored beside the handle.
		Title:       "PDF processing",
		Description: "Extract, inspect and transform PDF files.",
		// The hash stands in for the package's bytes. The store never computes
		// it, it only compares it, which is exactly what makes a re-import
		// idempotent.
		SHA256: "sha-" + name + "-" + script,
		Files: []model.PackageFile{
			{
				Path: "SKILL.md", FileType: model.SkillFileSkill,
				MIMEType: "text/markdown; charset=utf-8",
				Text:     "---\nname: " + name + "\n---\n\n# Title\n\nDo the thing.\n\n## Deeper\n\nCarefully.\n",
				Size:     72, SHA256: "sha-manifest",
				Sections: []model.PackageSection{
					{Heading: "", Path: "", Body: "---\nname: " + name + "\n---\n", LineStart: 1, LineEnd: 3, SHA256: "s0"},
					{Heading: "Title", Path: "Title", Body: "# Title\n\nDo the thing.\n", LineStart: 4, LineEnd: 7, SHA256: "s1"},
					{Heading: "Deeper", Path: "Title > Deeper", Body: "## Deeper\n\nCarefully.\n", LineStart: 8, LineEnd: 10, SHA256: "s2"},
				},
			},
			{
				Path: "scripts/extract.py", FileType: model.SkillFileScript,
				MIMEType: "text/x-python", Text: script,
				Size: int64(len(script)), SHA256: "sha-script-" + script,
				Sections: []model.PackageSection{
					{Heading: "scripts/extract.py", Path: "scripts/extract.py", Body: script, LineStart: 1, LineEnd: 1, SHA256: "s3"},
				},
			},
			{
				// A binary asset: bytes, no text, and deliberately no sections.
				Path: "assets/template.docx", FileType: model.SkillFileAsset,
				MIMEType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
				Bytes:    []byte{0x50, 0x4b, 0x03, 0x04, 0x00},
				Size:     5, SHA256: "sha-asset",
			},
		},
	}
}

// importer is whoever the tests import as, unless a test cares who.
func importer(t *testing.T, st store.Store, wsID int64, email string) model.Actor {
	t.Helper()
	user := mustUser(t, st, wsID, email)
	return model.Actor{UserID: user.ID, Name: user.Name}
}

// makeLive rolls a skill back and changes nothing else.
//
// The form saves the words and the live version together, so a test about the
// rollback still has to say what the words are. Saying "whatever they already
// were" in one place beats repeating them at four call sites, where one would
// eventually be wrong and the test would assert a rename it never meant.
func makeLive(st store.Store, wsID int64, skill *model.Skill, versionID int64) error {
	// Read the row first, exactly as a form does when it opens: what it sends
	// back for the fields nobody touched has to be what is stored NOW, not what
	// this test happened to capture several writes ago.
	current, err := st.Skills().Skill(ctx(), wsID, skill.ID)
	if err != nil {
		return err
	}
	_, err = st.Skills().Update(ctx(), wsID, skill.ID, store.SkillUpdate{
		Title:       current.Title,
		Description: current.Description,
		Status:      current.Status,
		VersionID:   versionID,
	}, model.Nobody())
	return err
}

// setStatus flips the switch and nothing else.
//
// A skill's title, description and status are saved by one call, because they
// are one form. A test about the switch still has to say what the other two
// are, and saying "whatever they already were" in one place beats repeating
// them at every call site, where one of them would eventually be wrong and the
// test would be asserting a rename it did not mean to make.
func setStatus(st store.Store, wsID int64, skill *model.Skill, status string) (*model.Skill, error) {
	return st.Skills().Update(ctx(), wsID, skill.ID, store.SkillUpdate{
		Title:       skill.Title,
		Description: skill.Description,
		Status:      status,
	}, model.Nobody())
}

func mustImport(t *testing.T, st store.Store, wsID int64, pkg *model.SkillPackage) (*model.Skill, bool) {
	t.Helper()
	return mustImportAs(t, st, wsID, pkg, model.Nobody())
}

func mustImportAs(t *testing.T, st store.Store, wsID int64, pkg *model.SkillPackage, by model.Actor) (*model.Skill, bool) {
	t.Helper()
	skill, added, err := st.Skills().Import(ctx(), wsID, pkg, by)
	if err != nil {
		t.Fatalf("import skill: %v", err)
	}
	return skill, added
}

func testSkillImport(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	skill, added := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))

	if !added {
		t.Error("the first import did not report that it wrote a version")
	}
	if skill.Name != "pdf-processing" {
		t.Errorf("name = %q", skill.Name)
	}
	// The handle and the name for a person are both kept, and Label is the one
	// thing every caller reads so a screen and a prompt cannot disagree.
	if skill.Title != "PDF processing" {
		t.Errorf("title = %q", skill.Title)
	}
	if skill.Label() != "PDF processing" {
		t.Errorf("label = %q, want the title", skill.Label())
	}
	// An imported skill is ready to use. A skill that imported into 'draft'
	// would be a skill nothing can reach for a reason nobody stated.
	if skill.Status != model.SkillActive {
		t.Errorf("status = %q, want %q", skill.Status, model.SkillActive)
	}
	if skill.Version == nil {
		t.Fatal("the skill has no active version")
	}
	if skill.Version.Number != 1 || skill.Version.Status != model.SkillVersionActive {
		t.Errorf("version %d is %q, want 1 and %q", skill.Version.Number, skill.Version.Status, model.SkillVersionActive)
	}
	if skill.Version.Source != model.SkillSourceImported {
		t.Errorf("source = %q", skill.Version.Source)
	}
	// The pointer and the status are two records of one fact, and this is the
	// assertion that they were written together.
	if skill.ActiveVersionID != skill.Version.ID {
		t.Errorf("the skill points at %d and its active version is %d", skill.ActiveVersionID, skill.Version.ID)
	}
	if skill.Version.ParentVersionID != 0 {
		t.Errorf("a first version has a parent: %d", skill.Version.ParentVersionID)
	}
	// Imported versions are validated by the format check they passed; what they
	// do not need is execution evidence.
	if skill.Version.ValidatedAt == nil {
		t.Error("an imported version was not recorded as validated")
	}
	if skill.Version.ActivatedAt == nil {
		t.Error("the live version has no activation time")
	}

	if skill.Files != 3 || skill.Sections != 4 {
		t.Errorf("counts = %d files, %d sections; want 3 and 4", skill.Files, skill.Sections)
	}

	files, err := st.Skills().Files(ctx(), ws.ID, skill.Version.ID)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("files = %d", len(files))
	}
	// Every file says which skill it belongs to, not only which version, which
	// is what takes a join out of every scoped read.
	for _, f := range files {
		if f.SkillID != skill.ID {
			t.Errorf("%s belongs to skill %d, want %d", f.Path, f.SkillID, skill.ID)
		}
	}
	// Ordered by path, so a tree is drawn without the client sorting it.
	if files[0].Path != "SKILL.md" || files[1].Path != "assets/template.docx" || files[2].Path != "scripts/extract.py" {
		t.Errorf("files are not in path order: %v", pathsOf(files))
	}

	byPath := map[string]*model.SkillFile{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	// Which column holds the content is a fact about the row, and a LISTING has
	// to carry it: a screen with no content loaded still has to know whether to
	// offer a file to read or a file to download.
	if byPath["SKILL.md"].Binary {
		t.Error("the manifest is stored as bytes")
	}
	if !byPath["assets/template.docx"].Binary {
		t.Error("the asset is not stored as bytes")
	}
	if got := byPath["assets/template.docx"].Sections; got != 0 {
		t.Errorf("the binary asset has %d sections, want none", got)
	}
	if got := byPath["SKILL.md"].Sections; got != 3 {
		t.Errorf("the manifest has %d sections, want 3", got)
	}

	// Content is read one file at a time, and comes back as it went in.
	manifest, err := st.Skills().File(ctx(), ws.ID, byPath["SKILL.md"].ID)
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}
	if manifest.Text == "" || manifest.Bytes != nil {
		t.Errorf("the manifest came back as bytes: text=%q bytes=%v", manifest.Text, manifest.Bytes)
	}
	if manifest.Text != samplePackage("pdf-processing", "print(1)").Files[0].Text {
		t.Error("the manifest did not come back byte for byte")
	}
	asset, err := st.Skills().File(ctx(), ws.ID, byPath["assets/template.docx"].ID)
	if err != nil {
		t.Fatalf("read the asset: %v", err)
	}
	if string(asset.Bytes) != "PK\x03\x04\x00" {
		t.Errorf("the asset's bytes came back as %q", asset.Bytes)
	}
	if asset.Text != "" {
		t.Errorf("the asset has text: %q", asset.Text)
	}

	sections, err := st.Skills().Sections(ctx(), ws.ID, skill.Version.ID)
	if err != nil {
		t.Fatalf("list sections: %v", err)
	}
	if len(sections) != 4 {
		t.Fatalf("sections = %d", len(sections))
	}
	// One sequence across the whole version, in order, because that is what the
	// unique key says it is.
	for i, section := range sections {
		if section.Sequence != i+1 {
			t.Errorf("section %d has sequence %d", i, section.Sequence)
		}
		if section.File == "" {
			t.Errorf("section %d does not say which file it came from", i)
		}
		if section.SizeBytes == 0 {
			t.Errorf("section %d reports no size", i)
		}
		if section.SkillID != skill.ID {
			t.Errorf("section %d belongs to skill %d, want %d", i, section.SkillID, skill.ID)
		}
	}
	if sections[2].Path != "Title > Deeper" {
		t.Errorf("the heading path was not kept: %q", sections[2].Path)
	}
	if sections[1].LineStart != 4 || sections[1].LineEnd != 7 {
		t.Errorf("line range = %d-%d, want 4-7", sections[1].LineStart, sections[1].LineEnd)
	}
}

func testSkillImportIsIdempotent(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	pkg := samplePackage("pdf-processing", "print(1)")

	first, added := mustImport(t, st, ws.ID, pkg)
	if !added {
		t.Fatal("the first import wrote nothing")
	}
	again, addedAgain := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))

	// The same bytes are the same version. Uploading a file twice is not a
	// change, and a version history that grows on a double click is a history
	// of clicks rather than of the skill.
	if addedAgain {
		t.Error("re-importing the identical package reported a new version")
	}
	if again.Versions != 1 {
		t.Errorf("versions = %d after re-importing the same package, want 1", again.Versions)
	}
	if again.Version.ID != first.Version.ID {
		t.Errorf("the active version changed: %d then %d", first.Version.ID, again.Version.ID)
	}

	versions, err := st.Skills().Versions(ctx(), ws.ID, again.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d", len(versions))
	}
}

func testSkillUpdateAddsAVersion(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	first, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))
	firstFiles, err := st.Skills().Files(ctx(), ws.ID, first.Version.ID)
	if err != nil {
		t.Fatalf("list the first version's files: %v", err)
	}

	second, added := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(2)"))
	if !added {
		t.Fatal("a changed package did not write a version")
	}
	if second.ID != first.ID {
		t.Errorf("the update made a second skill: %d then %d", first.ID, second.ID)
	}
	if second.Version.Number != 2 {
		t.Errorf("version number = %d, want 2", second.Version.Number)
	}
	// The lineage: what this version was built from, which is what a rollback
	// follows.
	if second.Version.ParentVersionID != first.Version.ID {
		t.Errorf("parent = %d, want the version it superseded (%d)", second.Version.ParentVersionID, first.Version.ID)
	}
	if second.ActiveVersionID != second.Version.ID {
		t.Error("the skill does not point at its new version")
	}

	versions, err := st.Skills().Versions(ctx(), ws.ID, second.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
	// Newest first, and exactly one of them is live.
	if versions[0].Number != 2 || versions[0].Status != model.SkillVersionActive {
		t.Errorf("version %d is %q, want 2 active", versions[0].Number, versions[0].Status)
	}
	if versions[1].Number != 1 || versions[1].Status != model.SkillVersionArchived {
		t.Errorf("version %d is %q, want 1 archived", versions[1].Number, versions[1].Status)
	}

	// A VERSION IS IMMUTABLE. The version that was live still has its own files,
	// unchanged, which is the whole reason "which version answered that" is a
	// question with an answer.
	stillThere, err := st.Skills().Files(ctx(), ws.ID, first.Version.ID)
	if err != nil {
		t.Fatalf("list the first version's files again: %v", err)
	}
	if len(stillThere) != len(firstFiles) {
		t.Fatalf("the first version has %d files, had %d", len(stillThere), len(firstFiles))
	}
	old, err := st.Skills().File(ctx(), ws.ID, fileAt(t, stillThere, "scripts/extract.py").ID)
	if err != nil {
		t.Fatalf("read the old script: %v", err)
	}
	if old.Text != "print(1)" {
		t.Errorf("the old version's script is now %q; an update overwrote it", old.Text)
	}
	fresh, err := st.Skills().File(ctx(), ws.ID, fileAt(t, mustFiles(t, st, ws.ID, second.Version.ID), "scripts/extract.py").ID)
	if err != nil {
		t.Fatalf("read the new script: %v", err)
	}
	if fresh.Text != "print(2)" {
		t.Errorf("the new version's script is %q", fresh.Text)
	}
}

// A package with no title of its own is printed by its handle, and the row says
// honestly that it carried none.
func testSkillWithNoTitleIsCalledByItsHandle(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	pkg := samplePackage("pdf-processing", "print(1)")
	pkg.Title = ""

	skill, _ := mustImport(t, st, ws.ID, pkg)
	if skill.Title != "" {
		t.Errorf("title = %q, want it empty rather than filled in", skill.Title)
	}
	if skill.Label() != "pdf-processing" {
		t.Errorf("label = %q, want the handle", skill.Label())
	}
}

// The title travels with the package, so an update brings the new one. Unlike
// the status, which is the administrator's.
func testSkillTitleFollowsThePackage(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))

	renamed := samplePackage("pdf-processing", "print(2)")
	renamed.Title = "PDF Toolkit"
	updated, added := mustImport(t, st, ws.ID, renamed)

	if !added {
		t.Fatal("the update wrote nothing")
	}
	if updated.Title != "PDF Toolkit" {
		t.Errorf("title = %q, want the new one", updated.Title)
	}
}

// What a skill is CALLED is the live version's, not the last import's.
//
// This is a real defect, found by clicking: the title and the description sat on
// the skill and were written by whatever was imported last, so importing a
// package and then rolling back left the skill running one version and
// describing another. They live on the version now, and the skill reads them
// through the join to whichever one is live.
func testSkillNameFollowsTheLiveVersion(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")

	first := samplePackage("pdf-processing", "print(1)")
	first.Title, first.Description = "The first name", "What version one did."
	one, _ := mustImport(t, st, ws.ID, first)
	firstVersionID := one.Version.ID

	second := samplePackage("pdf-processing", "print(2)")
	second.Title, second.Description = "The second name", "What version two does."
	two, _ := mustImport(t, st, ws.ID, second)

	if two.Title != "The second name" || two.Description != "What version two does." {
		t.Fatalf("after the update the skill is %q / %q", two.Title, two.Description)
	}

	// Back to the first version. The name and the description have to come back
	// with it: they describe what is running.
	if err := makeLive(st, ws.ID, two, firstVersionID); err != nil {
		t.Fatalf("activate the earlier version: %v", err)
	}
	back, err := st.Skills().Skill(ctx(), ws.ID, two.ID)
	if err != nil {
		t.Fatalf("read the skill: %v", err)
	}
	if back.Title != "The first name" {
		t.Errorf("title = %q, want the live version's (%q)", back.Title, "The first name")
	}
	if back.Description != "What version one did." {
		t.Errorf("description = %q, want the live version's", back.Description)
	}
	if back.Label() != "The first name" {
		t.Errorf("label = %q", back.Label())
	}

	// And every version still carries its own, so a picker can say what it is
	// offering to switch to.
	versions, err := st.Skills().Versions(ctx(), ws.ID, two.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("versions = %d", len(versions))
	}
	if versions[0].Title != "The second name" || versions[1].Title != "The first name" {
		t.Errorf("the versions report %q and %q", versions[0].Title, versions[1].Title)
	}
}

// Who made the skill, who made each version, and what happens when they leave.
//
// The pair exists for the case an agent makes real: a person imports a skill and
// something else improves it, and the history has to be able to say both. The
// frozen name is the half that matters, because an id can be deleted and a
// history that rewrites itself when somebody leaves is not a history.
func testSkillRecordsWhoMadeItAndEachVersion(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	person := importer(t, st, ws.ID, "importer@acme.test")

	skill, _ := mustImportAs(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"), person)
	if skill.CreatedBy != person.UserID || skill.CreatedByName != person.Name {
		t.Fatalf("created by %d/%q, want %d/%q", skill.CreatedBy, skill.CreatedByName, person.UserID, person.Name)
	}
	if skill.Version.CreatedBy != person.UserID || skill.Version.CreatedByName != person.Name {
		t.Errorf("the version says %d/%q", skill.Version.CreatedBy, skill.Version.CreatedByName)
	}

	// Something that is not a person improves it, which is what the shape is
	// for: an agent has no user row, so the id is nothing and the name is what
	// should be printed.
	agent := model.Actor{Name: "Research agent"}
	updated, added := mustImportAs(t, st, ws.ID, samplePackage("pdf-processing", "print(2)"), agent)
	if !added {
		t.Fatal("the update wrote nothing")
	}
	// Who CREATED the skill does not change. Crediting the agent because it
	// wrote the newest version would lose who brought the skill here.
	if updated.CreatedBy != person.UserID || updated.CreatedByName != person.Name {
		t.Errorf("the creator changed to %d/%q", updated.CreatedBy, updated.CreatedByName)
	}
	// And the version says who wrote THAT one, which is the question a version
	// history exists to answer.
	if updated.Version.CreatedBy != 0 || updated.Version.CreatedByName != "Research agent" {
		t.Errorf("version 2 says %d/%q wrote it", updated.Version.CreatedBy, updated.Version.CreatedByName)
	}
	versions, err := st.Skills().Versions(ctx(), ws.ID, updated.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if versions[0].CreatedByName != "Research agent" || versions[1].CreatedByName != person.Name {
		t.Errorf("the history reads %q then %q", versions[0].CreatedByName, versions[1].CreatedByName)
	}

	// The person leaves. Their id goes, their name stays: this is why there are
	// two columns and not one.
	if err := st.Users().Delete(ctx(), person.UserID); err != nil {
		t.Fatalf("delete the person: %v", err)
	}
	after, err := st.Skills().Skill(ctx(), ws.ID, updated.ID)
	if err != nil {
		t.Fatalf("read the skill: %v", err)
	}
	if after.CreatedBy != 0 {
		t.Errorf("the deleted person is still linked: %d", after.CreatedBy)
	}
	if after.CreatedByName != person.Name {
		t.Errorf("the record of who created it went with them: %q", after.CreatedByName)
	}
	history, err := st.Skills().Versions(ctx(), ws.ID, updated.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if history[1].CreatedBy != 0 || history[1].CreatedByName != person.Name {
		t.Errorf("version 1 now reads %d/%q", history[1].CreatedBy, history[1].CreatedByName)
	}
}

func testSkillKeepsItsStatusWhenReimported(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	skill, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))

	if _, err := setStatus(st, ws.ID, skill, model.SkillDisabled); err != nil {
		t.Fatalf("disable the skill: %v", err)
	}
	updated, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(2)"))

	// Somebody switched this off on purpose. Importing an update is not a
	// request to switch it back on, and quietly doing so would put a skill back
	// in front of an agent that was deliberately taken away from it.
	if updated.Status != model.SkillDisabled {
		t.Errorf("status = %q after re-import, want it left %q", updated.Status, model.SkillDisabled)
	}
	if updated.Version.Number != 2 {
		t.Errorf("the update did not land: version %d", updated.Version.Number)
	}
}

func testSkillActivateIsHowARollbackWorks(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	first, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))
	firstVersionID := first.Version.ID
	second, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(2)"))

	if err := makeLive(st, ws.ID, second, firstVersionID); err != nil {
		t.Fatalf("activate the earlier version: %v", err)
	}

	back, err := st.Skills().Skill(ctx(), ws.ID, second.ID)
	if err != nil {
		t.Fatalf("read the skill: %v", err)
	}
	if back.ActiveVersionID != firstVersionID {
		t.Errorf("the skill points at %d, want %d", back.ActiveVersionID, firstVersionID)
	}
	if back.Version.Number != 1 || back.Version.Status != model.SkillVersionActive {
		t.Errorf("live version is %d/%q, want 1 active", back.Version.Number, back.Version.Status)
	}
	// And the one that WAS live is archived, so exactly one version claims to be
	// live however many times this is done.
	versions, err := st.Skills().Versions(ctx(), ws.ID, second.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	live := 0
	for _, v := range versions {
		if v.Status == model.SkillVersionActive {
			live++
		}
	}
	if live != 1 {
		t.Errorf("%d versions claim to be live", live)
	}
}

func testSkillActivateRefusesAnotherSkillsVersion(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	mine, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))
	theirs, _ := mustImport(t, st, ws.ID, samplePackage("csv-tools", "print(1)"))

	// A version id is not a capability. Activating another skill's version would
	// point a skill at files that are not its own.
	err := makeLive(st, ws.ID, mine, theirs.Version.ID)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("activating another skill's version = %v, want ErrNotFound", err)
	}
	unchanged, err := st.Skills().Skill(ctx(), ws.ID, mine.ID)
	if err != nil {
		t.Fatalf("read the skill: %v", err)
	}
	if unchanged.ActiveVersionID != mine.Version.ID {
		t.Error("the refused activation moved the pointer anyway")
	}
}

func testSkillWorkspaceScoping(t *testing.T, st store.Store) {
	mine := mustWorkspace(t, st, "acme")
	theirs := mustWorkspace(t, st, "globex")
	skill, _ := mustImport(t, st, mine.ID, samplePackage("pdf-processing", "print(1)"))
	files := mustFiles(t, st, mine.ID, skill.Version.ID)

	// Another workspace's skill does not fail to load: it does not exist.
	if _, err := st.Skills().Skill(ctx(), theirs.ID, skill.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading it as another workspace = %v, want ErrNotFound", err)
	}
	if got, err := st.Skills().Skills(ctx(), theirs.ID); err != nil || len(got) != 0 {
		t.Errorf("another workspace sees %d skills (err %v)", len(got), err)
	}
	// A file id from another workspace reaches nothing, which is the check that
	// matters: the file route is addressed by the file's own id.
	if _, err := st.Skills().File(ctx(), theirs.ID, files[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading a file as another workspace = %v, want ErrNotFound", err)
	}
	if got, err := st.Skills().Files(ctx(), theirs.ID, skill.Version.ID); err != nil || len(got) != 0 {
		t.Errorf("another workspace lists %d files (err %v)", len(got), err)
	}
	if got, err := st.Skills().Sections(ctx(), theirs.ID, skill.Version.ID); err != nil || len(got) != 0 {
		t.Errorf("another workspace lists %d sections (err %v)", len(got), err)
	}
	if got, err := st.Skills().Versions(ctx(), theirs.ID, skill.ID); err != nil || len(got) != 0 {
		t.Errorf("another workspace lists %d versions (err %v)", len(got), err)
	}
	if err := st.Skills().DeleteSkill(ctx(), theirs.ID, skill.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting it as another workspace = %v, want ErrNotFound", err)
	}
	if _, err := setStatus(st, theirs.ID, skill, model.SkillDisabled); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("disabling it as another workspace = %v, want ErrNotFound", err)
	}

	// The same name in two workspaces is two skills, because the unique key is
	// the pair.
	if _, _, err := st.Skills().Import(ctx(), theirs.ID, samplePackage("pdf-processing", "print(1)"), model.Nobody()); err != nil {
		t.Fatalf("importing the same name into another workspace: %v", err)
	}
}

func testSkillDeleteTakesEverythingWithIt(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	skill, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))
	second, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(2)"))
	versionIDs := []int64{skill.Version.ID, second.Version.ID}

	// This is the measurement, not a formality: ai_skills and ai_skill_versions
	// point at EACH OTHER (the skill at its live version, the version at its
	// skill), and the two directions carry different rules. If the cascade and
	// the set-null cannot both run, this errors instead of deleting.
	if err := st.Skills().DeleteSkill(ctx(), ws.ID, skill.ID); err != nil {
		t.Fatalf("delete the skill: %v", err)
	}
	if _, err := st.Skills().Skill(ctx(), ws.ID, skill.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the skill is still there: %v", err)
	}
	for _, versionID := range versionIDs {
		if got, err := st.Skills().Files(ctx(), ws.ID, versionID); err != nil || len(got) != 0 {
			t.Errorf("version %d still has %d files (err %v)", versionID, len(got), err)
		}
		if got, err := st.Skills().Sections(ctx(), ws.ID, versionID); err != nil || len(got) != 0 {
			t.Errorf("version %d still has %d sections (err %v)", versionID, len(got), err)
		}
	}
	if got, err := st.Skills().Versions(ctx(), ws.ID, skill.ID); err != nil || len(got) != 0 {
		t.Errorf("the skill still has %d versions (err %v)", len(got), err)
	}
}

func testSkillStatusIsHeldToTheOnesThatExist(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	skill, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))

	// Refused here rather than by the column, so a caller passing something
	// that is not a status reads as exactly that instead of as a database error.
	if _, err := setStatus(st, ws.ID, skill, "enabled"); err == nil {
		t.Error("a status that does not exist was accepted")
	}
	after, err := st.Skills().Skill(ctx(), ws.ID, skill.ID)
	if err != nil {
		t.Fatalf("read the skill: %v", err)
	}
	if after.Status != model.SkillActive {
		t.Errorf("status = %q, the refused write landed anyway", after.Status)
	}
}

func mustFiles(t *testing.T, st store.Store, wsID, versionID int64) []*model.SkillFile {
	t.Helper()
	files, err := st.Skills().Files(ctx(), wsID, versionID)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	return files
}

func fileAt(t *testing.T, files []*model.SkillFile, path string) *model.SkillFile {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no file at %q; got %v", path, pathsOf(files))
	return nil
}

func pathsOf(files []*model.SkillFile) []string {
	out := []string{}
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// A NAME SOMEBODY CHOSE IS THEIRS, and an import does not take it away.
//
// The rule has two halves and they pull against each other, so both are pinned
// here: a title nobody has touched follows whatever version is live (the test
// above), and a title somebody wrote survives everything (this one). What tells
// them apart is the version that WAS live, which is why there is no "edited"
// column: if the row still says what that version said, nobody has been here.
func testSkillRenameSurvivesAnImport(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	first := samplePackage("pdf-processing", "print(1)")
	first.Title, first.Description = "The package's name", "What the package says."
	skill, _ := mustImport(t, st, ws.ID, first)

	// An administrator calls it what this workspace calls it.
	renamed, err := st.Skills().Update(ctx(), ws.ID, skill.ID, store.SkillUpdate{
		Title:       "Invoice tooling",
		Description: "What we actually use it for.",
		Status:      model.SkillActive,
	}, model.Nobody())
	if err != nil {
		t.Fatalf("rename the skill: %v", err)
	}
	if renamed.Title != "Invoice tooling" {
		t.Fatalf("title = %q, the rename did not land", renamed.Title)
	}

	// A new version arrives, carrying its own words. It does not get to
	// overwrite theirs: somebody decided what this is called on purpose.
	second := samplePackage("pdf-processing", "print(2)")
	second.Title, second.Description = "A new package name", "A new package description."
	after, added := mustImport(t, st, ws.ID, second)
	if !added {
		t.Fatal("the update wrote no version")
	}
	if after.Title != "Invoice tooling" {
		t.Errorf("title = %q, the import overwrote a name somebody chose", after.Title)
	}
	if after.Description != "What we actually use it for." {
		t.Errorf("description = %q, the import overwrote one somebody wrote", after.Description)
	}

	// And the version still records what ITS package said, because that is what
	// a version history is for. Losing it would make the rename destructive.
	if after.Version == nil || after.Version.Title != "A new package name" {
		t.Errorf("the version reads %+v, want the package's own words", after.Version)
	}
}

// The same, through a rollback rather than an import: the other way the live
// version changes, and the other way a name somebody chose could be lost.
func testSkillRenameSurvivesARollback(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	first := samplePackage("pdf-processing", "print(1)")
	first.Title = "Version one's name"
	one, _ := mustImport(t, st, ws.ID, first)
	firstVersionID := one.Version.ID

	second := samplePackage("pdf-processing", "print(2)")
	second.Title = "Version two's name"
	two, _ := mustImport(t, st, ws.ID, second)

	if _, err := st.Skills().Update(ctx(), ws.ID, two.ID, store.SkillUpdate{
		Title:       "Ours",
		Description: two.Description,
		Status:      model.SkillActive,
	}, model.Nobody()); err != nil {
		t.Fatalf("rename the skill: %v", err)
	}

	if err := makeLive(st, ws.ID, two, firstVersionID); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	back, err := st.Skills().Skill(ctx(), ws.ID, two.ID)
	if err != nil {
		t.Fatalf("read the skill: %v", err)
	}
	if back.Title != "Ours" {
		t.Errorf("title = %q, the rollback renamed a skill somebody had named", back.Title)
	}
	// The rollback still did its job.
	if back.ActiveVersionID != firstVersionID {
		t.Errorf("active version = %d, want %d", back.ActiveVersionID, firstVersionID)
	}
}

// Update records WHO, because the row is now something a person edits and
// migration 63's rule applies to it: the id, and the name frozen beside it.
func testSkillUpdateRecordsWhoChangedIt(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	skill, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))
	person := importer(t, st, ws.ID, "editor@acme.test")

	updated, err := st.Skills().Update(ctx(), ws.ID, skill.ID, store.SkillUpdate{
		Title:       "Renamed",
		Description: skill.Description,
		Status:      model.SkillActive,
	}, person)
	if err != nil {
		t.Fatalf("update the skill: %v", err)
	}
	if updated.UpdatedBy != person.UserID || updated.UpdatedByName != person.Name {
		t.Errorf("changed by %d/%q, want %d/%q",
			updated.UpdatedBy, updated.UpdatedByName, person.UserID, person.Name)
	}
	// Who CREATED it is the one fact an edit must not touch.
	if updated.CreatedByName != skill.CreatedByName {
		t.Errorf("created by %q, want %q: an edit rewrote the author",
			updated.CreatedByName, skill.CreatedByName)
	}
}

// Search is over three columns and they are three different ways of asking for
// the same skill: the handle somebody read in a SKILL.md, the name they gave
// it, and a word out of what it is for.
func testSkillSearch(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	pdf := samplePackage("pdf-processing", "print(1)")
	pdf.Title, pdf.Description = "Invoice tooling", "Extract totals from supplier documents."
	mustImport(t, st, ws.ID, pdf)

	csv := samplePackage("csv-tools", "print(1)")
	csv.Title, csv.Description = "Spreadsheets", "Read and write tabular exports."
	mustImport(t, st, ws.ID, csv)

	found := func(query string) []string {
		t.Helper()
		hits, err := st.Skills().Search(ctx(), ws.ID, query, 20)
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		names := make([]string, 0, len(hits))
		for _, hit := range hits {
			names = append(names, hit.Name)
		}
		return names
	}

	for _, probe := range []struct {
		query string
		want  string
		why   string
	}{
		{"pdf", "pdf-processing", "the handle, which is what a SKILL.md shows"},
		{"invoice", "pdf-processing", "the name somebody gave it"},
		{"supplier", "pdf-processing", "a word out of the description"},
		{"spreadsheets", "csv-tools", "the other skill, so this is not matching everything"},
	} {
		got := found(probe.query)
		if len(got) != 1 || got[0] != probe.want {
			t.Errorf("search %q = %v, want [%s] (%s)", probe.query, got, probe.want, probe.why)
		}
	}

	// A word that is in neither. The control for every line above: without it,
	// a search that returned the whole workspace would pass all four.
	if got := found("helicopter"); len(got) != 0 {
		t.Errorf("search for a word nobody used = %v, want nothing", got)
	}

	// Below the index's token size. Nothing, rather than everything, which is
	// what an empty full-text match would return.
	if got := found("of"); len(got) != 0 {
		t.Errorf("search for a two letter word = %v, want nothing", got)
	}

	// Another workspace's skills are not findable, whatever they are called.
	theirs := mustWorkspace(t, st, "other")
	if hits, err := st.Skills().Search(ctx(), theirs.ID, "invoice", 20); err != nil || len(hits) != 0 {
		t.Errorf("another workspace found %d skills (err %v)", len(hits), err)
	}
}

// WHICH SKILLS AN AGENT MAY USE (migration 68).
//
// The same shape the brains follow and tested the same way, because the
// failures are the same: a skill from another workspace sticking, an assignment
// that does not round-trip onto the form, and no way to take one away.
func testAgentSkills(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	other := mustWorkspace(t, st, "globex")

	ours, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))
	theirs, _ := mustImport(t, st, other.ID, samplePackage("csv-tools", "print(1)"))

	// Created already naming both: the one from another workspace must not
	// stick, and the INSERT ... SELECT is what stops it rather than a check
	// somebody has to remember.
	agent := &model.Agent{
		WorkspaceID: ws.ID, Key: model.DefaultAgentKey, Name: "House",
		Skills: []int64{ours.ID, theirs.ID},
	}
	if err := st.Agents().Create(ctx(), agent, model.Nobody()); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	loaded, err := st.Agents().GetByID(ctx(), ws.ID, agent.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if len(loaded.Skills) != 1 || loaded.Skills[0] != ours.ID {
		t.Fatalf("assigned skills = %v, want only this workspace's (%d)",
			loaded.Skills, ours.ID)
	}

	// Replaced wholesale, so revoking is possible: an empty (not nil) list
	// clears, and a nil one means no opinion and leaves what is stored alone.
	loaded.Skills = []int64{}
	if err := st.Agents().Update(ctx(), loaded, model.Nobody()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	after, err := st.Agents().GetByID(ctx(), ws.ID, agent.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if len(after.Skills) != 0 {
		t.Fatalf("a revoked skill is still assigned: %v", after.Skills)
	}

	after.Skills = []int64{ours.ID}
	if err := st.Agents().Update(ctx(), after, model.Nobody()); err != nil {
		t.Fatalf("assign again: %v", err)
	}
	after.Skills = nil
	if err := st.Agents().Update(ctx(), after, model.Nobody()); err != nil {
		t.Fatalf("update with no opinion: %v", err)
	}
	kept, err := st.Agents().GetByID(ctx(), ws.ID, agent.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if len(kept.Skills) != 1 || kept.Skills[0] != ours.ID {
		t.Fatalf("a nil list changed the assignment: %v", kept.Skills)
	}
}

// Deleting a skill takes every agent's assignment of it with it. The database
// enforces that, not a function: an assignment to a skill that does not exist
// is a row that could only ever be filtered out on read.
func testDeletingASkillUnassignsIt(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	skill, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))
	agent := &model.Agent{
		WorkspaceID: ws.ID, Key: model.DefaultAgentKey, Name: "House",
		Skills: []int64{skill.ID},
	}
	if err := st.Agents().Create(ctx(), agent, model.Nobody()); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := st.Skills().DeleteSkill(ctx(), ws.ID, skill.ID); err != nil {
		t.Fatalf("delete the skill: %v", err)
	}
	left, err := st.Agents().GetByID(ctx(), ws.ID, agent.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if len(left.Skills) != 0 {
		t.Errorf("the agent still holds a deleted skill: %v", left.Skills)
	}
}

// A draft is a version that exists and is not live.
//
// This is the whole of what Draft promises: a new numbered version, the agent
// still using the one it was using, and the skill's own words untouched until
// somebody publishes.
func testSkillDraftIsNotLive(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	skill, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))
	live := skill.Version.ID

	edited := samplePackage("pdf-processing", "print('edited')")
	draft, added, err := st.Skills().Draft(ctx(), ws.ID, skill.ID, live, edited,
		"edited scripts/extract.py", model.Actor{Name: "Someone"})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if !added {
		t.Fatal("the draft did not report that it wrote a version")
	}
	if draft.Status != model.SkillVersionDraft {
		t.Errorf("status = %q, want %q", draft.Status, model.SkillVersionDraft)
	}
	if draft.Source != model.SkillSourceManual {
		t.Errorf("source = %q, want %q", draft.Source, model.SkillSourceManual)
	}
	if draft.Number != 2 {
		t.Errorf("number = %d, want 2", draft.Number)
	}
	if draft.ParentVersionID != live {
		t.Errorf("parent = %d, want the version it was edited from (%d)", draft.ParentVersionID, live)
	}
	if draft.ChangeSummary != "edited scripts/extract.py" {
		t.Errorf("summary = %q", draft.ChangeSummary)
	}
	if draft.Files == 0 {
		t.Error("the draft has no files, so it carried nothing forward")
	}

	// The skill has NOT moved: this is the point of a draft.
	back, err := st.Skills().Skill(ctx(), ws.ID, skill.ID)
	if err != nil {
		t.Fatalf("read the skill: %v", err)
	}
	if back.ActiveVersionID != live {
		t.Errorf("the skill points at %d, want the version that was live (%d)", back.ActiveVersionID, live)
	}
	if back.Version.Status != model.SkillVersionActive || back.Version.ID != live {
		t.Errorf("the live version is %d/%q", back.Version.ID, back.Version.Status)
	}
	// And the file an agent would be given is still the old one.
	files, err := st.Skills().Files(ctx(), ws.ID, live)
	if err != nil {
		t.Fatalf("list the live version's files: %v", err)
	}
	for _, f := range files {
		if f.Path != "scripts/extract.py" {
			continue
		}
		whole, err := st.Skills().File(ctx(), ws.ID, f.ID)
		if err != nil {
			t.Fatalf("read the script: %v", err)
		}
		if whole.Text != "print(1)" {
			t.Errorf("the live script says %q, want the original", whole.Text)
		}
	}
}

// Publishing a draft is the same act as a rollback, through the same route:
// there is no second way to make a version live.
func testSkillDraftIsPublishedByMakingItLive(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	skill, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))
	was := skill.Version.ID

	draft, _, err := st.Skills().Draft(ctx(), ws.ID, skill.ID, was,
		samplePackage("pdf-processing", "print('edited')"), "edited", model.Actor{Name: "Someone"})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if err := makeLive(st, ws.ID, skill, draft.ID); err != nil {
		t.Fatalf("publish the draft: %v", err)
	}

	back, err := st.Skills().Skill(ctx(), ws.ID, skill.ID)
	if err != nil {
		t.Fatalf("read the skill: %v", err)
	}
	if back.ActiveVersionID != draft.ID {
		t.Fatalf("the skill points at %d, want the published draft %d", back.ActiveVersionID, draft.ID)
	}
	if back.Version.Status != model.SkillVersionActive {
		t.Errorf("the published version is %q", back.Version.Status)
	}
	// Exactly one version claims to be live, and the one that was is archived.
	versions, err := st.Skills().Versions(ctx(), ws.ID, skill.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	live := 0
	for _, v := range versions {
		if v.Status == model.SkillVersionActive {
			live++
		}
		if v.ID == was && v.Status != model.SkillVersionArchived {
			t.Errorf("the version that was live is %q, want archived", v.Status)
		}
	}
	if live != 1 {
		t.Errorf("%d versions claim to be live", live)
	}
}

// Editing a file back to what it already said writes nothing. A person who
// undoes their own change has not made a version of anything.
func testSkillDraftIsIdempotentOnTheBytes(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	skill, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))
	live := skill.Version.ID

	same, added, err := st.Skills().Draft(ctx(), ws.ID, skill.ID, live,
		samplePackage("pdf-processing", "print(1)"), "no change", model.Actor{Name: "Someone"})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if added {
		t.Error("an identical package was written as a new version")
	}
	if same.ID != live {
		t.Errorf("it answered with version %d, want the one that already holds those bytes (%d)",
			same.ID, live)
	}
	versions, err := st.Skills().Versions(ctx(), ws.ID, skill.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 1 {
		t.Errorf("%d versions, want the one", len(versions))
	}
}

// A draft is turned down, and only a draft: an active version is what the agent
// is using and an archived one is the history a rollback walks.
func testSkillDiscardOnlyTakesADraft(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	skill, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))
	live := skill.Version.ID

	draft, _, err := st.Skills().Draft(ctx(), ws.ID, skill.ID, live,
		samplePackage("pdf-processing", "print('edited')"), "edited", model.Actor{Name: "Someone"})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}

	// The live one cannot be discarded.
	if err := st.Skills().Discard(ctx(), ws.ID, skill.ID, live, model.Actor{Name: "Someone"}); err == nil {
		t.Error("the live version was discarded")
	}
	// Nor can somebody else's workspace reach it.
	other := mustWorkspace(t, st, "other")
	if err := st.Skills().Discard(ctx(), other.ID, skill.ID, draft.ID, model.Actor{Name: "Someone"}); err == nil {
		t.Error("a draft was discarded from another workspace")
	}

	if err := st.Skills().Discard(ctx(), ws.ID, skill.ID, draft.ID, model.Actor{Name: "Someone"}); err != nil {
		t.Fatalf("discard the draft: %v", err)
	}
	versions, err := st.Skills().Versions(ctx(), ws.ID, skill.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	for _, v := range versions {
		if v.ID == draft.ID && v.Status != model.SkillVersionRejected {
			t.Errorf("the discarded draft is %q, want %q", v.Status, model.SkillVersionRejected)
		}
	}
	// Twice is not done twice: the second is told there was nothing to turn down.
	if err := st.Skills().Discard(ctx(), ws.ID, skill.ID, draft.ID, model.Actor{Name: "Someone"}); err == nil {
		t.Error("discarding the same draft twice both succeeded")
	}
}

// A version of another skill is not a parent this skill may claim.
func testSkillDraftRefusesAnotherSkillsParent(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	mine, _ := mustImport(t, st, ws.ID, samplePackage("pdf-processing", "print(1)"))
	theirs, _ := mustImport(t, st, ws.ID, samplePackage("csv-tools", "print(2)"))

	_, _, err := st.Skills().Draft(ctx(), ws.ID, mine.ID, theirs.Version.ID,
		samplePackage("pdf-processing", "print('edited')"), "edited", model.Actor{Name: "Someone"})
	if err == nil {
		t.Fatal("a draft claimed another skill's version as its parent")
	}
}

// testToolSkills is the same contract one level over: a TOOL points at the
// procedures that document it.
//
// The failures worth catching are the ones agent_skills has, and the first is
// the one that matters: a skill from another workspace must not stick. The
// INSERT ... SELECT is what stops it, rather than a check somebody has to
// remember to write, so this asserts the mechanism and not a habit.
func testToolSkills(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	other := mustWorkspace(t, st, "globex")

	ours, _ := mustImport(t, st, ws.ID, samplePackage("invoice-api", "print(1)"))
	theirs, _ := mustImport(t, st, other.ID, samplePackage("their-api", "print(1)"))

	made := &model.Tool{
		WorkspaceID: ws.ID, Name: "api_invoices", Kind: "custom", Template: "api",
		FriendlyName: "Invoices", Description: "The invoice API.",
		Risk: string(tool.RiskReadOnly), Status: model.StatusActive,
		Skills: []int64{ours.ID, theirs.ID},
	}
	if err := st.Tools().CreateCustom(ctx(), made, model.Nobody()); err != nil {
		t.Fatalf("create custom tool: %v", err)
	}

	loaded, err := st.Tools().GetByID(ctx(), ws.ID, made.ID)
	if err != nil {
		t.Fatalf("get tool: %v", err)
	}
	if len(loaded.Skills) != 1 || loaded.Skills[0] != ours.ID {
		t.Fatalf("a tool's skills = %v, want only this workspace's (%d)", loaded.Skills, ours.ID)
	}

	// Replaced wholesale, so taking one away is possible: an empty (not nil)
	// list clears, and a nil one means no opinion and leaves what is stored.
	loaded.Skills = []int64{}
	if err := st.Tools().UpdateCustom(ctx(), loaded, model.Nobody()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	after, err := st.Tools().GetByID(ctx(), ws.ID, made.ID)
	if err != nil {
		t.Fatalf("get tool: %v", err)
	}
	if len(after.Skills) != 0 {
		t.Fatalf("a detached procedure is still attached: %v", after.Skills)
	}

	after.Skills = nil
	if err := st.Tools().UpdateCustom(ctx(), after, model.Nobody()); err != nil {
		t.Fatalf("no opinion: %v", err)
	}
	after.Skills = []int64{ours.ID}
	if err := st.Tools().UpdateCustom(ctx(), after, model.Nobody()); err != nil {
		t.Fatalf("attach again: %v", err)
	}
	again, err := st.Tools().GetByID(ctx(), ws.ID, made.ID)
	if err != nil {
		t.Fatalf("get tool: %v", err)
	}
	if len(again.Skills) != 1 || again.Skills[0] != ours.ID {
		t.Fatalf("re-attached = %v, want %d", again.Skills, ours.ID)
	}

	// Deleting the SKILL takes the pointer with it: a tool naming a procedure
	// that no longer exists would name it in its guide too.
	if err := st.Skills().DeleteSkill(ctx(), ws.ID, ours.ID); err != nil {
		t.Fatalf("delete skill: %v", err)
	}
	gone, err := st.Tools().GetByID(ctx(), ws.ID, made.ID)
	if err != nil {
		t.Fatalf("get tool: %v", err)
	}
	if len(gone.Skills) != 0 {
		t.Fatalf("a deleted procedure is still pointed at: %v", gone.Skills)
	}
}
