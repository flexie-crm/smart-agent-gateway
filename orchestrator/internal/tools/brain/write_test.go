package brain

import (
	"strings"
	"testing"
)

// import splits a Markdown blob into documents on top-level "# " headings. The
// heading is the title, deeper headings stay inside the body, and text before
// the first heading has no title, so it is dropped.
func TestSplitMarkdown(t *testing.T) {
	md := "intro with no heading, ignored\n" +
		"# First\n" +
		"body one\n" +
		"## A subsection stays inside\n" +
		"still first\n" +
		"# Second\n" +
		"body two"

	docs := splitMarkdown(md)
	if len(docs) != 2 {
		t.Fatalf("expected two documents, got %d: %+v", len(docs), docs)
	}
	if docs[0].title != "First" {
		t.Fatalf("first title wrong: %q", docs[0].title)
	}
	if !strings.HasPrefix(docs[0].content, "body one") || !strings.Contains(docs[0].content, "## A subsection stays inside") {
		t.Fatalf("a subsection was not kept inside its document: %q", docs[0].content)
	}
	if docs[1].title != "Second" || docs[1].content != "body two" {
		t.Fatalf("second document wrong: %+v", docs[1])
	}

	if got := splitMarkdown("just text, no heading at all"); len(got) != 0 {
		t.Fatalf("text with no top-level heading produced a document: %+v", got)
	}
	// "# " with nothing after it is not a title, so it makes no document.
	if got := splitMarkdown("# \nbody"); len(got) != 0 {
		t.Fatalf("an empty heading produced a document: %+v", got)
	}
}
