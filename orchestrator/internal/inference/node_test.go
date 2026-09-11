package inference

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"flexie.io/sag/internal/nodeca"
)

// The fake machine these tests talk to is a REAL one as far as the connection is
// concerned: it holds a certificate from an authority, it demands one back, and
// the client is built the way the server builds it. So what is exercised here is
// not only the control calls but the channel they travel on, which is the part
// that has to keep working when a machine is on a public address.

const testNodeID = "nd-test"

// fleet is an authority with the two identities that face each other across a
// connection: what a machine serves with, and what we call with.
type fleet struct {
	authority *nodeca.Authority
	machine   tls.Certificate
	ours      tls.Certificate
}

func newFleet(t *testing.T, nodeID string) *fleet {
	t.Helper()
	authority, err := nodeca.Create(time.Now())
	if err != nil {
		t.Fatalf("authority: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("machine key: %v", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: nodeID}}, key)
	if err != nil {
		t.Fatalf("certificate request: %v", err)
	}
	certPEM, err := authority.Issue(csr, nodeID, []string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("encode machine key: %v", err)
	}
	machine, err := tls.X509KeyPair(certPEM,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatalf("machine identity: %v", err)
	}

	ours, err := authority.IssueClient("sag-orchestrator", time.Now())
	if err != nil {
		t.Fatalf("our identity: %v", err)
	}
	return &fleet{authority: authority, machine: machine, ours: ours}
}

func TestControlRootIsTheSiblingOfTheInferenceURL(t *testing.T) {
	// An administrator configures ONE address for a node, the one inference
	// goes to. Asking for the same machine twice is how the two drift apart.
	cases := []struct {
		in, want string
	}{
		{"https://box:8081/v1", "https://box:8081"},
		{"https://box:8081/v1/", "https://box:8081"},
		{"https://box:8081", "https://box:8081"},
		{"https://box:8081/", "https://box:8081"},
		{"https://gpu.internal/v1", "https://gpu.internal"},
		// A node behind a path prefix keeps the prefix and loses only the
		// version segment.
		{"https://proxy/nodes/a/v1", "https://proxy/nodes/a"},
	}
	for _, c := range cases {
		got, err := controlRoot(c.in)
		if err != nil {
			t.Fatalf("controlRoot(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("controlRoot(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAnAddressThatIsNotOneIsRefused(t *testing.T) {
	// http among them: a machine serves nothing without a certificate now, so a
	// plain address is one no call could ever be made on.
	for _, bad := range []string{"", "   ", "box:8081", "ftp://box/v1", "/v1", "://",
		"http://box:8081/v1"} {
		if _, err := controlRoot(bad); err == nil {
			t.Errorf("controlRoot(%q) was accepted", bad)
		}
	}
}

func TestANodeWithoutAKeyIsRefused(t *testing.T) {
	// The key is the whole boundary in front of a node (KB/35). A client built
	// without one would send unauthenticated requests that all fail, which
	// reads as a broken node rather than a missing credential.
	if _, err := New("https://box:8081/v1", "", dialWith(&tls.Config{MinVersion: tls.VersionTLS13})); err == nil {
		t.Fatal("a node with no key was accepted")
	}
}

func TestANodeWithNothingToAuthenticateItIsRefused(t *testing.T) {
	// A machine will not answer a caller that cannot prove who it is, and the
	// client is what carries that proof, so a Node built without one could only
	// fail on the wire. Failing here says what is actually wrong.
	if _, err := New("https://box:8081/v1", "test-key", nil); err == nil {
		t.Fatal("a node client with no identity was accepted")
	}
}

// dialWith is a caller's client, which is what a Node borrows rather than
// builds. Only tests construct one this plainly: the server keeps one per
// machine (see app.machineTrust), because that is the object whose lifetime a
// connection pool must follow.
func dialWith(cfg *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
}

// node stands up a fake machine, serving with a real certificate and demanding
// one back, and hands back a client pointed at it.
func node(t *testing.T, handler http.HandlerFunc) *Node {
	t.Helper()
	f := newFleet(t, testNodeID)

	server := httptest.NewUnstartedServer(handler)
	server.TLS = f.authority.ServerTLS(f.machine)
	server.StartTLS()
	t.Cleanup(server.Close)

	n, err := New(server.URL+"/v1", "test-key", dialWith(f.authority.ClientTLS(f.ours, testNodeID)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return n
}

func TestEveryCallCarriesTheNodesKey(t *testing.T) {
	var seen string
	n := node(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	})

	if _, err := n.Info(context.Background()); err != nil {
		t.Fatalf("Info: %v", err)
	}
	if seen != "Bearer test-key" {
		t.Errorf("authorization was %q", seen)
	}
}

func TestTheControlPathsAreTheOnesTheNodeServes(t *testing.T) {
	// A path renamed on one side and not the other is a feature that fails only
	// against a real node, so the whole surface is asserted here in one place.
	var got string
	n := node(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path
		// The list calls decode into a slice and the rest into an object, so the
		// fake answers the shape each one asked for. Only a GET lists: POST to
		// the same path starts one download and answers with it.
		listing := r.Method == http.MethodGet && (strings.HasSuffix(r.URL.Path, "/models") ||
			strings.HasSuffix(r.URL.Path, "/pulls") ||
			strings.HasSuffix(r.URL.Path, "/search"))
		if listing {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	ctx := context.Background()

	cases := []struct {
		call func() error
		want string
	}{
		{func() error { _, err := n.Info(ctx); return err }, "GET /node"},
		{func() error { _, err := n.Models(ctx); return err }, "GET /node/models"},
		{func() error { _, err := n.Model(ctx, "u1"); return err }, "GET /node/models/u1"},
		{func() error { _, err := n.Form(ctx, "u1"); return err }, "GET /node/models/u1/form"},
		{func() error { _, err := n.Load(ctx, "u1"); return err }, "POST /node/models/u1/load"},
		{func() error { _, err := n.Unload(ctx, "u1"); return err }, "POST /node/models/u1/unload"},
		{func() error { _, err := n.Delete(ctx, "u1"); return err }, "DELETE /node/models/u1"},
		{func() error { _, err := n.SaveSettings(ctx, "u1", nil); return err }, "PUT /node/models/u1/settings"},
		{func() error { _, err := n.Search(ctx, "q", 5); return err }, "GET /node/library/search"},
		{func() error { _, err := n.Describe(ctx, "v/m", "", ""); return err }, "GET /node/library/describe"},
		{func() error { _, err := n.Pulls(ctx); return err }, "GET /node/pulls"},
		{func() error { _, err := n.Pull(ctx, "p1"); return err }, "GET /node/pulls/p1"},
		{func() error { _, err := n.StartPull(ctx, "v/m", "", ""); return err }, "POST /node/pulls"},
		// Three verbs, because they are three different things: stopping keeps
		// what arrived, resuming continues it, deleting throws the bytes away.
		{func() error { _, err := n.CancelPull(ctx, "p1"); return err }, "POST /node/pulls/p1/cancel"},
		{func() error { _, err := n.ResumePull(ctx, "p1"); return err }, "POST /node/pulls/p1/resume"},
		{func() error { return n.DeletePull(ctx, "p1") }, "DELETE /node/pulls/p1"},
	}
	for _, c := range cases {
		if err := c.call(); err != nil {
			t.Fatalf("%s: %v", c.want, err)
		}
		if got != c.want {
			t.Errorf("called %q, want %q", got, c.want)
		}
	}
}

func TestASearchSendsItsQuery(t *testing.T) {
	var query string
	n := node(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`[]`))
	})
	if _, err := n.Search(context.Background(), "a phrase & more", 7); err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Encoded, not concatenated: an ampersand in what somebody typed must not
	// become another parameter.
	if !strings.Contains(query, "q=a+phrase+%26+more") || !strings.Contains(query, "limit=7") {
		t.Errorf("query was %q", query)
	}
}

func TestDescribeSendsOnlyWhatItWasGiven(t *testing.T) {
	var query string
	n := node(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	})
	ctx := context.Background()

	if _, err := n.Describe(ctx, "v/m", "", ""); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if query != "repo=v%2Fm" {
		t.Errorf("a blank revision or file was sent: %q", query)
	}

	if _, err := n.Describe(ctx, "v/m", "abc", "w.gguf"); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for _, want := range []string{"repo=v%2Fm", "revision=abc", "file=w.gguf"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q is missing %q", query, want)
		}
	}
}

func TestAModelIsReadWholeIncludingItsHandleAndFacts(t *testing.T) {
	n := node(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{
			"uid":"48e61d05","repo":"Qwen/Qwen3-0.6B","revision":"c1899de",
			"name":"Qwen3-0.6B","handle":"Qwen3-0.6B","kind":"chat",
			"facts":{"architecture":"Qwen3ForCausalLM","context_length":40960,
			         "license":"apache-2.0","files":7,"size_bytes":1519182365},
			"settings":{"quantization":"Q4K"},"resident":true,"residency":"resident",
			"added_at":"2026-08-04T22:37:31.875166Z"
		}]`))
	})

	models, err := n.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("got %d models", len(models))
	}
	m := models[0]
	// The handle is what an ai_models row stores as its key, so losing it here
	// would make every local model a uuid on screen.
	if m.Handle != "Qwen3-0.6B" {
		t.Errorf("handle = %q", m.Handle)
	}
	if m.UID != "48e61d05" || m.Kind != "chat" || m.Residency != "resident" {
		t.Errorf("model read wrong: %+v", m)
	}
	if m.Facts.ContextLength != 40960 || m.Facts.SizeBytes != 1519182365 {
		t.Errorf("facts read wrong: %+v", m.Facts)
	}
	if m.Settings["quantization"] != "Q4K" {
		t.Errorf("settings read wrong: %+v", m.Settings)
	}
}

func TestSettingsAreSentUnderTheKeyTheNodeExpects(t *testing.T) {
	var body map[string]map[string]string
	n := node(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{}`))
	})

	if _, err := n.SaveSettings(context.Background(), "u1", map[string]string{"quantization": "Q4K"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if body["values"]["quantization"] != "Q4K" {
		t.Errorf("body was %+v", body)
	}
}

func TestClearingEverySettingSendsAnEmptyMapAndNotNothing(t *testing.T) {
	// nil would encode as `null`, which the node reads as no values field at
	// all. Somebody clearing the last setting would find it still set.
	var raw map[string]any
	n := node(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = w.Write([]byte(`{}`))
	})

	if _, err := n.SaveSettings(context.Background(), "u1", nil); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	values, ok := raw["values"].(map[string]any)
	if !ok {
		t.Fatalf("values was %#v, want an object", raw["values"])
	}
	if len(values) != 0 {
		t.Errorf("values was %+v", values)
	}
}

func TestARefusalKeepsTheNodesOwnWords(t *testing.T) {
	n := node(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"this node already has Qwen3-0.6B at this version"}}`))
	})

	_, err := n.StartPull(context.Background(), "Qwen/Qwen3-0.6B", "", "")
	if err == nil {
		t.Fatal("a refusal was read as a success")
	}
	nodeErr, ok := AsError(err)
	if !ok {
		t.Fatalf("the refusal was not the node's: %v", err)
	}
	if nodeErr.Status != http.StatusConflict || nodeErr.Code != "conflict" {
		t.Errorf("refusal read wrong: %+v", nodeErr)
	}
	if !strings.Contains(nodeErr.Message, "already has Qwen3-0.6B") {
		t.Errorf("the node's words were lost: %q", nodeErr.Message)
	}
}

func TestANotFoundIsRecognisable(t *testing.T) {
	n := node(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"model not found"}}`))
	})

	_, err := n.Model(context.Background(), "gone")
	nodeErr, ok := AsError(err)
	if !ok || !nodeErr.NotFound() {
		t.Fatalf("a missing model was not recognisable: %v", err)
	}
}

