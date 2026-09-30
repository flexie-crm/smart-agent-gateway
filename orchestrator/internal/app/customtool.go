package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/apitool"
	"flexie.io/sag/internal/tools/skills"
	"flexie.io/sag/internal/tools/template"
)

// Native custom tools: instantiating a template into a tools row, and binding a
// stored row back to a live tool.
//
// The whole configuration, connection auth included, lives in the tools row's
// config JSON. Secret values are sealed in place: a sealed value carries a
// marker no typed credential can, so the loadout opens them by walking the
// config, never needing to know which fields were secret. A password is never
// at rest in the clear.

// sealMarker prefixes a sealed value in the config JSON. It is printable, so a
// stored secret reads as plain UTF-8 (enc:...) instead of an escaped control
// byte that JSON, the database, and a person all have to squint at. Safety does
// not rest on the marker being untypable: sealing always works from plaintext
// and never skips a value because it looks sealed, so a secret a person types
// that happens to begin with the marker is still sealed, never stored in clear.
const sealMarker = "enc:"

// CreateCustomTool instantiates a template into a new tools row. It validates
// the input through the template, seals the secret fields, and stores the
// self-describing tool. The new tool is immediately grantable, like any other.
// skills is a separate argument rather than a field of template.Input because
// no template reads it: the written procedures documenting a tool are standard
// for every custom tool, whatever it was built from, and putting them in the
// template's contract would offer every template a thing to have an opinion
// about when none of them should.
func (a *App) CreateCustomTool(ctx context.Context, workspaceID int64, templateName string, in template.Input, skills []int64, by model.Actor) (*model.Tool, error) {
	tmpl, ok := a.Templates.Get(templateName)
	if !ok {
		return nil, fmt.Errorf("unknown tool template %q", templateName)
	}
	// There is one name on the form. The identifier the assistant calls the
	// tool by is derived from it here, once, before any template sees the
	// input, so all three go on validating exactly what they always did.
	//
	// Still accepted when it is sent, because deriving is a convenience for the
	// form and not a rule about the field: something driving this directly may
	// name the tool itself.
	if strings.TrimSpace(in.Alias) == "" {
		in.Alias = template.AliasFrom(in.DisplayName)
		if in.Alias == "" {
			return nil, fmt.Errorf("give this tool a name with letters in it, " +
				"so the assistant has something to call it by")
		}
	}
	inst, err := tmpl.Build(in)
	if err != nil {
		return nil, err
	}
	// A tool is not stored until it has been proved to work. Settings that
	// describe a connection nobody can make would otherwise sit in the catalog
	// looking usable, be granted to an assistant, and fail the first time
	// somebody asked it for something.
	if err := tmpl.Test(ctx, inst.Config); err != nil {
		return nil, fmt.Errorf("this server could not be connected to, so the tool was not created: %w", err)
	}
	sealed, err := a.sealConfigSecrets(inst.Config, tmpl.SecretPaths(in.Variant))
	if err != nil {
		return nil, fmt.Errorf("secure the tool's secrets: %w", err)
	}

	row := &model.Tool{
		WorkspaceID:  workspaceID,
		Name:         inst.Schema.Name,
		Kind:         string(tool.KindCustom),
		Template:     templateName,
		FriendlyName: inst.Schema.FriendlyName,
		Description:  inst.Schema.Description,
		InputSchema:  inst.Schema.InputSchema,
		Risk:         string(inst.Schema.Risk),
		Config:       sealed,
		Guide:        inst.Guide,
		Status:       model.StatusActive,
		Skills:       skills,
	}
	if err := a.Store.Tools().CreateCustom(ctx, row, by); err != nil {
		return nil, err
	}
	return row, nil
}

