package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/oauth"
	"flexie.io/sag/internal/tool"
)

// Our MCP server: the inverse of the client in mcp.go. An external agent
// holds one of OUR OAuth tokens and calls OUR tools. The CRM's four-layer
// gate is kept whole: authorize and token issuance already re-check the
// person; this file is the resource side, re-run on every request, plus the
// call-time authorization that runs per tool call.

// MCPCaller is who an inbound bearer resolved to.
type MCPCaller struct {
	UserID      int64
	WorkspaceID int64
}

// ErrMCPUnauthorized is the one answer for every failed check on the
// resource side. Which check failed is logged, never disclosed: an invalid,
// expired, revoked, and under-privileged token must be indistinguishable
// from outside.
var ErrMCPUnauthorized = fmt.Errorf("the token is invalid, expired, or lacks the required access")

// MCPIdentity resolves an inbound bearer, re-checking everything live:
// token usable, client active, scope and audience, the person real, active,
// still a member, and still holding the permission. Token possession is
// never sufficient by itself.
func (a *App) MCPIdentity(ctx context.Context, bearer string) (*MCPCaller, error) {
	if bearer == "" {
		return nil, ErrMCPUnauthorized
	}
	token, err := a.Store.OAuth().GetAccessTokenByHash(ctx, oauth.HashToken(bearer))
	if err != nil || token.Revoked || time.Now().UTC().After(token.ExpiresAt) {
		return nil, ErrMCPUnauthorized
	}
	client, err := a.Store.OAuth().GetClientForWorkspace(ctx, token.WorkspaceID, token.ClientPK)
	if err != nil || client.Status != model.StatusActive {
		return nil, ErrMCPUnauthorized
	}
	if !hasScope(token.Scopes, model.ScopeMCP) || token.Audience != model.ScopeMCP {
		return nil, ErrMCPUnauthorized
	}
	user, err := a.Store.Users().GetByID(ctx, token.UserID)
	if err != nil || user.Status != model.StatusActive {
		return nil, ErrMCPUnauthorized
	}
	member, err := a.Store.Workspaces().IsMember(ctx, token.WorkspaceID, user.ID)
	if err != nil || !member {
		return nil, ErrMCPUnauthorized
	}
	allowed, err := a.Authorize(ctx, user.ID, model.PermMCPConnect)
	if err != nil || !allowed {
		return nil, ErrMCPUnauthorized
	}
	return &MCPCaller{UserID: user.ID, WorkspaceID: token.WorkspaceID}, nil
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// MCPExposedNames resolves the exposure kill-switch: the tool names our MCP
// server offers at all, before anyone's grants are consulted. Unconfigured
// means the whole catalog (CRM parity); a saved config means exactly the
// enabled subset.
func (a *App) MCPExposedNames(ctx context.Context, workspaceID int64) (map[string]bool, error) {
	rows, err := a.Store.Tools().List(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	catalog := make(map[string]bool, len(rows))
	for _, t := range rows {
		if t.Status != model.StatusActive || t.RemoteMissing {
			continue
		}
		catalog[t.Name] = true
	}

	settings, err := a.Store.MCPServers().GetSettings(ctx, workspaceID)
	if err != nil {
		// Unconfigured: the whole catalog, exactly the CRM's default.
		return catalog, nil
	}
	var config map[string]model.MCPToolSwitch
	if err := json.Unmarshal(settings.ToolConfig, &config); err != nil {
		return nil, fmt.Errorf("read mcp tool config: %w", err)
	}
	exposed := make(map[string]bool, len(config))
	for name, sw := range config {
		if sw.Enabled && catalog[name] {
			exposed[name] = true
		}
	}
	return exposed, nil
}

// MCPLoadout is what one caller sees over MCP right now: the exposure
// kill-switch intersected with their own grants, resolved fresh. Listing
// and calling both go through here, so a revoked grant or a flipped switch
// takes effect on the next call, not the next session.
func (a *App) MCPLoadout(ctx context.Context, caller *MCPCaller) (tool.Loadout, error) {
	exposed, err := a.MCPExposedNames(ctx, caller.WorkspaceID)
	if err != nil {
		return tool.Loadout{}, err
	}
	names := make([]string, 0, len(exposed))
	for name := range exposed {
		names = append(names, name)
	}
	// No confirm set: confirmations do not exist over MCP (the exposure list is
	// the control), and a code-locked tool is refused outright in MCPCallTool.
	// No brains either: an external caller is not an agent with an assignment, so
	// the brain tools reach nothing here (their empty allow-list default).
	// An external agent over MCP is one agent per request: MCP calls are
	// self-contained, so there is nothing for it to come back to.
	loadout, err := a.Loadout(ctx, caller.WorkspaceID, caller.UserID, "", names, nil, nil, tool.OwnerOfAgent())
	if err != nil {
		return tool.Loadout{}, err
	}
	// Internal tools ride along for the agent loop's sake; they are not part
	// of the surface an external agent is offered.
	kept := loadout.Schemas[:0]
	for _, schema := range loadout.Schemas {
		if schema.Kind == tool.KindInternal {
			delete(loadout.Handlers, schema.Name)
			continue
		}
		kept = append(kept, schema)
	}
	loadout.Schemas = kept
	return loadout, nil
}

// MCPCallTool authorizes and runs one tool call for an inbound caller. The
// authorization is resolved NOW, not at session start. Confirmations do not
// exist over MCP (CRM parity: the exposure list is the control), with one
// deliberate tightening: a tool whose CODE demands approval never runs
// here, because that lock is not the administrator's to lift.
func (a *App) MCPCallTool(ctx context.Context, caller *MCPCaller, name string, args json.RawMessage) (json.RawMessage, bool, error) {
	loadout, err := a.MCPLoadout(ctx, caller)
	if err != nil {
		return nil, false, err
	}
	schema, known := loadout.Schema(name)
	handler, hasHandler := loadout.Handlers[name]
	if !known || !hasHandler {
		return nil, false, fmt.Errorf("tool not enabled")
	}
	if registered, ok := a.Tools.Load([]string{name}).Schema(name); ok && registered.RequiresApproval {
		payload, _ := json.Marshal(map[string]any{
			"success": false,
			"error":   "this action requires a person's approval and cannot run over this surface",
		})
		return payload, true, nil
	}
	_ = schema

	result, err := handler(ctx, tool.Call{
		WorkspaceID: caller.WorkspaceID,
		UserID:      caller.UserID,
		Name:        name,
		Args:        args,
		NoConfirm:   true,
	})
	if err != nil {
		// The raw cause is logged, never handed to an external agent.
		a.Log.Error().Err(err).Str("tool", name).Int64("user_id", caller.UserID).Msg("mcp tool failed")
		payload, _ := json.Marshal(map[string]any{"success": false, "error": "the tool failed"})
		return payload, true, nil
	}
	return result.Content, false, nil
}
