package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/nodeca"
)

// A machine adding itself.
//
// The join is the ONE call that goes from a machine to us, so it is the one
// place where an unauthenticated request reaches a write. What these assert is
// that holding the token is the whole gate, that the token itself never crosses
// the network, that nothing in the request can be altered on the way, and that a
// machine coming back updates its own row rather than leaving a dead one behind
// and a duplicate beside it.

const aNodeKey = "0123456789abcdef0123456789abcdef"

// aCertificateRequest is a machine asking for an identity: a request over a key
// it keeps. A fresh key each time, because two machines sharing one would be the
// bug this whole mechanism exists to prevent.
func aCertificateRequest(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: "a machine"}}, key)
	if err != nil {
		t.Fatalf("certificate request: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func joinBody(t *testing.T, nodeID, name, key string) map[string]any {
	return map[string]any{
		"node_id": nodeID, "name": name, "key": key,
		"port":                19443,
		"version":             "0.1.0",
		"certificate_request": aCertificateRequest(t),
		"issued_at":           time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// joinToken mints one, in somebody's name, the way the console does.
//
// A token belongs to a person now, so a test cannot ask for "the deployment's
// token" any more than an administrator can.
func (e *testEnv) joinToken() string {
	e.t.Helper()
	return e.joinTokenFor(e.createUser(
		fmt.Sprintf("machines-%d@example.test", time.Now().UnixNano()), "x"))
}

func (e *testEnv) joinTokenFor(user *model.User) string {
	e.t.Helper()
	token, err := e.app.MintJoinToken(context.Background(), user.ID)
	if err != nil {
		e.t.Fatalf("mint a join token: %v", err)
	}
	return token
}

// join sends a join request signed the way a machine signs one.
func (e *testEnv) join(token string, body map[string]any) *httptest.ResponseRecorder {
	e.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		e.t.Fatalf("marshal: %v", err)
	}
	return e.joinRaw(token, raw)
}

// joinRaw is join without re-encoding, for the tests that need the signed bytes
// and the sent bytes to differ.
func (e *testEnv) joinRaw(token string, raw []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.joinSigned(sign(token, raw), raw)
}

func (e *testEnv) joinSigned(signature string, raw []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/nodes/join", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, signature)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func sign(token string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestAMachineJoinsAndBecomesAPlatformRow(t *testing.T) {
	env := newTestEnv(t)
	token := env.joinToken()

	rec := env.join(token, joinBody(t, "nd-1", "gpu-1", aNodeKey))
	env.expectStatus(rec, http.StatusOK)

	var got struct {
		Name        string `json:"name"`
		BaseURL     string `json:"base_url"`
		Joined      bool   `json:"joined"`
		Authority   string `json:"authority"`
		Certificate string `json:"certificate"`
	}
	env.decode(rec, &got)
	if !got.Joined || got.Name != "gpu-1" {
		t.Fatalf("join answered %+v", got)
	}
	// The stored address is the INFERENCE url, because that is what a vendor row
	// is and what the gateway uses unchanged. https, because a machine now holds
	// a certificate and serves nothing without one.
	if got.BaseURL != "https://192.0.2.1:19443/v1" {
		t.Errorf("base url = %q", got.BaseURL)
	}

	// A machine is a PLATFORM row. Joining creates no vendor row and no model
	// row: what a workspace may use is a separate decision made afterwards.
	nodes, err := env.app.Store.Nodes().List(context.Background())
	if err != nil {
		t.Fatalf("list machines: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d machines", len(nodes))
	}
	n := nodes[0]
	if n.NodeID != "nd-1" || n.Name != "gpu-1" || n.BaseURL != "https://192.0.2.1:19443/v1" {
		t.Errorf("machine row wrong: %+v", n)
	}
	key, err := env.app.Keyring.Open(n.Key)
	if err != nil {
		t.Fatalf("open the machine key: %v", err)
	}
	if string(key) != aNodeKey {
		t.Errorf("stored key = %q", key)
	}

	vendors, _ := env.app.Store.Vendors().List(context.Background(), env.ws.ID)
	if len(vendors) != 0 {
		t.Errorf("joining put %d vendor rows into a workspace", len(vendors))
	}
}

func TestTheMachineIsGivenAnIdentityItCanServeWith(t *testing.T) {
	// The point of the whole exchange: the machine leaves it able to prove who it
	// is, and we leave able to check. Everything after this call depends on both.
	env := newTestEnv(t)
	rec := env.join(env.joinToken(), joinBody(t, "nd-1", "gpu-1", aNodeKey))
	env.expectStatus(rec, http.StatusOK)

	var got struct {
		Authority   string `json:"authority"`
		Certificate string `json:"certificate"`
	}
	env.decode(rec, &got)

	authority, err := nodeca.ParseCertificate([]byte(got.Authority))
	if err != nil {
		t.Fatalf("the authority we sent is not a certificate: %v", err)
	}
	cert, err := nodeca.ParseCertificate([]byte(got.Certificate))
	if err != nil {
		t.Fatalf("the certificate we sent is not a certificate: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(authority)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   nodeca.ServerName,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("the machine could not serve with what we gave it: %v", err)
	}
	// Named for the machine, so the control surface can check it reached the one
	// it meant to.
	if cert.Subject.CommonName != "nd-1" {
		t.Errorf("certificate common name = %q", cert.Subject.CommonName)
	}
	// And the token names this authority, which is what the machine checks
	// before it believes any of this.
	if want := joinTokenFingerprint(env.joinToken()); want != nodeca.Fingerprint(authority.Raw) {
		t.Errorf("the token names a different authority than the one we sent")
	}

	// The row remembers when it runs out, so the console can say so.
	n, _ := env.app.Store.Nodes().ByNodeID(context.Background(), "nd-1")
	if n.CertExpiresAt == nil {
		t.Fatal("the row does not know when the certificate expires")
	}
	if got, want := *n.CertExpiresAt, cert.NotAfter; got.Sub(want).Abs() > time.Minute {
		t.Errorf("expiry recorded as %v, certificate says %v", got, want)
	}
}

// joinTokenFingerprint is the authority fingerprint a machine reads out of its
// token. Spelled out here rather than shared, because the machine is written in
// another language and this is the format both sides must agree on.
func joinTokenFingerprint(token string) string {
	fingerprint, _, _ := strings.Cut(token, "_")
	return fingerprint
}

func TestTheJoinTokenNeverCrossesTheNetwork(t *testing.T) {
	// The property that makes a join safe over an open network: an eavesdropper
	// who records the whole exchange learns nothing they can reuse. The machine
	// proves it holds the token by signing with it, and a signature is good for
	// exactly the bytes it was made over.
	env := newTestEnv(t)
	token := env.joinToken()
	body := joinBody(t, "nd-1", "gpu-1", aNodeKey)
	raw, _ := json.Marshal(body)

	rec := env.joinRaw(token, raw)
	env.expectStatus(rec, http.StatusOK)

	if strings.Contains(string(raw), token) {
		t.Error("the join request carried the token itself")
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Error("the answer carried the token itself")
	}
}

func TestAnAlteredJoinIsRefused(t *testing.T) {
	// A signature over the whole body, not merely proof of the sender: an
	// intermediary must not be able to redirect a machine's address, swap its key
	// or substitute the certificate request while leaving a valid signature.
	env := newTestEnv(t)
	token := env.joinToken()
	honest, _ := json.Marshal(joinBody(t, "nd-1", "gpu-1", aNodeKey))
	signature := sign(token, honest)

	tampered := map[string]string{
		"a different port":    `"port":19443`,
		"a different key":     `"key":"` + aNodeKey + `"`,
		"a different machine": `"node_id":"nd-1"`,
	}
	replacements := map[string]string{
		"a different port":    `"port":9999`,
		"a different key":     `"key":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`,
		"a different machine": `"node_id":"nd-2"`,
	}

	for what, find := range tampered {
		altered := bytes.Replace(honest, []byte(find), []byte(replacements[what]), 1)
		if bytes.Equal(altered, honest) {
			t.Fatalf("%s: the test did not actually change anything", what)
		}
		// The ORIGINAL signature, which is all an intermediary has.
		rec := env.joinSigned(signature, altered)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s was accepted (%d)", what, rec.Code)
		}
	}

	nodes, _ := env.app.Store.Nodes().List(context.Background())
	if len(nodes) != 0 {
		t.Fatalf("a tampered join still made %d rows", len(nodes))
	}
}

func TestAStaleJoinCannotBeReplayed(t *testing.T) {
	// A captured join is a valid signature forever, so the request says when it
	// was made. Without this, a recording from last week could be replayed to
	// point a machine's row at somebody else's address.
	env := newTestEnv(t)
	token := env.joinToken()

	for what, when := range map[string]time.Time{
		"an hour ago":    time.Now().Add(-time.Hour),
		"an hour ahead":  time.Now().Add(time.Hour),
		"never said när": {},
	} {
		body := joinBody(t, "nd-1", "gpu-1", aNodeKey)
		body["issued_at"] = when.UTC().Format(time.RFC3339Nano)
		if rec := env.join(token, body); rec.Code != http.StatusUnauthorized {
			t.Errorf("a join stamped %s was accepted (%d)", what, rec.Code)
		}
	}
}

func TestAMachineComingBackUpdatesItsOwnRow(t *testing.T) {
	// The whole reason a node id exists: a container rescheduled onto another
	// address must not leave a dead row behind and a duplicate beside it.
	env := newTestEnv(t)
	env.expectStatus(env.join(env.joinToken(), joinBody(t, "nd-1", "gpu-1", aNodeKey)), http.StatusOK)

	// Signed with the key it registered with, and carrying a NEW one: this is a
	// machine saying "still me, here is where I am now, and here is the secret to
	// call me on from here". The invitation it arrived on is long gone.
	moved := joinBody(t, "nd-1", "gpu-1-renamed", "fedcba9876543210fedcba9876543210")
	moved["port"] = 9100
	rec := env.join(aNodeKey, moved)
	env.expectStatus(rec, http.StatusOK)

	var got struct {
		Joined bool `json:"joined"`
	}
	env.decode(rec, &got)
	if got.Joined {
		t.Error("a machine coming back was reported as a new one")
	}

	nodes, _ := env.app.Store.Nodes().List(context.Background())
	if len(nodes) != 1 {
		t.Fatalf("a returning machine made %d rows", len(nodes))
	}
	n := nodes[0]
	if n.BaseURL != "https://192.0.2.1:9100/v1" || n.Name != "gpu-1-renamed" {
		t.Errorf("the row was not updated: %+v", n)
	}
	key, _ := env.app.Keyring.Open(n.Key)
	if string(key) != "fedcba9876543210fedcba9876543210" {
		t.Error("the machine's new key was not stored")
	}

	// And the change took: the new key checks in and the old one no longer does.
	env.expectStatus(env.join("fedcba9876543210fedcba9876543210",
		joinBody(t, "nd-1", "gpu-1-renamed", "fedcba9876543210fedcba9876543210")), http.StatusOK)
	env.expectStatus(env.join(aNodeKey,
		joinBody(t, "nd-1", "gpu-1-renamed", aNodeKey)), http.StatusUnauthorized)
}

func TestTwoMachinesAreTwoRows(t *testing.T) {
	env := newTestEnv(t)

	// An invitation each, because an invitation admits one machine. Racking two
	// boxes means opening the dialog twice, which is the cost of a token that
	// cannot be reused by whoever finds it afterwards.
	for _, id := range []string{"nd-1", "nd-2"} {
		env.expectStatus(env.join(env.joinToken(), joinBody(t, id, "gpu-"+id, aNodeKey)), http.StatusOK)
	}
	nodes, _ := env.app.Store.Nodes().List(context.Background())
	if len(nodes) != 2 {
		t.Fatalf("two machines made %d rows", len(nodes))
	}
}

func TestAJoinWithoutTheRightTokenIsRefusedAndSaysNothing(t *testing.T) {
	env := newTestEnv(t)
	real := env.joinToken()

	attempts := map[string]string{
		"no token":         "",
		"nonsense":         "hello",
		"right shape":      "abc123_wrongwrongwrongwrongwrong",
		"one char short":   real[:len(real)-1],
		"one char extra":   real + "x",
		"secret only":      strings.SplitN(real, "_", 2)[1],
		"fingerprint only": joinTokenFingerprint(real),
	}
	var bodies []string
	for what, token := range attempts {
		rec := env.join(token, joinBody(t, "nd-x", "sneaky", aNodeKey))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s answered %d", what, rec.Code)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for _, body := range bodies[1:] {
		if body != bodies[0] {
			t.Errorf("a refusal told the caller which kind of wrong it was:\n%s\n%s", bodies[0], body)
		}
	}

	// And no signature at all is the same answer.
	raw, _ := json.Marshal(joinBody(t, "nd-x", "sneaky", aNodeKey))
	if rec := env.joinSigned("", raw); rec.Code != http.StatusUnauthorized {
		t.Errorf("an unsigned join answered %d", rec.Code)
	}

	nodes, _ := env.app.Store.Nodes().List(context.Background())
	if len(nodes) != 0 {
		t.Fatalf("a refused join still made %d rows", len(nodes))
	}
}

func TestAMachineOfferingAWeakKeyIsRefused(t *testing.T) {
	// The same floor the machine enforces on itself. A short key here would be a
	// machine anyone on the network could drive.
	env := newTestEnv(t)
	rec := env.join(env.joinToken(), joinBody(t, "nd-1", "gpu-1", "short"))
	env.expectStatus(rec, http.StatusUnauthorized)
}

func TestAMachineWithNoCertificateRequestIsRefused(t *testing.T) {
	// It would join and then be unreachable, because there is no unencrypted way
	// to reach a machine any more. Refusing now says why; refusing later would
	// look like a network fault.
	env := newTestEnv(t)
	body := joinBody(t, "nd-1", "gpu-1", aNodeKey)
	body["certificate_request"] = ""
	env.expectStatus(env.join(env.joinToken(), body), http.StatusUnauthorized)

	body["certificate_request"] = "not a certificate request"
	env.expectStatus(env.join(env.joinToken(), body), http.StatusUnauthorized)
}

func TestAMachineCannotAskForACertificateOverAnotherMachinesKey(t *testing.T) {
	// A certificate request is signed by the key it asks us to certify. A machine
	// that could get one issued over a public key it had merely seen could then
	// impersonate its owner to us.
	env := newTestEnv(t)
	body := joinBody(t, "nd-1", "gpu-1", aNodeKey)

	honest := body["certificate_request"].(string)
	block, _ := pem.Decode([]byte(honest))
	block.Bytes[len(block.Bytes)-1] ^= 0xff
	body["certificate_request"] = string(pem.EncodeToMemory(block))

	env.expectStatus(env.join(env.joinToken(), body), http.StatusUnauthorized)
}

func TestAMachineThatSaysNoAddressIsReachedWhereItCalledFrom(t *testing.T) {
	// What is pasted onto a machine is --url and --token, and nothing else, so
	// this is the ordinary path and not a fallback. Each side supplies the half
	// it has: we saw the host it connected from, it knows the port it listens
	// on.
	//
	// This test asserted a REFUSAL, and was right to: taking the source host on
	// its own produced an address with no port, which the next check rejected.
	// So an address was mandatory on every machine while the flag offering it was
	// documented as being for machines behind NAT.
	env := newTestEnv(t)
	body := joinBody(t, "nd-1", "gpu-1", aNodeKey)
	delete(body, "advertise")

	rec := env.join(env.joinToken(), body)
	env.expectStatus(rec, http.StatusOK)

	n, err := env.app.Store.Nodes().ByNodeID(context.Background(), "nd-1")
	if err != nil {
		t.Fatalf("read the machine: %v", err)
	}
	if n.BaseURL != "https://192.0.2.1:19443/v1" {
		t.Errorf("base url = %q, want the address we saw on the port it declared", n.BaseURL)
	}

	// A machine that declares no port is still refused, because then there
	// genuinely is no address: it is the one half we cannot supply ourselves.
	silent := joinBody(t, "nd-2", "gpu-2", aNodeKey)
	delete(silent, "port")
	rec = env.join(env.joinToken(), silent)
	env.expectStatus(rec, http.StatusUnauthorized)
	if !strings.Contains(rec.Body.String(), "port") {
		t.Errorf("the refusal did not say what was missing: %s", rec.Body.String())
	}
}

func TestAMachineCannotBeRegisteredAtAnUnencryptedAddress(t *testing.T) {
	// Everything after the join is a mutually authenticated channel. A row
	// pointing at a plain address is a row that cannot be used, and storing one
	// would turn a configuration mistake into a connection failure days later.
	env := newTestEnv(t)
	body := joinBody(t, "nd-1", "gpu-1", aNodeKey)
	body["advertise"] = "http://10.0.0.9:8081"

	rec := env.join(env.joinToken(), body)
	env.expectStatus(rec, http.StatusUnauthorized)
}

func TestRotatingTheTokenRefusesTheOldOneAndKeepsJoinedMachines(t *testing.T) {
	// What rotating costs, exactly: anything not yet joined needs the new token,
	// and everything already joined is untouched because it holds its own key and
	// its own certificate.
	env := newTestEnv(t)
	old := env.joinToken()
	env.expectStatus(env.join(old, joinBody(t, "nd-1", "gpu-1", aNodeKey)), http.StatusOK)

	env.createUser("rot@test", "password1234", model.PermMachinesCreate)
	token, _ := env.login("rot@test", "password1234")
	rec := env.do(http.MethodPost, "/v1/nodes/join-token", token, nil)
	env.expectStatus(rec, http.StatusOK)

	var rotated struct {
		Token string `json:"token"`
	}
	env.decode(rec, &rotated)
	if rotated.Token == old || rotated.Token == "" {
		t.Fatal("rotating gave back the same token")
	}
	// The authority does NOT change with the token: machines already holding a
	// certificate from it must go on working, which is the whole claim above.
	if joinTokenFingerprint(rotated.Token) != joinTokenFingerprint(old) {
		t.Error("rotating the token replaced the authority, orphaning every machine")
	}

	// The old one no longer works.
	env.expectStatus(env.join(old, joinBody(t, "nd-2", "gpu-2", aNodeKey)), http.StatusUnauthorized)
	// The new one does.
	env.expectStatus(env.join(rotated.Token, joinBody(t, "nd-2", "gpu-2", aNodeKey)), http.StatusOK)

	// And the machine that joined before the rotation still has its row.
	n, err := env.app.Store.Nodes().ByNodeID(context.Background(), "nd-1")
	if err != nil {
		t.Fatalf("a joined machine lost its row on rotation: %v", err)
	}
	if n.Name != "gpu-1" {
		t.Errorf("row changed: %+v", n)
	}
}

func TestMintingAJoinTokenNeedsThePermissionToAddAMachine(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("nobody@test", "password1234")
	token, _ := env.login("nobody@test", "password1234")

	rec := env.do(http.MethodPost, "/v1/nodes/join-token", token, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("minting a join token answered %d", rec.Code)
	}
}

func TestThereIsNoWayToReadAJoinTokenBack(t *testing.T) {
	// A token used to be read back on demand, indefinitely, so that it could be
	// pasted onto machine after machine. It is now one person's, lasts an hour
	// and is destroyed by the machine that uses it, so the only useful answer is
	// a fresh one, and the route that handed a live credential to a GET is gone.
	// A GET is what ends up in a proxy log and a browser history.
	env := newTestEnv(t)
	user := env.createUser("machines@test", "password1234", model.PermMachinesCreate)
	token, _ := env.login("machines@test", "password1234")

	rec := env.do(http.MethodGet, "/v1/nodes/join-token", token, nil)
	if rec.Code == http.StatusOK {
		t.Fatalf("a GET handed back a join token: %s", rec.Body.String())
	}

	// And minting twice gives two different tokens, because there is nothing
	// being remembered to give back.
	first := env.joinTokenFor(user)
	if second := env.joinTokenFor(user); second == first {
		t.Error("asking twice returned the same token, so one is being kept")
	}
}

func TestTheTokenNamesTheAuthorityAMachineWillBeGiven(t *testing.T) {
	// The bootstrap: a machine trusts nothing when it first calls, so the one
	// thing carried to it by hand has to say who it is about to talk to.
	env := newTestEnv(t)
	authority, err := env.app.Authority(context.Background())
	if err != nil {
		t.Fatalf("authority: %v", err)
	}
	if got := joinTokenFingerprint(env.joinToken()); got != authority.Fingerprint() {
		t.Errorf("token names %q, the authority is %q", got, authority.Fingerprint())
	}
}

// --- an invitation, and what it costs -----------------------------------------
//
// A join token belongs to a person, stands for an hour, and is destroyed by the
// machine that uses it. None of that is worth anything unless a spent one is
// actually refused, so these run the attacks rather than reading the code.

func TestATokenAdmitsOneMachineAndThenNothing(t *testing.T) {
	env := newTestEnv(t)
	token := env.joinToken()

	env.expectStatus(env.join(token, joinBody(t, "nd-1", "gpu-1", aNodeKey)), http.StatusOK)

	// The same token, a DIFFERENT machine. This is the whole point: somebody who
	// watched an install, or found the command in a shell history, does not get
	// to add a machine of their own to the fleet.
	rec := env.join(token, joinBody(t, "nd-2", "gpu-2", aNodeKey))
	env.expectStatus(rec, http.StatusUnauthorized)

	nodes, err := env.app.Store.Nodes().List(context.Background())
	if err != nil {
		t.Fatalf("list machines: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("a spent token admitted %d machines", len(nodes))
	}
}

func TestAMachineChecksBackInOnItsOwnKeyAndCostsNoToken(t *testing.T) {
	// The property single use rests on. A machine re-enrols every day for a fresh
	// certificate; if that took an invitation, an invitation could never be spent
	// and never expire.
	env := newTestEnv(t)
	env.expectStatus(env.join(env.joinToken(), joinBody(t, "nd-1", "gpu-1", aNodeKey)), http.StatusOK)

	// No token anywhere in this: signed with the key the machine sent when it
	// registered, which is the key we call it with.
	body := joinBody(t, "nd-1", "gpu-1-renamed", aNodeKey)
	rec := env.join(aNodeKey, body)
	env.expectStatus(rec, http.StatusOK)

	var got struct {
		Name   string `json:"name"`
		Joined bool   `json:"joined"`
	}
	env.decode(rec, &got)
	if got.Joined {
		t.Error("a re-enrolment was reported as a new machine")
	}
	if got.Name != "gpu-1-renamed" {
		t.Errorf("the row was not refreshed: name = %q", got.Name)
	}

	// And it can do it again, forever, which is what a daily check-in is.
	env.expectStatus(env.join(aNodeKey, joinBody(t, "nd-1", "gpu-1", aNodeKey)), http.StatusOK)

	// "Costs no token" said as a number. Somebody else's unused invitation is
	// still sitting there after two check-ins; if re-enrolment took one, a fleet
	// of machines would quietly eat every invitation anybody minted.
	somebodyElse := env.joinTokenFor(env.createUser("bystander@example.test", "x"))
	env.expectStatus(env.join(aNodeKey, joinBody(t, "nd-1", "gpu-1", aNodeKey)), http.StatusOK)
	live, err := env.app.Store.NodeJoin().Live(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("read the live tokens: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("a check-in changed the number of live invitations to %d", len(live))
	}
	env.expectStatus(env.join(somebodyElse, joinBody(t, "nd-9", "theirs", aNodeKey)), http.StatusOK)
}

func TestAMachineCannotCheckInAsOneItIsNot(t *testing.T) {
	// The other half: signing with a key that is not the one on that machine's
	// row is refused, or "check in as yourself" would be "name any machine".
	env := newTestEnv(t)
	env.expectStatus(env.join(env.joinToken(), joinBody(t, "nd-1", "gpu-1", aNodeKey)), http.StatusOK)

	rec := env.join("ffffffffffffffffffffffffffffffff", joinBody(t, "nd-1", "stolen", aNodeKey))
	env.expectStatus(rec, http.StatusUnauthorized)

	n, err := env.app.Store.Nodes().ByNodeID(context.Background(), "nd-1")
	if err != nil {
		t.Fatalf("read the machine: %v", err)
	}
	if n.Name != "gpu-1" {
		t.Errorf("a refused check-in still rewrote the row: name = %q", n.Name)
	}
}

func TestAnExpiredTokenAdmitsNobody(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("expiry@example.test", "x")
	token := env.joinTokenFor(user)

	// Age it the way an hour would, in the one place that decides: the row.
	ctx := context.Background()
	live, err := env.app.Store.NodeJoin().Live(ctx, time.Now())
	if err != nil || len(live) == 0 {
		t.Fatalf("no live token to expire: %v", err)
	}
	if err := env.app.Store.NodeJoin().Mint(ctx, user.ID, live[0].Sealed,
		time.Now().Add(-time.Minute), time.Now()); err != nil {
		t.Fatalf("expire the token: %v", err)
	}

	env.expectStatus(env.join(token, joinBody(t, "nd-1", "gpu-1", aNodeKey)), http.StatusUnauthorized)
}

func TestOnePersonsTokenIsNotAnothersAndMintingReplacesRatherThanAdds(t *testing.T) {
	// Why a token has an owner at all. Two administrators racking hardware at the
	// same time each hold their own, so neither takes the other's away; and
	// asking twice updates one row, so nobody leaves working credentials behind
	// them.
	env := newTestEnv(t)
	ctx := context.Background()
	alice := env.createUser("alice-machines@example.test", "x")
	bob := env.createUser("bob-machines@example.test", "x")

	aliceToken := env.joinTokenFor(alice)
	bobToken := env.joinTokenFor(bob)
	if aliceToken == bobToken {
		t.Fatal("two people were given one token")
	}

	// Alice asks again. Her old one dies; Bob's is untouched.
	aliceAgain := env.joinTokenFor(alice)
	if aliceAgain == aliceToken {
		t.Fatal("asking again returned the same token")
	}

	live, err := env.app.Store.NodeJoin().Live(ctx, time.Now())
	if err != nil {
		t.Fatalf("read the live tokens: %v", err)
	}
	if len(live) != 2 {
		t.Fatalf("got %d live tokens, want one each for two people", len(live))
	}

	env.expectStatus(env.join(aliceToken, joinBody(t, "nd-old", "old", aNodeKey)), http.StatusUnauthorized)
	env.expectStatus(env.join(bobToken, joinBody(t, "nd-bob", "bob", aNodeKey)), http.StatusOK)
	env.expectStatus(env.join(aliceAgain, joinBody(t, "nd-alice", "alice", aNodeKey)), http.StatusOK)
}

func TestTheAddressIsTheMachinesAndNotTheProxysTest(t *testing.T) {
	// A machine registered itself at 10.0.3.49, which is a container network and
	// not anywhere it can be reached from. Nothing lied: on every real deployment
	// there is a proxy in front, so `RemoteAddr` is the proxy talking to us on a
	// private network, and reading it honestly gave the address of the wrong end.
	cases := map[string]struct {
		forwarded, real, remote, want string
	}{
		"through a proxy":  {forwarded: "203.0.113.7", remote: "10.0.3.49:41234", want: "203.0.113.7"},
		"a chain of them":  {forwarded: "203.0.113.7, 10.0.3.1", remote: "10.0.3.49:41234", want: "203.0.113.7"},
		"the other header": {real: "203.0.113.7", remote: "10.0.3.49:41234", want: "203.0.113.7"},
		// No proxy at all, which is a machine on the same network as the gateway.
		"straight to us": {remote: "203.0.113.7:41234", want: "203.0.113.7"},
		// A header carrying something that is not an address cannot put anything
		// into a row: it is ignored and the connection is used.
		"a header that is not an address": {
			forwarded: "evil.example.com", remote: "203.0.113.7:41234", want: "203.0.113.7"},
		"an empty header": {forwarded: "  ", remote: "203.0.113.7:41234", want: "203.0.113.7"},
	}

	for name, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/nodes/join", nil)
		req.RemoteAddr = c.remote
		if c.forwarded != "" {
			req.Header.Set("X-Forwarded-For", c.forwarded)
		}
		if c.real != "" {
			req.Header.Set("X-Real-IP", c.real)
		}
		if got := sourceAddress(req); got != c.want {
			t.Errorf("%s: sourceAddress = %q, want %q", name, got, c.want)
		}
	}
}
