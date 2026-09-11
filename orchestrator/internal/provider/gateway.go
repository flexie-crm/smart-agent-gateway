package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"flexie.io/sag/internal/model"
)

// Resolved is a model plus the provider that can run it, ready to call.
type Resolved struct {
	Provider Provider
	Model    *model.AIModel
	Vendor   *model.AIVendor
}

// reserveTokens is the room left for the reply when trimming a transcript to
// the model's context window.
const reserveTokens = 8000

// Prepare turns a caller's request into what actually goes on the wire: it
// names the model and trims the transcript to the model's context window.
//
// It does NOT second-guess the model's capabilities. Whether a model can call
// tools or reason varies per model even within a vendor, and there is no
// reliable machine source for it, so those are the AGENT's choice: the request
// carries exactly what the agent asked for. A model that cannot honour tools or
// reasoning returns a vendor error, which the loop surfaces to the user as a
// normal failure rather than a stored capability flag guessing on its behalf.
func (r *Resolved) Prepare(req GenerateRequest) GenerateRequest {
	req.Model = r.Model.ModelKey
	req.Messages = TrimToBudget(req.Messages, BudgetFor(r.Model, reserveTokens))
	// Resolved here, once, so no adapter has to know where a value came from.
	// The narrower decision wins, which is the layered order the rest of the
	// product already uses (KB/15): whatever the caller set (the agent), then
	// the model, then the vendor. A key the caller already resolved is left
	// alone, which is how an agent overrides both.
	// Guarded, because a Resolved can legitimately carry a model without a
	// vendor (a caller that only needed the model's shape), and a settings
	// lookup must never be the thing that ends a turn.
	var modelSettings, vendorSettings model.Settings
	if r.Model != nil {
		modelSettings = r.Model.Settings
	}
	if r.Vendor != nil {
		vendorSettings = r.Vendor.Settings
	}
	req.Settings = mergeSettings(req.Settings, modelSettings, vendorSettings)
	return req
}

// mergeSettings folds bags nearest-first: the first bag to name a key wins.
func mergeSettings(bags ...model.Settings) model.Settings {
	var out model.Settings
	for _, bag := range bags {
		for k, v := range bag {
			if v == "" {
				continue
			}
			if out == nil {
				out = model.Settings{}
			}
			if _, taken := out[k]; !taken {
				out[k] = v
			}
		}
	}
	return out
}

// CredentialOpener hands back a vendor's decrypted API key. The gateway does
// not decrypt anything itself: the app layer owns the keyring, and this
// narrow interface keeps the gateway free of it.
type CredentialOpener interface {
	VendorCredentials(ctx context.Context, workspaceID, vendorID int64) (string, error)
}

// ModelLookup is the slice of the store the gateway needs.
type ModelLookup interface {
	AIModel(ctx context.Context, workspaceID, modelID int64) (*model.AIModel, error)
	Vendor(ctx context.Context, workspaceID, vendorID int64) (*model.AIVendor, error)
}

var (
	ErrModelDisabled   = errors.New("provider: model is disabled")
	ErrVendorDisabled  = errors.New("provider: vendor is disabled")
	ErrUnknownVendor   = errors.New("provider: unknown vendor")
	ErrMissingEndpoint = errors.New("provider: vendor has no endpoint")
	// ErrMachineUnreachable is a model on a machine of ours asked for by
	// something that was never told how to reach one. It means a wiring mistake
	// on our side, not a misconfigured vendor, so it says so rather than failing
	// later as a connection nobody can explain.
	ErrMachineUnreachable = errors.New("provider: there is no way to reach that machine")
)

// Gateway turns a model id into something callable. It is the only place
// that knows which adapter serves which vendor, so the agent loop never
// learns the difference.
type Gateway struct {
	lookup      ModelLookup
	credentials CredentialOpener
	// build turns a resolved vendor into a callable provider. It is the gateway's
	// own by default; the E2E harness swaps it for a scripted provider so the
	// browser tests drive a deterministic model. Production never replaces it.
	build func(*model.AIVendor, string) (Provider, error)
	// machine hands back the client for talking to a machine we run ourselves,
	// which is a mutually authenticated one (KB/35). Nil until the server wires
	// it, and a vendor that points at a machine cannot be called without it: the
	// machine will not answer a caller it cannot identify.
	machine func(nodeID int64) (*http.Client, error)
}

func NewGateway(lookup ModelLookup, credentials CredentialOpener) *Gateway {
	g := &Gateway{lookup: lookup, credentials: credentials}
	g.build = g.buildProvider
	return g
}

