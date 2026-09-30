package skill

import (
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// The manifest's frontmatter.
//
// Only two fields are READ, and nothing is ever removed. The format says an
// implementation preserves fields it does not understand, and here that is
// structural rather than careful: SKILL.md is stored whole, byte for byte, so
// `license`, `compatibility`, `metadata`, `allowed-tools` and anything a future
// version of the format adds survive an import and an export without this file
// knowing they exist. The two fields below are parsed because the runtime needs
// them: a skill has to have a name to be addressed by and a description to be
// chosen by.

// front is what we read out of the manifest.
type front struct {
	Name        string
	Description string
	// Title is a name for a PERSON, and the format has no field for one: `name`
	// is an identifier (lowercase, hyphens, and it must equal the directory
	// name), so a list of skills reads like a directory listing.
	//
	// The specification's answer to a property it does not define is `metadata`,
	// which it says outright is where "clients can use this to store additional
	// properties not defined by the Agent Skills spec". So an author who wants
	// to be called something writes it there, and a package carrying one still
	// works in every other implementation, because it is an optional map they
	// all ignore.
	//
	// Empty when the author gave none. Read() then falls back to the manifest's
	// own first heading, and the model falls back to the handle after that, so
	// there is always something to print and none of it is invented.
	Title string
}

// frontmatter splits the YAML block off the top of the manifest and reads it.
func frontmatter(manifest string) (front, error) {
	// A byte order mark is invisible, survives a copy between editors, and would
	// make the first line "\ufeff---" and the file look like it has no
	// frontmatter at all. Dropped here rather than earlier: the stored bytes are
	// what arrived.
	body := strings.TrimPrefix(manifest, "\ufeff")

	lines := strings.Split(body, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r \t") != "---" {
		return front{}, reject("%s must begin with a --- line and its YAML frontmatter", Manifest)
	}

	end := -1
	for i := 1; i < len(lines); i++ {
		switch strings.TrimRight(lines[i], "\r \t") {
		case "---", "...":
			end = i
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return front{}, reject("the frontmatter in %s is never closed by a --- line", Manifest)
	}

	// Into a map, not a struct with unknown fields refused: an unknown key is
	// somebody else's extension, and the format says to keep it.
	var fields map[string]any
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &fields); err != nil {
		return front{}, reject("the frontmatter in %s is not valid YAML", Manifest)
	}

	return front{
		Name:        asString(fields["name"]),
		Description: asString(fields["description"]),
		Title:       asString(mapping(fields["metadata"])["title"]),
	}, nil
}

// mapping reads a field that must be a map, and answers with an empty one for
// anything else. YAML lets `metadata` be a string, a list or nothing at all,
// and none of those has a title in it.
func mapping(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// asString reads a field that must be text. A YAML value can be a number, a
// list or a map, and `name: 12` must read as a wrong name rather than as the
// string "12": accepting it would let a skill be addressed by something its
// author never wrote.
func asString(value any) string {
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// checkName holds the name to the format's rule, and to the directory it
// arrived in.
//
// The directory match is the half worth explaining. A package whose folder says
// `pdf-tools` and whose manifest says `pdf-processing` has two names, and
// everything downstream picks one: the export writes the manifest's, a person
// looking at the archive reads the folder's. Refusing it makes the sender decide
// which one they meant. It is only checked when there IS a directory: an archive
// of the skill's contents has nothing to disagree with.
func checkName(name, root string) error {
	switch {
	case name == "":
		return reject("%s has no name in its frontmatter", Manifest)
	case utf8.RuneCountInString(name) > 64:
		return reject("a skill name may be up to 64 characters, and %q is longer", name)
	}
	for _, r := range name {
		lower := r >= 'a' && r <= 'z'
		digit := r >= '0' && r <= '9'
		if !lower && !digit && r != '-' {
			return reject("a skill name may only hold lowercase letters, numbers and hyphens, and %q does not", name)
		}
	}
	// The three rules beyond the alphabet, all of them the specification's own.
	// They are not decoration: the reference validator refuses these, so a
	// package we accepted and another implementation rejected would be a package
	// that imported here and worked nowhere else.
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return reject("a skill name may not start or end with a hyphen, and %q does", name)
	}
	if strings.Contains(name, "--") {
		return reject("a skill name may not hold two hyphens in a row, and %q does", name)
	}
	if root != "" && name != root {
		return reject("the skill is named %q in %s and its directory is called %q; the two must match", name, Manifest, root)
	}
	return nil
}

func checkDescription(description string) error {
	switch {
	case description == "":
		return reject("%s has no description in its frontmatter, and the description is how a skill is chosen", Manifest)
	case utf8.RuneCountInString(description) > 1024:
		return reject("a skill description may be up to 1024 characters, and that one is longer")
	}
	return nil
}

// Body is the manifest without its frontmatter.
//
// The frontmatter is machine-readable metadata (the handle, the description an
// agent selects on, a licence) and it is the first thing in every SKILL.md, so
// anything that shows the document as prose would open on a block of YAML. What
// reads it separately gets it separately: `frontmatter` above, and the stored
// title and description on the version.
//
// Only a block at the very TOP is taken, and only one: a `---` further down is a
// horizontal rule in somebody's prose, and a document that begins with one is
// not carrying frontmatter. Never closed means nothing is stripped, because the
// file is malformed and showing it as it is says more than showing nothing.
//
// The console has the same function in TypeScript (`withoutFrontmatter`), which
// is a second implementation and unavoidable: it renders the same document in a
// language this one cannot reach.
func Body(manifest string) string {
	body := strings.TrimPrefix(manifest, "\ufeff")
	lines := strings.Split(body, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r \t") != "---" {
		return body
	}
	for i := 1; i < len(lines); i++ {
		switch strings.TrimRight(lines[i], "\r \t") {
		case "---", "...":
			// Past the closing marker, and past the blank lines that usually
			// follow it, so the document does not open on empty space.
			start := i + 1
			for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
				start++
			}
			return strings.Join(lines[start:], "\n")
		}
	}
	return body
}
