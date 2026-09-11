package mcpclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SAG as an OAuth 2.1 client: the half of the dance we play when a remote
// MCP server demands OAuth. Discovery finds the authorization server behind
// the resource (RFC 9728 then RFC 8414), registration introduces us to it
// (RFC 7591), and the code + PKCE exchange and the refresh are plain OAuth.
// Everything here works on plaintext values; sealing them is the app's job.

// Discovery is what we learned about the remote's authorization server. It is
// cached on the connection so a refresh never re-walks discovery.
type Discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint,omitempty"`
	// ScopesSupported is what the authorization server advertises (RFC 8414).
	ScopesSupported []string `json:"scopes_supported,omitempty"`
	// Scope is what WE request at authorize time: the scope registration
	// granted us, or everything the server advertises. Some servers (the
	// CRM's among them) refuse an authorize request that names no scope.
	Scope string `json:"scope,omitempty"`
	// Resource is the MCP endpoint the tokens are for, sent as the RFC 8707
	// resource indicator so an authorization server that scopes tokens per
	// resource issues them for the right one.
	Resource string `json:"resource"`
	// TokenEndpointAuthMethods is how this server lets a client prove who it is.
	// It decides what to ASK FOR when registering: a server that only accepts
	// public clients ("none") issues no secret, and registering as a
	// confidential client there produces a client that cannot authenticate.
	TokenEndpointAuthMethods []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	// CodeChallengeMethods is how this server does PKCE. Its ABSENCE is
	// meaningful: it is the only way a client can establish that PKCE is
	// supported at all, and the specification says a client must refuse to
	// proceed without it.
	CodeChallengeMethods []string `json:"code_challenge_methods_supported,omitempty"`
	// ClientIDMetadataDocument says the server will accept a URL as the client
	// id and fetch our description from it, so no registration is needed at all.
	// The third way in, after dynamic registration and a client entered by hand.
	ClientIDMetadataDocument bool `json:"client_id_metadata_document_supported,omitempty"`
}

// PrefersPublicClient reports that this server will not issue a secret, so we
// must register (or present ourselves) as a public client. True when it
// advertises "none" and nothing else we can use.
func (d *Discovery) PrefersPublicClient() bool {
	if len(d.TokenEndpointAuthMethods) == 0 {
		return false // said nothing; the OAuth default is client_secret_basic
	}
	for _, m := range d.TokenEndpointAuthMethods {
		if m == "client_secret_basic" || m == "client_secret_post" {
			return false
		}
	}
	return true
}

// RegistrationAuthMethod is what to ask for when registering: the confidential
// default, unless this server only takes public clients.
func (d *Discovery) RegistrationAuthMethod() string {
	if d.PrefersPublicClient() {
		return "none"
	}
	return "client_secret_basic"
}

// authServerMetadataURLs is where to look for a server's metadata, in the order
// the specification lays down.
//
// An issuer WITH a path is the awkward case, and the reason this is a list
// rather than two guesses: RFC 8414 inserts the path after the well-known
// segment, OpenID Connect Discovery historically appends it, and real servers
// do both. A client "MUST attempt multiple well-known endpoints", so a tenant
// under a path is found rather than reported as publishing nothing.
func authServerMetadataURLs(issuer string) []string {
	issuer = strings.TrimSuffix(issuer, "/")
	u, err := url.Parse(issuer)
	if err != nil || u.Path == "" || u.Path == "/" {
		return []string{
			issuer + "/.well-known/oauth-authorization-server",
			issuer + "/.well-known/openid-configuration",
		}
	}
	origin := u.Scheme + "://" + u.Host
	path := strings.TrimSuffix(u.Path, "/")
	return []string{
		origin + "/.well-known/oauth-authorization-server" + path,
		origin + "/.well-known/openid-configuration" + path,
		issuer + "/.well-known/openid-configuration",
		// Not in the specification's list, and tried last for that reason. The
		// appended OAuth form predates RFC 8414 settling on insertion and is
		// still what some servers publish; the list is a floor, and a server we
		// could have found is not worth refusing to be exactly three requests.
		issuer + "/.well-known/oauth-authorization-server",
	}
}

