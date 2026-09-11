package datasource

import "testing"

// The MySQL driver registers itself, and the registry exposes it by key, in the
// driver list, and by its secret fields, which is everything a caller (the query
// tool, a future workflow, the admin form) needs to find and configure it.
func TestOpenRejectsUnknownDriver(t *testing.T) {
	if _, err := Open(Config{Driver: "no-such-db"}); err == nil {
		t.Fatal("opening an unknown driver should fail")
	}
	if _, ok := Get("no-such-db"); ok {
		t.Fatal("an unknown driver reported as registered")
	}
	if keys := SecretFieldKeys("no-such-db"); keys != nil {
		t.Fatalf("secret fields for an unknown driver should be nil: %+v", keys)
	}
}
