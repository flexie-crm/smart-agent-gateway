package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
)

const secretAPIKey = "sk-ant-do-not-leak-me-0123456789"

// The models a fake vendor says it offers. Saving a model now asks the vendor
// whether it exists, so a vendor in a test has to be a real endpoint that
// answers. It must never be the real one: a suite that depends on a vendor being
// up, and on somebody's key being valid, fails for reasons that have nothing to
// do with the code it claims to test.
var fakeVendorModels = []string{"claude-opus-4-8", "claude-sonnet-5", "claude-haiku-4-5"}

// fakeVendorServer serves the catalog endpoint, in the vendor's own shape.
func fakeVendorServer(t *testing.T, offers []string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entries := make([]map[string]any, 0, len(offers))
		for _, id := range offers {
			entries = append(entries, map[string]any{
				"id": id, "display_name": id, "type": "model",
				"created_at": "2026-01-01T00:00:00Z",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": entries, "has_more": false})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// createVendorViaAPI makes a vendor pointed at a fake endpoint that offers
// fakeVendorModels.
func (e *testEnv) createVendorViaAPI(token, name, credentials string) vendorBody {
	e.t.Helper()
	return e.createVendorAt(token, name, credentials, fakeVendorServer(e.t, fakeVendorModels))
}

func (e *testEnv) createVendorAt(token, name, credentials, baseURL string) vendorBody {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/v1/vendors", token, map[string]any{
		"vendor_key": model.VendorAnthropic, "name": name,
		"credentials": credentials, "base_url": baseURL,
	})
	e.expectStatus(rec, http.StatusCreated)
	var v vendorBody
	e.decode(rec, &v)
	return v
}

// The vendor form is drawn from this endpoint, so what it serves is a contract
// and not an implementation detail. It served a bare list of keys while the
// console read a key and a name off each entry, and the result was a dropdown
// of seven blank options: every vendor was there, and none of them could be
// read or chosen. Nothing failed, which is why nothing caught it.
func TestVendorKindsAreLabelledForTheForm(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodGet, "/v1/vendor-kinds", token, nil)
	env.expectStatus(rec, http.StatusOK)

	var kinds []model.VendorInfo
	env.decode(rec, &kinds)

	if len(kinds) != len(model.KnownVendorKeys) {
		t.Fatalf("catalog has %d entries, the validator knows %d",
			len(kinds), len(model.KnownVendorKeys))
	}
	for _, kind := range kinds {
		if kind.Key == "" || kind.Name == "" {
			t.Errorf("an unnamed vendor cannot be rendered or chosen: %+v", kind)
		}
		// Every offered key must be one the validator will accept, or the form
		// offers a vendor that cannot be saved.
		if !slices.Contains(model.KnownVendorKeys, kind.Key) {
			t.Errorf("the form offers %q, which POST /v1/vendors would reject", kind.Key)
		}
	}

	// And the other way: a key the validator accepts but the catalog omits is a
	// vendor nobody can pick.
	for _, key := range model.KnownVendorKeys {
		if !slices.ContainsFunc(kinds, func(k model.VendorInfo) bool { return k.Key == key }) {
			t.Errorf("%q is a valid vendor that the form never offers", key)
		}
	}
}

// TestVendorCredentialsNeverLeave is the test that matters most here: an API
// key handed to us must never come back out over HTTP, in any shape, on any
// route.
func TestVendorCredentialsNeverLeave(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	created := env.createVendorViaAPI(token, "Anthropic", secretAPIKey)
	if !created.HasCredentials {
		t.Fatal("vendor must report that a credential is stored")
	}

	// Neither the plaintext nor the sealed blob may appear in any response.
	for _, path := range []string{"/v1/vendors", "/v1/vendors/" + itoa(created.ID)} {
		rec := env.do(http.MethodGet, path, token, nil)
		env.expectStatus(rec, http.StatusOK)
		if bytes.Contains(rec.Body.Bytes(), []byte(secretAPIKey)) {
			t.Fatalf("%s leaked the API key: %s", path, rec.Body.String())
		}
		if bytes.Contains(rec.Body.Bytes(), []byte("credential")) &&
			!bytes.Contains(rec.Body.Bytes(), []byte("has_credentials")) {
			t.Fatalf("%s exposed a credential field: %s", path, rec.Body.String())
		}
	}

	// The secret is stored encrypted, not in the clear.
	stored, err := env.app.Store.Vendors().GetByID(context.Background(), env.ws.ID, created.ID)
	if err != nil {
		t.Fatalf("load vendor: %v", err)
	}
	if bytes.Contains(stored.Credentials, []byte(secretAPIKey)) {
		t.Fatal("the API key is stored in plaintext in the database")
	}

	// And it decrypts back to exactly what was submitted.
	plaintext, err := env.app.VendorCredentials(context.Background(), env.ws.ID, created.ID)
	if err != nil {
		t.Fatalf("open credentials: %v", err)
	}
	if plaintext != secretAPIKey {
		t.Fatalf("credentials did not survive the round trip: %q", plaintext)
	}
}

// TestVendorUpdateKeepsCredentials guards the mistake that silently breaks a
// workspace: renaming a vendor must not wipe its API key.
func TestVendorUpdateKeepsCredentials(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	created := env.createVendorViaAPI(token, "Anthropic", secretAPIKey)

	// Update without sending credentials.
	rec := env.do(http.MethodPut, "/v1/vendors/"+itoa(created.ID), token, map[string]any{
		"name": "Anthropic Production", "status": model.StatusActive,
	})
	env.expectStatus(rec, http.StatusOK)
	var updated vendorBody
	env.decode(rec, &updated)
	if updated.Name != "Anthropic Production" {
		t.Fatalf("rename not applied: %+v", updated)
	}
	if !updated.HasCredentials {
		t.Fatal("editing a vendor erased its stored credentials")
	}
	plaintext, err := env.app.VendorCredentials(context.Background(), env.ws.ID, created.ID)
	if err != nil || plaintext != secretAPIKey {
		t.Fatalf("credential changed during an unrelated edit: %q %v", plaintext, err)
	}

	// Sending a new credential replaces it.
	const rotated = "sk-ant-rotated-key-9876543210"
	env.expectStatus(env.do(http.MethodPut, "/v1/vendors/"+itoa(created.ID), token, map[string]any{
		"name": "Anthropic Production", "status": model.StatusActive, "credentials": rotated,
	}), http.StatusOK)
	plaintext, err = env.app.VendorCredentials(context.Background(), env.ws.ID, created.ID)
	if err != nil || plaintext != rotated {
		t.Fatalf("credential not rotated: %q %v", plaintext, err)
	}

	// Clearing removes it.
	env.expectStatus(env.do(http.MethodDelete, "/v1/vendors/"+itoa(created.ID)+"/credentials", token, nil),
		http.StatusNoContent)
	rec = env.do(http.MethodGet, "/v1/vendors/"+itoa(created.ID), token, nil)
	env.expectStatus(rec, http.StatusOK)
	env.decode(rec, &updated)
	if updated.HasCredentials {
		t.Fatal("credentials were not cleared")
	}
}

func TestVendorValidation(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	cases := []struct {
		name string
		body map[string]any
	}{
		{"unknown vendor key", map[string]any{"vendor_key": "not-a-vendor", "name": "X"}},
		{"missing name", map[string]any{"vendor_key": model.VendorAnthropic}},
		{"openai-compatible without base url", map[string]any{
			"vendor_key": model.VendorOpenAICompatible, "name": "Local",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env.expectStatus(env.do(http.MethodPost, "/v1/vendors", token, tc.body), http.StatusBadRequest)
		})
	}

	// A local server is valid with a base URL and no credentials at all.
	rec := env.do(http.MethodPost, "/v1/vendors", token, map[string]any{
		"vendor_key": model.VendorOpenAICompatible, "name": "Local model",
		"base_url": "http://127.0.0.1:8000/v1",
	})
	env.expectStatus(rec, http.StatusCreated)
	var local vendorBody
	env.decode(rec, &local)
	if local.HasCredentials {
		t.Fatal("a vendor created without a secret must report no credentials")
	}

	// The adapter cannot be swapped after creation.
	env.expectStatus(env.do(http.MethodPut, "/v1/vendors/"+itoa(local.ID), token, map[string]any{
		"vendor_key": model.VendorAnthropic, "name": "Local model",
		"base_url": "http://127.0.0.1:8000/v1", "status": model.StatusActive,
	}), http.StatusBadRequest)
}

func TestModelCRUDAndValidation(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	vendor := env.createVendorViaAPI(token, "Anthropic", secretAPIKey)

	rec := env.do(http.MethodPost, "/v1/models", token, map[string]any{
		"vendor_id": vendor.ID, "model_key": "claude-sonnet-5", "type": model.ModelTypeChat,
		"context_window": 200000, "description": "the smart one",
		"input_price_per_1m": 3.0, "output_price_per_1m": 15.0,
	})
	env.expectStatus(rec, http.StatusCreated)
	var created modelBody
	env.decode(rec, &created)
	if created.ModelKey != "claude-sonnet-5" || created.Description != "the smart one" {
		t.Fatalf("model not created as sent: %+v", created)
	}

	invalid := []struct {
		name string
		body map[string]any
	}{
		{"unknown type", map[string]any{
			"vendor_id": vendor.ID, "model_key": "x", "type": "telepathy",
		}},
		{"missing model key", map[string]any{
			"vendor_id": vendor.ID, "type": model.ModelTypeChat,
		}},
		{"unknown vendor", map[string]any{
			"vendor_id": 999999, "model_key": "x", "type": model.ModelTypeChat,
		}},
		{"negative price", map[string]any{
			"vendor_id": vendor.ID, "model_key": "x", "type": model.ModelTypeChat,
			"input_price_per_1m": -1.0,
		}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			env.expectStatus(env.do(http.MethodPost, "/v1/models", token, tc.body), http.StatusBadRequest)
		})
	}

	// A vendor with models cannot be deleted.
	env.expectStatus(env.do(http.MethodDelete, "/v1/vendors/"+itoa(vendor.ID), token, nil), http.StatusConflict)

	env.expectStatus(env.do(http.MethodDelete, "/v1/models/"+itoa(created.ID), token, nil), http.StatusNoContent)
	env.expectStatus(env.do(http.MethodDelete, "/v1/vendors/"+itoa(vendor.ID), token, nil), http.StatusNoContent)
}

