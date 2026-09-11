package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
)

// SAG as the MCP client, tested against a REAL MCP server running in-process:
// the wire protocol, the auth, the projection, the drift semantics, and the
// full agent loop calling a remote tool, happy paths and refusals alike.

// fakeRemote is a scriptable MCP server behind streamable HTTP. Its toolset
// can be swapped between syncs, which is exactly how a third party behaves.
type fakeRemote struct {
	ts  *httptest.Server
	key string

	mu     sync.Mutex
	server *sdkmcp.Server
	calls  map[string]int
	// authIssuer, when set, is served in the (public) protected-resource
	// metadata, pointing OAuth discovery at the authorization server.
	authIssuer string
}

func newFakeRemote(t *testing.T, key string) *fakeRemote {
	t.Helper()
	f := &fakeRemote{key: key, calls: map[string]int{}}
	inner := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.server
	}, nil)
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RFC 9728: the protected-resource document is public; it is HOW a
		// client finds out where to authenticate in the first place.
		if strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource") {
			f.mu.Lock()
			issuer := f.authIssuer
			f.mu.Unlock()
			if issuer == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resource": f.ts.URL, "authorization_servers": []string{issuer},
			})
			return
		}
		f.mu.Lock()
		key := f.key
		f.mu.Unlock()
		if key != "" && r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(f.ts.Close)
	return f
}

// offer replaces the remote's toolset: name -> description. Every tool
// answers with its own name and counts its calls.
func (f *fakeRemote) offer(tools map[string]string) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fake-remote", Version: "1"}, nil)
	for name, description := range tools {
		toolName := name
		server.AddTool(&sdkmcp.Tool{
			Name:        toolName,
			Description: description,
			InputSchema: map[string]any{"type": "object"},
		}, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			f.mu.Lock()
			f.calls[toolName]++
			f.mu.Unlock()
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "answer from " + toolName}},
			}, nil
		})
	}
	f.mu.Lock()
	f.server = server
	f.mu.Unlock()
}

func (f *fakeRemote) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

// createConnection registers the fake remote as a connection and returns it.
func (e *testEnv) createConnection(token, name, key string, remote *fakeRemote) mcpServerBody {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/v1/mcp-servers", token, map[string]any{
		"name": name, "url": remote.ts.URL, "auth_type": model.MCPAuthAPIKey, "api_key": key,
	})
	e.expectStatus(rec, http.StatusCreated)
	var body mcpServerBody
	e.decode(rec, &body)
	return body
}

func (e *testEnv) syncConnection(token string, id int64) model.MCPSyncResult {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/v1/mcp-servers/"+itoa(id)+"/sync", token, nil)
	e.expectStatus(rec, http.StatusOK)
	var result model.MCPSyncResult
	e.decode(rec, &result)
	return result
}

func (e *testEnv) connectionTools(token string, id int64) []model.Tool {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/v1/mcp-servers/"+itoa(id)+"/tools", token, nil)
	e.expectStatus(rec, http.StatusOK)
	var tools []model.Tool
	e.decode(rec, &tools)
	return tools
}

