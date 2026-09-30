// Package apispec turns an API specification into a skill.
//
// WHY A SKILL AND NOT TOOL CONFIGURATION. A real specification is hundreds of
// operations. Putting those in the tool would mean an enum of them in the input
// schema, sent to the model on every turn of every conversation whether or not
// the tool was touched, which is the arithmetic that took 79% out of the system
// prompt. A skill is searched and read on demand instead: the HTTP API tool
// stays a transport with a fixed, tiny schema, and the endpoints are looked up
// when they are wanted.
//
// So this package reads a document and writes a package in the Agent Skills
// format. It stores nothing: the archive goes through the ordinary skill import
// (app.ImportSkills), which already validates it, keeps it as an immutable
// version, indexes its passages for search, and serves them one at a time. A
// second path into storage would be a second set of rules to get wrong.
package apispec

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
)

// Spec is a specification as this package needs it: what the API is called,
// where it lives, and its operations grouped the way a reader would group them.
//
// A normalised view rather than the library's document, because what is written
// out is the same whichever format came in, and the two formats differ in ways
// nothing downstream should have to know about.
type Spec struct {
	Title       string
	Version     string
	Description string
	// BaseURL is the address the tool should be pointed at, when the document
	// says. A Swagger 2.0 document says it in three fields (host, basePath,
	// schemes) and an OpenAPI 3 one in a server list; by here it is one string.
	BaseURL string
	// BasePath is the path part on its own, for the very common document that
	// states where its API sits but not which host it is on.
	//
	// Kept for the administrator, and deliberately NOT completed into an
	// address. Nothing here can know the host: a specification is published
	// where the vendor publishes it, and for an API whose host differs per
	// installation there is no single one to find. The document states the
	// path, so the path is what is offered, and the host stays the
	// administrator's to give.
	BasePath string
	Groups   []Group
}

// Group is the operations under one tag, which is how an API's own authors
// grouped them and therefore how a reader expects to find them.
type Group struct {
	Name        string
	Description string
	Operations  []Operation
}

// Operation is one call.
type Operation struct {
	Method      string
	Path        string
	Summary     string
	Description string
	Deprecated  bool
	Params      []Param
	Body        string
	Responses   []Response
}

// Param is one input, in the place the API wants it.
type Param struct {
	Name        string
	In          string
	Required    bool
	Type        string
	Description string
}

// Response is one documented outcome.
type Response struct {
	Status      string
	Description string
}

// Read parses a specification, whichever of the two formats it is in.
//
// Swagger 2.0 is LIFTED to OpenAPI 3 rather than read separately, so there is
// one reader and one shape below it: two readers would be two sets of rules for
// which field means what, and the differences are exactly where a mistake would
// hide. Measured on real documents both ways, including that the conversion
// reconstructs the server URL from host, basePath and schemes.
//
// JSON or YAML: the loader takes either, and a specification in the wild is as
// often one as the other.
func Read(data []byte) (*Spec, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("the specification is empty")
	}
	doc, err := load(data)
	if err != nil {
		return nil, err
	}
	if doc.Info == nil {
		return nil, fmt.Errorf("this document has no info section, so it is not a specification we can read")
	}
	if doc.Paths == nil || doc.Paths.Len() == 0 {
		return nil, fmt.Errorf("this specification describes no paths, so there would be nothing to write down")
	}
	return normalise(doc), nil
}

// load reads the document, lifting a 2.0 one on the way.
func load(data []byte) (*openapi3.T, error) {
	if isSwagger2(data) {
		var two openapi2.T
		if err := two.UnmarshalJSON(data); err != nil {
			return nil, fmt.Errorf("this Swagger 2.0 document could not be read: %w", err)
		}
		lifted, err := openapi2conv.ToV3(&two)
		if err != nil {
			return nil, fmt.Errorf("this Swagger 2.0 document could not be converted: %w", err)
		}
		// A 2.0 document with a basePath and no host converts to no server at
		// all, and the path it DID state is the part worth keeping: it is half
		// the address, and a fetch supplies the other half.
		if len(lifted.Servers) == 0 && strings.TrimSpace(two.BasePath) != "" {
			lifted.Servers = openapi3.Servers{{URL: two.BasePath}}
		}
		return lifted, nil
	}
	loader := openapi3.NewLoader()
	// Deliberately NOT IsExternalRefsAllowed: a $ref to a URL would have this
	// fetch whatever an uploaded document names, from inside the server, which
	// is a request somebody else chose. A document that needs its own files is
	// refused with that said rather than quietly reached for.
	doc, err := loader.LoadFromData(data)
	if err != nil {
		return nil, fmt.Errorf("this specification could not be read: %w", err)
	}
	return doc, nil
}

// isSwagger2 reads the version field both formats carry, because which reader
// to use is the document's own claim and not something to infer from shape.
func isSwagger2(data []byte) bool {
	var head struct {
		Swagger string `json:"swagger"`
		OpenAPI string `json:"openapi"`
	}
	if err := json.Unmarshal(data, &head); err == nil {
		return strings.HasPrefix(head.Swagger, "2.") && head.OpenAPI == ""
	}
	// YAML, where a cheap look at the first lines is enough: the key is at the
	// top level, so it is at the start of a line.
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "swagger:") && strings.Contains(line, "2.") {
			return true
		}
		if strings.HasPrefix(line, "openapi:") {
			return false
		}
	}
	return false
}