// A model id is typed by hand, and a typo saves happily and then fails on the
// first real question somebody asks it. The vendor is the only authority on
// whether one of its models exists, so we ask it.
func TestAModelTheVendorDoesNotOfferIsRefused(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	vendor := env.createVendorViaAPI(token, "Anthropic", secretAPIKey)

	good := map[string]any{
		"vendor_id": vendor.ID, "model_key": "claude-sonnet-5",
		"type": model.ModelTypeChat,
	}
	env.expectStatus(env.do(http.MethodPost, "/v1/models", token, good), http.StatusCreated)

	// The typo. It is refused on the field that carries it, and the message names
	// what the vendor does offer: a refusal that does not is a guessing game.
	typo := map[string]any{
		"vendor_id": vendor.ID, "model_key": "sdfasdfasdf",
		"type": model.ModelTypeChat,
	}
	rec := env.do(http.MethodPost, "/v1/models", token, typo)
	env.expectStatus(rec, http.StatusBadRequest)

	var body struct {
		Fields map[string]string `json:"fields"`
	}
	env.decode(rec, &body)
	message := body.Fields["model_key"]
	if message == "" {
		t.Fatalf("the typo was not reported on model_key: %+v", body.Fields)
	}
	for _, want := range []string{"sdfasdfasdf", "Anthropic", "claude-sonnet-5"} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal never mentions %q: %s", want, message)
		}
	}

	// And the same check guards an edit: a model that was right can be made wrong.
	created := env.do(http.MethodPost, "/v1/models", token, map[string]any{
		"vendor_id": vendor.ID, "model_key": "claude-haiku-4-5",
		"type": model.ModelTypeChat,
	})
	env.expectStatus(created, http.StatusCreated)
	var m modelBody
	env.decode(created, &m)

	env.expectStatus(env.do(http.MethodPut, "/v1/models/"+itoa(m.ID), token, map[string]any{
		"vendor_id": vendor.ID, "model_key": "claude-haiku-4-6-typo",
		"type":   model.ModelTypeChat,
		"status": model.StatusActive,
	}), http.StatusBadRequest)
}

