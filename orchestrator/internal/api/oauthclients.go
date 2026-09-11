package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/oauth"
)

// The OAuth clients admin surface (CRM ClientController parity): the three
// client types, secrets and service tokens shown exactly once, and deletion
// as the kill switch that takes every token the client owns.

type oauthClientHandlers struct{ app *app.App }

func mountOAuthClients(r chi.Router, a *app.App) {
	h := &oauthClientHandlers{app: a}
	r.Route("/oauth-clients", func(r chi.Router) {
		r.With(requirePermission(a, model.PermOAuthClientsView)).Get("/", h.list)
		r.With(requirePermission(a, model.PermOAuthClientsCreate)).Post("/", h.create)
		r.With(requirePermission(a, model.PermOAuthClientsEdit)).Put("/{id}", h.update)
		r.With(requirePermission(a, model.PermOAuthClientsDelete)).Delete("/{id}", h.delete)
	})
}

// oauthClientBody never carries a secret or a token. Those exist in a
// response exactly once, at mint time, in the dedicated fields below.
type oauthClientBody struct {
	ID           int64     `json:"id"`
	ClientID     string    `json:"client_id"`
	Name         string    `json:"name"`
	ClientType   string    `json:"client_type"`
	RedirectURIs []string  `json:"redirect_uris"`
	GrantTypes   []string  `json:"grant_types"`
	Scopes       []string  `json:"scopes"`
	IsDCR        bool      `json:"is_dcr"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`

	// Shown once, at mint time, never again: the confidential secret or the
	// service token. Copy it now or roll the client.
	ClientSecret string `json:"client_secret,omitempty"`
	ServiceToken string `json:"service_token,omitempty"`
}

func newOAuthClientBody(c *model.OAuthClient) *oauthClientBody {
	return &oauthClientBody{
		ID: c.ID, ClientID: c.ClientID, Name: c.Name, ClientType: c.ClientType,
		RedirectURIs: c.RedirectURIs, GrantTypes: c.GrantTypes, Scopes: c.Scopes,
		IsDCR: c.IsDCR, Status: c.Status, CreatedAt: c.CreatedAt,
	}
}

type oauthClientRequest struct {
	Name         string   `json:"name"`
	ClientType   string   `json:"client_type"`
	RedirectURIs []string `json:"redirect_uris"`
	Status       string   `json:"status"`
}

func (h *oauthClientHandlers) list(w http.ResponseWriter, r *http.Request) {
	clients, err := h.app.Store.OAuth().ListClients(r.Context(), claimsFrom(r).WorkspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, mapSlice(clients, newOAuthClientBody))
}

func (h *oauthClientHandlers) create(w http.ResponseWriter, r *http.Request) {
	var req oauthClientRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	claims := claimsFrom(r)
	req.RedirectURIs = trimNonEmpty(req.RedirectURIs)
	if problems := h.clientProblems(r, &req); len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}

	clientID, err := oauth.NewClientID()
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	workspaceID := claims.WorkspaceID
	client := &model.OAuthClient{
		WorkspaceID:  &workspaceID,
		ClientID:     clientID,
		ClientType:   req.ClientType,
		Name:         strings.TrimSpace(req.Name),
		RedirectURIs: req.RedirectURIs,
		GrantTypes:   grantTypesFor(req.ClientType),
		Scopes:       []string{model.ScopeMCP},
		Status:       model.StatusActive,
	}

	body := &oauthClientBody{}
	if req.ClientType == model.OAuthClientConfidential {
		secret, hash, err := oauth.NewClientSecret()
		if err != nil {
			writeStoreError(w, h.app, err)
			return
		}
		client.ClientSecretHash = hash
		body.ClientSecret = secret
	}
	if err := h.app.Store.OAuth().CreateClient(r.Context(), client); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	// The service token is minted against the person who asked for it: it
	// acts as them, carries their access, and dies with their account or
	// their permission.
	if req.ClientType == model.OAuthClientService {
		token, err := h.app.OAuth.IssueServiceToken(r.Context(), client, claims.UserID, claims.WorkspaceID)
		if err != nil {
			writeStoreError(w, h.app, err)
			return
		}
		body.ServiceToken = token
	}

	*body = oauthClientBody{
		ID: client.ID, ClientID: client.ClientID, Name: client.Name, ClientType: client.ClientType,
		RedirectURIs: client.RedirectURIs, GrantTypes: client.GrantTypes, Scopes: client.Scopes,
		IsDCR: client.IsDCR, Status: client.Status, CreatedAt: client.CreatedAt,
		ClientSecret: body.ClientSecret, ServiceToken: body.ServiceToken,
	}
	writeJSON(w, http.StatusCreated, body)
}

