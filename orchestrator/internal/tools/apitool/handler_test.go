package apitool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"flexie.io/sag/internal/tool"
)

// seen is what actually arrived at the far end, which is the only place some of
// these claims can be checked: whether a credential was attached, and whether a
// refused call was refused BEFORE anything was sent.
// Written by the SERVER's goroutines, and the concurrency test sends twenty
// requests at once, so this needs a lock of its own. Without it the race
// detector reports two handlers racing each other, which is a flaky test
// rather than a defect in the tool, and a flaky test is its own defect.
type seen struct {
	mu     sync.Mutex
	calls  int
	method string
	path   string
	query  string
	auth   string
	header http.Header
	body   string
}

func serve(t *testing.T, status int, contentType, body string) (*httptest.Server, *seen) {
	t.Helper()
	got := &seen{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := json.Marshal(nil)
		if r.Body != nil {
			buf := make([]byte, 4096)
			n, _ := r.Body.Read(buf)
			raw = buf[:n]
		}
		got.mu.Lock()
		got.calls++
		got.method, got.path, got.query = r.Method, r.URL.Path, r.URL.RawQuery
		got.auth = r.Header.Get("Authorization")
		got.header = r.Header.Clone()
		got.body = string(raw)
		got.mu.Unlock()
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Header().Set("Link", `<https://api/next>; rel="next"`)
		w.Header().Set("Set-Cookie", "session=super-secret-cookie")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s, got
}

func run(t *testing.T, s Settings, args string) map[string]any {
	t.Helper()
	res, err := Handler(s, nil, nil, nil, nil)(context.Background(), tool.Call{Args: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("the tool errored: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Content, &out); err != nil {
		t.Fatalf("the result is not readable: %v (%s)", err, res.Content)
	}
	return out
}

// reparse puts settings through Parse again, which is what reads the extras
// into pairs. A test that set the text and skipped this would be asserting
// against a half-built value.
func reparse(t *testing.T, s Settings) Settings {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return out
}

func settings(t *testing.T, base string, a Auth, p Policy) Settings {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"base_url": base, "auth": a, "policy": p})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return s
}

// The ordinary case, end to end against a real server.
func TestAGetReachesTheApiWithItsParametersAndComesBackParsed(t *testing.T) {
	srv, got := serve(t, 200, "application/json", `{"data":[{"id":"in_1"}],"has_more":false}`)
	s := settings(t, srv.URL+"/v1", Auth{Kind: AuthBearer, Token: "sk_live_abc"}, Policy{Mode: Denylist, Verbs: WriteVerbs()})

	out := run(t, s, `{"method":"get","path":"/invoices","query":{"customer":"cus_1","limit":10,"paid":true}}`)

	if got.method != "GET" || got.path != "/v1/invoices" {
		t.Fatalf("the API saw %s %s", got.method, got.path)
	}
	// Numbers without a decimal tail, booleans as words: what an API expects.
	for _, want := range []string{"customer=cus_1", "limit=10", "paid=true"} {
		if !strings.Contains(got.query, want) {
			t.Errorf("the query does not carry %s: %s", want, got.query)
		}
	}
	if got.auth != "Bearer sk_live_abc" {
		t.Fatalf("the credential did not arrive: %q", got.auth)
	}
	// Parsed, not a string, so the model can use the fields.
	body, ok := out["body"].(map[string]any)
	if !ok {
		t.Fatalf("the body did not come back parsed: %#v", out["body"])
	}
	if _, ok := body["data"].([]any); !ok {
		t.Fatalf("the body's own shape was lost: %#v", body)
	}
	if out["ok"] != true {
		t.Errorf("a 200 did not read as ok: %#v", out["ok"])
	}
	// Link is how most APIs page, so it is one of the headers kept.
	headers, _ := out["headers"].(map[string]any)
	if headers["Link"] == nil {
		t.Errorf("the paging header was dropped: %#v", headers)
	}
}

// The credential NEVER comes back, whichever way it was attached.
//
// The whole promise of this tool is that an administrator gives it the key once
// and the assistant works the API without ever holding it. That promise is
// worth an assertion of its own, against every placement.
func TestTheCredentialIsNeverInWhatTheModelGetsBack(t *testing.T) {
	const secret = "sk_live_do_not_leak_this"
	cases := []struct {
		name string
		auth Auth
	}{
		{"bearer", Auth{Kind: AuthBearer, Token: secret}},
		{"api key in a header", Auth{Kind: AuthAPIKey, Key: secret, Placement: InHeader, Name: "X-Api-Key"}},
		{"api key in the query", Auth{Kind: AuthAPIKey, Key: secret, Placement: InQuery, Name: "api_key"}},
		{"basic", Auth{Kind: AuthBasic, User: "u", Password: secret}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, got := serve(t, 200, "application/json", `{"ok":true}`)
			s := settings(t, srv.URL, c.auth, Policy{Mode: Denylist})

			res, err := Handler(s, nil, nil, nil, nil)(context.Background(),
				tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/thing"}`)})
			if err != nil {
				t.Fatalf("errored: %v", err)
			}
			if got.calls != 1 {
				t.Fatalf("the API was called %d times", got.calls)
			}
			if strings.Contains(string(res.Content), secret) {
				t.Fatalf("the credential came back to the model: %s", res.Content)
			}
			// Including through request_url, which is where a key in the QUERY
			// would otherwise appear in plain sight.
			if url, _ := jsonField(res.Content, "request_url"); strings.Contains(url, secret) {
				t.Fatalf("the credential is in request_url: %s", url)
			}
			// And a cookie the API set is not handed over either.
			if strings.Contains(string(res.Content), "super-secret-cookie") {
				t.Fatalf("a Set-Cookie from the API reached the model: %s", res.Content)
			}
		})
	}
}

// A refusal by the API is an ANSWER, not a broken tool.
//
// 404 means it is not there, 429 means wait, 422 says which field was wrong.
// Every one is something the model must read and act on, and drawing a red row
// over an API behaving exactly as designed is the mistake the guide tools made.
func TestAnApiSayingNoIsAnAnswerNotAFailure(t *testing.T) {
	for _, status := range []int{400, 404, 422, 429, 500, 503} {
		srv, _ := serve(t, status, "application/json", `{"error":{"message":"no such invoice"}}`)
		s := settings(t, srv.URL, Auth{Kind: AuthNone}, Policy{Mode: Denylist})

		res, err := Handler(s, nil, nil, nil, nil)(context.Background(),
			tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/invoices/nope"}`)})
		if err != nil {
			t.Fatalf("errored: %v", err)
		}
		if res.Failed() {
			t.Fatalf("a %d was reported as a broken tool: %s", status, res.Content)
		}
		var out map[string]any
		_ = json.Unmarshal(res.Content, &out)
		if out["status"] != float64(status) {
			t.Fatalf("the status did not reach the model: %#v", out["status"])
		}
		if out["ok"] != false {
			t.Fatalf("a %d read as ok", status)
		}
		// And the API's own explanation is there to act on.
		if !strings.Contains(string(res.Content), "no such invoice") {
			t.Fatalf("the API's reason was dropped: %s", res.Content)
		}
	}
}

// A verb the policy refuses is refused BEFORE anything is sent.
//
// Asserted at the far end, because "the result says blocked" would pass just as
// well if the request had already been made and the answer thrown away.
func TestARefusedVerbNeverReachesTheApi(t *testing.T) {
	srv, got := serve(t, 200, "application/json", `{}`)
	s := settings(t, srv.URL, Auth{Kind: AuthNone}, Policy{Mode: Denylist, Verbs: WriteVerbs()})

	res, err := Handler(s, nil, nil, nil, nil)(context.Background(),
		tool.Call{Args: json.RawMessage(`{"method":"DELETE","path":"/invoices/in_1"}`)})
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	if !res.Failed() {
		t.Fatal("a refused verb was not refused")
	}
	if got.calls != 0 {
		t.Fatalf("the API was reached %d times by a call the policy refused", got.calls)
	}
	if !strings.Contains(string(res.Content), "DELETE") {
		t.Fatalf("the refusal does not name the verb: %s", res.Content)
	}
	// The control: the same call with the policy allowing it does arrive, so
	// the assertion above is about the policy and not about the plumbing.
	open := settings(t, srv.URL, Auth{Kind: AuthNone}, Policy{Mode: Denylist})
	if _, err := Handler(open, nil, nil, nil, nil)(context.Background(),
		tool.Call{Args: json.RawMessage(`{"method":"DELETE","path":"/invoices/in_1"}`)}); err != nil {
		t.Fatalf("errored: %v", err)
	}
	if got.calls != 1 {
		t.Fatalf("with DELETE allowed the API saw %d calls", got.calls)
	}
	if got.method != "DELETE" {
		t.Fatalf("the allowed verb arrived as %s", got.method)
	}
}

// A path that would leave the API never reaches anything.
func TestAPathThatEscapesNeverReachesTheApi(t *testing.T) {
	srv, got := serve(t, 200, "application/json", `{}`)
	s := settings(t, srv.URL+"/v1", Auth{Kind: AuthBearer, Token: "t"}, Policy{Mode: Denylist})

	for _, escape := range []string{
		"https://elsewhere.example.com/steal",
		"//elsewhere.example.com/steal",
		"/../../secrets",
		"%2e%2e/%2e%2e/secrets",
	} {
		args, _ := json.Marshal(map[string]any{"method": "GET", "path": escape})
		res, err := Handler(s, nil, nil, nil, nil)(context.Background(), tool.Call{Args: args})
		if err != nil {
			t.Fatalf("errored: %v", err)
		}
		if !res.Failed() {
			t.Errorf("%q was allowed: %s", escape, res.Content)
		}
	}
	if got.calls != 0 {
		t.Fatalf("an escaping path reached the server %d times", got.calls)
	}
}

// A body is sent as JSON, with the content type that says so.
func TestABodyIsSentAsJson(t *testing.T) {
	srv, got := serve(t, 201, "application/json", `{"id":"cus_new"}`)
	s := settings(t, srv.URL, Auth{Kind: AuthNone}, Policy{Mode: Allowlist, Verbs: []string{"POST"}})

	out := run(t, s, `{"method":"POST","path":"/customers","body":{"email":"a@b.test","metadata":{"k":"v"}}}`)

	if got.method != "POST" {
		t.Fatalf("method was %s", got.method)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type was %q", ct)
	}
	if !strings.Contains(got.body, `"email":"a@b.test"`) {
		t.Fatalf("the body did not arrive: %s", got.body)
	}
	if out["status"] != float64(201) {
		t.Fatalf("status = %#v", out["status"])
	}
}

// Something that is not text is described, not dumped into the context.
func TestABinaryResponseIsDescribedRatherThanDumped(t *testing.T) {
	srv, _ := serve(t, 200, "application/pdf", "%PDF-1.4 binary junk here")
	s := settings(t, srv.URL, Auth{Kind: AuthNone}, Policy{Mode: Denylist})

	out := run(t, s, `{"method":"GET","path":"/report.pdf"}`)
	// One body field, so this is the whole check rather than half of it.
	if out["body"] != nil {
		t.Fatalf("a pdf was put in the context: %#v", out["body"])
	}
	note, _ := out["body_note"].(string)
	if !strings.Contains(note, "application/pdf") {
		t.Fatalf("the note does not say what it was: %q", note)
	}
}

func jsonField(raw []byte, key string) (string, bool) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", false
	}
	s, ok := m[key].(string)
	return s, ok
}