// supportsS256 reports whether a server offers the challenge method the
// specification requires a client to use "when technically capable".
func supportsS256(methods []string) bool {
	for _, m := range methods {
		if m == "S256" {
			return true
		}
	}
	return false
}

// Tokens is one issued pair, expiry resolved to a point in time.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    *time.Time
}

const oauthTimeout = 20 * time.Second

// RemoteOAuthError is the reason an authorization server gave for refusing a
// token request, in the server's own words (its `error_description`, or the
// `error` code). It is meant to be read by the person who tried to connect: a
// refusal like "the user is not eligible for MCP access" is the answer they
// need, not a generic failure the client invented over it.
type RemoteOAuthError struct {
	Reason string
}

func (e *RemoteOAuthError) Error() string {
	return "the service refused the token request: " + e.Reason
}

// Discover walks from the MCP endpoint to its authorization server's
// metadata.
//
// The order is the one the MCP specification lays down, and the first step is
// the one this used to skip: ASK THE SERVER. An unauthenticated request is
// answered with 401 and a `WWW-Authenticate` header naming where its protected
// resource metadata lives, and a client "MUST be able to parse WWW-Authenticate
// headers and respond appropriately" (MCP authorization, Authorization Server
// Location). Guessing the well-known path instead works only for a server that
// keeps its document where we happened to look, which is why the header exists.
//
// The guesses are kept as a fallback, in the order RFC 9728 defines: the
// path-scoped document, then the origin's. Failing all of it, the MCP origin is
// assumed to be its own issuer, the shape SAG's own surface has.
func Discover(ctx context.Context, mcpURL string) (*Discovery, error) {
	endpoint, err := url.Parse(mcpURL)
	if err != nil {
		return nil, fmt.Errorf("parse server url: %w", err)
	}
	origin := endpoint.Scheme + "://" + endpoint.Host

	issuer := origin
	var resource struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	challenge := challengeFrom(ctx, mcpURL)
	found := false
	if challenge.ResourceMetadata != "" {
		if err := getJSON(ctx, challenge.ResourceMetadata, &resource); err == nil &&
			len(resource.AuthorizationServers) > 0 {
			issuer = strings.TrimSuffix(resource.AuthorizationServers[0], "/")
			found = true
		}
	}
	if found {
		// nothing more to try: the server told us where to look
	} else if err := getJSON(ctx, origin+"/.well-known/oauth-protected-resource"+endpoint.Path, &resource); err == nil &&
		len(resource.AuthorizationServers) > 0 {
		issuer = strings.TrimSuffix(resource.AuthorizationServers[0], "/")
	} else if err := getJSON(ctx, origin+"/.well-known/oauth-protected-resource", &resource); err == nil &&
		len(resource.AuthorizationServers) > 0 {
		issuer = strings.TrimSuffix(resource.AuthorizationServers[0], "/")
	}

	d := &Discovery{Resource: mcpURL}
	var lastErr error
	for _, candidate := range authServerMetadataURLs(issuer) {
		if err := getJSON(ctx, candidate, d); err == nil {
			lastErr = nil
			break
		} else {
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("the service does not publish authorization metadata: %w", lastErr)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		return nil, fmt.Errorf("the service's authorization metadata names no endpoints")
	}
	// PKCE is not optional and there is no way to ask whether it is supported,
	// so the metadata is the only place it can be established: "If
	// code_challenge_methods_supported is absent, the authorization server does
	// not support PKCE and MCP clients MUST refuse to proceed."
	//
	// Refusing here rather than discovering it at the token endpoint means the
	// person is told before a browser is opened and before anybody consents to
	// anything.
	if !supportsS256(d.CodeChallengeMethods) {
		return nil, fmt.Errorf("the service does not support PKCE, which is required to connect safely")
	}
	d.Resource = mcpURL
	// A challenge may also name the scope the resource wants, which matters for
	// the servers that refuse an authorize request naming none. Only used when
	// the metadata documents said nothing.
	if d.Scope == "" && challenge.Scope != "" {
		d.Scope = challenge.Scope
	}
	return d, nil
}

// challenge is what an MCP server says when asked for something without a token.
type challenge struct {
	// ResourceMetadata is where its protected resource metadata lives, which is
	// the whole point of reading this header: it is the server's own answer to
	// "where do I look", and it need not be where a client would guess.
	ResourceMetadata string
	Scope            string
}

// challengeFrom asks the MCP endpoint for something without a token and reads
// the WWW-Authenticate header of the 401 it answers with (RFC 9728 section 5.1).
//
// Best effort by design: a server that is down, or answers something other than
// a challenge, leaves discovery to fall back on the well-known paths exactly as
// it did before. Failing here must never be the reason a connection cannot be
// made, because a server that publishes its documents where we would guess is
// still perfectly connectable.
func challengeFrom(ctx context.Context, mcpURL string) challenge {
	reqCtx, cancel := context.WithTimeout(ctx, oauthTimeout)
	defer cancel()

	// The shape a real client opens with. The point is only to be refused, and
	// a server refuses before it reads any of this, but sending something
	// well-formed keeps the request indistinguishable from an ordinary one.
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize",` +
		`"params":{"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"SAG","version":"1"}}}`)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, mcpURL, body)
	if err != nil {
		return challenge{}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return challenge{}
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))

	return parseChallenge(res.Header.Get("WWW-Authenticate"))
}

