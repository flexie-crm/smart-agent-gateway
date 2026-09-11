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