// Any number of extra pairs reach the service, in whichever place the key goes.
//
// One field, many rows: a service wanting a key and a secret and an account id
// and a version header is ordinary, and a form with a fixed "second" pair could
// only ever carry one of them.
func TestEveryAdditionalParameterIsSentWithTheKey(t *testing.T) {
	const extra = "X-Account-Id: acct_123\nX-Api-Secret: sec_456\nX-Api-Version: 2026-01-01"

	t.Run("in the headers", func(t *testing.T) {
		srv, got := serve(t, 200, "application/json", `{}`)
		s := settings(t, srv.URL, Auth{
			Kind: AuthAPIKey, Key: "key_abc", Placement: InHeader, Name: "X-Api-Key",
		}, Policy{Mode: Denylist})
		s.ExtraHeaders = extra
		s = reparse(t, s)

		res, err := Handler(s, nil, nil, nil, nil)(context.Background(),
			tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/things"}`)})
		if err != nil {
			t.Fatalf("errored: %v", err)
		}
		for name, want := range map[string]string{
			"X-Api-Key":     "key_abc",
			"X-Account-Id":  "acct_123",
			"X-Api-Secret":  "sec_456",
			"X-Api-Version": "2026-01-01",
		} {
			if h := got.header.Get(name); h != want {
				t.Errorf("the service saw %s = %q, want %q", name, h, want)
			}
		}
		// THE CREDENTIAL does not come back. The extras DO, and that is a
		// decision, taken after the first version of this hid them: they are a
		// version, an account id, a tenant, and hiding them meant an
		// administrator could not tell whether the tool had attached what they
		// configured. A credential belongs on the Authentication tab, which is
		// what the field now says, and the consequence of putting one here
		// instead is that the assistant can read it.
		if strings.Contains(string(res.Content), "key_abc") {
			t.Errorf("the credential reached the model: %s", res.Content)
		}
		for _, sent := range []string{"acct_123", "sec_456", "2026-01-01"} {
			if !strings.Contains(string(res.Content), sent) {
				t.Errorf("%q is not reported as sent, so nobody can tell that it was: %s",
					sent, res.Content)
			}
		}
	})

	t.Run("in the query", func(t *testing.T) {
		srv, got := serve(t, 200, "application/json", `{}`)
		s := settings(t, srv.URL, Auth{
			Kind: AuthAPIKey, Key: "key_abc", Placement: InQuery, Name: "api_key",
		}, Policy{Mode: Denylist})
		s.ExtraQuery = "account: acct_123\nversion: 2026-01-01"
		s = reparse(t, s)

		res, err := Handler(s, nil, nil, nil, nil)(context.Background(),
			tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/things","query":{"limit":5}}`)})
		if err != nil {
			t.Fatalf("errored: %v", err)
		}
		for _, want := range []string{"api_key=key_abc", "account=acct_123", "version=2026-01-01", "limit=5"} {
			if !strings.Contains(got.query, want) {
				t.Errorf("the query does not carry %s: %s", want, got.query)
			}
		}
		// The shown address carries everything that was sent except the
		// credential: the model's own parameter, the administrator's, and the
		// key's NAME with its value replaced.
		url, _ := jsonField(res.Content, "request_url")
		for _, want := range []string{"limit=5", "account=acct_123", "version=2026-01-01"} {
			if !strings.Contains(url, want) {
				t.Errorf("request_url does not carry %s: %s", want, url)
			}
		}
		if strings.Contains(url, "key_abc") {
			t.Errorf("request_url carries the credential: %s", url)
		}
		if !strings.Contains(url, "api_key=") {
			t.Errorf("request_url does not even name the credential: %s", url)
		}
	})
}

// A value may contain a colon, because a URL or a timestamp in a header is
// ordinary and splitting on every colon would truncate it.
//
// Read through tool.ReadPairs, which is where the format lives: this asserts
// the behaviour a template GETS, not a copy of the rule kept here.
func TestAnAdditionalValueMayContainAColon(t *testing.T) {
	pairs, err := tool.ReadPairs("X-Callback: https://example.com:8443/hook\nX-At: 12:30:00")
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("read %d pairs", len(pairs))
	}
	if pairs[0].Value != "https://example.com:8443/hook" {
		t.Errorf("the URL was truncated: %q", pairs[0].Value)
	}
	if pairs[1].Value != "12:30:00" {
		t.Errorf("the time was truncated: %q", pairs[1].Value)
	}
}

// The extras are the API's, not the credential's, so they are sent whatever
// the authentication is, including none.
//
// That is the reason they moved off the authentication form: a version header
// or a tenant id belongs to the API, and an administrator configuring a public
// one still needs them.
func TestTheApisOwnExtrasAreSentWithNoAuthenticationAtAll(t *testing.T) {
	srv, got := serve(t, 200, "application/json", `{}`)
	s := settings(t, srv.URL, Auth{Kind: AuthNone}, Policy{Mode: Denylist})
	s.ExtraHeaders = "X-Api-Version: 2026-01-01\nX-Tenant: acme"
	s.ExtraQuery = "locale: en_GB"
	s = reparse(t, s)

	if _, err := Handler(s, nil, nil, nil, nil)(context.Background(),
		tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/things"}`)}); err != nil {
		t.Fatalf("errored: %v", err)
	}
	if v := got.header.Get("X-Api-Version"); v != "2026-01-01" {
		t.Errorf("X-Api-Version = %q", v)
	}
	if v := got.header.Get("X-Tenant"); v != "acme" {
		t.Errorf("X-Tenant = %q", v)
	}
	if !strings.Contains(got.query, "locale=en_GB") {
		t.Errorf("the query does not carry the extra: %s", got.query)
	}
}

// Headers and query parameters are two lists because where one goes is a
// property of the parameter.
//
// A single list would have to follow the key's placement, so a service wanting
// an account id in a header AND a version in the query could only ever have
// one of them.
func TestEachListGoesWhereItSaysAndNowhereElse(t *testing.T) {
	srv, got := serve(t, 200, "application/json", `{}`)
	s := settings(t, srv.URL, Auth{Kind: AuthAPIKey, Key: "k", Placement: InQuery, Name: "api_key"},
		Policy{Mode: Denylist})
	s.ExtraHeaders = "X-Account: acct_1"
	s.ExtraQuery = "version: 2"
	s = reparse(t, s)

	if _, err := Handler(s, nil, nil, nil, nil)(context.Background(),
		tool.Call{Args: json.RawMessage(`{"method":"GET","path":"/things"}`)}); err != nil {
		t.Fatalf("errored: %v", err)
	}
	// The header one is a header and is NOT in the query.
	if got.header.Get("X-Account") != "acct_1" {
		t.Errorf("the header extra did not arrive: %q", got.header.Get("X-Account"))
	}
	if strings.Contains(got.query, "X-Account") {
		t.Errorf("a header extra leaked into the query: %s", got.query)
	}
	// And the query one is in the query and is NOT a header.
	if !strings.Contains(got.query, "version=2") {
		t.Errorf("the query extra did not arrive: %s", got.query)
	}
	if got.header.Get("version") != "" {
		t.Errorf("a query extra leaked into the headers: %q", got.header.Get("version"))
	}
}
