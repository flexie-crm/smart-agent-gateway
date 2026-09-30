// Package skill reads a skill package: a zip in the Agent Skills directory
// format, turned into a validated, parsed thing in memory.
//
// It touches no database and serves no request. Everything here is a pure
// function of the bytes that arrived, which is what makes the awkward half of
// this feature (a hostile archive) testable in milliseconds and without a
// server.
//
// # Nothing is executed
//
// A package carries scripts. They are stored as source and read as text, and at
// no point during an import is anything in the archive run, opened by a helper
// that might run it, or written to a path the archive chose. That is the whole
// security posture of this package, and it is structural: the only thing here
// that touches the filesystem is the reader handed in by the caller.
package skill

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"flexie.io/sag/internal/model"
)

// The ceilings. Every one of them is a refusal before anything is kept, and
// each exists for a failure somebody has actually shipped:
//
//   - MaxUploadBytes bounds the archive itself, so a huge upload is refused by
//     the connection rather than after it has all arrived;
//   - MaxExpandedBytes bounds everything inside it ADDED UP, which is what a
//     zip bomb defeats when only the archive is bounded: a few kilobytes of
//     zeroes expand to gigabytes;
//   - MaxFileBytes bounds one file, because a skill is instructions and a
//     template, not a disk image;
//   - MaxFiles bounds the count, because ten thousand empty files cost nothing
//     to compress and one row each to store;
//   - MaxPackages bounds how many archives one import may carry, because a
//     batch is a loop and a loop with no ceiling holds the connection for as
//     long as somebody cares to keep dragging files onto the drop zone;
//   - MaxBatchBytes bounds those archives ADDED UP. MaxPackages times
//     MaxUploadBytes is a gigabyte of temporary files for a screen that imports
//     instructions and templates, so the batch has a ceiling of its own and
//     somebody importing genuinely enormous packages does it in two goes.
const (
	MaxUploadBytes   = 20 << 20 // 20 MiB, one archive
	MaxExpandedBytes = 64 << 20 // 64 MiB, its contents added up
	MaxFileBytes     = 16 << 20 // 16 MiB, any one file
	MaxFiles         = 1000
	MaxPackages      = 50
	MaxBatchBytes    = 100 << 20 // 100 MiB, every archive in one import
	// maxPathLength matches the column the path is stored in. Refusing here
	// rather than at the insert means the person is told which file is wrong.
	maxPathLength = 512
)

// Manifest is the file every package must carry.
const Manifest = "SKILL.md"

// Rejection is a package we will not accept, with the reason a person reads.
//
// It is a type rather than a sentinel because every refusal says something
// different and specific ("scripts/run.sh is a symbolic link"), and a caller
// needs to tell "this archive is wrong" apart from "something failed here":
// the first is the sender's to fix and the second is ours.
type Rejection struct{ Reason string }

func (r *Rejection) Error() string { return r.Reason }

func reject(format string, args ...any) error {
	return &Rejection{Reason: fmt.Sprintf(format, args...)}
}

