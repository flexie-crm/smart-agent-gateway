package skill

import (
	"path"
	"strings"

	"flexie.io/sag/internal/model"
)

// Parsing a text file into searchable passages.
//
// Split on HEADINGS, not on a token count. A skill is instructions, and an
// instruction cut in half retrieves as two passages that each read as wrong: the
// step without its warning, and the warning without its step. A heading is where
// the author already decided one thing ends and the next begins, so it is the
// only boundary in the file that is not ours to invent.
//
// What this does NOT handle, stated rather than discovered later: setext
// headings, the underlined kind written as text with ==== or ---- beneath it.
// The cost of missing one is a LARGER section (the passage keeps its content and
// loses a boundary), never a lost or mangled one, and the alternative is reading
// every --- line as a possible heading in a format whose frontmatter is
// delimited by exactly that.

// maxSectionBytes is where a passage stops being one.
//
// A section is a retrieval unit: it is what comes back when the skill is
// searched, so it has to be small enough to read and big enough to act on. Past
// this it is split, and only ever at a blank line outside a code fence, so no
// split ever lands inside an instruction, a list item or a code block.
const maxSectionBytes = 8000

// sections is what a file contributes to the search index.
func sections(f *model.PackageFile) []model.PackageSection {
	if f.Bytes != nil || f.Text == "" {
		// Bytes are not indexed, and an empty file has nothing to index. A
		// binary asset is a template something opens, not prose somebody reads.
		return nil
	}
	if markdown(f.Path) {
		return headings(f.Text)
	}
	// A script, or any other text. One passage per file, which is what the
	// format asks for: splitting a script by function needs a parser per
	// language, and a wrong split in source code is worse than none.
	return split(block{heading: f.Path, path: f.Path, start: 1, lines: content(f.Text)})
}

func markdown(relative string) bool {
	switch strings.ToLower(path.Ext(relative)) {
	case ".md", ".markdown":
		return true
	default:
		return false
	}
}

// block is one heading's worth of a file, before it is measured.
type block struct {
	heading string
	path    string
	start   int // the 1-based line lines[0] sits on
	lines   []string
}

// content splits a file into lines, without the phantom last line a trailing
// newline produces. A file ending in a newline has as many lines as it has
// newlines, and counting the empty string after the final one would report every
// such file as one line longer than it is.
func content(text string) []string {
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// headings walks a Markdown file and cuts it at every ATX heading.
func headings(text string) []model.PackageSection {
	lines := content(text)
	var (
		blocks  []block
		current = block{start: 1}
		trail   []crumb
		fence   string
	)

	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		indent, rest := undent(line)

		// Inside a fenced block nothing is a heading. This is the case that
		// matters most in practice: a skill's SKILL.md is mostly code examples,
		// and a shell comment is a # at the start of a line.
		if fence != "" {
			if indent <= 3 && closes(rest, fence) {
				fence = ""
			}
			current.lines = append(current.lines, raw)
			continue
		}
		if indent <= 3 {
			if marker := opens(rest); marker != "" {
				fence = marker
				current.lines = append(current.lines, raw)
				continue
			}
			if level, title := atx(rest); level > 0 {
				if used(current) {
					blocks = append(blocks, current)
				}
				trail = descend(trail, level, title)
				current = block{
					heading: title,
					path:    trailPath(trail),
					start:   i + 1,
					lines:   []string{raw},
				}
				continue
			}
		}
		current.lines = append(current.lines, raw)
	}
	if used(current) {
		blocks = append(blocks, current)
	}

	out := []model.PackageSection{}
	for _, b := range blocks {
		out = append(out, split(b)...)
	}
	return out
}

// used says whether a block is worth keeping. A heading with nothing under it
// is kept (the heading itself is the information, and a stub is a real thing to
// find), but the run of lines before the first heading is only kept if it says
// something: in a manifest that is the frontmatter and the opening paragraph,
// and in a file that begins with a heading it is nothing at all.
func used(b block) bool {
	if b.heading != "" {
		return true
	}
	return strings.TrimSpace(strings.Join(b.lines, "\n")) != ""
}

// crumb is one level of the heading trail.
type crumb struct {
	level int
	title string
}

// descend puts a heading in its place in the trail: everything at its level or
// deeper is finished, and it takes their place.
func descend(trail []crumb, level int, title string) []crumb {
	for len(trail) > 0 && trail[len(trail)-1].level >= level {
		trail = trail[:len(trail)-1]
	}
	return append(trail, crumb{level: level, title: title})
}

