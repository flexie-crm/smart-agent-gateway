package app_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tools/apitool"
	"flexie.io/sag/internal/tools/template"
)

// Connecting a tool to a service that signs a PERSON in.
//
// The round trip, the sealed state that is the whole authentication of the
// callback, and the token that is kept afterwards.

// connectable makes an API tool configured for the authorization-code grant.
func connectable(t *testing.T, e *env, authorize, token string) *model.Tool {
	t.Helper()
	tpl, ok := e.app.Templates.Get(apitool.TemplateName)
	if !ok {
		t.Fatal("the api template is not registered")
	}
	inst, err := tpl.Build(template.Input{
		Alias: "service", Variant: apitool.AuthAuthorizationCode,
		Settings: map[string]any{
			"base_url":           "https://api.service.test",
			"auth.authorize_url": authorize,
			"auth.token_url":     token,
			"auth.client_id":     "client-abc",
			"auth.client_secret": "shhh",
			"auth.scope":         "records.read",
			"policy.mode":        apitool.Denylist,
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	row := &model.Tool{
		WorkspaceID: e.ws.ID, Name: inst.Schema.Name, Kind: string(inst.Schema.Kind),
		Template: apitool.TemplateName, FriendlyName: inst.Schema.FriendlyName,
		Description: inst.Schema.Description, InputSchema: inst.Schema.InputSchema,
		Risk: string(inst.Schema.Risk), Config: inst.Config, Status: model.StatusActive,
	}
	if err := e.app.Store.Tools().CreateCustom(context.Background(), row, model.Nobody()); err != nil {
		t.Fatalf("create: %v", err)
	}
	return row
}

// grantOf reads the sign-in out of a tool's config, which is where it lives.
//
// Through the STORE and then opened, so what it reads is what was written:
// asserting on an in-memory value would pass with nothing persisted at all,
// which is most of what these tests are for.
func grantOf(t *testing.T, e *env, id int64) apitool.Grant {
	t.Helper()
	held := sealedGrantOf(t, e, id)
	if held == nil {
		return apitool.Grant{}
	}
	// Unsealed by hand rather than through the app, which ASSERTS the sealing
	// instead of trusting it: a token stored in the clear would not survive
	// this, where a helper that opened anything would read it back happily.
	out := apitool.Grant{
		AccessToken:  unsealed(t, e, held, "access_token"),
		RefreshToken: unsealed(t, e, held, "refresh_token"),
	}
	if name, ok := held["connected_by_name"].(string); ok {
		out.ConnectedByName = name
	}
	return out
}

// unsealed opens one field of the stored grant, refusing a value that is not
// sealed at all.
func unsealed(t *testing.T, e *env, held map[string]any, key string) string {
	t.Helper()
	raw, ok := held[key].(string)
	if !ok || raw == "" {
		return ""
	}
	// "enc:" is the marker a sealed value carries in a config. Written out here
	// on purpose: a test that took the constant from the code under test would
	// still pass if the marker stopped being applied.
	if !strings.HasPrefix(raw, "enc:") {
		t.Fatalf("%s is in the config UNSEALED: %q", key, raw)
	}
	sealed, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "enc:"))
	if err != nil {
		t.Fatalf("%s is not the encoding we seal with: %v", key, err)
	}
	opened, err := e.app.Keyring.Open(sealed)
	if err != nil {
		t.Fatalf("%s could not be opened with this installation's key: %v", key, err)
	}
	return string(opened)
}

// sealedGrantOf reads the grant as STORED, still sealed, so a test can assert
// that a token never reached the database in the clear.
func sealedGrantOf(t *testing.T, e *env, id int64) map[string]any {
	t.Helper()
	row, err := e.app.Store.Tools().GetByID(context.Background(), e.ws.ID, id)
	if err != nil {
		t.Fatalf("read the tool: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(row.Config, &cfg); err != nil {
		t.Fatalf("read the stored config: %v", err)
	}
	held, _ := cfg[apitool.GrantPath].(map[string]any)
	return held
}

// The whole round trip, against an authorization server that behaves.
func TestConnectingATool(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("consenter@acme.test")

	var exchanged url.Values
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		exchanged = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"granted","refresh_token":"renewable","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(auth.Close)

	row := connectable(t, e, "https://service.test/oauth/authorize", auth.URL+"/token")

	where, err := e.app.BeginToolConnect(ctx, e.ws.ID, row.ID, user.ID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	sent, err := url.Parse(where)
	if err != nil {
		t.Fatalf("the authorize address is not a URL: %v", err)
	}
	q := sent.Query()
	if sent.Host != "service.test" || sent.Path != "/oauth/authorize" {
		t.Fatalf("the person is sent to %s", sent)
	}
	if q.Get("client_id") != "client-abc" || q.Get("response_type") != "code" {
		t.Fatalf("the request is wrong: %s", q.Encode())
	}
	// PKCE: the CHALLENGE travels and the verifier does not. A code
	// intercepted without the verifier is worthless, which is the whole point.
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("no PKCE challenge was sent: %s", q.Encode())
	}
	state := q.Get("state")
	if state == "" {
		t.Fatal("no state was sent, so the callback would have no authentication")
	}

	// Nothing is written by beginning: a consent started and abandoned leaves
	// no trace to clean up.
	if held := grantOf(t, e, row.ID); held.Held() {
		t.Fatalf("beginning a connection wrote a credential: %+v", held)
	}

	claim, err := e.app.OpenToolState(state)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	if err := e.app.CompleteToolConnect(ctx, claim, "the-code"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// The verifier reached the token request, which is what proves this is the
	// same client that started the flow.
	if exchanged.Get("code_verifier") == "" {
		t.Fatalf("no verifier was sent to the token endpoint: %v", exchanged)
	}
	if exchanged.Get("code") != "the-code" || exchanged.Get("grant_type") != "authorization_code" {
		t.Fatalf("the exchange is wrong: %v", exchanged)
	}

	// Kept, and kept SEALED: grantOf opens it through the keyring and refuses a
	// value that was not sealed, so the token reaching the database in the
	// clear fails here rather than passing quietly.
	held := grantOf(t, e, row.ID)
	if !held.Held() {
		t.Fatal("nothing was kept, so the sign-in is lost the moment this process ends")
	}
	if held.AccessToken != "granted" {
		t.Fatalf("the token that came back is not the one the service issued: %q", held.AccessToken)
	}
	if held.RefreshToken == "" {
		t.Fatal("no refresh token was kept, so this connection dies at the first expiry")
	}
	// And the raw config carries neither in the clear.
	if raw := sealedGrantOf(t, e, row.ID); raw != nil {
		for _, key := range []string{"access_token", "refresh_token"} {
			if plain, _ := raw[key].(string); strings.Contains(plain, "granted") || strings.Contains(plain, "renewable") {
				t.Fatalf("%s is in the database in the clear: %q", key, plain)
			}
		}
	}
	if held.ConnectedByName == "" {
		t.Fatal("who consented was not recorded, so the audit trail stops at our edge")
	}

	// And disconnecting takes the grant and leaves the tool.
	if err := e.app.DisconnectTool(ctx, e.ws.ID, row.ID); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if held := grantOf(t, e, row.ID); held.Held() {
		t.Fatal("the grant survived disconnecting")
	}
	if _, err := e.app.Store.Tools().GetByID(ctx, e.ws.ID, row.ID); err != nil {
		t.Fatalf("disconnecting took the tool with it: %v", err)
	}
}

// The state is the callback's ONLY authentication, so what it refuses matters
// as much as what it accepts.
func TestTheStateIsTheAuthentication(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ name, state string }{
		{"nothing", ""},
		{"not our encoding", "not-base64-%%%"},
		{"something else entirely", "YWJjZGVmZ2hpamtsbW5vcA"},
	} {
		if _, err := e.app.OpenToolState(c.state); err == nil {
			t.Errorf("%s was accepted as a state", c.name)
		}
	}
	// The control: one we minted is accepted, so the refusals are about the
	// states and not about the opener being broken.
	ctx := context.Background()
	user := e.user("control@acme.test")
	row := connectable(t, e, "https://service.test/authorize", "https://service.test/token")
	where, err := e.app.BeginToolConnect(ctx, e.ws.ID, row.ID, user.ID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	sent, _ := url.Parse(where)
	claim, err := e.app.OpenToolState(sent.Query().Get("state"))
	if err != nil {
		t.Fatalf("our own state was refused: %v", err)
	}
	if claim.ToolID != row.ID || claim.WorkspaceID != e.ws.ID || claim.UserID != user.ID {
		t.Fatalf("the state lost which tool and who: %+v", claim)
	}
}

// A tool that does not sign in to a service has nothing to connect, and says so
// rather than producing an address that goes nowhere.
func TestAToolWithNoSignInCannotBeConnected(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("nobody@acme.test")

	tpl, _ := e.app.Templates.Get(apitool.TemplateName)
	inst, err := tpl.Build(template.Input{
		Alias: "keyed", Variant: apitool.AuthBearer,
		Settings: map[string]any{
			"base_url": "https://api.service.test", "auth.token": "t", "policy.mode": apitool.Denylist,
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	row := &model.Tool{
		WorkspaceID: e.ws.ID, Name: inst.Schema.Name, Kind: string(inst.Schema.Kind),
		Template: apitool.TemplateName, FriendlyName: inst.Schema.FriendlyName,
		Description: inst.Schema.Description, InputSchema: inst.Schema.InputSchema,
		Risk: string(inst.Schema.Risk), Config: inst.Config, Status: model.StatusActive,
	}
	if err := e.app.Store.Tools().CreateCustom(ctx, row, model.Nobody()); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = e.app.BeginToolConnect(ctx, e.ws.ID, row.ID, user.ID)
	if err == nil {
		t.Fatal("a tool with a bearer token was offered a sign-in")
	}
	if !strings.Contains(err.Error(), "nothing to connect") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
}

// An authorization server that refuses says so in its own words.
func TestAServiceRefusingTheExchangeIsReported(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("refused@acme.test")

	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"that code was already used"}`))
	}))
	t.Cleanup(auth.Close)

	row := connectable(t, e, "https://service.test/authorize", auth.URL+"/token")
	where, _ := e.app.BeginToolConnect(ctx, e.ws.ID, row.ID, user.ID)
	sent, _ := url.Parse(where)
	claim, _ := e.app.OpenToolState(sent.Query().Get("state"))

	err := e.app.CompleteToolConnect(ctx, claim, "used-code")
	if err == nil {
		t.Fatal("a refused exchange was treated as a connection")
	}
	if !strings.Contains(err.Error(), "already used") {
		t.Fatalf("the service's own reason was dropped: %v", err)
	}
	// And nothing was kept from a failure.
	if held := grantOf(t, e, row.ID); held.Held() {
		t.Fatalf("a failed connection left a credential: %+v", held)
	}
}

// grantTo connects a tool through the REAL exchange, so a test asserting on a
// connected tool is asserting on one connected the way the product does.
//
// Planting a row instead is how a test comes to pass against a storage rule
// that no longer holds, which is exactly what happened when the tokens moved
// out of a table and into the tool's own config.
func (e *env) grantTo(t *testing.T, toolID, userID int64, who string) {
	t.Helper()
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"granted-token","refresh_token":"refresh-1","expires_in":3600}`))
	}))
	t.Cleanup(auth.Close)

	row, err := e.app.Store.Tools().GetByID(context.Background(), e.ws.ID, toolID)
	if err != nil {
		t.Fatalf("read the tool: %v", err)
	}
	// Point the stored tool at the fake token endpoint for the exchange.
	var cfg map[string]any
	if err := json.Unmarshal(row.Config, &cfg); err != nil {
		t.Fatalf("read config: %v", err)
	}
	if a, ok := cfg["auth"].(map[string]any); ok {
		a["token_url"] = auth.URL + "/token"
	}
	patched, _ := json.Marshal(cfg)
	if err := e.app.Store.Tools().SetConfig(context.Background(), e.ws.ID, toolID, patched); err != nil {
		t.Fatalf("point the tool at the fake service: %v", err)
	}

	where, err := e.app.BeginToolConnect(context.Background(), e.ws.ID, toolID, userID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	parsed, _ := url.Parse(where)
	claim, err := e.app.OpenToolState(parsed.Query().Get("state"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	if err := e.app.CompleteToolConnect(context.Background(), claim, "the-code"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	_ = who
}

// The Connect button, offered by the DRIVER rather than by the console.
//
// The console knows nothing about OAuth: it runs the one action every template
// has and renders what comes back. So "is there a sign-in to do" has to be
// answered here, by the template that can read its own configuration and the
// app that can mint an address.
func TestTestingAToolThatSignsInOffersTheConnection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("offered@acme.test")

	// A service whose root answers, so the settings test passes and the offer
	// is about the sign-in rather than about a bad address.
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(service.Close)

	row := connectable(t, e, "https://service.test/oauth/authorize", "https://service.test/oauth/token")
	settings := map[string]any{
		"base_url":           service.URL,
		"auth.authorize_url": "https://service.test/oauth/authorize",
		"auth.token_url":     "https://service.test/oauth/token",
		"auth.client_id":     "client-abc",
		// Retyped here because this row was created directly with plaintext
		// settings, so there is no sealed value for the edit path to carry.
		"auth.client_secret": "shhh",
		"policy.mode":        apitool.Denylist,
	}

	result, err := e.app.RunToolActionForEdit(ctx, e.ws.ID, user.ID, row.ID, settings, app.ToolAction{})
	if err != nil {
		t.Fatalf("run action: %v", err)
	}
	if !result.OK {
		t.Fatalf("the settings were reported wrong: %s", result.Message)
	}
	if result.Visit == "" {
		t.Fatal("a tool that signs in to a service offered no way to do it")
	}
	if !strings.Contains(result.Visit, "service.test/oauth/authorize") {
		t.Fatalf("the address is not the service's sign-in: %s", result.Visit)
	}
	if result.Visiting == "" {
		t.Fatal("the button has no words on it")
	}

	// Once connected, the same test says so instead of offering again. Written
	// through the real round trip rather than by planting a row, because
	// planting one is how a test comes to pass against a storage rule that no
	// longer holds.
	e.grantTo(t, row.ID, user.ID, "The Consenter")
	result, err = e.app.RunToolActionForEdit(ctx, e.ws.ID, user.ID, row.ID, settings, app.ToolAction{})
	if err != nil {
		t.Fatalf("run action: %v", err)
	}
	if result.Visit != "" {
		t.Fatalf("a connected tool was offered a sign-in again: %s", result.Visit)
	}
	if !strings.Contains(result.Message, "Connected") {
		t.Fatalf("it does not say the tool is connected: %s", result.Message)
	}
}

// And a tool that does NOT sign in is offered nothing, which is the control:
// without it, offering everything would pass the test above.
func TestTestingAToolWithAKeyOffersNoConnection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("keyed2@acme.test")

	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(service.Close)

	tpl, _ := e.app.Templates.Get(apitool.TemplateName)
	inst, err := tpl.Build(template.Input{
		Alias: "keyed2", Variant: apitool.AuthBearer,
		Settings: map[string]any{"base_url": service.URL, "auth.token": "t", "policy.mode": apitool.Denylist},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	row := &model.Tool{
		WorkspaceID: e.ws.ID, Name: inst.Schema.Name, Kind: string(inst.Schema.Kind),
		Template: apitool.TemplateName, FriendlyName: inst.Schema.FriendlyName,
		Description: inst.Schema.Description, InputSchema: inst.Schema.InputSchema,
		Risk: string(inst.Schema.Risk), Config: inst.Config, Status: model.StatusActive,
	}
	if err := e.app.Store.Tools().CreateCustom(ctx, row, model.Nobody()); err != nil {
		t.Fatalf("create: %v", err)
	}
	result, err := e.app.RunToolActionForEdit(ctx, e.ws.ID, user.ID, row.ID,
		map[string]any{"base_url": service.URL, "auth.token": "t", "policy.mode": apitool.Denylist}, app.ToolAction{})
	if err != nil {
		t.Fatalf("run action: %v", err)
	}
	if result.Visit != "" {
		t.Fatalf("a tool with a bearer token was offered a sign-in: %s", result.Visit)
	}
}

// The rule that makes the grant safe to keep in the tool's own config: an EDIT
// carries it forward, and the FORM can never write it.
//
// These are the two failures that config storage invites, and neither of them
// announces itself. A save that drops the key disconnects the tool silently,
// because Config() builds a fresh object from the fields and a key no field
// writes is simply absent. A save that ACCEPTS the key lets anyone who can
// reach the API install a sign-in nobody gave.
func TestAnEditKeepsTheSignInAndCannotWriteOne(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("editor@acme.test")

	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(service.Close)

	row := connectable(t, e, "https://service.test/oauth/authorize", "https://service.test/oauth/token")
	e.grantTo(t, row.ID, user.ID, "The Consenter")

	before := grantOf(t, e, row.ID)
	if !before.Held() {
		t.Fatal("the fixture did not connect the tool, so this proves nothing")
	}

	// An ordinary edit: change the timeout and nothing else. The form has no
	// field for the grant and sends none.
	edited := map[string]any{
		"base_url":           service.URL,
		"timeout_seconds":    float64(45),
		"auth.authorize_url": "https://service.test/oauth/authorize",
		"auth.token_url":     "https://service.test/oauth/token",
		"auth.client_id":     "client-abc",
		"auth.client_secret": "shhh",
		"policy.mode":        apitool.Denylist,
	}
	if _, err := e.app.UpdateCustomTool(ctx, e.ws.ID, row.ID, template.Input{
		Variant: apitool.AuthAuthorizationCode, Settings: edited,
	}, nil, model.Nobody()); err != nil {
		t.Fatalf("edit the tool: %v", err)
	}

	after := grantOf(t, e, row.ID)
	if !after.Held() {
		t.Fatal("saving the form disconnected the tool, which nothing would have told anybody")
	}
	if after.AccessToken != before.AccessToken || after.RefreshToken != before.RefreshToken {
		t.Fatalf("the sign-in changed across an edit that never touched it: %q/%q became %q/%q",
			before.AccessToken, before.RefreshToken, after.AccessToken, after.RefreshToken)
	}

	// The real sign-in is also not disturbed by a request that names the
	// reserved key, though on a CONNECTED tool the carry-forward would mask
	// that anyway. The case where it is load-bearing has its own test below.
	forged := map[string]any{
		"base_url":           service.URL,
		"auth.authorize_url": "https://service.test/oauth/authorize",
		"auth.token_url":     "https://service.test/oauth/token",
		"auth.client_id":     "client-abc",
		"auth.client_secret": "shhh",
		"policy.mode":        apitool.Denylist,
		"grant.access_token": "a-token-nobody-granted",
	}
	if _, err := e.app.UpdateCustomTool(ctx, e.ws.ID, row.ID, template.Input{
		Variant: apitool.AuthAuthorizationCode, Settings: forged,
	}, nil, model.Nobody()); err != nil {
		t.Fatalf("edit the tool: %v", err)
	}
	if forgedGrant := grantOf(t, e, row.ID); forgedGrant.AccessToken != before.AccessToken {
		t.Fatalf("the real sign-in was disturbed by the attempt: %q", forgedGrant.AccessToken)
	}
}

// A sign-in cannot be INSTALLED through the settings endpoint.
//
// On a tool that has never been connected, which is the only case that tests
// the refusal: where a grant already exists the carry-forward overwrites
// whatever was sent and the attempt is masked. Found by running the control,
// which changed nothing until this test existed.
//
// It matters because the reserved key is only reserved by rule. nest() builds
// whatever keys it is handed, so "the form has no field for it" is a fact about
// the form and not a property of the path, and anything that can reach the API
// composes its own settings map.
func TestASignInCannotBeInstalledByEditingTheSettings(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("forger@acme.test")
	_ = user

	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(service.Close)

	row := connectable(t, e, "https://service.test/oauth/authorize", "https://service.test/oauth/token")
	if held := grantOf(t, e, row.ID); held.Held() {
		t.Fatal("the fixture arrived connected, so nothing here tests the refusal")
	}

	forged := map[string]any{
		"base_url":            service.URL,
		"auth.authorize_url":  "https://service.test/oauth/authorize",
		"auth.token_url":      "https://service.test/oauth/token",
		"auth.client_id":      "client-abc",
		"auth.client_secret":  "shhh",
		"policy.mode":         apitool.Denylist,
		"grant.access_token":  "a-token-nobody-granted",
		"grant.refresh_token": "and-a-way-to-keep-it",
	}
	if _, err := e.app.UpdateCustomTool(ctx, e.ws.ID, row.ID, template.Input{
		Variant: apitool.AuthAuthorizationCode, Settings: forged,
	}, nil, model.Nobody()); err != nil {
		t.Fatalf("edit the tool: %v", err)
	}

	if held := grantOf(t, e, row.ID); held.Held() {
		t.Fatalf("a sign-in nobody gave was installed through the settings endpoint: %+v", held)
	}
	// And nothing of it is in the stored config under any shape.
	if raw := sealedGrantOf(t, e, row.ID); raw != nil {
		t.Fatalf("the reserved key was written by the form: %+v", raw)
	}
}
