package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"flexie.io/sag/internal/auth"
	"flexie.io/sag/internal/model"
)

// The adversary's playbook.
//
// Each test here runs a real account-takeover attempt against the real router
// and proves it is refused. Read together they are the safety argument for the
// cookie-based session: an account cannot be taken over by reading browser
// storage, forging or tampering a token, replaying a spent one, presenting none,
// or riding another site's request. The rotation, revocation, membership,
// password-change, and disable defences live next door in api_test.go; these add
// the ones the cookie model turns on.

// The refresh token must never appear in a response body: it leaves only as the
// HttpOnly cookie, so no script, and therefore no XSS, can read it. And the
// cookie must carry the three locks that make that guarantee real.
func TestRefreshTokenNeverInBodyAndCookieIsLockedDown(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/auth/login", "", map[string]string{
		"email": "u@acme.test", "password": "dev-Passw0rd!",
	})
	env.expectStatus(rec, http.StatusOK)

	// The body carries the access token and not a whiff of a refresh token: no
	// "refresh" field, no token with the refresh prefix.
	if body := rec.Body.String(); strings.Contains(body, "refresh") || strings.Contains(body, "sag_ut_") {
		t.Fatalf("the refresh token leaked into the response body: %s", body)
	}

	// The raw Set-Cookie header carries every lock, unambiguously.
	setCookie := rec.Result().Header.Get("Set-Cookie")
	for _, must := range []string{"sag_refresh=sag_ut_", "HttpOnly", "SameSite=Lax", "Path=/v1/auth"} {
		if !strings.Contains(setCookie, must) {
			t.Fatalf("refresh cookie is missing %q: %s", must, setCookie)
		}
	}
}

// A refresh with no cookie is refused: possession of the cookie is the whole
// credential, and nothing in a header or body can stand in for it.
func TestRefreshWithoutCookieIsRefused(t *testing.T) {
	env := newTestEnv(t)
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, nil), http.StatusUnauthorized)
}

// A forged refresh cookie (right shape, never issued) is refused: the server
// looks a refresh token up by its SHA-256 hash and finds no session.
func TestForgedRefreshCookieIsRefused(t *testing.T) {
	env := newTestEnv(t)
	forged := &http.Cookie{Name: refreshCookie, Value: "sag_ut_deadbeefdeadbeefdeadbeefdeadbeef"}
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, forged), http.StatusUnauthorized)
}

// An access token this server did not sign is refused, three ways: signed with a
// secret the server does not hold, an alg:none token carrying no signature at
// all, and a genuine token with a claim tampered after signing (the identity or
// privilege swap). None validate, and the untouched genuine token still does, so
// the endpoint is really checking rather than accepting everything.
func TestForgedOrTamperedAccessTokensAreRefused(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	access, _ := env.login("u@acme.test", "dev-Passw0rd!")

	claims := func() auth.Claims {
		return auth.Claims{
			UserID: 1, WorkspaceID: 1,
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    "flexie-sag",
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
		}
	}

	// 1. Signed with a secret the server does not hold.
	wrongSigned, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims()).
		SignedString([]byte("an-attacker-secret-not-the-servers"))
	if err != nil {
		t.Fatalf("sign wrong-secret token: %v", err)
	}
	env.expectStatus(env.do(http.MethodGet, "/v1/auth/me", wrongSigned, nil), http.StatusUnauthorized)

	// 2. alg:none, the unsigned-token attack.
	noneSigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims()).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none token: %v", err)
	}
	env.expectStatus(env.do(http.MethodGet, "/v1/auth/me", noneSigned, nil), http.StatusUnauthorized)

	// 3. A genuine token, tampered after signing.
	env.expectStatus(env.do(http.MethodGet, "/v1/auth/me", tamperJWTPayload(access), nil), http.StatusUnauthorized)

	// The genuine, untouched token still works.
	env.expectStatus(env.do(http.MethodGet, "/v1/auth/me", access, nil), http.StatusOK)
}

