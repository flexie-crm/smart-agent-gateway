package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"flexie.io/sag/internal/store"
)

// The one credential this deployment uses to fetch weights published under
// terms somebody had to accept.
//
// # Why it is central and not per machine
//
// It belongs to a PERSON'S account with the library, not to a box. The same
// token is what every machine would present, so holding it per machine means
// pasting it again for every GPU racked and rotating it in as many places, and
// a fleet where one machine has been forgotten is a fleet where one machine
// mysteriously cannot fetch what the others can.
//
// # What we do not claim to do
//
// Accepting a publisher's terms is between that person and whoever published
// the weights, made under their own account. There is no way to do it on
// somebody's behalf that would not amount to signing in their name, and this
// deliberately does not try: what it holds is the PROOF that they accepted,
// which is a credential they minted themselves and chose to give us.

// ErrNoLibraryCredential means no credential has been set for the deployment.
var ErrNoLibraryCredential = errors.New("no model library credential is configured")

// LibraryCredential is what a screen may know about it: that there is one, and
// who put it there. Never the credential.
type LibraryCredential struct {
	Configured bool   `json:"configured"`
	UpdatedBy  *int64 `json:"updated_by,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

// DescribeLibraryCredential answers what a screen needs WITHOUT reading the
// secret.
//
// Deliberately a different call from the one that opens it: a route that only
// wants to say "one is set" must not be holding the credential to find that
// out, because a value never in hand is a value that cannot be logged,
// returned, or leaked by a later mistake.
func (a *App) DescribeLibraryCredential(ctx context.Context) (LibraryCredential, error) {
	by, at, err := a.Store.ModelLibrary().Describe(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return LibraryCredential{}, nil
	}
	if err != nil {
		return LibraryCredential{}, err
	}
	return LibraryCredential{
		Configured: true,
		UpdatedBy:  by,
		UpdatedAt:  at.UTC().Format(time.RFC3339),
	}, nil
}

// LibraryCredential opens the stored credential.
//
// The empty string with no error means none is set, which is the ordinary
// state of a deployment that only fetches open weights, and NOT a failure: a
// machine with no credential simply cannot take a gated model, and says so
// where somebody is looking rather than failing on the wire.
func (a *App) LibraryCredential(ctx context.Context) (string, error) {
	sealed, err := a.Store.ModelLibrary().Token(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	plaintext, err := a.Keyring.Open(sealed)
	if err != nil {
		// The real cause is logged; callers get a stable sentinel so key
		// management never reaches a person through an error string.
		a.Log.Error().Err(err).Msg("open the model library credential")
		return "", ErrCredentialsUnavailable
	}
	return string(plaintext), nil
}

// SetLibraryCredential stores it, sealed, replacing whatever was there.
//
// An upsert, unlike the machine authority: this is somebody's credential with
// an outside service, it expires, and rotating it is the only thing anybody
// ever does with it. Refusing to replace would refuse the whole point.
func (a *App) SetLibraryCredential(ctx context.Context, token string, by int64) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("a credential cannot be blank")
	}
	sealed, err := a.Keyring.Seal([]byte(token))
	if err != nil {
		return fmt.Errorf("seal the model library credential: %w", err)
	}
	if err := a.Store.ModelLibrary().Set(ctx, sealed, by); err != nil {
		return err
	}
	a.Log.Info().Int64("user", by).Msg("the model library credential was set")
	return nil
}

// ClearLibraryCredential removes it, which is how a deployment stops being able
// to fetch gated weights at all.
func (a *App) ClearLibraryCredential(ctx context.Context) error {
	if err := a.Store.ModelLibrary().Clear(ctx); err != nil {
		return err
	}
	a.Log.Info().Msg("the model library credential was removed")
	return nil
}