// UpdateCustomTool rewrites a custom tool's configuration from a fresh form. The
// tool's identity is fixed on edit, the template, the alias (its name), and the
// driver, so only the settings, the presentation, and the guide change. Secrets
// left blank are kept: the previously sealed value is carried forward, so an
// administrator editing a host need not re-enter the password.
func (a *App) UpdateCustomTool(ctx context.Context, workspaceID, id int64, in template.Input, skills []int64, by model.Actor) (*model.Tool, error) {
	existing, err := a.Store.Tools().GetByID(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if existing.Kind != string(tool.KindCustom) {
		return nil, fmt.Errorf("only a custom tool can be edited this way")
	}
	tmpl, ok := a.Templates.Get(existing.Template)
	if !ok {
		return nil, fmt.Errorf("unknown tool template %q", existing.Template)
	}

	in.Alias = strings.TrimPrefix(existing.Name, existing.Template+"_")
	in.Variant = driverName(existing.Config)
	paths := tmpl.SecretPaths(in.Variant)
	// Before Build, so a template that REQUIRES a secret does not refuse an
	// edit in which the administrator simply did not retype it.
	if err := a.carryIntoSettings(in.Settings, existing.Config, paths); err != nil {
		return nil, fmt.Errorf("preserve the tool's secrets: %w", err)
	}
	inst, err := tmpl.Build(in)
	if err != nil {
		return nil, err
	}

	carried, err := a.carrySecrets(inst.Config, existing.Config, paths)
	if err != nil {
		return nil, fmt.Errorf("preserve the tool's secrets: %w", err)
	}
	sealed, err := a.sealConfigSecrets(carried, paths)
	if err != nil {
		return nil, fmt.Errorf("secure the tool's secrets: %w", err)
	}

	existing.FriendlyName = inst.Schema.FriendlyName
	existing.Description = inst.Schema.Description
	existing.InputSchema = inst.Schema.InputSchema
	existing.Risk = string(inst.Schema.Risk)
	// The grant is the one part of a config the form does not own, so an edit
	// carries it forward verbatim. Without this, saving the form would silently
	// disconnect the tool: Config() builds a fresh object from the fields, and
	// a key no field writes is simply absent from it.
	withGrant, err := carryGrant(sealed, existing.Config)
	if err != nil {
		return nil, err
	}
	existing.Config = withGrant
	existing.Guide = inst.Guide
	existing.Skills = skills
	if err := a.Store.Tools().UpdateCustom(ctx, existing, by); err != nil {
		return nil, err
	}
	return existing, nil
}

// CustomToolEdit is the whole edit form, assembled here rather than by the
// console: the driver, the form its fields make up, the current settings with
// secrets blanked (write-only, so they are never sent back), the guide, and each
// parameter's description. One request answers "show me this tool", because a
// form composed from several is a form whose parts can disagree.
type CustomToolEdit struct {
	Variant      string
	VariantLabel string
	// About and Params are the template's, carried here so that opening a tool is
	// ONE request. The console needed the whole template catalogue to show three
	// things: what this kind of tool is, what its driver is called, and which
	// parameters it takes. Fetching every template to answer a question about one
	// tool is the kind of thing that looks harmless and is not.
	About             string
	Params            []template.Param
	Sections          []template.Section
	Settings          map[string]any
	Guide             string
	ParamDescriptions map[string]string
}

// NewToolForm is the other half: the form for a tool that does not exist yet,
// and the values it starts with. Those are the template's own defaults, which is
// what a default is for.
//
// The pairing matters more than either half. A form and the values that fill it
// are one answer, and the difference between the two cases lives HERE, in one
// place: a new tool starts from what the template suggests, an existing one
// shows what is stored and nothing else. A console that decided that for itself
// would, sooner or later, offer somebody back a setting they had cleared.
func (a *App) NewToolForm(templateName, variant string) ([]template.Section, map[string]any, error) {
	tmpl, ok := a.Templates.Get(templateName)
	if !ok {
		return nil, nil, fmt.Errorf("unknown tool template %q", templateName)
	}
	sections, err := tmpl.Fields(variant)
	if err != nil {
		return nil, nil, err
	}
	values := map[string]any{}
	for _, section := range sections {
		for _, field := range section.Fields {
			values[field.Key] = field.Default
		}
	}
	return sections, values, nil
}

// CustomToolForEdit reads a stored custom tool back into the shape its form
// needs, without ever revealing a secret: the secret paths are blanked, so the
// form shows them empty and a blank on save means unchanged.
//
// It reads the OPENED config, and then shows only what the form declares.
//
// Opened, because a value can be sealed in the database and not be a secret any
// more: the API tool's additional headers and query parameters were sealed once
// and are ordinary settings now, and reading the stored config raw showed them
// as `enc:...` ciphertext to the person who typed them.
//
// Only the declared fields, because the config holds more than a form does. The
// reserved `grant` subtree is the case that matters: it is an access token and a
// refresh token, no form field names either of them, and flattening the whole
// config put both into the answer a browser gets. Filtering by what the form
// declares closes that structurally rather than by naming the subtree here,
// which would need naming again for the next one.
func (a *App) CustomToolForEdit(row *model.Tool) (CustomToolEdit, error) {
	variant := driverName(row.Config)
	// A value that will not open does not stop the form. It is the only place
	// a wrong or lost secret can be RETYPED, so refusing to draw it would take
	// the one repair away: what cannot be opened is left as it is, which the
	// blanking below hides for a secret path and shows as ciphertext for
	// anything else, where at least it can be seen and replaced.
	opened := row.Config
	if plain, err := a.openConfigSecrets(row.Config); err == nil {
		opened = plain
	} else {
		a.Log.Warn().Err(err).Str("tool", row.Name).Msg("a stored value could not be opened for the form")
	}
	var nested map[string]any
	if err := json.Unmarshal(opened, &nested); err != nil {
		return CustomToolEdit{}, fmt.Errorf("read config: %w", err)
	}
	delete(nested, "driver")
	held := map[string]any{}
	flattenConfig("", nested, held)
	settings := map[string]any{}

	descriptions := paramDescriptions(row.InputSchema)
	var sections []template.Section
	var params []template.Param
	var about, variantLabel string
	if tmpl, ok := a.Templates.Get(row.Template); ok {
		var err error
		if sections, err = tmpl.Fields(variant); err != nil {
			return CustomToolEdit{}, fmt.Errorf("build the tool's form: %w", err)
		}
		// What this form asks for, and nothing else in the config.
		for _, section := range sections {
			for _, f := range section.Fields {
				if v, ok := held[f.Key]; ok {
					settings[f.Key] = v
				}
			}
		}
		params, about = tmpl.Params(), tmpl.Description()
		for _, v := range tmpl.Variants() {
			if v.Key == variant {
				variantLabel = v.Label
			}
		}
		for _, p := range tmpl.SecretPaths(variant) {
			if _, present := settings[p]; present {
				settings[p] = ""
			}
		}
		// A parameter the template has gained since this tool was made has nothing
		// stored against it, and an empty box in the form would read as a parameter
		// nobody has described. The template's own wording fills it, which is what
		// a tool created today would start from.
		for _, p := range tmpl.Params() {
			if strings.TrimSpace(descriptions[p.Key]) == "" {
				descriptions[p.Key] = p.Description
			}
		}
		// The same for a setting that can only be one of a few things. A choice
		// with no value is not a choice somebody cleared, it is a control that
		// cannot hold what it was given: it shows its first option while holding
		// nothing, and the save is then refused for a field the form appeared to
		// have filled in. Its default fills it, as on a new tool.
		//
		// Blank counts as no value here, not only absent. A choice has no empty
		// option to pick, so an empty one was never chosen: it was stored by a
		// form that had nothing to send. That is the case this got wrong first
		// time round, and it is the one that actually happens.
		//
		// Only a choice. A list somebody emptied was emptied on purpose, and
		// handing its default back would put what they took out straight back in.
		for _, s := range sections {
			for _, f := range s.Fields {
				if f.Type != template.FieldSelect || f.Secret || f.Default == "" {
					continue
				}
				if stored, ok := settings[f.Key]; !ok || fmt.Sprint(stored) == "" {
					settings[f.Key] = f.Default
				}
			}
		}
	}
	return CustomToolEdit{
		Variant:           variant,
		VariantLabel:      variantLabel,
		About:             about,
		Params:            params,
		Sections:          sections,
		Settings:          settings,
		Guide:             row.Guide,
		ParamDescriptions: descriptions,
	}, nil
}

// carryIntoSettings fills each blank secret in what the administrator submitted
// with the one already stored, BEFORE the template is asked to build or
// validate anything.
//
// The order is the point. A secret is blanked when the form is reopened, so an
// edit that changed a host arrives with an empty password, and carrying it
// afterwards is too late for a template that REQUIRES one: it has already
// refused settings that are in fact complete. Invisible until a template makes
// a secret mandatory (an OAuth client secret is), and then it is an edit that
// cannot be saved or tested at all.
//
// Works on the flat settings because SecretPaths are the form's own dotted
// keys, which is what makes this a fill rather than a merge.
func (a *App) carryIntoSettings(settings map[string]any, stored json.RawMessage, paths []string) error {
	if len(paths) == 0 || len(stored) == 0 || settings == nil {
		return nil
	}
	var old map[string]any
	if err := json.Unmarshal(stored, &old); err != nil {
		return err
	}
	for _, path := range paths {
		if v, ok := settings[path]; ok && strings.TrimSpace(fmt.Sprint(v)) != "" {
			continue
		}
		sealed, ok := leafString(old, path)
		if !ok || !strings.HasPrefix(sealed, sealMarker) {
			continue
		}
		opened, err := a.openValue(sealed)
		if err != nil {
			return err
		}
		if plain, ok := opened.(string); ok && plain != "" {
			settings[path] = plain
		}
	}
	return nil
}

// carrySecrets fills each blank secret in the new config with the plaintext of
// the value sealed in the old one, so a secret the administrator did not
// re-enter survives the edit. The result is all plaintext, ready to seal (on a
// save) or to open a connection with (on a test). A secret typed through is
// kept as it is.
func (a *App) carrySecrets(newCfg, oldCfg json.RawMessage, paths []string) (json.RawMessage, error) {
	var nm, om map[string]any
	if err := json.Unmarshal(newCfg, &nm); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(oldCfg, &om); err != nil {
		return nil, err
	}
	for _, p := range paths {
		if cur, ok := leafString(nm, p); ok && cur != "" {
			continue
		}
		old, ok := leafString(om, p)
		if !ok || !strings.HasPrefix(old, sealMarker) {
			continue
		}
		opened, err := a.openValue(old)
		if err != nil {
			return nil, err
		}
		plain, _ := opened.(string)
		setLeaf(nm, p, plain)
	}
	return json.Marshal(nm)
}

// driverName reads the driver key stored in a connection config.
func driverName(config json.RawMessage) string {
	var c struct {
		Driver string `json:"driver"`
	}
	_ = json.Unmarshal(config, &c)
	return c.Driver
}

// flattenConfig turns the nested config back into the dotted keys the form uses
// (tls.mode, ssh.host), the reverse of the template's nesting.
func flattenConfig(prefix string, m map[string]any, out map[string]any) {
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok {
			flattenConfig(key, sub, out)
		} else {
			out[key] = v
		}
	}
}

