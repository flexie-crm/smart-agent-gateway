package api

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
)

type authHandlers struct{ app *app.App }

func mountAuth(r chi.Router, a *app.App) {
	h := &authHandlers{app: a}
	r.Post("/auth/login", h.login)
	r.Post("/auth/refresh", h.refresh)
	r.Post("/auth/logout", h.logout)
	// The Enter button. It exists ONLY where there is one person and only this
	// machine can reach the port, so it is registered rather than gated: on a
	// deployment the route is not there at all, which is a stronger statement
	// than a handler that checks a flag and could be reached by a mistake.
	//
	// A working copy (`sag dev`) has the same button for the same reason: only
	// this machine can reach it, and the person at it already owns the database.
	// `sag server` cannot reach either condition, so on a deployment the route
	// is still not there at all.
	if a.Config.Personal || a.Config.Dev {
		r.Post("/auth/local", h.localSignIn)
	}

	r.Group(func(r chi.Router) {
		r.Use(requireAuth(a))
		r.Get("/auth/me", h.me)
		r.Post("/auth/workspace", h.switchWorkspace)
		// The workspaces the CALLER may act in, which is what the switcher
		// offers. The tenant's list is /v1/workspaces, a different question
		// with its own permission.
		r.Get("/auth/workspaces", h.listWorkspaces)

		r.Get("/me/settings", h.listSettings)
		r.Put("/me/settings/{key}", h.setSetting)
		r.Delete("/me/settings/{key}", h.deleteSetting)
	})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	// The refresh token is DELIBERATELY not in this body. It travels only in an
	// HttpOnly cookie the browser attaches on its own, so no JavaScript, and
	// therefore no XSS, can ever read it. The access token, short-lived, is the
	// only credential the client code touches, and it is held in memory.
	User      *userBody      `json:"user"`
	Workspace *workspaceBody `json:"workspace"`
	// Workspaces is every workspace the person may enter, so the client can
	// offer the choice without a second request. The current one is Workspace.
	Workspaces []*workspaceBody `json:"workspaces"`
}

const (
	// refreshCookie carries the rotating refresh token. Its three attributes are
	// the whole point: HttpOnly keeps it out of JavaScript's reach (an XSS cannot
	// read or exfiltrate it), SameSite=Lax means the browser never sends it on a
	// cross-SITE request (which is what a CSRF against /v1/auth/refresh would be),
	// and the path scopes it to the auth endpoints so it rides no other traffic,
	// not the chat, not the stream.
	refreshCookie     = "sag_refresh"
	refreshCookiePath = "/v1/auth"
)

// cookieSecure reports whether the refresh cookie should be marked Secure. It
// tracks the request scheme rather than being hardcoded, exactly as the OAuth
// session cookie does (oauth.go): a Secure cookie is never sent over http, which
// would break local development on http://localhost, while any https deployment
// gets it. X-Forwarded-Proto is honoured for a TLS-terminating proxy; spoofing
// it can only make a cookie MORE restrictive (Secure), never grant anything, so
// it is safe to read here even though trusted-proxy handling is otherwise
// deferred.
func cookieSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// setRefreshCookie writes the rotating refresh token into the HttpOnly cookie.
// Every issuing path (login, refresh, switch) calls it, so a new token always
// replaces the old one in the same place.
func setRefreshCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is scheme-derived, see cookieSecure
		Name:     refreshCookie,
		Value:    token,
		Path:     refreshCookiePath,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(r),
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
	})
}

// clearRefreshCookie removes the cookie by expiring it in place, with the SAME
// name and path, so logout and a dead-token refresh leave nothing behind that
// the browser would keep sending.
func clearRefreshCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is scheme-derived, see cookieSecure
		Name:     refreshCookie,
		Value:    "",
		Path:     refreshCookiePath,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(r),
		MaxAge:   -1,
	})
}

type workspaceBody struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func newWorkspaceBody(w *model.Workspace) *workspaceBody {
	if w == nil {
		return nil
	}
	return &workspaceBody{ID: w.ID, Slug: w.Slug, Name: w.Name}
}

