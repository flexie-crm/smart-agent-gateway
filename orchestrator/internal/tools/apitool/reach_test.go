package apitool

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
)

// carried is a stand-in for the chat applications connected right now.
//
// It dials for real, to a real listener, so what is being tested is an HTTP
// request travelling over a connection somebody else opened: the same shape
// the link gives us (a net.Conn), without needing a websocket and a Rust
// client to get one.
type carried struct {
	mu      sync.Mutex
	to      string // where this "computer" can see
	asked   []string
	reasons []string
	online  bool
	fail    error
}

func (c *carried) Dial(ctx context.Context, _, _ int64, device, host string, port int, reason string) (net.Conn, error) {
	c.mu.Lock()
	c.asked = append(c.asked, fmt.Sprintf("%s->%s:%d", device, host, port))
	c.reasons = append(c.reasons, reason)
	fail, to := c.fail, c.to
	c.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", to)
}

func (c *carried) Online(_, _ int64, _ string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.online
}

func (c *carried) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.asked...)
}

// An API on somebody's own network is reached through their chat application.
//
// The same one checkbox the database and server tools offer, for the same
// reason: the address exists on an office network, this server cannot see it,
// and the chat application is installed on a machine that can.
//
// What makes this a real test rather than a shape: the request is made to an
// address nothing in this process is listening on, and it arrives at a server
// that only the stand-in machine can see.
func TestAnApiOnSomebodyElsesNetworkIsReachedThroughTheirChat(t *testing.T) {
	// The API, which only the "computer" can see.
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"host":"` + r.Host + `","path":"` + r.URL.Path +
			`","auth":"` + r.Header.Get("Authorization") + `"}`))
	}))
	defer internal.Close()

	machines := &carried{to: strings.TrimPrefix(internal.URL, "http://"), online: true}

	// Built the way the product builds it: through Config, which is what puts
	// the form's dotted key into the shape it is STORED in. Setting the field
	// by hand instead was how the first version of this test passed while the
	// reader was still broken.
	stored, err := New(nil).Config(AuthBearer, map[string]any{
		"base_url":   "http://accounts.internal.example:8080/v1",
		"auth.token": "sk_internal", "policy.mode": Denylist, "policy.verbs": "DELETE",
		template.ReachChatKey: "true",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if !strings.Contains(string(stored), `"reach"`) {
		t.Fatalf("the checkbox was not stored at all: %s", stored)
	}
	s, err := Parse(stored)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !s.ThroughChat {
		t.Fatalf("the checkbox did not survive being stored: %s", stored)
	}

	res, err := Handler(s, nil, nil, nil, machines)(context.Background(), tool.Call{
		WorkspaceID: 7, UserID: 9, DeviceID: "device-a",
		Args: json.RawMessage(`{"method":"GET","path":"/invoices"}`),
	})
	if err != nil {
		t.Fatalf("the tool errored: %v", err)
	}
	out := readResult(t, res)
	if out["success"] == false {
		t.Fatalf("the call did not go through: %v", out["error"])
	}

	// It really went over the carried connection, to the right address.
	if seen := machines.seen(); len(seen) != 1 || seen[0] != "device-a->accounts.internal.example:8080" {
		t.Fatalf("what the computer was asked to dial: %v", seen)
	}
	// And the API saw the request, with the credential attached.
	body, _ := out["body"].(map[string]any)
	if body["host"] != "accounts.internal.example:8080" {
		t.Fatalf("the API saw host %v", body["host"])
	}
	if body["path"] != "/v1/invoices" {
		t.Fatalf("the API saw path %v", body["path"])
	}
	if body["auth"] != "Bearer sk_internal" {
		t.Fatalf("the credential did not arrive: %v", body["auth"])
	}
	// The person is told what the connection is for.
	machines.mu.Lock()
	reason := machines.reasons[0]
	machines.mu.Unlock()
	if !strings.Contains(reason, "accounts.internal.example") {
		t.Fatalf("the person is not told what the connection is for: %q", reason)
	}
}

// Nobody home is a passing condition, said as one.
func TestNoChatApplicationIsAPassingCondition(t *testing.T) {
	machines := &carried{online: false}
	s := carriedSettings(t)

	res, err := Handler(s, nil, nil, nil, machines)(context.Background(), tool.Call{
		WorkspaceID: 7, UserID: 9, DeviceID: "device-a",
		Args: json.RawMessage(`{"method":"GET","path":"/x"}`),
	})
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	out := readResult(t, res)
	if out["success"] != false {
		t.Fatalf("a call with nobody to carry it reported success: %v", out)
	}
	if said, _ := out["error"].(string); !strings.Contains(said, "no chat application is connected") {
		t.Fatalf("the reason is not said: %q", said)
	}
	if res.Err != tool.ErrorTransient {
		t.Fatalf("a laptop that is away is passing, not broken: %v", res.Err)
	}
	// And nothing was dialled.
	if seen := machines.seen(); len(seen) != 0 {
		t.Fatalf("it dialled anyway: %v", seen)
	}
}

// A request with no computer behind it cannot use this at all, and says so.
//
// A browser, the worker, a scheduled run: every one of them has no device, and
// a tool whose whole point is somebody's own network has nothing to reach from.
func TestACallWithNoComputerIsRefusedClearly(t *testing.T) {
	machines := &carried{online: true}
	s := carriedSettings(t)

	res, err := Handler(s, nil, nil, nil, machines)(context.Background(), tool.Call{
		WorkspaceID: 7, UserID: 9, Args: json.RawMessage(`{"method":"GET","path":"/x"}`),
	})
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	out := readResult(t, res)
	if out["success"] != false {
		t.Fatalf("a call from nowhere was carried: %v", out)
	}
	if said, _ := out["error"].(string); !strings.Contains(said, "only be used from one") {
		t.Fatalf("the reason is not said: %q", said)
	}
}

// An installation with no link refuses the tool at BIND, so the model is never
// offered something that cannot work.
func TestAToolReachingThroughChatIsRefusedWhereThereIsNoLink(t *testing.T) {
	made, err := New(nil).Build(template.Input{
		Alias: "internal_api", Variant: AuthNone,
		Settings: map[string]any{
			"base_url":   "http://accounts.internal.example/v1",
			"reach.chat": "true", "policy.mode": Denylist, "policy.verbs": "DELETE",
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := New(nil).Bind(made.Config, tool.OwnerNone); err == nil {
		t.Fatal("an installation with no chat link bound the tool anyway")
	} else if !strings.Contains(err.Error(), "through the chat application") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
	// And with a link, it binds.
	if _, err := New(&carried{online: true}).Bind(made.Config, tool.OwnerNone); err != nil {
		t.Fatalf("a tool that can be carried was refused: %v", err)
	}
}

// A token obtained over one person's network is never handed to another's.
//
// Two computers answer for the same host name and are different servers. The
// cache is keyed on the credential, so without the network in that key the
// first person's token is handed to the second.
//
// Driven through the HANDLER, with one shared cache, because asking keyOf
// directly proves only that it can tell two keys apart and not that the caller
// ever passes the second one. The first version of this test did exactly that
// and passed with the wiring removed.
func TestATokenIsNotSharedAcrossTwoPeoplesNetworks(t *testing.T) {
	// Each "computer" has its own token endpoint and its own API, both
	// answering for the same host names.
	type network struct {
		token, api *httptest.Server
		issued     string
		saw        []string
		mu         sync.Mutex
	}
	build := func(name string) *network {
		n := &network{issued: "token-from-" + name}
		n.token = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"` + n.issued + `","expires_in":3600}`))
		}))
		n.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.mu.Lock()
			n.saw = append(n.saw, r.Header.Get("Authorization"))
			n.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}))
		return n
	}
	a, b := build("a"), build("b")
	defer func() { a.token.Close(); a.api.Close(); b.token.Close(); b.api.Close() }()

	// Each device sees its own pair of servers, under the same host names.
	bare := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }
	machines := &routed{to: map[string]map[string]string{
		"device-a": {"api.internal.example": bare(a.api), "auth.internal.example": bare(a.token)},
		"device-b": {"api.internal.example": bare(b.api), "auth.internal.example": bare(b.token)},
	}}

	stored, err := New(nil).Config(AuthClientCredentials, map[string]any{
		"base_url": "http://api.internal.example/v1", "auth.token_url": "http://auth.internal.example/token",
		"auth.client_id": "a-client", "auth.client_secret": "a-secret",
		"policy.mode": Denylist, "policy.verbs": "DELETE",
		template.ReachChatKey: "true",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	s, err := Parse(stored)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// ONE cache, shared, which is the whole point: it is the template's.
	cache := newTokens(nil)
	handle := Handler(s, nil, cache, nil, machines)
	for _, device := range []string{"device-a", "device-b"} {
		res, err := handle(context.Background(), tool.Call{
			WorkspaceID: 7, UserID: 9, DeviceID: device,
			Args: json.RawMessage(`{"method":"GET","path":"/x"}`),
		})
		if err != nil {
			t.Fatalf("%s: %v", device, err)
		}
		if out := readResult(t, res); out["success"] == false {
			t.Fatalf("%s: %v", device, out["error"])
		}
	}

	a.mu.Lock()
	sawA := append([]string(nil), a.saw...)
	a.mu.Unlock()
	b.mu.Lock()
	sawB := append([]string(nil), b.saw...)
	b.mu.Unlock()

	if len(sawA) != 1 || sawA[0] != "Bearer token-from-a" {
		t.Fatalf("the first network saw %v", sawA)
	}
	if len(sawB) != 1 || sawB[0] != "Bearer token-from-b" {
		t.Fatalf("the second network was handed %v: a token from somebody else's server", sawB)
	}
}

// routed is a machines stand-in that sends each device to its own network, so
// two people's computers really are two different places answering for the
// same host names.
type routed struct {
	to map[string]map[string]string // device -> host -> the real address
}

func (r *routed) Dial(ctx context.Context, _, _ int64, device, host string, _ int, _ string) (net.Conn, error) {
	hosts, ok := r.to[device]
	if !ok {
		return nil, fmt.Errorf("no computer called %q", device)
	}
	address, ok := hosts[host]
	if !ok {
		return nil, fmt.Errorf("%q cannot see %q", device, host)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", address)
}

func (r *routed) Online(_, _ int64, device string) bool {
	_, ok := r.to[device]
	return ok
}

// The form offers the checkbox, on the section the address is on.
func TestTheApiFormOffersTheChatReach(t *testing.T) {
	sections, err := New(nil).Fields(AuthNone)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range sections {
		for _, f := range section.Fields {
			if f.Key != template.ReachChatKey {
				continue
			}
			if section.Title != "The API" {
				t.Fatalf("the checkbox is on %q, not beside the address", section.Title)
			}
			if f.Type != template.FieldCheckbox {
				t.Fatalf("it is a %q", f.Type)
			}
			if !strings.Contains(strings.ToLower(f.Help), "api") {
				t.Fatalf("its wording does not name what it applies to: %q", f.Help)
			}
			return
		}
	}
	t.Fatal("the API form offers no way to reach an API on somebody's own network")
}

// readResult reads a handler's answer the way run() does, for the cases that
// need their own tool.Call rather than run()'s.
func readResult(t *testing.T, res tool.Result) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(res.Content, &out); err != nil {
		t.Fatalf("the result is not readable: %v (%s)", err, res.Content)
	}
	return out
}

// carriedSettings is an API reached through the chat application, built the way
// the product builds one: through Config, so the checkbox goes through the
// shape it is actually stored in.
func carriedSettings(t *testing.T) Settings {
	t.Helper()
	stored, err := New(nil).Config(AuthNone, map[string]any{
		"base_url":    "http://accounts.internal.example/v1",
		"policy.mode": Denylist, "policy.verbs": "DELETE",
		template.ReachChatKey: "true",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	s, err := Parse(stored)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !s.ThroughChat {
		t.Fatalf("the checkbox did not survive being stored: %s", stored)
	}
	return s
}
