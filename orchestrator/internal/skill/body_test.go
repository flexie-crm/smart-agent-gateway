package skill

import "testing"

func TestBodyStripsOnlyTheFrontmatter(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"ordinary",
			"---\nname: x\ndescription: y\n---\n\n# Title\n\nDo it.\n",
			"# Title\n\nDo it.\n"},
		{"a byte order mark, which survives a copy between editors",
			"\ufeff---\nname: x\n---\n\n# Title\n",
			"# Title\n"},
		{"closed with dots, which YAML allows",
			"---\nname: x\n...\n\nBody.\n", "Body.\n"},
		{"a rule further down is prose, not a second block",
			"---\nname: x\n---\n\nOne\n\n---\n\nTwo\n",
			"One\n\n---\n\nTwo\n"},
		{"no frontmatter at all is left alone",
			"# Title\n\nDo it.\n", "# Title\n\nDo it.\n"},
		{"a document that opens on a rule is not carrying frontmatter",
			"---\n\nJust a rule above me.\n", "---\n\nJust a rule above me.\n"},
		{"never closed strips nothing: the file is malformed and showing it says more",
			"---\nname: x\nno closing line\n", "---\nname: x\nno closing line\n"},
		{"empty", "", ""},
	} {
		if got := Body(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
