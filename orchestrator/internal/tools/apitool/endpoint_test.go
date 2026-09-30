package apitool

// What a tool reaches when its base address IS the endpoint, and where its
// token travels on a retry. Both reported from a live JWT tool pointed at a
// webhook listener.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// The base address IS the endpoint, and there is no way to say so.
//
// Reported from a real JWT tool whose base address is a webhook listener:
//
//	https://crm.example.com/listener/a1b2c3d4.../e5f6a7b8...
//
// There is nothing to append to that. The model asked with an empty path and
// was refused ("path is required"), so it asked again with "/", which is the
// only other thing that could be legal, and that CHANGED the address: the
// listener never saw the call and the site answered its own front page.
//
// Driven against a real server, because what arrived at the far end is the only
// place this can be settled.
func TestTheBaseAddressCanBeTheWholeEndpoint(t *testing.T) {
	server, got := serve(t, http.StatusOK, "application/json", `{"ok":true}`)
	base := server.URL + "/listener/a1b2c3d4/e5f6a7b8"

	s := settings(t, base, Auth{
		Kind: AuthJWTBearer, Issuer: "Uv8CBjLiwPIBQRlzNaR99SMyTT6E",
		PrivateKey: "a-shared-secret", Algorithm: AlgHS256,
		Name: HeaderAuthorization, Lifetime: 30,
	}, Policy{Mode: Denylist, Verbs: []string{"DELETE"}})

	// What the reporter configured, and what the model has to be able to say.
	out := run(t, s, `{"method":"POST","path":"","body":{"source":"SV2021","table":"FJ"}}`)
	if out["success"] == false {
		t.Fatalf("a tool whose base address is the whole endpoint cannot be called at all: %v", out["error"])
	}

	got.mu.Lock()
	path, body, auth := got.path, got.body, got.auth
	got.mu.Unlock()

	// The address the administrator typed, byte for byte. No slash added.
	if path != "/listener/a1b2c3d4/e5f6a7b8" {
		t.Fatalf("the call reached %q, not the address that was configured", path)
	}
	// The body the model sent really was sent.
	if !strings.Contains(body, `"source":"SV2021"`) {
		t.Fatalf("the body did not arrive: %q", body)
	}
	// And the JWT went with it, signed and under the configured name.
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Fatalf("no bearer token arrived: %q", auth)
	}
	claims := claimsOf(t, strings.TrimPrefix(auth, "Bearer "))
	if claims["iss"] != "Uv8CBjLiwPIBQRlzNaR99SMyTT6E" {
		t.Fatalf("the token does not carry the configured issuer: %v", claims)
	}
	if claims["exp"] == nil {
		t.Fatalf("the token carries no expiry: %v", claims)
	}
}

// A trailing slash the model asked for is still its own choice, and kept.
func TestATrailingSlashIsStillSentWhenItIsAskedFor(t *testing.T) {
	server, got := serve(t, http.StatusOK, "application/json", `{}`)
	s := settings(t, server.URL+"/listener/abc", Auth{Kind: AuthNone},
		Policy{Mode: Denylist, Verbs: []string{"DELETE"}})

	run(t, s, `{"method":"GET","path":"/"}`)
	got.mu.Lock()
	path := got.path
	got.mu.Unlock()
	if path != "/listener/abc/" {
		t.Fatalf("an asked-for trailing slash was not sent: %q", path)
	}
}

// And the model is TOLD the path may be left out, or it will never try.
func TestTheModelIsToldThePathMayBeTheBaseAddress(t *testing.T) {
	var tmpl apiTemplate
	for _, p := range tmpl.Params() {
		if p.Key != "path" {
			continue
		}
		if p.Required {
			t.Fatal("path is declared required, so a model cannot address the base endpoint")
		}
		said := strings.ToLower(p.Description)
		if !strings.Contains(said, "leave") && !strings.Contains(said, "empty") {
			t.Fatalf("nothing tells the model it may leave the path out: %q", p.Description)
		}
		return
	}
	t.Fatal("there is no path parameter at all")
}