func normalise(doc *openapi3.T) *Spec {
	spec := &Spec{
		Title:       strings.TrimSpace(doc.Info.Title),
		Version:     strings.TrimSpace(doc.Info.Version),
		Description: strings.TrimSpace(doc.Info.Description),
	}
	if len(doc.Servers) > 0 {
		first := strings.TrimRight(strings.TrimSpace(doc.Servers[0].URL), "/")
		// A relative server is a path and not an address: the document is
		// saying where its API sits without saying on which host.
		if strings.HasPrefix(first, "/") {
			spec.BasePath = first
		} else {
			spec.BaseURL = first
			if at, err := url.Parse(first); err == nil {
				spec.BasePath = strings.TrimRight(at.Path, "/")
			}
		}
	}

	// What each tag is for, when the document says, so a group can carry its
	// own sentence rather than only a name.
	about := map[string]string{}
	for _, tag := range doc.Tags {
		about[tag.Name] = strings.TrimSpace(tag.Description)
	}

	byTag := map[string][]Operation{}
	for _, path := range sortedPaths(doc) {
		item := doc.Paths.Find(path)
		for _, method := range methodOrder {
			op := item.GetOperation(method)
			if op == nil {
				continue
			}
			built := Operation{
				Method:      method,
				Path:        path,
				Summary:     strings.TrimSpace(op.Summary),
				Description: strings.TrimSpace(op.Description),
				Deprecated:  op.Deprecated,
				Params:      params(item.Parameters, op.Parameters),
				Body:        body(op),
				Responses:   responses(op),
			}
			// An operation with no tag still has to land somewhere, and a
			// document that tags nothing is common: everything in one group is
			// the honest answer rather than a group per path.
			tag := "Operations"
			if len(op.Tags) > 0 && strings.TrimSpace(op.Tags[0]) != "" {
				tag = strings.TrimSpace(op.Tags[0])
			}
			byTag[tag] = append(byTag[tag], built)
		}
	}

	names := make([]string, 0, len(byTag))
	for name := range byTag {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		spec.Groups = append(spec.Groups, Group{
			Name:        name,
			Description: about[name],
			Operations:  byTag[name],
		})
	}
	return spec
}

// methodOrder is the order operations are written in, so two runs over one
// document produce the same package. Map iteration would not.
var methodOrder = []string{"GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE", "TRACE"}

func sortedPaths(doc *openapi3.T) []string {
	paths := make([]string, 0, doc.Paths.Len())
	for path := range doc.Paths.Map() {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// params merges the ones declared on the path with the ones on the operation,
// because a parameter declared once for every method of a path is still a
// parameter of each, and a reader who was not told it would build a call that
// fails.
func params(onPath, onOperation openapi3.Parameters) []Param {
	var out []Param
	seen := map[string]bool{}
	for _, set := range []openapi3.Parameters{onOperation, onPath} {
		for _, ref := range set {
			if ref == nil || ref.Value == nil {
				continue
			}
			p := ref.Value
			key := p.In + ":" + p.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, Param{
				Name:        p.Name,
				In:          p.In,
				Required:    p.Required,
				Type:        schemaType(p.Schema),
				Description: strings.TrimSpace(p.Description),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].In != out[j].In {
			return out[i].In < out[j].In
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func body(op *openapi3.Operation) string {
	if op.RequestBody == nil || op.RequestBody.Value == nil {
		return ""
	}
	for _, kind := range []string{"application/json", "application/x-www-form-urlencoded"} {
		if media := op.RequestBody.Value.Content.Get(kind); media != nil {
			return describeSchema(media.Schema, 0)
		}
	}
	return ""
}

func responses(op *openapi3.Operation) []Response {
	if op.Responses == nil {
		return nil
	}
	var out []Response
	codes := make([]string, 0, op.Responses.Len())
	for code := range op.Responses.Map() {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		ref := op.Responses.Value(code)
		if ref == nil || ref.Value == nil {
			continue
		}
		text := ""
		if ref.Value.Description != nil {
			text = strings.TrimSpace(*ref.Value.Description)
		}
		out = append(out, Response{Status: code, Description: text})
	}
	return out
}

func schemaType(ref *openapi3.SchemaRef) string {
	if ref == nil || ref.Value == nil {
		return ""
	}
	s := ref.Value
	kind := ""
	if s.Type != nil && len(*s.Type) > 0 {
		kind = (*s.Type)[0]
	}
	if kind == "array" && s.Items != nil {
		if inner := schemaType(s.Items); inner != "" {
			return inner + "[]"
		}
		return "array"
	}
	if len(s.Enum) > 0 {
		var options []string
		for _, v := range s.Enum {
			options = append(options, fmt.Sprint(v))
		}
		return kind + " (" + strings.Join(options, ", ") + ")"
	}
	return kind
}

// describeSchema writes a body's fields, bounded in depth.
//
// Bounded because a specification's schemas are a graph and a $ref can point at
// an ancestor: unbounded, one document would write itself out for ever. Three
// levels is what a reader needs to build a call; deeper is the service's own
// documentation to give.
func describeSchema(ref *openapi3.SchemaRef, depth int) string {
	if ref == nil || ref.Value == nil || depth > 2 {
		return ""
	}
	s := ref.Value
	if len(s.Properties) == 0 {
		return schemaType(ref)
	}
	required := map[string]bool{}
	for _, name := range s.Required {
		required[name] = true
	}
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		prop := s.Properties[name]
		b.WriteString(strings.Repeat("  ", depth))
		b.WriteString("- `" + name + "`")
		if kind := schemaType(prop); kind != "" {
			b.WriteString(" " + kind)
		}
		if required[name] {
			b.WriteString(" (required)")
		}
		if prop != nil && prop.Value != nil && strings.TrimSpace(prop.Value.Description) != "" {
			b.WriteString(" - " + strings.TrimSpace(prop.Value.Description))
		}
		b.WriteString("\n")
		if prop != nil && prop.Value != nil && len(prop.Value.Properties) > 0 {
			b.WriteString(describeSchema(prop, depth+1))
		}
	}
	return b.String()
}
