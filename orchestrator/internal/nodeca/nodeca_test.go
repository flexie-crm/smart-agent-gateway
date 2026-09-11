package nodeca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"
	"time"
)

// The authority exists so two machines on an open network can tell each other
// apart. What is asserted here is that property and the ways it could be lost,
// not that the standard library can sign a certificate.

func authority(t *testing.T) *Authority {
	t.Helper()
	ca, err := Create(time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return ca
}

// request makes a certificate request the way a machine would.
func request(t *testing.T, commonName string) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: commonName}}, key)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return der, key
}

func TestAnIssuedCertificateIsTrustedByTheAuthorityAndNothingElse(t *testing.T) {
	ca := authority(t)
	csr, _ := request(t, "ignored")
	certPEM, err := ca.Issue(csr, "nd_abc", []string{"203.0.113.7"}, time.Now())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	cert, err := ParseCertificate(certPEM)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// It chains to us.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("the authority certificate would not load")
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("an issued certificate did not verify against its own authority: %v", err)
	}

	// And to nobody else. A second authority must not accept the first's work,
	// which is the whole point of not using a public one.
	stranger := authority(t)
	strangerPool := x509.NewCertPool()
	strangerPool.AppendCertsFromPEM(stranger.CertPEM())
	if _, err := cert.Verify(x509.VerifyOptions{Roots: strangerPool}); err == nil {
		t.Error("a certificate verified against an authority that did not issue it")
	}
}

func TestTheCertificateIsNamedForTheMachineAndNotItsAddress(t *testing.T) {
	// A machine IS its node id here: that is what makes one rescheduled onto a
	// new address the same machine. Verification follows identity, so a leaf
	// named for an address would break the moment the address moved.
	ca := authority(t)
	csr, _ := request(t, "whatever the machine felt like putting here")
	certPEM, err := ca.Issue(csr, "nd_abc", []string{"203.0.113.7"}, time.Now())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	cert, _ := ParseCertificate(certPEM)

	if cert.Subject.CommonName != "nd_abc" {
		t.Errorf("common name = %q, want the node id", cert.Subject.CommonName)
	}
	// The address rides along for a stack that insists on matching what it
	// dialled, but it is not the identity.
	if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "203.0.113.7" {
		t.Errorf("addresses = %v", cert.IPAddresses)
	}
}

func TestAMachineCannotAskForACertificateOverSomebodyElsesKey(t *testing.T) {
	// A request is signed by the key it asks us to certify. Without checking
	// that, anyone could take a public key they had seen and get a certificate
	// issued over it, then impersonate its owner to every other machine.
	ca := authority(t)
	csr, _ := request(t, "nd_abc")

	// Corrupt the signature while leaving the request otherwise well formed.
	forged := make([]byte, len(csr))
	copy(forged, csr)
	forged[len(forged)-1] ^= 0xff

	if _, err := ca.Issue(forged, "nd_abc", nil, time.Now()); err == nil {
		t.Fatal("a certificate request with a broken signature was signed")
	}
}

func TestRubbishIsNotACertificateRequest(t *testing.T) {
	ca := authority(t)
	if _, err := ca.Issue([]byte("hello"), "nd_abc", nil, time.Now()); err == nil {
		t.Error("arbitrary bytes were accepted as a certificate request")
	}
}

func TestACertificateNeedsAMachineToBelongTo(t *testing.T) {
	// An empty common name is a certificate that identifies nothing, and every
	// verifier downstream would have nothing to compare against.
	ca := authority(t)
	csr, _ := request(t, "nd_abc")
	if _, err := ca.Issue(csr, "", nil, time.Now()); err == nil {
		t.Error("a certificate was issued with no machine")
	}
}

func TestTheAuthoritySurvivesBeingStoredAndReadBack(t *testing.T) {
	// It lives sealed in one row. A certificate issued before a restart must
	// still verify after one, or every machine would have to re-enrol.
	ca := authority(t)
	csr, _ := request(t, "nd_abc")
	before, err := ca.Issue(csr, "nd_abc", nil, time.Now())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	keyPEM, err := ca.KeyPEM()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	reloaded, err := Load(keyPEM, ca.CertPEM())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if reloaded.Fingerprint() != ca.Fingerprint() {
		t.Error("the authority changed identity across a restart")
	}

	cert, _ := ParseCertificate(before)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(reloaded.CertPEM())
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Errorf("a certificate issued before the restart stopped verifying: %v", err)
	}
}