// UseMachineClient tells the gateway how to reach a machine of our own.
//
// It is set by the server rather than built here because the certificate both
// ends of that connection use comes from the deployment's authority, which is
// the app layer's to open, and a gateway that could mint one would be a gateway
// that had to hold a signing key.
//
// It takes WHICH machine, and it did not, which was a real fault rather than a
// tidier signature. Machines are not all reached the same way: one that joined
// is verified by chaining to our authority under the fleet name, and one added
// by hand presents a certificate it signed itself, which is verified by holding
// its exact bytes. Without the id, every machine got the fleet client, so an
// added-by-hand machine answered the control plane perfectly and then failed
// every inference call with "certificate is valid for <its hostname>, not
// machine.sag.internal". The control plane worked because it always knew the id.
func (g *Gateway) UseMachineClient(machine func(nodeID int64) (*http.Client, error)) {
	g.machine = machine
}

// UseProviderBuilder replaces how a vendor becomes a provider. It exists solely
// for the E2E harness, which wires a scripted provider in place of a real vendor
// SDK so the browser tests run against a deterministic model. Nothing in
// production calls it; the default builder is the real per-vendor adapter.
func (g *Gateway) UseProviderBuilder(b func(*model.AIVendor, string) (Provider, error)) {
	g.build = b
}

// Resolve builds a provider for a model inside a workspace. Everything is
// re-checked here, on every turn: a model or vendor disabled a second ago
// must stop working now, not when a cache expires.
func (g *Gateway) Resolve(ctx context.Context, workspaceID, modelID int64) (*Resolved, error) {
	aiModel, err := g.lookup.AIModel(ctx, workspaceID, modelID)
	if err != nil {
		return nil, err
	}
	if aiModel.Status != model.StatusActive {
		return nil, ErrModelDisabled
	}

	vendor, err := g.lookup.Vendor(ctx, workspaceID, aiModel.VendorID)
	if err != nil {
		return nil, err
	}
	if vendor.Status != model.StatusActive {
		return nil, ErrVendorDisabled
	}

	// An unreadable credential surfaces as an error rather than an empty
	// key, so a lost encryption key never becomes a confusing 401 from a
	// vendor.
	apiKey, err := g.credentials.VendorCredentials(ctx, workspaceID, vendor.ID)
	if err != nil {
		return nil, err
	}

	p, err := g.build(vendor, apiKey)
	if err != nil {
		return nil, err
	}
	return &Resolved{Provider: p, Model: aiModel, Vendor: vendor}, nil
}

// VendorModels asks a vendor what models it offers.
//
// It resolves a vendor without a model, which Resolve cannot do: what a vendor
// offers is precisely the question you ask BEFORE you have a model, and it is
// how a model id gets checked against the only authority on it.
//
// A disabled vendor is still asked. Someone editing the models of a vendor they
// have switched off is doing something reasonable, and refusing to name the
// models would only make the form useless without making anything safer.
func (g *Gateway) VendorModels(ctx context.Context, workspaceID, vendorID int64) ([]ModelInfo, error) {
	vendor, err := g.lookup.Vendor(ctx, workspaceID, vendorID)
	if err != nil {
		return nil, err
	}

	apiKey, err := g.credentials.VendorCredentials(ctx, workspaceID, vendor.ID)
	if err != nil {
		return nil, err
	}

	p, err := g.build(vendor, apiKey)
	if err != nil {
		return nil, err
	}
	return p.ListModels(ctx)
}

// buildProvider picks the adapter for a vendor. Anthropic has its own wire
// format; everything else is the OpenAI-compatible adapter with a dialect.
func (g *Gateway) buildProvider(vendor *model.AIVendor, apiKey string) (Provider, error) {
	if vendor.VendorKey == model.VendorAnthropic {
		return NewAnthropic(apiKey, vendor.BaseURL), nil
	}

	dialect, ok := DialectFor(vendor.VendorKey)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownVendor, vendor.VendorKey)
	}
	if model.RequiresBaseURL(vendor.VendorKey) && vendor.BaseURL == "" {
		return nil, fmt.Errorf("%w: %s", ErrMissingEndpoint, vendor.VendorKey)
	}

	// A machine of ours is reached over a channel both ends authenticate, so it
	// needs a client the hosted vendors do not: theirs is the ordinary one, with
	// a public authority behind it and an API key as the credential.
	var hc *http.Client
	if vendor.NodeID != 0 {
		if g.machine == nil {
			return nil, ErrMachineUnreachable
		}
		var err error
		if hc, err = g.machine(vendor.NodeID); err != nil {
			return nil, err
		}
	}
	// Which wire, from the vendor's dialect and nothing else. No model name
	// reaches this decision and none may: models turn over monthly and a table
	// of them would be wrong in both directions, refusing ones that work and
	// promising ones that no longer do.
	if dialect.Wire == WireResponses {
		return NewOpenAIResponses(dialect, apiKey, vendor.BaseURL, hc)
	}
	return NewOpenAICompatible(dialect, apiKey, vendor.BaseURL, hc)
}

// BudgetFor derives the character budget for a model's context window,
// reserving room for the reply.
func BudgetFor(m *model.AIModel, reserveTokens int) int {
	if m.ContextWindow <= 0 {
		// An unconfigured window means the operator did not tell us; not
		// trimming is safer than trimming to a number we invented.
		return 0
	}
	return BudgetChars(m.ContextWindow, reserveTokens)
}