// paramDescriptions reads each input's description out of a tool's JSON Schema,
// so the edit form can show what the administrator wrote for each parameter.
func paramDescriptions(schema json.RawMessage) map[string]string {
	var s struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(schema, &s)
	out := map[string]string{}
	for k, p := range s.Properties {
		out[k] = p.Description
	}
	return out
}

// TestCustomTool opens a live connection for a stored tool (or a draft config)
// and checks it, for the admin's Test button. The config's secrets are opened
// first, so it works on a saved tool without the secret being re-entered.
func (a *App) TestCustomTool(ctx context.Context, templateName string, config json.RawMessage) error {
	tmpl, ok := a.Templates.Get(templateName)
	if !ok {
		return fmt.Errorf("unknown tool template %q", templateName)
	}
	opened, err := a.openConfigSecrets(config)
	if err != nil {
		return fmt.Errorf("open the tool's secrets: %w", err)
	}
	return tmpl.Test(ctx, opened)
}

// TestCustomToolSettings tests a draft the admin is filling in, before it is
// saved: it builds the config from the form settings and opens a connection.
// The settings are plaintext (never yet sealed), which pass through the opener
// untouched.
func (a *App) TestCustomToolSettings(ctx context.Context, templateName, variant string, settings map[string]any) error {
	tmpl, ok := a.Templates.Get(templateName)
	if !ok {
		return fmt.Errorf("unknown tool template %q", templateName)
	}
	config, err := tmpl.Config(variant, settings)
	if err != nil {
		return err
	}
	return tmpl.Test(ctx, config)
}