// The retry after a 401 sends the token where the FIRST attempt sent it, and
// NOWHERE ELSE.
//
// This is the defect it closes. A JWT tool whose service wants its token under
// a name of its own was refused, the tool fetched a fresh token, and set it on
// Authorization. What reached the service on the retry was therefore the STALE
// token still sitting under the configured name (req.Clone carries it) plus a
// fresh one under a header the service does not read. The one request that
// exists to recover from a refusal was the one guaranteed to be refused again.
//
// Asserting "which header was present" is not enough to see that, and the
// first version of this test passed on the broken code for exactly that
// reason: the clone means the right header is always there. What tells them
// apart is the header that must NOT be there.
func TestTheRetryAfterARefusalUsesTheConfiguredHeaderAndNoOther(t *testing.T) {
	type attempt struct{ configured, authorization string }
	var attempts []attempt
	var lock sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		attempts = append(attempts, attempt{
			configured:    r.Header.Get("Token"),
			authorization: r.Header.Get("Authorization"),
		})
		first := len(attempts) == 1
		lock.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if first {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"expired"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	s := settings(t, server.URL, Auth{
		Kind: AuthJWTBearer, Issuer: "an-issuer", PrivateKey: "a-shared-secret",
		Algorithm: AlgHS256, Name: "Token", Lifetime: 30,
	}, Policy{Mode: Denylist, Verbs: []string{"DELETE"}})

	out := run(t, s, `{"method":"GET","path":"/thing"}`)
	if out["success"] == false {
		t.Fatalf("the retry did not recover: %v", out["error"])
	}

	lock.Lock()
	defer lock.Unlock()
	if len(attempts) != 2 {
		t.Fatalf("the refusal was not retried: %d attempts", len(attempts))
	}
	for i, a := range attempts {
		if a.configured == "" {
			t.Fatalf("attempt %d carried no token under the configured name", i+1)
		}
		// The one that tells the fix from the bug: a token under a name the
		// service does not read is a token sent nowhere.
		if a.authorization != "" {
			t.Fatalf("attempt %d also set Authorization, which this service does not read: %q",
				i+1, a.authorization)
		}
		if !strings.HasPrefix(a.configured, "eyJ") {
			t.Fatalf("attempt %d did not carry a signed token: %q", i+1, a.configured)
		}
	}
}

// The body and the query a model sent are SHOWN to the person who opens the
// call.
//
// Reported as "there is no body being sent" about a call whose arguments
// carried a whole invoice: the panel showed the verb and the path, so the body
// was invisible and read as absent. What makes it safe is where it comes from:
// Sent is read off the model's own arguments, and the credential is attached to
// the request afterwards.
func TestWhatWasSentIsShownToThePerson(t *testing.T) {
	display := apiTemplate{}.Display()

	fields := map[string]string{}
	for _, shown := range display.Sent {
		fields[shown.Field] = string(shown.As)
	}
	for field, want := range map[string]string{
		"method": "text", "path": "text",
		// As a payload, so an object is read as the JSON it is rather than
		// flattened into one line.
		"query": "body", "body": "body",
	} {
		got, ok := fields[field]
		if !ok {
			t.Fatalf("a person opening one of these calls is not shown %q: %v", field, fields)
		}
		if got != want {
			t.Fatalf("%q is shown as %q, want %q", field, got, want)
		}
	}
	// And nothing of the credential, on either side.
	for _, shown := range append(display.Sent, display.Answered...) {
		switch shown.Field {
		case "headers":
			t.Fatal("the request headers are shown, and they carry the credential")
		case "request_url":
			t.Fatal("the address is shown as a field; it belongs in Where, redacted")
		}
	}
}

// The path rule belongs to the ADDRESS, not to the credential.
//
// Reported as a doubt worth settling: the reported tool happened to use JWT,
// and a fix that only worked there would be no fix. Join is called once, at
// handler.go:66, before a single line of auth is read, so there is nothing for
// a driver to change. Driven through every driver the form offers rather than
// argued from that, because "one call site" is a reading and this is a
// measurement.
func TestEveryDriverReachesTheBaseAddressWithNoPath(t *testing.T) {
	for _, auth := range []Auth{
		{Kind: AuthNone},
		{Kind: AuthBearer, Token: "sk_live_abc"},
		{Kind: AuthAPIKey, Placement: InHeader, Name: "X-Api-Key", Key: "key_abc"},
		{Kind: AuthAPIKey, Placement: InQuery, Name: "api_key", Key: "key_abc"},
		{Kind: AuthBasic, User: "u", Password: "p"},
		{Kind: AuthJWTBearer, Issuer: "an-issuer", PrivateKey: "a-secret",
			Algorithm: AlgHS256, Name: HeaderAuthorization, Lifetime: 30},
	} {
		t.Run(auth.Kind+"/"+auth.Placement, func(t *testing.T) {
			server, got := serve(t, http.StatusOK, "application/json", `{"ok":true}`)
			s := settings(t, server.URL+"/listener/a1b2c3d4/e5f6a7b8", auth,
				Policy{Mode: Denylist, Verbs: []string{"DELETE"}})

			out := run(t, s, `{"method":"POST","body":{"a":1}}`)
			if out["success"] == false {
				t.Fatalf("%s cannot address its own base endpoint: %v", auth.Kind, out["error"])
			}
			got.mu.Lock()
			path := got.path
			got.mu.Unlock()
			if path != "/listener/a1b2c3d4/e5f6a7b8" {
				t.Fatalf("%s reached %q, not the configured address", auth.Kind, path)
			}
		})
	}
}

// And it holds for every SHAPE of base address, not just the reported one.
//
// A provider's endpoint is whatever they wrote down: a bare host, a deep path,
// a trailing slash somebody typed on purpose, a port, and a query string
// carrying an identifier (which is how plenty of listener addresses are
// issued). None of those may be rewritten by asking for no path.
func TestNoPathPreservesWhateverShapeTheAddressHas(t *testing.T) {
	for _, c := range []struct{ name, base, wantPath, wantQuery string }{
		{"a bare host", "", "/", ""},
		{"a deep path", "/listener/a1b2c3d4/e5f6a7b8", "/listener/a1b2c3d4/e5f6a7b8", ""},
		{"a trailing slash somebody typed", "/hooks/inbound/", "/hooks/inbound/", ""},
		{"an identifier in the query", "/hooks?id=9f2a&v=2", "/hooks", "id=9f2a&v=2"},
		{"a single segment", "/webhook", "/webhook", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			server, got := serve(t, http.StatusOK, "application/json", `{}`)
			s := settings(t, server.URL+c.base, Auth{Kind: AuthNone},
				Policy{Mode: Denylist, Verbs: []string{"DELETE"}})

			out := run(t, s, `{"method":"POST","body":{"a":1}}`)
			if out["success"] == false {
				t.Fatalf("%v", out["error"])
			}
			got.mu.Lock()
			path, query := got.path, got.query
			got.mu.Unlock()
			if path != c.wantPath {
				t.Fatalf("reached path %q, want %q", path, c.wantPath)
			}
			if query != c.wantQuery {
				t.Fatalf("reached query %q, want %q", query, c.wantQuery)
			}
		})
	}
}

