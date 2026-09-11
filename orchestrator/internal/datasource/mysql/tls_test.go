package mysql

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"flexie.io/sag/internal/datasource"
)

// A plain mode maps to one of the driver's named shortcuts, and an unknown mode
// is refused before it can reach the driver.
func TestRegisterTLSNamedShortcuts(t *testing.T) {
	for mode, want := range map[string]string{"": "", "disable": "", "require": "skip-verify", "verify": "true"} {
		got, err := registerTLS(datasource.TLS{Mode: mode})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if got != want {
			t.Fatalf("mode %q: got %q, want %q", mode, got, want)
		}
	}
	if _, err := registerTLS(datasource.TLS{Mode: "bogus"}); err == nil {
		t.Fatal("an unknown TLS mode was accepted")
	}
}

// Custom material (a CA, a client certificate, a server name) builds a real
// config, registered under a name derived from its contents, so identical
// material registers once and different material never collides.
// Custom material (a CA, a client certificate, a server name) builds a real
// config, registered under a name derived from its contents, so identical
// material registers once and different material never collides.
func TestRegisterTLSCustom(t *testing.T) {
	cert, key := selfSigned(t)
	tl := datasource.TLS{Mode: "verify", CACert: cert, ClientCert: cert, ClientKey: key, ServerName: "db.internal"}

	c, err := datasource.TLSConfig(tl)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if c.RootCAs == nil {
		t.Fatal("the CA certificate was not loaded")
	}
	if len(c.Certificates) != 1 {
		t.Fatal("the client certificate was not loaded")
	}
	if c.ServerName != "db.internal" {
		t.Fatalf("the server name was not set: %q", c.ServerName)
	}

	name, err := registerTLS(tl)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !strings.HasPrefix(name, "sag-") {
		t.Fatalf("a custom config should get its own name: %q", name)
	}
	if again, _ := registerTLS(tl); again != name {
		t.Fatalf("identical material got two names: %q and %q", name, again)
	}
	other := tl
	other.ServerName = "elsewhere"
	if diff, _ := registerTLS(other); diff == name {
		t.Fatal("different material collided on one name")
	}
}

// Broken PEM is refused when the config is built, not on first connect.

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

// A plain mode maps to one of the driver's named shortcuts, and an unknown mode
// is refused before it can reach the driver.
// Broken PEM is refused when the config is built, not on first connect.
