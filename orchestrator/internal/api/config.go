package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
)

// The admin surface of the layered configuration model: the tools a workspace
// has, the agents that use them, and the workflows that override the defaults
// for some people on some channels.

type configHandlers struct{ app *app.App }

func mountConfig(r chi.Router, a *app.App) {
	h := &configHandlers{app: a}

	// A built-in tool is never created over the API: it arrives with a deploy,
	// because it is code, and what an administrator decides is whether it is on,
	// whether it needs a human, and who may reach it. A NATIVE custom tool is
	// the exception, instantiated from a template here (POST /tools/custom).
	r.Route("/tools", func(r chi.Router) {
		r.With(requirePermission(a, model.PermToolsView)).Get("/", h.listTools)
		// The native tool templates and the form for one, so the console can
		// offer "Add tool" without knowing what a template needs.
		r.With(requirePermission(a, model.PermToolsView)).Get("/templates", h.listTemplates)
		r.With(requirePermission(a, model.PermToolsView)).Get("/templates/{name}/fields", h.templateFields)
		// A native custom tool is created here (unlike a built-in, which arrives
		// with a deploy): pick a template, fill its form, test it, save it.
		r.With(requirePermission(a, model.PermToolsEdit)).Post("/custom", h.createCustomTool)
		r.With(requirePermission(a, model.PermToolsEdit)).Post("/custom/test", h.testCustomTool)
		// A native custom tool's configuration is edited here (its identity,
		// template and driver, stays fixed); the admin-owned status, approval and
		// grants stay on PUT /{id}.
		r.With(requirePermission(a, model.PermToolsEdit)).Put("/custom/{id}", h.updateCustomTool)
		// Testing an edit tests against the stored tool, so a secret left blank is
		// carried forward, unlike the stateless create-time test above.
		r.With(requirePermission(a, model.PermToolsEdit)).Post("/custom/{id}/test", h.testCustomToolEdit)
		r.With(requirePermission(a, model.PermToolsView)).Get("/{id}", h.getTool)
		r.With(requirePermission(a, model.PermToolsEdit)).Put("/{id}", h.updateTool)
		r.With(requirePermission(a, model.PermToolsEdit)).Delete("/{id}", h.deleteCustomTool)
	})

	// The Gateway SCREEN, in one answer, shaped like the screen: what arrives is
	// read (files, audio), the Gateway thinks with it, and work goes to agents.
	// `/agents` stays the collection it is named after.
	r.Route("/gateway", func(r chi.Router) {
		r.With(requirePermission(a, model.PermAgentsView)).Get("/", h.gatewayScreen)
		// Each section of the screen is written on its own. A form that changes
		// four file rules used to have to send the whole agent back, which meant
		// fetching the whole agent first to have something to echo: its
		// instructions, its brains, its confirm list and its timestamps, all
		// round-tripped so one field could move.
		r.With(requirePermission(a, model.PermAgentsEdit)).Put("/files", h.putGatewayFiles)
		r.With(requirePermission(a, model.PermAgentsEdit)).Put("/audio", h.putGatewayAudio)
		// And each form is READ on its own, when it opens: the values it edits
		// and the choices it offers, together. A dialog that changes one audio
		// model was waiting on the tool catalogue and the brains, because every
		// form on this screen shared one pile of requests.
		r.With(requirePermission(a, model.PermAgentsView)).Get("/files/form", h.filesForm)
		r.With(requirePermission(a, model.PermAgentsView)).Get("/audio/form", h.audioForm)
	})

	r.Route("/agents", func(r chi.Router) {
		r.With(requirePermission(a, model.PermAgentsView)).Get("/", h.listAgents)
		// The form for an agent that does not exist yet, and for one that does.
		// Static before parameterised, so `form` is never read as an id.
		r.With(requirePermission(a, model.PermAgentsView)).Get("/form", h.agentForm)
		r.With(requirePermission(a, model.PermAgentsView)).Get("/{id}/form", h.agentForm)
		r.With(requirePermission(a, model.PermAgentsView)).Get("/{id}", h.getAgent)
		r.With(requirePermission(a, model.PermAgentsCreate)).Post("/", h.createAgent)
		r.With(requirePermission(a, model.PermAgentsEdit)).Put("/{id}", h.updateAgent)
		r.With(requirePermission(a, model.PermAgentsDelete)).Delete("/{id}", h.deleteAgent)
	})

	r.Route("/workflows", func(r chi.Router) {
		r.With(requirePermission(a, model.PermWorkflowsView)).Get("/", h.listWorkflows)
		r.With(requirePermission(a, model.PermWorkflowsView)).Get("/{id}", h.getWorkflow)
		r.With(requirePermission(a, model.PermWorkflowsCreate)).Post("/", h.createWorkflow)
		r.With(requirePermission(a, model.PermWorkflowsEdit)).Put("/{id}", h.updateWorkflow)
		r.With(requirePermission(a, model.PermWorkflowsDelete)).Delete("/{id}", h.deleteWorkflow)

		r.With(requirePermission(a, model.PermWorkflowsView)).Get("/{id}/versions", h.listVersions)
		r.With(requirePermission(a, model.PermWorkflowsEdit)).Post("/{id}/versions", h.createVersion)
		r.With(requirePermission(a, model.PermWorkflowsPublish)).Post("/{id}/publish", h.publishVersion)

		r.With(requirePermission(a, model.PermWorkflowsView)).Get("/{id}/assignments", h.listAssignments)
		r.With(requirePermission(a, model.PermWorkflowsEdit)).Put("/{id}/assignments", h.setAssignments)
	})
}

// --- tools ---------------------------------------------------------------------