// An access token signed with the SERVER's real secret but already expired is
// refused: a leaked short-lived token cannot be used past its 15-minute life,
// and a token with no expiry at all is refused too (expiry is required).
func TestExpiredOrUnboundedAccessTokenIsRefused(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!")

	expired, err := jwt.NewWithClaims(jwt.SigningMethodHS256, auth.Claims{
		UserID: 1, WorkspaceID: 1,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "flexie-sag",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
		},
	}).SignedString([]byte(testSessionSecret))
	if err != nil {
		t.Fatalf("sign expired token: %v", err)
	}
	env.expectStatus(env.do(http.MethodGet, "/v1/auth/me", expired, nil), http.StatusUnauthorized)

	// No expiry claim at all: refused, because a token that never dies is not one
	// this server issues.
	unbounded, err := jwt.NewWithClaims(jwt.SigningMethodHS256, auth.Claims{
		UserID: 1, WorkspaceID: 1,
		RegisteredClaims: jwt.RegisteredClaims{Issuer: "flexie-sag"},
	}).SignedString([]byte(testSessionSecret))
	if err != nil {
		t.Fatalf("sign unbounded token: %v", err)
	}
	env.expectStatus(env.do(http.MethodGet, "/v1/auth/me", unbounded, nil), http.StatusUnauthorized)
}

// A person's refresh cookie only ever refreshes into their OWN session: it is
// bound to the user it was issued for, so it cannot be turned into another
// account. Alice's cookie yields Alice, never Bob.
func TestRefreshIsBoundToItsOwnUser(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("alice@acme.test", "dev-Passw0rd!")
	env.createUser("bob@acme.test", "dev-Passw0rd!")
	_, aliceRefresh := env.login("alice@acme.test", "dev-Passw0rd!")

	rec := env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, aliceRefresh)
	env.expectStatus(rec, http.StatusOK)
	var refreshed struct {
		AccessToken string `json:"access_token"`
	}
	env.decode(rec, &refreshed)

	who := env.do(http.MethodGet, "/v1/auth/me", refreshed.AccessToken, nil)
	env.expectStatus(who, http.StatusOK)
	var me struct {
		User userBody `json:"user"`
	}
	env.decode(who, &me)
	if me.User.Email != "alice@acme.test" {
		t.Fatalf("a refresh crossed accounts: got %s", me.User.Email)
	}
}

// A single refresh token, presented by many requests at once, is redeemed
// EXACTLY once. This is the race the atomic consume closes: without it, two
// concurrent refreshes of one cookie both pass the used==0 read and both mint a
// session, silently forking a parallel session. With it, one wins and the rest
// are refused. The client bootstraps on every page load and two tabs share the
// cookie, so this race is reached in normal use, not only by an attacker.
func TestConcurrentRefreshRedeemsTokenExactlyOnce(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!")
	_, refresh := env.login("u@acme.test", "dev-Passw0rd!")

	const n = 8
	codes := make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all at once, so they genuinely race the same token
			codes[i] = env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, refresh).Code
		}(i)
	}
	close(start)
	wg.Wait()

	ok, refused := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusUnauthorized:
			refused++
		default:
			t.Fatalf("unexpected refresh status %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("one refresh token was redeemed %d times; single-use is broken under concurrency", ok)
	}
	if refused != n-1 {
		t.Fatalf("expected %d concurrent refreshes refused, got %d", n-1, refused)
	}
}

