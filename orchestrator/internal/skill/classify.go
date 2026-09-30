package skill

import (
	"io/fs"
	"mime"
	"path"
	"strings"
	"unicode/utf8"

	"flexie.io/sag/internal/model"
)

// modeSymlink is named here so the reader can ask what an entry IS without
// importing the operating system's package to do it.
const modeSymlink = fs.ModeSymlink

// describe turns one file's bytes into the row that will be stored: what the
// file is for, what kind of data it holds, and which column it goes in.
func describe(relative string, content []byte) model.PackageFile {
	file := model.PackageFile{
		Path:     relative,
		FileType: role(relative),
		Size:     int64(len(content)),
		SHA256:   hash(content),
	}
	if text(content) {
		// Text, so it is stored as text and can be searched. The bytes are kept
		// exactly as they arrived: the hash above is of the original, and a file
		// whose line endings were "helpfully" normalised would no longer match
		// what the sender sent.
		file.Text = string(content)
		file.MIMEType = mimeOf(relative, true)
		return file
	}
	file.Bytes = content
	file.MIMEType = mimeOf(relative, false)
	return file
}

// role is what a file is FOR, read from where it sits in the package.
//
// The format puts meaning in the directory names, so that is what is read. It
// deliberately does not look at the extension: a .md under scripts/ is part of
// how the script is used, and a .py under references/ is being shown as an
// example rather than offered to run.
func role(relative string) string {
	if relative == Manifest {
		return model.SkillFileSkill
	}
	switch first(relative) {
	case "references":
		return model.SkillFileReference
	case "scripts":
		return model.SkillFileScript
	case "assets":
		return model.SkillFileAsset
	default:
		// In the package, in none of the named directories. Kept, because the
		// format says an unknown file is preserved rather than discarded, and
		// indexed if it is text, because it is probably prose.
		return model.SkillFileOther
	}
}

func first(relative string) string {
	if i := strings.IndexByte(relative, '/'); i >= 0 {
		return relative[:i]
	}
	return ""
}

// text reports whether these bytes are text.
//
// It asks the CONTENT, not the extension. An extension is what somebody named a
// file; the content is what it is, and the decision here settles which column
// the file is stored in and whether it can be searched at all. Getting it from a
// list of known extensions means every unlisted script is filed as a binary blob
// nobody can search.
//
// The test is the one every version control system uses: valid UTF-8 with no NUL
// byte. A NUL is the thing text never contains and binary formats almost always
// do, and invalid UTF-8 cannot be stored in a utf8mb4 column anyway. An empty
// file is text: it has no bytes to disagree about, and storing nothing as a blob
// would make an empty placeholder unsearchable for no reason.
func text(content []byte) bool {
	if len(content) == 0 {
		return true
	}
	for _, b := range content {
		if b == 0 {
			return false
		}
	}
	return utf8.Valid(content)
}

// known is the type of the file extensions a skill actually carries, so the
// answer is the same on every machine.
//
// The standard library's table is built partly from the system's own mime.types
// file, which exists on one developer's laptop and not on another's, and is
// missing most of these everywhere. A package that imported as text/x-python on
// a Mac and application/octet-stream in a container would be a difference nobody
// could explain from the code.
var known = map[string]string{
	".md":       "text/markdown; charset=utf-8",
	".markdown": "text/markdown; charset=utf-8",
	".txt":      "text/plain; charset=utf-8",
	".py":       "text/x-python",
	".sh":       "application/x-sh",
	".bash":     "application/x-sh",
	".js":       "text/javascript; charset=utf-8",
	".ts":       "text/typescript; charset=utf-8",
	".json":     "application/json",
	".yaml":     "application/yaml",
	".yml":      "application/yaml",
	".toml":     "application/toml",
	".csv":      "text/csv; charset=utf-8",
	".sql":      "application/sql",
	".rb":       "text/x-ruby",
	".go":       "text/x-go",
	".rs":       "text/x-rust",
	".html":     "text/html; charset=utf-8",
	".css":      "text/css; charset=utf-8",
	".xml":      "application/xml",
	".pdf":      "application/pdf",
	".png":      "image/png",
	".jpg":      "image/jpeg",
	".jpeg":     "image/jpeg",
	".gif":      "image/gif",
	".svg":      "image/svg+xml",
	".webp":     "image/webp",
	".zip":      "application/zip",
	".docx":     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx":     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx":     "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// mimeOf is the file's type: our own table first so the answer is deterministic,
// the standard library's for anything it happens to know, and a last resort that
// says only which half of the world the file is in.
func mimeOf(relative string, isText bool) string {
	ext := strings.ToLower(path.Ext(relative))
	if t, ok := known[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	if isText {
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}
