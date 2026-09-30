package model

import "time"

// A skill is a procedure somebody wrote down, in the Agent Skills package
// format, imported and kept here.
//
//	Skill            "pdf-processing"
//	  Version          immutable, numbered, one of them active
//	    File             SKILL.md, scripts/extract.py, assets/template.docx
//	      Section          a searchable passage of a text file
//
// The vocabulary is the format's own, deliberately: somebody who has written a
// skill for a compatible implementation should recognise every word here.
//
// Two rules hold the whole thing up, and both are enforced rather than
// documented:
//
//   - a VERSION IS IMMUTABLE. An updated upload adds a version beside the
//     active one, never over it, so "which version answered that" stays a fact;
//   - the SECTIONS ARE DERIVED. They are a search index over the files and can
//     always be rebuilt from them, which is why an export reads the files and
//     never the sections.

// What a file is FOR, which is not the same as what kind of data it holds. A
// script is text and an asset is bytes, but the reason each is in the package is
// what a reader needs to know.
const (
	SkillFileSkill     = "skill"     // SKILL.md itself
	SkillFileReference = "reference" // references/, the deep material
	SkillFileScript    = "script"    // scripts/, kept as clear source
	SkillFileAsset     = "asset"     // assets/, templates and binaries
	SkillFileOther     = "other"     // in the package, in none of the above
)

// Where a version came from. Stage 1 writes only imported; manual and learned
// are what the editing and the learning loop will write.
const (
	SkillSourceImported = "imported"
	SkillSourceManual   = "manual"
	SkillSourceLearned  = "learned"
)

// A skill's own status: whether it is available at all.
const (
	SkillDraft    = "draft"
	SkillActive   = "active"
	SkillDisabled = "disabled"
	SkillArchived = "archived"
)

// A version's status. Exactly one version of a skill is active, and it is the
// one ai_skills.active_version_id points at. The two are written together, in
// one transaction, by one method.
const (
	SkillVersionDraft    = "draft"
	SkillVersionActive   = "active"
	SkillVersionRejected = "rejected"
	SkillVersionArchived = "archived"
)

// Skill is the identity: one row per skill per workspace, and the pointer at
// whichever version is live.
type Skill struct {
	ID          int64 `json:"id"`
	WorkspaceID int64 `json:"workspace_id"`
	// Name is the HANDLE: lowercase, hyphenated, and equal to the package's own
	// directory name, because the Agent Skills format says so. It is what the
	// agent addresses the skill by, and it is not a name for a person.
	Name string `json:"name"`
	// Who created it, and who last edited it. The second pair is here because
	// the Title and Description below can be changed by a person, which is the
	// only thing on this row that can.
	Authored
	Edited
	// Title and Description are THE SKILL'S OWN, and columns of this table.
	//
	// They are seeded from the package's frontmatter, because that is where the
	// author wrote what the thing is called and what it is for, and from then on
	// they are an administrator's to change: a skill that arrives called "PDF
	// Toolkit" can be called what this workspace calls it.
	//
	// They used to be the live version's, read through a join, to avoid a skill
	// named after whatever was imported LAST (roll back to version 2 and the row
	// still described version 5). That worry is answered better by these being
	// the skill's rather than a copy of anything: a rollback changes which
	// version is live and does not touch them. The version keeps its own
	// immutable pair, so what each package said is still on record.
	//
	// An import takes a new version's pair only when these still equal the
	// version that was live, which is the proof nobody has edited them; see
	// sqlstore.findOrCreate. No column records "edited", because the old version
	// already answers it.
	//
	// Empty is a real state: a package may carry no title, and the label falls
	// back to the handle.
	Title       string `json:"title"`
	Description string `json:"description"`
	// ActiveVersionID is zero when no version is active, which is a real state:
	// a skill whose only version was rejected has an identity and nothing to run.
	ActiveVersionID int64     `json:"active_version_id"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`

	// Version is the active version, when the caller asked for it.
	Version *SkillVersion `json:"version,omitempty"`

	// Counts are what makes a list worth reading. A skill with one file looks
	// exactly like a skill with forty until something says so.
	Versions int `json:"versions"`
	Files    int `json:"files"`
	Sections int `json:"sections"`
}

// Label is what to print. The title the package carried, or the handle when it
// carried none.
//
// A method rather than a column that is never empty, so the row stays an honest
// record of the package and every caller still has one thing to read. Two
// callers each writing the fallback is how a screen and a prompt end up
// disagreeing about what a skill is called.
func (s *Skill) Label() string {
	if s.Title != "" {
		return s.Title
	}
	return s.Name
}

