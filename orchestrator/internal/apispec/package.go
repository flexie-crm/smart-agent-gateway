package apispec

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Package writes a specification as an Agent Skills package: the bytes of a zip
// an administrator could have uploaded by hand.
//
// A zip and not a direct write to the store, because the ordinary import
// (app.ImportSkills) already validates a package, keeps it as an immutable
// version, indexes its passages and serves them one at a time. Going around it
// would be a second path into storage with its own rules to get wrong, and the
// generated package would be the one thing in the library nobody could check by
// reading it on the screen.
//
// The shape is decided by how a skill is READ. load_skill hands over SKILL.md
// plus an index of the other files, then one file at a time, and the search
// index splits on headings. So: a manifest that says how to call the tool and
// nothing else, one file per group of operations, and a heading per operation.
// A single file holding four hundred endpoints would be one part and would
// defeat both.
func Package(spec *Spec, handle string) ([]byte, error) {
	if spec == nil {
		return nil, fmt.Errorf("there is no specification to write")
	}
	handle = strings.TrimSpace(handle)
	if !handleOK.MatchString(handle) {
		return nil, fmt.Errorf("the name must be lowercase letters, numbers and hyphens, starting with a letter, such as billing-api")
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// One root directory whose name equals the manifest's `name`, which the
	// format requires and the reader checks.
	add := func(path, body string) error {
		w, err := zw.Create(handle + "/" + path)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(body))
		return err
	}

	if err := add("SKILL.md", manifest(spec, handle)); err != nil {
		return nil, err
	}
	for _, group := range spec.Groups {
		if err := add("reference/"+slug(group.Name)+".md", reference(spec, group)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var handleOK = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// manifest is what the model reads first: what this API is, how to call it with
// the tool, and where the endpoints are.
//
// Deliberately NOT a list of endpoints. That is the whole reason this is a
// skill: the list is in the reference files and is read when it is wanted.
func manifest(spec *Spec, handle string) string {
	var b strings.Builder
	// The frontmatter the format defines: name is an identifier and must equal
	// the directory, description is what a reader is offered. metadata.title is
	// where a name for a PERSON goes, the specification having no field for one.
	b.WriteString("---\n")
	b.WriteString("name: " + handle + "\n")
	b.WriteString("description: " + quoted(description(spec)) + "\n")
	if spec.Title != "" {
		b.WriteString("metadata:\n  title: " + quoted(spec.Title) + "\n")
	}
	b.WriteString("---\n\n")

	b.WriteString("# " + heading(spec) + "\n\n")
	if spec.Description != "" {
		b.WriteString(spec.Description + "\n\n")
	}
	b.WriteString("## How to call it\n\n")
	b.WriteString("Every endpoint below is reached with this workspace's HTTP API tool for this " +
		"service. You give it a verb and a path; the credentials are held by the tool and are " +
		"never shown to you.\n\n")
	switch {
	case spec.BaseURL != "":
		b.WriteString("The tool is configured with the base address `" + spec.BaseURL +
			"`, so a path here is written under it: `/invoices`, not the whole address.\n\n")
	case spec.BasePath != "":
		// The document states a path and no host, which is what an API whose
		// address differs per installation looks like. The administrator has
		// already given the tool the address, so what a reader needs is only
		// whether the paths below are written under it.
		b.WriteString("The paths below sit under `" + spec.BasePath +
			"`, which the tool's base address already includes. So call `/leads`, not `" +
			spec.BasePath + "/leads` and not a whole address.\n\n")
	}
	b.WriteString("```\n")
	b.WriteString("{ \"method\": \"GET\", \"path\": \"/some/path\", \"query\": { \"limit\": 10 } }\n")
	b.WriteString("```\n\n")
	b.WriteString("A 4xx or 5xx is the service answering, not the tool breaking: read the body, " +
		"which usually says which field was wrong. Paging is the service's own, so pass its paging " +
		"parameters in `query` and follow the `Link` header or the next-page field it returns.\n\n")

	b.WriteString("## Where the endpoints are\n\n")
	b.WriteString("This skill's other files, one per group. Read the one you need rather than all of them.\n\n")
	for _, group := range spec.Groups {
		b.WriteString("- `reference/" + slug(group.Name) + ".md` - " + group.Name +
			", " + count(len(group.Operations)) + "\n")
	}
	return b.String()
}

// reference is one group's operations, a heading each so the search index and
// the reader see them separately.
func reference(spec *Spec, group Group) string {
	var b strings.Builder
	b.WriteString("# " + group.Name + "\n\n")
	if group.Description != "" {
		b.WriteString(group.Description + "\n\n")
	}
	if spec.BaseURL != "" {
		b.WriteString("Paths are under `" + spec.BaseURL + "`.\n\n")
	}
	for _, op := range group.Operations {
		b.WriteString("## " + op.Method + " " + op.Path + "\n\n")
		if op.Deprecated {
			b.WriteString("**Deprecated.** The service says not to use this.\n\n")
		}
		if op.Summary != "" {
			b.WriteString(op.Summary + "\n\n")
		}
		if op.Description != "" && op.Description != op.Summary {
			b.WriteString(op.Description + "\n\n")
		}
		writeParams(&b, op)
		if op.Body != "" {
			// A schema with no properties describes to a bare type name, and a
			// lone "object" on a line is noise that reads as a mistake. Say
			// what it means instead: there IS a body and the specification does
			// not describe it, which is a different thing from no body at all.
			if strings.Contains(op.Body, "- `") {
				b.WriteString("Body (JSON):\n\n" + strings.TrimRight(op.Body, "\n") + "\n\n")
			} else {
				b.WriteString("Takes a JSON body, whose fields this specification does not describe.\n\n")
			}
		}
		if len(op.Responses) > 0 {
			b.WriteString("Answers:\n\n")
			for _, r := range op.Responses {
				b.WriteString("- `" + r.Status + "`")
				if r.Description != "" {
					b.WriteString(" " + r.Description)
				}
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// writeParams writes the inputs grouped by WHERE they go, because that is what
// the caller has to decide: a path parameter is part of the path, a query one
// goes in `query`, and a header one this tool sends from its own settings
// rather than per call.
func writeParams(b *strings.Builder, op Operation) {
	byPlace := map[string][]Param{}
	for _, p := range op.Params {
		byPlace[p.In] = append(byPlace[p.In], p)
	}
	for _, place := range []struct{ in, says string }{
		{"path", "In the path"},
		{"query", "In `query`"},
		{"header", "Headers (set these on the tool, not per call)"},
		{"cookie", "Cookies"},
	} {
		list := byPlace[place.in]
		if len(list) == 0 {
			continue
		}
		b.WriteString(place.says + ":\n\n")
		for _, p := range list {
			b.WriteString("- `" + p.Name + "`")
			if p.Type != "" {
				b.WriteString(" " + p.Type)
			}
			if p.Required {
				b.WriteString(" (required)")
			}
			if p.Description != "" {
				b.WriteString(" - " + p.Description)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
}

// description is what a reader is offered when choosing a skill, and it is the
// field the format requires. The API's own first sentence when it has one,
// because it was written for exactly this.
func description(spec *Spec) string {
	if first := firstSentence(spec.Description); first != "" {
		return first
	}
	name := spec.Title
	if name == "" {
		name = "this API"
	}
	return "How to call " + name + ": its endpoints, their parameters and what they answer."
}

func heading(spec *Spec) string {
	if spec.Title == "" {
		return "The API"
	}
	if spec.Version == "" {
		return spec.Title
	}
	return spec.Title + " " + spec.Version
}

func firstSentence(text string) string {
	text = oneLine(text)
	if text == "" {
		return ""
	}
	if at := strings.Index(text, ". "); at > 0 {
		return text[:at+1]
	}
	return text
}

// oneLine keeps a value on one line, because frontmatter is line based and a
// description with a newline in it would end the field early.
func oneLine(text string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", " ")), " ")
}

// quoted writes a value the frontmatter parser will read back as the string it
// is, which means quoting it whatever it looks like.
//
// Found by the reader refusing a package: a fallback description of "How to
// call Legacy Warehouse: its endpoints, ..." is not valid YAML, because an
// unquoted scalar containing ": " is a MAPPING. Titles and descriptions with a
// colon in them are ordinary ("Billing: the invoices API"), and so are ones
// starting with a # or a -, so nothing here is safe bare and guessing which is
// worse than quoting everything.
func quoted(text string) string {
	text = oneLine(text)
	// JSON's string form is valid YAML and escapes the quote and the backslash,
	// which is the whole of what a double-quoted YAML scalar needs.
	raw, err := json.Marshal(text)
	if err != nil {
		return `""`
	}
	return string(raw)
}

func count(n int) string {
	if n == 1 {
		return "1 endpoint"
	}
	return fmt.Sprintf("%d endpoints", n)
}

// slug is a file name from a group's name, and it must be stable: the manifest
// names these files, so a name that came out differently would be an index
// pointing at nothing.
func slug(name string) string {
	var b strings.Builder
	last := byte('-')
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 32)
			last = c
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)
			last = c
		default:
			if last != '-' {
				b.WriteByte('-')
				last = '-'
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "operations"
	}
	return out
}
