package skill

// Editing a package where it lives, rather than uploading a new zip.
//
// A version is immutable, so an edit is not a change to one: it is a new
// version made of the files the old one held, with some of their contents
// replaced. What comes out is a `*model.SkillPackage`, exactly what Read
// produces from an archive, so everything downstream of it (the version row,
// the files, the sections, the hash, and every rule about what a package may
// be) is the same code on both routes.
//
// WHY THE SAME CONSTRUCTION AND NOT A SHORTCUT. It would be easy to write the
// edited text into the row and recompute two columns. The reason not to is that
// a package's parts are DERIVED: the type comes from where the file sits, the
// mime type from its name, whether it is text at all from its content, the
// passages from its headings, the title from the frontmatter or the first
// heading, and the package hash from every file's hash. A second place deriving
// any of those is a second opinion, and a stored package that disagrees with
// what an import of the same bytes would have produced is a whole class of bug
// with no symptom until something reads it.

import (
	"fmt"
	"sort"

	"flexie.io/sag/internal/model"
)

// Edit is new content for one file a version already holds.
type Edit struct {
	Path string
	Text string
}

// Rewrite builds the package a set of edits produces.
//
// `handle` is the skill's own handle, which the manifest's frontmatter must
// still name: a package whose `name` has changed is a different skill, not an
// edit of this one, and the handle is what the agent calls it by and what its
// directory on somebody's computer is named after.
//
// `held` is every file of the version being edited from, contents included.
// Every edit must name one of them: this is editing, and a package that gains
// or loses a file is a different shape of change.
//
// It answers with the package, and with the paths that actually differ. An edit
// that sets a file to what it already said is not a change, and a caller with
// an empty list of changes has nothing to save.
func Rewrite(handle string, held []model.PackageFile, edits []Edit) (*model.SkillPackage, []string, error) {
	if len(held) == 0 {
		return nil, nil, reject("that version holds no files")
	}
	if len(edits) == 0 {
		return nil, nil, reject("nothing was edited")
	}

	// By path, so an edit can be matched to the file it replaces and two edits
	// to one path are caught rather than silently resolved by ordering.
	replacement := make(map[string]string, len(edits))
	for _, e := range edits {
		if _, twice := replacement[e.Path]; twice {
			return nil, nil, reject("%s was edited twice in one save", e.Path)
		}
		replacement[e.Path] = e.Text
	}

	pkg := &model.SkillPackage{Files: make([]model.PackageFile, 0, len(held))}
	var changed []string
	var total int64
	for _, file := range held {
		text, edited := replacement[file.Path]
		if !edited {
			// Carried forward untouched, hash and all: it is the same bytes, so
			// it is the same file, and re-deriving it would be a chance to
			// differ from what was stored.
			total += file.Size
			pkg.Files = append(pkg.Files, file)
			continue
		}
		delete(replacement, file.Path)

		if file.Bytes != nil {
			return nil, nil, reject("%s is not text, so it cannot be edited here", file.Path)
		}
		if int64(len(text)) > MaxFileBytes {
			return nil, nil, reject("%s is larger than the %d MB a single file may be",
				file.Path, MaxFileBytes>>20)
		}
		// Described from its new content by the same function an import uses,
		// so the type, the mime type and the hash are worked out once in the
		// codebase and not twice.
		rebuilt := describe(file.Path, []byte(text))
		if rebuilt.Bytes != nil {
			// The content decides whether a file is text, so an edit can turn a
			// readable file into one that is not. Refused with the reason: a
			// stray NUL saved as a script would vanish from the reader and out
			// of the search index at once.
			return nil, nil, reject("%s would no longer be a text file: it holds bytes that are not text",
				file.Path)
		}
		total += rebuilt.Size
		pkg.Files = append(pkg.Files, rebuilt)
		if rebuilt.SHA256 != file.SHA256 {
			changed = append(changed, file.Path)
		}
	}
	// Anything left is an edit to a path this version does not have.
	if len(replacement) != 0 {
		return nil, nil, reject("%s is not in this version", anyPath(replacement))
	}
	if total > MaxExpandedBytes {
		return nil, nil, reject("that would make the package larger than the %d MB allowed",
			MaxExpandedBytes>>20)
	}

	manifest := manifestOf(pkg.Files)
	if manifest == nil {
		return nil, nil, reject("that version has no %s", Manifest)
	}
	front, err := frontmatter(manifest.Text)
	if err != nil {
		return nil, nil, err
	}
	// The handle rather than a directory name, which is the same check Read
	// makes against the archive's root: the manifest has to keep naming the
	// skill it belongs to.
	if err := checkName(front.Name, handle); err != nil {
		return nil, nil, err
	}
	if err := checkDescription(front.Description); err != nil {
		return nil, nil, err
	}
	pkg.Name, pkg.Description = front.Name, front.Description

	// Over the files as they will be stored, and for every file rather than
	// only the edited ones: a section carries the line numbers it was found at,
	// and a heading moved in one file does not move the passages of another,
	// but doing them all is cheap and makes the index provably a reading of
	// this package rather than a merge of two readings.
	for i := range pkg.Files {
		pkg.Files[i].Sections = sections(&pkg.Files[i])
	}
	pkg.Title = title(front.Title, manifestOf(pkg.Files))
	pkg.SHA256 = fingerprint(pkg.Files)

	sort.Strings(changed)
	return pkg, changed, nil
}

// anyPath names one of the paths left over, so the refusal says which.
func anyPath(left map[string]string) string {
	paths := make([]string, 0, len(left))
	for path := range left {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return fmt.Sprintf("%q", paths[0])
}
