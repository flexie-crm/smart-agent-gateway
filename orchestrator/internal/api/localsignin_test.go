package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/model"
)

// The Enter button hands out an administrative session to whoever asks. What
// makes that safe is not the handler being clever, it is that the route does not
// exist on a server and that only this machine can reach it where it does. Both
// are pinned here, along with the claim that what it issues is an ORDINARY
// session rather than something weaker.

const localOwnerEmail = "owner@localhost.fx"
const localOwnerSecret = "b8f2c1a90e4d47f6a3b5c7d9e1f2a4b6c8d0e2f4a6b8c0d2e4f6a8b0c2d4e6f8"

func personalEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t, func(cfg *config.Config) {
		cfg.Personal = true
		cfg.PersonalOwnerEmail = localOwnerEmail
		cfg.PersonalOwnerSecret = localOwnerSecret
	})
	// Seeded the way desktop mode seeds it: a real user with a real hash of a
	// real password, holding everything.
	env.createUser(localOwnerEmail, localOwnerSecret, model.PermSuperuser)
	return env
}

// fromLocalhost is what a request from the person at this computer looks like.
// httptest gives every request a non-loopback peer by default, which is why the
// accepting path has to say so explicitly.
func (e *testEnv) localPost(path, remoteAddr string) *httptest.ResponseRecorder {
	return e.localPostAs(path, remoteAddr, "")
}

func (e *testEnv) localPostAs(path, remoteAddr, token string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func TestTheLocalSignInDoesNotExistOnAServer(t *testing.T) {
	// Not gated, not disabled: absent. A handler that checks a flag can be
	// reached by a routing mistake; a route that was never registered cannot.
	env := newTestEnv(t)
	rec := env.localPost("/v1/auth/local", "127.0.0.1:5000")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d on a server deployment, want 404: the passwordless "+
			"sign-in must not exist there at all", rec.Code)
	}
}

const devSignInEmail = "dev@localhost.fx"
const devSignInSecret = "3f9a1c7e5b2d8046f1a3c5e7b9d0f2a4c6e8b0d2f4a6c8e0b2d4f6a8c0e2b4d6"

// devEnv is a working copy: `sag dev`, which is `sag server` plus the flag.
func devEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t, func(cfg *config.Config) {
		cfg.Dev = true
		cfg.DevSignInEmail = devSignInEmail
		cfg.DevSignInPassword = devSignInSecret
	})
	env.createUser(devSignInEmail, devSignInSecret, model.PermSuperuser)
	return env
}

// The whole safety argument for putting this on a working copy rests on Dev
// coming from the SUBCOMMAND. If naming an account in the environment were
// enough to create the route, a deploy file with a stray variable in it would
// open a passwordless administrative sign-in on a production server.
func TestTheDevCredentialAloneDoesNotCreateTheRoute(t *testing.T) {
	env := newTestEnv(t, func(cfg *config.Config) {
		cfg.DevSignInEmail = devSignInEmail
		cfg.DevSignInPassword = devSignInSecret
	})
	env.createUser(devSignInEmail, devSignInSecret, model.PermSuperuser)

	rec := env.localPost("/v1/auth/local", "127.0.0.1:5000")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d with only the environment set, want 404: naming a dev "+
			"account must never be what decides the route exists", rec.Code)
	}
}

func TestTheDevSignInIssuesAnOrdinarySession(t *testing.T) {
	env := devEnv(t)
	rec := env.localPost("/v1/auth/local", "127.0.0.1:5000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.AccessToken == "" {
		t.Fatal("no access token: a working copy signs in through the ordinary Login")
	}
}

// Same guard as the desktop's. A working copy is safe because only the person at
// it can reach it, so that has to be true and not merely intended.
func TestTheDevSignInRefusesAnotherMachine(t *testing.T) {
	env := devEnv(t)
	rec := env.localPost("/v1/auth/local", "203.0.113.9:5000")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d from another machine, want 403", rec.Code)
	}
}

// A wrong password is a wrong password. Nothing about dev mode weakens the
// check: if it did, the sign-in would be a second authentication path and the
// working copy would stop proving anything about production.
func TestTheDevSignInStillChecksTheCredential(t *testing.T) {
	env := newTestEnv(t, func(cfg *config.Config) {
		cfg.Dev = true
		cfg.DevSignInEmail = devSignInEmail
		cfg.DevSignInPassword = "not-the-password-that-was-seeded"
	})
	env.createUser(devSignInEmail, devSignInSecret, model.PermSuperuser)

	rec := env.localPost("/v1/auth/local", "127.0.0.1:5000")
	if rec.Code == http.StatusOK {
		t.Fatal("signed in with the wrong password: the typing is removed, the check is not")
	}
}

