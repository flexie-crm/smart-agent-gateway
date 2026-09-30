package apispec_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"flexie.io/sag/internal/apispec"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/skill"
)

const openapi3Doc = `{
  "openapi": "3.0.3",
  "info": {
    "title": "Billing",
    "version": "2026-01",
    "description": "Invoices and customers. Everything is JSON and paged with limit and starting_after."
  },
  "servers": [{"url": "https://api.example.com/v1/"}],
  "tags": [{"name": "Invoices", "description": "Bills you have sent."}],
  "paths": {
    "/invoices": {
      "parameters": [{"name": "X-Tenant", "in": "header", "schema": {"type": "string"}, "description": "Which tenant"}],
      "get": {
        "summary": "List invoices",
        "tags": ["Invoices"],
        "parameters": [
          {"name": "customer", "in": "query", "schema": {"type": "string"}, "description": "Only this customer"},
          {"name": "limit", "in": "query", "schema": {"type": "integer"}},
          {"name": "status", "in": "query", "schema": {"type": "string", "enum": ["draft", "open", "paid"]}}
        ],
        "responses": {"200": {"description": "A page of invoices"}, "401": {"description": "Bad credentials"}}
      },
      "post": {
        "summary": "Create an invoice",
        "tags": ["Invoices"],
        "requestBody": {"content": {"application/json": {"schema": {
          "type": "object",
          "required": ["customer"],
          "properties": {
            "customer": {"type": "string", "description": "Who to bill"},
            "lines": {"type": "array", "items": {"type": "string"}},
            "meta": {"type": "object", "properties": {"note": {"type": "string"}}}
          }}}}},
        "responses": {"201": {"description": "Created"}}
      }
    },
    "/invoices/{id}": {
      "delete": {"summary": "Void an invoice", "tags": ["Invoices"], "deprecated": true,
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {"204": {"description": "Voided"}}}
    },
    "/health": {"get": {"summary": "Is it up", "responses": {"200": {"description": "yes"}}}}
  }
}`

// A Swagger 2.0 document, which is what a great many real APIs still publish.
const swagger2Doc = `{
  "swagger": "2.0",
  "info": {"title": "Legacy Warehouse", "version": "1.2"},
  "host": "warehouse.example.com",
  "basePath": "/api",
  "schemes": ["https"],
  "paths": {
    "/stock/{sku}": {
      "get": {
        "summary": "Stock for one item",
        "tags": ["Stock"],
        "parameters": [{"name": "sku", "in": "path", "required": true, "type": "string", "description": "The item"}],
        "responses": {"200": {"description": "A count"}}
      }
    }
  }
}`

// The whole point: the package this writes is one the REAL reader accepts.
//
// Asserted by handing it to skill.Read, the same function an uploaded archive
// goes through, rather than by inspecting the bytes we just wrote. A generated
// package that only our own tests accept would be refused the moment anybody
// imported it.
func TestTheGeneratedPackageIsOneTheSkillReaderAccepts(t *testing.T) {
	spec, err := apispec.Read([]byte(openapi3Doc))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	archive, err := apispec.Package(spec, "billing-api")
	if err != nil {
		t.Fatalf("package: %v", err)
	}

	pkg, err := skill.Read(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("the real skill reader refused the package we generated: %v", err)
	}
	if pkg.Name != "billing-api" {
		t.Errorf("name = %q", pkg.Name)
	}
	if pkg.Title != "Billing" {
		t.Errorf("title = %q, want the API's own name", pkg.Title)
	}
	if !strings.Contains(pkg.Description, "Invoices and customers") {
		t.Errorf("description = %q, want the API's own first sentence", pkg.Description)
	}
	// One file per group plus the manifest, which is what makes load_skill's
	// drill-down work: a single file of four hundred endpoints is one part.
	var paths []string
	for _, f := range pkg.Files {
		paths = append(paths, f.Path)
	}
	for _, want := range []string{"SKILL.md", "reference/invoices.md", "reference/operations.md"} {
		if !has(paths, want) {
			t.Errorf("the package has no %s: %v", want, paths)
		}
	}
	// And the passages are split, so a search can find one endpoint rather
	// than the whole file.
	var sections int
	for _, f := range pkg.Files {
		sections += len(f.Sections)
	}
	if sections < 5 {
		t.Errorf("only %d passages across the package, so nothing could be found by heading", sections)
	}
}

