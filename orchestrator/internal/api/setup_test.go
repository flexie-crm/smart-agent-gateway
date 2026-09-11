package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"flexie.io/sag/internal/model"
)

// Readiness gates the whole product on a desktop: the chat refuses to offer a
// composer until this says yes. Saying yes too early puts somebody in front of
// an assistant that cannot answer, which is the worse of the two failures.

func readSetup(t *testing.T, env *testEnv, token string) setupState {
	t.Helper()
	rec := env.do(http.MethodGet, "/v1/setup", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var state setupState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestAFreshInstallationIsNotReady(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("owner@test", "pw-12345678", model.PermSuperuser)
	token, _ := env.login("owner@test", "pw-12345678")

	state := readSetup(t, env, token)
	if state.Ready {
		t.Fatal("an installation with nothing configured reported itself ready")
	}
	if state.HasVendor || state.HasModel {
		t.Fatalf("nothing was configured, yet it reports %+v", state)
	}
}

func TestAVendorAndAModelAreNotEnoughOnTheirOwn(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("owner@test", "pw-12345678", model.PermSuperuser)
	token, _ := env.login("owner@test", "pw-12345678")
	ctx := context.Background()

	vendor := &model.AIVendor{WorkspaceID: env.ws.ID, Name: "Anthropic", VendorKey: "anthropic"}
	if err := env.app.Store.Vendors().Create(ctx, vendor); err != nil {
		t.Fatal(err)
	}
	aiModel := &model.AIModel{
		WorkspaceID: env.ws.ID, VendorID: vendor.ID,
		ModelKey: "claude-opus-4-8", Type: "chat", ContextWindow: 200000,
	}
	if err := env.app.Store.AIModels().Create(ctx, aiModel); err != nil {
		t.Fatal(err)
	}

	// The pieces exist and nothing is using them. A person who stopped here
	// would have a chat that still cannot answer, so this must not read as done.
	state := readSetup(t, env, token)
	if !state.HasVendor || !state.HasModel {
		t.Fatalf("the pieces that exist were not reported: %+v", state)
	}
	if state.Ready {
		t.Fatal("reported ready with nothing pointing the Gateway at a model")
	}
}

func TestItIsReadyOnceTheGatewayHasAModel(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("owner@test", "pw-12345678", model.PermSuperuser)
	token, _ := env.login("owner@test", "pw-12345678")
	ctx := context.Background()

	vendor := &model.AIVendor{WorkspaceID: env.ws.ID, Name: "Anthropic", VendorKey: "anthropic"}
	if err := env.app.Store.Vendors().Create(ctx, vendor); err != nil {
		t.Fatal(err)
	}
	aiModel := &model.AIModel{
		WorkspaceID: env.ws.ID, VendorID: vendor.ID,
		ModelKey: "claude-opus-4-8", Type: "chat", ContextWindow: 200000,
	}
	if err := env.app.Store.AIModels().Create(ctx, aiModel); err != nil {
		t.Fatal(err)
	}
	pointGatewayAt(t, env, aiModel.ID)

	state := readSetup(t, env, token)
	if !state.Ready {
		t.Fatalf("the Gateway has a model and this is not ready: %+v", state)
	}
	// Named, so a screen can say what it is going to think with rather than
	// only that something was chosen.
	if state.GatewayModel != "claude-opus-4-8" {
		t.Fatalf("the model was not named: %+v", state)
	}
}

func TestAGatewayPointingAtADeletedModelIsNotReady(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("owner@test", "pw-12345678", model.PermSuperuser)
	token, _ := env.login("owner@test", "pw-12345678")
	ctx := context.Background()

	vendor := &model.AIVendor{WorkspaceID: env.ws.ID, Name: "Anthropic", VendorKey: "anthropic"}
	if err := env.app.Store.Vendors().Create(ctx, vendor); err != nil {
		t.Fatal(err)
	}
	aiModel := &model.AIModel{
		WorkspaceID: env.ws.ID, VendorID: vendor.ID,
		ModelKey: "claude-opus-4-8", Type: "chat", ContextWindow: 200000,
	}
	if err := env.app.Store.AIModels().Create(ctx, aiModel); err != nil {
		t.Fatal(err)
	}
	pointGatewayAt(t, env, aiModel.ID)
	if err := env.app.Store.AIModels().Delete(ctx, env.ws.ID, aiModel.ID); err != nil {
		t.Fatal(err)
	}

	// A row pointing at something that no longer exists is a configuration that
	// LOOKS finished and answers nothing. Checking only that a model was chosen
	// would let this through.
	if state := readSetup(t, env, token); state.Ready {
		t.Fatalf("ready, pointing at a model that was deleted: %+v", state)
	}
}

// pointGatewayAt is what finishing setup does: the Gateway is given a model.
func pointGatewayAt(t *testing.T, env *testEnv, modelID int64) {
	t.Helper()
	ctx := context.Background()
	agents, err := env.app.Store.Agents().List(ctx, env.ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range agents {
		if agent.Key != model.DefaultAgentKey {
			continue
		}
		agent.ModelID = &modelID
		if err := env.app.Store.Agents().Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		return
	}
	// A real workspace gets its Gateway when it is created; a bare test
	// workspace does not, so this makes the one setup would be pointing at.
	gateway := &model.Agent{
		WorkspaceID: env.ws.ID,
		Key:         model.DefaultAgentKey,
		Name:        "Gateway",
		Status:      model.StatusActive,
		ModelID:     &modelID,
	}
	if err := env.app.Store.Agents().Create(ctx, gateway); err != nil {
		t.Fatal(err)
	}
}