type toolBody struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Template     string `json:"template,omitempty"`
	FriendlyName string `json:"friendly_name"`
	Description  string `json:"description"`
	// About is what a person reads about a built-in tool: what it is for, in
	// business language. Description is the model's copy, and a console that
	// shows it is showing a prompt to somebody deciding a permission.
	About       string          `json:"about,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Risk        string          `json:"risk"`
	Status      string          `json:"status"`
	// Source is where this tool came from: the connection that projected it, or
	// the product itself. The catalogue is read in groups by it, which is what a
	// prefix on a name was standing in for.
	Source string `json:"source"`
	// ShortName is Name without that prefix, for a screen that shows the source
	// as a heading. Name stays what it is: what the model calls, what a grant
	// records, what everything else in the product means by this tool.
	ShortName string  `json:"short_name"`
	Grants    []int64 `json:"grants"`

	// The projection flags of a remote MCP tool, so the tools screen can
	// mark what drifted. Zero on every other kind.
	MCPServerID         *int64     `json:"mcp_server_id,omitempty"`
	RemoteMissing       bool       `json:"remote_missing,omitempty"`
	DefinitionChangedAt *time.Time `json:"definition_changed_at,omitempty"`
}

func (h *configHandlers) toolToBody(t *model.Tool, sources map[int64]mcpSource) toolBody {
	body := toolBody{
		ID:           t.ID,
		Name:         t.Name,
		Kind:         t.Kind,
		Source:       toolSource(t, sources),
		ShortName:    toolShortName(t, sources),
		Template:     t.Template,
		FriendlyName: t.FriendlyName,
		Description:  t.Description,
		InputSchema:  t.InputSchema,
		Risk:         t.Risk,
		Status:       t.Status,
		Grants:       t.Grants,

		MCPServerID:         t.MCPServerID,
		RemoteMissing:       t.RemoteMissing,
		DefinitionChangedAt: t.DefinitionChangedAt,
	}
	// What a PERSON is told this tool is for. Approval is not read from the
	// schema here: where a call pauses is the agent's screen, not this one.
	if schema, ok := h.app.Tools.Load([]string{t.Name}).Schema(t.Name); ok {
		body.About = schema.About
	}
	return body
}

// mcpSource is what a connection contributes to the presentation of the tools
// it projected: the heading they are grouped under, and the prefix that heading
// stands in for.
type mcpSource struct {
	Name   string
	Prefix string
}

// toolSources names where each tool comes from, so a catalogue can be read in
// groups instead of as one flat list with a prefix people are asked to decode.
//
// A projected tool belongs to the CONNECTION that projected it, by that
// connection's name. Everything else is ours, split the way an administrator
// already thinks about it: what the product ships, and what somebody here built.
//
// Answered once per request and passed down, rather than looked up per tool: a
// workspace with three connections and sixty tools would otherwise ask the same
// question sixty times.
func mcpToolSources(ctx context.Context, a *app.App, workspaceID int64) map[int64]mcpSource {
	servers, err := a.Store.MCPServers().List(ctx, workspaceID)
	if err != nil {
		// A name we cannot look up is not a reason to refuse the screen. The
		// group falls back to the generic one below, the name keeps its prefix,
		// and everything still lists.
		return nil
	}
	sources := make(map[int64]mcpSource, len(servers))
	for _, s := range servers {
		sources[s.ID] = mcpSource{Name: s.Name, Prefix: s.ToolPrefix}
	}
	return sources
}

// toolSources answers it for the workspace this request is in.
func (h *configHandlers) toolSources(r *http.Request) map[int64]mcpSource {
	return mcpToolSources(r.Context(), h.app, claimsFrom(r).WorkspaceID)
}

// toolSource is the group heading for one tool.
func toolSource(t *model.Tool, sources map[int64]mcpSource) string {
	if t.MCPServerID != nil {
		if src, known := sources[*t.MCPServerID]; known && src.Name != "" {
			return src.Name
		}
		return "Connected service"
	}
	if t.Kind == string(tool.KindCustom) {
		return "Custom"
	}
	return "Built-in"
}

// toolShortName is the name a person reads, under the heading that says which
// service it came from: `update_entity`, not `nli_update_entity`.
//
// The prefix is our namespacing and nobody else's business. It is still the
// name the model calls and the name every grant is recorded under; only the
// screen drops it, and only where the heading has already said what it meant.
// A connection we cannot resolve leaves the name whole rather than guessing at
// where one name ends and the other begins.
func toolShortName(t *model.Tool, sources map[int64]mcpSource) string {
	if t.MCPServerID == nil {
		return t.Name
	}
	return app.UnprefixedToolName(sources[*t.MCPServerID].Prefix, t.Name)
}

func (h *configHandlers) listTools(w http.ResponseWriter, r *http.Request) {
	tools, err := h.app.Store.Tools().List(r.Context(), claimsFrom(r).WorkspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	sources := h.toolSources(r)
	out := make([]toolBody, 0, len(tools))
	for _, t := range tools {
		// A tool the code marks Hidden is real and grantable, but not advertised:
		// infrastructure or a testing aid an administrator has no reason to browse.
		// Keep it out of the catalog while leaving it in the table for grants.
		if s, ok := h.app.Tools.Lookup(t.Name); ok && s.Hidden {
			continue
		}
		out = append(out, h.toolToBody(t, sources))
	}
	writeJSON(w, http.StatusOK, out)
}

// toolDetailBody is a single tool with, for a custom tool, everything its edit
// form needs to prefill: the driver, the current settings (secrets blanked), the
// guide, and each parameter's description.
// toolDetailBody is everything the edit form needs, in one answer: the tool, the
// form its driver makes up, and the values that fill it. Composing a form from
// several requests is how its parts come to disagree.
type toolDetailBody struct {
	toolBody
	// Groups is who this tool CAN be granted to; `grants` on the tool itself is
	// who it IS granted to. Both are the edit form, so both arrive with it: the
	// screen used to fetch the workspace's groups separately, on the mere
	// possibility that somebody opened a dialog.
	Groups            []groupOption      `json:"groups"`
	Variant           string             `json:"variant,omitempty"`
	VariantLabel      string             `json:"variant_label,omitempty"`
	About             string             `json:"about,omitempty"`
	Params            []template.Param   `json:"params,omitempty"`
	Sections          []template.Section `json:"sections,omitempty"`
	Guide             string             `json:"guide,omitempty"`
	Settings          map[string]any     `json:"settings,omitempty"`
	ParamDescriptions map[string]string  `json:"param_descriptions,omitempty"`
}

// groupOption is a group as a form offers it to be ticked.
type groupOption struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (h *configHandlers) getTool(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ws := claimsFrom(r).WorkspaceID
	t, err := h.app.Store.Tools().GetByID(r.Context(), ws, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	groups, err := h.app.Store.Groups().List(r.Context(), ws)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	out := toolDetailBody{toolBody: h.toolToBody(t, h.toolSources(r)), Groups: make([]groupOption, 0, len(groups))}
	for _, g := range groups {
		out.Groups = append(out.Groups, groupOption{ID: g.ID, Name: g.Name})
	}
	if t.Kind != string(tool.KindCustom) {
		// A native tool with settings of its own: the terminal's list of what
		// it may run. The form comes from the code that ships the tool and the
		// values from its row, which is the same pair a custom tool has, so the
		// console renders it with the component it already has.
		if schema, known := h.app.Tools.Lookup(t.Name); known && len(schema.Settings) > 0 {
			out.Sections = schema.Settings
			out.Settings = declaredValues(schema.Settings, t.Config)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	edit, err := h.app.CustomToolForEdit(t)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	out.Variant = edit.Variant
	out.VariantLabel = edit.VariantLabel
	out.About = edit.About
	out.Params = edit.Params
	out.Sections = edit.Sections
	out.Guide = edit.Guide
	out.Settings = edit.Settings
	out.ParamDescriptions = edit.ParamDescriptions
	writeJSON(w, http.StatusOK, out)
}

type toolUpdateBody struct {
	Status string  `json:"status"`
	Grants []int64 `json:"grants"`
	// Settings are a native tool's own, for the tools that declare a form: the
	// terminal's list of what it may run. Flat keys as the form uses them
	// (policy.mode), stored nested, exactly as a custom tool's are.
	//
	// Only what the tool DECLARES is kept. A key nothing declares is ignored
	// rather than obeyed, so a value from a setting that was removed cannot
	// quietly steer anything.
	Settings map[string]string `json:"settings,omitempty"`
}

// updateTool writes what an administrator owns about a tool: whether it is on,
// and who may reach it. Approval is not here for any kind of tool: "which tools
// are worth a pause" is a decision about an assistant, so it lives on the agent
// (agent_confirm_tools), and the floor under a dangerous built-in is the code's
// and cannot be lowered from an API. A projected tool used to carry a third
// answer here, which migration 57 removed.
func (h *configHandlers) updateTool(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body toolUpdateBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Status != model.StatusActive && body.Status != model.StatusDisabled {
		writeInvalidFields(w, fieldErrors{"status": "must be active or disabled"})
		return
	}

	ctx := r.Context()
	workspaceID := claimsFrom(r).WorkspaceID
	t, err := h.app.Store.Tools().GetByID(ctx, workspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	t.Status = body.Status
	t.Grants = body.Grants
	if t.Grants == nil {
		t.Grants = []int64{}
	}
	// A native tool's own settings, when it declares a form for them and the
	// caller sent any. A custom tool's config is written by its own endpoint,
	// which validates it against the template and seals its secrets.
	if t.Kind != string(tool.KindCustom) && len(body.Settings) > 0 {
		schema, known := h.app.Tools.Lookup(t.Name)
		if !known || len(schema.Settings) == 0 {
			writeInvalidFields(w, fieldErrors{"settings": "this tool has no settings"})
			return
		}
		config, reason := declaredConfig(schema.Settings, body.Settings)
		if reason != "" {
			writeInvalidFields(w, fieldErrors{"settings": reason})
			return
		}
		// And what the form itself cannot say: the terminal's allowlist with
		// nothing in it. Approval equals success, so a tool that would refuse
		// every call is refused here instead of saved and reported as saved.
		if schema.CheckSettings != nil {
			if err := schema.CheckSettings(config); err != nil {
				writeInvalidFields(w, fieldErrors{"settings": err.Error()})
				return
			}
		}
		t.Config = config
	}
	if err := h.app.Store.Tools().Update(ctx, t); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, h.toolToBody(t, h.toolSources(r)))
}

// --- native custom tools -------------------------------------------------------

type templateBody struct {
	Name        string             `json:"name"`
	Title       string             `json:"title"`
	Description string             `json:"description"`
	Variants    []template.Variant `json:"variants"`
	// The inputs the tool takes (identity fixed, description editable) and the
	// guide to prefill, so the form shows the full tool, not just its connection.
	Params       []template.Param `json:"params"`
	DefaultGuide string           `json:"default_guide"`
}

// listTemplates powers the "Add tool" picker: the native templates this build
// offers, each with its variants (a query template's database drivers), its
// input params, and its default guide.
func (h *configHandlers) listTemplates(w http.ResponseWriter, r *http.Request) {
	out := make([]templateBody, 0)
	for _, t := range h.app.Templates.All() {
		out = append(out, templateBody{
			Name: t.Name(), Title: t.Title(), Description: t.Description(), Variants: t.Variants(),
			Params: t.Params(), DefaultGuide: t.DefaultGuide(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// newToolFormBody is the form for a tool that does not exist yet: the driver's
// fields, and the values it starts with. They arrive together because they are
// one answer, and because deciding what an unfilled field starts as is the
// server's business, not the console's.
type newToolFormBody struct {
	Sections []template.Section `json:"sections"`
	Values   map[string]any     `json:"values"`
}

// templateFields returns the form for a template's chosen variant, filled with
// what a new tool starts from, so the console renders the settings inputs
// without knowing what a driver needs or what any of them should default to.
func (h *configHandlers) templateFields(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if _, ok := h.app.Templates.Get(name); !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such tool template")
		return
	}
	sections, values, err := h.app.NewToolForm(name, r.URL.Query().Get("variant"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, newToolFormBody{Sections: sections, Values: values})
}

type createCustomBody struct {
	Template          string            `json:"template"`
	Variant           string            `json:"variant"`
	Alias             string            `json:"alias"`
	DisplayName       string            `json:"display_name"`
	Description       string            `json:"description"`
	Guide             string            `json:"guide"`
	ParamDescriptions map[string]string `json:"param_descriptions"`
	Settings          map[string]any    `json:"settings"`
}

// createCustomTool instantiates a template into a new tools row: validate,
// seal the secrets, store. The new query_<alias> tool is then grantable to an
// agent like any other.
func (h *configHandlers) createCustomTool(w http.ResponseWriter, r *http.Request) {
	var body createCustomBody
	if !decodeJSON(w, r, &body) {
		return
	}
	created, err := h.app.CreateCustomTool(r.Context(), claimsFrom(r).WorkspaceID, body.Template, template.Input{
		Alias: body.Alias, Variant: body.Variant, Settings: body.Settings,
		DisplayName: body.DisplayName, Description: body.Description, Guide: body.Guide,
		ParamDescriptions: body.ParamDescriptions,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, h.toolToBody(created, h.toolSources(r)))
}

type updateCustomBody struct {
	DisplayName       string            `json:"display_name"`
	Description       string            `json:"description"`
	Guide             string            `json:"guide"`
	ParamDescriptions map[string]string `json:"param_descriptions"`
	Settings          map[string]any    `json:"settings"`
}

// updateCustomTool rewrites a custom tool's configuration. The template, alias,
// and driver are fixed on edit, so the body carries only the settings, the
// presentation, and the guide; a secret left blank is kept.
func (h *configHandlers) updateCustomTool(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body updateCustomBody
	if !decodeJSON(w, r, &body) {
		return
	}
	updated, err := h.app.UpdateCustomTool(r.Context(), claimsFrom(r).WorkspaceID, id, template.Input{
		Settings: body.Settings, DisplayName: body.DisplayName, Description: body.Description,
		Guide: body.Guide, ParamDescriptions: body.ParamDescriptions,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeStoreError(w, h.app, err)
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, h.toolToBody(updated, h.toolSources(r)))
}

type testCustomBody struct {
	Template string         `json:"template"`
	Variant  string         `json:"variant"`
	Settings map[string]any `json:"settings"`
	// Action, Token and Values carry a template's own operation. Empty means the
	// one every template has: test this connection. A template that answers with
	// a question sends its token back here alongside the answers, which are
	// opaque to this layer.
	Action string            `json:"action,omitempty"`
	Token  string            `json:"token,omitempty"`
	Values map[string]string `json:"values,omitempty"`
}

// testCustomTool runs a template operation against the settings an admin is
// filling in, so they can confirm the connection works before saving. Testing is
// the default; a template that needs something more asks for it in the result.
func (h *configHandlers) testCustomTool(w http.ResponseWriter, r *http.Request) {
	var body testCustomBody
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := h.app.RunToolAction(r.Context(), body.Template, body.Variant, body.Settings,
		app.ToolAction{Name: body.Action, Token: body.Token, Values: body.Values})
	if err != nil {
		// The message is the connection's or the settings' own, useful to the
		// admin fixing them; it is not an internal one.
		writeJSON(w, http.StatusOK, actionBody(template.ActionResult{Message: err.Error()}))
		return
	}
	writeJSON(w, http.StatusOK, actionBody(result))
}

// actionBody shapes an action's outcome for the console: whether it worked, what
// to show, and what to ask for when it is unfinished. The error field is the
// message under its old name, so a client that only knows about testing still
// reads the reason.
func actionBody(result template.ActionResult) map[string]any {
	out := map[string]any{"ok": result.OK, "message": result.Message}
	if !result.OK && result.Message != "" {
		out["error"] = result.Message
	}
	if result.Prompt != nil {
		out["prompt"] = result.Prompt
	}
	return out
}

type testCustomEditBody struct {
	Settings map[string]any    `json:"settings"`
	Action   string            `json:"action,omitempty"`
	Token    string            `json:"token,omitempty"`
	Values   map[string]string `json:"values,omitempty"`
}

// testCustomToolEdit tests an edit against the stored tool, so a secret left
// blank is carried forward from the sealed value rather than sent empty.
func (h *configHandlers) testCustomToolEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body testCustomEditBody
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := h.app.RunToolActionForEdit(r.Context(), claimsFrom(r).WorkspaceID, id, body.Settings,
		app.ToolAction{Name: body.Action, Token: body.Token, Values: body.Values})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeStoreError(w, h.app, err)
			return
		}
		// A connection or settings error is the far end's own message, for the
		// admin fixing it; it is not an internal failure.
		writeJSON(w, http.StatusOK, actionBody(template.ActionResult{Message: err.Error()}))
		return
	}
	writeJSON(w, http.StatusOK, actionBody(result))
}

// deleteCustomTool removes a native custom tool. The store refuses to delete a
// built-in or an MCP tool this way, so this only ever removes a custom one.
func (h *configHandlers) deleteCustomTool(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := h.app.Store.Tools().Delete(r.Context(), claimsFrom(r).WorkspaceID, id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- agents --------------------------------------------------------------------

type agentBody struct {
	ID           int64  `json:"id"`
	Key          string `json:"key"`
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
	ModelID      *int64 `json:"model_id"`
	Reasoning    bool   `json:"reasoning"`
	// Settings are this agent's chosen values for what its vendor declares, most
	// notably how hard to think. Chosen here rather than on the model because it
	// is a property of the job: one model serves a classifier that should answer
	// fast and a researcher that should not.
	Settings model.Settings `json:"settings,omitempty"`
	Status   string         `json:"status"`
	// DelegationMode pins how the Gateway runs this agent: auto (the Gateway
	// decides), background, or inline. Empty/absent is auto.
	DelegationMode string   `json:"delegation_mode"`
	Tools          []string `json:"tools"`
	// ConfirmTools names the tools this agent must stop and confirm before
	// running. A subset of Tools: gating a tool the agent cannot call is
	// meaningless.
	ConfirmTools []string `json:"confirm_tools"`
	// Brains lists the ids of the knowledge bases this agent may read.
	Brains []int64 `json:"brains"`
	// FileRules is which model reads which uploaded file, in the order they are
	// tried: the first rule whose types match wins, and a rule with no types
	// matches everything. No rules at all means no file can be uploaded, which
	// is what the chat reads to decide whether to offer an attach button.
	// Meaningful on the Gateway; an agent is handed the text.
	FileRules []model.FileRule `json:"file_rules"`
	// AudioModelID turns a recording into words, so somebody can talk instead of
	// typing. Null means the Gateway takes no audio.
	AudioModelID *int64 `json:"audio_model_id"`
	// MemoryBrainID is the one (unlocked) brain the agent keeps as its long-term
	// memory. Null means it keeps none. It need not be among Brains.
	MemoryBrainID *int64    `json:"memory_brain_id"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	// ApprovalTTLSeconds is how long this agent's confirmations stay
	// answerable. Null means it has no opinion and the deployment's window
	// applies.
	ApprovalTTLSeconds *int64 `json:"approval_ttl_seconds"`
	// MaxIterations bounds this agent's tool loop. Null uses the code default.
	MaxIterations *int `json:"max_iterations"`
	// MaxFleetAgents bounds how many agents may be started in ONE batch. Null
	// uses the code default. Meaningful only for the Gateway, which is the only
	// thing that starts a batch.
	MaxFleetAgents *int `json:"max_fleet_agents"`
	// BackgroundTimeoutSeconds bounds one working leg of a background agent.
	// Null uses the code default. Meaningful only for an agent.
	BackgroundTimeoutSeconds *int64 `json:"background_timeout_seconds"`
}

