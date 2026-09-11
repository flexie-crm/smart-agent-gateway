// Package nodeca is the certificate authority for machines that run our models.
//
// A machine used to be reachable only inside one network, and KB/35 said so:
// plain HTTP, the sealed key as the authentication, no external hop. Putting a
// machine on a public address breaks that premise, and a shared key over plain
// HTTP on the open internet is a key anybody in the path can read.
//
// So the two ends authenticate each other with certificates, and this issues
// them. Deliberately NOT a public authority: a public one vouches that somebody
// controls a DOMAIN, which is meaningless between two machines you own and
// address by IP. What matters here is that the machine holding this certificate
// is the machine that joined, and that is exactly what a private authority can
// say and a public one cannot.
//
// The shape is Docker Swarm's, which is also where the join came from. The
// authority signs a leaf whose common name is the NODE ID, because that is
// already how a machine is recognised when its address changes. Verification
// then checks the chain and that name, never a hostname, which is what lets a
// bare IP work with nothing special.
//
// The part that makes it safe over an open network is in the join token, not
// here: the token carries this authority's FINGERPRINT, so a joining machine
// checks who it is talking to before it sends the secret. Without that, the
// first thing to answer gets the secret, which on a public network is a machine
// in the middle waiting to happen.
package nodeca

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// How long the authority itself lasts.
//
// Long, because replacing it means re-enrolling every machine: it is the thing
// every certificate chains to. Ten years is what an internal authority is
// usually given, and rotating it early is a deliberate operation rather than
// something anybody should be forced into by an expiry.
const authorityLife = 10 * 365 * 24 * time.Hour

// How long a machine's certificate lasts.
//
// Short, because a leaf is cheap to replace: a machine re-enrols every time it
// starts, so ninety days is dozens of renewals for a machine that is restarted
// even monthly. Short-lived leaves are what make a stolen one worth little.
const CertificateLife = 90 * 24 * time.Hour

// When a certificate is old enough to be worth replacing.
//
// Half its life. Renewing at every join would issue a certificate per restart
// for no gain; waiting until it has expired means a machine that was down over
// the boundary comes back unable to talk. Halfway gives forty-five days of
// restarts in which to renew, which no working machine will miss.
const renewAfter = CertificateLife / 2

// ServerName is the name every machine's certificate carries, and the name we
// ask for when we dial one.
//
// It is deliberately the SAME on every machine, and deliberately not a real
// name: nothing resolves it and nothing is meant to. Naming a machine after its
// address would mean a certificate that stops verifying the moment the address
// changes, which is the situation node ids exist to survive; asking for the
// address we dialled would mean a certificate per address. Asking for a fixed
// name lets us dial a bare IP, a container name or a tunnel with the ordinary
// verification path and no exceptions carved into it.
//
// What it therefore proves is "a machine of this fleet", which is precisely what
// the authority can vouch for. WHICH machine answered is a separate question,
// and the control surface answers it by checking the common name against the
// machine it meant to call.
const ServerName = "machine.sag.internal"

// Authority is the signing identity: its key, and the certificate every machine
// chains to.
type Authority struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
	// certPEM is kept because it is handed to every machine, and re-encoding it
	// on each join would be work for a value that never changes.
	certPEM []byte
}

