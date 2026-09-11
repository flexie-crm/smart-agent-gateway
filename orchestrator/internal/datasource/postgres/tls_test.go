package postgres

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"flexie.io/sag/internal/datasource"
)

// What an administrator asking for TLS actually gets. It is checked here rather
// than against a server because it is not visible from a *sql.DB: a mistake in
// this mapping would be a connection that looks encrypted and is not, or one
// that verifies nothing, and neither announces itself.

func TestTheThreeModes(t *testing.T) {
	for _, mode := range []string{"", "disable"} {
		c, err := transport(datasource.Config{Host: "db.internal", TLS: datasource.TLS{Mode: mode}})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if c != nil {
			t.Fatalf("mode %q should be a plain connection, got a TLS config", mode)
		}
	}

	// require encrypts and does not check who answered.
	c, err := transport(datasource.Config{Host: "db.internal", TLS: datasource.TLS{Mode: "require"}})
	if err != nil {
		t.Fatalf("require: %v", err)
	}
	if c == nil || !c.InsecureSkipVerify {
		t.Fatalf("require must encrypt without verifying: %+v", c)
	}

	// verify encrypts and checks.
	c, err = transport(datasource.Config{Host: "db.internal", TLS: datasource.TLS{Mode: "verify"}})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c == nil || c.InsecureSkipVerify {
		t.Fatal("verify must check the certificate")
	}

	if _, err := transport(datasource.Config{TLS: datasource.TLS{Mode: "bogus"}}); err == nil {
		t.Fatal("an unknown TLS mode was accepted")
	}
}

// A private CA, a client certificate and a server name are the settings that
// make TLS usable on somebody else's network, and all three are shared with
// every other driver.
func TestCustomMaterialIsCarriedThrough(t *testing.T) {
	cert, key := selfSigned(t)
	c, err := transport(datasource.Config{Host: "db.internal", TLS: datasource.TLS{
		Mode: "verify", CACert: cert, ClientCert: cert, ClientKey: key, ServerName: "elsewhere",
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if c.RootCAs == nil {
		t.Fatal("the CA certificate was not loaded")
	}
	if len(c.Certificates) != 1 {
		t.Fatal("the client certificate was not loaded")
	}
	if c.ServerName != "elsewhere" {
		t.Fatalf("a server name that was given must win over the host: %q", c.ServerName)
	}

	// Broken material is refused when the config is built, not on first connect.
	if _, err := transport(datasource.Config{TLS: datasource.TLS{Mode: "verify", CACert: "not pem"}}); err == nil {
		t.Fatal("a CA certificate that is not PEM was accepted")
	}
}

// The name the certificate is checked against, when nobody said one.
//
// This is the one that matters through an SSH bastion: by the time a driver is
// called the host is the local end of the tunnel, so verify without a Server
// name checks the certificate against 127.0.0.1 and fails. That is the safe way
// round, and it is the same on the MySQL driver, whose own library fills the
// name in from the address for exactly the same reason.
func TestTheServerNameDefaultsToTheHost(t *testing.T) {
	c, err := transport(datasource.Config{Host: "db.internal", TLS: datasource.TLS{Mode: "verify"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if c.ServerName != "db.internal" {
		t.Fatalf("ServerName = %q, want the host", c.ServerName)
	}

	// Through a tunnel the host is the forwarder, and nothing pretends otherwise.
	c, err = transport(datasource.Config{Host: "127.0.0.1", TLS: datasource.TLS{Mode: "verify"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if c.ServerName != "127.0.0.1" {
		t.Fatalf("ServerName = %q; a tunnelled connection must not silently borrow the real hostname", c.ServerName)
	}
}

func selfSigned(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}
