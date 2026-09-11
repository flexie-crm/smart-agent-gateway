package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/auth"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/oauth"
)

const (
	sessionCookie = "sag_oauth_session"
	sessionTTL    = 2 * time.Hour
)

type oauthHandlers struct {
	app *app.App
}

// mountOAuth registers the authorization-server endpoints. Discovery
// documents sit at the site root as RFC 8414/9728 require.
func mountOAuth(r chi.Router, a *app.App) {
	h := &oauthHandlers{app: a}

	r.Get("/.well-known/oauth-authorization-server", h.wellKnownAuthServer)
	r.Get("/.well-known/oauth-protected-resource", h.wellKnownProtectedResource)

	r.Route("/oauth2", func(r chi.Router) {
		r.Post("/register", h.register)
		r.Post("/token", h.token)
		r.Post("/revoke", h.revoke)
		r.Post("/introspect", h.introspect)
		r.Get("/authorize", h.authorize)
		r.Post("/login", h.login)
		r.Post("/consent", h.consent)
	})
}

// --- Discovery ----------------------------------------------------------------

func (h *oauthHandlers) wellKnownAuthServer(w http.ResponseWriter, _ *http.Request) {
	issuer := h.app.OAuth.Issuer()
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/oauth2/authorize",
		"token_endpoint":                        issuer + "/oauth2/token",
		"registration_endpoint":                 issuer + "/oauth2/register",
		"revocation_endpoint":                   issuer + "/oauth2/revoke",
		"introspection_endpoint":                issuer + "/oauth2/introspect",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic", "client_secret_post"},
		"scopes_supported":                      oauth.KnownScopes,
	})
}

func (h *oauthHandlers) wellKnownProtectedResource(w http.ResponseWriter, _ *http.Request) {
	issuer := h.app.OAuth.Issuer()
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 issuer + "/mcp",
		"authorization_servers":    []string{issuer},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         oauth.KnownScopes,
	})
}

// --- Client registration (RFC 7591) --------------------------------------------

