// Package template is the code side of native custom tools. A built-in tool is
// a fixed Schema+Handler; a native custom tool is instantiated from a TEMPLATE
// (the query template, later others) with settings an administrator fills in,
// and stored as a tools row (kind='custom'). A template is the recipe: it
// declares the form to configure an instance, turns that form into a
// self-describing tool, and binds a live handler to a stored instance.
//
// Templates are registered explicitly (no import-magic): the wiring names the
// templates a build ships, the same way it names the built-in tools.
package template

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"flexie.io/sag/internal/tool"
)

// The form vocabulary lives in the tool package, beside the schema, because a
// native tool declares settings the same way a template does. These aliases
// keep it spelled `template.Field` where a template is what is being described.
type (
	FieldType = tool.FieldType
	Option    = tool.Option
	Field     = tool.Field
	Section   = tool.Section
)

const (
	FieldText     = tool.FieldText
	FieldNumber   = tool.FieldNumber
	FieldPassword = tool.FieldPassword
	FieldSelect   = tool.FieldSelect
	FieldTextarea = tool.FieldTextarea
	FieldCheckbox = tool.FieldCheckbox
	FieldPairs    = tool.FieldPairs
	FieldCallback = tool.FieldCallback
)

// Choice pairs a stored value with what it says to a person.
func Choice(value, label string) Option { return tool.Choice(value, label) }

// Variant is a sub-kind of a template with its own settings form: for the query
// template, a database driver (MySQL, Postgres, ...). A template with a single
// form has one variant, or none.
type Variant struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// Param is one input the tool takes when the model calls it. For a native tool
// the template fixes the inputs (their key, type, and whether they are
// required); an administrator may only refine the description, for better model
// guidance. So the identity is the template's and the description is editable.
type Param struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

// Input is what the admin submitted to create or edit an instance. Alias,
// Variant, and Settings are the connection; DisplayName, Description, Guide, and
// ParamDescriptions are the editable presentation, each falling back to the
// template's default when blank.
type Input struct {
	Alias    string
	Variant  string
	Settings map[string]any

	DisplayName string
	Description string
	Guide       string
	// ParamDescriptions overrides a param's description by its key. A key absent
	// here keeps the template's default; the param's identity is never changed.
	ParamDescriptions map[string]string
}

// Instance is what a template produces from valid Input: the self-describing
// tool schema to persist as the tools row, and the config JSON to store with it.
// The secret paths in Config are still plaintext here; the app seals them (using
// SecretPaths) before the row is written.
type Instance struct {
	Schema tool.Schema
	Config json.RawMessage
	// Guide is the tool's own guide text to store (the admin's, or the template's
	// default). Empty means the template's guide is used at load time.
	Guide string
}

// Template is a recipe for a native custom tool.
type Template interface {
	// Name is the template's stable id, and the prefix of every tool it makes
	// ("query" -> "query_<alias>").
	Name() string
	Title() string
	Description() string

	// Variants are the sub-kinds the admin picks between (drivers, for query).
	Variants() []Variant
	// Fields are the settings to collect for a variant, grouped into sections
	// (the connection, the SSH tunnel, ...) the console renders as it sees fit.
	Fields(variant string) ([]Section, error)
	// SecretPaths are the config keys the app seals at rest, for a variant.
	SecretPaths(variant string) []string
	// Params are the inputs the tool takes, with their default descriptions, so
	// the form shows them prefilled (identity locked, description editable).
	Params() []Param
	// DefaultDescription is the model-facing description to prefill the form
	// with: what the AGENT reads to understand what this tool is and how to
	// call it.
	//
	// Not Description(), which is the paragraph a PERSON reads in the picker
	// while deciding what to make. The two were briefly the same field and it
	// showed: the form put the sales copy into the model's instructions.
	//
	// Static, because the form prefills it before any setting has been typed.
	// Build still writes an instance-specific one (naming the address, the
	// engine, the server) for anything created with this cleared out, so an
	// emptied field is a sensible tool rather than a nameless one.
	DefaultDescription() string
	// DefaultGuide is the guide text to prefill the form with, so an administrator
	// starts from the template's own and refines it.
	DefaultGuide() string

	// Config builds the connection config JSON from a variant's settings, without
	// making a tool. It is what a Test-before-save uses; Build wraps it. Secrets
	// are plaintext here (the app seals them for storage, or passes them through
	// for a test).
	Config(variant string, settings map[string]any) (json.RawMessage, error)
	// Build validates the input and produces the tool to persist. It does not
	// touch secrets beyond placing them in Config for the app to seal.
	Build(in Input) (Instance, error)
	// Bind builds the live handler for a stored instance, from its config with
	// the secrets already opened by the app. owner is the agent this handler
	// belongs to: a template that keeps anything between calls files it under
	// that, never under the conversation (see tool.Owner).
	Bind(config json.RawMessage, owner tool.Owner) (tool.Handler, error)
	// Test opens a live connection for a config (secrets opened) and checks it,
	// for the admin's Test button.
	Test(ctx context.Context, config json.RawMessage) error
	// Documentation is the system-owned instruction for a built instance, derived
	// from the template and the instance's config. It is never editable in the
	// form, so it always holds: the Note is appended to the tool's description (so
	// the model knows facts like which database engine it is), and Topics are the
	// tool's drill-down guide the tool_guide tool navigates.
	Documentation(config json.RawMessage) Documentation
}