// Create makes a new authority.
//
// P-256 rather than RSA: it is smaller, faster to verify, and universally
// supported by the TLS stacks at both ends. There is no interoperability reason
// to reach for RSA here, since nothing outside this product ever sees it.
func Create(now time.Time) (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("nodeca: generate authority key: %w", err)
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "SAG machine authority"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(authorityLife),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		// One level: this signs machines and servers, and nothing signs on its
		// behalf. A path length of zero says so rather than leaving it open.
		MaxPathLen:     0,
		MaxPathLenZero: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("nodeca: create authority certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("nodeca: parse authority certificate: %w", err)
	}
	return &Authority{
		key:     key,
		cert:    cert,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

// Load rebuilds an authority from what was stored.
func Load(keyPEM, certPEM []byte) (*Authority, error) {
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("nodeca: the authority key is not readable")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("nodeca: parse authority key: %w", err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("nodeca: the authority certificate is not readable")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("nodeca: parse authority certificate: %w", err)
	}
	return &Authority{key: key, cert: cert, certPEM: certPEM}, nil
}

// KeyPEM is the authority's private key, for sealing and storing. It is the one
// secret in this package and never leaves the server.
func (a *Authority) KeyPEM() ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(a.key)
	if err != nil {
		return nil, fmt.Errorf("nodeca: encode authority key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// CertPEM is the authority's certificate. Every machine gets a copy: it is what
// they verify us against, and what we verify them against.
func (a *Authority) CertPEM() []byte { return a.certPEM }

// Fingerprint identifies this authority in one short string.
//
// It goes in the join token so a machine can check who answered BEFORE it sends
// the secret. That is the whole of the bootstrap problem: at that moment the
// machine has nothing else to trust, and without this it would trust whatever
// replied.
func (a *Authority) Fingerprint() string {
	return Fingerprint(a.cert.Raw)
}

// Fingerprint is the same value computed from a certificate's raw bytes, for the
// side that has only received one.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// Issue signs a certificate for a machine, from the request it sent.
//
// The common name is the NODE ID and not an address, because a machine is its
// node id here: that is what makes a container rescheduled onto a new address
// the same machine. The address goes in as well, when there is one, so a stack
// that insists on matching what it dialled has something to match; verification
// does not depend on it.
func (a *Authority) Issue(csrDER []byte, nodeID string, addresses []string, now time.Time) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("nodeca: that is not a certificate request: %w", err)
	}
	// Checked, because a request carries a signature made with the key it is
	// asking us to certify. Skipping it would let one machine ask for a
	// certificate over another machine's key.
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("nodeca: the certificate request is not signed by its own key: %w", err)
	}
	if nodeID == "" {
		return nil, fmt.Errorf("nodeca: a certificate needs a machine to belong to")
	}

	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(CertificateLife),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		// A machine SERVES, and that is all it may do. Giving it the client usage
		// as well would mean any machine could present its certificate to another
		// machine AS the orchestrator, and every machine in the fleet holds one.
		// The two roles are separated here rather than by a rule somebody has to
		// remember, and each side's TLS stack enforces it as a matter of course.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{ServerName},
	}
	applyAddresses(template, addresses)

	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, csr.PublicKey, a.key)
	if err != nil {
		return nil, fmt.Errorf("nodeca: sign certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// IssueClient mints the certificate the ORCHESTRATOR presents to machines.
//
// Key and all, in one call and in memory: unlike a machine's, this certificate
// is not on the other side of a network, so there is nothing to be gained by
// making it travel and nothing to store. It is made fresh at every boot, which
// means it is never old and there is no renewal to get wrong.
//
// The client usage and nothing else, for the same reason a machine gets the
// server usage and nothing else: neither can stand in for the other.
func (a *Authority) IssueClient(commonName string, now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("nodeca: generate client key: %w", err)
	}
	serial, err := serialNumber()
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(CertificateLife),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("nodeca: sign client certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("nodeca: parse client certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// Pool is the authority as something to verify against.
func (a *Authority) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)
	return pool
}

// ClientTLS is how we dial a machine.
//
// nodeID is the machine we mean to reach, and empty means "any machine of this
// fleet". Both are honest positions and the difference is real: the control
// surface knows which machine it is calling and says so, while inference goes
// through the model gateway, where a vendor row is all there is and the machine
// behind it is not the gateway's business.
func (a *Authority) ClientTLS(ours tls.Certificate, nodeID string) *tls.Config {
	cfg := &tls.Config{
		Certificates: []tls.Certificate{ours},
		RootCAs:      a.Pool(),
		// The fleet name, asked for instead of the host we dialled: see
		// ServerName above for why that is what makes a bare IP work.
		ServerName: ServerName,
		MinVersion: tls.VersionTLS13,
	}
	if nodeID != "" {
		cfg.VerifyConnection = requireCommonName(nodeID)
	}
	return cfg
}

// ClientTLSPinned is how we dial a machine that issued its own certificate.
//
// A machine that joined was signed by this authority, so it is verified by
// chaining to it, and one certificate is as good as another of the same fleet.
// A machine enrolled by hand never spoke to us: it generated its key and signed
// its own certificate, which chains to nothing, so there is no authority to
// check it against and the certificate ITSELF is the only thing that identifies
// it. We hold the exact bytes and accept nothing else.
//
// That makes it strictly narrower than the fleet case rather than weaker. A
// forged certificate for the same name does not help, because the name is not
// what is checked; only the one certificate pasted in by the person who read it
// off the machine will do.
//
// Verification is therefore by hand, and `InsecureSkipVerify` is what turns the
// library's own check off so ours can run. The name is exactly wrong here: the
// check below is tighter than the one being skipped, which only ever asked
// whether some authority vouched for the name.
func ClientTLSPinned(ours tls.Certificate, pinnedPEM string) (*tls.Config, error) {
	block, _ := pem.Decode([]byte(pinnedPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("nodeca: that is not a certificate")
	}
	pinned, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("nodeca: that certificate cannot be read: %w", err)
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{ours},
		InsecureSkipVerify: true, //nolint:gosec // replaced by the exact-match check below
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			// The leaf and nothing else. A chain sent by the machine is not
			// evidence about anything here: what we are asserting is that the
			// certificate is the one somebody copied off that machine.
			if len(raw) == 0 {
				return fmt.Errorf("that machine presented no certificate")
			}
			if !bytes.Equal(raw[0], pinned.Raw) {
				return fmt.Errorf("that machine presented a different certificate " +
					"from the one it was added with")
			}
			return nil
		},
	}, nil
}