func (h *authHandlers) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "email and password are required")
		return
	}

	result, err := h.app.Login(r.Context(), app.LoginRequest{
		Email:     req.Email,
		Password:  req.Password,
		UserAgent: r.UserAgent(),
		IP:        clientIP(r),
	})
	if errors.Is(err, app.ErrInvalidCredentials) {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
		return
	}
	// The password was right. Saying "check your credentials" here would send
	// someone to fix the one thing that is not broken.
	if errors.Is(err, app.ErrNoWorkspace) {
		writeError(w, http.StatusForbidden, "no_workspace",
			"this account is not in any workspace yet. An administrator has to add it to one.")
		return
	}
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeTokens(w, r, result)
}

// localSignIn hands the person at this computer their session.
//
// There is no shortcut underneath it: it calls the same Login every other client
// calls, with the seeded owner's real password, against the same hash. So the
// session, its rotation family, its revocation and the live permission re-check
// on every later request are all exactly what a server deployment does. What is
// removed is the typing, not the check.
//
// The credential it presents on the caller's behalf is a file in the
// application's own data directory, readable only by this account. That is the
// trust boundary: whoever can reach this server is already sitting at the
// computer whose files hold it, and asking them to invent a password to protect
// data they already own would be a ritual rather than a control.
func (h *authHandlers) localSignIn(w http.ResponseWriter, r *http.Request) {
	if !fromThisMachine(r) {
		writeError(w, http.StatusForbidden, "not_local",
			"this sign-in only answers the computer it is running on")
		return
	}
	email, secret := h.localCredential()
	if secret == "" {
		writeError(w, http.StatusServiceUnavailable, "not_ready",
			"this installation has not finished setting itself up")
		return
	}

	result, err := h.app.Login(r.Context(), app.LoginRequest{
		Email:     email,
		Password:  secret,
		UserAgent: r.UserAgent(),
		IP:        clientIP(r),
	})
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeTokens(w, r, result)
}

// localCredential is the account the local sign-in presents, which depends on
// what put the route there.
//
// Both are a REAL credential going through the ordinary Login. A desktop's is
// generated once and kept in a file only its owner can read; a working copy's
// is named in the environment by whoever is working. Neither is a second
// authentication path, which is the property worth keeping: a development
// server that signed people in differently would prove nothing about the one
// that matters.
func (h *authHandlers) localCredential() (email, secret string) {
	if h.app.Config.Dev {
		return h.app.Config.DevSignInEmail, h.app.Config.DevSignInPassword
	}
	return h.app.Config.PersonalOwnerEmail, h.app.Config.PersonalOwnerSecret
}

type switchWorkspaceRequest struct {
	WorkspaceID int64 `json:"workspace_id"`
}

// switchWorkspace re-issues the caller's tokens against another workspace they
// belong to. It is a token operation, not a preference: the new workspace is a
// claim, so it can only come from the server.
func (h *authHandlers) switchWorkspace(w http.ResponseWriter, r *http.Request) {
	var req switchWorkspaceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.WorkspaceID == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "workspace_id is required")
		return
	}
	// The old refresh token comes from the cookie, so switching revokes the
	// session it belonged to and leaves nothing that still speaks for the
	// workspace being left.
	old := ""
	if cookie, err := r.Cookie(refreshCookie); err == nil {
		old = cookie.Value
	}
	result, err := h.app.SwitchWorkspace(r.Context(), claimsFrom(r).UserID, req.WorkspaceID,
		old, r.UserAgent(), clientIP(r))
	if errors.Is(err, app.ErrNotMember) {
		writeError(w, http.StatusForbidden, "forbidden", "you are not a member of that workspace")
		return
	}
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeTokens(w, r, result)
}

// listWorkspaces returns the workspaces the CALLER may act in, which is what the
// switcher offers. It is not the tenant's list of workspaces: seeing one you
// cannot enter is an invitation to a 403.
func (h *authHandlers) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	workspaces, err := h.app.Store.Workspaces().ListForUser(r.Context(), claimsFrom(r).UserID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	bodies := make([]*workspaceBody, 0, len(workspaces))
	for _, ws := range workspaces {
		bodies = append(bodies, newWorkspaceBody(ws))
	}
	writeJSON(w, http.StatusOK, bodies)
}

// --- settings ---------------------------------------------------------------
//
// A person's own preferences, and only their own: the key is scoped by the user
// id on the token, never by anything in the request.

func (h *authHandlers) listSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := h.app.Store.Settings().All(r.Context(), claimsFrom(r).UserID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	values := map[string]string{}
	for _, s := range settings {
		values[s.Key] = s.Value
	}
	writeJSON(w, http.StatusOK, values)
}