func TestMCPConnectionValidationAndSecrets(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// Everything wrong answers at once, field by field.
	env.expectFields(env.do(http.MethodPost, "/v1/mcp-servers", token, map[string]any{
		"name": " ", "url": "not a url", "auth_type": "telepathy",
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"name":      "a name is required",
		"url":       "not a usable address",
		"auth_type": "must be none, api_key or oauth",
	})

	remote := newFakeRemote(t, "sk-remote-secret")
	remote.offer(map[string]string{"echo": "echoes"})
	created := env.createConnection(token, "Remote", "sk-remote-secret", remote)
	if created.ToolPrefix != "remote" || !created.HasAPIKey || created.Connected {
		t.Fatalf("create did not land right: %+v", created)
	}

	// The key never comes back out, on any read.
	rec := env.do(http.MethodGet, "/v1/mcp-servers", token, nil)
	env.expectStatus(rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "sk-remote-secret") {
		t.Fatalf("the api key leaked: %s", rec.Body.String())
	}

	// An update with no key keeps the stored one.
	rec = env.do(http.MethodPut, "/v1/mcp-servers/"+itoa(created.ID), token, map[string]any{
		"name": "Remote", "url": remote.ts.URL, "auth_type": model.MCPAuthAPIKey,
		"status": model.StatusActive,
	})
	env.expectStatus(rec, http.StatusOK)
	var updated mcpServerBody
	env.decode(rec, &updated)
	if !updated.HasAPIKey {
		t.Fatal("editing the connection wiped its key")
	}

	// Two connections cannot share a name (the prefix namespace hangs off it).
	env.expectFields(env.do(http.MethodPost, "/v1/mcp-servers", token, map[string]any{
		"name": "Remote", "url": remote.ts.URL, "auth_type": model.MCPAuthNone,
	}), http.StatusConflict, "conflict", map[string]string{
		"name": "another connection already uses this name",
	})
}

func TestMCPSyncProjectsAndTracksDrift(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	remote := newFakeRemote(t, "sk-remote-secret")
	remote.offer(map[string]string{
		"create_lead":  "makes a lead",
		"find_contact": "finds a contact",
	})
	created := env.createConnection(token, "CRM", "sk-remote-secret", remote)

	// The first sync projects both tools. Inert means no agent has them: the
	// allow-lists never heard of them. It does NOT mean an approval bit of
	// their own, which they no longer carry (migration 57).
	result := env.syncConnection(token, created.ID)
	if result.Offered != 2 || result.Added != 2 {
		t.Fatalf("first sync: %+v", result)
	}
	tools := env.connectionTools(token, created.ID)
	if len(tools) != 2 {
		t.Fatalf("projection: %d tools", len(tools))
	}
	for _, tl := range tools {
		if tl.RequiresApproval || tl.Kind != "mcp" || !strings.HasPrefix(tl.Name, "crm_") {
			t.Fatalf("a projected tool arrived wrong: %+v", tl)
		}
	}

	// The remote renames find_contact and rewrites create_lead's description.
	remote.offer(map[string]string{
		"create_lead":    "makes a lead, differently",
		"search_contact": "finds a contact",
	})
	result = env.syncConnection(token, created.ID)
	if result.Added != 1 || result.Changed != 1 || result.Missing != 1 {
		t.Fatalf("drift sync: %+v", result)
	}
	byRemote := map[string]model.Tool{}
	for _, tl := range env.connectionTools(token, created.ID) {
		byRemote[tl.RemoteName] = tl
	}
	if !byRemote["find_contact"].RemoteMissing {
		t.Fatal("the vanished tool was not flagged missing")
	}
	if byRemote["create_lead"].DefinitionChangedAt == nil {
		t.Fatal("the changed definition was not stamped")
	}
	if byRemote["search_contact"].RemoteMissing || byRemote["search_contact"].RequiresApproval {
		t.Fatalf("the renamed arrival is wrong: %+v", byRemote["search_contact"])
	}
	// A changed definition is STAMPED and nothing more. It used to be held for
	// approval too, which read as a guard against a service redefining a tool
	// under us and could not be one: nothing syncs on its own, so it fired when
	// an administrator happened to press refresh.
	if byRemote["create_lead"].RequiresApproval {
		t.Fatalf("a changed tool was held for approval: %+v", byRemote["create_lead"])
	}

	// A remote that stops accepting our key is a recorded failure, not a 500.
	badRemote := newFakeRemote(t, "a-different-secret")
	badRemote.offer(map[string]string{"x": "y"})
	stranger := env.createConnection(token, "Stranger", "the-wrong-key", badRemote)
	rec := env.do(http.MethodPost, "/v1/mcp-servers/"+itoa(stranger.ID)+"/sync", token, nil)
	env.expectStatus(rec, http.StatusBadGateway)
	var listed []mcpServerBody
	rec = env.do(http.MethodGet, "/v1/mcp-servers", token, nil)
	env.expectStatus(rec, http.StatusOK)
	env.decode(rec, &listed)
	for _, s := range listed {
		if s.ID == stranger.ID && s.LastError == "" {
			t.Fatal("the refused sync left no trace where an administrator looks")
		}
	}
}

// mcpVendor scripts a model that calls the projected tool, then narrates.
func mcpVendor(t *testing.T, narration string) *fakeVendor {
	t.Helper()
	return newFakeVendor(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"remote_echo","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		},
		[]string{
			`{"choices":[{"index":0,"delta":{"content":"` + narration + `"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
	)
}

// prepareMCPTurn wires the full stage: a remote with one tool, a synced
// connection, an agent allowed to use it, and a scripted model.
//
// confirm asks the AGENT to pause on that tool, which is where a pause is
// decided for every kind of tool. A projected one used to carry an approval bit
// of its own, set by the sync (migration 57).
func prepareMCPTurn(t *testing.T, env *testEnv, token string, confirm ...string) (*fakeRemote, mcpServerBody, int64) {
	t.Helper()
	remote := newFakeRemote(t, "sk-remote-secret")
	remote.offer(map[string]string{"echo": "echoes what it is sent"})
	connection := env.createConnection(token, "Remote", "sk-remote-secret", remote)
	env.syncConnection(token, connection.ID)

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	vendor := mcpVendor(t, "It answered.")
	env.pointModelAt(modelID, vendor)

	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": "default", "name": "Gateway", "tools": []string{"remote_echo"},
		"confirm_tools": confirm,
	})
	env.expectStatus(rec, http.StatusCreated)
	return remote, connection, modelID
}

func TestMCPToolRunsInsideATurn(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	// Nothing is relaxed first: a projected tool arrives ready to run, and
	// pausing on it is the agent's decision, which this agent has not made.
	remote, _, modelID := prepareMCPTurn(t, env, token)

	frames := env.streamTurn(token, map[string]any{
		"prompt": "ask the remote", "model_id": modelID,
	})

	done := framesOfType(frames, chat.FrameTool)
	var result string
	for _, frame := range done {
		msg := toolMessage(t, frame)
		if msg.Done && msg.Name == "remote_echo" {
			result = string(msg.Output)
		}
	}
	if !strings.Contains(result, "answer from echo") {
		t.Fatalf("the remote's answer did not reach the turn: %q (frames %+v)", result, frames)
	}
	if remote.callCount("echo") != 1 {
		t.Fatalf("the remote was called %d times", remote.callCount("echo"))
	}
	if deltaText(frames) != "It answered." {
		t.Fatalf("the model did not narrate: %q", deltaText(frames))
	}
}

func TestMCPApprovalRefusesADriftedDefinition(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	// The agent is told to confirm this tool, which is the only thing that
	// pauses a projected one now. The turn parks with a card that names the
	// connected service, written by us, not by the remote.
	remote, connection, modelID := prepareMCPTurn(t, env, token, "remote_echo")
	frames := env.streamTurn(token, map[string]any{
		"prompt": "ask the remote", "model_id": modelID,
	})
	last := frames[len(frames)-1]
	if last.Type != chat.FrameConfirmRequest || !last.Final {
		t.Fatalf("the turn did not park: %+v", frames)
	}
	card := confirmRequest(t, last)
	if !strings.Contains(card.Title, "Remote") {
		t.Fatalf("the card does not name the service: %+v", card)
	}
	if remote.callCount("echo") != 0 {
		t.Fatal("the remote ran before anyone approved")
	}

	// While the card waits, the remote rewrites the tool and a sync sees it.
	remote.offer(map[string]string{"echo": "now does something else entirely"})
	env.syncConnection(token, connection.ID)

	// The approval is stale: nobody approved the NEW semantics. The resume
	// refuses, tells the model why, and the remote is never called.
	resumed := env.streamTurn(token, map[string]any{
		"resume_token": card.Token, "resume_action": "approved", "model_id": modelID,
	})
	done := framesOfType(resumed, chat.FrameTool)
	if len(done) == 0 {
		t.Fatalf("the refusal was not reported as a tool result: %+v", resumed)
	}
	msg := toolMessage(t, done[len(done)-1])
	if msg.Status != model.ToolCallFailed {
		t.Fatalf("a drifted approval must fail the call, got %q", msg.Status)
	}
	if remote.callCount("echo") != 0 {
		t.Fatal("a stale approval executed against redefined semantics")
	}
}

// A service that will not issue an OAuth client of its own.
//
// Most do (RFC 7591) and SAG registers itself. The ones that do not expect an
// administrator to create the application on their side, and until now there was
// nowhere to put what they got back: the columns existed, the connect flow
// already preferred a stored client id, and neither the API nor the form would
// accept one. The server said "enter a client id by hand" and pointed at a field
// that did not exist anywhere.
func TestAHandEnteredOAuthClientIsKept(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("mcpauth@test", "password1234", model.PermSuperuser)
	token, _ := env.login("mcpauth@test", "password1234")

	rec := env.do(http.MethodPost, "/v1/mcp-servers", token, map[string]any{
		"name": "Hand Registered", "url": "https://service.test/mcp",
		"auth_type":           model.MCPAuthOAuth,
		"oauth_client_id":     "client-from-their-console",
		"oauth_client_secret": "the-secret-they-were-shown-once",
	})
	env.expectStatus(rec, http.StatusCreated)
	var created mcpServerBody
	env.decode(rec, &created)

	if created.OAuthClientID != "client-from-their-console" {
		t.Fatalf("client id %q did not survive create", created.OAuthClientID)
	}
	// The id is not a secret and has to be visible: an administrator needs to
	// see which client a connection uses. The secret is, and must not be.
	if !created.HasOAuthClientSecret {
		t.Fatal("the client secret was not stored")
	}
	if strings.Contains(rec.Body.String(), "the-secret-they-were-shown-once") {
		t.Fatal("the client secret was echoed back; it is write-only")
	}

	// Editing something else must not wipe the secret, the same rule the API
	// key follows: renaming a connection is not consent to break it.
	rec = env.do(http.MethodPut, "/v1/mcp-servers/"+itoa(created.ID), token, map[string]any{
		"name": "Renamed", "url": "https://service.test/mcp",
		"auth_type": model.MCPAuthOAuth, "status": model.StatusActive,
		"oauth_client_id": "client-from-their-console",
	})
	env.expectStatus(rec, http.StatusOK)
	var renamed mcpServerBody
	env.decode(rec, &renamed)
	if !renamed.HasOAuthClientSecret {
		t.Fatal("renaming the connection wiped its client secret")
	}
	if renamed.OAuthClientID != "client-from-their-console" {
		t.Fatalf("client id %q lost on update", renamed.OAuthClientID)
	}

	// And it can be taken back off, which is what moves a connection to a client
	// the service issues itself. The id is an ordinary field, not a secret, so
	// empty means empty rather than "leave it alone".
	rec = env.do(http.MethodPut, "/v1/mcp-servers/"+itoa(created.ID), token, map[string]any{
		"name": "Renamed", "url": "https://service.test/mcp",
		"auth_type": model.MCPAuthOAuth, "status": model.StatusActive,
		"oauth_client_id": "",
	})
	env.expectStatus(rec, http.StatusOK)
	var cleared mcpServerBody
	env.decode(rec, &cleared)
	if cleared.OAuthClientID != "" {
		t.Fatalf("client id %q could not be cleared", cleared.OAuthClientID)
	}
}

// The refusal a person can DO something about must say so. It used to be
// rendered as "the service could not be reached for authorization", which sends
// somebody to check a network that is working perfectly.
func TestNoClientRegistrationIsReportedAsSomethingToFix(t *testing.T) {
	// A remote that publishes authorization metadata with no registration
	// endpoint: the exact shape of a service that expects a client by hand.
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ".well-known") {
			writeJSON(w, http.StatusOK, map[string]any{
				"issuer":                           "https://service.test",
				"authorization_endpoint":           "https://service.test/authorize",
				"token_endpoint":                   "https://service.test/token",
				"code_challenge_methods_supported": []string{"S256"},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer remote.Close()

	env := newTestEnv(t)
	env.createUser("mcpreg@test", "password1234", model.PermSuperuser)
	token, _ := env.login("mcpreg@test", "password1234")

	rec := env.do(http.MethodPost, "/v1/mcp-servers", token, map[string]any{
		"name": "No Registration", "url": remote.URL, "auth_type": model.MCPAuthOAuth,
	})
	env.expectStatus(rec, http.StatusCreated)
	var created mcpServerBody
	env.decode(rec, &created)

	rec = env.do(http.MethodPost, "/v1/mcp-servers/"+itoa(created.ID)+"/connect", token, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: a service that will not register clients is not a "+
			"service we failed to reach", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "client_registration_required") {
		t.Fatalf("the refusal does not name its cause: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "could not be reached") {
		t.Fatal("an actionable refusal was reported as an unreachable service")
	}
}

// The specification's own words: a client "MUST be able to parse
// WWW-Authenticate headers". A connection to a server that keeps its metadata
// somewhere a client would not guess has to work, because the header is how the
// server says where it is.
func TestAConnectionFollowsTheServersOwnPointerToItsMetadata(t *testing.T) {
	mux := http.NewServeMux()
	remote := httptest.NewServer(mux)
	defer remote.Close()

	// Deliberately NOT where the well-known guesses would look.
	mux.HandleFunc("/where/we/keep/it", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"resource":              remote.URL + "/mcp",
			"authorization_servers": []string{remote.URL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                           remote.URL,
			"authorization_endpoint":           remote.URL + "/authorize",
			"token_endpoint":                   remote.URL + "/token",
			"code_challenge_methods_supported": []string{"S256"},
			"registration_endpoint":            remote.URL + "/register",
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"client_id": "issued-on-the-spot"})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate",
			`Bearer resource_metadata="`+remote.URL+`/where/we/keep/it", scope="mcp"`)
		w.WriteHeader(http.StatusUnauthorized)
	})

	env := newTestEnv(t)
	env.createUser("mcpwww@test", "password1234", model.PermSuperuser)
	token, _ := env.login("mcpwww@test", "password1234")

	rec := env.do(http.MethodPost, "/v1/mcp-servers", token, map[string]any{
		"name": "Points Elsewhere", "url": remote.URL + "/mcp", "auth_type": model.MCPAuthOAuth,
	})
	env.expectStatus(rec, http.StatusCreated)
	var created mcpServerBody
	env.decode(rec, &created)

	rec = env.do(http.MethodPost, "/v1/mcp-servers/"+itoa(created.ID)+"/connect", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var out connectResponse
	env.decode(rec, &out)

	// It got all the way to an authorize URL, which it could only do by reading
	// the header: nothing else names that document.
	if !strings.HasPrefix(out.AuthorizeURL, remote.URL+"/authorize") {
		t.Fatalf("authorize url %q", out.AuthorizeURL)
	}
	// And the request carries what the spec requires of every client.
	for _, required := range []string{"code_challenge=", "code_challenge_method=S256", "resource=", "state="} {
		if !strings.Contains(out.AuthorizeURL, required) {
			t.Fatalf("the authorize request is missing %s: %s", required, out.AuthorizeURL)
		}
	}
}

// A client id we DERIVE has to be stored, not just used once.
//
// The metadata-document client was worked out to build the authorize URL and
// never written down, so the callback exchanged the code with no client_id at
// all and the service answered "client authentication required" for a client it
// had itself created from our document seconds earlier. The authorize step
// looked perfect; only the last hop failed.
func TestADerivedClientIDIsRememberedForTheCallback(t *testing.T) {
	mux := http.NewServeMux()
	remote := httptest.NewServer(mux)
	defer remote.Close()

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"resource":              remote.URL + "/mcp",
			"authorization_servers": []string{remote.URL},
		})
	})
	// Takes a URL as a client id, and registers nobody.
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                remote.URL,
			"authorization_endpoint":                remote.URL + "/authorize",
			"token_endpoint":                        remote.URL + "/token",
			"code_challenge_methods_supported":      []string{"S256"},
			"client_id_metadata_document_supported": true,
		})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })

	env := newTestEnv(t)
	env.createUser("cimd@test", "password1234", model.PermSuperuser)
	token, _ := env.login("cimd@test", "password1234")

	rec := env.do(http.MethodPost, "/v1/mcp-servers", token, map[string]any{
		"name": "Takes A Document", "url": remote.URL + "/mcp", "auth_type": model.MCPAuthOAuth,
	})
	env.expectStatus(rec, http.StatusCreated)
	var created mcpServerBody
	env.decode(rec, &created)

	rec = env.do(http.MethodPost, "/v1/mcp-servers/"+itoa(created.ID)+"/connect", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var out connectResponse
	env.decode(rec, &out)
	if !strings.Contains(out.AuthorizeURL, "client_id=") {
		t.Fatalf("no client id in the authorize request: %s", out.AuthorizeURL)
	}

	// The half that was missing: it must still be there when the person comes
	// back, because the token exchange has nothing else to identify us with.
	rec = env.do(http.MethodGet, "/v1/mcp-servers", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var connections []mcpServerBody
	env.decode(rec, &connections)
	for _, c := range connections {
		if c.ID != created.ID {
			continue
		}
		if c.OAuthClientID == "" {
			t.Fatal("the derived client id was not remembered; the callback would " +
				"exchange the code with no client_id and be refused")
		}
		if !strings.HasSuffix(c.OAuthClientID, "/connect/mcp/client-metadata.json") {
			t.Fatalf("client id %q is not a metadata document URL", c.OAuthClientID)
		}
		return
	}
	t.Fatal("the connection came back missing from the list")
}