// Read validates and parses a skill package.
//
// The reader must be positioned at the start of a zip archive of the declared
// size. What comes back is everything needed to store a version, and nothing
// has been written anywhere.
func Read(r io.ReaderAt, size int64) (*model.SkillPackage, error) {
	if size <= 0 {
		return nil, reject("that file is empty")
	}
	if size > MaxUploadBytes {
		return nil, reject("a skill package may be up to %d MB, and that one is larger", MaxUploadBytes>>20)
	}

	archive, err := zip.NewReader(r, size)
	if err != nil {
		return nil, reject("that file is not a zip archive")
	}

	entries, err := usable(archive.File)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, reject("that archive holds no files")
	}
	if len(entries) > MaxFiles {
		return nil, reject("a skill package may hold up to %d files, and that one holds %d", MaxFiles, len(entries))
	}

	root, err := skillRoot(entries)
	if err != nil {
		return nil, err
	}

	pkg := &model.SkillPackage{}
	total := int64(0)
	for _, entry := range entries {
		relative, err := within(entry.Name, root)
		if err != nil {
			return nil, err
		}

		// The header's own claim, checked first because it is free. It is not
		// trusted: the real bound is on the bytes actually read, below, since a
		// lying header is precisely the archive this guard exists for.
		if entry.UncompressedSize64 > MaxFileBytes {
			return nil, reject("%s is larger than the %d MB a single file may be", relative, MaxFileBytes>>20)
		}

		content, err := contents(entry, relative)
		if err != nil {
			return nil, err
		}
		total += int64(len(content))
		if total > MaxExpandedBytes {
			return nil, reject("that package expands to more than the %d MB allowed", MaxExpandedBytes>>20)
		}

		pkg.Files = append(pkg.Files, describe(relative, content))
	}

	manifest := manifestOf(pkg.Files)
	if manifest == nil {
		// Unreachable via skillRoot, which already required one. Kept because
		// this function must not be able to return a package with no manifest
		// whatever else changes above it.
		return nil, reject("that package has no %s", Manifest)
	}
	front, err := frontmatter(manifest.Text)
	if err != nil {
		return nil, err
	}
	if err := checkName(front.Name, root); err != nil {
		return nil, err
	}
	if err := checkDescription(front.Description); err != nil {
		return nil, err
	}
	pkg.Name, pkg.Description = front.Name, front.Description

	// Sections come last, over the files as stored, so what is indexed is
	// provably what is kept rather than a second reading of the archive.
	for i := range pkg.Files {
		pkg.Files[i].Sections = sections(&pkg.Files[i])
	}
	// And the title after them, because the fallback is the manifest's own first
	// heading and the headings are what the parse just worked out. Nothing is
	// derived from the handle here: a title made by title-casing `pdf-processing`
	// would read "Pdf processing" and be a name the author never wrote.
	pkg.Title = title(front.Title, manifest)
	pkg.SHA256 = fingerprint(pkg.Files)
	return pkg, nil
}

// title is what a person should call this skill.
//
// What the author SAID, in `metadata.title`, comes first. Failing that, the
// manifest's own first top-level heading, which is what authors actually write
// at the top of a SKILL.md and is a name they chose rather than one we made.
// Failing both, nothing: the model falls back to the handle, and an empty title
// is an honest record that the package carried none.
func title(declared string, manifest *model.PackageFile) string {
	if declared != "" {
		return clip(declared)
	}
	for _, section := range manifest.Sections {
		// Path == Heading is a top-level heading: a deeper one carries its
		// parents in front of it ("Recovery > Caveats"). The first one is the
		// document's title; a later "## Usage" is not.
		if section.Heading != "" && section.Path == section.Heading {
			return clip(section.Heading)
		}
	}
	return ""
}

// clip holds a title to the column's width. Truncated rather than refused: a
// long title is somebody's prose, not a malformed package.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 255 {
		s = strings.TrimSpace(s[:255])
	}
	return s
}