// Displayed is a template that says what a person sees when they open one of
// its calls in the chat (tool.Display). Optional, like ActionTemplate: a
// template without it shows everything, which is the right default for a tool
// nobody has thought about.
//
// It belongs to the TEMPLATE rather than to each instance because it is about
// the shape of the tool's calls, which every instance shares: a server tool
// answers with output and an exit code whichever server it is pointed at.
type Displayed interface {
	Display() tool.Display
}

// Grant is one tool's sign-in as a template sees it: a usable access token,
// and nothing else.
//
// The split is deliberate and survives the tokens moving into the tool's own
// config. A template knows how to ATTACH a token (which header, which scheme);
// keeping one alive needs the store, the keyring and a conversation with an
// authorization server, so that stays with the app. A template that had to do
// the second would need all three, and every template after it would be free
// to do it differently.
type Grant interface {
	// Token answers with an access token that is usable now, renewing a stale
	// one on the way. It fails when nobody has signed in, which a handler must
	// REPORT rather than paper over: a call with no credential is one the
	// service refuses anyway, and saying why is the difference between
	// reconnecting and guessing.
	Token(ctx context.Context) (string, error)
	// Renew answers with a token to use instead of one the service has just
	// refused, renewing the sign-in unless somebody already has. Answering the
	// refused token itself means there is nothing better to offer, and the
	// refusal stands.
	Renew(ctx context.Context, refused string) (string, error)
}

// GrantedTemplate is a template whose tool is CONNECTED as well as configured:
// part of its credential was given by a person in a browser and rotates on its
// own.
//
// Optional, like Displayed and ActionTemplate, so templates that need nothing
// of the sort are untouched. One that implements it is bound through here
// instead of through Bind, with a Grant for the tool being bound.
type GrantedTemplate interface {
	BindGranted(config json.RawMessage, owner tool.Owner, grant Grant) (tool.Handler, error)
	// Connectable says this particular configuration needs a person to sign in,
	// so the console should offer it.
	//
	// The TEMPLATE answers because only it knows: the same template makes tools
	// with a key, with client credentials, and with a person's consent, and
	// which was chosen is inside a config nothing above can read. What the app
	// does with a true is mint the address, which only IT can.
	Connectable(config json.RawMessage) bool
}

// ActionTemplate is a template that offers its own operations to the console,
// beyond the fixed ones every template has. It is optional: a template without
// it is unchanged, and its Test button goes through Test as before.
//
// The point is that the core does not learn what any of them mean. A template
// names its own actions and reads its own payloads; what comes back is either an
// outcome to show, or a request for something more from the administrator,
// expressed in the same Field vocabulary the settings form already renders. So a
// template that needs a verification code, or a confirmation, or a choice from a
// list, asks for it without a line of that appearing in the API or the console.
type ActionTemplate interface {
	// Action runs one of the template's own operations against a configuration
	// whose secrets the app has opened. The body is the payload the console sent,
	// unread by anything between the two.
	Action(ctx context.Context, config json.RawMessage, action string, body json.RawMessage) (ActionResult, error)
}

// ActionTest is the one action every template answers to, so the console has a
// single Test button whether or not a template implements ActionTemplate: for
// one that does not, the app runs Test instead.
const ActionTest = "test"