// A base address with a QUERY STRING keeps it, and a path still goes in the
// path.
//
// Asked exactly this way: "so there could be tons of cases, like
// https://..../?querystring=whatever as a base API, so now when the agent
// provide the path /whatver/v1/whatver, do you parse the URL properly and have
// it as https://..../whatver/v1/whatver?querystring=whatever or you doing the
// dirty work of attaching whatver the agent is sending to the URL?"
//
// It parses. Every part is set on a parsed URL and the whole is re-encoded, so
// the path goes in the path and the address's own parameters survive it. The
// base used to be REFUSED outright for carrying a query at all.
func TestAQueryOnTheBaseSurvivesAPathBeingAdded(t *testing.T) {
	server, got := serve(t, http.StatusOK, "application/json", `{}`)
	s := settings(t, server.URL+"/?querystring=whatever", Auth{Kind: AuthNone},
		Policy{Mode: Denylist, Verbs: []string{"DELETE"}})

	out := run(t, s, `{"method":"GET","path":"/whatver/v1/whatver"}`)
	if out["success"] == false {
		t.Fatalf("%v", out["error"])
	}
	got.mu.Lock()
	path, query := got.path, got.query
	got.mu.Unlock()
	if path != "/whatver/v1/whatver" {
		t.Fatalf("the path arrived as %q", path)
	}
	if query != "querystring=whatever" {
		t.Fatalf("the address's own parameter arrived as %q", query)
	}
	// And it is on the address a person reads, not only on the wire.
	if url, _ := out["request_url"].(string); !strings.Contains(url, "querystring=whatever") ||
		!strings.Contains(url, "/whatver/v1/whatver") {
		t.Fatalf("the address shown is not the address called: %q", url)
	}
}

