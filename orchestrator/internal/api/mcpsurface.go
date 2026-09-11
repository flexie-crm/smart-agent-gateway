package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"flexie.io/sag/internal/app"
)

// Our MCP server: the surface an external agent connects to with one of OUR
// OAuth tokens. Transport and sessions come from the protocol SDK; identity,
// exposure, and authorization are ours, re-checked on every request and
// again on every call (KB/05: token possession is never sufficient).

type mcpSurface struct{ app *app.App }

func mountMCPSurface(r chi.Router, a *app.App) {
	s := &mcpSurface{app: a}

	// Discovery is public: it is HOW a client finds out where to
	// authenticate in the first place.
	r.Get("/mcp/.well-known/mcp", s.discovery)

	handler := sdkmcp.NewStreamableHTTPHandler(func(req *http.Request) *sdkmcp.Server {
		caller, ok := callerFrom(req.Context())
		if !ok {
			return nil // the middleware refused already; belt and braces
		}
		return s.serverFor(caller)
	}, nil)
	authed := s.requireBearer(handler)
	r.Handle("/mcp", authed)
}

type callerKey struct{}

func callerFrom(ctx context.Context) (*app.MCPCaller, bool) {
	caller, ok := ctx.Value(callerKey{}).(*app.MCPCaller)
	return caller, ok
}

// requireBearer is the resource-side gate, run on EVERY request of a
// session: a token revoked mid-session stops working on the next message,
// not the next session. A refusal never says which check failed, and the
// 401 carries the RFC 9728 pointer to our resource metadata.
func (s *mcpSurface) requireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := ""
		if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
			bearer = strings.TrimSpace(h[7:])
		}
		caller, err := s.app.MCPIdentity(r.Context(), bearer)
		if err != nil {
			w.Header().Set("WWW-Authenticate",
				`Bearer resource_metadata="`+strings.TrimSuffix(s.app.Config.BaseURL, "/")+
					`/.well-known/oauth-protected-resource"`)
			writeError(w, http.StatusUnauthorized, "invalid_token",
				"the token is invalid, expired, or lacks the required access")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, caller)))
	})
}

// serverFor builds the protocol server one session speaks to. The tool list
// is the caller's view at session start; AUTHORIZATION is not: every call
// re-resolves exposure and grants inside MCPCallTool, so the snapshot can
// only ever show a stale name, never run a revoked one.
func (s *mcpSurface) serverFor(caller *app.MCPCaller) *sdkmcp.Server {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{
		Name:    "sag",
		Title:   "SAG",
		Version: "1.0",
	}, nil)

	loadout, err := s.app.MCPLoadout(context.Background(), caller)
	if err != nil {
		s.app.Log.Error().Err(err).Int64("user_id", caller.UserID).Msg("mcp loadout")
		return server
	}
	for _, schema := range loadout.Schemas {
		server.AddTool(&sdkmcp.Tool{
			Name:        schema.Name,
			Title:       schema.FriendlyName,
			Description: schema.Description,
			InputSchema: schema.InputSchema,
		}, s.callHandler(caller, schema.Name))
	}
	return server
}

// callHandler runs one call AS THE REQUEST'S caller when the transport
// carries it (each POST is separately authenticated), falling back to the
// session's own: a stolen session id with someone else's valid token must
// act as the token's owner, never the session's.
func (s *mcpSurface) callHandler(sessionCaller *app.MCPCaller, name string) sdkmcp.ToolHandler {
	return func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		caller, ok := callerFrom(ctx)
		if !ok {
			caller = sessionCaller
		}
		var args json.RawMessage
		if req.Params != nil && req.Params.Arguments != nil {
			raw, err := json.Marshal(req.Params.Arguments)
			if err != nil {
				return refusalResult("the arguments could not be read"), nil
			}
			args = raw
		}
		content, isError, err := s.app.MCPCallTool(ctx, caller, name, args)
		if err != nil {
			// "tool not enabled" and friends: a curated message, never a raw
			// internal error.
			return refusalResult(err.Error()), nil
		}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(content)}},
			IsError: isError,
		}, nil
	}
}

func refusalResult(message string) *sdkmcp.CallToolResult {
	payload, _ := json.Marshal(map[string]any{"success": false, "error": message})
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(payload)}},
		IsError: true,
	}
}

// discovery answers what this surface is and how to get in (CRM parity).
func (s *mcpSurface) discovery(w http.ResponseWriter, _ *http.Request) {
	base := strings.TrimSuffix(s.app.Config.BaseURL, "/")
	writeJSON(w, http.StatusOK, map[string]any{
		"name":      "sag",
		"version":   "1.0",
		"transport": "streamable-http",
		"endpoint":  "/mcp",
		"authentication": map[string]any{
			"scheme":                "bearer",
			"header":                "Authorization",
			"format":                "Bearer <oauth-access-token>",
			"flow":                  "oauth2",
			"authorization_servers": []string{base},
			"resource_metadata":     base + "/.well-known/oauth-protected-resource",
			"required_scope":        "mcp",
		},
		"capabilities": []string{"tools"},
	})
}
