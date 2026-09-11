package datasource

import (
	"testing"
)

func TestTLSConfigRejectsBadMaterial(t *testing.T) {
	if _, err := tlsConfig(TLS{Mode: "verify", CACert: "not a certificate"}); err == nil {
		t.Fatal("an invalid CA certificate was accepted")
	}
	if _, err := tlsConfig(TLS{Mode: "require", ClientCert: "nope", ClientKey: "nope"}); err == nil {
		t.Fatal("an invalid client certificate and key were accepted")
	}
}