// ActionResult is what an action produced: an outcome to show, and, when the
// action cannot finish without something more, what to ask for.
type ActionResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	// Prompt asks the administrator for more. Its presence means the action is
	// unfinished, not that it failed.
	Prompt *ActionPrompt `json:"prompt,omitempty"`
	// Visit is an address the administrator has to go to before this can
	// finish, and Visiting is the words on the button that takes them there.
	//
	// The other half of Prompt: one asks for something they can type, this for
	// something they can only do somewhere else. The console renders a link and
	// learns nothing about why, exactly as it renders prompt fields without
	// knowing what they are for.
	Visit    string `json:"visit,omitempty"`
	Visiting string `json:"visiting,omitempty"`
}

// ActionPrompt is a template asking the administrator a question mid-action. The
// console renders the fields, collects the answers, and sends them back to the
// named action along with the token, which only the template understands.
type ActionPrompt struct {
	Action string `json:"action"`
	Token  string `json:"token"`
	Title  string `json:"title"`
	Hint   string `json:"hint,omitempty"`
	// Submit is the label for the button that sends the answers, so the template
	// decides what the administrator is agreeing to do.
	Submit string  `json:"submit,omitempty"`
	Fields []Field `json:"fields"`
}

// Documentation is a template's system-owned instruction for a built tool: a
// note the app appends to the description, and drill-down topics (with bare
// slug ids the app namespaces by the tool's name).
type Documentation struct {
	Note   string
	Topics []tool.Topic
}

// Registry holds the templates a build ships. It is populated explicitly at
// wiring time and read at create time (the admin form) and load time (binding a
// stored instance).
type Registry struct {
	mu   sync.RWMutex
	byID map[string]Template
}

func NewRegistry() *Registry { return &Registry{byID: map[string]Template{}} }

// Add registers a template. A duplicate name is a programming error and panics
// at wiring time, not silently at runtime.
func (r *Registry) Add(t Template) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byID[t.Name()]; exists {
		panic("template already registered: " + t.Name())
	}
	r.byID[t.Name()] = t
}

func (r *Registry) Get(name string) (Template, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byID[name]
	return t, ok
}

// All returns the templates, sorted by name, for the admin's template picker.
func (r *Registry) All() []Template {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Template, 0, len(r.byID))
	for _, t := range r.byID {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// AliasFrom turns the name somebody typed into the identifier the assistant
// calls the tool by.
//
// There is ONE name on the form. "Production orders" becomes production_orders
// and the tool is query_production_orders, so nobody has to invent a second
// name, keep the two in step, or learn that an identifier has rules at all.
//
// The rule it produces to is the one every template already validates against
// (a letter first, then lowercase letters, digits and underscores, 49 at most),
// so what comes out of here is either something Build accepts or nothing, and
// nothing is a far better error than a regular expression somebody has to
// decode. Digits before the first letter are dropped rather than made legal by
// force: a tool called "2024 invoices" is invoices, which is what it is.
func AliasFrom(name string) string {
	var b strings.Builder
	gap := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z':
			if gap && b.Len() > 0 {
				b.WriteByte('_')
			}
			gap = false
			b.WriteRune(r)
		case r >= '0' && r <= '9' && b.Len() > 0:
			if gap {
				b.WriteByte('_')
			}
			gap = false
			b.WriteRune(r)
		default:
			// Anything else is a separator, however many of them there are in a
			// row, and a run of them is one underscore rather than several.
			gap = true
		}
	}
	alias := b.String()
	if len(alias) > 49 {
		alias = strings.TrimRight(alias[:49], "_")
	}
	return alias
}

// InputSchema is the JSON Schema a template's parameters make, with an
// administrator's own wording put over the template's where they gave any.
//
// ONE builder, because there were three near-identical ones and they had
// already drifted: only the query tool's emitted `items` for an array, which
// is not decoration (a schema saying "array" and nothing about its contents is
// refused outright by some providers). A copy per template is a rule that holds
// until somebody improves one of them.
//
// The SHAPE is the template's and is not an administrator's to change: the keys,
// their types, and which are required are what the handler reads by name. The
// DESCRIPTIONS are theirs, because explaining an API better is exactly the sort
// of local knowledge a template cannot have.
func InputSchema(params []Param, descriptions map[string]string) json.RawMessage {
	properties := map[string]any{}
	var required []string
	for _, p := range params {
		desc := p.Description
		if override, ok := descriptions[p.Key]; ok && strings.TrimSpace(override) != "" {
			desc = override
		}
		property := map[string]any{"type": p.Type, "description": desc}
		if p.Type == "array" {
			property["items"] = map[string]any{}
		}
		properties[p.Key] = property
		if p.Required {
			required = append(required, p.Key)
		}
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	raw, _ := json.Marshal(schema)
	return raw
}
