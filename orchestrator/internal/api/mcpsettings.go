package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// The configuration of OUR MCP server: which tools it exposes and which
// brains an external agent may reach. The exposure list is a kill-switch,
// never a grant: who may call an exposed tool is still the grants' decision,
// re-checked per call.

type mcpSettingsHandlers struct{ app *app.App }

func mountMCPSettings(r chi.Router, a *app.App) {
	h := &mcpSettingsHandlers{app: a}
	r.With(requirePermission(a, model.PermMCPServerView)).Get("/mcp-server", h.get)
	r.With(requirePermission(a, model.PermMCPServerEdit)).Put("/mcp-server", h.put)
}

type mcpSettingsBody struct {
	// Configured says whether an admin has ever saved. Unconfigured means
	// the whole catalog is exposed (CRM parity), and the screen says so.
	Configured  bool            `json:"configured"`
	ToolConfig  json.RawMessage `json:"tool_config"`
	BrainConfig json.RawMessage `json:"brain_config"`
}

// mcpScreenBody is the MCP server screen in one answer: what may be exposed,
// and whether each thing IS.
//
// It was three requests (the tool catalogue, the brains, the saved settings)
// and a client-side join: the screen fetched two lists, waited for the config,
// then walked the catalogue deciding each checkbox from `configured ? saved :
// true`. That rule is the server's, and it is answered here, so a checkbox
// arrives already knowing whether it is ticked.
type mcpScreenBody struct {
	// Configured says whether an admin has ever saved. Unconfigured means the
	// whole catalog is exposed (CRM parity), and the screen says so.
	Configured bool          `json:"configured"`
	Tools      []mcpToolRow  `json:"tools"`
	Brains     []mcpBrainRow `json:"brains"`
}

type mcpToolRow struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	FriendlyName string `json:"friendly_name"`
	// Kind separates a tool of ours from one this workspace consumes from
	// another service and relays.
	Kind string `json:"kind"`
	// Source and ShortName present a relayed tool the way the catalogue does:
	// grouped under the service it came from, named without the prefix that
	// grouping stands in for (KB/20). Two services can each offer a `query`,
	// so the heading is what keeps the short name unambiguous.
	Source    string `json:"source"`
	ShortName string `json:"short_name"`
	// Exposed is the answer, not the ingredients: unconfigured means every
	// active tool is exposed, and after a save it is what was saved.
	Exposed bool `json:"exposed"`
}

type mcpBrainRow struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Chosen bool   `json:"chosen"`
}

func (h *mcpSettingsHandlers) get(w http.ResponseWriter, r *http.Request) {
	workspaceID := claimsFrom(r).WorkspaceID

	configured := true
	var toolConfig map[string]model.MCPToolSwitch
	var chosenBrains []int64
	settings, err := h.app.Store.MCPServers().GetSettings(r.Context(), workspaceID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		configured = false
	case err != nil:
		writeStoreError(w, h.app, err)
		return
	default:
		_ = json.Unmarshal(settings.ToolConfig, &toolConfig)
		_ = json.Unmarshal(settings.BrainConfig, &chosenBrains)
	}

	tools, err := h.app.Store.Tools().List(r.Context(), workspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	brains, err := h.app.Store.Brains().Brains(r.Context(), workspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	out := mcpScreenBody{
		Configured: configured,
		Tools:      make([]mcpToolRow, 0, len(tools)),
		Brains:     make([]mcpBrainRow, 0, len(brains)),
	}
	sources := mcpToolSources(r.Context(), h.app, workspaceID)
	for _, t := range tools {
		out.Tools = append(out.Tools, mcpToolRow{
			ID: t.ID, Name: t.Name, FriendlyName: t.FriendlyName, Kind: t.Kind,
			Source: toolSource(t, sources), ShortName: toolShortName(t, sources),
			Exposed: !configured || toolConfig[t.Name].Enabled,
		})
	}
	for _, b := range brains {
		out.Brains = append(out.Brains, mcpBrainRow{
			ID: b.ID, Name: b.Name, Chosen: slices.Contains(chosenBrains, b.ID),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *mcpSettingsHandlers) put(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ToolConfig  json.RawMessage `json:"tool_config"`
		BrainConfig json.RawMessage `json:"brain_config"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	problems := fieldErrors{}
	var tools map[string]model.MCPToolSwitch
	if len(req.ToolConfig) == 0 || json.Unmarshal(req.ToolConfig, &tools) != nil {
		problems["tool_config"] = "must be a map of tool names to {enabled}"
	}
	var brains []int64
	if len(req.BrainConfig) == 0 || json.Unmarshal(req.BrainConfig, &brains) != nil {
		problems["brain_config"] = "must be a list of brain ids"
	}
	if len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}

	settings := &model.MCPSettings{
		WorkspaceID: claimsFrom(r).WorkspaceID,
		ToolConfig:  req.ToolConfig,
		BrainConfig: req.BrainConfig,
	}
	if err := h.app.Store.MCPServers().PutSettings(r.Context(), settings); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, mcpSettingsBody{
		Configured:  true,
		ToolConfig:  settings.ToolConfig,
		BrainConfig: settings.BrainConfig,
	})
}
