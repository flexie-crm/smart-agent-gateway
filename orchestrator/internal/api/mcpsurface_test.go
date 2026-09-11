package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"flexie.io/sag/internal/mcpclient"
	"flexie.io/sag/internal/model"
)

// Our MCP server, consumed by OUR OWN MCP client over a real HTTP listener:
// the strongest dogfood there is. A service token gets in, the exposure
// kill-switch and the caller's grants decide what exists, a code-locked
// dangerous tool refuses, and every kill switch takes effect on the next
// request, not the next session.

// mintServiceClient creates a service client over the API and returns the
// shown-once token.
func (e *testEnv) mintServiceClient(token, name string) (clientID int64, serviceToken string) {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/v1/oauth-clients", token, map[string]any{
		"name": name, "client_type": model.OAuthClientService,
	})
	e.expectStatus(rec, http.StatusCreated)
	var body oauthClientBody
	e.decode(rec, &body)
	if body.ServiceToken == "" {
		e.t.Fatal("a service client must answer with its token, once")
	}
	return body.ID, body.ServiceToken
}

func TestOurMCPServerEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// The console runs on ServeHTTP; the MCP client needs a real listener.
	ts := httptest.NewServer(env.router)
	t.Cleanup(ts.Close)

	clientRow, serviceToken := env.mintServiceClient(token, "Reporting Bot")

	// The token never appears again on any read.
	rec := env.do(http.MethodGet, "/v1/oauth-clients", token, nil)
	env.expectStatus(rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), serviceToken) {
		t.Fatal("the service token leaked on a list")
	}

	// Our own client connects to our own server with that token.
	session, err := mcpclient.Dial(context.Background(), ts.URL+"/mcp", serviceToken)
	if err != nil {
		t.Fatalf("dial our own mcp server: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	// Unconfigured means the whole catalog (CRM parity): every active tool.
	tools, err := session.ListTools(context.Background())
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
	}
	if !names["current_time"] || !names["set_model_status"] {
		t.Fatalf("the unconfigured catalog is incomplete: %v", names)
	}

	// A harmless tool answers.
	content, isError, err := session.CallTool(context.Background(), "current_time", nil)
	if err != nil || isError {
		t.Fatalf("current_time failed: %v %v %s", err, isError, content)
	}

	// A tool whose CODE demands approval never runs over this surface: that
	// lock is not the administrator's to lift, and there is no person here
	// to ask.
	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	args, _ := json.Marshal(map[string]any{"model_id": modelID, "status": "disabled"})
	content, isError, err = session.CallTool(context.Background(), "set_model_status", args)
	if err != nil || !isError || !strings.Contains(content, "approval") {
		t.Fatalf("a code-locked tool was not refused: %v %v %s", err, isError, content)
	}
	target, err := env.app.Store.AIModels().GetByID(context.Background(), env.ws.ID, modelID)
	if err != nil || target.Status != model.StatusActive {
		t.Fatalf("the refused action ran anyway: %v %+v", err, target)
	}

	// The admin narrows the exposure to one tool. Authorization is resolved
	// per call, so the running session feels it immediately.
	rec = env.do(http.MethodPut, "/v1/mcp-server", token, map[string]any{
		"tool_config":  map[string]any{"current_time": map[string]any{"enabled": true}},
		"brain_config": []int64{},
	})
	env.expectStatus(rec, http.StatusOK)

	content, isError, err = session.CallTool(context.Background(), "list_models", nil)
	if err != nil || !isError || !strings.Contains(content, "not enabled") {
		t.Fatalf("an unexposed tool still ran: %v %v %s", err, isError, content)
	}
	if _, isError, _ = session.CallTool(context.Background(), "current_time", nil); isError {
		t.Fatal("the one exposed tool stopped working")
	}

	// A fresh session lists exactly the exposure.
	fresh, err := mcpclient.Dial(context.Background(), ts.URL+"/mcp", serviceToken)
	if err != nil {
		t.Fatalf("dial again: %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	tools, err = fresh.ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "current_time" {
		t.Fatalf("the narrowed exposure did not narrow the list: %v %+v", err, tools)
	}

	// Disabling the client is a kill switch that works mid-session: the very
	// next request is refused, session id or not.
	rec = env.do(http.MethodPut, "/v1/oauth-clients/"+itoa(clientRow), token, map[string]any{
		"name": "Reporting Bot", "client_type": model.OAuthClientService, "status": model.StatusDisabled,
	})
	env.expectStatus(rec, http.StatusOK)
	if _, _, err = fresh.CallTool(context.Background(), "current_time", nil); err == nil {
		t.Fatal("a disabled client's token still worked")
	}
}

func TestServiceTokenNeedsThePermission(t *testing.T) {
	env := newTestEnv(t)
	// A user allowed to manage clients but NOT to connect to MCP: minting a
	// token that acts as them would mint access they do not have.
	env.createUser("clerk@acme.test", "dev-Passw0rd!",
		model.PermOAuthClientsView, model.PermOAuthClientsCreate)
	token, _ := env.login("clerk@acme.test", "dev-Passw0rd!")

	env.expectFields(env.do(http.MethodPost, "/v1/oauth-clients", token, map[string]any{
		"name": "Bot", "client_type": model.OAuthClientService,
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"client_type": "a service token acts as you, and your account is not allowed to connect an external agent",
	})
}

func TestOAuthClientLifecycle(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	ts := httptest.NewServer(env.router)
	t.Cleanup(ts.Close)

	// Everything wrong answers at once, field by field.
	env.expectFields(env.do(http.MethodPost, "/v1/oauth-clients", token, map[string]any{
		"name": " ", "client_type": "wizard",
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"name":        "a name is required",
		"client_type": "must be public, confidential or service",
	})
	env.expectFields(env.do(http.MethodPost, "/v1/oauth-clients", token, map[string]any{
		"name": "App", "client_type": model.OAuthClientPublic,
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"redirect_uris": "at least one redirect address is required",
	})

	// A confidential client's secret is shown once and never again.
	rec := env.do(http.MethodPost, "/v1/oauth-clients", token, map[string]any{
		"name": "Backend", "client_type": model.OAuthClientConfidential,
		"redirect_uris": []string{"https://app.example.test/callback"},
	})
	env.expectStatus(rec, http.StatusCreated)
	var created oauthClientBody
	env.decode(rec, &created)
	if created.ClientSecret == "" || created.ServiceToken != "" {
		t.Fatalf("a confidential client mints a secret, not a token: %+v", created)
	}
	rec = env.do(http.MethodGet, "/v1/oauth-clients", token, nil)
	env.expectStatus(rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), created.ClientSecret) {
		t.Fatal("the client secret leaked on a list")
	}

	// Deleting a service client takes its token with it: the kill switch.
	clientRow, serviceToken := env.mintServiceClient(token, "Doomed Bot")
	session, err := mcpclient.Dial(context.Background(), ts.URL+"/mcp", serviceToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	env.expectStatus(env.do(http.MethodDelete, "/v1/oauth-clients/"+itoa(clientRow), token, nil),
		http.StatusNoContent)
	if _, _, err := session.CallTool(context.Background(), "current_time", nil); err == nil {
		t.Fatal("a deleted client's token still worked")
	}
}
