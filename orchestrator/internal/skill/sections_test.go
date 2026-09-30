package skill

import (
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
)

// Parsing a text file into passages.
//
// The two things that go wrong here are a heading that is not one (a shell
// comment inside a code block) and a boundary in the wrong place (a passage cut
// through the middle of an instruction). Both are asserted directly.

func parse(t *testing.T, path, text string) []model.PackageSection {
	t.Helper()
	f := &model.PackageFile{Path: path, Text: text}
	return sections(f)
}

func TestAMarkdownFileIsCutAtItsHeadings(t *testing.T) {
	got := parse(t, "references/recovery.md", `Some opening prose.

# Recovery

How to recover.

## Point-in-time recovery

Pick a timestamp.

### Caveats

It is slow.

## Full restore

Copy everything.
`)

	want := []struct {
		heading   string
		path      string
		lineStart int
		lineEnd   int
	}{
		{"", "", 1, 2},
		{"Recovery", "Recovery", 3, 6},
		{"Point-in-time recovery", "Recovery > Point-in-time recovery", 7, 10},
		{"Caveats", "Recovery > Point-in-time recovery > Caveats", 11, 14},
		{"Full restore", "Recovery > Full restore", 15, 17},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d sections, want %d: %v", len(got), len(want), headingsOf(got))
	}
	for i, w := range want {
		if got[i].Heading != w.heading {
			t.Errorf("section %d heading = %q, want %q", i, got[i].Heading, w.heading)
		}
		// The path is what places a passage without opening the file, and it is
		// the reason a deeper heading is not just a name.
		if got[i].Path != w.path {
			t.Errorf("section %d path = %q, want %q", i, got[i].Path, w.path)
		}
		if got[i].LineStart != w.lineStart || got[i].LineEnd != w.lineEnd {
			t.Errorf("section %d lines = %d-%d, want %d-%d",
				i, got[i].LineStart, got[i].LineEnd, w.lineStart, w.lineEnd)
		}
	}

	// The body starts at the heading itself, so the line range and the text
	// agree about where the passage is. A body offset by one from its own range
	// is a citation that points at the wrong place.
	if !strings.HasPrefix(got[1].Body, "# Recovery") {
		t.Errorf("the body does not begin at its heading: %q", got[1].Body)
	}
	if !strings.Contains(got[1].Body, "How to recover.") {
		t.Errorf("the body lost its content: %q", got[1].Body)
	}
	if strings.Contains(got[1].Body, "Point-in-time") {
		t.Errorf("the body ran into the next section: %q", got[1].Body)
	}
}

func TestAHashInsideACodeBlockIsNotAHeading(t *testing.T) {
	// The case that matters most in practice: a skill is mostly examples, and a
	// shell comment is a # at the start of a line. Read as headings, one code
	// block becomes four passages, each holding a fragment of a command.
	got := parse(t, Manifest, `# Running it

    Some indented code:

`+"```bash\n"+`# install the thing
apt-get install thing

# then run it
thing --now
`+"```"+`

Done.
`)

	if len(got) != 1 {
		t.Fatalf("got %d sections, want 1: %v", len(got), headingsOf(got))
	}
	if !strings.Contains(got[0].Body, "apt-get install thing") || !strings.Contains(got[0].Body, "thing --now") {
		t.Errorf("the code block was broken up: %q", got[0].Body)
	}
}

func TestATildeFenceIsAFenceToo(t *testing.T) {
	got := parse(t, Manifest, `# Title

~~~
# not a heading
~~~

## Real
`)
	if len(got) != 2 {
		t.Fatalf("got %d sections, want 2: %v", len(got), headingsOf(got))
	}
	if got[1].Heading != "Real" {
		t.Errorf("second heading = %q", got[1].Heading)
	}
}

func TestABacktickFenceIsNotClosedByALongerTildeRun(t *testing.T) {
	// A fence is closed by its OWN character. Reading any fence as closing any
	// other would reopen heading detection in the middle of a code block.
	got := parse(t, Manifest, "# Title\n\n```\n~~~\n# still code\n```\n\n## After\n")
	if len(got) != 2 {
		t.Fatalf("got %d sections, want 2: %v", len(got), headingsOf(got))
	}
	if got[1].Heading != "After" {
		t.Errorf("second heading = %q", got[1].Heading)
	}
}

func TestAnIndentedHashIsNotAHeading(t *testing.T) {
	// Four spaces is an indented code block, where # is a comment.
	got := parse(t, Manifest, "# Title\n\n    # indented, so code\n\nprose\n")
	if len(got) != 1 {
		t.Fatalf("got %d sections, want 1: %v", len(got), headingsOf(got))
	}
}

func TestAHashWithNoSpaceIsNotAHeading(t *testing.T) {
	got := parse(t, Manifest, "# Title\n\n#hashtag is not a heading\n")
	if len(got) != 1 {
		t.Fatalf("got %d sections, want 1: %v", len(got), headingsOf(got))
	}
}

func TestAClosedAtxHeadingKeepsItsText(t *testing.T) {
	got := parse(t, Manifest, "## Backup ##\n\nDo it.\n")
	if len(got) != 1 {
		t.Fatalf("got %d sections: %v", len(got), headingsOf(got))
	}
	if got[0].Heading != "Backup" {
		t.Errorf("heading = %q, want %q", got[0].Heading, "Backup")
	}
}