// Both pages read this to decide whether to say so, and a deployment saying it
// is a working copy would be the badge teaching people to ignore it.
func TestThePostureReportsWhichServerThisIs(t *testing.T) {
	for _, c := range []struct {
		name string
		env  *testEnv
		want bool
	}{
		{"a deployment", newTestEnv(t), false},
		{"a working copy", devEnv(t), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/meta", nil)
			rec := httptest.NewRecorder()
			c.env.router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Dev bool `json:"dev"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Dev != c.want {
				t.Fatalf("dev=%v, want %v", body.Dev, c.want)
			}
		})
	}
}

func TestTheLocalSignInIssuesAnOrdinarySession(t *testing.T) {
	env := personalEnv(t)
	rec := env.localPost("/v1/auth/local", "127.0.0.1:5000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.AccessToken == "" {
		t.Fatal("no access token was issued")
	}
	// The refresh cookie is what makes this a session rather than a token: the
	// same rotation, revocation and reuse detection as every other sign-in.
	var refresh *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == refreshCookie {
			refresh = c
		}
	}
	if refresh == nil {
		t.Fatal("no refresh cookie: this would be a token, not a session")
	}
	if !refresh.HttpOnly {
		t.Fatal("the refresh cookie is readable by scripts")
	}

	// And it is a session that actually works, against a route that re-checks
	// the permission live on every request.
	me := env.do(http.MethodGet, "/v1/auth/me", body.AccessToken, nil)
	if me.Code != http.StatusOK {
		t.Fatalf("the issued session could not be used: %d %s", me.Code, me.Body.String())
	}
}

func TestTheLocalSignInRefusesAnythingButThisMachine(t *testing.T) {
	env := personalEnv(t)
	// The listener is already bound to loopback, so reaching this handler from
	// elsewhere should be impossible. It is refused anyway: two things have to
	// be wrong before an administrative session leaves the machine, not one.
	for _, peer := range []string{
		"192.168.1.50:4000", // the local network
		"10.0.0.9:4000",     // the local network
		"203.0.113.7:4000",  // the internet
	} {
		rec := env.localPost("/v1/auth/local", peer)
		if rec.Code != http.StatusForbidden {
			t.Errorf("a request from %s got %d, want 403", peer, rec.Code)
		}
	}
}

func TestTheLocalSignInRefusesWhenNothingWasSeeded(t *testing.T) {
	// Desktop posture, but the owner's credential never made it into the
	// configuration. Signing somebody in here would mean signing in as nobody.
	env := newTestEnv(t, func(cfg *config.Config) { cfg.Personal = true })
	rec := env.localPost("/v1/auth/local", "127.0.0.1:5000")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 while the installation is not set up", rec.Code)
	}
}

func TestThePostureTellsThePagesWhatToShow(t *testing.T) {
	desktop := personalEnv(t)
	rec := desktop.do(http.MethodGet, "/v1/meta", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var p posture
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if !p.SingleUser || !p.LocalSignIn || !p.SingleMachine {
		t.Fatalf("a desktop reported %+v", p)
	}
	// Local models are the one capability a desktop does not always have: it can
	// only offer them if this build carries an engine. Asserted against the same
	// constant the server reads, so the test states the rule rather than the
	// answer on whichever platform it happens to run on.
	if p.LocalModels != config.EngineBundled {
		t.Fatalf("a desktop reported local_models=%v with EngineBundled=%v", p.LocalModels, config.EngineBundled)
	}

	server := newTestEnv(t)
	rec = server.do(http.MethodGet, "/v1/meta", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	// A deployment administers users, has many machines, and asks for a
	// password. Getting this backwards would hide the login form on a server.
	if p.SingleUser || p.LocalSignIn || p.SingleMachine {
		t.Fatalf("a server deployment reported %+v", p)
	}
	// And it always has local models to talk about, on every platform, because
	// machines join a deployment over the network rather than shipping inside
	// it. A server built on Windows must not hide its own fleet.
	if !p.LocalModels {
		t.Fatalf("a server deployment reported local_models=false, but machines join it over the network")
	}
}

func TestThePostureAnswersBeforeAnybodyHasSignedIn(t *testing.T) {
	env := personalEnv(t)
	// It has to: the sign-in screen decides what to render from it.
	rec := env.do(http.MethodGet, "/v1/meta", "", nil)
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("the posture requires a session, so no page can read it before signing in")
	}
}
