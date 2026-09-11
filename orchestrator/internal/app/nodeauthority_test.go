package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"flexie.io/sag/internal/nodeca"
)

// What these tests are about is the KEEPING of a connection to a machine, which
// is not a performance concern however much it looks like one.
//
// A transport is a connection pool, and Go does not close a pool that is merely
// dropped: its idle connections stay open until the transport says otherwise,
// and a transport nothing refers to any more never says anything. So a client
// built per call does not fail to pool, it leaks one live socket per call. The
// control surface did exactly that, and after 1141 polls the machine on the
// other end stopped completing handshakes: local inference was gone, the screen
// said the machine did not answer, and the process was still running so nothing
// restarted it (KB/29).
//
// The test therefore counts CONNECTIONS rather than asserting that some cache
// returns the same pointer. A cache is one way to have this property; the
// property is the thing that has to hold.

const testMachine = "nd-test"

// newTrust is the authority and our own identity, as machineTrust holds them.
func newTrust(t *testing.T) *machineTrust {
	t.Helper()
	authority, err := nodeca.Create(time.Now())
	if err != nil {
		t.Fatalf("authority: %v", err)
	}
	ours, err := authority.IssueClient(orchestratorName, time.Now())
	if err != nil {
		t.Fatalf("our identity: %v", err)
	}
	return &machineTrust{
		authority: authority,
		ours:      ours,
		control:   map[string]*http.Client{},
	}
}

// machineCert is what one machine serves with: a certificate from the authority,
// named for the machine rather than for its address.
func machineCert(t *testing.T, authority *nodeca.Authority, nodeID string) tls.Certificate {
	t.Helper()
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
	cert, err := tls.X509KeyPair(certPEM,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatalf("machine identity: %v", err)
	}
	return cert
}

// machineServing stands up a real machine: it holds a certificate, it demands
// one back, and it reports how many connections were opened to it.
func machineServing(t *testing.T, trust *machineTrust, nodeID string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var opened atomic.Int64

	server := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		}))
	server.TLS = trust.authority.ServerTLS(machineCert(t, trust.authority, nodeID))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, &opened
}

// ask makes one call the way internal/inference does, which includes draining
// the body: a body left unread is a connection that cannot go back to the pool,
// so a client that is reused correctly and read carelessly leaks just the same.
func ask(t *testing.T, client *http.Client, url string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("calling the machine: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the machine answered %d", resp.StatusCode)
	}
}

func TestCallingOneMachineManyTimesOpensOneConnection(t *testing.T) {
	trust := newTrust(t)
	server, opened := machineServing(t, trust, testMachine)

	// Fetched inside the loop on purpose: App.Node builds a client for every
	// request it serves, so a fix that only works when the caller holds on to
	// one would not be a fix at all.
	const calls = 20
	for range calls {
		ask(t, trust.controlClient(testMachine), server.URL)
	}

	if got := opened.Load(); got != 1 {
		t.Errorf("%d calls opened %d connections, want 1", calls, got)
	}
}

func TestEachMachineIsCalledOnItsOwnConnection(t *testing.T) {
	// Because the certificate is pinned to the machine we mean to reach, one
	// pool cannot serve two machines: sharing here would mean either dropping
	// that check or sending machine A's connection to machine B.
	trust := newTrust(t)
	first, firstOpened := machineServing(t, trust, "nd-first")
	second, secondOpened := machineServing(t, trust, "nd-second")

	for range 5 {
		ask(t, trust.controlClient("nd-first"), first.URL)
		ask(t, trust.controlClient("nd-second"), second.URL)
	}

	if got := firstOpened.Load(); got != 1 {
		t.Errorf("the first machine was opened %d times, want 1", got)
	}
	if got := secondOpened.Load(); got != 1 {
		t.Errorf("the second machine was opened %d times, want 1", got)
	}
	if trust.controlClient("nd-first") == trust.controlClient("nd-second") {
		t.Error("two machines were given the same client, so one of them is not being checked")
	}
}

func TestRequestsArrivingTogetherShareOneClient(t *testing.T) {
	// Every request the server handles asks for this, so the answer is worked
	// out on a path that is concurrent by definition. Run under -race, this is
	// what says the map holding them is actually guarded; without the lock it
	// reports the write, and two racing callers could otherwise each build a
	// pool and one of them would be dropped, which is the original defect again
	// with a smaller number in front of it.
	trust := newTrust(t)

	const callers = 50
	got := make(chan *http.Client, callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			<-start
			got <- trust.controlClient(testMachine)
		}()
	}
	close(start)

	first := <-got
	for range callers - 1 {
		if c := <-got; c != first {
			t.Fatal("two callers at once were given different clients, so one machine has two pools")
		}
	}
}

func TestAMachinesConnectionsAreCapped(t *testing.T) {
	// The bound matters more than the number. A hand-built http.Transport
	// inherits none of http.DefaultTransport's limits: left alone it has no idle
	// timeout and no per-host cap, so a pool that is reused but never pruned is
	// only a slower version of the fault above.
	transport := machineTransport(&tls.Config{MinVersion: tls.VersionTLS13})
	if transport.IdleConnTimeout <= 0 {
		t.Error("idle connections to a machine are never pruned")
	}
	if transport.MaxIdleConnsPerHost <= 0 {
		t.Error("idle connections to one machine are not capped")
	}
}