// TestCustomToolEdit tests the settings an administrator is editing against a
// stored tool. It is the test twin of UpdateCustomTool: a secret left blank is
// filled from the value already sealed in the row, then the secrets are opened
// for the live connection, so a connection can be checked after changing a host
// without re-entering the password. The identity (template, driver) is the
// stored tool's, never the client's.
func (a *App) TestCustomToolEdit(ctx context.Context, workspaceID, id int64, settings map[string]any) error {
	existing, err := a.Store.Tools().GetByID(ctx, workspaceID, id)
	if err != nil {
		return err
	}
	if existing.Kind != string(tool.KindCustom) {
		return fmt.Errorf("only a custom tool can be tested this way")
	}
	tmpl, ok := a.Templates.Get(existing.Template)
	if !ok {
		return fmt.Errorf("unknown tool template %q", existing.Template)
	}
	variant := driverName(existing.Config)

	if err := a.carryIntoSettings(settings, existing.Config, tmpl.SecretPaths(variant)); err != nil {
		return fmt.Errorf("preserve the tool's secrets: %w", err)
	}
	draft, err := tmpl.Config(variant, settings)
	if err != nil {
		return err
	}
	// Carry a blank secret forward as plaintext, so the draft is a complete
	// plaintext config to open a connection with.
	carried, err := a.carrySecrets(draft, existing.Config, tmpl.SecretPaths(variant))
	if err != nil {
		return fmt.Errorf("preserve the tool's secrets: %w", err)
	}
	return tmpl.Test(ctx, carried)
}