// The refusal must rest on the vendor SAYING the model is not there, never on
// our failing to ask. A vendor that is down, or has no key yet, knows nothing
// about whether a model exists, and treating its silence as a denial would mean
// an unreachable vendor stops an administrator from doing their job.
func TestAVendorThatCannotBeAskedDoesNotBlockTheSave(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// An endpoint that answers nothing at all: closed port, no catalog, no
	// opinion. The model is saved, because nothing said it was wrong.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()

	vendor := env.createVendorAt(token, "Unreachable", secretAPIKey, dead.URL)
	env.expectStatus(env.do(http.MethodPost, "/v1/models", token, map[string]any{
		"vendor_id": vendor.ID, "model_key": "a-model-nobody-can-confirm",
		"type": model.ModelTypeChat,
	}), http.StatusCreated)
}

// The picker the model form draws from. It reports whether the vendor would say
// at all, so a client can tell "offers nothing" apart from "would not answer".
func TestVendorCatalogIsWhatTheVendorSaysItOffers(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	vendor := env.createVendorViaAPI(token, "Anthropic", secretAPIKey)

	rec := env.do(http.MethodGet, "/v1/vendors/"+itoa(vendor.ID)+"/catalog", token, nil)
	env.expectStatus(rec, http.StatusOK)

	var catalog app.VendorCatalog
	env.decode(rec, &catalog)
	if !catalog.Listed {
		t.Fatal("a vendor that answered is reported as unlistable")
	}
	if len(catalog.Models) != len(fakeVendorModels) {
		t.Fatalf("got %d models, want %d: %+v",
			len(catalog.Models), len(fakeVendorModels), catalog.Models)
	}
	if catalog.Models[0].ID != fakeVendorModels[0] {
		t.Errorf("first model is %+v", catalog.Models[0])
	}

	// A vendor that will not answer says so, and does not pretend to be empty.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()
	mute := env.createVendorAt(token, "Mute", secretAPIKey, dead.URL)

	rec = env.do(http.MethodGet, "/v1/vendors/"+itoa(mute.ID)+"/catalog", token, nil)
	env.expectStatus(rec, http.StatusOK)
	env.decode(rec, &catalog)
	if catalog.Listed {
		t.Error("a vendor that refused to answer is reported as having listed")
	}
}