// Part of an address cannot be displaced by a call.
//
// The precedence is stated once in path.go and it matters most here: the call's
// parameters go on FIRST, so the address's own, the administrator's extras and
// the credential all overwrite them rather than the other way round. A model
// naming the identifier that makes a listener address what it is must not be
// able to replace it.
func TestNothingACallAsksForCanDisplaceTheAddressOrTheConfiguration(t *testing.T) {
	server, got := serve(t, http.StatusOK, "application/json", `{}`)
	s := reparse(t, settings(t, server.URL+"/hooks?id=9f2a", Auth{
		Kind: AuthAPIKey, Placement: InQuery, Name: "api_key", Key: "key_abc",
	}, Policy{Mode: Denylist, Verbs: []string{"DELETE"}}))
	s.ExtraQuery = "version: 2"
	s = reparse(t, s)

	// The call tries to take all three.
	out := run(t, s, `{"method":"GET","query":{"id":"mine","version":"99","api_key":"mine","page":"2"}}`)
	if out["success"] == false {
		t.Fatalf("%v", out["error"])
	}
	got.mu.Lock()
	sent, _ := url.ParseQuery(got.query)
	got.mu.Unlock()

	for name, want := range map[string]string{
		"id":      "9f2a",    // the address's own
		"version": "2",       // the administrator's
		"api_key": "key_abc", // the credential
		"page":    "2",       // the call's own, untouched
	} {
		if sent.Get(name) != want {
			t.Fatalf("%s arrived as %q, want %q (whole query: %v)", name, sent.Get(name), want, sent)
		}
	}
}

// A LIST is more than one of something, and is sent that way.
//
// It used to be JSON-encoded into the literal text "[1,2]", which no service
// reads, and there was no other way to ask for two of anything.
func TestAListBecomesARepeatedParameter(t *testing.T) {
	server, got := serve(t, http.StatusOK, "application/json", `{}`)
	s := settings(t, server.URL, Auth{Kind: AuthNone}, Policy{Mode: Denylist, Verbs: []string{"DELETE"}})

	run(t, s, `{"method":"GET","path":"/invoices","query":{"id":[1,2,"in_3"],"expand":["lines"],"q":""}}`)
	got.mu.Lock()
	sent, _ := url.ParseQuery(got.query)
	got.mu.Unlock()

	if strings.Join(sent["id"], ",") != "1,2,in_3" {
		t.Fatalf("a list arrived as %v", sent["id"])
	}
	if strings.Join(sent["expand"], ",") != "lines" {
		t.Fatalf("a one-item list arrived as %v", sent["expand"])
	}
	// An empty string is a value: "?q=" is a real request.
	if _, asked := sent["q"]; !asked {
		t.Fatalf("an empty value was dropped rather than sent: %v", sent)
	}
}

// A body goes out the way the SERVICE wants it, not the way this tool assumed.
//
// Every body used to be JSON under application/json, so an endpoint wanting a
// form, or XML, or a line of text could not be called at all.
func TestABodyIsEncodedTheWayTheServiceAsksFor(t *testing.T) {
	for _, c := range []struct{ name, args, wantType, wantBody string }{
		{
			"json by default, which is what it always did",
			`{"method":"POST","path":"/x","body":{"a":1}}`,
			"application/json", `{"a":1}`,
		},
		{
			"a form, which plenty of token and webhook routes require",
			`{"method":"POST","path":"/x","body_type":"form","body":{"grant_type":"client_credentials"}}`,
			"application/x-www-form-urlencoded", "grant_type=client_credentials",
		},
		{
			"a form with more than one of something",
			`{"method":"POST","path":"/x","body_type":"form","body":{"id":[1,2]}}`,
			"application/x-www-form-urlencoded", "id=1&id=2",
		},
		{
			"xml, sent as its own characters and not as a quoted string",
			`{"method":"POST","path":"/x","body_type":"xml","body":"<a>1</a>"}`,
			"application/xml", "<a>1</a>",
		},
		{
			"plain text",
			`{"method":"POST","path":"/x","body_type":"text","body":"hello"}`,
			"text/plain; charset=utf-8", "hello",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			server, got := serve(t, http.StatusOK, "application/json", `{}`)
			s := settings(t, server.URL, Auth{Kind: AuthNone},
				Policy{Mode: Denylist, Verbs: []string{"DELETE"}})

			out := run(t, s, c.args)
			if out["success"] == false {
				t.Fatalf("%v", out["error"])
			}
			got.mu.Lock()
			body, kind := got.body, got.header.Get("Content-Type")
			got.mu.Unlock()
			if kind != c.wantType {
				t.Fatalf("content type %q, want %q", kind, c.wantType)
			}
			if body != c.wantBody {
				t.Fatalf("body %q, want %q", body, c.wantBody)
			}
		})
	}
}