// A template's own operations.
//
// Testing a connection is the one operation every template has, and for some it
// is not a single step: a server may want a verification code before it will
// accept anything. Rather than teach this layer what any of that means, a
// template may offer actions of its own, and what comes back is either an
// outcome or a request for more, in the field vocabulary the console already
// renders. Nothing here reads the payload in either direction.

// ToolAction is one request to run a template's operation: which one, the token
// of the question being answered when it answers one, and the answers.
type ToolAction struct {
	Name   string
	Token  string
	Values map[string]string
}

// RunToolAction runs an action against settings an administrator is filling in,
// before there is a tool to run it against.
func (a *App) RunToolAction(ctx context.Context, templateName, variant string, settings map[string]any, action ToolAction) (template.ActionResult, error) {
	tmpl, ok := a.Templates.Get(templateName)
	if !ok {
		return template.ActionResult{}, fmt.Errorf("unknown tool template %q", templateName)
	}
	config, err := tmpl.Config(variant, settings)
	if err != nil {
		return template.ActionResult{}, err
	}
	return a.runAction(ctx, tmpl, config, action)
}

// RunToolActionForEdit runs an action against the settings an administrator is
// editing, carrying forward any secret they did not retype, so a connection can
// be tested after changing a host without re-entering a password.
func (a *App) RunToolActionForEdit(ctx context.Context, workspaceID, userID, id int64, settings map[string]any, action ToolAction) (template.ActionResult, error) {
	existing, err := a.Store.Tools().GetByID(ctx, workspaceID, id)
	if err != nil {
		return template.ActionResult{}, err
	}
	if existing.Kind != string(tool.KindCustom) {
		return template.ActionResult{}, fmt.Errorf("only a custom tool can be tested this way")
	}
	tmpl, ok := a.Templates.Get(existing.Template)
	if !ok {
		return template.ActionResult{}, fmt.Errorf("unknown tool template %q", existing.Template)
	}
	variant := driverName(existing.Config)

	if err := a.carryIntoSettings(settings, existing.Config, tmpl.SecretPaths(variant)); err != nil {
		return template.ActionResult{}, fmt.Errorf("preserve the tool's secrets: %w", err)
	}
	draft, err := tmpl.Config(variant, settings)
	if err != nil {
		return template.ActionResult{}, err
	}
	carried, err := a.carrySecrets(draft, existing.Config, tmpl.SecretPaths(variant))
	if err != nil {
		return template.ActionResult{}, fmt.Errorf("preserve the tool's secrets: %w", err)
	}
	// And the sign-in, for the same reason the save path carries it: the form
	// does not send it, so a config built from the form has none. Without this
	// a CONNECTED tool tests as though nobody had signed in, which means the
	// test makes an unauthenticated call and the console offers the sign-in
	// again to somebody who has already done it.
	carried, err = carryGrant(carried, existing.Config)
	if err != nil {
		return template.ActionResult{}, err
	}
	result, err := a.runAction(ctx, tmpl, carried, action)
	if err != nil {
		return result, err
	}
	return a.offerConnection(ctx, tmpl, carried, workspaceID, userID, id, result), nil
}