type settingRequest struct {
	Value string `json:"value"`
}

func (h *authHandlers) setSetting(w http.ResponseWriter, r *http.Request) {
	var req settingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	key := chi.URLParam(r, "key")
	if key == "" || len(key) > settingKeyMax {
		writeError(w, http.StatusBadRequest, "invalid_request", "a setting key is 1 to 191 characters")
		return
	}
	if err := h.app.Store.Settings().Set(r.Context(), claimsFrom(r).UserID, key, req.Value); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *authHandlers) deleteSetting(w http.ResponseWriter, r *http.Request) {
	if err := h.app.Store.Settings().Delete(r.Context(), claimsFrom(r).UserID, chi.URLParam(r, "key")); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// settingKeyMax matches the column: a key that would be truncated by the
// database is refused by the API instead, so nobody stores one thing and reads
// back another.
const settingKeyMax = 191

// refresh mints a fresh access token from the refresh-token cookie and rotates
// the cookie in the same response (the presented token is consumed, single-use).
// It is what a page load calls to recover a session that lives only in an
// HttpOnly cookie: there is no body, the cookie is the whole credential.
func (h *authHandlers) refresh(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(refreshCookie)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "invalid_grant", "no active session")
		return
	}
	result, err := h.app.Refresh(r.Context(), cookie.Value, r.UserAgent(), clientIP(r))
	if errors.Is(err, app.ErrInvalidRefresh) {
		// The token is dead (expired, revoked, or already rotated). Clear the
		// cookie so the browser stops presenting one that can only ever fail.
		clearRefreshCookie(w, r)
		writeError(w, http.StatusUnauthorized, "invalid_grant", "invalid or expired refresh token")
		return
	}
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeTokens(w, r, result)
}

func (h *authHandlers) logout(w http.ResponseWriter, r *http.Request) {
	// The refresh token comes from the cookie: revoke the session behind it, and
	// clear the cookie either way so a logged-out browser is left holding nothing.
	if cookie, err := r.Cookie(refreshCookie); err == nil && cookie.Value != "" {
		if err := h.app.Logout(r.Context(), cookie.Value); err != nil {
			writeStoreError(w, h.app, err)
			return
		}
	}
	clearRefreshCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

type meResponse struct {
	User        *userBody        `json:"user"`
	Permissions []string         `json:"permissions"`
	Workspace   *workspaceBody   `json:"workspace"`
	Workspaces  []*workspaceBody `json:"workspaces"`
}

func (h *authHandlers) me(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	user, err := h.app.Store.Users().GetByID(r.Context(), claims.UserID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	perms, err := h.app.Store.Users().EffectivePermissions(r.Context(), claims.UserID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	// The console needs three things to draw itself, and one round trip is
	// enough for all of them: who you are, what you may do, and where you are
	// standing (plus where else you could stand).
	workspaces, err := h.app.Store.Workspaces().ListForUser(r.Context(), claims.UserID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	ids := make([]int64, 0, len(workspaces))
	body := meResponse{Permissions: perms, Workspaces: []*workspaceBody{}}
	for _, ws := range workspaces {
		ids = append(ids, ws.ID)
		body.Workspaces = append(body.Workspaces, newWorkspaceBody(ws))
		if ws.ID == claims.WorkspaceID {
			body.Workspace = newWorkspaceBody(ws)
		}
	}
	body.User = newUserBody(user, ids)
	writeJSON(w, http.StatusOK, body)
}

func writeTokens(w http.ResponseWriter, r *http.Request, result *app.LoginResult) {
	w.Header().Set("Cache-Control", "no-store")
	// The rotating refresh token leaves ONLY as the HttpOnly cookie, never in the
	// body, so client JavaScript never holds it.
	setRefreshCookie(w, r, result.RefreshToken, result.RefreshTokenExpiresAt)
	workspaces := make([]*workspaceBody, 0, len(result.Workspaces))
	for _, ws := range result.Workspaces {
		workspaces = append(workspaces, newWorkspaceBody(ws))
	}
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: result.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int64(time.Until(result.AccessTokenExpiresAt).Seconds()),
		User:        newUserBody(result.User, nil),
		Workspace:   newWorkspaceBody(result.Workspace),
		Workspaces:  workspaces,
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
