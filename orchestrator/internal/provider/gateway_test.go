package provider

import (
	"context"
	"errors"
	"testing"

	"flexie.io/sag/internal/model"
)

// fakeLookup and fakeOpener stand in for the store and the keyring: the
// gateway's job is resolution and re-checking, and that is what is tested
// here. The adapters have their own wire tests.
type fakeLookup struct {
	models  map[int64]*model.AIModel
	vendors map[int64]*model.AIVendor
}

var errNotFound = errors.New("not found")

func (f *fakeLookup) AIModel(_ context.Context, workspaceID, id int64) (*model.AIModel, error) {
	m, ok := f.models[id]
	if !ok || m.WorkspaceID != workspaceID {
		return nil, errNotFound
	}
	return m, nil
}

func (f *fakeLookup) Vendor(_ context.Context, workspaceID, id int64) (*model.AIVendor, error) {
	v, ok := f.vendors[id]
	if !ok || v.WorkspaceID != workspaceID {
		return nil, errNotFound
	}
	return v, nil
}

type fakeOpener struct {
	key string
	err error
}

func (f *fakeOpener) VendorCredentials(_ context.Context, _, _ int64) (string, error) {
	return f.key, f.err
}

func newGateway(vendorKey, baseURL string, modelStatus, vendorStatus string) (*Gateway, *fakeOpener) {
	vendor := &model.AIVendor{
		ID: 1, WorkspaceID: 7, VendorKey: vendorKey,
		Name: "V", BaseURL: baseURL, Status: vendorStatus,
	}
	aiModel := &model.AIModel{
		ID: 10, WorkspaceID: 7, VendorID: 1, ModelKey: "m",
		Type: model.ModelTypeChat, ContextWindow: 100_000, Status: modelStatus,
	}
	lookup := &fakeLookup{
		models:  map[int64]*model.AIModel{10: aiModel},
		vendors: map[int64]*model.AIVendor{1: vendor},
	}
	opener := &fakeOpener{key: "secret-key"}
	return NewGateway(lookup, opener), opener
}

func TestGatewayResolvesEachVendorToAnAdapter(t *testing.T) {
	cases := []struct {
		vendorKey string
		baseURL   string
		wantName  string
	}{
		{model.VendorAnthropic, "", "anthropic"},
		{model.VendorOpenAI, "", "openai"},
		{model.VendorDeepSeek, "", "deepseek"},
		{model.VendorZAI, "", "zai"},
		{model.VendorMistral, "", "mistral"},
		{model.VendorAzureOpenAI, "https://acme.openai.azure.com", "azure-openai"},
		{model.VendorOpenAICompatible, "http://127.0.0.1:8000/v1", "openai-compatible"},
	}
	for _, tc := range cases {
		t.Run(tc.vendorKey, func(t *testing.T) {
			gw, _ := newGateway(tc.vendorKey, tc.baseURL, model.StatusActive, model.StatusActive)
			resolved, err := gw.Resolve(context.Background(), 7, 10)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if resolved.Provider.Name() != tc.wantName {
				t.Fatalf("wrong adapter: %s", resolved.Provider.Name())
			}
			if resolved.Model.ID != 10 || resolved.Vendor.ID != 1 {
				t.Fatalf("wrong resolution: %+v", resolved)
			}
		})
	}
}

// A model or vendor disabled a moment ago must stop working now. Resolution
// re-checks on every turn precisely so there is no cache to go stale.
func TestGatewayRefusesDisabledModelOrVendor(t *testing.T) {
	gw, _ := newGateway(model.VendorOpenAI, "", model.StatusDisabled, model.StatusActive)
	if _, err := gw.Resolve(context.Background(), 7, 10); !errors.Is(err, ErrModelDisabled) {
		t.Fatalf("expected ErrModelDisabled, got %v", err)
	}

	gw, _ = newGateway(model.VendorOpenAI, "", model.StatusActive, model.StatusDisabled)
	if _, err := gw.Resolve(context.Background(), 7, 10); !errors.Is(err, ErrVendorDisabled) {
		t.Fatalf("expected ErrVendorDisabled, got %v", err)
	}
}

// One workspace must never resolve another workspace's model, which would
// spend another tenant's credentials.
func TestGatewayRefusesForeignWorkspace(t *testing.T) {
	gw, _ := newGateway(model.VendorOpenAI, "", model.StatusActive, model.StatusActive)
	if _, err := gw.Resolve(context.Background(), 999, 10); err == nil {
		t.Fatal("a model from another workspace must not resolve")
	}
}

// An unreadable credential must fail the turn, not silently produce a
// provider with no key that gets a confusing 401 from the vendor.
func TestGatewayPropagatesCredentialFailure(t *testing.T) {
	gw, opener := newGateway(model.VendorOpenAI, "", model.StatusActive, model.StatusActive)
	opener.err = errors.New("credentials cannot be decrypted")

	if _, err := gw.Resolve(context.Background(), 7, 10); err == nil {
		t.Fatal("a credential failure must fail resolution")
	}
}

// Azure and self-hosted servers are nothing but an endpoint, so resolving one
// without a base URL is an error rather than a call to the wrong place.
func TestGatewayRequiresEndpointWhereItIsMandatory(t *testing.T) {
	for _, vendorKey := range []string{model.VendorAzureOpenAI, model.VendorOpenAICompatible} {
		t.Run(vendorKey, func(t *testing.T) {
			gw, _ := newGateway(vendorKey, "", model.StatusActive, model.StatusActive)
			_, err := gw.Resolve(context.Background(), 7, 10)
			if !errors.Is(err, ErrMissingEndpoint) {
				t.Fatalf("expected ErrMissingEndpoint, got %v", err)
			}
		})
	}
}

func TestGatewayRefusesUnknownVendor(t *testing.T) {
	gw, _ := newGateway("not-a-vendor", "", model.StatusActive, model.StatusActive)
	if _, err := gw.Resolve(context.Background(), 7, 10); !errors.Is(err, ErrUnknownVendor) {
		t.Fatalf("expected ErrUnknownVendor, got %v", err)
	}
}

// Every vendor key the API accepts must actually resolve to an adapter, or an
// operator could save a vendor the gateway cannot reach.
func TestEveryKnownVendorKeyHasAnAdapter(t *testing.T) {
	for _, key := range model.KnownVendorKeys {
		if key == model.VendorAnthropic {
			continue // its own adapter, not a dialect
		}
		if _, ok := DialectFor(key); !ok {
			t.Fatalf("vendor %q is offered by the API but has no dialect", key)
		}
	}
}

func TestBudgetForModel(t *testing.T) {
	m := &model.AIModel{ContextWindow: 200_000}
	if got := BudgetFor(m, 8_000); got != 192_000*CharsPerToken {
		t.Fatalf("budget: %d", got)
	}
	// An unconfigured window means we were not told, so nothing is trimmed
	// on a number we invented.
	if got := BudgetFor(&model.AIModel{}, 8_000); got != 0 {
		t.Fatalf("an unset context window must not trim: %d", got)
	}
}
