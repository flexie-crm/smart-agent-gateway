package mysql

import (
	"testing"

	"flexie.io/sag/internal/datasource"
)

// The MySQL driver registers itself, and the registry exposes it by key, in the
// driver list, and by its secret fields, which is everything a caller (the query
// tool, a future workflow, the admin form) needs to find and configure it.
func TestMySQLDriverIsRegistered(t *testing.T) {
	d, ok := datasource.Get("mysql")
	if !ok {
		t.Fatal("the mysql driver did not register itself")
	}
	if d.Label() == "" || d.DefaultPort() != 3306 {
		t.Fatalf("driver metadata wrong: label=%q port=%d", d.Label(), d.DefaultPort())
	}

	var found bool
	for _, listed := range datasource.All() {
		if listed.Key() == "mysql" {
			found = true
		}
	}
	if !found {
		t.Fatal("the mysql driver is not in the driver list")
	}
}

// The driver declares its fields (so the admin form is built from the driver,
// not hard-coded), and its password field is the one secret to seal.
// The driver declares its fields (so the admin form is built from the driver,
// not hard-coded), and its password field is the one secret to seal.
func TestMySQLFieldsAndSecrets(t *testing.T) {
	d, _ := datasource.Get("mysql")
	keys := map[string]datasource.Field{}
	for _, f := range d.Fields() {
		keys[f.Key] = f
	}
	for _, required := range []string{"host", "database", "username", "password"} {
		if _, ok := keys[required]; !ok {
			t.Fatalf("the mysql driver does not declare the %q field", required)
		}
	}
	if !keys["password"].Secret {
		t.Fatal("the password field is not marked secret")
	}
	if keys["host"].Secret {
		t.Fatal("the host field must not be secret")
	}

	secrets := datasource.SecretFieldKeys("mysql")
	if len(secrets) != 1 || secrets[0] != "password" {
		t.Fatalf("secret fields wrong: %+v", secrets)
	}
}
