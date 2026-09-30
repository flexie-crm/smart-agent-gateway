package skill

import (
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
)

// The files a version holds, as Rewrite is given them. Built by reading a real
// archive, so the input to an edit is exactly what an import produced rather
// than a hand-written approximation of it: if the two ever disagree, they
// disagree here rather than in production.
func heldFiles(t *testing.T) []model.PackageFile {
	t.Helper()
	return mustRead(t, pack(t, good()...)).Files
}

func rewrite(t *testing.T, edits ...Edit) (*model.SkillPackage, []string) {
	t.Helper()
	pkg, changed, err := Rewrite("pdf-processing", heldFiles(t), edits)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	return pkg, changed
}

// refusesEdit asserts the edit is refused AND that the reason names the problem.
func refusesEdit(t *testing.T, mentions string, edits ...Edit) {
	t.Helper()
	pkg, _, err := Rewrite("pdf-processing", heldFiles(t), edits)
	if err == nil {
		t.Fatalf("expected a refusal, got a package of %d files", len(pkg.Files))
	}
	if !strings.Contains(err.Error(), mentions) {
		t.Fatalf("the reason does not mention %q: %v", mentions, err)
	}
}

// An edited script comes out as a whole package: the file it changed, and every
// other file exactly as it was.
func TestAnEditedScriptMakesAWholePackage(t *testing.T) {
	const edited = "import sys\n\nprint(len(sys.argv))\n"
	pkg, changed := rewrite(t, Edit{Path: "scripts/extract.py", Text: edited})

	if len(changed) != 1 || changed[0] != "scripts/extract.py" {
		t.Fatalf("changed = %v, want just the script", changed)
	}
	// Every file of the original is still there, and there are no others.
	held := heldFiles(t)
	if len(pkg.Files) != len(held) {
		t.Fatalf("the package holds %d files, want %d", len(pkg.Files), len(held))
	}
	for _, was := range held {
		now := fileAt(t, pkg, was.Path)
		if was.Path == "scripts/extract.py" {
			if now.Text != edited {
				t.Errorf("the script holds %q, want the edit", now.Text)
			}
			// Derived again rather than carried: size and hash are of the new
			// content, and the type still comes from where it sits.
			if now.Size != int64(len(edited)) {
				t.Errorf("size = %d, want %d", now.Size, len(edited))
			}
			if now.SHA256 == was.SHA256 {
				t.Error("the hash did not change with the content")
			}
			if now.FileType != model.SkillFileScript {
				t.Errorf("file type = %q, want a script", now.FileType)
			}
			continue
		}
		if now.SHA256 != was.SHA256 || now.Text != was.Text {
			t.Errorf("%s changed and should not have", was.Path)
		}
	}
	// And the package's own identity moved, which is what makes it a new
	// version rather than the same one.
	if pkg.SHA256 == mustRead(t, pack(t, good()...)).SHA256 {
		t.Error("the package hash did not change, so this would import as the same version")
	}
	if pkg.Name != "pdf-processing" {
		t.Errorf("name = %q", pkg.Name)
	}
}

// Editing the manifest re-reads everything the manifest decides: its passages,
// the title and the description.
func TestEditingTheManifestRereadsWhatItDecides(t *testing.T) {
	const edited = `---
name: pdf-processing
description: Now it says something else entirely about PDFs.
metadata:
  title: The New Name
---

# Ignored Heading

Read it.

## A Fresh Section

Do this.
`
	pkg, changed := rewrite(t, Edit{Path: Manifest, Text: edited})
	if len(changed) != 1 || changed[0] != Manifest {
		t.Fatalf("changed = %v", changed)
	}
	if pkg.Title != "The New Name" {
		t.Errorf("title = %q, want the one the frontmatter declares", pkg.Title)
	}
	if !strings.Contains(pkg.Description, "something else entirely") {
		t.Errorf("description = %q", pkg.Description)
	}
	// The passages are a reading of the new text, not the old.
	manifest := fileAt(t, pkg, Manifest)
	var headings []string
	for _, s := range manifest.Sections {
		if s.Heading != "" {
			headings = append(headings, s.Heading)
		}
	}
	if strings.Join(headings, "|") != "Ignored Heading|A Fresh Section" {
		t.Errorf("headings = %v, want the edited ones", headings)
	}
}

// Setting a file to what it already says is not a change. The caller uses this
// to refuse a save that would write a version identical to the live one.
func TestWritingTheSameContentIsNotAChange(t *testing.T) {
	held := heldFiles(t)
	same := fileIn(held, "scripts/extract.py")
	pkg, changed, err := Rewrite("pdf-processing", held, []Edit{{Path: same.Path, Text: same.Text}})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want nothing", changed)
	}
	// And the package is byte for byte the one that was there, so a store that
	// is idempotent on the hash writes no version at all.
	if pkg.SHA256 != mustRead(t, pack(t, good()...)).SHA256 {
		t.Error("the package hash moved without any content changing")
	}
}

// The handle is the skill's identity, not a field on a form. A manifest that
// stops naming this skill is a different skill.
func TestTheManifestMustKeepNamingThisSkill(t *testing.T) {
	refusesEdit(t, "must match", Edit{Path: Manifest, Text: `---
name: something-else
description: Extract, inspect, and transform PDF files. Use for PDF-related tasks.
---

# Title

Body.
`})
}

// A package gains and loses files by being imported, not by being edited.
func TestAnEditToAFileThatIsNotThereIsRefused(t *testing.T) {
	refusesEdit(t, "not in this version",
		Edit{Path: "scripts/invented.py", Text: "print(1)\n"})
}

// A binary asset has nothing to edit as text.
func TestABinaryFileCannotBeEditedAsText(t *testing.T) {
	refusesEdit(t, "not text", Edit{Path: "assets/template.docx", Text: "hello"})
}

// Whether a file is text is decided by its CONTENT, so an edit can make a
// readable file unreadable. Refused, because a script saved with a stray NUL
// would leave the reader and the search index at once.
func TestAnEditThatWouldStopBeingTextIsRefused(t *testing.T) {
	refusesEdit(t, "no longer be a text file",
		Edit{Path: "scripts/extract.py", Text: "print(1)\x00\n"})
}

// Two edits to one path is a caller bug, not something to resolve by ordering.
func TestOnePathCannotBeEditedTwiceInOneSave(t *testing.T) {
	refusesEdit(t, "edited twice",
		Edit{Path: "scripts/extract.py", Text: "print(1)\n"},
		Edit{Path: "scripts/extract.py", Text: "print(2)\n"})
}

// A manifest that no longer parses is refused by the same frontmatter reader an
// import uses, so the two routes cannot disagree about what a manifest is.
func TestAManifestThatNoLongerParsesIsRefused(t *testing.T) {
	refusesEdit(t, Manifest, Edit{Path: Manifest, Text: "# No frontmatter at all\n"})
}

func fileAt(t *testing.T, pkg *model.SkillPackage, path string) model.PackageFile {
	t.Helper()
	for _, f := range pkg.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("%s is not in the package", path)
	return model.PackageFile{}
}

func fileIn(files []model.PackageFile, path string) model.PackageFile {
	for _, f := range files {
		if f.Path == path {
			return f
		}
	}
	return model.PackageFile{}
}