// And a body this tool cannot produce is refused, saying what it can.
func TestABodyTypeThisToolCannotProduceIsRefusedClearly(t *testing.T) {
	server, got := serve(t, http.StatusOK, "application/json", `{}`)
	s := settings(t, server.URL, Auth{Kind: AuthNone}, Policy{Mode: Denylist, Verbs: []string{"DELETE"}})

	for _, c := range []struct{ name, args, say string }{
		{"a type nobody can produce here", `{"method":"POST","path":"/x","body_type":"multipart","body":{"a":1}}`, "multipart"},
		{"a form whose body is not an object", `{"method":"POST","path":"/x","body_type":"form","body":"a=1"}`, "object"},
		{"xml whose body is not a string", `{"method":"POST","path":"/x","body_type":"xml","body":{"a":1}}`, "string"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := run(t, s, c.args)
			if out["success"] != false {
				t.Fatalf("%s was accepted", c.name)
			}
			if said, _ := out["error"].(string); !strings.Contains(said, c.say) {
				t.Fatalf("the refusal does not say what is wrong: %q", said)
			}
			// And nothing was sent.
			got.mu.Lock()
			calls := got.calls
			got.mu.Unlock()
			if calls != 0 {
				t.Fatalf("a body that could not be built still reached the service")
			}
		})
	}
}

// No body is no body, and no content type either.
//
// A GET carrying `Content-Type: application/json` and nothing after it is a
// request some servers refuse outright.
func TestACallWithNoBodySendsNoContentType(t *testing.T) {
	server, got := serve(t, http.StatusOK, "application/json", `{}`)
	s := settings(t, server.URL, Auth{Kind: AuthNone}, Policy{Mode: Denylist, Verbs: []string{"DELETE"}})

	for _, args := range []string{
		`{"method":"GET","path":"/x"}`,
		`{"method":"GET","path":"/x","body":null}`,
	} {
		run(t, s, args)
		got.mu.Lock()
		kind, body := got.header.Get("Content-Type"), got.body
		got.mu.Unlock()
		if kind != "" {
			t.Fatalf("%s sent a content type with no body: %q", args, kind)
		}
		if body != "" {
			t.Fatalf("%s sent a body: %q", args, body)
		}
	}
}

