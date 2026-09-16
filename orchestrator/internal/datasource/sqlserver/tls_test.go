package sqlserver

import (
	"testing"

	"flexie.io/sag/internal/datasource"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// What an administrator asking for TLS actually gets. It is checked here rather
// than against a server because it is not visible from a *sql.DB: a mistake in
// this mapping would be a connection that looks encrypted and is not, and it
// would not announce itself.
//
// This driver has a second half the others do not. The encryption LEVEL and the
// certificate settings are separate values, and the level's zero value is a trap
// (see transport's own comment), so both are asserted here.

func TestTheThreeModes(t *testing.T) {
	for _, mode := range []string{"", "disable"} {
		level, c, err := transport(datasource.Config{Host: "db.internal", TLS: datasource.TLS{Mode: mode}})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if c != nil {
			t.Fatalf("mode %q should be a plain connection, got a TLS config", mode)
		}
		// EncryptionDisabled, never the zero value: see below.
		if level != msdsn.EncryptionDisabled {
			t.Fatalf("mode %q should be EncryptionDisabled (%d), got %d",
				mode, msdsn.EncryptionDisabled, level)
		}
	}

	// require encrypts and does not check who answered.
	level, c, err := transport(datasource.Config{Host: "db.internal", TLS: datasource.TLS{Mode: "require"}})
	if err != nil {
		t.Fatalf("require: %v", err)
	}
	if c == nil || !c.InsecureSkipVerify {
		t.Fatalf("require must encrypt without verifying: %+v", c)
	}
	if level != msdsn.EncryptionRequired {
		t.Fatalf("require must ask for encryption, got level %d", level)
	}

	// verify encrypts and checks.
	level, c, err = transport(datasource.Config{Host: "db.internal", TLS: datasource.TLS{Mode: "verify"}})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c == nil || c.InsecureSkipVerify {
		t.Fatal("verify must check the certificate")
	}
	if level != msdsn.EncryptionRequired {
		t.Fatalf("verify must ask for encryption, got level %d", level)
	}

	if _, _, err := transport(datasource.Config{TLS: datasource.TLS{Mode: "bogus"}}); err == nil {
		t.Fatal("an unknown TLS mode was accepted")
	}
}

// The one that would be silent.
//
// msdsn.Config's zero Encryption is EncryptionOff, and EncryptionOff is not off:
// the driver does a TLS handshake for the login packet and then puts the plain
// socket back underneath, so the password is encrypted and every row of every
// answer is not. An administrator who chose "Off" is owed a connection that is
// honestly plain, and one who chose nothing must never land on the halfway
// setting by accident.
//
// This asserts the mapping never produces that value, which is the whole
// difference between a plaintext connection somebody chose and one nobody did.
func TestTheHalfwayEncryptionLevelIsNeverChosen(t *testing.T) {
	for _, mode := range []string{"", "disable", "require", "verify"} {
		level, _, err := transport(datasource.Config{Host: "db.internal", TLS: datasource.TLS{Mode: mode}})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if level == msdsn.EncryptionOff {
			t.Fatalf("mode %q produced EncryptionOff, which encrypts the login and "+
				"sends the data in the clear", mode)
		}
	}

	// The control: EncryptionOff really is the zero value, so the assertion above
	// is about something rather than about nothing. If this ever fails, the test
	// above stopped testing what it claims to.
	var unset msdsn.Config
	if unset.Encryption != msdsn.EncryptionOff {
		t.Fatalf("the zero value is no longer EncryptionOff (%d), so the trap this "+
			"guards against has changed shape: re-read transport's comment",
			unset.Encryption)
	}
}

// A server name that differs from the host is what makes verify usable through
// an SSH bastion, where by connect time the host is the local end of a tunnel.
// Without it, verify would check the certificate against 127.0.0.1.
func TestTheServerNameToCheckAgainst(t *testing.T) {
	_, c, err := transport(datasource.Config{Host: "db.internal", TLS: datasource.TLS{Mode: "verify"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.ServerName != "db.internal" {
		t.Fatalf("with nothing said, the host is the name to expect, got %q", c.ServerName)
	}

	_, c, err = transport(datasource.Config{
		Host: "127.0.0.1",
		TLS:  datasource.TLS{Mode: "verify", ServerName: "sql.customer.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.ServerName != "sql.customer.example" {
		t.Fatalf("a server name that was given must win over the host, got %q", c.ServerName)
	}
}
