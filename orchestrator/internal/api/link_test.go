package api

import (
	"net/http"
	"testing"

	"flexie.io/sag/internal/auth"
	"flexie.io/sag/internal/model"
)

// The credential Rust holds opens one socket and nothing else.

func TestALinkTokenIsMintedForTheSignedInPerson(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("someone@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("someone@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/link/token", token, linkTokenRequest{DeviceID: "this-laptop"})
	env.expectStatus(rec, http.StatusOK)
	var body linkTokenBody
	env.decode(rec, &body)
	if body.Token == "" || body.ExpiresAt.IsZero() {
		t.Fatalf("nothing to hold: %+v", body)
	}

	claims, err := env.app.Tokens.ParseLinkToken(body.Token)
	if err != nil {
		t.Fatalf("the minted token is not a link token: %v", err)
	}
	if claims.Audience != auth.AudienceLink || claims.SessionID == 0 {
		t.Fatalf("a link token must say what it is for and which session it belongs to: %+v", claims)
	}
	// And which of the person's computers it was minted for. Without it the
	// gateway would have to guess which machine to reach, and a person signed
	// in twice makes that a coin toss.
	if claims.DeviceID != "this-laptop" {
		t.Fatalf("the token does not name the installation: %+v", claims)
	}

	// A credential for no particular computer is refused: it could not be
	// routed to anything, and issuing one would only move the failure later.
	rec = env.do(http.MethodPost, "/v1/link/token", token, linkTokenRequest{})
	env.expectStatus(rec, http.StatusBadRequest)
}

// The two credentials are not interchangeable, in either direction. One opens
// the API; the other opens a route into somebody's own network.
func TestAnAccessTokenIsNotALinkTokenAndTheReverse(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("someone@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	access, _ := env.login("someone@acme.test", "dev-Passw0rd!")

	if _, err := env.app.Tokens.ParseLinkToken(access); err == nil {
		t.Fatal("an access token was accepted as a link token")
	}

	rec := env.do(http.MethodPost, "/v1/link/token", access, linkTokenRequest{DeviceID: "this-laptop"})
	env.expectStatus(rec, http.StatusOK)
	var body linkTokenBody
	env.decode(rec, &body)

	// And the link token opens nothing on the API.
	rec = env.do(http.MethodGet, "/v1/tools", body.Token, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a link token reached the API: %d", rec.Code)
	}
}

// Signing out ends the link. The token still has an hour on it, and that is
// exactly why the session is re-checked when it is used.
func TestALinkTokenDiesWithItsSession(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("someone@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	access, refresh := env.login("someone@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/link/token", access, linkTokenRequest{DeviceID: "this-laptop"})
	env.expectStatus(rec, http.StatusOK)
	var body linkTokenBody
	env.decode(rec, &body)

	claims, err := env.app.Tokens.ParseLinkToken(body.Token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/logout", "", nil, refresh), http.StatusNoContent)

	// The signature is still good and it has not expired: what stops it is the
	// live session check the socket does when the token is presented.
	if _, err := env.app.Tokens.ParseLinkToken(body.Token); err != nil {
		t.Fatalf("the token itself should still verify: %v", err)
	}
	ok, err := env.app.Store.Sessions().ValidateAccess(t.Context(), claims.SessionID, claims.UserID, claims.WorkspaceID)
	if err != nil || ok {
		t.Fatalf("a signed-out session still validates: ok=%v err=%v", ok, err)
	}
}