// parseChallenge reads the parameters of a Bearer challenge.
//
//	Bearer resource_metadata="https://host/.well-known/...", scope="mcp"
//
// Hand-parsed rather than split on commas: a quoted value may contain one, and
// a scope list ("mcp api") is exactly the kind of value that does.
func parseChallenge(header string) challenge {
	var out challenge
	rest := strings.TrimSpace(header)
	if i := strings.IndexByte(rest, ' '); i >= 0 && strings.EqualFold(rest[:i], "bearer") {
		rest = rest[i+1:]
	} else if rest != "" {
		return out // some other scheme; nothing here for us
	}

	for _, name := range []string{"resource_metadata", "scope"} {
		value, ok := authParam(rest, name)
		if !ok {
			continue
		}
		switch name {
		case "resource_metadata":
			out.ResourceMetadata = value
		case "scope":
			out.Scope = value
		}
	}
	return out
}

// authParam pulls one quoted parameter out of a challenge, matching the name
// only where it starts a parameter, so `error="x"` cannot answer for `x`.
func authParam(rest, name string) (string, bool) {
	for i := 0; i+len(name) < len(rest); i++ {
		if !strings.HasPrefix(rest[i:], name) {
			continue
		}
		if i > 0 && !isParamBoundary(rest[i-1]) {
			continue
		}
		after := strings.TrimLeft(rest[i+len(name):], " ")
		if !strings.HasPrefix(after, "=") {
			continue
		}
		after = strings.TrimLeft(after[1:], " ")
		if !strings.HasPrefix(after, `"`) {
			// Unquoted values are legal in the grammar and rare here; take up
			// to the next separator.
			if end := strings.IndexAny(after, ", "); end >= 0 {
				return after[:end], true
			}
			return after, true
		}
		if end := strings.IndexByte(after[1:], '"'); end >= 0 {
			return after[1 : 1+end], true
		}
	}
	return "", false
}

func isParamBoundary(c byte) bool { return c == ' ' || c == ',' }

// ErrNoClientRegistration says this service will not issue a client of its own,
// so somebody has to create the application on its side and enter the
// credentials here.
//
// A named error rather than a sentence, because the caller has to tell it apart
// from a network failure: the two need opposite things from the person reading
// them, and the API used to render both as "the service could not be reached".
var ErrNoClientRegistration = errors.New(
	"this service does not register clients itself; create the application on its side " +
		"and enter the client id (and secret, if it issues one) on this connection")

