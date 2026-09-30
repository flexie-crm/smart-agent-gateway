package skill

import (
	"archive/zip"
	"bytes"
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
)

// Reading a skill package.
//
// Half of these tests are refusals, and that is the point: the archive arrives
// from outside, and every one of them is a way an archive has been used to write
// a file somewhere it was not supposed to go. A refusal is asserted by the
// MESSAGE reaching a person as well as by the error, because "that failed" tells
// the sender nothing about which of their files is wrong.

type entry struct {
	name string
	body []byte
	mode fs.FileMode
}

func file(name, body string) entry    { return entry{name: name, body: []byte(body)} }
func bin(name string, b []byte) entry { return entry{name: name, body: b} }

func pack(t *testing.T, entries ...entry) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	w := zip.NewWriter(buf)
	for _, e := range entries {
		header := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			header.SetMode(e.mode)
		}
		out, err := w.CreateHeader(header)
		if err != nil {
			t.Fatalf("pack %s: %v", e.name, err)
		}
		if _, err := out.Write(e.body); err != nil {
			t.Fatalf("write %s: %v", e.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	return buf.Bytes()
}

func read(t *testing.T, archive []byte) (*model.SkillPackage, error) {
	t.Helper()
	return Read(bytes.NewReader(archive), int64(len(archive)))
}

func mustRead(t *testing.T, archive []byte) *model.SkillPackage {
	t.Helper()
	pkg, err := read(t, archive)
	if err != nil {
		t.Fatalf("read package: %v", err)
	}
	return pkg
}

// refuses asserts the package is rejected AND that the reason names the problem.
func refuses(t *testing.T, archive []byte, mentions string) {
	t.Helper()
	pkg, err := read(t, archive)
	if err == nil {
		t.Fatalf("expected a refusal, got a package named %q", pkg.Name)
	}
	var rejection *Rejection
	if !errors.As(err, &rejection) {
		t.Fatalf("expected a Rejection the sender can act on, got %T: %v", err, err)
	}
	if !strings.Contains(rejection.Reason, mentions) {
		t.Fatalf("the refusal does not mention %q: %s", mentions, rejection.Reason)
	}
}

const manifest = `---
name: pdf-processing
description: Extract, inspect, and transform PDF files. Use for PDF-related tasks.
license: Apache-2.0
allowed-tools: [Read, Bash]
metadata:
  author: somebody
---

# PDF processing

Read the file first.

## Extracting

Run the script.
`

func good() []entry {
	return []entry{
		file("pdf-processing/"+Manifest, manifest),
		file("pdf-processing/scripts/extract.py", "import sys\n\nprint(sys.argv)\n"),
		file("pdf-processing/references/formats.md", "# Formats\n\nPDF 1.7 is the one.\n"),
		// A NUL byte is what makes this binary, and it is how a real docx reads.
		bin("pdf-processing/assets/template.docx", []byte{0x50, 0x4b, 0x03, 0x04, 0x00, 0x01, 0x02}),
	}
}

func TestAPackageIsReadWithItsStructureIntact(t *testing.T) {
	pkg := mustRead(t, pack(t, good()...))

	if pkg.Name != "pdf-processing" {
		t.Errorf("name = %q", pkg.Name)
	}
	if !strings.HasPrefix(pkg.Description, "Extract, inspect") {
		t.Errorf("description = %q", pkg.Description)
	}
	if len(pkg.Files) != 4 {
		t.Fatalf("files = %d, want 4", len(pkg.Files))
	}

	byPath := map[string]model.PackageFile{}
	for _, f := range pkg.Files {
		byPath[f.Path] = f
	}

	// The path is the RELATIVE one: the root directory is the skill's name and
	// is not part of any file's path, which is what makes an export reproduce
	// the directory rather than nest it twice.
	for _, want := range []string{Manifest, "scripts/extract.py", "references/formats.md", "assets/template.docx"} {
		if _, ok := byPath[want]; !ok {
			t.Errorf("no file at %q; got %v", want, paths(pkg))
		}
	}

	if got := byPath[Manifest]; got.FileType != model.SkillFileSkill {
		t.Errorf("%s is %q, want %q", Manifest, got.FileType, model.SkillFileSkill)
	}
	if got := byPath["scripts/extract.py"]; got.FileType != model.SkillFileScript {
		t.Errorf("script is %q", got.FileType)
	}
	if got := byPath["references/formats.md"]; got.FileType != model.SkillFileReference {
		t.Errorf("reference is %q", got.FileType)
	}
	if got := byPath["assets/template.docx"]; got.FileType != model.SkillFileAsset {
		t.Errorf("asset is %q", got.FileType)
	}

	// Text goes in Text and bytes go in Binary, and never both: which column is
	// populated is what the store writes, so a script filed as bytes would be a
	// script nothing can search.
	script := byPath["scripts/extract.py"]
	if script.Text == "" || script.Bytes != nil {
		t.Errorf("the script is not stored as text: text=%q binary=%v", script.Text, script.Bytes)
	}
	if script.MIMEType != "text/x-python" {
		t.Errorf("script mime = %q", script.MIMEType)
	}
	asset := byPath["assets/template.docx"]
	if asset.Bytes == nil || asset.Text != "" {
		t.Errorf("the asset is not stored as bytes: text=%q", asset.Text)
	}
	if !strings.Contains(asset.MIMEType, "wordprocessingml") {
		t.Errorf("asset mime = %q", asset.MIMEType)
	}
	if asset.Size != 7 {
		t.Errorf("asset size = %d, want 7", asset.Size)
	}

	// A binary asset is deliberately not indexed; every text file is.
	if len(asset.Sections) != 0 {
		t.Errorf("the binary asset produced %d sections, want none", len(asset.Sections))
	}
	if len(script.Sections) != 1 {
		t.Errorf("the script produced %d sections, want one per file", len(script.Sections))
	}
	if len(byPath[Manifest].Sections) == 0 {
		t.Error("the manifest produced no sections")
	}
}

// The format says an implementation preserves what it does not understand. Here
// that is structural, and this is the assertion of it: the manifest is stored
// byte for byte, so the fields nothing reads are still there afterwards.
func TestUnknownFrontmatterSurvivesTheImport(t *testing.T) {
	pkg := mustRead(t, pack(t, good()...))
	stored := ""
	for _, f := range pkg.Files {
		if f.Path == Manifest {
			stored = f.Text
		}
	}
	for _, field := range []string{"license: Apache-2.0", "allowed-tools: [Read, Bash]", "author: somebody"} {
		if !strings.Contains(stored, field) {
			t.Errorf("the stored manifest lost %q", field)
		}
	}
	if stored != manifest {
		t.Error("the manifest was not stored exactly as it arrived")
	}
}

func TestTheManifestMayBeAtTheTopOfTheArchive(t *testing.T) {
	// Compressing the skill's CONTENTS rather than its folder. There is no
	// directory name to disagree with, so the manifest's name stands alone.
	pkg := mustRead(t, pack(t,
		file(Manifest, manifest),
		file("scripts/extract.py", "print(1)\n"),
	))
	if pkg.Name != "pdf-processing" {
		t.Errorf("name = %q", pkg.Name)
	}
	if len(pkg.Files) != 2 {
		t.Errorf("files = %d", len(pkg.Files))
	}
}

func TestWhatAMacAddsToAnArchiveIsNotPartOfThePackage(t *testing.T) {
	// The ordinary way a person produces a zip on a Mac. Read literally, the
	// __MACOSX directory is a second top-level folder and the package has "more
	// than one skill root", which would refuse almost every archive somebody
	// sends from a desktop.
	pkg := mustRead(t, pack(t,
		file("pdf-processing/"+Manifest, manifest),
		file("__MACOSX/._"+Manifest, "\x00\x05\x16\x07"),
		file("__MACOSX/pdf-processing/._scripts", "\x00\x05\x16\x07"),
		file("pdf-processing/.DS_Store", "\x00\x00\x00"),
		file("pdf-processing/._notes.md", "\x00\x05\x16\x07"),
		file("pdf-processing/scripts/extract.py", "print(1)\n"),
	))
	if len(pkg.Files) != 2 {
		t.Fatalf("files = %v, want just the manifest and the script", paths(pkg))
	}
}

func TestTheSameFilesInAnyOrderAreTheSamePackage(t *testing.T) {
	// The hash is what makes a re-import idempotent, so it must not depend on
	// the order the archive happened to list its entries in.
	forward := good()
	backward := []entry{forward[3], forward[1], forward[0], forward[2]}

	first, second := mustRead(t, pack(t, forward...)), mustRead(t, pack(t, backward...))
	if first.SHA256 != second.SHA256 {
		t.Errorf("the same files hashed differently:\n  %s\n  %s", first.SHA256, second.SHA256)
	}
	if first.SHA256 == "" {
		t.Error("the package has no hash")
	}
}

func TestOneChangedByteIsADifferentPackage(t *testing.T) {
	before := mustRead(t, pack(t, good()...))

	changed := good()
	changed[1] = file("pdf-processing/scripts/extract.py", "import sys\n\nprint(sys.argv)\n\n")
	after := mustRead(t, pack(t, changed...))

	if before.SHA256 == after.SHA256 {
		t.Error("a changed script did not change the package hash")
	}
}

func TestHostileArchivesAreRefused(t *testing.T) {
	tests := []struct {
		name     string
		entries  []entry
		mentions string
	}{
		{
			name:     "a path climbing out of the package",
			entries:  []entry{file("pdf-processing/"+Manifest, manifest), file("pdf-processing/../../etc/passwd", "root")},
			mentions: "points outside the package",
		},
		{
			name:     "an absolute path",
			entries:  []entry{file("pdf-processing/"+Manifest, manifest), file("/etc/passwd", "root")},
			mentions: "absolute path",
		},
		{
			name:     "a Windows absolute path",
			entries:  []entry{file("pdf-processing/"+Manifest, manifest), file(`C:/Windows/System32/x.dll`, "x")},
			mentions: "absolute path",
		},
		{
			name:     "a backslash path",
			entries:  []entry{file("pdf-processing/"+Manifest, manifest), file(`pdf-processing\scripts\x.py`, "x")},
			mentions: "not a valid path",
		},
		{
			name: "a symbolic link",
			entries: []entry{
				file("pdf-processing/"+Manifest, manifest),
				{name: "pdf-processing/scripts/run.sh", body: []byte("/etc/passwd"), mode: fs.ModeSymlink | 0o777},
			},
			mentions: "symbolic link",
		},
		{
			name: "something that is not a file at all",
			entries: []entry{
				file("pdf-processing/"+Manifest, manifest),
				{name: "pdf-processing/dev/null", body: nil, mode: fs.ModeDevice | 0o666},
			},
			mentions: "not an ordinary file",
		},
		{
			name: "two skills in one archive",
			entries: []entry{
				file("pdf-processing/"+Manifest, manifest),
				file("other-skill/"+Manifest, strings.Replace(manifest, "pdf-processing", "other-skill", 1)),
			},
			mentions: "more than one skill",
		},
		{
			name:     "no manifest",
			entries:  []entry{file("pdf-processing/README.md", "# nothing")},
			mentions: "no " + Manifest,
		},
		{
			name:     "a manifest named in the wrong case",
			entries:  []entry{file("pdf-processing/skill.md", manifest)},
			mentions: "must be called exactly",
		},
		{
			name:     "a manifest buried in a subdirectory",
			entries:  []entry{file("bundle/pdf-processing/"+Manifest, manifest)},
			mentions: "not at the top of the package",
		},
		{
			name: "a file outside the skill's own directory",
			entries: []entry{
				file("pdf-processing/"+Manifest, manifest),
				file("notes.txt", "loose"),
			},
			mentions: "outside the skill's own directory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			refuses(t, pack(t, tc.entries...), tc.mentions)
		})
	}
}