// A cross-site "simple request" cannot slip a state change past the origin
// allow-list. A text/plain body (CORS-safelisted, so it triggers no preflight)
// that would decode as JSON is refused, which is what stops a login-CSRF from
// planting the attacker's session in a victim's browser; the proper
// application/json content type with the same credentials still works, so the
// gate is the content type, not the login.
func TestNonJSONContentTypeIsRefused(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!")

	body := `{"email":"u@acme.test","password":"dev-Passw0rd!"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("a text/plain login was not refused: %d %s", rec.Code, rec.Body.String())
	}

	env.expectStatus(env.do(http.MethodPost, "/v1/auth/login", "", map[string]string{
		"email": "u@acme.test", "password": "dev-Passw0rd!",
	}), http.StatusOK)
}

// An access token stops working the instant its session is revoked, not fifteen
// minutes later at expiry: requireAuth re-checks the session live, so a logout
// takes effect on the very next request even though the token is still unexpired.
func TestAccessTokenDiesWhenItsSessionIsRevoked(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!")
	access, refresh := env.login("u@acme.test", "dev-Passw0rd!")

	// The freshly-issued token works.
	env.expectStatus(env.do(http.MethodGet, "/v1/auth/me", access, nil), http.StatusOK)

	// Logout revokes the session; the SAME access token is now refused.
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/logout", "", nil, refresh), http.StatusNoContent)
	env.expectStatus(env.do(http.MethodGet, "/v1/auth/me", access, nil), http.StatusUnauthorized)
}

// A disabled user's existing access token stops working at once, not at expiry.
func TestAccessTokenDiesWhenTheUserIsDisabled(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	victim := env.createUser("victim@acme.test", "dev-Passw0rd!")
	adminToken, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	victimToken, _ := env.login("victim@acme.test", "dev-Passw0rd!")

	env.expectStatus(env.do(http.MethodGet, "/v1/auth/me", victimToken, nil), http.StatusOK)

	env.expectStatus(env.do(http.MethodPut, "/v1/users/"+itoa(victim.ID), adminToken, map[string]string{
		"email": "victim@acme.test", "name": "Victim", "status": model.StatusDisabled,
	}), http.StatusOK)

	env.expectStatus(env.do(http.MethodGet, "/v1/auth/me", victimToken, nil), http.StatusUnauthorized)
}

// A member removed from a workspace loses access to it at once, even holding a
// still-valid access token that names it: the live check verifies membership
// every request, so there is no window where a non-member keeps acting.
func TestAccessTokenDiesWhenMembershipIsRemoved(t *testing.T) {
	env := newTestEnv(t)
	victim := env.createUser("victim@acme.test", "dev-Passw0rd!", model.PermUsersView)
	victimToken, _ := env.login("victim@acme.test", "dev-Passw0rd!")

	env.expectStatus(env.do(http.MethodGet, "/v1/users", victimToken, nil), http.StatusOK)

	// Remove the victim from every workspace (as an admin edit would).
	if err := env.app.Store.Workspaces().SetMembers(context.Background(), victim.ID, []int64{}); err != nil {
		t.Fatalf("remove membership: %v", err)
	}

	env.expectStatus(env.do(http.MethodGet, "/v1/users", victimToken, nil), http.StatusUnauthorized)
}

// Replaying a spent refresh token FROM SOMEWHERE ELSE is treated as theft: it
// revokes the whole rotation family, so the session that was rotated to (which
// was live) dies too, not just the replayed one. This is OAuth-BCP reuse
// detection: a stolen token that was already rotated cannot leave the thief with
// a surviving session.
func TestReplayingARotatedTokenFromAnotherBrowserRevokesTheFamily(t *testing.T) {
	env := newTestEnv(t)
	env.as("Mozilla/5.0 (the person's own browser)")
	env.createUser("u@acme.test", "dev-Passw0rd!")
	_, original := env.login("u@acme.test", "dev-Passw0rd!")

	// Rotate once: the original token is spent and we now hold the rotated one.
	rec := env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, original)
	env.expectStatus(rec, http.StatusOK)
	rotated := env.refreshCookie(rec)
	if rotated == nil {
		t.Fatal("rotation set no new cookie")
	}

	// A thief replays the ORIGINAL (spent) token from their own machine. The
	// grace window does not cover them: it is scoped to the browser the token
	// was issued to, and they are not it.
	env.as("curl/8.0 (the thief)")
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, original), http.StatusUnauthorized)

	// The whole family is revoked, so the rotated token, which WAS live, is dead
	// too, even back on the real browser: a thief's replay does not leave either
	// of them with a session.
	env.as("Mozilla/5.0 (the person's own browser)")
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, rotated), http.StatusUnauthorized)
}

// A rotation the client never received is not theft, and must not cost them
// their session.
//
// We can spend a refresh token and fail to deliver its replacement: the response
// dies in flight, the process is killed mid-write, a phone loses signal. The
// browser still holds the old cookie and presents it, and strict reuse detection
// answered that by revoking the whole family — permanently signing somebody out
// for a failure that was ours. In development, where a rebuild is twenty seconds
// of no server, it happened constantly (KB/08).
func TestAReplayFromTheSameBrowserIsTreatedAsALostRotation(t *testing.T) {
	env := newTestEnv(t)
	env.as("Mozilla/5.0 (the person's own browser)")
	env.createUser("u@acme.test", "dev-Passw0rd!")
	_, original := env.login("u@acme.test", "dev-Passw0rd!")

	// The rotation happens, and its response is imagined lost: we keep holding
	// the original, exactly as a browser that never received the Set-Cookie.
	rec := env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, original)
	env.expectStatus(rec, http.StatusOK)
	orphaned := env.refreshCookie(rec)

	// Presenting it again works, and hands back a fresh one.
	retry := env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, original)
	env.expectStatus(retry, http.StatusOK)
	recovered := env.refreshCookie(retry)
	if recovered == nil || recovered.Value == original.Value {
		t.Fatal("the retry did not hand back a fresh refresh token")
	}

	// The successor nobody received is revoked, so the family has exactly one
	// live token: the one the browser is actually holding.
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, orphaned), http.StatusUnauthorized)

	// And the recovered session is a working session, not a consolation prize.
	// (That last refusal must not have revoked the family either.)
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, recovered), http.StatusOK)
}

// The grace is ONE recovery, not an open door.
//
// A spent token gets a single retry, claimed atomically. Without that bound,
// anything holding the old cookie could keep redeeming it for as long as the
// window lasted, which is a stolen token that works repeatedly rather than once.
func TestALostRotationIsRecoveredOnlyOnce(t *testing.T) {
	env := newTestEnv(t)
	env.as("Mozilla/5.0 (the person's own browser)")
	env.createUser("u@acme.test", "dev-Passw0rd!")
	_, original := env.login("u@acme.test", "dev-Passw0rd!")

	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, original), http.StatusOK)
	// The first replay is the recovery.
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, original), http.StatusOK)
	// The second is not: the grace was spent, and this is reuse again.
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, original), http.StatusUnauthorized)
}

// The window is a window. Past it, a replay is theft again.
func TestAReplayAfterTheGraceWindowIsStillTheft(t *testing.T) {
	env := newTestEnv(t)
	env.as("Mozilla/5.0 (the person's own browser)")
	env.createUser("u@acme.test", "dev-Passw0rd!")
	_, original := env.login("u@acme.test", "dev-Passw0rd!")

	rec := env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, original)
	env.expectStatus(rec, http.StatusOK)
	rotated := env.refreshCookie(rec)

	// Age the spent token past the window rather than sleeping through it.
	if _, err := env.sql.DB().ExecContext(context.Background(),
		`UPDATE user_sessions SET used_at = ? WHERE used = 1`,
		time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("age the spent token: %v", err)
	}

	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, original), http.StatusUnauthorized)
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, rotated), http.StatusUnauthorized)
}

// tamperJWTPayload flips one character in a JWT's payload segment. The signature
// no longer matches the payload, which is exactly what makes an identity swap by
// editing claims impossible.
func tamperJWTPayload(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return token
	}
	p := []byte(parts[1])
	i := len(p) / 2
	if p[i] == 'A' {
		p[i] = 'B'
	} else {
		p[i] = 'A'
	}
	parts[1] = string(p)
	return strings.Join(parts, ".")
}