func TestAnAuthorityThatWillNotLoadIsRefusedRatherThanGuessedAt(t *testing.T) {
	for _, bad := range [][2]string{
		{"", ""},
		{"not a key", "not a cert"},
		{"-----BEGIN EC PRIVATE KEY-----\nrubbish\n-----END EC PRIVATE KEY-----", ""},
	} {
		if _, err := Load([]byte(bad[0]), []byte(bad[1])); err == nil {
			t.Errorf("Load(%q, %q) was accepted", bad[0], bad[1])
		}
	}
}

func TestTheFingerprintIsTheSameOnBothSides(t *testing.T) {
	// The machine computes it from the certificate it was handed and compares
	// with the one in its join token. They must agree or the check is useless.
	ca := authority(t)
	cert, err := ParseCertificate(ca.CertPEM())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if Fingerprint(cert.Raw) != ca.Fingerprint() {
		t.Error("the two sides compute different fingerprints for one authority")
	}
	// And two authorities are told apart by it, which is the point.
	if authority(t).Fingerprint() == ca.Fingerprint() {
		t.Error("two authorities share a fingerprint")
	}
}

func TestRenewalHappensLongBeforeExpiryAndAfterIt(t *testing.T) {
	ca := authority(t)
	csr, _ := request(t, "nd_abc")
	issued := time.Now()
	certPEM, err := ca.Issue(csr, "nd_abc", nil, issued)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	cert, _ := ParseCertificate(certPEM)

	if NeedsRenewal(cert, issued) {
		t.Error("a certificate wanted renewing the moment it was issued")
	}
	if NeedsRenewal(cert, issued.Add(CertificateLife/4)) {
		t.Error("a certificate a quarter through its life wanted renewing")
	}
	// Past halfway: forty-five days of restarts in which to renew.
	if !NeedsRenewal(cert, issued.Add(CertificateLife*3/4)) {
		t.Error("a certificate three quarters through its life did not want renewing")
	}
	// Already expired, which is a machine that was down over the boundary. It
	// must still be told to renew, or it comes back unable to talk at all.
	if !NeedsRenewal(cert, issued.Add(CertificateLife*2)) {
		t.Error("an expired certificate did not want renewing")
	}
}

func TestAMachineCertificateCannotSignOthers(t *testing.T) {
	// One level, deliberately. A leaf that could sign would let any machine mint
	// identities for every other machine.
	ca := authority(t)
	csr, _ := request(t, "nd_abc")
	certPEM, _ := ca.Issue(csr, "nd_abc", nil, time.Now())
	cert, _ := ParseCertificate(certPEM)

	if cert.IsCA {
		t.Error("a machine certificate is an authority")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign != 0 {
		t.Error("a machine certificate may sign certificates")
	}
}

func TestAMachineCannotPresentItselfAsTheOrchestrator(t *testing.T) {
	// Every machine in the fleet holds a certificate from this authority. If one
	// of them were also good as a CLIENT, it could open the control surface of
	// every other machine as us. The two roles are separated by usage, and each
	// side's TLS stack enforces that without being asked to.
	ca := authority(t)
	csr, _ := request(t, "nd_abc")
	certPEM, _ := ca.Issue(csr, "nd_abc", nil, time.Now())
	machine, _ := ParseCertificate(certPEM)

	pool := ca.Pool()
	if _, err := machine.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err == nil {
		t.Error("a machine's certificate was accepted as a client certificate")
	}
	// And it does serve, which is what it is for.
	if _, err := machine.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   ServerName,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("a machine's certificate did not verify as a server: %v", err)
	}
}

func TestOurOwnCertificateIsAClientAndNothingElse(t *testing.T) {
	// The mirror image: we call machines and serve them nothing, so a stolen
	// copy of our certificate cannot be used to impersonate a machine.
	ca := authority(t)
	ours, err := ca.IssueClient("sag-orchestrator", time.Now())
	if err != nil {
		t.Fatalf("issue client: %v", err)
	}
	pool := ca.Pool()

	if _, err := ours.Leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("our own certificate did not verify as a client: %v", err)
	}
	if _, err := ours.Leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err == nil {
		t.Error("our own certificate was accepted as a server certificate")
	}
	if ours.PrivateKey == nil {
		t.Error("our own certificate came without its key, so nothing could use it")
	}
}