func TestAManifestIsHeldToTheFormat(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		mentions string
	}{
		{
			name:     "no frontmatter at all",
			manifest: "# pdf-processing\n\nJust prose.\n",
			mentions: "must begin with a --- line",
		},
		{
			name:     "frontmatter that is never closed",
			manifest: "---\nname: pdf-processing\ndescription: Something.\n",
			mentions: "never closed",
		},
		{
			name:     "frontmatter that is not YAML",
			manifest: "---\nname: [unclosed\n---\n",
			mentions: "not valid YAML",
		},
		{
			name:     "no name",
			manifest: "---\ndescription: Something useful.\n---\n",
			mentions: "no name",
		},
		{
			name:     "no description",
			manifest: "---\nname: pdf-processing\n---\n",
			mentions: "no description",
		},
		{
			name:     "a name that is not a name",
			manifest: "---\nname: PDF Processing\ndescription: Something useful.\n---\n",
			mentions: "lowercase letters, numbers and hyphens",
		},
		{
			name:     "a name that is not even text",
			manifest: "---\nname: 12\ndescription: Something useful.\n---\n",
			mentions: "no name",
		},
		{
			name:     "a name longer than the format allows",
			manifest: "---\nname: " + strings.Repeat("a", 65) + "\ndescription: Something useful.\n---\n",
			mentions: "up to 64 characters",
		},
		{
			name:     "a description longer than the format allows",
			manifest: "---\nname: pdf-processing\ndescription: " + strings.Repeat("x", 1025) + "\n---\n",
			mentions: "up to 1024 characters",
		},
		{
			name:     "a name that disagrees with the directory",
			manifest: "---\nname: pdf-tools\ndescription: Something useful.\n---\n",
			mentions: "the two must match",
		},
		// The specification's own rules, and the reference validator refuses
		// each of these: a package we accepted and another implementation
		// rejected would be a package that imports here and works nowhere else.
		{
			name:     "a name starting with a hyphen",
			manifest: "---\nname: -pdf\ndescription: Something useful.\n---\n",
			mentions: "may not start or end with a hyphen",
		},
		{
			name:     "a name ending with a hyphen",
			manifest: "---\nname: pdf-\ndescription: Something useful.\n---\n",
			mentions: "may not start or end with a hyphen",
		},
		{
			name:     "a name with two hyphens in a row",
			manifest: "---\nname: pdf--processing\ndescription: Something useful.\n---\n",
			mentions: "two hyphens in a row",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Named after the manifest's own name where it is a legal one, so a
			// rule about the NAME is not answered by the directory check. The
			// hyphen cases keep the ordinary directory, since a directory named
			// "-pdf" would be refused for the wrong reason.
			root := "pdf-processing"
			refuses(t, pack(t, file(root+"/"+Manifest, tc.manifest)), tc.mentions)
		})
	}
}

