package app_test

import (
	"context"
	"errors"
	"testing"

	"flexie.io/sag/internal/app"
)

// The one credential for weights published under terms somebody accepted.
//
// What these hold it to is not that it round-trips, which any store does, but
// the two properties that make it safe to ask a person for: it is never stored
// where a reader of the database could use it, and there is no path anywhere
// that hands it back.

func TestACredentialIsSealedAndComesBackWhole(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("owner@acme.test")

	const token = "hf_aVeryRealLookingAccessToken_0123456789"
	if err := e.app.SetLibraryCredential(ctx, token, user.ID); err != nil {
		t.Fatalf("set: %v", err)
	}

	opened, err := e.app.LibraryCredential(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if opened != token {
		t.Fatalf("the credential did not survive the round trip: %q", opened)
	}

	// And what actually sits in the row is NOT the credential. Sealing that
	// merely round-trips would round-trip just as well storing it in the clear,
	// so the bytes are looked at rather than trusted.
	sealed, err := e.app.Store.ModelLibrary().Token(ctx)
	if err != nil {
		t.Fatalf("read the sealed bytes: %v", err)
	}
	if string(sealed) == token {
		t.Fatal("the credential is stored in the clear")
	}
	if len(sealed) == 0 {
		t.Fatal("nothing was stored")
	}
}

func TestWhatAScreenLearnsNeverIncludesTheCredential(t *testing.T) {
	// Describe is a different store call from Token on purpose: a route that
	// only reports whether one exists must not be holding the secret to find
	// out, because a value never in hand cannot be logged, returned, or leaked
	// by a later mistake.
	e := newEnv(t)
	ctx := context.Background()

	before, err := e.app.DescribeLibraryCredential(ctx)
	if err != nil {
		t.Fatalf("describe with none set: %v", err)
	}
	if before.Configured {
		t.Fatal("a fresh installation claims to hold a credential")
	}

	user := e.user("owner@acme.test")
	if err := e.app.SetLibraryCredential(ctx, "hf_secret_value", user.ID); err != nil {
		t.Fatalf("set: %v", err)
	}

	after, err := e.app.DescribeLibraryCredential(ctx)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if !after.Configured {
		t.Fatal("a credential was set and the screen would say there is none")
	}
	// Who set it, because this is a named person's credential with an outside
	// service and the person who has to rotate it when it expires is a fact
	// worth keeping.
	if after.UpdatedBy == nil || *after.UpdatedBy != user.ID {
		t.Errorf("the person who set it was not recorded: %+v", after.UpdatedBy)
	}
	if after.UpdatedAt == "" {
		t.Error("when it was set was not recorded")
	}
}

func TestRotatingReplacesRatherThanRefuses(t *testing.T) {
	// Unlike the machine authority, which refuses to be replaced because doing
	// so orphans every machine holding a certificate from it. This is somebody's
	// credential with an outside service, it expires, and rotating it is the
	// only thing anybody ever does with it: an insert that would not overwrite
	// would refuse the whole point.
	e := newEnv(t)
	ctx := context.Background()
	first := e.user("first@acme.test")
	second := e.user("second@acme.test")

	if err := e.app.SetLibraryCredential(ctx, "hf_the_old_one", first.ID); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := e.app.SetLibraryCredential(ctx, "hf_the_new_one", second.ID); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	opened, err := e.app.LibraryCredential(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if opened != "hf_the_new_one" {
		t.Fatalf("rotating did not replace it: %q", opened)
	}
	described, err := e.app.DescribeLibraryCredential(ctx)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if described.UpdatedBy == nil || *described.UpdatedBy != second.ID {
		t.Error("the row still credits whoever set the previous one")
	}
}

func TestNoCredentialIsAnOrdinaryStateAndNotAFailure(t *testing.T) {
	// Most installations only ever fetch open weights. Reporting that as an
	// error would put a failure on a screen for a deployment that is working
	// exactly as intended, and would make callers branch on an error to find
	// out something ordinary.
	e := newEnv(t)
	ctx := context.Background()

	opened, err := e.app.LibraryCredential(ctx)
	if err != nil {
		t.Fatalf("an installation with no credential reported an error: %v", err)
	}
	if opened != "" {
		t.Fatalf("a credential appeared from nowhere: %q", opened)
	}
}

func TestAnEmptyCredentialIsRefusedRatherThanStored(t *testing.T) {
	// Storing blank would leave an installation that BELIEVES it has one: the
	// screen would say a credential is set, and every gated download would fail
	// on the wire with nothing to explain it.
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("owner@acme.test")

	for _, blank := range []string{"", "   ", "\t\n"} {
		if err := e.app.SetLibraryCredential(ctx, blank, user.ID); err == nil {
			t.Fatalf("a blank credential (%q) was accepted", blank)
		}
	}
	described, err := e.app.DescribeLibraryCredential(ctx)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if described.Configured {
		t.Fatal("a refused credential still marked the installation as configured")
	}
}

func TestClearingTakesItAwayForGood(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("owner@acme.test")

	if err := e.app.SetLibraryCredential(ctx, "hf_value", user.ID); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := e.app.ClearLibraryCredential(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}

	described, err := e.app.DescribeLibraryCredential(ctx)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if described.Configured {
		t.Fatal("clearing left the installation claiming to hold one")
	}
	opened, err := e.app.LibraryCredential(ctx)
	if err != nil {
		t.Fatalf("open after clear: %v", err)
	}
	if opened != "" {
		t.Fatalf("the credential survived being cleared: %q", opened)
	}
	// And clearing again is not an error: it is already gone, which is what
	// the caller wanted.
	if err := e.app.ClearLibraryCredential(ctx); err != nil {
		t.Fatalf("clearing twice: %v", err)
	}
}

func TestACredentialThatCannotBeOpenedSaysSoRatherThanNothing(t *testing.T) {
	// The failure that must not read as "there is no credential": the row is
	// there and cannot be opened, which is a key-management problem somebody has
	// to fix rather than an installation that never had one. Reported as absent,
	// every gated download would fail with nothing pointing at the cause.
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("owner@acme.test")

	// Bytes the keyring cannot open, written where a sealed credential goes.
	// This is what a row sealed by a key this deployment no longer holds looks
	// like from here.
	if err := e.app.Store.ModelLibrary().Set(ctx, []byte("not a sealed value"), user.ID); err != nil {
		t.Fatalf("write an unopenable credential: %v", err)
	}

	if _, err := e.app.LibraryCredential(ctx); !errors.Is(err, app.ErrCredentialsUnavailable) {
		t.Fatalf("an unreadable credential did not say so, got: %v", err)
	}
	// And the screen still reports one as SET, because there is one: the fault
	// is that it cannot be read, not that it is missing.
	described, err := e.app.DescribeLibraryCredential(ctx)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if !described.Configured {
		t.Error("a credential that exists but cannot be opened was reported as absent")
	}
}
