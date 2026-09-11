package api

import (
	"errors"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/mcpclient"
	"flexie.io/sag/internal/model"
)

// The admin surface of the MCP client: the connections this workspace
// consumes. The projected tools themselves are governed on /v1/tools like
// every other tool; here lives only the connection: where it is, how it
// authenticates, and when it last agreed with the remote.

type mcpHandlers struct{ app *app.App }

func mountMCPServers(r chi.Router, a *app.App) {
	h := &mcpHandlers{app: a}
	r.Route("/mcp-servers", func(r chi.Router) {
		r.With(requirePermission(a, model.PermMCPServersView)).Get("/", h.list)
		r.With(requirePermission(a, model.PermMCPServersCreate)).Post("/", h.create)
		r.With(requirePermission(a, model.PermMCPServersEdit)).Put("/{id}", h.update)
		r.With(requirePermission(a, model.PermMCPServersDelete)).Delete("/{id}", h.delete)

		r.With(requirePermission(a, model.PermMCPServersEdit)).Post("/{id}/sync", h.sync)
		r.With(requirePermission(a, model.PermMCPServersEdit)).Post("/{id}/connect", h.connect)
		r.With(requirePermission(a, model.PermMCPServersView)).Get("/{id}/tools", h.tools)
	})
}

// mountMCPCallback registers the OAuth return address. It is UNAUTHENTICATED
// on purpose: a browser mid-redirect carries no bearer token. The sealed
// state parameter is the whole authentication, and it was minted by us.
func mountMCPCallback(r chi.Router, a *app.App) {
	h := &mcpHandlers{app: a}
	r.Get("/connect/mcp/callback", h.callback)
	// Who we are, for a service that takes a URL as a client id and reads the
	// rest from it. Unauthenticated by necessity: the reader is somebody else's
	// authorization server, and it says nothing a person could not learn by
	// starting a connection. There is no secret in this flow to leak.
	r.Get("/connect/mcp/client-metadata.json", h.clientMetadata)
}

// clientMetadata publishes this deployment's description as an OAuth client.
func (h *mcpHandlers) clientMetadata(w http.ResponseWriter, _ *http.Request) {
	if h.app.Config.Local() {
		// A local installation borrows a published document rather than serving
		// one, so there is nothing here to fetch and nothing outside could have
		// reached it anyway.
		writeError(w, http.StatusNotFound, "not_available",
			"this installation has no address a remote service could reach")
		return
	}
	url := mcpclient.ClientMetadataURL(h.app.Config.BaseURL, false, h.app.Config.MCPClientMetadataURL)
	writeJSON(w, http.StatusOK, mcpclient.NewClientMetadata(url, h.app.MCPRedirectURI(), "mcp"))
}

