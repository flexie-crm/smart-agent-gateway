package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"flexie.io/sag/internal/mcpclient"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
)

// SAG as the MCP client: the app-layer glue between the connection registry,
// the wire client, and the tool projection. Secrets are opened here and only
// here; everything below the store holds sealed bytes, everything above holds
// a bearer for the length of one errand.

// mcpCallbackPath is where the remote authorization server sends the person
// back. It is registered unauthenticated: a browser mid-redirect carries no
// bearer token.
const mcpCallbackPath = "/connect/mcp/callback"

// MCPRedirectURI is the OAuth redirect this deployment registers with remote
// authorization servers.
func (a *App) MCPRedirectURI() string {
	return strings.TrimSuffix(a.Config.BaseURL, "/") + mcpCallbackPath
}

// --- bearer resolution ------------------------------------------------------------

// mcpRefresh serializes token refreshes per connection: refresh tokens
// rotate, and two calls refreshing at once would spend a token the other
// already rotated away.
var mcpRefresh sync.Map // server id -> *sync.Mutex

// MCPBearer opens the credential a connection authenticates with, refreshing
// an expired OAuth token first. It returns "" for connections that need none.
func (a *App) MCPBearer(ctx context.Context, m *model.MCPServer) (string, error) {
	switch m.AuthType {
	case model.MCPAuthNone:
		return "", nil
	case model.MCPAuthAPIKey:
		if !m.HasAPIKey() {
			return "", fmt.Errorf("the connection has no key stored")
		}
		key, err := a.Keyring.Open(m.APIKey)
		if err != nil {
			a.Log.Error().Err(err).Int64("mcp_server_id", m.ID).Msg("open mcp api key")
			return "", ErrCredentialsUnavailable
		}
		return string(key), nil
	case model.MCPAuthOAuth:
		return a.mcpOAuthBearer(ctx, m)
	default:
		return "", fmt.Errorf("unknown auth type %q", m.AuthType)
	}
}