// The friendly name.
//
// The format has no field for one: `name` is an identifier, constrained to
// lowercase and hyphens and required to equal the directory name, so a list of
// skills reads like a directory listing. These are the two honest places a
// human name can come from, in the order they are trusted.
func TestTheNameForAPersonComesFromThePackage(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
		why      string
	}{
		{
			name: "the author said so",
			manifest: "---\nname: pdf-processing\ndescription: Does things.\n" +
				"metadata:\n  title: PDF Toolkit\n---\n\n# Something else entirely\n",
			want: "PDF Toolkit",
			why:  "metadata.title is where the specification says a client puts a property it needs, so it wins",
		},
		{
			name:     "the manifest's own first heading",
			manifest: "---\nname: pdf-processing\ndescription: Does things.\n---\n\n# PDF processing\n\nprose\n",
			want:     "PDF processing",
			why:      "what the author wrote at the top of their own document",
		},
		{
			name: "not a deeper heading",
			manifest: "---\nname: pdf-processing\ndescription: Does things.\n---\n\n" +
				"# Top\n\n## Usage\n",
			want: "Top",
			why:  "the first TOP-LEVEL heading, not the first heading of any depth",
		},
		{
			name:     "nothing to go on",
			manifest: "---\nname: pdf-processing\ndescription: Does things.\n---\n\nJust prose, no heading.\n",
			want:     "",
			why:      "empty rather than invented: the handle is what gets printed, and title-casing it would produce a name nobody wrote",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pkg := mustRead(t, pack(t, file("pdf-processing/"+Manifest, tc.manifest)))
			if pkg.Title != tc.want {
				t.Errorf("title = %q, want %q\n  %s", pkg.Title, tc.want, tc.why)
			}
			// Whatever the title, the handle is untouched: it is what the agent
			// addresses the skill by and what the directory is called.
			if pkg.Name != "pdf-processing" {
				t.Errorf("name = %q", pkg.Name)
			}
		})
	}
}