func TestResumingASessionDoesNotSkipCheckingWhichMachineAnswered(t *testing.T) {
	// The trap this was written against: the per-certificate hook is not called
	// at all when a session is resumed, so a check installed there passes once
	// and is then silently absent for every later connection to that address.
	// Here the second connection resumes and must still be judged.
	ca := authority(t)
	server := aServer(t, ca, "nd-1")

	// A cache shared between the two dials is what makes resumption possible at
	// all: it is where the ticket from the first lands.
	cache := tls.NewLRUClientSessionCache(4)
	ours, err := ca.IssueClient("sag-orchestrator", time.Now())
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	dial := func(expect string) (*tls.Conn, error) {
		cfg := ca.ClientTLS(ours, expect)
		cfg.ClientSessionCache = cache
		// TLS 1.3 sends its ticket after the handshake, so a resumption test
		// that never reads would race the ticket it depends on.
		conn, err := tls.Dial("tcp", server, cfg)
		if err != nil {
			return nil, err
		}
		if _, err := conn.Write([]byte("hello")); err != nil {
			return nil, err
		}
		buf := make([]byte, 5)
		if _, err := conn.Read(buf); err != nil {
			return nil, err
		}
		// TLS 1.3 hands out its ticket AFTER the handshake, and a client only
		// files one away while reading. Without this the cache stays empty and
		// the test would quietly prove nothing.
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _ = conn.Read(make([]byte, 1))
		_ = conn.SetReadDeadline(time.Time{})
		return conn, nil
	}

	first, err := dial("nd-1")
	if err != nil {
		t.Fatalf("the first connection failed: %v", err)
	}
	_ = first.Close()

	// Prove the shortcut is really available before relying on it, or the rest
	// of this would pass by proving nothing.
	second, err := dial("nd-1")
	if err != nil {
		t.Fatalf("the second connection failed: %v", err)
	}
	resumed := second.ConnectionState().DidResume
	_ = second.Close()
	if !resumed {
		t.Fatal("the session was not resumed, so this test cannot say anything")
	}

	// Now ask for a DIFFERENT machine at the same address. It must be refused,
	// and the whole point is that it must be refused on a handshake that skips
	// the certificates entirely.
	if third, err := dial("nd-2"); err == nil {
		_ = third.Close()
		t.Fatal("a resumed session answered for a machine we did not ask for")
	}
}

// aServer stands up something that serves as a machine and echoes, so a test can
// complete a handshake and read past it.
func aServer(t *testing.T, ca *Authority, nodeID string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: nodeID}}, key)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	certPEM, err := ca.Issue(csr, nodeID, []string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	identity, err := tls.X509KeyPair(certPEM,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	listener, err := tls.Listen("tcp", "127.0.0.1:0", ca.ServerTLS(identity))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 5)
				if _, err := conn.Read(buf); err == nil {
					_, _ = conn.Write(buf)
				}
			}()
		}
	}()
	return listener.Addr().String()
}

func TestEveryMachineAnswersToTheSameName(t *testing.T) {
	// It is what lets us dial a bare IP, a container name or a tunnel with the
	// ordinary verification path: we ask for a name no machine's ADDRESS has to
	// match, so an address that changes does not invalidate a certificate.
	ca := authority(t)
	for _, address := range []string{"", "203.0.113.7:9000", "gpu.example.com"} {
		csr, _ := request(t, "nd_abc")
		certPEM, _ := ca.Issue(csr, "nd_abc", []string{address}, time.Now())
		cert, _ := ParseCertificate(certPEM)
		if _, err := cert.Verify(x509.VerifyOptions{
			Roots:     ca.Pool(),
			DNSName:   ServerName,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			t.Errorf("a machine advertising %q did not answer to the fleet name: %v", address, err)
		}
	}
}

func TestTwoCertificatesNeverShareASerial(t *testing.T) {
	// A predictable or repeated serial helps anybody who can get one signed.
	ca := authority(t)
	seen := map[string]bool{}
	for range 20 {
		csr, _ := request(t, "nd_abc")
		certPEM, err := ca.Issue(csr, "nd_abc", nil, time.Now())
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		cert, _ := ParseCertificate(certPEM)
		serial := cert.SerialNumber.String()
		if seen[serial] {
			t.Fatalf("serial %s was issued twice", serial)
		}
		seen[serial] = true
	}
}