// offerConnection adds the sign-in when this tool needs one and has not had it.
//
// After the test rather than instead of it: an address or a client id that is
// wrong should be said so here, where somebody is looking at the form, rather
// than by a service refusing them halfway through a browser redirect.
func (a *App) offerConnection(ctx context.Context, tmpl template.Template, config json.RawMessage,
	workspaceID, userID, toolID int64, result template.ActionResult,
) template.ActionResult {
	granted, ok := tmpl.(template.GrantedTemplate)
	if !ok || !result.OK || result.Prompt != nil {
		return result
	}
	opened, err := a.openConfigSecrets(config)
	if err != nil || !granted.Connectable(opened) {
		return result
	}
	if held := settingsOf(opened).Grant; held.Held() {
		result.Message = "Connected"
		if held.ConnectedByName != "" {
			result.Message += " by " + held.ConnectedByName
		}
		if held.ConnectedAt != nil {
			result.Message += " on " + held.ConnectedAt.Format("2 January 2006")
		}
		result.Message += ". These settings are correct."
		return result
	}
	where, err := a.BeginToolConnect(ctx, workspaceID, toolID, userID)
	if err != nil {
		// The settings are fine and only the sign-in could not be started, so
		// that is what it says: reporting the whole test as failed would send
		// somebody back to check an address that is right.
		result.Message = "These settings are correct, but the sign-in could not be started: " + err.Error()
		return result
	}
	result.Message = "These settings are correct. Now sign in to the service to finish connecting."
	result.Visit, result.Visiting = where, "Connect"
	return result
}

// carryGrant moves the sign-in from the stored config into the one an edit
// produced, because the form neither shows it nor sends it back.
//
// Verbatim, sealed value and all: it is already sealed in the stored config,
// and opening it to seal it again would be two more chances to write a
// plaintext token into the database for no gain.
//
// Both sides are the config as STORED, so nothing here needs the keyring.
func carryGrant(fresh, stored json.RawMessage) (json.RawMessage, error) {
	var was map[string]any
	if err := json.Unmarshal(stored, &was); err != nil {
		return nil, fmt.Errorf("read the tool's stored settings: %w", err)
	}
	held, ok := was[apitool.GrantPath]
	if !ok {
		return fresh, nil
	}
	var now map[string]any
	if err := json.Unmarshal(fresh, &now); err != nil {
		return nil, fmt.Errorf("read the edited settings: %w", err)
	}
	now[apitool.GrantPath] = held
	return json.Marshal(now)
}

// settingsOf reads a tool's opened config as API-tool settings, answering an
// empty one for a tool built from any other template.
func settingsOf(opened []byte) apitool.Settings {
	settings, err := apitool.Parse(opened)
	if err != nil {
		return apitool.Settings{}
	}
	return settings
}

