package apitool

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The base address bounds this tool, INCLUDING where it is redirected to.
//
// The form promises exactly that ("a path the assistant asks for is resolved
// UNDER this and cannot leave it, so this is what bounds the tool"), and Join
// delivered it for the request we build. Where the answer came FROM was then
// the far end's decision, and it was followed anywhere.
//
// What that measured, before the fix: a 302 to another host was followed, the
// call was answered by that host with ok:true, request_url went on naming the
// configured API, and the other host was handed the JWT (under its own header
// name, which Go does not strip across hosts: it knows Authorization, Cookie
// and WWW-Authenticate and nothing of "Token") plus every additional header the
// administrator had configured.
func TestARedirectCannotLeaveTheConfiguredAddress(t *testing.T) {
	var sawElsewhere int
	var lock sync.Mutex
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		lock.Lock()
		sawElsewhere++
		lock.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"who":"somewhere else entirely"}`))
	}))
	defer elsewhere.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/moved":
			// Within the tool's own address: ordinary, and followed.
			http.Redirect(w, r, "/v1/invoices", http.StatusFound)
		case "/v1/invoices":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"who":"the configured api"}`))
		case "/v1/away":
			http.Redirect(w, r, elsewhere.URL+"/collect", http.StatusFound)
		case "/v1/upstairs":
			// Same host, outside the base path: the boundary is the base, not
			// the host, which is what the form says and what Join enforces.
			http.Redirect(w, r, "/admin/secrets", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()

	s := settings(t, api.URL+"/v1", Auth{
		Kind: AuthJWTBearer, Issuer: "an-issuer", PrivateKey: "a-shared-secret",
		Algorithm: AlgHS256, Name: "Token", Lifetime: 30,
	}, Policy{Mode: Denylist, Verbs: []string{"DELETE"}})
	s.ExtraHeaders = "X-Account: acct_123"
	s = reparse(t, s)

	// A redirect INSIDE the tool's address is followed, because that is
	// ordinary and refusing it would break half the APIs in the world.
	out := run(t, s, `{"method":"GET","path":"/moved"}`)
	if out["success"] == false {
		t.Fatalf("a redirect within the tool's own address was refused: %v", out["error"])
	}
	if body, _ := out["body"].(map[string]any); body["who"] != "the configured api" {
		t.Fatalf("a redirect within the address did not arrive: %v", out["body"])
	}

	// Leaving the host is refused, and the other host is never spoken to.
	out = run(t, s, `{"method":"GET","path":"/away"}`)
	if out["success"] != false {
		t.Fatalf("a redirect to another host was followed: %v", out)
	}
	said, _ := out["error"].(string)
	for _, want := range []string{"redirected outside", "credential"} {
		if !strings.Contains(said, want) {
			t.Fatalf("the refusal does not say what happened: %q", said)
		}
	}
	lock.Lock()
	reached := sawElsewhere
	lock.Unlock()
	if reached != 0 {
		t.Fatalf("the other host was called %d times, so it saw our headers", reached)
	}

	// And leaving the base PATH on the same host is refused too.
	out = run(t, s, `{"method":"GET","path":"/upstairs"}`)
	if out["success"] != false {
		t.Fatalf("a redirect out of the base path was followed: %v", out)
	}
}

// A loop is a loop, not an answer.
func TestARedirectLoopIsStoppedAndSaidSo(t *testing.T) {
	var hops int
	var lock sync.Mutex
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		hops++
		lock.Unlock()
		http.Redirect(w, r, "/v1/round", http.StatusFound)
	}))
	defer api.Close()

	s := settings(t, api.URL+"/v1", Auth{Kind: AuthNone},
		Policy{Mode: Denylist, Verbs: []string{"DELETE"}})

	out := run(t, s, `{"method":"GET","path":"/round"}`)
	if out["success"] != false {
		t.Fatalf("a redirect loop was not stopped: %v", out)
	}
	if said, _ := out["error"].(string); !strings.Contains(said, "loop") {
		t.Fatalf("the refusal does not name the loop: %q", said)
	}
	lock.Lock()
	defer lock.Unlock()
	if hops > maxRedirects+1 {
		t.Fatalf("the loop was walked %d times, want at most %d", hops, maxRedirects+1)
	}
}

// A request that never said where it may go does not get a silent exemption.
//
// This is the fail-closed half, and it is the property that makes the rest of
// it hold: the boundary rides on a request's context, so a caller that forgets
// to put one there would otherwise be back where this started.
func TestARequestWithNoBoundaryFollowsNothing(t *testing.T) {
	err := checkRedirect(httptest.NewRequest(http.MethodGet, "https://anywhere.test/x", nil), nil)
	if err == nil {
		t.Fatal("a redirect with no boundary on the request was allowed")
	}
	if !strings.Contains(err.Error(), "no address boundary") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
}