func (h *oauthHandlers) register(w http.ResponseWriter, r *http.Request) {
	var req oauth.DCRRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_client_metadata", "error_description": "malformed JSON body",
		})
		return
	}
	resp, err := h.app.OAuth.RegisterClient(r.Context(), req)
	if err != nil {
		h.writeOAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// --- Token, revocation, introspection ---------------------------------------------

func (h *oauthHandlers) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.writeOAuthError(w, &oauth.Error{Code: "invalid_request", Description: "malformed form body", Status: http.StatusBadRequest})
		return
	}
	clientID, clientSecret := clientCredentials(r)
	resp, err := h.app.OAuth.Token(r.Context(), oauth.TokenParams{
		GrantType:    r.PostFormValue("grant_type"),
		Code:         r.PostFormValue("code"),
		RedirectURI:  r.PostFormValue("redirect_uri"),
		CodeVerifier: r.PostFormValue("code_verifier"),
		RefreshToken: r.PostFormValue("refresh_token"),
		Scope:        r.PostFormValue("scope"),
		ClientID:     clientID,
		ClientSecret: clientSecret,
	})
	if err != nil {
		h.writeOAuthError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

func (h *oauthHandlers) revoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.writeOAuthError(w, &oauth.Error{Code: "invalid_request", Description: "malformed form body", Status: http.StatusBadRequest})
		return
	}
	clientID, clientSecret := clientCredentials(r)
	if err := h.app.OAuth.Revoke(r.Context(), clientID, clientSecret, r.PostFormValue("token")); err != nil {
		h.writeOAuthError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *oauthHandlers) introspect(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.writeOAuthError(w, &oauth.Error{Code: "invalid_request", Description: "malformed form body", Status: http.StatusBadRequest})
		return
	}
	clientID, clientSecret := clientCredentials(r)
	resp, err := h.app.OAuth.Introspect(r.Context(), clientID, clientSecret, r.PostFormValue("token"))
	if err != nil {
		h.writeOAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// clientCredentials pulls client auth from Basic auth first, then the form
// body (client_secret_post), then bare client_id (public clients).
func clientCredentials(r *http.Request) (string, string) {
	if id, secret, ok := r.BasicAuth(); ok {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
		return id, secret
	}
	return r.PostFormValue("client_id"), r.PostFormValue("client_secret")
}

// --- Authorize flow (login + consent) ------------------------------------------------

func authorizeParamsFromValues(v url.Values) oauth.AuthorizeParams {
	return oauth.AuthorizeParams{
		ClientID:            v.Get("client_id"),
		RedirectURI:         v.Get("redirect_uri"),
		ResponseType:        v.Get("response_type"),
		Scope:               v.Get("scope"),
		State:               v.Get("state"),
		CodeChallenge:       v.Get("code_challenge"),
		CodeChallengeMethod: v.Get("code_challenge_method"),
	}
}

func (h *oauthHandlers) authorize(w http.ResponseWriter, r *http.Request) {
	params := authorizeParamsFromValues(r.URL.Query())
	req, fatal, redirectErr := h.app.OAuth.ValidateAuthorize(r.Context(), params)
	if fatal != nil {
		h.renderErrorPage(w, fatal)
		return
	}
	if redirectErr != nil {
		redirectWithError(w, r, params, redirectErr)
		return
	}

	session := h.currentSession(r)
	if session == nil {
		h.renderLogin(w, r.URL.RequestURI(), "")
		return
	}

	if h.app.OAuth.HasConsent(r.Context(), session.UserID, req.Client.ID, req.Scopes) {
		h.issueCodeAndRedirect(w, r, req, params, session)
		return
	}
	h.renderConsent(w, req, params)
}

func (h *oauthHandlers) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderErrorPage(w, &oauth.Error{Code: "invalid_request", Description: "malformed form body", Status: http.StatusBadRequest})
		return
	}
	next := r.PostFormValue("next")
	if !strings.HasPrefix(next, "/oauth2/authorize?") {
		h.renderErrorPage(w, &oauth.Error{Code: "invalid_request", Description: "invalid return target", Status: http.StatusBadRequest})
		return
	}

	ctx := r.Context()

	user, err := h.app.Authenticate(ctx, r.PostFormValue("email"), r.PostFormValue("password"))
	if err != nil {
		h.renderLogin(w, next, "Invalid email or password.")
		return
	}
	// The consent screen has no workspace switcher, so it grants the one the
	// person works in: the last they chose, or the first they belong to. A
	// person who belongs nowhere consents to nothing.
	ws, err := h.app.ActiveWorkspace(ctx, user.ID)
	if errors.Is(err, app.ErrNoWorkspace) {
		h.renderLogin(w, next, "This account is not in any workspace yet.")
		return
	}
	if err != nil {
		h.app.Log.Error().Err(err).Msg("resolve workspace")
		h.renderErrorPage(w, &oauth.Error{Code: "server_error", Description: "temporary failure", Status: http.StatusInternalServerError})
		return
	}

	value, err := h.app.Sessions.Issue(user.ID, ws.ID, sessionTTL)
	if err != nil {
		h.app.Log.Error().Err(err).Msg("issue session")
		h.renderErrorPage(w, &oauth.Error{Code: "server_error", Description: "temporary failure", Status: http.StatusInternalServerError})
		return
	}
	// Secure tracks the issuer scheme rather than being hardcoded: a
	// Secure cookie is never sent over http, which would break local
	// development. Any https deployment gets it. HttpOnly and SameSite are
	// unconditional.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is scheme-derived, see above
		Name:     sessionCookie,
		Value:    value,
		Path:     "/oauth2",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   strings.HasPrefix(h.app.OAuth.Issuer(), "https://"),
		MaxAge:   int(sessionTTL.Seconds()),
	})
	// next is not an open redirect: it was rejected above unless it starts
	// with "/oauth2/authorize?", so it is always a path inside this app.
	http.Redirect(w, r, next, http.StatusFound) //nolint:gosec // G710: prefix-validated relative path
}

