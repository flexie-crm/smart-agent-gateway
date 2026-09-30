package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"flexie.io/sag/internal/oauthclient"
	"flexie.io/sag/internal/tools/apitool"
	"flexie.io/sag/internal/tools/template"
)

// Connecting a custom tool to a service that authenticates a PERSON.
//
// The same dance the MCP client already does, and deliberately the same shape:
// begin, consent in a browser, come back to a callback carrying a sealed state.
// What differs is only what is being connected and where the tokens are kept.
//
// WHY A PERSON AT ALL. The other grants need nobody: an API key is typed once,
// and client credentials prove the TOOL. This proves a person, so what the
// assistant may do at the service is exactly what the administrator who
// consented may do, and the service's own audit log says their name. That is
// the reason to want it and also the reason it cannot be automated.
//
// WHERE THE TOKENS LIVE. In the tool's own config, under `grant`, which is the
// per-instance JSON payload a custom tool already has. They are kept apart from
// the rest of it by rule rather than by table: the form cannot write that key
// (apitool.Config deletes it), an edit carries the whole subtree forward
// untouched (carryGrant), and a renewal writes only the config (Tools().
// SetConfig), so nobody is stamped as having edited a tool they never opened.

// toolCallbackPath is where a service returns the person after they consent.
//
// ONE address for every custom tool, with which tool it is carried in the
// sealed state rather than in the path. That is not tidiness: the address is
// registered at the service BY HAND, and an address per tool would mean
// registering a new one every time somebody adds a tool, which is the step
// that would stop people using this at all.
const toolCallbackPath = "/connect/tool/callback"

// ToolRedirectURI is the redirect this deployment registers with a service for
// its custom API tools. Exactly this string: OAuth compares it character for
// character, so a missing scheme or a trailing slash is a refusal at the
// consent screen and nowhere earlier.
func (a *App) ToolRedirectURI() string {
	return strings.TrimSuffix(a.Config.BaseURL, "/") + toolCallbackPath
}