// agentBounds are the parsed, validated run limits an agent body carries.
type agentBounds struct {
	approvalTTL       *time.Duration
	maxIterations     *int
	maxFleetAgents    *int
	backgroundTimeout *time.Duration
}

func agentToBody(a *model.Agent) agentBody {
	tools := a.Tools
	if tools == nil {
		tools = []string{}
	}
	confirm := a.ConfirmTools
	if confirm == nil {
		confirm = []string{}
	}
	brains := a.Brains
	if brains == nil {
		brains = []int64{}
	}
	rules := a.FileRules
	if rules == nil {
		rules = []model.FileRule{}
	}
	body := agentBody{
		ID: a.ID, Key: a.Key, Name: a.Name, Instructions: a.Instructions,
		ModelID: a.ModelID, Reasoning: a.Reasoning, Settings: a.Settings, Status: a.Status,
		DelegationMode: a.DelegationMode,
		Tools:          tools, ConfirmTools: confirm,
		Brains: brains, MemoryBrainID: a.MemoryBrainID,
		FileRules: rules, AudioModelID: a.AudioModelID,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
	if a.ApprovalTTL != nil {
		seconds := int64(a.ApprovalTTL.Seconds())
		body.ApprovalTTLSeconds = &seconds
	}
	body.MaxIterations = a.MaxIterations
	body.MaxFleetAgents = a.MaxFleetAgents
	if a.BackgroundTimeout != nil {
		seconds := int64(a.BackgroundTimeout.Seconds())
		body.BackgroundTimeoutSeconds = &seconds
	}
	return body
}

// validate reads the request into what the handlers save, collecting every
// problem: a nameless agent, a keyless one, and an approval window that would
// be a trap, one that expires before a person can read the card, or one that
// survives long enough to be redeemed against a world that has moved on.
// modelNames reads the workspace's models once, for the agent responses that
// carry the names of the ones they use.
func (h *configHandlers) modelNames(r *http.Request) map[int64]string {
	models, err := h.app.Store.AIModels().List(r.Context(), claimsFrom(r).WorkspaceID)
	if err != nil {
		// A missing name is a name the screen falls back on, not a failed
		// request: the agents themselves are what was asked for.
		h.app.Log.Warn().Err(err).Msg("load model names for agent response")
		return nil
	}
	out := make(map[int64]string, len(models))
	for _, m := range models {
		out[m.ID] = m.ModelKey
	}
	return out
}

func (b agentBody) validate() (agentBounds, fieldErrors) {
	problems := fieldErrors{}
	// The key is not asked of a person: the main agent is "default", and a
	// agent's is derived from its name by the store. So only the name is
	// required here; an empty key on create is normal, not an error.
	if b.Name == "" {
		problems["name"] = "a name is required"
	}
	var bounds agentBounds
	if b.ApprovalTTLSeconds != nil {
		window := time.Duration(*b.ApprovalTTLSeconds) * time.Second
		if model.ValidApprovalTTL(window) {
			bounds.approvalTTL = &window
		} else {
			problems["approval_ttl_seconds"] = fmt.Sprintf("must be between %s and %s",
				spoken(model.MinApprovalTTL), spoken(model.MaxApprovalTTL))
		}
	}
	if b.MaxIterations != nil {
		if model.ValidMaxIterations(*b.MaxIterations) {
			n := *b.MaxIterations
			bounds.maxIterations = &n
		} else {
			problems["max_iterations"] = fmt.Sprintf("must be between %d and %d",
				model.MinIterationLimit, model.MaxIterationLimit)
		}
	}
	if b.MaxFleetAgents != nil {
		if model.ValidMaxFleetAgents(*b.MaxFleetAgents) {
			n := *b.MaxFleetAgents
			bounds.maxFleetAgents = &n
		} else {
			problems["max_fleet_agents"] = fmt.Sprintf("must be between %d and %d",
				model.MinFleetAgents, model.MaxFleetAgents)
		}
	}
	if b.BackgroundTimeoutSeconds != nil {
		window := time.Duration(*b.BackgroundTimeoutSeconds) * time.Second
		if model.ValidBackgroundTimeout(window) {
			bounds.backgroundTimeout = &window
		} else {
			problems["background_timeout_seconds"] = fmt.Sprintf("must be between %s and %s",
				spoken(model.MinBackgroundTimeout), spoken(model.MaxBackgroundTimeout))
		}
	}
	// Confirmation only makes sense for a tool the agent can call: you cannot
	// pause on a tool it was never given.
	allowed := make(map[string]bool, len(b.Tools))
	for _, name := range b.Tools {
		allowed[name] = true
	}
	for _, name := range b.ConfirmTools {
		if !allowed[name] {
			problems["confirm_tools"] = "must be tools this agent can use"
			break
		}
	}
	return bounds, problems
}

// checkMemoryBrain refuses a long-term memory brain that is not this
// workspace's, or is locked. An agent WRITES its memory back, and a locked
// brain is read-only by design, so choosing one would be an approval the write
// can never satisfy.
func (h *configHandlers) checkMemoryBrain(r *http.Request, id *int64) fieldErrors {
	if id == nil {
		return nil
	}
	brain, err := h.app.Store.Brains().Brain(r.Context(), claimsFrom(r).WorkspaceID, *id)
	if err != nil {
		return fieldErrors{"memory_brain_id": "no such brain"}
	}
	if brain.Locked {
		return fieldErrors{"memory_brain_id": "must be a brain that is not locked"}
	}
	return nil
}

// checkFileRules refuses routing that could not do what it says.
//
// Each is caught here rather than at the moment somebody uploads a file and
// gets a failure they cannot act on: a model that is gone or belongs to another
// workspace, and a model that cannot read a file at all (an embedding model
// chosen for PDFs). A catch-all that is not last is refused too, because
// everything after it is unreachable and nobody writing it meant that.
func (h *configHandlers) checkFileRules(r *http.Request, rules []model.FileRule) fieldErrors {
	if len(rules) == 0 {
		return nil
	}
	problems := fieldErrors{}
	for i, rule := range rules {
		field := fmt.Sprintf("file_rules.%d", i)
		if len(rule.Types) == 0 && i != len(rules)-1 {
			problems[field] = "a rule for every file has to be the last one, or the rules below it can never match"
		}
		m, err := h.app.Store.AIModels().GetByID(r.Context(), claimsFrom(r).WorkspaceID, rule.ModelID)
		if err != nil {
			problems[field] = "no such model"
			continue
		}
		if m.Type != model.ModelTypeChat {
			problems[field] = fmt.Sprintf("must be a model that can read a file; %s is a %s model", m.ModelKey, m.Type)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return problems
}

// checkAudioModel refuses a model that cannot transcribe. Reading audio is not
// reading a file: it is a different call, and only a model that offers it can
// answer.
func (h *configHandlers) checkAudioModel(r *http.Request, id *int64) fieldErrors {
	if id == nil {
		return nil
	}
	m, err := h.app.Store.AIModels().GetByID(r.Context(), claimsFrom(r).WorkspaceID, *id)
	if err != nil {
		return fieldErrors{"audio_model_id": "no such model"}
	}
	if m.Type != model.ModelTypeSTT {
		return fieldErrors{"audio_model_id": fmt.Sprintf("must be a model that transcribes; %s is a %s model", m.ModelKey, m.Type)}
	}
	return nil
}

// The Gateway screen: four sections, because the screen is four sections.
//
// It used to be served by the agents COLLECTION, and the client picked it apart:
// find the one whose key is "default", call that the Gateway, call the rest the
// agents, then reach inside the Gateway for its file rules and its audio model
// and present those as two more sections. That is the screen's own structure,
// rebuilt on the far side of the wire from a list that had been flattened to
// send it. What arrives is read (files, audio), the Gateway thinks with it, and
// work goes to the agents; the answer says so.
type gatewayScreenBody struct {
	Files filesSection `json:"files"`
	Audio audioSection `json:"audio"`
	// Gateway is null when the workspace has not set one up yet.
	Gateway *agentRow  `json:"gateway"`
	Agents  []agentRow `json:"agents"`
}

// agentRow is one LINE of the screen and nothing else: a name, what it thinks
// with, how many tools it has, whether it reasons, whether it is on.
//
// Not the whole agent. Sending the entity meant every row carried its
// instructions, its brains, its confirm list, its file rules, its timestamps
// and a map of model names, none of which the screen draws; and the Gateway's
// row carried the file rules and the audio model a SECOND time, because those
// are already their own sections above it. What a form needs, the form asks for
// by id when it opens.
type agentRow struct {
	ID        int64          `json:"id"`
	Key       string         `json:"key"`
	Name      string         `json:"name"`
	ModelName string         `json:"model_name"`
	Tools     int            `json:"tools"`
	Reasoning bool           `json:"reasoning"`
	Settings  model.Settings `json:"settings"`
	Status    string         `json:"status"`
}

func agentToRow(a *model.Agent, names map[int64]string) agentRow {
	row := agentRow{
		ID: a.ID, Key: a.Key, Name: a.Name,
		Tools: len(a.Tools), Reasoning: a.Reasoning, Status: a.Status,
	}
	if a.ModelID != nil {
		row.ModelName = names[*a.ModelID]
	}
	return row
}

// filesSection is what may be attached and what reads it, in the order the
// rules are tried: the first whose types match wins, and a rule with no types
// matches anything.
type filesSection struct {
	Rules []fileRuleView `json:"rules"`
}

// A rule carries BOTH: the name the table prints, and the id the form edits.
//
// Dropping the id was an over-correction. The two forms on this screen change
// exactly these sections, so with the id here they need no request of their
// own; without it, each had to fetch the whole agent to find one number, which
// is how a modal that edits four file rules ended up pulling instructions,
// brains, a confirm list and timestamps it does not show.
type fileRuleView struct {
	Types     []string `json:"types"`
	ModelID   int64    `json:"model_id"`
	ModelName string   `json:"model_name"`
}

// audioSection is the model that turns speech into words. A null id is the
// answer "nothing is chosen", which the screen says in its own words.
type audioSection struct {
	ModelID   *int64 `json:"model_id"`
	ModelName string `json:"model_name"`
}

func (h *configHandlers) gatewayScreen(w http.ResponseWriter, r *http.Request) {
	agents, err := h.app.Store.Agents().List(r.Context(), claimsFrom(r).WorkspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	names := h.modelNames(r)

	out := gatewayScreenBody{
		Files:  filesSection{Rules: []fileRuleView{}},
		Agents: []agentRow{},
	}
	for _, a := range agents {
		if a.Key != model.DefaultAgentKey {
			out.Agents = append(out.Agents, agentToRow(a, names))
			continue
		}
		row := agentToRow(a, names)
		out.Gateway = &row
		// The Gateway's file and audio settings ARE the two sections above it,
		// so they are read out here rather than repeated on its row.
		for _, rule := range a.FileRules {
			out.Files.Rules = append(out.Files.Rules, fileRuleView{
				Types:     rule.Types,
				ModelID:   rule.ModelID,
				ModelName: names[rule.ModelID],
			})
		}
		out.Audio.ModelID = a.AudioModelID
		if a.AudioModelID != nil {
			out.Audio.ModelName = names[*a.AudioModelID]
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// gateway loads the workspace's Gateway, or says there is not one yet.
func (h *configHandlers) gateway(w http.ResponseWriter, r *http.Request) (*model.Agent, bool) {
	a, err := h.app.Store.Agents().GetByKey(r.Context(), claimsFrom(r).WorkspaceID, model.DefaultAgentKey)
	if err != nil {
		writeStoreError(w, h.app, err)
		return nil, false
	}
	return a, true
}

func (h *configHandlers) putGatewayFiles(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rules []model.FileRule `json:"rules"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if problems := h.checkFileRules(r, body.Rules); len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}
	a, ok := h.gateway(w, r)
	if !ok {
		return
	}
	a.FileRules = body.Rules
	if err := h.app.Store.Agents().Update(r.Context(), a); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *configHandlers) putGatewayAudio(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ModelID *int64 `json:"model_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if problems := h.checkAudioModel(r, body.ModelID); len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}
	a, ok := h.gateway(w, r)
	if !ok {
		return
	}
	a.AudioModelID = body.ModelID
	if err := h.app.Store.Agents().Update(r.Context(), a); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *configHandlers) listAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := h.app.Store.Agents().List(r.Context(), claimsFrom(r).WorkspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	out := make([]agentBody, 0, len(agents))
	for _, a := range agents {
		out = append(out, agentToBody(a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *configHandlers) getAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	a, err := h.app.Store.Agents().GetByID(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, agentToBody(a))
}

// --- the forms on the Gateway screen -------------------------------------------
//
// A form asks ONE question, and the answer is that whole form: the values it
// edits, and the choices it offers. Anything else is the screen composing a
// form out of separate answers, which is how its parts come to disagree.
//
// This screen had three dialogs and five requests, and every one of them fired
// when ANY dialog opened: the model catalogue, the vendor catalogue (to turn a
// vendor id into the name printed beside each model), the tool catalogue, the
// brains, and the agent itself. A dialog that changes one audio model was
// waiting on the tools and the brains; a dialog that grants tools was waiting
// on a list of vendors so it could compose a label.

// modelChoice is a model as a form OFFERS it: the id the form sends back, and
// the one string a person reads to pick it.
//
// The label is composed here because it is a value the control displays, and
// composing it on the far side meant fetching every vendor to turn one number
// into one word.
type modelChoice struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

// modelChoices lists the workspace's models of the given types, labelled with
// the vendor they come from, so two models of the same name are told apart.
func (h *configHandlers) modelChoices(r *http.Request, types ...string) ([]modelChoice, error) {
	ws := claimsFrom(r).WorkspaceID
	models, err := h.app.Store.AIModels().List(r.Context(), ws)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	vendors, err := h.app.Store.Vendors().List(r.Context(), ws)
	if err != nil {
		return nil, fmt.Errorf("list vendors: %w", err)
	}
	names := make(map[int64]string, len(vendors))
	for _, v := range vendors {
		names[v.ID] = v.Name
	}
	out := make([]modelChoice, 0, len(models))
	for _, m := range models {
		if !slices.Contains(types, m.Type) {
			continue
		}
		vendor := names[m.VendorID]
		if vendor == "" {
			vendor = "vendor"
		}
		out = append(out, modelChoice{ID: m.ID, Label: vendor + " / " + m.ModelKey})
	}
	return out, nil
}

// filesFormBody is the Files dialog: the rules it edits, and the models that
// can read a file. Nothing else, because it changes nothing else.
type filesFormBody struct {
	Rules  []model.FileRule `json:"rules"`
	Models []modelChoice    `json:"models"`
}

func (h *configHandlers) filesForm(w http.ResponseWriter, r *http.Request) {
	a, ok := h.gateway(w, r)
	if !ok {
		return
	}
	// A file is read by a model that takes one and answers in words, which is a
	// chat model. Offering anything else offers a choice that cannot work.
	models, err := h.modelChoices(r, model.ModelTypeChat)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	rules := a.FileRules
	if rules == nil {
		rules = []model.FileRule{}
	}
	writeJSON(w, http.StatusOK, filesFormBody{Rules: rules, Models: models})
}

// audioFormBody is the Audio dialog: the one model id it changes, and the
// models that transcribe.
type audioFormBody struct {
	ModelID *int64        `json:"model_id"`
	Models  []modelChoice `json:"models"`
}

func (h *configHandlers) audioForm(w http.ResponseWriter, r *http.Request) {
	a, ok := h.gateway(w, r)
	if !ok {
		return
	}
	models, err := h.modelChoices(r, model.ModelTypeSTT)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, audioFormBody{ModelID: a.AudioModelID, Models: models})
}

// toolChoice is a tool as the agent form offers it: what to tick, what to call
// it, and whether confirmation on it is the code's decision rather than a
// choice.
type toolChoice struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	FriendlyName   string `json:"friendly_name"`
	ApprovalLocked bool   `json:"approval_locked"`
	// Source groups the choices, so an agent's tools are picked from the same
	// headings the catalogue shows rather than from one long list.
	Source string `json:"source"`
	// ShortName is the name without the connection's prefix, which the heading
	// above the run of checkboxes has already said.
	ShortName string `json:"short_name"`
}

// brainChoice is a brain as the agent form offers it. Locked means the agent
// may read it and never write it, so it cannot be the agent's own memory.
type brainChoice struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Locked bool   `json:"locked"`
}

// agentFormBody is the agent dialog, whole: the agent being edited (null when
// one is being created), and everything it can be given.
type agentFormBody struct {
	Agent  *agentBody    `json:"agent"`
	Models []modelChoice `json:"models"`
	Tools  []toolChoice  `json:"tools"`
	Brains []brainChoice `json:"brains"`
	// Settings is what this agent may be configured with beyond the fields the
	// form knows by name: today, how hard to think.
	//
	// Not narrowed to the chosen model's vendor, and that is the decision. The
	// choice is the same four words on every vendor that has the idea, by
	// design, and a model that has no such idea does not fail: the adapter is
	// refused once, drops the parameter and asks again (provider/unsupported.go).
	// Narrowing it here would mean the field appearing and disappearing as
	// somebody changes the model, to protect against something that cannot
	// happen.
	Settings []model.SettingSpec `json:"settings"`
}

func (h *configHandlers) agentForm(w http.ResponseWriter, r *http.Request) {
	ws := claimsFrom(r).WorkspaceID
	out := agentFormBody{Settings: provider.AgentSettings()}

	// An id in the path means an existing agent; without one this is the form
	// for an agent that does not exist yet, and there is nothing to prefill.
	if raw := chi.URLParam(r, "id"); raw != "" {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		a, err := h.app.Store.Agents().GetByID(r.Context(), ws, id)
		if err != nil {
			writeStoreError(w, h.app, err)
			return
		}
		body := agentToBody(a)
		out.Agent = &body
	}

	models, err := h.modelChoices(r, model.ModelTypeChat)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	out.Models = models

	tools, err := h.app.Store.Tools().List(r.Context(), ws)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	sources := h.toolSources(r)
	out.Tools = make([]toolChoice, 0, len(tools))
	for _, t := range tools {
		// A hidden tool is real and grantable but not advertised, exactly as on
		// the tools screen. Offering it here would be a catalogue disagreeing
		// with itself depending on which screen asked.
		schema, known := h.app.Tools.Lookup(t.Name)
		if known && schema.Hidden {
			continue
		}
		out.Tools = append(out.Tools, toolChoice{
			ID:             t.ID,
			Name:           t.Name,
			FriendlyName:   t.FriendlyName,
			ApprovalLocked: known && schema.RequiresApproval,
			Source:         toolSource(t, sources),
			ShortName:      toolShortName(t, sources),
		})
	}

	brains, err := h.app.Store.Brains().Brains(r.Context(), ws)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	out.Brains = make([]brainChoice, 0, len(brains))
	for _, b := range brains {
		out.Brains = append(out.Brains, brainChoice{ID: b.ID, Name: b.Name, Locked: b.Locked})
	}

	writeJSON(w, http.StatusOK, out)
}

func (h *configHandlers) createAgent(w http.ResponseWriter, r *http.Request) {
	var body agentBody
	if !decodeJSON(w, r, &body) {
		return
	}
	bounds, problems := body.validate()
	for field, msg := range h.checkFileRules(r, body.FileRules) {
		problems[field] = msg
	}
	for field, msg := range h.checkAudioModel(r, body.AudioModelID) {
		problems[field] = msg
	}
	for field, msg := range h.checkMemoryBrain(r, body.MemoryBrainID) {
		problems[field] = msg
	}
	if len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}

	a := &model.Agent{
		WorkspaceID:       claimsFrom(r).WorkspaceID,
		Key:               body.Key,
		Name:              body.Name,
		Instructions:      body.Instructions,
		ModelID:           body.ModelID,
		Reasoning:         body.Reasoning,
		Settings:          body.Settings,
		ApprovalTTL:       bounds.approvalTTL,
		MaxIterations:     bounds.maxIterations,
		MaxFleetAgents:    bounds.maxFleetAgents,
		BackgroundTimeout: bounds.backgroundTimeout,
		Status:            defaultStatus(body.Status),
		DelegationMode:    normalizeDelegationMode(body.DelegationMode),
		Tools:             body.Tools,
		ConfirmTools:      body.ConfirmTools,
		Brains:            body.Brains,
		FileRules:         body.FileRules,
		AudioModelID:      body.AudioModelID,
		MemoryBrainID:     body.MemoryBrainID,
	}
	if err := h.app.Store.Agents().Create(r.Context(), a); err != nil {
		writeSaveError(w, h.app, err, "key", "another agent already uses this key")
		return
	}
	h.forgetWorkspaceNotes(r, a.WorkspaceID)
	writeJSON(w, http.StatusCreated, agentToBody(a))
}

func (h *configHandlers) updateAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body agentBody
	if !decodeJSON(w, r, &body) {
		return
	}
	bounds, problems := body.validate()
	for field, msg := range h.checkMemoryBrain(r, body.MemoryBrainID) {
		problems[field] = msg
	}
	if len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}

	// The key is internal and derived, so a caller that does not name one is
	// not asking for it to change. Taking the body's empty string literally
	// blanks it, and for the Gateway that is the whole workspace losing the one
	// agent it answers with, silently, on a save that reported success.
	// Read for the same reason, and for the fields this form does not own.
	//
	// Files and audio are written by `PUT /gateway/files` and `PUT /gateway/audio`,
	// each from a dialog that asks one question. This form does not edit them and
	// does not send them, and a full replace turns "not sent" into "cleared": one
	// save of the Gateway removed its microphone and its ability to read a
	// document, from a screen showing neither, with nothing to say it had
	// happened. What a form does not own, it does not get to blank.
	existing, err := h.app.Store.Agents().GetByID(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	key := body.Key
	if key == "" {
		key = existing.Key
	}

	a := &model.Agent{
		ID:                id,
		WorkspaceID:       claimsFrom(r).WorkspaceID,
		Key:               key,
		Name:              body.Name,
		Instructions:      body.Instructions,
		ModelID:           body.ModelID,
		Reasoning:         body.Reasoning,
		Settings:          body.Settings,
		ApprovalTTL:       bounds.approvalTTL,
		MaxIterations:     bounds.maxIterations,
		MaxFleetAgents:    bounds.maxFleetAgents,
		BackgroundTimeout: bounds.backgroundTimeout,
		Status:            defaultStatus(body.Status),
		DelegationMode:    normalizeDelegationMode(body.DelegationMode),
		Tools:             body.Tools,
		ConfirmTools:      body.ConfirmTools,
		Brains:            body.Brains,
		FileRules:         existing.FileRules,
		AudioModelID:      existing.AudioModelID,
		MemoryBrainID:     body.MemoryBrainID,
	}
	if err := h.app.Store.Agents().Update(r.Context(), a); err != nil {
		writeSaveError(w, h.app, err, "key", "another agent already uses this key")
		return
	}
	h.forgetWorkspaceNotes(r, a.WorkspaceID)
	writeJSON(w, http.StatusOK, agentToBody(a))
}

func (h *configHandlers) deleteAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := h.app.Store.Agents().Delete(r.Context(), claimsFrom(r).WorkspaceID, id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	h.forgetWorkspaceNotes(r, claimsFrom(r).WorkspaceID)
	w.WriteHeader(http.StatusNoContent)
}

// forgetWorkspaceNotes clears the workspace's distilled working notes. They are
// the assistant's own memory of what it could do and how, learned under the
// agent setup as it stood. Changing that setup (an agent's tools, the roster of
// agents, the main agent) can leave a note claiming a capability that no
// longer exists, so a reconfiguration wipes them and the assistant re-learns
// under the new setup. Best effort: a note that will not clear must not fail the
// change that prompted it.
func (h *configHandlers) forgetWorkspaceNotes(r *http.Request, workspaceID int64) {
	if err := h.app.Store.Memory().SetWorkspaceMemory(r.Context(), workspaceID, ""); err != nil {
		h.app.Log.Error().Err(err).Int64("workspace_id", workspaceID).
			Msg("clear workspace memory after agent change")
	}
}

// --- workflows -----------------------------------------------------------------

type workflowBody struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedBy int64     `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func workflowToBody(w *model.Workflow) workflowBody {
	return workflowBody{
		ID: w.ID, Name: w.Name, Status: w.Status,
		CreatedBy: w.CreatedBy, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}
}

func (h *configHandlers) listWorkflows(w http.ResponseWriter, r *http.Request) {
	workflows, err := h.app.Store.Workflows().List(r.Context(), claimsFrom(r).WorkspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	out := make([]workflowBody, 0, len(workflows))
	for _, wf := range workflows {
		out = append(out, workflowToBody(wf))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *configHandlers) getWorkflow(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	wf, err := h.app.Store.Workflows().GetByID(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, workflowToBody(wf))
}

func (h *configHandlers) createWorkflow(w http.ResponseWriter, r *http.Request) {
	var body workflowBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name == "" {
		writeInvalidFields(w, fieldErrors{"name": "a name is required"})
		return
	}
	claims := claimsFrom(r)

	// A new workflow is a draft, whatever the request says. Publishing is a
	// separate permission, and creating must not be a way around it.
	wf := &model.Workflow{
		WorkspaceID: claims.WorkspaceID,
		Name:        body.Name,
		Status:      model.WorkflowDraft,
		CreatedBy:   claims.UserID,
	}
	if err := h.app.Store.Workflows().Create(r.Context(), wf); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusCreated, workflowToBody(wf))
}

func (h *configHandlers) updateWorkflow(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body workflowBody
	if !decodeJSON(w, r, &body) {
		return
	}
	problems := fieldErrors{}
	if body.Name == "" {
		problems["name"] = "a name is required"
	}
	switch body.Status {
	case model.WorkflowDraft, model.WorkflowArchived:
	case model.WorkflowPublished:
		// Publishing happens by publishing a version, which is a different
		// permission. Flipping the status here would let an editor turn on a
		// workflow without anyone approving what is in it.
		problems["status"] = "a workflow goes live by publishing a version"
	default:
		problems["status"] = "unknown status"
	}
	if len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}

	wf := &model.Workflow{
		ID:          id,
		WorkspaceID: claimsFrom(r).WorkspaceID,
		Name:        body.Name,
		Status:      body.Status,
	}
	if err := h.app.Store.Workflows().Update(r.Context(), wf); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, workflowToBody(wf))
}

func (h *configHandlers) deleteWorkflow(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := h.app.Store.Workflows().Delete(r.Context(), claimsFrom(r).WorkspaceID, id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type versionBody struct {
	ID          int64           `json:"id"`
	Version     int             `json:"version"`
	Definition  json.RawMessage `json:"definition"`
	IsPublished bool            `json:"is_published"`
	CreatedBy   int64           `json:"created_by"`
	CreatedAt   time.Time       `json:"created_at"`
}

func versionToBody(v *model.WorkflowVersion) versionBody {
	return versionBody{
		ID: v.ID, Version: v.Version, Definition: v.Definition,
		IsPublished: v.IsPublished, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt,
	}
}

func (h *configHandlers) listVersions(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	versions, err := h.app.Store.Workflows().ListVersions(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	out := make([]versionBody, 0, len(versions))
	for _, v := range versions {
		out = append(out, versionToBody(v))
	}
	writeJSON(w, http.StatusOK, out)
}

// createVersion validates the definition before storing it. A definition that
// only fails at run time fails in front of a user, mid-conversation, which is
// the worst possible place to discover a typo.
func (h *configHandlers) createVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body versionBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := app.ValidateDefinition(body.Definition); err != nil {
		writeInvalidFields(w, fieldErrors{"definition": err.Error()})
		return
	}
	claims := claimsFrom(r)

	v := &model.WorkflowVersion{
		WorkflowID: id,
		Definition: body.Definition,
		CreatedBy:  claims.UserID,
	}
	if err := h.app.Store.Workflows().CreateVersion(r.Context(), claims.WorkspaceID, v); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusCreated, versionToBody(v))
}

type publishBody struct {
	VersionID int64 `json:"version_id"`
}

func (h *configHandlers) publishVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body publishBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.VersionID == 0 {
		writeInvalidFields(w, fieldErrors{"version_id": "a version to publish is required"})
		return
	}
	if err := h.app.Store.Workflows().Publish(r.Context(), claimsFrom(r).WorkspaceID, id, body.VersionID); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type assignmentBody struct {
	MatchType  string `json:"match_type"`
	MatchValue string `json:"match_value"`
	Priority   int    `json:"priority"`
}

func (h *configHandlers) listAssignments(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	assignments, err := h.app.Store.Workflows().ListAssignments(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	out := make([]assignmentBody, 0, len(assignments))
	for _, a := range assignments {
		out = append(out, assignmentBody{MatchType: a.MatchType, MatchValue: a.MatchValue, Priority: a.Priority})
	}
	writeJSON(w, http.StatusOK, out)
}

// setAssignments replaces the conditions wholesale, so the workspace never
// passes through a half-applied rule set that nobody asked for.
func (h *configHandlers) setAssignments(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body []assignmentBody
	if !decodeJSON(w, r, &body) {
		return
	}

	assignments := make([]model.WorkflowAssignment, 0, len(body))
	for _, a := range body {
		switch a.MatchType {
		case model.MatchUser, model.MatchGroup, model.MatchRole:
			if _, err := strconv.ParseInt(a.MatchValue, 10, 64); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request",
					"an identity condition must name an id")
				return
			}
		case model.MatchChannel:
			if !model.IsChannel(a.MatchValue) {
				writeError(w, http.StatusBadRequest, "invalid_request", "unknown channel")
				return
			}
		default:
			writeError(w, http.StatusBadRequest, "invalid_request", "unknown match type")
			return
		}
		assignments = append(assignments, model.WorkflowAssignment{
			MatchType: a.MatchType, MatchValue: a.MatchValue, Priority: a.Priority,
		})
	}

	if err := h.app.Store.Workflows().SetAssignments(r.Context(), claimsFrom(r).WorkspaceID, id, assignments); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers -------------------------------------------------------------------

// spoken formats a duration the way a person says it: "7 days", not "168h0m0s".
// It is for messages about the approval window, so whole days, whole hours, or
// minutes are the only shapes it meets.
func spoken(d time.Duration) string {
	unit, name := time.Minute, "minute"
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		unit, name = 24*time.Hour, "day"
	case d >= time.Hour && d%time.Hour == 0:
		unit, name = time.Hour, "hour"
	}
	n := int(d / unit)
	if n == 1 {
		return "1 " + name
	}
	return fmt.Sprintf("%d %ss", n, name)
}

func defaultStatus(status string) string {
	if status == model.StatusDisabled {
		return model.StatusDisabled
	}
	return model.StatusActive
}

// normalizeDelegationMode keeps only a known pinned mode; anything else (empty,
// or a value the UI should never send) is auto, so the Gateway decides.
func normalizeDelegationMode(mode string) string {
	switch mode {
	case model.DelegationModeBackground, model.DelegationModeInline, model.DelegationModeFleet:
		return mode
	default:
		return model.DelegationModeAuto
	}
}

// declaredValues is what a native tool's form opens filled with, which is the
// same question the runtime asks and so is answered in the same place
// (tool.Settings): what is stored, and the declared default for what is not.
//
// It was answered here, separately, and the two answers disagreed. See the
// comment on tool.Settings.
func declaredValues(sections []tool.Section, stored json.RawMessage) map[string]any {
	// A form ignores the read error on purpose: it shows the declared defaults,
	// which is the best thing to put in front of somebody who has come to fix
	// exactly that. The gate that decides what may RUN does the opposite.
	values, _ := tool.Settings(sections, stored)
	if len(values) == 0 {
		return nil
	}
	return values
}

// declaredConfig turns what a form sent into what a tool stores, keeping only
// the keys the tool declares and refusing a choice that is not one of them.
func declaredConfig(sections []tool.Section, sent map[string]string) (json.RawMessage, string) {
	flat := map[string]any{}
	for _, section := range sections {
		for _, field := range section.Fields {
			value, given := sent[field.Key]
			if !given {
				continue
			}
			if len(field.Options) > 0 {
				allowed := false
				for _, option := range field.Options {
					if option.Value == value {
						allowed = true
						break
					}
				}
				if !allowed {
					return nil, fmt.Sprintf("%s is not one of the choices for %s", value, field.Label)
				}
			}
			flat[field.Key] = value
		}
	}
	raw, err := json.Marshal(tool.Nest(flat))
	if err != nil {
		return nil, "these settings could not be stored"
	}
	return raw, ""
}