func TestVendorFieldErrors(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// A blank name and an unknown adapter answer together, each on its field.
	env.expectFields(env.do(http.MethodPost, "/v1/vendors", token, map[string]any{
		"vendor_key": "not-a-vendor", "name": " ",
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"name":       "a name is required",
		"vendor_key": "unknown vendor",
	})

	// An adapter that is defined by its endpoint needs one.
	env.expectFields(env.do(http.MethodPost, "/v1/vendors", token, map[string]any{
		"vendor_key": model.VendorOpenAICompatible, "name": "Local",
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"base_url": "this vendor has no default endpoint, so one is required",
	})

	// Swapping the adapter after creation is refused on the vendor_key field.
	created := env.createVendorViaAPI(token, "Anthropic", secretAPIKey)
	env.expectFields(env.do(http.MethodPut, "/v1/vendors/"+itoa(created.ID), token, map[string]any{
		"vendor_key": model.VendorOpenAI, "name": "Anthropic", "status": model.StatusActive,
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"vendor_key": "the vendor kind cannot be changed once created",
	})
}

func TestModelFieldErrors(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	vendor := env.createVendorViaAPI(token, "Anthropic", secretAPIKey)

	// Four problems, one answer.
	env.expectFields(env.do(http.MethodPost, "/v1/models", token, map[string]any{
		"vendor_id": int64(999999), "type": "telepathy",
		"input_price_per_1m": -1.0,
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"model_key":          "the vendor's own name for the model is required",
		"type":               "unknown model type",
		"input_price_per_1m": "cannot be negative",
		"vendor_id":          "unknown vendor",
	})

	// The same model registered twice on one vendor lands on the key field.
	body := map[string]any{
		"vendor_id": vendor.ID, "model_key": "claude-sonnet-5", "type": model.ModelTypeChat,
	}
	env.expectStatus(env.do(http.MethodPost, "/v1/models", token, body), http.StatusCreated)
	env.expectFields(env.do(http.MethodPost, "/v1/models", token, body),
		http.StatusConflict, "conflict", map[string]string{
			"model_key": "this vendor already has this model",
		})
}

// A model must never be attached to another tenant's vendor: that would let
// one workspace spend another workspace's API key.
func TestModelCannotUseForeignVendor(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	ctx := context.Background()
	other := &model.Workspace{Slug: "globex", Name: "Globex"}
	if err := env.app.Store.Workspaces().Create(ctx, other); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	foreignVendor := &model.AIVendor{
		WorkspaceID: other.ID, VendorKey: model.VendorAnthropic,
		Name: "Their Anthropic", Credentials: []byte("their-sealed-key"),
	}
	if err := env.app.Store.Vendors().Create(ctx, foreignVendor); err != nil {
		t.Fatalf("create foreign vendor: %v", err)
	}

	env.expectStatus(env.do(http.MethodPost, "/v1/models", token, map[string]any{
		"vendor_id": foreignVendor.ID, "model_key": "claude-sonnet-5",
		"type": model.ModelTypeChat,
	}), http.StatusBadRequest)

	// The foreign vendor is also invisible and untouchable.
	env.expectStatus(env.do(http.MethodGet, "/v1/vendors/"+itoa(foreignVendor.ID), token, nil), http.StatusNotFound)
	env.expectStatus(env.do(http.MethodDelete, "/v1/vendors/"+itoa(foreignVendor.ID)+"/credentials", token, nil),
		http.StatusNotFound)
}

func TestAIRoutesRequirePermissions(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	adminToken, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	vendor := env.createVendorViaAPI(adminToken, "Anthropic", secretAPIKey)

	// A viewer may read vendors but not create, edit, or delete them.
	env.createUser("viewer@acme.test", "dev-Passw0rd!", model.PermVendorsView)
	viewer, _ := env.login("viewer@acme.test", "dev-Passw0rd!")

	env.expectStatus(env.do(http.MethodGet, "/v1/vendors", viewer, nil), http.StatusOK)
	env.expectStatus(env.do(http.MethodPost, "/v1/vendors", viewer, map[string]any{
		"vendor_key": model.VendorAnthropic, "name": "Sneaky",
	}), http.StatusForbidden)
	env.expectStatus(env.do(http.MethodDelete, "/v1/vendors/"+itoa(vendor.ID)+"/credentials", viewer, nil),
		http.StatusForbidden)
	env.expectStatus(env.do(http.MethodDelete, "/v1/vendors/"+itoa(vendor.ID), viewer, nil), http.StatusForbidden)
	// Vendor permissions do not imply model permissions.
	env.expectStatus(env.do(http.MethodGet, "/v1/models", viewer, nil), http.StatusForbidden)

	// A user with no AI permissions at all sees nothing.
	env.createUser("nobody@acme.test", "dev-Passw0rd!")
	nobody, _ := env.login("nobody@acme.test", "dev-Passw0rd!")
	env.expectStatus(env.do(http.MethodGet, "/v1/vendors", nobody, nil), http.StatusForbidden)
	env.expectStatus(env.do(http.MethodGet, "/v1/models", nobody, nil), http.StatusForbidden)
}

// A credential sealed with a key that is no longer in the keyring must be
// reported, not silently treated as "no credentials", which would send the
// gateway to a vendor unauthenticated.
func TestUnreadableCredentialsAreReported(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	vendor := env.createVendorViaAPI(token, "Anthropic", secretAPIKey)

	ctx := context.Background()
	stored, err := env.app.Store.Vendors().GetByID(ctx, env.ws.ID, vendor.ID)
	if err != nil {
		t.Fatalf("load vendor: %v", err)
	}
	// Corrupt the sealed value the way a lost key would look: unknown id.
	stored.Credentials = append([]byte{1, 1, 'z'}, stored.Credentials[3:]...)
	if err := env.app.Store.Vendors().Update(ctx, stored); err != nil {
		t.Fatalf("store corrupted credentials: %v", err)
	}

	_, err = env.app.VendorCredentials(ctx, env.ws.ID, vendor.ID)
	if err == nil {
		t.Fatal("unreadable credentials must be reported, not returned as empty")
	}
	if !errors.Is(err, app.ErrCredentialsUnavailable) {
		t.Fatalf("expected ErrCredentialsUnavailable, got %v", err)
	}
}

// The form is drawn from what the vendor declares, so the vendor list carries
// it. Without this the console would have to know what a reasoning effort is,
// which is exactly the coupling the declared-settings design removes.
func TestTheVendorListCarriesWhatAnAgentMayBeConfiguredWith(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("specs@test", "password1234", model.PermSuperuser)
	token, _ := env.login("specs@test", "password1234")
	env.createVendorViaAPI(token, "Anthropic", "sk-test")

	rec := env.do(http.MethodGet, "/v1/models", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var body modelsBody
	env.decode(rec, &body)
	if len(body.Vendors) == 0 {
		t.Fatal("no vendors came back")
	}
	for _, spec := range body.Vendors[0].Settings {
		if spec.Key == "reasoning_effort" {
			if len(spec.Choices) == 0 {
				t.Fatal("a choice setting arrived with no choices to pick from")
			}
			if spec.Default == "" {
				t.Fatal("no default, so a form has nothing to show before anybody chooses")
			}
			return
		}
	}
	t.Fatal("the vendor declared no reasoning effort")
}