// ServerTLS is how a machine serves: its own certificate, and a demand that
// whoever calls presents one from this authority.
//
// The machine that runs in production is written in another language and builds
// this for itself, so what this really is, is the CONTRACT stated in code, and
// the fake machine in the tests holds the real side of it. A machine's own
// certificate cannot stand in for a caller's, because it does not carry the
// client usage, so this refuses one machine calling another as us.
func (a *Authority) ServerTLS(own tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{own},
		ClientCAs:    a.Pool(),
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
}

// requireCommonName checks that the machine which answered is the one we meant
// to call.
//
// The chain and the fleet name are verified by the time this runs, so what is
// left is WHICH machine, and its certificate says so in the common name.
// Without this, a machine that took over another's address could answer for it
// with a certificate of its own that is perfectly valid and belongs to somebody
// else.
//
// Deliberately on the CONNECTION rather than on the certificates. The two hooks
// look interchangeable and are not: the per-certificate one is skipped entirely
// when a session is resumed, so a machine that had once been talked to could
// answer for another on the second connection and never be looked at. This one
// runs on every handshake, resumed or not, and the state carries the peer's
// certificates either way.
func requireCommonName(nodeID string) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) > 0 &&
			state.PeerCertificates[0].Subject.CommonName == nodeID {
			return nil
		}
		return fmt.Errorf("nodeca: the machine that answered is not %s", nodeID)
	}
}

// NeedsRenewal reports whether a certificate is far enough through its life to
// be worth replacing.
//
// Asked at every join, which is every boot, so a machine restarted at all in
// forty-five days never reaches its expiry. One that has ALREADY expired also
// answers true: it can still re-enrol with the token it holds.
func NeedsRenewal(cert *x509.Certificate, now time.Time) bool {
	return now.After(cert.NotAfter.Add(-renewAfter))
}

// ParseCertificate reads one back, for deciding whether it needs renewing.
func ParseCertificate(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("nodeca: that is not a certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("nodeca: parse certificate: %w", err)
	}
	return cert, nil
}

// applyAddresses puts whatever we were told into the certificate, as the right
// kind of name for what it is.
func applyAddresses(template *x509.Certificate, addresses []string) {
	for _, address := range addresses {
		if address == "" {
			continue
		}
		if ip := parseIP(address); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
			continue
		}
		template.DNSNames = append(template.DNSNames, address)
	}
}

// parseIP is net.ParseIP, named here so the address logic above reads as one
// idea rather than as a type check.
func parseIP(address string) net.IP { return net.ParseIP(address) }

func serialNumber() (*big.Int, error) {
	// 128 bits, which is what a serial is expected to carry: it must be
	// unpredictable, since a predictable one helps an attacker who can get a
	// certificate signed.
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("nodeca: serial number: %w", err)
	}
	return serial, nil
}