// mcpServerBody deliberately has no secret fields. Keys and tokens are
// sealed on arrival and never read back out; the API reports only whether
// they exist.
type mcpServerBody struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	AuthType  string `json:"auth_type"`
	HasAPIKey bool   `json:"has_api_key"`
	// OAuthClientID is shown back because it is not a secret, and an
	// administrator needs to see which client a connection is using.
	OAuthClientID string `json:"oauth_client_id,omitempty"`
	// HasOAuthClientSecret reports only that one is held.
	HasOAuthClientSecret bool       `json:"has_oauth_client_secret"`
	Connected            bool       `json:"connected"`
	Status               string     `json:"status"`
	ToolPrefix           string     `json:"tool_prefix"`
	LastSynced           *time.Time `json:"last_synced_at"`
	LastError            string     `json:"last_error,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

func newMCPServerBody(m *model.MCPServer) *mcpServerBody {
	return &mcpServerBody{
		ID: m.ID, Name: m.Name, URL: m.URL, AuthType: m.AuthType,
		HasAPIKey:            m.HasAPIKey(),
		OAuthClientID:        m.OAuthClientID,
		HasOAuthClientSecret: len(m.OAuthClientSecret) > 0,
		Connected:            m.Connected(),
		Status:               m.Status, ToolPrefix: m.ToolPrefix,
		LastSynced: m.LastSyncedAt, LastError: m.LastError,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

type mcpServerRequest struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	AuthType string `json:"auth_type"`
	// APIKey is write-only: sealed on arrival, never returned. Empty on
	// update means "leave the stored key alone".
	APIKey string `json:"api_key"`
	// OAuthClientID and OAuthClientSecret register SAG with a service that does
	// NOT issue clients itself. Most do (RFC 7591) and these stay empty; the
	// ones that do not expect an administrator to create the application on
	// their side and paste the credentials here.
	//
	// The id is not a secret, so it round-trips and can be cleared. The secret
	// follows the API key's rule: write-only, and empty on update leaves the
	// stored one alone.
	OAuthClientID     string `json:"oauth_client_id"`
	OAuthClientSecret string `json:"oauth_client_secret"`
	Status            string `json:"status"`
}

func (h *mcpHandlers) list(w http.ResponseWriter, r *http.Request) {
	servers, err := h.app.Store.MCPServers().List(r.Context(), claimsFrom(r).WorkspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, mapSlice(servers, newMCPServerBody))
}

func (h *mcpHandlers) create(w http.ResponseWriter, r *http.Request) {
	var req mcpServerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	m := &model.MCPServer{
		WorkspaceID:   claimsFrom(r).WorkspaceID,
		Name:          strings.TrimSpace(req.Name),
		URL:           strings.TrimSpace(req.URL),
		AuthType:      req.AuthType,
		OAuthClientID: strings.TrimSpace(req.OAuthClientID),
		Status:        model.StatusActive,
		ToolPrefix:    model.Slugify(req.Name),
	}
	if len(m.ToolPrefix) > 64 {
		m.ToolPrefix = strings.Trim(m.ToolPrefix[:64], "-")
	}
	if problems := mcpServerProblems(m); len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}
	if req.APIKey != "" {
		sealed, err := h.app.SealCredentials(req.APIKey)
		if err != nil {
			writeStoreError(w, h.app, err)
			return
		}
		m.APIKey = sealed
	}
	if req.OAuthClientSecret != "" {
		sealed, err := h.app.SealCredentials(req.OAuthClientSecret)
		if err != nil {
			writeStoreError(w, h.app, err)
			return
		}
		m.OAuthClientSecret = sealed
	}
	if err := h.app.Store.MCPServers().Create(r.Context(), m); err != nil {
		writeSaveError(w, h.app, err, "name", "another connection already uses this name")
		return
	}
	writeJSON(w, http.StatusCreated, newMCPServerBody(m))
}

func (h *mcpHandlers) update(w http.ResponseWriter, r *http.Request) {
	m, ok := h.load(w, r)
	if !ok {
		return
	}
	var req mcpServerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	problems := fieldErrors{}
	if !validStatus(req.Status) {
		problems["status"] = "must be active or disabled"
	}
	m.Name = strings.TrimSpace(req.Name)
	m.URL = strings.TrimSpace(req.URL)
	m.AuthType = req.AuthType
	m.OAuthClientID = strings.TrimSpace(req.OAuthClientID)
	m.Status = req.Status
	problems.merge(mcpServerProblems(m))
	if len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}

	// nil means "leave the stored secret untouched": editing a name must
	// never wipe a key.
	m.APIKey = nil
	if req.APIKey != "" {
		sealed, err := h.app.SealCredentials(req.APIKey)
		if err != nil {
			writeStoreError(w, h.app, err)
			return
		}
		m.APIKey = sealed
	}
	// Same rule for the client secret: nil leaves the stored one alone.
	m.OAuthClientSecret = nil
	if req.OAuthClientSecret != "" {
		sealed, err := h.app.SealCredentials(req.OAuthClientSecret)
		if err != nil {
			writeStoreError(w, h.app, err)
			return
		}
		m.OAuthClientSecret = sealed
	}
	if err := h.app.Store.MCPServers().Update(r.Context(), m); err != nil {
		writeSaveError(w, h.app, err, "name", "another connection already uses this name")
		return
	}
	fresh, err := h.app.Store.MCPServers().GetByID(r.Context(), m.WorkspaceID, m.ID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, newMCPServerBody(fresh))
}

func (h *mcpHandlers) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := h.app.Store.MCPServers().Delete(r.Context(), claimsFrom(r).WorkspaceID, id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sync asks the remote what it offers, right now, and answers with the diff.
func (h *mcpHandlers) sync(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	result, err := h.app.SyncMCPServer(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		h.app.Log.Error().Err(err).Int64("mcp_server_id", id).Msg("mcp sync")
		writeError(w, http.StatusBadGateway, "sync_failed", "the service did not answer; the connection records the reason")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// tools lists one connection's projection, the missing and changed included:
// this is where the drift an admin has to look at becomes visible.
func (h *mcpHandlers) tools(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	tools, err := h.app.Store.Tools().ListByMCPServer(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, tools)
}

type connectResponse struct {
	// AuthorizeURL is where the person's browser goes to consent.
	AuthorizeURL string `json:"authorize_url"`
}

// connect begins the OAuth dance and hands back the address to visit.
func (h *mcpHandlers) connect(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	claims := claimsFrom(r)
	authorizeURL, err := h.app.BeginMCPConnect(r.Context(), claims.WorkspaceID, claims.UserID, id)
	if err != nil {
		h.app.Log.Error().Err(err).Int64("mcp_server_id", id).Msg("begin mcp connect")
		// A service that will not register clients is not a service we failed to
		// reach, and telling somebody it was unreachable sends them to check a
		// network that is working. It is the one cause here with something to
		// DO about it, so it is the one that gets said out loud; everything else
		// stays generic, because the detail of a remote failure is for the log.
		if errors.Is(err, mcpclient.ErrNoClientRegistration) {
			writeError(w, http.StatusBadRequest, "client_registration_required",
				mcpclient.ErrNoClientRegistration.Error())
			return
		}
		writeError(w, http.StatusBadGateway, "connect_failed",
			"the service could not be reached for authorization")
		return
	}
	writeJSON(w, http.StatusOK, connectResponse{AuthorizeURL: authorizeURL})
}

// callback is where the remote authorization server sends the person back.
// It answers a human looking at a browser tab, so it speaks HTML, once,
// plainly.
func (h *mcpHandlers) callback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	state, code := query.Get("state"), query.Get("code")
	if remoteErr := query.Get("error"); remoteErr != "" {
		h.callbackPage(w, http.StatusBadRequest, "The service refused the connection.",
			firstNonEmpty(query.Get("error_description"), remoteErr))
		return
	}
	if state == "" || code == "" {
		h.callbackPage(w, http.StatusBadRequest, "This address only works at the end of a connection attempt.", "")
		return
	}

	server, result, err := h.app.CompleteMCPConnect(r.Context(), state, code)
	if err != nil {
		h.app.Log.Error().Err(err).Msg("complete mcp connect")
		// The same words the console is told over its socket, from one place:
		// two accounts of one failure is how they come to disagree.
		h.callbackPage(w, http.StatusBadRequest, "The connection could not be completed.",
			app.MCPConnectReason(err))
		return
	}
	detail := "No tools were found yet; use Sync in the console."
	if result.Offered > 0 {
		detail = "Its tools are now available to grant in the console."
	}
	h.callbackPage(w, http.StatusOK, "Connected to "+server.Name+".", detail)
}

// callbackPage is deliberately self-contained HTML: this window was opened by a
// redirect and does not share the console's session. It is the last page of the
// consent and it speaks to the person looking at it, once, plainly.
//
// It used to carry a script that posted the outcome to `window.opener` and
// closed itself, so the console the person came from could say so and refresh.
// That is gone, and with it the last thing here that ran. The consent is opened
// from a click of the person's own now (KB/36: a window cannot be opened after
// an await, and the desktop application has no windows to open), which means
// this page is very often not in the same browser as the console and, on the
// desktop, not in the same APPLICATION. There is nobody on the other end of a
// postMessage. The console is told over its socket instead, by the gateway,
// which knows the answer either way (`CompleteMCPConnect`).
//
// So there is no script at all. Nothing to close a window with, and no message
// posted to `"*"` in the hope somebody is listening.
func (h *mcpHandlers) callbackPage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	page := "<!doctype html><meta charset=\"utf-8\"><title>" + html.EscapeString(title) + "</title>" +
		"<body style=\"font-family:system-ui;display:grid;place-items:center;min-height:90vh\">" +
		"<div style=\"text-align:center\"><h1 style=\"font-size:1.2rem\">" + html.EscapeString(title) + "</h1>" +
		"<p style=\"color:#666\">" + html.EscapeString(detail) + "</p>" +
		"<p style=\"color:#666\">You can close this window and go back to the console.</p></div>" +
		"</body>"
	_, _ = w.Write([]byte(page))
}

func (h *mcpHandlers) load(w http.ResponseWriter, r *http.Request) (*model.MCPServer, bool) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return nil, false
	}
	m, err := h.app.Store.MCPServers().GetByID(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return nil, false
	}
	return m, true
}

func mcpServerProblems(m *model.MCPServer) fieldErrors {
	problems := fieldErrors{}
	if m.Name == "" {
		problems["name"] = "a name is required"
	} else if m.ToolPrefix == "" {
		problems["name"] = "the name gave nothing to derive a tool prefix from"
	}
	parsed, err := url.Parse(m.URL)
	switch {
	case m.URL == "":
		problems["url"] = "the service's address is required"
	case err != nil || parsed.Host == "":
		problems["url"] = "not a usable address"
	case parsed.Scheme != "https" && parsed.Scheme != "http":
		problems["url"] = "the address must be http or https"
	}
	valid := false
	for _, t := range model.KnownMCPAuthTypes {
		valid = valid || m.AuthType == t
	}
	if !valid {
		problems["auth_type"] = "must be none, api_key or oauth"
	}
	return problems
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