// SkillVersion is one immutable snapshot of a package.
type SkillVersion struct {
	ID      int64 `json:"id"`
	SkillID int64 `json:"skill_id"`
	// ParentVersionID is what this version was built from: the version it
	// superseded on an update, or zero for the first import. It is the lineage,
	// and it is why a rollback knows where to go back to.
	ParentVersionID int64  `json:"parent_version_id"`
	Number          int    `json:"number"`
	Source          string `json:"source"`
	Status          string `json:"status"`
	// Who imported THIS version, which is a different fact from who created the
	// skill: a person imports one and an agent improves it, and crediting the
	// person for the agent's version would be wrong in the one place somebody
	// goes to find out what changed.
	//
	// No Edited here, and there must not be: a version cannot be changed.
	Authored
	// Title and Description are this package's own, out of its frontmatter. The
	// title is empty when it carried none (see the reader: metadata.title, then
	// the manifest's first heading, then nothing).
	Title         string `json:"title"`
	Description   string `json:"description"`
	ChangeSummary string `json:"change_summary"`
	// PackageSHA256 is the whole package, hashed. Unique per skill, so
	// re-uploading the identical package is the SAME version rather than a
	// second one: an import is idempotent on the bytes.
	PackageSHA256 string     `json:"package_sha256"`
	ValidatedAt   *time.Time `json:"validated_at"`
	ActivatedAt   *time.Time `json:"activated_at"`
	CreatedAt     time.Time  `json:"created_at"`

	Files    int `json:"files"`
	Sections int `json:"sections"`
}

// SkillFile is one file of one version, complete.
//
// Text goes in Text and bytes go in Binary, never both: a script is source
// somebody reads and an asset is a payload something opens, and storing a
// script as bytes would mean it could not be searched.
type SkillFile struct {
	ID int64 `json:"id"`
	// SkillID beside VersionID, the shape BrainDocument uses for its brain: it
	// takes a join out of every scoped read. It cannot drift, because a version
	// is immutable and its files are written once, with it.
	SkillID   int64  `json:"skill_id"`
	VersionID int64  `json:"version_id"`
	Path      string `json:"path"`
	FileType  string `json:"file_type"`
	MIMEType  string `json:"mime_type"`
	// Text is empty for a binary file, and is only loaded when the caller asked
	// for one file. A version's file LIST carries no content: a package may hold
	// a twenty megabyte template nobody is reading.
	Text string `json:"text,omitempty"`
	// Bytes never reaches JSON. They are handed back by their own route, as a
	// download, because base64 in a screen's payload is a file nobody can use
	// arriving in a form nothing can read.
	Bytes []byte `json:"-"`
	// Binary says which of the two this file is, and it is READ FROM THE ROW
	// rather than worked out from whether the content happens to be loaded.
	// A listing carries no content at all, and a screen still has to know
	// whether to offer a file to read or a file to download; deriving it from
	// an empty Text would call every listed file binary.
	Binary    bool      `json:"binary"`
	SizeBytes int64     `json:"size_bytes"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`

	// Sections is how many searchable passages this file produced. Zero is
	// meaningful: a binary asset is deliberately not indexed.
	Sections int `json:"sections"`
}

// SkillSection is a searchable passage of a text file: a heading, its body, and
// where in the file it came from.
//
// Derived, and therefore disposable. Nothing outside the index may depend on a
// section's id surviving a re-parse.
type SkillSection struct {
	ID        int64  `json:"id"`
	SkillID   int64  `json:"skill_id"`
	VersionID int64  `json:"version_id"`
	FileID    int64  `json:"file_id"`
	Heading   string `json:"heading"`
	// Path is the heading PATH, so a passage can be placed without opening the
	// file: "Recovery > Point-in-time recovery".
	Path string `json:"path"`
	Body string `json:"body,omitempty"`
	// LineStart and LineEnd are 1-based and inclusive, counted in the file as
	// uploaded, so a passage can be pointed at rather than described.
	LineStart int `json:"line_start"`
	LineEnd   int `json:"line_end"`
	Sequence  int `json:"sequence"`
	// SizeBytes is how big the passage is, so a list of them can be read
	// without carrying every body: a version may hold hundreds, and the place
	// to READ one is the file it came from, with its line range.
	SizeBytes int       `json:"size_bytes"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`

	// File is the path of the file this came from, when a list needs to say so.
	File string `json:"file,omitempty"`
}

// --- what arrived, before it is stored -------------------------------------------

// SkillPackage is a skill as it ARRIVED: extracted, validated against the
// format, parsed, and not yet written anywhere.
//
// It is a separate type from the rows above on purpose. This one has no ids, no
// version and no workspace: it is the contents of a zip somebody sent, and the
// store is what turns it into a version. Reusing the row types here would mean
// every one of them carried fields that are meaningless half the time.
type SkillPackage struct {
	Name string
	// Title is the name for a person: metadata.title if the author set one, the
	// manifest's first heading if not, and empty if neither.
	Title       string
	Description string
	// SHA256 covers the whole package: every path and every byte, in a fixed
	// order. Two uploads of the same files agree on it whatever order the zip
	// happened to list them in, and any change to any byte breaks it.
	SHA256 string
	Files  []PackageFile
}

// PackageFile is one file of an arriving package.
type PackageFile struct {
	Path     string
	FileType string
	MIMEType string
	// Text or Bytes, never both: a nil Bytes IS the statement that this file is
	// text, which is what decides the column it is stored in.
	Text   string
	Bytes  []byte
	Size   int64
	SHA256 string
	// Sections are what the parser made of this file. Empty for a binary asset,
	// which is deliberately not indexed.
	Sections []PackageSection
}

// PackageSection is a parsed passage, before it has a row.
type PackageSection struct {
	Heading   string
	Path      string
	Body      string
	LineStart int
	LineEnd   int
	SHA256    string
}