func (a *App) mcpOAuthBearer(ctx context.Context, m *model.MCPServer) (string, error) {
	if !m.Connected() {
		return "", fmt.Errorf("the connection has not been authorized yet")
	}

	fresh := m.OAuthTokenExpires == nil || time.Until(*m.OAuthTokenExpires) > time.Minute
	if fresh {
		token, err := a.Keyring.Open(m.OAuthAccessToken)
		if err != nil {
			a.Log.Error().Err(err).Int64("mcp_server_id", m.ID).Msg("open mcp access token")
			return "", ErrCredentialsUnavailable
		}
		return string(token), nil
	}

	lock, _ := mcpRefresh.LoadOrStore(m.ID, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	// Someone may have refreshed while we waited on the lock: re-read.
	current, err := a.Store.MCPServers().GetByID(ctx, m.WorkspaceID, m.ID)
	if err != nil {
		return "", err
	}
	if current.OAuthTokenExpires == nil || time.Until(*current.OAuthTokenExpires) > time.Minute {
		token, err := a.Keyring.Open(current.OAuthAccessToken)
		if err != nil {
			return "", ErrCredentialsUnavailable
		}
		return string(token), nil
	}

	discovery, err := a.mcpDiscovery(current)
	if err != nil {
		return "", err
	}
	refreshToken, err := a.Keyring.Open(current.OAuthRefreshToken)
	if err != nil {
		return "", ErrCredentialsUnavailable
	}
	clientSecret, err := a.openOptional(current.OAuthClientSecret)
	if err != nil {
		return "", err
	}

	tokens, err := mcpclient.Refresh(ctx, discovery, current.OAuthClientID, clientSecret, string(refreshToken))
	if err != nil {
		return "", fmt.Errorf("the service refused to renew the connection: %w", err)
	}
	if tokens.RefreshToken == "" {
		// The server does not rotate refresh tokens; keep the one we hold.
		tokens.RefreshToken = string(refreshToken)
	}
	if err := a.storeMCPTokens(ctx, current.ID, tokens); err != nil {
		return "", err
	}
	return tokens.AccessToken, nil
}

func (a *App) storeMCPTokens(ctx context.Context, serverID int64, tokens *mcpclient.Tokens) error {
	access, err := a.Keyring.Seal([]byte(tokens.AccessToken))
	if err != nil {
		return fmt.Errorf("seal access token: %w", err)
	}
	refresh, err := a.Keyring.Seal([]byte(tokens.RefreshToken))
	if err != nil {
		return fmt.Errorf("seal refresh token: %w", err)
	}
	return a.Store.MCPServers().SetTokens(ctx, serverID, access, refresh, tokens.ExpiresAt)
}

func (a *App) openOptional(sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	value, err := a.Keyring.Open(sealed)
	if err != nil {
		return "", ErrCredentialsUnavailable
	}
	return string(value), nil
}

func (a *App) mcpDiscovery(m *model.MCPServer) (*mcpclient.Discovery, error) {
	if len(m.OAuthMetadata) == 0 {
		return nil, fmt.Errorf("the connection has no cached authorization metadata; connect it again")
	}
	d := &mcpclient.Discovery{}
	if err := json.Unmarshal(m.OAuthMetadata, d); err != nil {
		return nil, fmt.Errorf("read cached authorization metadata: %w", err)
	}
	return d, nil
}

// --- the OAuth connect dance --------------------------------------------------------

// mcpConnectState is the sealed round-trip cookie of the connect dance: it
// carries the PKCE verifier through the person's browser without trusting it.
type mcpConnectState struct {
	ServerID    int64     `json:"server_id"`
	WorkspaceID int64     `json:"workspace_id"`
	UserID      int64     `json:"user_id"`
	Verifier    string    `json:"verifier"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// BeginMCPConnect walks discovery, registers SAG with the remote
// authorization server when it has to, and returns the address the person's
// browser should visit to consent.
func (a *App) BeginMCPConnect(ctx context.Context, workspaceID, userID, serverID int64) (string, error) {
	m, err := a.Store.MCPServers().GetByID(ctx, workspaceID, serverID)
	if err != nil {
		return "", err
	}
	if m.AuthType != model.MCPAuthOAuth {
		return "", fmt.Errorf("the connection does not use OAuth")
	}

	discovery, err := mcpclient.Discover(ctx, m.URL)
	if err != nil {
		return "", err
	}
	// The scope to ask for, best knowledge first: what an earlier
	// registration was granted (cached), what a fresh registration grants,
	// or everything the server advertises. Some servers refuse an authorize
	// request that names no scope at all.
	if cached, err := a.mcpDiscovery(m); err == nil && cached.Scope != "" {
		discovery.Scope = cached.Scope
	}

	// How to be a client, in the order the specification lays down: what somebody
	// pre-registered, then a metadata document, then dynamic registration, and
	// only then asking a person to go and arrange something.
	//
	// The order used to put dynamic registration first, on the reasoning that it
	// is the one that works from a machine nobody outside can reach. That
	// reasoning was mine and the specification's is the opposite: a document is
	// now the SHOULD and registration is a MAY kept "for backwards
	// compatibility". A pre-registered client comes before both because somebody
	// arranged it deliberately, and nothing we choose automatically should
	// quietly replace a decision that was made on purpose.
	clientID := m.OAuthClientID
	registered := false
	var sealedSecret []byte
	needsRegistration := false
	switch {
	case clientID != "" && discovery.RegistrationEndpoint == "":
		// Arranged by hand on the service's side, and there is no way to mint
		// another. Reused as it is.
		//
		// The specification puts a client we already hold first outright. This
		// narrows that to the case where we cannot register, deliberately: a
		// client WE minted dynamically may hold a secret that is gone or wrong,
		// a secret is returned exactly once, and a reconnect that reuses it can
		// never complete. Where registration is available, starting again is
		// free and always works, so recovery beats reuse.
	case discovery.ClientIDMetadataDocument:
		// The client id is a URL to a document describing us, which the server
		// reads when the person authorizes. Nothing is arranged in advance and
		// there is no secret. A deployment publishes its own document; a working
		// copy or the personal edition borrows the published one, because
		// nothing outside can fetch a laptop.
		clientID = mcpclient.ClientMetadataURL(
			a.Config.BaseURL, a.Config.Local(), a.Config.MCPClientMetadataURL)
	case discovery.RegistrationEndpoint != "":
		needsRegistration = true
	default:
		// Nothing left but to ask, which is the specification's last resort too.
		return "", mcpclient.ErrNoClientRegistration
	}
	if needsRegistration {
		issued, secret, granted, err := mcpclient.Register(ctx, discovery, a.MCPRedirectURI())
		if err != nil {
			return "", err
		}
		clientID, registered = issued, true
		if granted != "" {
			discovery.Scope = granted
		}
		if secret != "" {
			if sealedSecret, err = a.Keyring.Seal([]byte(secret)); err != nil {
				return "", fmt.Errorf("seal client secret: %w", err)
			}
		}
	}
	if discovery.Scope == "" {
		discovery.Scope = strings.Join(discovery.ScopesSupported, " ")
	}

	// Persist what we learned BEFORE the redirect: the callback needs the
	// endpoints and the client, and it must not depend on re-discovery. A
	// reused hand-entered client keeps its credentials; writing them again with
	// an empty secret would erase what we hold and break the exchange.
	//
	// A client id we DERIVED has to be written too, and forgetting that is what
	// broke this: a metadata-document client is worked out here and never
	// stored, so the callback exchanged the code with no client_id at all and
	// the service answered "client authentication required" for a client it had
	// just created from our document. It is written without a secret because
	// this flow has none, which is also why it cannot erase one.
	metadata, err := json.Marshal(discovery)
	if err != nil {
		return "", err
	}
	switch {
	case registered:
		if err := a.Store.MCPServers().SetOAuthClient(ctx, m.ID, clientID, sealedSecret, metadata); err != nil {
			return "", err
		}
	case clientID != m.OAuthClientID:
		if err := a.Store.MCPServers().SetOAuthClient(ctx, m.ID, clientID, nil, metadata); err != nil {
			return "", err
		}
	default:
		if err := a.Store.MCPServers().SetOAuthMetadata(ctx, m.ID, metadata); err != nil {
			return "", err
		}
	}

	verifier, err := mcpclient.NewVerifier()
	if err != nil {
		return "", err
	}
	state, err := a.sealMCPState(mcpConnectState{
		ServerID:    m.ID,
		WorkspaceID: workspaceID,
		UserID:      userID,
		Verifier:    verifier,
		ExpiresAt:   time.Now().UTC().Add(10 * time.Minute),
	})
	if err != nil {
		return "", err
	}
	return mcpclient.AuthorizeURL(discovery, clientID, a.MCPRedirectURI(), state, verifier), nil
}

// CompleteMCPConnect finishes the dance: it redeems the code, seals the
// tokens, and runs the first sync so connecting ends with tools on the
// table, not with a silent success.
//
// However it ends, the person who started it is told over their socket. The
// console cannot see the end of this for itself: the consent happens in a
// browser, and the page that lands back here has no way to reach the window
// that sent it away. In the desktop application that browser is a different
// application entirely, and the console would otherwise wait for something
// that was never coming.
func (a *App) CompleteMCPConnect(ctx context.Context, state, code string) (*model.MCPServer, model.MCPSyncResult, error) {
	claim, err := a.openMCPState(state)
	if err != nil {
		// A state that will not open names nobody, so there is nobody to tell.
		return nil, model.MCPSyncResult{}, err
	}
	m, result, err := a.completeMCPConnect(ctx, claim, code)
	a.WS.Notify(claim.WorkspaceID, claim.UserID, mcpConnectNotice(claim, m, result, err))
	return m, result, err
}

func (a *App) completeMCPConnect(ctx context.Context, claim mcpConnectState, code string) (*model.MCPServer, model.MCPSyncResult, error) {
	m, err := a.Store.MCPServers().GetByID(ctx, claim.WorkspaceID, claim.ServerID)
	if err != nil {
		return nil, model.MCPSyncResult{}, err
	}
	discovery, err := a.mcpDiscovery(m)
	if err != nil {
		return nil, model.MCPSyncResult{}, err
	}
	clientSecret, err := a.openOptional(m.OAuthClientSecret)
	if err != nil {
		return nil, model.MCPSyncResult{}, err
	}

	tokens, err := mcpclient.Exchange(ctx, discovery, m.OAuthClientID, clientSecret,
		a.MCPRedirectURI(), code, claim.Verifier)
	if err != nil {
		return nil, model.MCPSyncResult{}, err
	}
	if err := a.storeMCPTokens(ctx, m.ID, tokens); err != nil {
		return nil, model.MCPSyncResult{}, err
	}

	result, err := a.SyncMCPServer(ctx, claim.WorkspaceID, m.ID)
	if err != nil {
		// The connection IS authorized; only the first sync failed. Say so:
		// the tokens are stored and a manual sync can finish the job.
		a.Log.Error().Err(err).Int64("mcp_server_id", m.ID).Msg("first sync after connect")
		return m, model.MCPSyncResult{}, nil
	}
	return m, result, nil
}

// mcpConnectNotice is what the console is told, in the words it will show.
//
// It is built here rather than at the socket because the outcome is only fully
// known here: whether the tokens took, and whether the first sync found
// anything worth mentioning. The connection's id is always present, so a screen
// can act on it even when the failure was early enough that nothing else is.
func mcpConnectNotice(claim mcpConnectState, m *model.MCPServer, result model.MCPSyncResult, err error) map[string]any {
	notice := map[string]any{"id": claim.ServerID, "connected": err == nil}
	if m != nil {
		notice["name"] = m.Name
	}
	switch {
	case err != nil:
		notice["detail"] = MCPConnectReason(err)
	case result.Offered > 0:
		notice["detail"] = "Its tools are now available to grant."
	default:
		notice["detail"] = "No tools were found yet; use Sync."
	}
	return map[string]any{"type": "mcp_connection", "payload": notice}
}

// MCPConnectReason turns a failed connection into something worth reading.
//
// When the remote gave a reason of its own ("the user is not eligible for MCP
// access"), that reason IS the answer and a generic "try again" printed over it
// is a step backwards. Otherwise there is nothing to add but what to do next.
func MCPConnectReason(err error) string {
	var remote *mcpclient.RemoteOAuthError
	if errors.As(err, &remote) && remote.Reason != "" {
		return remote.Reason
	}
	return "The connection could not be completed. Start it again from the console."
}

func (a *App) sealMCPState(claim mcpConnectState) (string, error) {
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

func (a *App) openMCPState(state string) (mcpConnectState, error) {
	var claim mcpConnectState
	// Three different failures, said apart. They only ever reach the log (the
	// person is told the same thing either way), and one line of it is the
	// difference between a diagnosis and an afternoon: "the state is not ours"
	// is true of a mangled parameter, of a replay, and of a consent that came
	// back to a DIFFERENT gateway on this machine, which is the one that
	// actually happened.
	sealed, err := base64.RawURLEncoding.DecodeString(state)
	if err != nil {
		return claim, fmt.Errorf("the state is not ours: not the encoding we mint: %w", err)
	}
	raw, err := a.Keyring.Open(sealed)
	if err != nil {
		return claim, fmt.Errorf(
			"the state is not ours: sealed with a different key, so it was minted by another installation "+
				"(check that the redirect address points back at THIS gateway): %w", err)
	}
	if err := json.Unmarshal(raw, &claim); err != nil {
		return claim, fmt.Errorf("the state is not ours: it opened but held nothing we recognise: %w", err)
	}
	if time.Now().UTC().After(claim.ExpiresAt) {
		return claim, fmt.Errorf("the connect attempt expired; start again")
	}
	return claim, nil
}

// --- sync -----------------------------------------------------------------------

// mcpNameSeparator joins the connection's prefix to the remote's own name.
//
// An underscore, because a dot is not a character a model API accepts in a tool
// name and this used to be a dot. Everything a remote offered was therefore
// projected under a name no vendor would take, and since the tools go up as one
// array, a single connection made EVERY tool of every agent that carried one
// unusable: a 400 on every turn (KB/29).
const mcpNameSeparator = "_"

// projectedToolName is what a model will see for one remote tool.
//
// The prefix is ours and already an ordinary slug; the remote's name is not
// ours and is rewritten into the alphabet a model accepts. Nothing is lost by
// that: the name we CALL is kept separately (`remote_name`), and this is only
// the name the conversation uses.
//
// Length is the one thing that cannot be repaired, so it is reported instead:
// shortening is guesswork and two names shortened alike would become one tool.
func projectedToolName(prefix, remote string) (string, error) {
	name := tool.UsableName(prefix) + mcpNameSeparator + tool.UsableName(remote)
	if err := tool.ValidName(name); err != nil {
		return "", err
	}
	return name, nil
}

// UnprefixedToolName is projectedToolName read backwards: the name without the
// namespace the connection put on the front of it.
//
// The prefix exists so two services can each offer a `query` and stay two
// tools. That is a fact about the registry, not about the tool, and a console
// that groups the catalogue by connection is already saying which service this
// came from: repeating it on every row asks somebody to decode `nli_` to read
// a name the heading above already gave them.
//
// Display only. Nothing is renamed by this: the model still calls the tool by
// its full name, and every grant, approval and transcript still records it.
func UnprefixedToolName(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return strings.TrimPrefix(name, tool.UsableName(prefix)+mcpNameSeparator)
}

// SyncMCPServer connects, lists the remote's tools, and reconciles the
// projection. The outcome, error included, is recorded on the connection:
// a remote that stopped answering must say so where an administrator looks.
func (a *App) SyncMCPServer(ctx context.Context, workspaceID, serverID int64) (model.MCPSyncResult, error) {
	m, err := a.Store.MCPServers().GetByID(ctx, workspaceID, serverID)
	if err != nil {
		return model.MCPSyncResult{}, err
	}

	result, err := a.syncMCPServer(ctx, m)
	now := time.Now().UTC()
	message := ""
	if err != nil {
		message = err.Error()
	}
	if stateErr := a.Store.MCPServers().SetSyncState(ctx, m.ID, now, message); stateErr != nil {
		a.Log.Error().Err(stateErr).Int64("mcp_server_id", m.ID).Msg("record sync state")
	}
	return result, err
}

func (a *App) syncMCPServer(ctx context.Context, m *model.MCPServer) (model.MCPSyncResult, error) {
	bearer, err := a.MCPBearer(ctx, m)
	if err != nil {
		return model.MCPSyncResult{}, err
	}
	session, err := mcpclient.Dial(ctx, m.URL, bearer)
	if err != nil {
		return model.MCPSyncResult{}, err
	}
	defer func() { _ = session.Close() }()

	remote, err := session.ListTools(ctx)
	if err != nil {
		return model.MCPSyncResult{}, err
	}

	skipped := 0
	offered := make([]*model.Tool, 0, len(remote))
	// Two remote names can be rewritten into one (`a.b` and `a-b` both become
	// `a_b`), and the registry holds one name per workspace. Merging them would
	// silently point one tool's calls at the other, so the second is refused
	// like any other tool that cannot be projected.
	taken := make(map[string]string, len(remote))
	for _, rt := range remote {
		name, nameErr := projectedToolName(m.ToolPrefix, rt.Name)
		if nameErr == nil {
			if first, clash := taken[name]; clash {
				nameErr = fmt.Errorf("it would be named %q, which %q already is", name, first)
			} else {
				taken[name] = rt.Name
			}
		}
		if nameErr != nil {
			// Counted, so the console says "N could not be projected" rather
			// than quietly offering fewer tools than the remote has.
			skipped++
			a.Log.Warn().Err(nameErr).Str("tool", rt.Name).Int64("mcp_server_id", m.ID).
				Msg("a remote tool could not be given a name a model accepts")
			continue
		}
		friendly := rt.Title
		if friendly == "" {
			friendly = rt.Name
		}
		offered = append(offered, &model.Tool{
			Name:         name,
			FriendlyName: friendly,
			Description:  rt.Description,
			InputSchema:  rt.InputSchema,
			// The remote's own hints are untrusted claims; every projected
			// tool talks to the outside world, and that is what the risk says.
			Risk:           string(tool.RiskExternalCommunication),
			RemoteName:     rt.Name,
			DefinitionHash: rt.DefinitionHash,
		})
	}

	result, err := a.Store.Tools().SyncMCPTools(ctx, m.WorkspaceID, m.ID, offered)
	if err != nil {
		return model.MCPSyncResult{}, err
	}
	result.Skipped = skipped
	return result, nil
}

// --- the proxy handler -----------------------------------------------------------

// mcpToolHandler backs one projected tool for one turn. A remote failure is
// an answer the model can read, never a dead turn; the raw cause goes to the
// log.
func (a *App) mcpToolHandler(serverID int64, remoteName string) tool.Handler {
	return func(ctx context.Context, call tool.Call) (tool.Result, error) {
		m, err := a.Store.MCPServers().GetByID(ctx, call.WorkspaceID, serverID)
		if err != nil {
			return tool.Result{}, fmt.Errorf("load mcp connection: %w", err)
		}
		if m.Status != model.StatusActive {
			return toolAnswer(map[string]any{
				"success": false,
				"error":   "this connection is switched off",
			})
		}
		bearer, err := a.MCPBearer(ctx, m)
		if err != nil {
			a.Log.Error().Err(err).Int64("mcp_server_id", m.ID).Msg("mcp bearer")
			return toolAnswer(map[string]any{
				"success": false,
				"error":   "the service's credentials are not usable; an administrator has to reconnect it",
			})
		}
		session, err := mcpclient.Dial(ctx, m.URL, bearer)
		if err != nil {
			a.Log.Error().Err(err).Int64("mcp_server_id", m.ID).Msg("mcp dial")
			return toolAnswer(map[string]any{
				"success": false,
				"error":   "the service did not answer",
			})
		}
		defer func() { _ = session.Close() }()

		content, isError, err := session.CallTool(ctx, remoteName, call.Args)
		if err != nil {
			a.Log.Error().Err(err).Int64("mcp_server_id", m.ID).Str("tool", remoteName).Msg("mcp call")
			return toolAnswer(map[string]any{
				"success": false,
				"error":   "the service could not run this action",
			})
		}
		if isError {
			return toolAnswer(map[string]any{"success": false, "error": content})
		}
		return toolAnswer(map[string]any{"success": true, "result": json.RawMessage(mustJSONString(content))})
	}
}

func toolAnswer(payload map[string]any) (tool.Result, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Content: raw}, nil
}

// mustJSONString wraps plain text as a JSON string; content that already is
// JSON passes through unwrapped, so structured answers stay structured.
func mustJSONString(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return trimmed
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return `""`
	}
	return string(raw)
}