func TestASeventhLevelIsNotAHeading(t *testing.T) {
	got := parse(t, Manifest, "# Title\n\n####### seven hashes\n")
	if len(got) != 1 {
		t.Fatalf("got %d sections, want 1: %v", len(got), headingsOf(got))
	}
}

func TestAHeadingWithNothingUnderItIsStillFound(t *testing.T) {
	// A stub is a real thing to find: the heading says the skill has a section
	// about this, and dropping it hides that.
	got := parse(t, Manifest, "# One\n\ncontent\n\n## Empty\n")
	if len(got) != 2 {
		t.Fatalf("got %d sections, want 2: %v", len(got), headingsOf(got))
	}
	if got[1].Heading != "Empty" {
		t.Errorf("heading = %q", got[1].Heading)
	}
}

func TestAFileThatBeginsWithAHeadingHasNoEmptyPreamble(t *testing.T) {
	got := parse(t, Manifest, "# First\n\ncontent\n")
	if len(got) != 1 {
		t.Fatalf("got %d sections, want 1: %v", len(got), headingsOf(got))
	}
	if got[0].Heading != "First" {
		t.Errorf("heading = %q", got[0].Heading)
	}
}

func TestAScriptIsIndexedOnceWithItsPath(t *testing.T) {
	got := parse(t, "scripts/extract.py", "import sys\n\n# a comment\nprint(1)\n")
	if len(got) != 1 {
		t.Fatalf("got %d sections, want 1", len(got))
	}
	// A script has no headings, so what places it is its path.
	if got[0].Heading != "scripts/extract.py" || got[0].Path != "scripts/extract.py" {
		t.Errorf("heading = %q, path = %q", got[0].Heading, got[0].Path)
	}
	if got[0].LineStart != 1 || got[0].LineEnd != 4 {
		t.Errorf("lines = %d-%d, want 1-4", got[0].LineStart, got[0].LineEnd)
	}
	if !strings.Contains(got[0].Body, "print(1)") {
		t.Errorf("body = %q", got[0].Body)
	}
}

func TestBinaryIsNotIndexed(t *testing.T) {
	f := &model.PackageFile{Path: "assets/t.docx", Bytes: []byte{0, 1, 2}}
	if got := sections(f); len(got) != 0 {
		t.Errorf("a binary file produced %d sections", len(got))
	}
}

func TestAnEmptyFileIsNotIndexed(t *testing.T) {
	if got := parse(t, "references/empty.md", ""); len(got) != 0 {
		t.Errorf("an empty file produced %d sections", len(got))
	}
}

func TestALongPassageIsSplitAtAParagraphAndNeverInsideACodeBlock(t *testing.T) {
	// One heading, a body well past the ceiling, and a code block sitting in the
	// middle of it. The split must land on a blank line between paragraphs, and
	// the fenced block must come back whole.
	paragraph := strings.Repeat("Some prose that goes on. ", 40) + "\n"
	fenced := "```sh\n" + strings.Repeat("echo keep-this-together\n", 30) + "```\n"

	body := "# Long\n\n"
	for i := 0; i < 8; i++ {
		body += paragraph + "\n"
	}
	body += fenced + "\n"
	for i := 0; i < 8; i++ {
		body += paragraph + "\n"
	}

	got := parse(t, Manifest, body)
	if len(got) < 2 {
		t.Fatalf("a %d byte passage was not split: %d section(s)", len(body), len(got))
	}
	for i, s := range got {
		if s.Heading != "Long" || s.Path != "Long" {
			t.Errorf("chunk %d lost its heading: %q", i, s.Heading)
		}
		// Every chunk must start and end between paragraphs, so no chunk may
		// begin or end inside the fence.
		if strings.Count(s.Body, "```")%2 != 0 {
			t.Errorf("chunk %d cuts through a code fence:\n%s", i, s.Body)
		}
	}

	// The code block survived in one piece, in exactly one chunk.
	whole := 0
	for _, s := range got {
		if strings.Count(s.Body, "echo keep-this-together") == 30 {
			whole++
		}
	}
	if whole != 1 {
		t.Errorf("the code block is in %d chunks whole, want exactly 1", whole)
	}

	// The ranges must stay in order and not overlap: they are what a citation
	// points at.
	for i := 1; i < len(got); i++ {
		if got[i].LineStart <= got[i-1].LineEnd {
			t.Errorf("chunk %d starts at %d, inside chunk %d which ends at %d",
				i, got[i].LineStart, i-1, got[i-1].LineEnd)
		}
	}
}

func TestEveryPassageCarriesItsOwnHash(t *testing.T) {
	got := parse(t, Manifest, "# One\n\nalpha\n\n# Two\n\nbeta\n")
	if len(got) != 2 {
		t.Fatalf("got %d sections", len(got))
	}
	if got[0].SHA256 == "" || got[1].SHA256 == "" {
		t.Fatal("a passage has no hash")
	}
	if got[0].SHA256 == got[1].SHA256 {
		t.Error("two different passages share a hash")
	}
}

func TestWindowsLineEndingsDoNotBecomePartOfTheHeading(t *testing.T) {
	got := parse(t, Manifest, "# Title\r\n\r\ncontent\r\n")
	if len(got) != 1 {
		t.Fatalf("got %d sections", len(got))
	}
	if got[0].Heading != "Title" {
		t.Errorf("heading = %q, want %q (a carriage return is not part of the name)", got[0].Heading, "Title")
	}
}

func headingsOf(sections []model.PackageSection) []string {
	out := []string{}
	for _, s := range sections {
		out = append(out, s.Heading+" ["+s.Path+"]")
	}
	return out
}