// usable is the entries worth looking at, with everything hostile refused.
//
// Directories are dropped (the structure is rebuilt from the paths) and so is
// the bookkeeping a desktop adds when somebody compresses a folder: a Mac
// writes __MACOSX/._name beside every file, and reading those as part of the
// package would make "exactly one skill root" fail on the ordinary way a person
// produces a zip.
func usable(files []*zip.File) ([]*zip.File, error) {
	kept := make([]*zip.File, 0, len(files))
	for _, f := range files {
		name := f.Name
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		if junk(name) {
			continue
		}

		// A path is refused for what it IS, never cleaned up into something
		// acceptable. Silently rewriting `../../etc/passwd` into `etc/passwd`
		// would store a file under a name nobody sent.
		if strings.ContainsRune(name, 0) {
			return nil, reject("that archive contains a file name this cannot read")
		}
		if strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) || windowsAbsolute(name) {
			return nil, reject("%s is an absolute path, and a package may only contain its own files", name)
		}
		if strings.Contains(name, `\`) {
			return nil, reject("%s is not a valid path inside a package", name)
		}
		for _, element := range strings.Split(name, "/") {
			if element == ".." || element == "." || element == "" {
				return nil, reject("%s points outside the package", name)
			}
		}

		// What the entry is, as the archive recorded it. A symbolic link is
		// refused rather than followed, and anything that is not an ordinary
		// file (a device, a socket, a named pipe) has no meaning in a package.
		mode := f.Mode()
		if mode&modeSymlink != 0 {
			return nil, reject("%s is a symbolic link, and a package may only contain real files", name)
		}
		if !mode.IsRegular() {
			return nil, reject("%s is not an ordinary file", name)
		}

		kept = append(kept, f)
	}
	return kept, nil
}

// junk is what a desktop adds to an archive and a package does not contain.
func junk(name string) bool {
	if strings.HasPrefix(name, "__MACOSX/") || strings.Contains(name, "/__MACOSX/") {
		return true
	}
	base := path.Base(name)
	return base == ".DS_Store" || base == "Thumbs.db" || strings.HasPrefix(base, "._")
}

// windowsAbsolute catches C:/... and C:\..., which are absolute on the platform
// that wrote them and would be read as a relative path here.
func windowsAbsolute(name string) bool {
	if len(name) < 2 || name[1] != ':' {
		return false
	}
	c := name[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// skillRoot is the one directory the skill lives in, or "" when the manifest is
// at the top of the archive.
//
// Both are accepted, because both are what people send: compressing the skill's
// folder gives the first, and compressing its contents gives the second. What is
// refused is ambiguity, which is more than one manifest (two skills in one
// archive) or none at all.
func skillRoot(entries []*zip.File) (string, error) {
	roots := map[string]bool{}
	atTop := false
	for _, f := range entries {
		switch dir, base := path.Split(f.Name); {
		case base != Manifest && strings.EqualFold(base, Manifest):
			return "", reject("%s must be called exactly %q, and that package has %q", Manifest, Manifest, base)
		case base != Manifest:
			// not a manifest; it says nothing about the root
		case dir == "":
			atTop = true
		case strings.Count(strings.TrimSuffix(dir, "/"), "/") == 0:
			roots[strings.TrimSuffix(dir, "/")] = true
		default:
			// A manifest deeper than one directory down is a skill nested
			// inside something else, which is not a package we can name.
			return "", reject("%s is not at the top of the package", f.Name)
		}
	}

	switch {
	case atTop && len(roots) > 0, len(roots) > 1:
		return "", reject("that archive holds more than one skill; import them one at a time")
	case atTop:
		return "", nil
	case len(roots) == 1:
		for root := range roots {
			return root, nil
		}
	}
	return "", reject("that archive has no %s, so it is not a skill package", Manifest)
}

// within turns an archive path into the package-relative one, and refuses
// anything that is not under the root at all: with a root established, a file
// outside it belongs to no skill.
func within(name, root string) (string, error) {
	if root == "" {
		if len(name) > maxPathLength {
			return "", reject("%s is a longer path than a package may contain", name)
		}
		return name, nil
	}
	prefix := root + "/"
	if !strings.HasPrefix(name, prefix) {
		return "", reject("%s is outside the skill's own directory", name)
	}
	relative := strings.TrimPrefix(name, prefix)
	if relative == "" {
		return "", reject("%s is not a file", name)
	}
	if len(relative) > maxPathLength {
		return "", reject("%s is a longer path than a package may contain", relative)
	}
	return relative, nil
}

// contents reads one entry, bounded by what a file may be whatever its header
// claimed. The limit is read one byte PAST the ceiling: a file exactly at the
// ceiling is fine, and one byte more has to be distinguishable from it.
func contents(entry *zip.File, relative string) ([]byte, error) {
	rc, err := entry.Open()
	if err != nil {
		return nil, reject("%s could not be read out of the archive", relative)
	}
	defer func() { _ = rc.Close() }()

	content, err := io.ReadAll(io.LimitReader(rc, MaxFileBytes+1))
	if err != nil {
		return nil, reject("%s could not be read out of the archive", relative)
	}
	if len(content) > MaxFileBytes {
		return nil, reject("%s is larger than the %d MB a single file may be", relative, MaxFileBytes>>20)
	}
	return content, nil
}

// manifestOf finds SKILL.md among the read files.
func manifestOf(files []model.PackageFile) *model.PackageFile {
	for i := range files {
		if files[i].Path == Manifest {
			return &files[i]
		}
	}
	return nil
}

// fingerprint hashes the whole package: every path with the hash of its
// contents, in path order.
//
// Path order and not archive order, because the archive's order is an accident
// of whatever wrote it. Two uploads of the same files must agree on this, or a
// re-import would create a second version of an identical package every time.
// A single changed byte in any file, a renamed file, an added file and a removed
// file each change it.
func fingerprint(files []model.PackageFile) string {
	lines := make([]string, 0, len(files))
	for _, f := range files {
		lines = append(lines, f.Path+"\x00"+f.SHA256+"\n")
	}
	sort.Strings(lines)

	sum := sha256.New()
	for _, line := range lines {
		sum.Write([]byte(line))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func hash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