// What a reader actually needs to build a call.
func TestTheReferenceSaysHowToCallEachEndpoint(t *testing.T) {
	spec, err := apispec.Read([]byte(openapi3Doc))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	archive, _ := apispec.Package(spec, "billing-api")
	pkg, err := skill.Read(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	invoices := file(t, pkg, "reference/invoices.md")

	for _, want := range []string{
		// A heading per operation, which is what the index splits on.
		"## GET /invoices",
		"## POST /invoices",
		"## DELETE /invoices/{id}",
		// Parameters grouped by where they go, because that is the decision
		// the caller has to make.
		"In `query`",
		"`limit` integer",
		"`status` string (draft, open, paid)",
		"In the path",
		"`id` string (required)",
		// A path-level parameter belongs to every method of that path, and a
		// caller who was not told it would build a call that fails.
		"X-Tenant",
		// The body, with what is required.
		"`customer` string (required) - Who to bill",
		"`lines` string[]",
		// Nested one level down, which is where a body's shape usually is.
		"`note`",
		// What it answers, including the failures.
		"`401` Bad credentials",
		// And a warning the service itself gave.
		"Deprecated",
	} {
		if !strings.Contains(invoices, want) {
			t.Errorf("the reference does not carry %q", want)
		}
	}

	// An untagged operation still lands somewhere findable rather than being
	// dropped, because a document that tags nothing is common.
	other := file(t, pkg, "reference/operations.md")
	if !strings.Contains(other, "## GET /health") {
		t.Errorf("an untagged operation was lost: %s", other)
	}
}

// The manifest says how to use the TOOL, and deliberately not what the
// endpoints are: that is the whole reason this is a skill.
func TestTheManifestExplainsTheToolAndPointsAtTheFiles(t *testing.T) {
	spec, _ := apispec.Read([]byte(openapi3Doc))
	archive, _ := apispec.Package(spec, "billing-api")
	pkg, err := skill.Read(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	manifest := file(t, pkg, "SKILL.md")

	for _, want := range []string{
		"https://api.example.com/v1", // the base address, trailing slash trimmed
		"reference/invoices.md",
		"4xx", // that a refusal is an answer
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the manifest does not carry %q", want)
		}
	}
	// The endpoints themselves are NOT in it. A manifest listing them would be
	// the thing this design exists to avoid.
	if strings.Contains(manifest, "## GET /invoices") {
		t.Errorf("the manifest lists endpoints, which is what the reference files are for:\n%s", manifest)
	}
}

// Swagger 2.0, which is what a great many APIs still publish.
func TestASwagger2DocumentIsLiftedAndReadTheSameWay(t *testing.T) {
	spec, err := apispec.Read([]byte(swagger2Doc))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// The conversion reconstructs the address from host, basePath and schemes,
	// which is exactly what the tool has to be pointed at.
	if spec.BaseURL != "https://warehouse.example.com/api" {
		t.Fatalf("base = %q", spec.BaseURL)
	}
	if spec.Title != "Legacy Warehouse" {
		t.Fatalf("title = %q", spec.Title)
	}
	archive, err := apispec.Package(spec, "warehouse")
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	pkg, err := skill.Read(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("the reader refused a package built from Swagger 2.0: %v", err)
	}
	stock := file(t, pkg, "reference/stock.md")
	if !strings.Contains(stock, "## GET /stock/{sku}") || !strings.Contains(stock, "`sku` string (required)") {
		t.Fatalf("the lifted operation lost its shape:\n%s", stock)
	}
}

// The same document twice produces the same bytes.
//
// Map iteration in Go is deliberately unordered, so a generator that walked
// paths, methods or properties straight out of a map would produce a different
// archive every run. That matters here beyond tidiness: a skill version is
// keyed on the hash of its bytes, so an unstable package would import as a new
// version every time somebody pressed the button.
func TestTheSameDocumentProducesTheSameBytes(t *testing.T) {
	for _, doc := range [][]byte{[]byte(openapi3Doc), []byte(swagger2Doc)} {
		first, err := apispec.Read(doc)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		second, _ := apispec.Read(doc)
		a, err := apispec.Package(first, "same")
		if err != nil {
			t.Fatalf("package: %v", err)
		}
		b, _ := apispec.Package(second, "same")
		if !bytes.Equal(a, b) {
			t.Fatalf("two runs over one document produced different archives (%d and %d bytes)", len(a), len(b))
		}
	}
}

// What is refused, and said plainly, because an administrator reads it.
func TestADocumentThatIsNotUsableIsRefused(t *testing.T) {
	for _, c := range []struct{ name, doc string }{
		{"empty", ""},
		{"not a specification at all", `{"hello":"world"}`},
		{"no paths", `{"openapi":"3.0.0","info":{"title":"X","version":"1"},"paths":{}}`},
		{"not json or yaml", "\x00\x01\x02 not a document"},
	} {
		if _, err := apispec.Read([]byte(c.doc)); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
	// The control: the good one is accepted, so the refusals above are about
	// the documents and not about the reader being broken.
	if _, err := apispec.Read([]byte(openapi3Doc)); err != nil {
		t.Fatalf("a real specification was refused: %v", err)
	}
}

// A bad handle is refused: it becomes the package's directory AND its name, and
// the format requires the two to match.
func TestABadNameIsRefused(t *testing.T) {
	spec, _ := apispec.Read([]byte(openapi3Doc))
	for _, handle := range []string{"", "Billing", "billing api", "billing_api", "1billing", strings.Repeat("a", 80)} {
		if _, err := apispec.Package(spec, handle); err == nil {
			t.Errorf("the name %q was accepted", handle)
		}
	}
}

func has(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func file(t *testing.T, pkg *model.SkillPackage, path string) string {
	t.Helper()
	for _, f := range pkg.Files {
		if f.Path == path {
			return f.Text
		}
	}
	t.Fatalf("the package has no %s", path)
	return ""
}

// A title or description with a colon in it does not break the frontmatter.
//
// The case that caught it: a fallback description reading "How to call Legacy
// Warehouse: its endpoints, ..." is not valid YAML, because an unquoted scalar
// containing ": " is a mapping. Titles like "Billing: the invoices API" are
// ordinary, so this is not an edge case, and the reader refuses the whole
// package when it happens.
func TestAColonInTheTitleDoesNotBreakTheFrontmatter(t *testing.T) {
	awkward := []string{
		`{"openapi":"3.0.0","info":{"title":"Billing: the invoices API","version":"1","description":"Money: in and out. Second sentence."},"paths":{"/x":{"get":{"responses":{"200":{"description":"ok"}}}}}}`,
		`{"openapi":"3.0.0","info":{"title":"#hashtag","version":"1"},"paths":{"/x":{"get":{"responses":{"200":{"description":"ok"}}}}}}`,
		`{"openapi":"3.0.0","info":{"title":"- leading dash","version":"1"},"paths":{"/x":{"get":{"responses":{"200":{"description":"ok"}}}}}}`,
		`{"openapi":"3.0.0","info":{"title":"quotes \"inside\" it","version":"1"},"paths":{"/x":{"get":{"responses":{"200":{"description":"ok"}}}}}}`,
	}
	for _, doc := range awkward {
		spec, err := apispec.Read([]byte(doc))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		archive, err := apispec.Package(spec, "awkward")
		if err != nil {
			t.Fatalf("package: %v", err)
		}
		pkg, err := skill.Read(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			t.Errorf("the reader refused a package whose title was %q: %v", spec.Title, err)
			continue
		}
		if pkg.Title != spec.Title {
			t.Errorf("title came back as %q, want %q", pkg.Title, spec.Title)
		}
	}
}

// A real specification, end to end.
//
// Kept as testdata rather than trusted to a network: 273 KB of Swagger 2.0 with
// 118 paths, 170 operations and 12 tags, published by a CRM. Everything above
// is a document written to exercise one rule; this is one nobody wrote for us,
// and it is where the shape of the output has to hold up.
func TestARealSpecificationBecomesAReadableSkill(t *testing.T) {
	raw, err := os.ReadFile("testdata/flexie-api.json")
	if err != nil {
		t.Skipf("no real specification to read: %v", err)
	}
	spec, err := apispec.Read(raw)
	if err != nil {
		t.Fatalf("a real published specification was refused: %v", err)
	}
	if spec.Title != "Flexie" {
		t.Fatalf("title = %q", spec.Title)
	}
	// Every operation survives: losing a fifth of an API quietly would be the
	// worst failure this can have, and a count is the only thing that sees it.
	var ops int
	for _, g := range spec.Groups {
		ops += len(g.Operations)
	}
	if ops != 170 {
		t.Errorf("read %d operations, want the 170 the document declares", ops)
	}
	if len(spec.Groups) != 12 {
		t.Errorf("read %d groups, want 12", len(spec.Groups))
	}

	// The document states its path and NOT its host, which is ordinary in 2.0:
	// whoever wrote it knew which server it was on. Nothing to guess from a
	// file, and the fetch address answers it.
	if spec.BaseURL != "" {
		t.Errorf("base = %q, want empty: this document does not state a host", spec.BaseURL)
	}
	if spec.BasePath != "/api" {
		t.Errorf("basePath = %q", spec.BasePath)
	}
	// And nothing invents one. This API's host differs per installation, and
	// the document is published from the vendor's own site, so completing the
	// address from where it was fetched would point every tool at a host no
	// call of theirs should go to, while looking correctly configured.
	if spec.BaseURL != "" {
		t.Errorf("a host was invented: %q", spec.BaseURL)
	}

	archive, err := apispec.Package(spec, "flexie-api")
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	pkg, err := skill.Read(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("the real reader refused a package built from a real specification: %v", err)
	}

	// One file per group plus the manifest, and every one of them split into
	// passages: a skill is READ one part at a time, and 170 endpoints in a
	// single file would be one part and would defeat both the drill-down and
	// the search.
	if len(pkg.Files) != 13 {
		t.Errorf("the package has %d files, want the manifest and one per group", len(pkg.Files))
	}
	var passages int
	for _, f := range pkg.Files {
		passages += len(f.Sections)
		if len(f.Sections) == 0 {
			t.Errorf("%s is one undivided passage", f.Path)
		}
	}
	if passages < 170 {
		t.Errorf("%d passages for 170 operations, so some are not separately findable", passages)
	}

	// And a specific endpoint is there, with what a caller needs to make it.
	notes := file(t, pkg, "reference/notes.md")
	for _, want := range []string{"## POST /notes/{entityType}/{entityId}", "`entityId` integer (required)"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the reference does not carry %q", want)
		}
	}
	// A body the specification does not describe is SAID to be undescribed,
	// not written out as a bare "object" that reads like a mistake.
	if strings.Contains(notes, "\nobject\n") {
		t.Errorf("an opaque body was written out as a bare type:\n%s", notes)
	}
}

// Fetching: what is refused, and the one thing the address tells us that the
// document often does not.
func TestFetchingASpecification(t *testing.T) {
	doc := `{"swagger":"2.0","info":{"title":"Served","version":"1"},"basePath":"/api",
	         "paths":{"/things":{"get":{"responses":{"200":{"description":"ok"}}}}}}`

	t.Run("a fetched document keeps its path and gains no host", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(doc))
		}))
		t.Cleanup(srv.Close)

		raw, err := apispec.Fetch(context.Background(), nil, srv.URL+"/api.json")
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		spec, err := apispec.Read(raw)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if spec.BasePath != "/api" {
			t.Fatalf("the path the document states was lost: %q", spec.BasePath)
		}
		// The host is the administrator's to give: where a document is
		// published is not where the API runs.
		if spec.BaseURL != "" {
			t.Fatalf("a host was taken from the fetch address: %q", spec.BaseURL)
		}
	})

	// The usual mistake, and worth its own message: pasting the address of the
	// documentation page rather than of the document behind it. A parse error
	// about an unexpected "<" would send somebody looking in the wrong place.
	t.Run("a web page is refused by saying it is one", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html><html><body>Our API docs</body></html>"))
		}))
		t.Cleanup(srv.Close)

		_, err := apispec.Fetch(context.Background(), nil, srv.URL)
		if err == nil {
			t.Fatal("a web page was accepted as a specification")
		}
		if !strings.Contains(err.Error(), "web page") {
			t.Fatalf("the refusal does not say what arrived: %v", err)
		}
	})

	t.Run("what else is refused", func(t *testing.T) {
		missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(missing.Close)
		empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		t.Cleanup(empty.Close)

		for _, c := range []struct{ name, address string }{
			{"nothing", ""},
			{"not an address", "://nope"},
			{"a scheme we do not fetch", "file:///etc/passwd"},
			{"no host", "https://"},
			{"an address that answers 404", missing.URL},
			{"an address that answers with nothing", empty.URL},
		} {
			if _, err := apispec.Fetch(context.Background(), nil, c.address); err == nil {
				t.Errorf("%s was accepted", c.name)
			}
		}
	})

	// A document that DOES state its servers is believed: it knows something
	// we would otherwise be guessing at.
	t.Run("a document that states its own address keeps it", func(t *testing.T) {
		spec, err := apispec.Read([]byte(`{"openapi":"3.0.0","info":{"title":"X","version":"1"},
			"servers":[{"url":"https://declared.example.com/v2"}],
			"paths":{"/x":{"get":{"responses":{"200":{"description":"ok"}}}}}}`))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if spec.BaseURL != "https://declared.example.com/v2" {
			t.Fatalf("base = %q, want the one the document declared", spec.BaseURL)
		}
		if spec.BasePath != "/v2" {
			t.Fatalf("basePath = %q", spec.BasePath)
		}
	})
}