func TestSomethingInFrontOfTheNodeIsNotReportedAsTheNodeSpeaking(t *testing.T) {
	// A proxy or load balancer answers with HTML. Passing that through would
	// put a page of markup in front of an administrator as though the node had
	// said it.
	n := node(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	})

	_, err := n.Info(context.Background())
	nodeErr, ok := AsError(err)
	if !ok {
		t.Fatalf("expected a node error, got %v", err)
	}
	if strings.Contains(nodeErr.Message, "<html>") {
		t.Errorf("markup reached the message: %q", nodeErr.Message)
	}
	if nodeErr.Code != "unexpected" || nodeErr.Status != http.StatusBadGateway {
		t.Errorf("refusal read wrong: %+v", nodeErr)
	}
}

func TestANodeThatDoesNotAnswerIsNotANodeRefusal(t *testing.T) {
	// A machine that is off must not be reported as a machine with an opinion.
	n, err := New("https://127.0.0.1:1/v1", "k", dialWith(&tls.Config{MinVersion: tls.VersionTLS13}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := n.Info(context.Background()); err == nil {
		t.Fatal("an unreachable node answered")
	} else if _, ok := AsError(err); ok {
		t.Errorf("a transport failure was reported as the node refusing: %v", err)
	}
}

func TestAnEndpointThatIsNotANodeIsNotMistakenForOne(t *testing.T) {
	// The probe: an ordinary self-hosted endpoint sharing the vendor key space
	// answers something, and it must not read as one of ours.
	n := node(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Not Found"}`))
	})
	if _, err := n.Info(context.Background()); err == nil {
		t.Fatal("a foreign endpoint passed as a node")
	}
}

func TestAPullKnowsWhenItIsFinished(t *testing.T) {
	for state, want := range map[string]bool{
		PullDownloading: false,
		PullVerifying:   false,
		PullReady:       true,
		PullFailed:      true,
		PullCancelled:   true,
	} {
		p := Pull{State: state}
		if p.Finished() != want {
			t.Errorf("%s finished = %v, want %v", state, p.Finished(), want)
		}
	}
}

func TestAContextThatIsCancelledStopsTheCall(t *testing.T) {
	n := node(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := n.Info(ctx); err == nil {
		t.Fatal("a cancelled call still ran")
	}
}

// A machine that has gone away does not refuse a connection, it says nothing,
// and the caller waits out whatever ceiling it was given.
//
// That is not a hypothetical: a decommissioned box left a row behind, and the
// Machines screen spun for thirty seconds because ONE row was a machine nobody
// had told us about. The ceiling for the question a screen waits on has to be
// short enough that "not answering" arrives as an answer.
func TestAMachineThatNeverAnswersGivesUpQuicklyEnoughToShow(t *testing.T) {
	// Accepts the connection and then says nothing at all, which is what a dead
	// host behind a firewall does and what no amount of retrying will improve.
	silent := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer silent.Close()

	n, err := New(silent.URL+"/v1", "test-key", dialWith(&tls.Config{InsecureSkipVerify: true})) //nolint:gosec // the test's own certificate
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()
	if _, err := n.Info(context.Background()); err == nil {
		t.Fatal("a machine that never answered was reported as reachable")
	}
	waited := time.Since(start)

	if waited > probeTimeout+3*time.Second {
		t.Errorf("waited %v on a machine that never answers; a screen cannot sit through that", waited)
	}
	// And the ceiling is the SHORT one: reading this as a pass while Info used
	// the ordinary thirty-second timeout is exactly the regression to catch.
	if probeTimeout > 10*time.Second {
		t.Errorf("probeTimeout is %v, which is too long for a screen to wait on", probeTimeout)
	}
}

// The caller's own deadline still wins. A screen that has given up must not be
// held open by a ceiling of ours that is longer than its own.
func TestTheCallersDeadlineIsStillRespected(t *testing.T) {
	silent := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer silent.Close()

	n, err := New(silent.URL+"/v1", "test-key", dialWith(&tls.Config{InsecureSkipVerify: true})) //nolint:gosec // the test's own certificate
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := n.Info(ctx); err == nil {
		t.Fatal("expected the call to fail")
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("waited %v, ignoring a caller who gave up after 300ms", waited)
	}
}
