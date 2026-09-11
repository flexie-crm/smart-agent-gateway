package storetest

import (
	"bytes"
	"errors"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

func mustVendor(t *testing.T, st store.Store, wsID int64, name string, creds []byte) *model.AIVendor {
	t.Helper()
	v := &model.AIVendor{
		WorkspaceID: wsID,
		VendorKey:   model.VendorAnthropic,
		Name:        name,
		Credentials: creds,
	}
	if err := st.Vendors().Create(ctx(), v); err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	return v
}

func mustAIModel(t *testing.T, st store.Store, wsID, vendorID int64, key string) *model.AIModel {
	t.Helper()
	m := &model.AIModel{
		WorkspaceID: wsID, VendorID: vendorID, ModelKey: key,
		Type: model.ModelTypeChat, ContextWindow: 200000,
		Description:     "notes",
		InputPricePer1M: 3, OutputPricePer1M: 15,
	}
	if err := st.AIModels().Create(ctx(), m); err != nil {
		t.Fatalf("create model: %v", err)
	}
	return m
}

func testVendors(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	sealed := []byte{0x01, 0x02, 0x03}
	v := mustVendor(t, st, ws.ID, "Anthropic", sealed)

	got, err := st.Vendors().GetByID(ctx(), ws.ID, v.ID)
	if err != nil {
		t.Fatalf("get vendor: %v", err)
	}
	if !bytes.Equal(got.Credentials, sealed) {
		t.Fatalf("sealed credentials not round-tripped: %v", got.Credentials)
	}
	if !got.HasCredentials() {
		t.Fatal("HasCredentials must be true when a secret is stored")
	}

	// A vendor with no secret is valid: a local model server needs no key.
	local := &model.AIVendor{
		WorkspaceID: ws.ID, VendorKey: model.VendorOpenAICompatible,
		Name: "Local model", BaseURL: "http://127.0.0.1:8000/v1",
	}
	if err := st.Vendors().Create(ctx(), local); err != nil {
		t.Fatalf("create local vendor: %v", err)
	}
	got, err = st.Vendors().GetByID(ctx(), ws.ID, local.ID)
	if err != nil {
		t.Fatalf("get local vendor: %v", err)
	}
	if got.HasCredentials() {
		t.Fatal("a vendor without a secret must report no credentials")
	}
	if got.BaseURL != "http://127.0.0.1:8000/v1" {
		t.Fatalf("base url not persisted: %q", got.BaseURL)
	}

	list, err := st.Vendors().List(ctx(), ws.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list vendors: %v (%d)", err, len(list))
	}

	other := mustWorkspace(t, st, "globex")
	if _, err := st.Vendors().GetByID(ctx(), other.ID, v.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("vendor leaked across workspaces: %v", err)
	}
	if list, err = st.Vendors().List(ctx(), other.ID); err != nil || len(list) != 0 {
		t.Fatalf("listing crossed the tenant boundary: %v (%d)", err, len(list))
	}
}

// testVendorCredentialUpdate covers the rule that costs an API key when it
// is wrong: editing a vendor without resending the secret must not erase it.
func testVendorCredentialUpdate(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	sealed := []byte("sealed-secret-v1")
	v := mustVendor(t, st, ws.ID, "Anthropic", sealed)

	// Update with nil credentials: the stored secret must survive.
	v.Name = "Anthropic (renamed)"
	v.Credentials = nil
	if err := st.Vendors().Update(ctx(), v); err != nil {
		t.Fatalf("update vendor: %v", err)
	}
	got, err := st.Vendors().GetByID(ctx(), ws.ID, v.ID)
	if err != nil {
		t.Fatalf("get vendor: %v", err)
	}
	if got.Name != "Anthropic (renamed)" {
		t.Fatalf("rename not persisted: %q", got.Name)
	}
	if !bytes.Equal(got.Credentials, sealed) {
		t.Fatalf("editing a vendor erased its credentials: %v", got.Credentials)
	}

	// Update with new credentials: the secret is replaced.
	rotated := []byte("sealed-secret-v2")
	got.Credentials = rotated
	if err := st.Vendors().Update(ctx(), got); err != nil {
		t.Fatalf("update vendor: %v", err)
	}
	if got, err = st.Vendors().GetByID(ctx(), ws.ID, v.ID); err != nil || !bytes.Equal(got.Credentials, rotated) {
		t.Fatalf("credentials not replaced: %v %v", err, got.Credentials)
	}

	// Clearing removes the secret but keeps the vendor.
	if err := st.Vendors().ClearCredentials(ctx(), ws.ID, v.ID); err != nil {
		t.Fatalf("clear credentials: %v", err)
	}
	if got, err = st.Vendors().GetByID(ctx(), ws.ID, v.ID); err != nil || got.HasCredentials() {
		t.Fatalf("credentials not cleared: %v %v", err, got.Credentials)
	}
	// Clearing again is harmless, but clearing an unknown vendor is not.
	if err := st.Vendors().ClearCredentials(ctx(), ws.ID, v.ID); err != nil {
		t.Fatalf("clearing an already-empty vendor must succeed: %v", err)
	}
	if err := st.Vendors().ClearCredentials(ctx(), ws.ID, 999999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for an unknown vendor, got %v", err)
	}

	// A no-op update must not be mistaken for a missing row.
	if err := st.Vendors().Update(ctx(), got); err != nil {
		t.Fatalf("no-op vendor update must succeed: %v", err)
	}
	if err := st.Vendors().Update(ctx(), &model.AIVendor{ID: 999999, WorkspaceID: ws.ID, Name: "X"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update of an unknown vendor must report ErrNotFound, got %v", err)
	}
}

func testAIModels(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	v := mustVendor(t, st, ws.ID, "Anthropic", []byte("sealed"))
	m := mustAIModel(t, st, ws.ID, v.ID, "claude-sonnet-5")

	got, err := st.AIModels().GetByID(ctx(), ws.ID, m.ID)
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	if got.ModelKey != "claude-sonnet-5" || got.VendorID != v.ID {
		t.Fatalf("model not round-tripped: %+v", got)
	}
	if got.ContextWindow != 200000 || got.InputPricePer1M != 3 || got.OutputPricePer1M != 15 {
		t.Fatalf("numbers not persisted: %+v", got)
	}
	if got.Description != "notes" {
		t.Fatalf("description not persisted: %q", got.Description)
	}

	got.ModelKey = "claude-opus-5"
	got.Description = "the smart one"
	got.Status = model.StatusDisabled
	if err := st.AIModels().Update(ctx(), got); err != nil {
		t.Fatalf("update model: %v", err)
	}
	if got, err = st.AIModels().GetByID(ctx(), ws.ID, m.ID); err != nil ||
		got.ModelKey != "claude-opus-5" || got.Description != "the smart one" || got.Status != model.StatusDisabled {
		t.Fatalf("model update not persisted: %v %+v", err, got)
	}
	// No-op update must not report a missing row.
	if err := st.AIModels().Update(ctx(), got); err != nil {
		t.Fatalf("no-op model update must succeed: %v", err)
	}

	other := mustWorkspace(t, st, "globex")
	if _, err := st.AIModels().GetByID(ctx(), other.ID, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("model leaked across workspaces: %v", err)
	}

	if err := st.AIModels().Delete(ctx(), ws.ID, m.ID); err != nil {
		t.Fatalf("delete model: %v", err)
	}
	if err := st.AIModels().Delete(ctx(), ws.ID, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound on repeat delete, got %v", err)
	}
}

// testVendorDeleteGuard proves a vendor cannot be deleted out from under its
// models, which would leave them pointing at credentials that no longer exist.
func testVendorDeleteGuard(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	v := mustVendor(t, st, ws.ID, "Anthropic", []byte("sealed"))
	m := mustAIModel(t, st, ws.ID, v.ID, "claude-sonnet-5")

	if err := st.Vendors().Delete(ctx(), ws.ID, v.ID); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("expected ErrInUse while models reference the vendor, got %v", err)
	}
	if _, err := st.Vendors().GetByID(ctx(), ws.ID, v.ID); err != nil {
		t.Fatalf("vendor must survive a refused delete: %v", err)
	}

	if err := st.AIModels().Delete(ctx(), ws.ID, m.ID); err != nil {
		t.Fatalf("delete model: %v", err)
	}
	if err := st.Vendors().Delete(ctx(), ws.ID, v.ID); err != nil {
		t.Fatalf("delete vendor after its models are gone: %v", err)
	}
	if _, err := st.Vendors().GetByID(ctx(), ws.ID, v.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("vendor still present after delete: %v", err)
	}
}

// Memory is what the assistant carries between turns, in two scopes that must
// not bleed into each other: a person's context is keyed by (workspace, user),
// the workspace's working notes by workspace alone. A scope never written reads
// as empty, writing again overwrites (a memory is current, not a log), and a
// deleted owner takes their memory with them.
func testMemory(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	other := mustWorkspace(t, st, "globex")
	alice := mustUser(t, st, ws.ID, "alice@acme.test")
	bob := mustUser(t, st, ws.ID, "bob@acme.test")

	// Nothing remembered yet is empty, not an error.
	if v, err := st.Memory().UserMemory(ctx(), ws.ID, alice.ID); err != nil || v != "" {
		t.Fatalf("unwritten user memory must be empty: %v %q", err, v)
	}
	if v, err := st.Memory().WorkspaceMemory(ctx(), ws.ID); err != nil || v != "" {
		t.Fatalf("unwritten workspace memory must be empty: %v %q", err, v)
	}

	// A person's memory is their own: the same workspace, a different user, is a
	// different note.
	if err := st.Memory().SetUserMemory(ctx(), ws.ID, alice.ID, "prefers terse answers"); err != nil {
		t.Fatalf("set user memory: %v", err)
	}
	if err := st.Memory().SetUserMemory(ctx(), ws.ID, bob.ID, "likes worked examples"); err != nil {
		t.Fatalf("set user memory for another person: %v", err)
	}
	if v, err := st.Memory().UserMemory(ctx(), ws.ID, alice.ID); err != nil || v != "prefers terse answers" {
		t.Fatalf("one person's memory must not be another's: %v %q", err, v)
	}

	// Writing again overwrites: the memory is the current note, not its history.
	if err := st.Memory().SetUserMemory(ctx(), ws.ID, alice.ID, "prefers terse answers; works in UTC"); err != nil {
		t.Fatalf("overwrite user memory: %v", err)
	}
	if v, err := st.Memory().UserMemory(ctx(), ws.ID, alice.ID); err != nil || v != "prefers terse answers; works in UTC" {
		t.Fatalf("overwrite not persisted: %v %q", err, v)
	}

	// The workspace's own working notes are keyed by workspace alone, and one
	// workspace's notes are not another's.
	if err := st.Memory().SetWorkspaceMemory(ctx(), ws.ID, "list_models before set_model_status"); err != nil {
		t.Fatalf("set workspace memory: %v", err)
	}
	if v, err := st.Memory().WorkspaceMemory(ctx(), ws.ID); err != nil || v != "list_models before set_model_status" {
		t.Fatalf("workspace memory not persisted: %v %q", err, v)
	}
	if v, err := st.Memory().WorkspaceMemory(ctx(), other.ID); err != nil || v != "" {
		t.Fatalf("workspace memory leaked across workspaces: %v %q", err, v)
	}
	if err := st.Memory().SetWorkspaceMemory(ctx(), ws.ID, "always confirm before set_model_status"); err != nil {
		t.Fatalf("overwrite workspace memory: %v", err)
	}
	if v, err := st.Memory().WorkspaceMemory(ctx(), ws.ID); err != nil || v != "always confirm before set_model_status" {
		t.Fatalf("workspace memory overwrite not persisted: %v %q", err, v)
	}

	// A deleted person's memory goes with them; the workspace's own notes stay.
	if err := st.Users().Delete(ctx(), alice.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if v, err := st.Memory().UserMemory(ctx(), ws.ID, alice.ID); err != nil || v != "" {
		t.Fatalf("user memory outlived its owner: %v %q", err, v)
	}
	if v, err := st.Memory().UserMemory(ctx(), ws.ID, bob.ID); err != nil || v != "likes worked examples" {
		t.Fatalf("deleting one person disturbed another's memory: %v %q", err, v)
	}
	if v, err := st.Memory().WorkspaceMemory(ctx(), ws.ID); err != nil || v != "always confirm before set_model_status" {
		t.Fatalf("workspace memory must survive a user deletion: %v %q", err, v)
	}

	// A deleted workspace takes its working notes with it.
	if err := st.Workspaces().Delete(ctx(), ws.ID); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	if v, err := st.Memory().WorkspaceMemory(ctx(), ws.ID); err != nil || v != "" {
		t.Fatalf("workspace memory outlived its workspace: %v %q", err, v)
	}
}
