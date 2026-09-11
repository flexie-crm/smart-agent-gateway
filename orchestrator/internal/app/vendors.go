package app

import (
	"context"
	"errors"
	"fmt"

	"flexie.io/sag/internal/provider"
)

// ErrCredentialsUnavailable means a vendor's stored secret cannot be opened,
// which happens when the encryption key that sealed it is missing from the
// keyring (a lost or misconfigured key). It is reported rather than
// swallowed: silently treating it as "no credentials" would make the gateway
// call a vendor unauthenticated and blame the vendor for the failure.
var ErrCredentialsUnavailable = errors.New("vendor credentials cannot be decrypted")

// SealCredentials encrypts a plaintext secret for storage. Sealing lives in
// the app layer so no plaintext ever reaches the store or a SQL statement.
func (a *App) SealCredentials(secret string) ([]byte, error) {
	sealed, err := a.Keyring.Seal([]byte(secret))
	if err != nil {
		return nil, fmt.Errorf("seal credentials: %w", err)
	}
	return sealed, nil
}

// VendorCredentials opens a vendor's sealed secret. It is the only path from
// a stored blob back to a usable API key, and it is called by the gateway,
// never by an HTTP handler.
func (a *App) VendorCredentials(ctx context.Context, workspaceID, vendorID int64) (string, error) {
	vendor, err := a.Store.Vendors().GetByID(ctx, workspaceID, vendorID)
	if err != nil {
		return "", err
	}
	if !vendor.HasCredentials() {
		return "", nil
	}
	plaintext, err := a.Keyring.Open(vendor.Credentials)
	if err != nil {
		// The real cause is logged; callers get a stable sentinel so they
		// cannot accidentally surface key-management details to a user.
		a.Log.Error().Err(err).Int64("vendor_id", vendorID).Msg("open vendor credentials")
		return "", ErrCredentialsUnavailable
	}
	return string(plaintext), nil
}

// VendorCatalog is what a vendor says it offers, and whether it would say.
//
// Listed is the part that matters. A vendor that cannot be asked (Azure, which
// names deployments rather than models; a vendor with no credential yet; a
// self-hosted endpoint that is down) is NOT a vendor that offers nothing, and
// the two must never collapse into one empty list.
type VendorCatalog struct {
	Listed bool                 `json:"listed"`
	Models []provider.ModelInfo `json:"models"`
}

// VendorModels asks a vendor for its catalog.
//
// Being unable to ask is not an error here, it is an answer: Listed is false
// and the caller falls back to trusting the person. Only a broken vendor row
// (an unopenable credential, an unknown vendor kind) is a real failure.
func (a *App) VendorModels(ctx context.Context, workspaceID, vendorID int64) (*VendorCatalog, error) {
	models, err := a.Gateway.VendorModels(ctx, workspaceID, vendorID)
	switch {
	case errors.Is(err, provider.ErrListingUnsupported):
		return &VendorCatalog{Listed: false, Models: []provider.ModelInfo{}}, nil
	case err != nil:
		// The vendor could not be reached, or would not answer: an expired key,
		// a server that is down, a network that is not there. That says nothing
		// about whether a model exists, so it must not be reported as if it did.
		a.Log.Warn().Err(err).Int64("vendor_id", vendorID).
			Msg("vendor did not list its models")
		return &VendorCatalog{Listed: false, Models: []provider.ModelInfo{}}, nil
	}
	return &VendorCatalog{Listed: true, Models: models}, nil
}

// ModelOffered checks a model id against the vendor's own catalog, which is the
// only authority on whether it exists.
//
// There are three answers and not two: the vendor offers it, the vendor does
// not offer it, or the vendor could not be asked. Only the middle one is a
// refusal. Refusing on "we could not ask" would mean a vendor being briefly
// unreachable stops an administrator from doing their job, and a self-hosted
// endpoint that publishes no catalog could never have a model added at all.
//
// known is what the vendor does offer, for an error message that helps rather
// than merely says no.
func (a *App) ModelOffered(ctx context.Context, workspaceID, vendorID int64, modelKey string) (checked, offered bool, known []string) {
	catalog, err := a.VendorModels(ctx, workspaceID, vendorID)
	if err != nil || !catalog.Listed {
		return false, false, nil
	}

	known = make([]string, 0, len(catalog.Models))
	for _, m := range catalog.Models {
		known = append(known, m.ID)
		if m.ID == modelKey {
			offered = true
		}
	}
	return true, offered, known
}

// RewrapVendorCredentials re-seals every credential that was sealed with a
// retired key, so a key rotation can be completed and the old key retired.
// It returns how many rows were rewrapped.
func (a *App) RewrapVendorCredentials(ctx context.Context, workspaceID int64) (int, error) {
	vendors, err := a.Store.Vendors().List(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	rewrapped := 0
	for _, vendor := range vendors {
		if !vendor.HasCredentials() || !a.Keyring.NeedsRewrap(vendor.Credentials) {
			continue
		}
		plaintext, err := a.Keyring.Open(vendor.Credentials)
		if err != nil {
			return rewrapped, fmt.Errorf("open vendor %d: %w", vendor.ID, err)
		}
		sealed, err := a.Keyring.Seal(plaintext)
		if err != nil {
			return rewrapped, fmt.Errorf("reseal vendor %d: %w", vendor.ID, err)
		}
		vendor.Credentials = sealed
		if err := a.Store.Vendors().Update(ctx, vendor); err != nil {
			return rewrapped, fmt.Errorf("store vendor %d: %w", vendor.ID, err)
		}
		rewrapped++
	}
	return rewrapped, nil
}