func (h *oauthClientHandlers) update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	client, err := h.app.Store.OAuth().GetClientForWorkspace(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	var req oauthClientRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.RedirectURIs = trimNonEmpty(req.RedirectURIs)
	problems := h.clientProblems(r, &req)
	if !validStatus(req.Status) {
		problems["status"] = "must be active or disabled"
	}
	if len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}

	// Type toggles follow the CRM exactly: leaving confidential clears the
	// secret; arriving at confidential mints a fresh one, shown once.
	// Editing a service client never re-mints its token; roll it by
	// deleting the client and creating a new one.
	body := newOAuthClientBody(client)
	if client.ClientType != model.OAuthClientConfidential && req.ClientType == model.OAuthClientConfidential {
		secret, hash, err := oauth.NewClientSecret()
		if err != nil {
			writeStoreError(w, h.app, err)
			return
		}
		client.ClientSecretHash = hash
		body.ClientSecret = secret
	}
	if req.ClientType != model.OAuthClientConfidential {
		client.ClientSecretHash = ""
	}

	client.Name = strings.TrimSpace(req.Name)
	client.ClientType = req.ClientType
	client.RedirectURIs = req.RedirectURIs
	client.GrantTypes = grantTypesFor(req.ClientType)
	client.Status = req.Status
	if err := h.app.Store.OAuth().UpdateClient(r.Context(), client); err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	fresh := newOAuthClientBody(client)
	fresh.ClientSecret = body.ClientSecret
	writeJSON(w, http.StatusOK, fresh)
}

// delete removes the client AND, through the database's cascades, every
// token it owns. For a service client this is the kill switch.
func (h *oauthClientHandlers) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := h.app.Store.OAuth().DeleteClient(r.Context(), claimsFrom(r).WorkspaceID, id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *oauthClientHandlers) clientProblems(r *http.Request, req *oauthClientRequest) fieldErrors {
	problems := fieldErrors{}
	if strings.TrimSpace(req.Name) == "" {
		problems["name"] = "a name is required"
	}
	switch req.ClientType {
	case model.OAuthClientPublic, model.OAuthClientConfidential:
		if len(req.RedirectURIs) == 0 {
			problems["redirect_uris"] = "at least one redirect address is required"
		}
	case model.OAuthClientService:
		// A machine token acts as the person minting it, so that person
		// must themselves be allowed onto the MCP surface.
		claims := claimsFrom(r)
		allowed, err := h.app.Authorize(r.Context(), claims.UserID, model.PermMCPConnect)
		if err != nil || !allowed {
			problems["client_type"] = "a service token acts as you, and your account is not allowed to connect an external agent"
		}
	default:
		problems["client_type"] = "must be public, confidential or service"
	}
	return problems
}

func grantTypesFor(clientType string) []string {
	if clientType == model.OAuthClientService {
		// Inert at every grant endpoint: the client exists to own one
		// long-lived token, nothing more.
		return []string{}
	}
	return []string{"authorization_code", "refresh_token"}
}

func trimNonEmpty(values []string) []string {
	kept := make([]string, 0, len(values))
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return kept
}