// Register introduces SAG to the remote authorization server (dynamic client
// registration). Returns the issued client id, the secret for confidential
// clients, and the scope the registration granted (RFC 7591 servers answer
// with one; it is what authorize requests should ask for).
func Register(ctx context.Context, d *Discovery, redirectURI string) (clientID, clientSecret, scope string, err error) {
	if d.RegistrationEndpoint == "" {
		return "", "", "", ErrNoClientRegistration
	}
	body, err := json.Marshal(map[string]any{
		"client_name":                "SAG",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": d.RegistrationAuthMethod(),
	})
	if err != nil {
		return "", "", "", err
	}

	reqCtx, cancel := context.WithTimeout(ctx, oauthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, d.RegistrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("register with the service: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("the service refused registration (%d)", res.StatusCode)
	}
	var issued struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Scope        string `json:"scope"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&issued); err != nil {
		return "", "", "", fmt.Errorf("read registration answer: %w", err)
	}
	if issued.ClientID == "" {
		return "", "", "", fmt.Errorf("the service issued no client id")
	}
	return issued.ClientID, issued.ClientSecret, issued.Scope, nil
}

// NewVerifier mints a PKCE code verifier.
func NewVerifier() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("mint verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// AuthorizeURL builds the address the person's browser visits to consent.
func AuthorizeURL(d *Discovery, clientID, redirectURI, state, verifier string) string {
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"resource":              {d.Resource},
	}
	// Some servers refuse an authorize request that names no scope.
	if d.Scope != "" {
		q.Set("scope", d.Scope)
	}
	separator := "?"
	if strings.Contains(d.AuthorizationEndpoint, "?") {
		separator = "&"
	}
	return d.AuthorizationEndpoint + separator + q.Encode()
}

// Exchange redeems the authorization code for the first token pair.
func Exchange(ctx context.Context, d *Discovery, clientID, clientSecret, redirectURI, code, verifier string) (*Tokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
		"resource":      {d.Resource},
	}
	return tokenRequest(ctx, d.TokenEndpoint, clientID, clientSecret, form)
}

// Refresh trades the refresh token for a new pair. Servers that rotate
// refresh tokens answer with a new one; those that do not leave it empty and
// the caller keeps the old.
func Refresh(ctx context.Context, d *Discovery, clientID, clientSecret, refreshToken string) (*Tokens, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"resource":      {d.Resource},
	}
	return tokenRequest(ctx, d.TokenEndpoint, clientID, clientSecret, form)
}

func tokenRequest(ctx context.Context, endpoint, clientID, clientSecret string, form url.Values) (*Tokens, error) {
	reqCtx, cancel := context.WithTimeout(ctx, oauthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Confidential clients authenticate with HTTP Basic (client_secret_basic,
	// the OAuth default and the only method some servers accept; the CRM
	// refused the form-body variant). The client_id stays in the form too,
	// which every server tolerates and some expect.
	if clientSecret != "" {
		req.SetBasicAuth(clientID, clientSecret)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the service's token endpoint: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	var body struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("read token answer (%d): %w", res.StatusCode, err)
	}
	if res.StatusCode != http.StatusOK || body.AccessToken == "" {
		reason := body.ErrorDescription
		if reason == "" {
			reason = body.Error
		}
		if reason == "" {
			reason = fmt.Sprintf("status %d", res.StatusCode)
		}
		return nil, &RemoteOAuthError{Reason: reason}
	}

	tokens := &Tokens{AccessToken: body.AccessToken, RefreshToken: body.RefreshToken}
	if body.ExpiresIn > 0 {
		at := time.Now().UTC().Add(time.Duration(body.ExpiresIn) * time.Second)
		tokens.ExpiresAt = &at
	}
	return tokens, nil
}

func getJSON(ctx context.Context, address string, dst any) error {
	reqCtx, cancel := context.WithTimeout(ctx, oauthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", address, res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(dst)
}

// --- Client ID Metadata Document ---------------------------------------------------

// A third way to be a client, and the only one that needs no arrangement at all.
//
// Dynamic registration has us ask the server for an identity. A client entered
// by hand has a person arrange one in advance. This has neither: the client id
// IS a URL, we publish a document there describing ourselves, and the server
// reads it when somebody authorizes. Nothing is stored on either side and there
// is no secret to lose.
//
// The catch, and it is the whole reason this cannot simply replace the others:
// the AUTHORIZATION SERVER fetches that URL. An installation nobody outside can
// reach cannot publish its own, so it borrows one that is published, which is
// what makes a laptop and a deployment behave the same way here.

// ClientMetadata is what we publish about ourselves at the client id URL.
//
// Deliberately a public client: there is no secret in this flow, so asking for
// one would be asking for something we could not keep. PKCE is what protects
// the exchange, and it is on for every connection regardless.
type ClientMetadata struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope,omitempty"`
}

// NewClientMetadata describes this deployment as a client. The document's own
// address is its client_id: the draft requires the two to agree, so a server
// that fetches the URL can confirm it is reading about the client it was told
// about rather than a document pointed at from somewhere else.
func NewClientMetadata(documentURL, redirectURI, scope string) ClientMetadata {
	return ClientMetadata{
		ClientID:                documentURL,
		ClientName:              "SAG",
		ClientURI:               strings.TrimSuffix(documentURL, clientMetadataPath),
		RedirectURIs:            append([]string{redirectURI}, LoopbackRedirectURIs...),
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scope:                   scope,
	}
}

// clientMetadataPath is where the document lives, relative to this deployment.
const clientMetadataPath = "/connect/mcp/client-metadata.json"

// LoopbackRedirectURIs are where an installation on somebody's own machine is
// sent back to, and they belong in the document a deployment publishes.
//
// This is what makes ONE document serve every installation. A laptop cannot be
// fetched by an authorization server, so it borrows a published document as its
// identity; the redirect, though, is followed by the person's own BROWSER, so
// loopback is exactly right there. A document naming only the deployment's own
// redirect would be an identity no local install could actually use, because the
// specification has the authorization server match redirect URIs exactly.
//
// Concrete addresses rather than a wildcard: exact matching is the rule, and
// these are the two a local installation is served on (the desktop binds
// 127.0.0.1:8080, a working copy runs on localhost:8080).
var LoopbackRedirectURIs = []string{
	"http://127.0.0.1:8080" + mcpCallbackPath,
	"http://localhost:8080" + mcpCallbackPath,
}

// mcpCallbackPath is where a service sends the person back after they authorize.
const mcpCallbackPath = "/connect/mcp/callback"

// DefaultClientMetadataURL is the identity an installation on somebody's own
// machine presents when a service takes a URL as a client id.
//
// Compiled in, because a person running the personal edition is not going to
// configure one, and asking them to would be the friction this whole mechanism
// exists to remove. It is the same document any SAG deployment publishes about
// itself; this names the one Flexie hosts, and SAG_MCP_CLIENT_METADATA_URL
// points a deployment at its own instead.
// The ORCHESTRATOR's host, not the chat's. A chat is served by something that
// answers every unknown path with the single-page application, so this document
// fetched from there comes back as HTML and the server reading it says the
// metadata is not valid JSON. The API, and therefore this document, is on the
// host that serves the console.
const DefaultClientMetadataURL = "https://sag-admin.flexie.io" + clientMetadataPath

// ClientMetadataURL is the client id this deployment presents: its own document
// when it publishes one, and the shared one when it runs on somebody's machine.
//
// `local` is the answer from the flags (config.Local), not a guess about the
// address: an installation nobody outside can reach cannot be fetched, however
// its base URL is spelled.
func ClientMetadataURL(baseURL string, local bool, configured string) string {
	if local {
		if configured != "" {
			return configured
		}
		return DefaultClientMetadataURL
	}
	if configured != "" {
		return configured
	}
	return strings.TrimSuffix(baseURL, "/") + clientMetadataPath
}