// toolConnectState is the sealed round-trip claim: it leaves here as an opaque
// `state` parameter and comes back untouched.
//
// It carries the PKCE verifier, which is the one thing that must survive the
// round trip and must never travel to the authorization server: the server sees
// only its hash, and a code intercepted without the verifier is worthless. It
// is not kept in a table because it is needed exactly once, by the request that
// comes back, and a table of half-finished consents is a table somebody has to
// clean up.
type toolConnectState struct {
	ToolID      int64     `json:"tool_id"`
	WorkspaceID int64     `json:"workspace_id"`
	UserID      int64     `json:"user_id"`
	Verifier    string    `json:"verifier"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// connectWindow is how long a consent may take. Long enough to sign in and read
// what is being asked; short enough that a state parameter found later is dead.
const connectWindow = 15 * time.Minute

// BeginToolConnect returns the address the person's browser should visit.
//
// Nothing is written here. A consent that is started and abandoned must leave
// no trace, so the only thing that survives this call is the sealed state in
// the URL, and that expires on its own.
func (a *App) BeginToolConnect(ctx context.Context, workspaceID, toolID, userID int64) (string, error) {
	settings, _, err := a.toolSettings(ctx, workspaceID, toolID)
	if err != nil {
		return "", err
	}
	if settings.Auth.Kind != apitool.AuthAuthorizationCode {
		return "", fmt.Errorf("this tool does not sign in to a service, so there is nothing to connect")
	}

	verifier, err := oauthclient.NewVerifier()
	if err != nil {
		return "", fmt.Errorf("the verifier could not be made: %w", err)
	}
	state, err := a.sealToolState(toolConnectState{
		ToolID:      toolID,
		WorkspaceID: workspaceID,
		UserID:      userID,
		Verifier:    verifier,
		ExpiresAt:   time.Now().UTC().Add(connectWindow),
	})
	if err != nil {
		return "", err
	}
	found, err := a.discoveryFor(ctx, settings.Auth, settings.BaseURL)
	if err != nil {
		return "", err
	}
	return oauthclient.AuthorizeURL(found, settings.Auth.ClientID,
		a.ToolRedirectURI(), state, verifier, authorizeExtras(settings.Auth)), nil
}

// CompleteToolConnect exchanges the code for tokens and keeps them.
//
// The claim is the authentication: it was minted here, sealed with this
// installation's key, and carries which tool and which person. A browser
// mid-redirect has no bearer token, so there is nothing else it could be.
func (a *App) CompleteToolConnect(ctx context.Context, claim toolConnectState, code string) error {
	settings, opened, err := a.toolSettings(ctx, claim.WorkspaceID, claim.ToolID)
	if err != nil {
		return err
	}
	if settings.Auth.Kind != apitool.AuthAuthorizationCode {
		return fmt.Errorf("this tool does not sign in to a service")
	}

	found, err := a.discoveryFor(ctx, settings.Auth, settings.BaseURL)
	if err != nil {
		return err
	}
	tokens, err := oauthclient.Exchange(ctx, found, settings.Auth.ClientID,
		settings.Auth.ClientSecret, a.ToolRedirectURI(), code, claim.Verifier,
		clientAuthFor(settings.Auth))
	if err != nil {
		return err
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return fmt.Errorf("the service completed the sign-in without issuing a token")
	}

	now := time.Now().UTC()
	grant := apitool.Grant{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresAt:    tokens.ExpiresAt,
		ConnectedAt:  &now,
		ConnectedBy:  claim.UserID,
	}
	// Who consented, frozen beside the id: the service's audit log will show
	// their name, and so should ours after they have left.
	if person, err := a.Store.Users().GetByID(ctx, claim.UserID); err == nil && person != nil {
		grant.ConnectedByName = person.Name
		if grant.ConnectedByName == "" {
			grant.ConnectedByName = person.Email
		}
	}
	return a.writeGrant(ctx, claim.WorkspaceID, claim.ToolID, opened, grant)
}

// DisconnectTool takes the sign-in away and leaves the tool.
//
// Revoking access is not deleting a configuration: the addresses and the client
// stay, so somebody can connect again without filling the form in twice.
func (a *App) DisconnectTool(ctx context.Context, workspaceID, toolID int64) error {
	_, opened, err := a.toolSettings(ctx, workspaceID, toolID)
	if err != nil {
		return err
	}
	return a.writeGrant(ctx, workspaceID, toolID, opened, apitool.Grant{})
}

// toolSettings reads a tool: its settings with every secret opened, and its
// config exactly as STORED, still sealed.
//
// Both, because the two are for different things and mixing them up is a
// plaintext credential in the database. The opened settings are what a caller
// reads. The sealed config is what a caller writes back to, so every value it
// did not touch goes back exactly as it came out.
func (a *App) toolSettings(ctx context.Context, workspaceID, toolID int64) (apitool.Settings, json.RawMessage, error) {
	row, err := a.Store.Tools().GetByID(ctx, workspaceID, toolID)
	if err != nil {
		return apitool.Settings{}, nil, err
	}
	opened, err := a.openConfigSecrets(row.Config)
	if err != nil {
		return apitool.Settings{}, nil, fmt.Errorf("this tool's settings could not be read: %w", err)
	}
	settings, err := apitool.Parse(opened)
	if err != nil {
		return apitool.Settings{}, nil, err
	}
	return settings, row.Config, nil
}

// writeGrant replaces the `grant` subtree of a tool's config and leaves every
// other key exactly as it was.
//
// It works on the config as STORED, never on an opened one. Writing back an
// opened config would put every other secret the tool holds into the database
// in plaintext, and it would do it quietly, because nothing downstream
// distinguishes a value that was never sealed from one that was opened.
// Untouched keys go back byte for byte.
//
// The tokens are sealed HERE rather than through SecretPaths, because sealing
// by path runs on save and this is not a save: a value already sealed would be
// sealed a second time. Opening needs no arrangement, because that walks the
// whole config on the marker.
//
// An empty grant removes the key rather than writing an empty object, so a
// disconnected tool reads as one that was never connected, which is what it is.
func (a *App) writeGrant(ctx context.Context, workspaceID, toolID int64, stored json.RawMessage, grant apitool.Grant) error {
	var cfg map[string]any
	if err := json.Unmarshal(stored, &cfg); err != nil {
		return fmt.Errorf("read this tool's settings: %w", err)
	}
	if !grant.Held() {
		delete(cfg, apitool.GrantPath)
	} else {
		sealedAccess, err := a.sealString(grant.AccessToken)
		if err != nil {
			return err
		}
		sealedRefresh := ""
		if strings.TrimSpace(grant.RefreshToken) != "" {
			if sealedRefresh, err = a.sealString(grant.RefreshToken); err != nil {
				return err
			}
		}
		held := apitool.Grant{
			AccessToken: sealedAccess, RefreshToken: sealedRefresh,
			ExpiresAt: grant.ExpiresAt, ConnectedAt: grant.ConnectedAt,
			ConnectedBy: grant.ConnectedBy, ConnectedByName: grant.ConnectedByName,
		}
		// The token fields here hold SEALED values, which is the whole purpose
		// of the lines above: `held` is built from sealedAccess and
		// sealedRefresh, never from the plaintext arguments. Marshalling
		// ciphertext into the config is what writing a kept sign-in IS.
		//nolint:gosec // G117: access_token and refresh_token are already sealed at this point
		raw, err := json.Marshal(held)
		if err != nil {
			return err
		}
		var asMap map[string]any
		if err := json.Unmarshal(raw, &asMap); err != nil {
			return err
		}
		cfg[apitool.GrantPath] = asMap
	}
	written, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return a.Store.Tools().SetConfig(ctx, workspaceID, toolID, written)
}

func (a *App) sealString(plain string) (string, error) {
	sealed, err := a.Keyring.Seal([]byte(plain))
	if err != nil {
		return "", fmt.Errorf("seal the sign-in: %w", err)
	}
	return sealMarker + base64.StdEncoding.EncodeToString(sealed), nil
}

func (a *App) sealToolState(claim toolConnectState) (string, error) {
	raw, err := json.Marshal(claim)
	if err != nil {
		return "", err
	}
	sealed, err := a.Keyring.Seal(raw)
	if err != nil {
		return "", fmt.Errorf("seal connect state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// OpenToolState reads a state parameter back, refusing one that is not ours.
//
// Three failures said apart, because they only ever reach the log and one line
// of it is the difference between a diagnosis and an afternoon: a mangled
// parameter, a replay, and a consent that came back to a DIFFERENT gateway on
// this machine, which is the one that actually happens.
func (a *App) OpenToolState(state string) (toolConnectState, error) {
	var claim toolConnectState
	sealed, err := base64.RawURLEncoding.DecodeString(state)
	if err != nil {
		return claim, fmt.Errorf("the state is not ours: not the encoding we mint: %w", err)
	}
	raw, err := a.Keyring.Open(sealed)
	if err != nil {
		return claim, fmt.Errorf("the state is not ours: sealed with a different key, so it was minted by "+
			"another installation (check that the callback address points back at THIS gateway): %w", err)
	}
	if err := json.Unmarshal(raw, &claim); err != nil {
		return claim, fmt.Errorf("the state is not ours: it opened but held nothing we recognise: %w", err)
	}
	if time.Now().UTC().After(claim.ExpiresAt) {
		return claim, fmt.Errorf("that sign-in took too long and has expired; start it again")
	}
	return claim, nil
}

// discoveryFor answers where a service's sign-in lives: asking the SERVICE
// first, and the administrator only for what it did not say.
//
// This is the difference between three fields and five. An MCP connection is
// one address because the protocol requires a metadata document AND lets us
// register a client; an ordinary service publishes the first often enough to be
// worth asking for (box, Google, Microsoft and Slack all do) and the second
// never, which is why the client id and secret are always typed and the two
// addresses usually are not.
//
// What was typed always wins: a service can publish one thing and document
// another, and the person configuring the tool is the one who knows which is
// right for their account. A service that publishes nothing and was given no
// addresses is answered with a message naming the two fields to fill, not with
// a failure somebody has to decode.
func (a *App) discoveryFor(ctx context.Context, auth apitool.Auth, baseURL string) (*oauthclient.Discovery, error) {
	if auth.AuthorizeURL != "" && auth.TokenURL != "" {
		return &oauthclient.Discovery{
			AuthorizationEndpoint: auth.AuthorizeURL,
			TokenEndpoint:         auth.TokenURL,
			Scope:                 auth.Scope,
		}, nil
	}

	found, err := oauthclient.DiscoverEndpoints(ctx, baseURL)
	if err != nil {
		a.Log.Info().Err(err).Str("base_url", baseURL).Msg("discover tool sign-in endpoints")
		return nil, fmt.Errorf("this service does not publish where its sign-in lives, so the " +
			"sign-in address and the token address have to be filled in on this tool. The " +
			"service's own API documentation names them")
	}
	if auth.AuthorizeURL != "" {
		found.AuthorizationEndpoint = auth.AuthorizeURL
	}
	if auth.TokenURL != "" {
		found.TokenEndpoint = auth.TokenURL
	}
	if auth.Scope != "" {
		found.Scope = auth.Scope
	}
	return found, nil
}

// authorizeExtras turns the pairs an administrator typed into parameters for
// the sign-in address.
//
// Typed, because a service that is not an MCP server publishes no metadata
// saying what it wants, and what it wants is often not optional. Google issues
// no refresh token unless the sign-in carries access_type=offline: a connection
// made without it works for an hour and then cannot be renewed at all.
func authorizeExtras(auth apitool.Auth) url.Values {
	if len(auth.AuthorizePairs) == 0 {
		return nil
	}
	extra := url.Values{}
	for _, pair := range auth.AuthorizePairs {
		extra.Add(pair.Name, pair.Value)
	}
	return extra
}

// clientAuthFor is how this tool proves itself at the token endpoint.
//
// The administrator's choice, because a service that publishes no metadata
// cannot be asked. Basic unless they said the body, which is the OAuth default
// and what the specification says to assume.
func clientAuthFor(auth apitool.Auth) oauthclient.ClientAuth {
	if auth.Credentials == apitool.CredentialsInBody {
		return oauthclient.ClientSecretPost
	}
	return oauthclient.ClientSecretBasic
}

// earlyForTool is how long before expiry a token is replaced, so one does not
// expire mid-flight and cost a call somebody has to understand.
const earlyForTool = time.Minute

// toolGrant is one tool's sign-in as a template sees it, and it is where a
// stale token is replaced.
type toolGrant struct {
	app         *App
	toolID      int64
	workspaceID int64
	// settings is the tool as it was bound. Its Grant is this binding's copy of
	// the sign-in and follows every renewal, under mu: a second call in the same
	// turn must use the token the first one renewed, not renew again from a
	// copy whose refresh token is already spent.
	mu       sync.Mutex
	settings apitool.Settings
}

// errNotSignedIn is a tool that needs a person's sign-in and has none.
var errNotSignedIn = errors.New("this tool has not been connected to the service yet, so there is no sign-in to use")

// Token answers with a usable access token, renewing a stale one.
func (g *toolGrant) Token(ctx context.Context) (string, error) {
	grant := g.known()
	if !grant.Held() {
		return "", errNotSignedIn
	}
	if !nearlyOver(grant.ExpiresAt) {
		return grant.AccessToken, nil
	}
	return g.renew(ctx, "")
}

// Renew answers with a token to use instead of one the service refused.
func (g *toolGrant) Renew(ctx context.Context, refused string) (string, error) {
	return g.renew(ctx, refused)
}

func (g *toolGrant) known() apitool.Grant {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.settings.Grant
}

func (g *toolGrant) remember(grant apitool.Grant) {
	g.mu.Lock()
	g.settings.Grant = grant
	g.mu.Unlock()
}

// renew renews the sign-in, or finds that it already has been.
//
// On 24 September three calls in one turn renewed from this binding's own copy:
// the first spent refresh token rt-1 and stored the new pair, the second sent
// rt-1 again, and the service answered "Refresh token reuse detected" and
// revoked the whole sign-in. So a renewal is made from the STORED sign-in, which
// is the only one that is current, and one at a time per tool across the
// process, because the Gateway and one of its agents, or two conversations,
// each bind their own copy.
//
// The renewal is written back because a refresh token often rotates with the
// access token: keeping the old one would mean the next renewal failing with
// nobody able to say why.
func (g *toolGrant) renew(ctx context.Context, refused string) (string, error) {
	release := g.app.renewingSignIn(g.toolID)
	defer release()

	latest, stored, err := g.app.toolSettings(ctx, g.workspaceID, g.toolID)
	if err != nil {
		return "", err
	}
	grant := latest.Grant
	if !grant.Held() {
		return "", errNotSignedIn
	}
	// Renewed already, by another call or another binding, while this one
	// waited or since it was bound.
	if grant.AccessToken != refused && !nearlyOver(grant.ExpiresAt) {
		g.remember(grant)
		return grant.AccessToken, nil
	}
	if strings.TrimSpace(grant.RefreshToken) == "" {
		return "", fmt.Errorf("this tool's sign-in has expired and the service issued no way to renew it, " +
			"so it has to be connected again")
	}

	found, err := g.app.discoveryFor(ctx, latest.Auth, latest.BaseURL)
	if err != nil {
		return "", err
	}
	issued, err := oauthclient.Refresh(ctx, found,
		latest.Auth.ClientID, latest.Auth.ClientSecret, grant.RefreshToken,
		clientAuthFor(latest.Auth))
	if err != nil {
		return "", fmt.Errorf("this tool's sign-in could not be renewed: %w", err)
	}
	renewed := apitool.Grant{
		AccessToken:  issued.AccessToken,
		RefreshToken: issued.RefreshToken,
		ExpiresAt:    issued.ExpiresAt,
		// A rotation is not a new consent and must not read as one.
		ConnectedAt:     grant.ConnectedAt,
		ConnectedBy:     grant.ConnectedBy,
		ConnectedByName: grant.ConnectedByName,
	}
	if strings.TrimSpace(renewed.RefreshToken) == "" {
		// A service that returns no new refresh token means keep the old one,
		// and discarding it would end the connection at the next expiry.
		renewed.RefreshToken = grant.RefreshToken
	}
	if err := g.app.writeGrant(ctx, g.workspaceID, g.toolID, stored, renewed); err != nil {
		return "", err
	}
	g.remember(renewed)
	return renewed.AccessToken, nil
}

// renewingSignIn holds the one renewal lock of a tool until release is called.
func (a *App) renewingSignIn(toolID int64) (release func()) {
	held, _ := a.signIns.LoadOrStore(toolID, &sync.Mutex{})
	lock := held.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

// nearlyOver reports that a token is spent or close enough to it. No expiry at
// all counts as still good: a service that named none is one we cannot second
// guess, and refreshing on every call would be a request per call.
func nearlyOver(at *time.Time) bool {
	if at == nil {
		return false
	}
	return time.Now().UTC().Add(earlyForTool).After(*at)
}

// ToolConnection is what an edit form shows about a tool's sign-in: whether it
// needs one at all, and whether it has one.
//
// It exists because the answer was previously reachable only by pressing Test:
// the Connect button arrived as a side effect of a successful connection test,
// so somebody opening a tool that had never been signed in to saw no sign-in
// and no reason to look for one. A thing a tool NEEDS should be visible when
// the tool is opened.
type ToolConnection struct {
	// Needed is true when this tool's configuration is the kind a person signs
	// in to. False for every other tool, which is most of them.
	Needed bool
	Held   bool
	ByName string
	At     *time.Time
}

// ToolConnection reports the sign-in state of one tool.
//
// Answers a zero value for anything that needs no sign-in, which is not a
// failure: it is the ordinary case, and the form shows nothing.
func (a *App) ToolConnection(ctx context.Context, workspaceID, toolID int64) ToolConnection {
	row, err := a.Store.Tools().GetByID(ctx, workspaceID, toolID)
	if err != nil {
		return ToolConnection{}
	}
	tmpl, ok := a.Templates.Get(row.Template)
	if !ok {
		return ToolConnection{}
	}
	granted, ok := tmpl.(template.GrantedTemplate)
	if !ok {
		return ToolConnection{}
	}
	opened, err := a.openConfigSecrets(row.Config)
	if err != nil || !granted.Connectable(opened) {
		return ToolConnection{}
	}
	state := ToolConnection{Needed: true}
	settings, err := apitool.Parse(opened)
	if err != nil {
		return state
	}
	grant := settings.Grant
	state.Held = grant.Held()
	state.ByName, state.At = grant.ConnectedByName, grant.ConnectedAt
	return state
}

// ConnectionOffer is what to say to somebody who has just made a tool: where to
// sign in, or why that cannot be offered yet. Both empty means this tool needs
// no sign-in at all, which is the ordinary case and not a failure.
type ConnectionOffer struct {
	URL    string
	Reason string
}

// OfferConnectionFor answers the sign-in for a tool that was just created, so
// the person who filled the form can finish in the same breath.
func (a *App) OfferConnectionFor(ctx context.Context, workspaceID, toolID, userID int64) ConnectionOffer {
	row, err := a.Store.Tools().GetByID(ctx, workspaceID, toolID)
	if err != nil {
		return ConnectionOffer{}
	}
	tmpl, ok := a.Templates.Get(row.Template)
	if !ok {
		return ConnectionOffer{}
	}
	granted, ok := tmpl.(template.GrantedTemplate)
	if !ok {
		return ConnectionOffer{}
	}
	opened, err := a.openConfigSecrets(row.Config)
	if err != nil || !granted.Connectable(opened) {
		return ConnectionOffer{}
	}
	where, err := a.BeginToolConnect(ctx, workspaceID, toolID, userID)
	if err != nil {
		// The tool is made and its settings are kept. Only the sign-in could
		// not be started, so that is what is said.
		return ConnectionOffer{Reason: err.Error()}
	}
	return ConnectionOffer{URL: where}
}