// toolSkillNote is the closing section of a tool's guide: the written
// procedures that document it, and how to open one.
//
// Handles and names only, the same as the prompt's map of an agent's skills and
// for the same reason: a description is the field the package format designed
// to be selected on and runs to hundreds of characters. What each is FOR is one
// call away and not worth spending here.
//
// The call shape is written out with the first handle, because a model that has
// to infer one from an input schema sometimes infers it wrongly, and this is
// the same example the skills section of the prompt uses so the two cannot
// teach two different shapes.
func toolSkillNote(docs []skillOnHand) string {
	if len(docs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("WRITTEN PROCEDURES FOR THIS ABILITY. Somebody documented how to use this one, " +
		"and what they wrote is the specific detail this guide does not carry: the exact paths, " +
		"what the codes mean, the order things have to happen in. Read it before working any of " +
		"that out yourself, with " + skills.LoadName + ", for example " +
		skills.LoadName + "(skill: \"" + docs[0].handle + "\").\n")
	for _, d := range docs {
		b.WriteString("\n- ")
		b.WriteString(d.handle)
		// The name only when it says something the handle does not, which is
		// the rule everywhere a skill is printed.
		if d.name != "" && d.name != d.handle {
			b.WriteString(" (")
			b.WriteString(d.name)
			b.WriteString(")")
		}
	}
	return b.String()
}

// runAction opens the configuration's secrets and hands it to the template. A
// template with no actions of its own still answers the one every template has,
// through Test, so the console has a single Test button either way.
func (a *App) runAction(ctx context.Context, tmpl template.Template, config json.RawMessage, action ToolAction) (template.ActionResult, error) {
	opened, err := a.openConfigSecrets(config)
	if err != nil {
		return template.ActionResult{}, fmt.Errorf("open the tool's secrets: %w", err)
	}

	name := action.Name
	if name == "" {
		name = template.ActionTest
	}

	if actions, ok := tmpl.(template.ActionTemplate); ok {
		body, err := json.Marshal(map[string]any{"token": action.Token, "values": action.Values})
		if err != nil {
			return template.ActionResult{}, fmt.Errorf("encode the request: %w", err)
		}
		return actions.Action(ctx, opened, name, body)
	}

	if name != template.ActionTest {
		return template.ActionResult{}, fmt.Errorf("this tool has no %q action", name)
	}
	if err := tmpl.Test(ctx, opened); err != nil {
		return template.ActionResult{OK: false, Message: err.Error()}, nil
	}
	return template.ActionResult{OK: true, Message: "Connected successfully."}, nil
}

// bindCustom turns a stored custom-tool row into a schema and handler for the
// loadout: it opens the sealed secrets, binds the template's handler to the
// config, and enriches the schema with the template's guide. A row whose
// template is gone, or whose config is broken, is skipped (logged), never fatal.
//
// owner is the agent the handler is being built for, so a template that keeps
// anything between calls files it under that agent rather than the conversation
// they may be sharing (tool.Owner).
func (a *App) bindCustom(row *model.Tool, owner tool.Owner, docs []skillOnHand) (tool.Schema, tool.Handler, bool) {
	tmpl, ok := a.Templates.Get(row.Template)
	if !ok {
		a.Log.Warn().Str("tool", row.Name).Str("template", row.Template).Msg("custom tool references an unknown template")
		return tool.Schema{}, nil, false
	}
	opened, err := a.openConfigSecrets(row.Config)
	if err != nil {
		a.Log.Warn().Err(err).Str("tool", row.Name).Msg("custom tool secrets could not be opened")
		return tool.Schema{}, nil, false
	}
	// A template whose credential was partly GRANTED is bound through its own
	// seam, with a grant for this tool. Optional, like Displayed: a template
	// that needs nothing of the sort goes through Bind untouched.
	//
	// The grant is built here because this is where the tool's identity is, and
	// because renewing one needs the store, the keyring and a way to talk to an
	// authorization server, none of which a template has.
	var handler tool.Handler
	if granted, ok := tmpl.(template.GrantedTemplate); ok && granted.Connectable(opened) {
		handler, err = granted.BindGranted(opened, owner, &toolGrant{
			app: a, toolID: row.ID, workspaceID: row.WorkspaceID, settings: settingsOf(opened),
		})
	} else {
		handler, err = tmpl.Bind(opened, owner)
	}
	if err != nil {
		a.Log.Warn().Err(err).Str("tool", row.Name).Msg("custom tool could not be bound")
		return tool.Schema{}, nil, false
	}
	// The guide is the tool's own text (or the template's default), surfaced by
	// tool_guide as a plain string.
	guideText := row.Guide
	if strings.TrimSpace(guideText) == "" {
		guideText = tmpl.DefaultGuide()
	}
	// The documentation somebody attached to THIS tool, named at the end of its
	// guide rather than in the prompt.
	//
	// This is the second half of a drill-down and it is deliberate. A skill for
	// a tool is often the only place the specifics live: which paths an API
	// has, what its codes mean, the order its calls go in. None of that belongs
	// in a description sent every turn, and all of it is wanted the moment the
	// tool is actually going to be used, which is exactly when the guide is
	// read.
	if note := toolSkillNote(docs); note != "" {
		if strings.TrimSpace(guideText) != "" {
			guideText += "\n\n"
		}
		guideText += note
	}
	var guide json.RawMessage
	if strings.TrimSpace(guideText) != "" {
		guide, _ = json.Marshal(guideText)
	}

	// System-owned documentation the form cannot remove: the engine note is
	// appended to the description so the model always knows which database this
	// is, and the topics are the tool's drill-down guide, namespaced by the
	// tool's name so each instance's topics are its own.
	doc := tmpl.Documentation(row.Config)
	description := strings.TrimSpace(row.Description)
	if note := strings.TrimSpace(doc.Note); note != "" {
		if description != "" {
			description += "\n\n"
		}
		description += note
	}

	// THE PARAMETERS ARE THE TEMPLATE'S, read every time this tool is bound.
	//
	// They used to be `row.InputSchema`: the copy written when the tool was
	// made, and frozen there. So a template improvement reached a tool created
	// afterwards and no other, and there was no way to get it: the form does not
	// offer the shape, because the shape is not an administrator's to choose.
	//
	// It cost exactly what that implies. The HTTP template learnt that a path
	// may be left out (for an endpoint there is nothing to append to) and that
	// a body need not be JSON, and the tool somebody had already built went on
	// being handed `required: [method, path]` and no way to say `body_type`:
	// fixed in the product, unfixed for the person who reported it.
	//
	// The administrator's own WORDING is still theirs, and is what the stored
	// schema is actually for: their descriptions are read back out of it and put
	// over the template's. Nothing else in it is authoritative any more.
	//
	// The guide beside this has always worked this way (row.Guide, falling back
	// to the template's). This is the same rule, applied to the half that
	// decides what a model is allowed to send.
	schema := tool.Schema{
		Name:             row.Name,
		FriendlyName:     row.FriendlyName,
		Description:      description,
		InputSchema:      template.InputSchema(tmpl.Params(), paramDescriptions(row.InputSchema)),
		Kind:             tool.KindCustom,
		Risk:             tool.RiskLevel(row.Risk),
		RequiresApproval: row.RequiresApproval,
		Guide:            guide,
		Topics:           namespaceTopics(row.Name, doc.Topics),
	}
	return schema, handler, true
}

// namespaceTopics prefixes a template's bare topic slugs (and their edge
// targets) with the tool's name, so each tool's topics are globally unique and
// named "<tool>/<slug>" the way the tool_guide graph expects.
func namespaceTopics(toolName string, topics []tool.Topic) []tool.Topic {
	if len(topics) == 0 {
		return nil
	}
	out := make([]tool.Topic, len(topics))
	for i, t := range topics {
		edges := make([]tool.TopicEdge, len(t.Edges))
		for j, e := range t.Edges {
			e.To = toolName + "/" + e.To
			edges[j] = e
		}
		t.ID = toolName + "/" + t.ID
		t.Edges = edges
		out[i] = t
	}
	return out
}

// --- secret sealing in the config JSON --------------------------------------

// sealConfigSecrets seals the plaintext values at the given dotted paths. The
// values must be plaintext: the update path carries a kept secret forward as
// plaintext before calling this, so a value is sealed whatever it looks like,
// even one that begins with the marker, and a secret can never survive in the
// clear. An empty value is left as-is: blank means "no secret" on create, and on
// an edit the kept plaintext was already carried in.
func (a *App) sealConfigSecrets(config json.RawMessage, paths []string) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal(config, &m); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	for _, path := range paths {
		leaf, ok := leafString(m, path)
		if !ok || leaf == "" {
			continue
		}
		sealed, err := a.Keyring.Seal([]byte(leaf))
		if err != nil {
			return nil, err
		}
		setLeaf(m, path, sealMarker+base64.StdEncoding.EncodeToString(sealed))
	}
	return json.Marshal(m)
}