// A title is not part of what makes a package the same package.
//
// It comes out of the manifest, so it cannot change on its own; this pins the
// other direction, that the hash covers the bytes and nothing about the reading
// of them.
func TestTheTitleIsNotPartOfTheHash(t *testing.T) {
	pkg := mustRead(t, pack(t, good()...))
	if pkg.Title != "PDF processing" {
		t.Fatalf("title = %q", pkg.Title)
	}
	if pkg.SHA256 == "" {
		t.Error("no hash")
	}
}

func TestAnArchiveThatIsNotOneIsRefused(t *testing.T) {
	refuses(t, []byte("this is not a zip file at all"), "not a zip archive")
}

func TestAnEmptyUploadIsRefused(t *testing.T) {
	if _, err := Read(bytes.NewReader(nil), 0); err == nil {
		t.Fatal("an empty upload was accepted")
	}
}

func TestAnArchiveWithNoFilesIsRefused(t *testing.T) {
	refuses(t, pack(t), "no files")
}

func TestAFileLargerThanTheCeilingIsRefused(t *testing.T) {
	// The zip of 16 MiB of zeroes is a few kilobytes, which is exactly why the
	// ceiling cannot be on the archive alone.
	refuses(t, pack(t,
		file("pdf-processing/"+Manifest, manifest),
		bin("pdf-processing/assets/big.bin", make([]byte, MaxFileBytes+1)),
	), "larger than")
}

func TestAPackageThatExpandsPastTheCeilingIsRefused(t *testing.T) {
	entries := []entry{file("pdf-processing/"+Manifest, manifest)}
	for i := 0; i < 5; i++ {
		entries = append(entries, bin("pdf-processing/assets/part"+string(rune('a'+i))+".bin", make([]byte, 14<<20)))
	}
	refuses(t, pack(t, entries...), "expands to more than")
}

func TestTooManyFilesIsRefused(t *testing.T) {
	entries := []entry{file("pdf-processing/"+Manifest, manifest)}
	for i := 0; i <= MaxFiles; i++ {
		entries = append(entries, file("pdf-processing/references/f"+strconv.Itoa(i)+".md", "x"))
	}
	refuses(t, pack(t, entries...), "up to 1000 files")
}

func paths(pkg *model.SkillPackage) []string {
	out := []string{}
	for _, f := range pkg.Files {
		out = append(out, f.Path)
	}
	return out
}