func (h *oauthHandlers) consent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderErrorPage(w, &oauth.Error{Code: "invalid_request", Description: "malformed form body", Status: http.StatusBadRequest})
		return
	}
	params := authorizeParamsFromValues(r.PostForm)

	// Everything is re-validated from scratch: the form is untrusted input.
	req, fatal, redirectErr := h.app.OAuth.ValidateAuthorize(r.Context(), params)
	if fatal != nil {
		h.renderErrorPage(w, fatal)
		return
	}
	if redirectErr != nil {
		redirectWithError(w, r, params, redirectErr)
		return
	}
	session := h.currentSession(r)
	if session == nil {
		h.renderLogin(w, "/oauth2/authorize?"+r.PostForm.Encode(), "")
		return
	}

	if r.PostFormValue("action") != "approve" {
		redirectWithError(w, r, params, &oauth.Error{Code: "access_denied", Description: "the user denied the request"})
		return
	}
	if r.PostFormValue("remember") == "on" {
		if err := h.app.OAuth.RememberConsent(r.Context(), session.UserID, req.Client.ID, req.Scopes); err != nil {
			h.app.Log.Error().Err(err).Msg("remember consent")
		}
	}
	h.issueCodeAndRedirect(w, r, req, params, session)
}

func (h *oauthHandlers) issueCodeAndRedirect(w http.ResponseWriter, r *http.Request, req *oauth.AuthorizeRequest, params oauth.AuthorizeParams, session *auth.Session) {
	code, err := h.app.OAuth.IssueAuthCode(r.Context(), req, params, session.UserID, session.WorkspaceID)
	if err != nil {
		h.app.Log.Error().Err(err).Msg("issue auth code")
		redirectWithError(w, r, params, &oauth.Error{Code: "server_error", Description: "temporary failure"})
		return
	}
	target, _ := url.Parse(params.RedirectURI)
	q := target.Query()
	q.Set("code", code)
	if params.State != "" {
		q.Set("state", params.State)
	}
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (h *oauthHandlers) currentSession(r *http.Request) *auth.Session {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	session, err := h.app.Sessions.Verify(cookie.Value)
	if err != nil {
		return nil
	}
	return session
}

func redirectWithError(w http.ResponseWriter, r *http.Request, params oauth.AuthorizeParams, oe *oauth.Error) {
	target, err := url.Parse(params.RedirectURI)
	if err != nil {
		http.Error(w, "invalid redirect", http.StatusBadRequest)
		return
	}
	q := target.Query()
	q.Set("error", oe.Code)
	q.Set("error_description", oe.Description)
	if params.State != "" {
		q.Set("state", params.State)
	}
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// --- Error + page rendering -------------------------------------------------------------

func (h *oauthHandlers) writeOAuthError(w http.ResponseWriter, err error) {
	var oe *oauth.Error
	if errors.As(err, &oe) {
		status := oe.Status
		if status == 0 || status == http.StatusFound {
			status = http.StatusBadRequest
		}
		if oe.Code == "invalid_client" {
			w.Header().Set("WWW-Authenticate", `Basic realm="oauth2"`)
		}
		writeJSON(w, status, map[string]string{"error": oe.Code, "error_description": oe.Description})
		return
	}
	// Never leak internals to external clients (error-hygiene rule).
	h.app.Log.Error().Err(err).Msg("oauth internal error")
	writeJSON(w, http.StatusInternalServerError, map[string]string{
		"error": "server_error", "error_description": "temporary failure",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Pages compose a layout with a per-page body template. Nothing is ever
// injected as pre-rendered template.HTML: every value passes through
// html/template escaping on the way out.

const layoutHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} | SAG</title>
<style>
 body{margin:0;min-height:100vh;display:grid;place-items:center;font-family:system-ui,sans-serif;background:#0f1115;color:#e6e8ee}
 .card{background:#181b22;border:1px solid #262b36;border-radius:12px;padding:32px;width:360px}
 h1{font-size:18px;margin:0 0 16px} p{color:#9aa1af;font-size:14px}
 label{display:block;font-size:13px;color:#9aa1af;margin:12px 0 4px}
 input[type=text],input[type=email],input[type=password]{width:100%;box-sizing:border-box;padding:8px 10px;border-radius:8px;border:1px solid #313848;background:#0f1115;color:#e6e8ee}
 button{margin-top:16px;width:100%;padding:10px;border:0;border-radius:8px;background:#4d7cfe;color:#fff;font-weight:600;cursor:pointer}
 button.secondary{background:#262b36}
 ul{padding-left:18px;color:#9aa1af;font-size:14px}
 .error{color:#ff7a7a;font-size:13px;margin-top:8px}
 .row{display:flex;gap:8px;align-items:center;margin-top:12px;font-size:13px;color:#9aa1af}
</style></head><body><div class="card">{{template "body" .}}</div></body></html>`

const loginHTML = `
<h1>Sign in to continue</h1>
<p>An application is requesting access to your SAG account.</p>
<form method="post" action="/oauth2/login">
 <input type="hidden" name="next" value="{{.Next}}">
 <label>Workspace</label><input type="text" name="workspace" required autofocus>
 <label>Email</label><input type="email" name="email" required>
 <label>Password</label><input type="password" name="password" required>
 {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
 <button type="submit">Sign in</button>
</form>`

const consentHTML = `
<h1>Authorize {{.ClientName}}</h1>
<p><strong>{{.ClientName}}</strong> is requesting:</p>
<ul>{{range .Scopes}}<li>{{.}}</li>{{end}}</ul>
<form method="post" action="/oauth2/consent">
 {{range $k, $v := .Params}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
 <div class="row"><input type="checkbox" name="remember" id="remember"><label for="remember" style="margin:0">Remember this decision</label></div>
 <button type="submit" name="action" value="approve">Allow</button>
 <button type="submit" name="action" value="deny" class="secondary">Deny</button>
</form>`

const errorHTML = `
<h1>Request error</h1>
<p>{{.Message}}</p>`

var (
	loginPage   = mustPage(loginHTML)
	consentPage = mustPage(consentHTML)
	errorPage   = mustPage(errorHTML)
)

func mustPage(body string) *template.Template {
	t := template.Must(template.New("layout").Parse(layoutHTML))
	return template.Must(t.New("body").Parse(body))
}

type loginPageData struct {
	Title string
	Next  string
	Error string
}

type consentPageData struct {
	Title      string
	ClientName string
	Scopes     []string
	Params     map[string]string
}

type errorPageData struct {
	Title   string
	Message string
}

// renderPage renders into a buffer first: a template failure must not leave
// a half-written response with a success status already sent.
func (h *oauthHandlers) renderPage(w http.ResponseWriter, status int, page *template.Template, data any) {
	var buf bytes.Buffer
	if err := page.ExecuteTemplate(&buf, "layout", data); err != nil {
		h.app.Log.Error().Err(err).Msg("render page")
		writeError(w, http.StatusInternalServerError, "server_error", "temporary failure")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (h *oauthHandlers) renderLogin(w http.ResponseWriter, next, errMsg string) {
	h.renderPage(w, http.StatusOK, loginPage, loginPageData{Title: "Sign in", Next: next, Error: errMsg})
}

var scopeDescriptions = map[string]string{
	model.ScopeMCP: "Use your account through connected AI tools (MCP)",
}

func (h *oauthHandlers) renderConsent(w http.ResponseWriter, req *oauth.AuthorizeRequest, params oauth.AuthorizeParams) {
	scopes := make([]string, 0, len(req.Scopes))
	for _, s := range req.Scopes {
		if d, ok := scopeDescriptions[s]; ok {
			scopes = append(scopes, d)
		} else {
			scopes = append(scopes, s)
		}
	}
	h.renderPage(w, http.StatusOK, consentPage, consentPageData{
		Title:      "Authorize",
		ClientName: req.Client.Name,
		Scopes:     scopes,
		Params: map[string]string{
			"client_id":             params.ClientID,
			"redirect_uri":          params.RedirectURI,
			"response_type":         params.ResponseType,
			"scope":                 params.Scope,
			"state":                 params.State,
			"code_challenge":        params.CodeChallenge,
			"code_challenge_method": params.CodeChallengeMethod,
		},
	})
}

func (h *oauthHandlers) renderErrorPage(w http.ResponseWriter, oe *oauth.Error) {
	status := oe.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	h.renderPage(w, status, errorPage, errorPageData{Title: "Error", Message: oe.Description})
}