// trailPath is the heading path, which is what lets a passage be placed without
// opening the file: "Recovery > Point-in-time recovery".
func trailPath(trail []crumb) string {
	titles := make([]string, 0, len(trail))
	for _, c := range trail {
		titles = append(titles, c.title)
	}
	crumbs := strings.Join(titles, " > ")
	if len(crumbs) > 1500 {
		// The column's width. Six levels of 500-character headings is prose
		// rather than structure, and a truncated path still places the passage.
		crumbs = strings.TrimSpace(crumbs[:1500])
	}
	return crumbs
}

// undent measures the leading spaces and hands back the rest.
//
// Up to three is still the line; four or more is an indented code block, where a
// # is a comment and not a heading. A leading tab counts as indented, which is
// the same reading the format's own renderers take.
func undent(line string) (int, string) {
	spaces := 0
	for spaces < len(line) && line[spaces] == ' ' {
		spaces++
	}
	if spaces < len(line) && line[spaces] == '\t' {
		return 4, line[spaces:]
	}
	return spaces, line[spaces:]
}

// atx reads a heading line: its level, and its text with the optional closing
// run of #s taken off.
func atx(rest string) (int, string) {
	level := 0
	for level < len(rest) && rest[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, ""
	}
	// "#hashtag" is not a heading: the hashes must be followed by a space, or be
	// the whole line.
	if level < len(rest) && rest[level] != ' ' && rest[level] != '\t' {
		return 0, ""
	}
	title := strings.TrimSpace(rest[level:])
	title = strings.TrimRight(title, "#")
	title = strings.TrimSpace(title)
	if len(title) > 500 {
		// The column's width. Truncated rather than refused: a very long heading
		// is somebody's prose, not a malformed package.
		title = strings.TrimSpace(title[:500])
	}
	return level, title
}

// opens reports the marker of a fence this line starts, or "".
func opens(rest string) string {
	for _, char := range []byte{'`', '~'} {
		run := 0
		for run < len(rest) && rest[run] == char {
			run++
		}
		if run >= 3 {
			// A backtick fence's info string may not contain a backtick, which
			// is what keeps `` `a` `` from reading as a fence.
			if char == '`' && strings.ContainsRune(rest[run:], '`') {
				continue
			}
			return rest[:run]
		}
	}
	return ""
}

// closes reports whether this line ends the open fence: the same character, at
// least as long, and nothing else on the line.
func closes(rest, fence string) bool {
	trimmed := strings.TrimRight(rest, " \t")
	if len(trimmed) < len(fence) {
		return false
	}
	char := fence[0]
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != char {
			return false
		}
	}
	return true
}

// split cuts an oversized block at paragraph boundaries, and leaves anything
// that fits alone.
//
// The cut is only ever a blank line outside a fence. A paragraph longer than the
// ceiling on its own is kept whole and over the ceiling, deliberately: half an
// instruction is worse than a long one.
func split(b block) []model.PackageSection {
	body := strings.Join(b.lines, "\n")
	if len(body) <= maxSectionBytes {
		return []model.PackageSection{passage(b, b.lines, b.start)}
	}

	out := []model.PackageSection{}
	var (
		chunk []string
		size  int
		at    = b.start
		fence string
	)
	for _, raw := range b.lines {
		line := strings.TrimRight(raw, "\r")
		indent, rest := undent(line)
		if fence == "" && indent <= 3 {
			if marker := opens(rest); marker != "" {
				fence = marker
			}
		} else if fence != "" && indent <= 3 && closes(rest, fence) {
			fence = ""
		}

		if fence == "" && size >= maxSectionBytes && strings.TrimSpace(line) == "" && len(chunk) > 0 {
			out = append(out, passage(b, chunk, at))
			at += len(chunk) + 1 // the blank line itself is the seam, and is dropped
			chunk, size = nil, 0
			continue
		}
		chunk = append(chunk, raw)
		size += len(raw) + 1
	}
	if len(chunk) > 0 {
		out = append(out, passage(b, chunk, at))
	}
	return out
}

func passage(b block, lines []string, start int) model.PackageSection {
	body := strings.Join(lines, "\n")
	return model.PackageSection{
		Heading:   b.heading,
		Path:      b.path,
		Body:      body,
		LineStart: start,
		LineEnd:   start + len(lines) - 1,
		SHA256:    hash([]byte(body)),
	}
}
