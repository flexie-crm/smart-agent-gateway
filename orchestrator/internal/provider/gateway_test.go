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

// A model's budget is its window in its own tokens, at the rate its own counts
// have shown, less the reply's room and the margin. Worked out by hand here:
// 200,000 less 8,000 is 192,000 tokens, and 95 percent of that is kept.
func TestBudgetForModel(t *testing.T) {
	// Never reported: the safe guess of 2 characters a token.
	m := &model.AIModel{ContextWindow: 200_000}
	if got := BudgetFor(m, 8_000); got != 364_800 { // 192,000 x 2 x 0.95
		t.Fatalf("an unmeasured model's budget is %d, want 364,800", got)
	}
	// Reported: 37,000 characters were 10,000 of its tokens, 3.7 a token.
	m.MeasuredChars, m.MeasuredTokens = 37_000, 10_000
	if got := BudgetFor(m, 8_000); got != 674_880 { // 192,000 x 3.7 x 0.95
		t.Fatalf("a measured model's budget is %d, want 674,880", got)
	}
	// An unconfigured window means we were not told, so nothing is trimmed
	// on a number we invented.
	if got := BudgetFor(&model.AIModel{}, 8_000); got != 0 {
		t.Fatalf("an unset context window must not trim: %d", got)
	}
}

// A model's rate is what its own counts have shown, and the guess until it has
// said anything.
func TestAModelsRateIsItsOwnCounts(t *testing.T) {
	if got := CharsPerToken(&model.AIModel{}); got != GuessCharsPerToken {
		t.Fatalf("a model that never reported is taken at %v, want the guess %v", got, GuessCharsPerToken)
	}
	if got := CharsPerToken(nil); got != GuessCharsPerToken {
		t.Fatalf("no model is taken at %v, want the guess", got)
	}
	if got := CharsPerToken(&model.AIModel{MeasuredChars: 27_030, MeasuredTokens: 7_318}); got < 3.69 || got > 3.70 {
		t.Fatalf("27,030 characters that were 7,318 tokens came out %v a token, want 3.69", got)
	}
}

// What one call may teach: a count, and one that could be the whole request.
func TestOnlyACountThatCanBeTrueIsLearnedFrom(t *testing.T) {
	for _, c := range []struct {
		chars, tokens int64
		want          bool
	}{
		{27_030, 7_318, true},    // gpt-6-sol through a bridge, measured: 3.7
		{360_000, 102_000, true}, // deepseek, measured: about 3.5
		{10_000, 0, false},       // a server that left the count out
		{0, 500, false},          // nothing counted here (the call carried files)
		{70_000, 10_000, false},  // 7 a token: no model measured is near it, so the count cannot be the whole request
		{900, 1_000, false},      // under one character a token: not a count of this request
	} {
		if got := Measurable(c.chars, c.tokens); got != c.want {
			t.Fatalf("%d characters as %d tokens: learnable %v, want %v", c.chars, c.tokens, got, c.want)
		}
	}
}
