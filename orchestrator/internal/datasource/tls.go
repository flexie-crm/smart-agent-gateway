package datasource

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
)

// TLSFields are the transport-security settings, shared across drivers so every
// database configures TLS the same way. The mode chooses the level; the rest
// support a private CA, a server name that differs from the host, and mutual
// TLS with a client certificate.
func TLSFields() []Field {
	return []Field{
		{Key: "tls.mode", Label: "Encryption", Type: FieldSelect, Default: "disable", Span: 2,
			Options: []Option{
				Choice("disable", "Off"),
				Choice("require", "Encrypted"),
				Choice("verify", "Encrypted, and the database verified"),
			},
			// No help line: the choices say what they do. One was there restating
			// them in a sentence, which is a paragraph asking to be read before a
			// dropdown that has already answered.
		},
		{Key: "tls.server_name", Label: "Server name", Type: FieldText, Span: 4,
			Help: "Name to expect on the certificate, if it differs from the host."},
		{Key: "tls.ca_cert", Label: "CA certificate", Type: FieldTextarea,
			Help: "PEM authority that signed the server certificate, to trust a private CA."},
		{Key: "tls.client_cert", Label: "Client certificate", Type: FieldTextarea,
			Help: "PEM client certificate, for mutual TLS."},
		{Key: "tls.client_key", Label: "Client key", Type: FieldTextarea, Secret: true,
			Help: "PEM private key for the client certificate."},
	}
}

// TLSSecretFields are the TLS config keys sealed at rest: the client key.
func TLSSecretFields() []string { return []string{"tls.client_key"} }

// custom reports whether these settings need a built tls.Config rather than one
// of a driver's named shortcuts: a CA to trust, a client certificate to present,
// or a server name to expect.
func (t TLS) custom() bool {
	return t.CACert != "" || t.ClientCert != "" || t.ClientKey != "" || t.ServerName != ""
}

// tlsConfig builds a *tls.Config for require or verify: require encrypts without
// verifying the server, verify checks it against the CA (a private one when
// given, the system roots otherwise). A client certificate, when present,
// authenticates this end.
func tlsConfig(t TLS) (*tls.Config, error) {
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	switch t.Mode {
	case "require":
		c.InsecureSkipVerify = true
	case "verify":
		if t.CACert != "" {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM([]byte(t.CACert)) {
				return nil, fmt.Errorf("the CA certificate is not valid PEM")
			}
			c.RootCAs = pool
		}
	default:
		return nil, fmt.Errorf("a certificate needs TLS mode require or verify")
	}
	if t.ServerName != "" {
		c.ServerName = t.ServerName
	}
	if t.ClientCert != "" || t.ClientKey != "" {
		cert, err := tls.X509KeyPair([]byte(t.ClientCert), []byte(t.ClientKey))
		if err != nil {
			return nil, fmt.Errorf("the client certificate and key are not a valid pair")
		}
		c.Certificates = []tls.Certificate{cert}
	}
	return c, nil
}

// CustomTLS and TLSConfig are what a driver package needs to turn these settings
// into a connection of its own. They are the same two calls this package makes;
// a driver lives in its own folder and cannot reach an unexported one.
func CustomTLS(t TLS) bool { return t.custom() }

func TLSConfig(t TLS) (*tls.Config, error) { return tlsConfig(t) }