// What is reported of a call: everything, except the credential itself.
//
// Three reports got this to where it is. First the additional headers and
// query parameters were not reported at all, so an administrator could not
// tell whether they had been attached. Then everything from the configuration
// was masked, which hid a version header along with the key: "the additional
// params are not a secret, you should not mask them". And the credential was
// masked whole, where "Authorization": "****secret****" should read "Bearer
// ****secret****", because the scheme is how the header is read and is not a
// secret.
func TestACallReportsEverythingButTheCredentialItself(t *testing.T) {
	server, got := serve(t, http.StatusOK, "application/json", `{}`)
	s := settings(t, server.URL+"/v1", Auth{Kind: AuthBearer, Token: "sk_live_abc"},
		Policy{Mode: Denylist, Verbs: []string{"DELETE"}})
	s.ExtraHeaders = "X-Account: acct_123"
	s.ExtraQuery = "version: 2"
	s = reparse(t, s)

	out := run(t, s, `{"method":"GET","path":"/invoices"}`)
	if out["success"] == false {
		t.Fatalf("%v", out["error"])
	}

	headers, ok := out["request_headers"].(map[string]any)
	if !ok {
		t.Fatalf("no headers reported: %v", out)
	}
	// The administrator's own, in full. They are not a secret and there is a
	// tab for the things that are.
	if headers["X-Account"] != "acct_123" {
		t.Fatalf("an additional header was not reported as sent: %v", headers["X-Account"])
	}
	// The credential: scheme kept, secret gone.
	if headers["Authorization"] != "Bearer "+Masked {
		t.Fatalf("the credential reads %v, want %q", headers["Authorization"], "Bearer "+Masked)
	}
	// Who we are, rather than which language we are written in.
	if headers["User-Agent"] != "SAG-http-client" {
		t.Fatalf("the user agent reads %v", headers["User-Agent"])
	}
	// The administrator's query parameter is on the address shown, in full.
	shown, _ := out["request_url"].(string)
	if !strings.Contains(shown, "version=2") {
		t.Fatalf("an additional query parameter is not on the address shown: %q", shown)
	}
	// And the token itself is nowhere in what the model is handed.
	whole, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(whole), "sk_live_abc") {
		t.Fatalf("the credential is readable in what the model is handed:\n%s", whole)
	}
	// It really was sent, to the service.
	got.mu.Lock()
	sentAgent, sentAuth := got.header.Get("User-Agent"), got.auth
	got.mu.Unlock()
	if sentAgent != "SAG-http-client" {
		t.Fatalf("the service was told we are %q", sentAgent)
	}
	if sentAuth != "Bearer sk_live_abc" {
		t.Fatalf("the credential did not reach the service: %q", sentAuth)
	}
}

// A credential with no scheme is replaced whole, because there is nothing in
// it but the secret.
func TestATokenSentBareIsMaskedWhole(t *testing.T) {
	server, _ := serve(t, http.StatusOK, "application/json", `{}`)
	s := settings(t, server.URL, Auth{
		Kind: AuthJWTBearer, Issuer: "an-issuer", PrivateKey: "a-secret",
		Algorithm: AlgHS256, Name: "Token", Lifetime: 30,
	}, Policy{Mode: Denylist, Verbs: []string{"DELETE"}})

	out := run(t, s, `{"method":"GET","path":"/x"}`)
	headers, _ := out["request_headers"].(map[string]any)
	if headers["Token"] != Masked {
		t.Fatalf("a bare token reads %v, want it replaced whole", headers["Token"])
	}
}

// An administrator may name the agent, because some vendors key a rate limit
// on it. Theirs wins, which is what applying the extras last means.
func TestAnAdministratorMayNameTheAgent(t *testing.T) {
	server, got := serve(t, http.StatusOK, "application/json", `{}`)
	s := settings(t, server.URL, Auth{Kind: AuthNone}, Policy{Mode: Denylist, Verbs: []string{"DELETE"}})
	s.ExtraHeaders = "User-Agent: AcmeIntegration/3"
	s = reparse(t, s)

	run(t, s, `{"method":"GET","path":"/x"}`)
	got.mu.Lock()
	agent := got.header.Get("User-Agent")
	got.mu.Unlock()
	if agent != "AcmeIntegration/3" {
		t.Fatalf("the administrator's agent was not used: %q", agent)
	}
}

// The API tool's additional parameters are not secrets, so they are not sealed
// and the form can show them.
func TestTheAdditionalParametersAreNotSecrets(t *testing.T) {
	for _, variant := range []string{AuthNone, AuthBearer, AuthAPIKey, AuthBasic,
		AuthClientCredentials, AuthAuthorizationCode, AuthJWTBearer} {
		for _, p := range New(nil).SecretPaths(variant) {
			if p == "extra_headers" || p == "extra_query" {
				t.Fatalf("%s seals %q, so the form blanks it and nobody can read back "+
					"what they configured", variant, p)
			}
		}
	}
	// And the credential still is one, on every driver that has one.
	for variant, want := range map[string]string{
		AuthBearer: "auth.token", AuthAPIKey: "auth.key", AuthBasic: "auth.password",
		AuthClientCredentials: "auth.client_secret", AuthAuthorizationCode: "auth.client_secret",
		AuthJWTBearer: "auth.private_key",
	} {
		found := false
		for _, p := range New(nil).SecretPaths(variant) {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s does not seal %q", variant, want)
		}
	}
}