// openConfigSecrets returns the config with every sealed value opened, by
// walking it: a caller needs no knowledge of which fields were secret.
func (a *App) openConfigSecrets(config json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(config, &v); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	opened, err := a.openValue(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(opened)
}

func (a *App) openValue(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		for k, inner := range t {
			opened, err := a.openValue(inner)
			if err != nil {
				return nil, err
			}
			t[k] = opened
		}
		return t, nil
	case []any:
		for i, inner := range t {
			opened, err := a.openValue(inner)
			if err != nil {
				return nil, err
			}
			t[i] = opened
		}
		return t, nil
	case string:
		if !strings.HasPrefix(t, sealMarker) {
			return t, nil
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(t, sealMarker))
		if err != nil {
			return nil, fmt.Errorf("decode sealed value: %w", err)
		}
		plain, err := a.Keyring.Open(raw)
		if err != nil {
			return nil, fmt.Errorf("open sealed value: %w", err)
		}
		return string(plain), nil
	default:
		return v, nil
	}
}

// leafString reads a dotted-path leaf as a string.
func leafString(m map[string]any, path string) (string, bool) {
	parts := strings.Split(path, ".")
	cur := m
	for i, p := range parts {
		if i == len(parts)-1 {
			s, ok := cur[p].(string)
			return s, ok
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			return "", false
		}
		cur = next
	}
	return "", false
}

// setLeaf writes a dotted-path leaf, creating intermediate objects as needed.
func setLeaf(m map[string]any, path string, val string) {
	parts := strings.Split(path, ".")
	cur := m
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = val
			return
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
}
