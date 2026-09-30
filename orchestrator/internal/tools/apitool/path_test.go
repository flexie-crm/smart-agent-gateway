package apitool

import "testing"

// Everything a path may be, and everything it may not.
//
// This is the tool's floor: the administrator chose ONE api and the credentials
// are attached to every call, so a path that escapes turns "an API this
// assistant may use" into "any address, with your key on it".
func TestAPathStaysUnderTheBaseAddress(t *testing.T) {
	const base = "https://api.example.com/v1"

	allowed := []struct{ given, want string }{
		{"/invoices", "https://api.example.com/v1/invoices"},
		// With or without the leading slash: both are obviously meant.
		{"invoices", "https://api.example.com/v1/invoices"},
		{"/invoices/in_123/lines", "https://api.example.com/v1/invoices/in_123/lines"},
		// A trailing slash is meaningful to some APIs and survives.
		{"/invoices/", "https://api.example.com/v1/invoices/"},
		// Climbing that stays inside is fine: it resolves to a real path here.
		{"/invoices/../customers", "https://api.example.com/v1/customers"},
		// A space is encoded rather than refused.
		{"/search/one two", "https://api.example.com/v1/search/one%20two"},
	}
	for _, c := range allowed {
		got, err := Join(base, c.given)
		if err != nil {
			t.Errorf("Join(%q) was refused: %v", c.given, err)
			continue
		}
		if got != c.want {
			t.Errorf("Join(%q) = %q, want %q", c.given, got, c.want)
		}
	}

	refused := []struct{ name, given string }{
		{"a whole address", "https://elsewhere.example.com/steal"},
		{"a whole address, no tls", "http://elsewhere.example.com/steal"},
		{"scheme relative, which url.Parse reads as a host", "//elsewhere.example.com/steal"},
		{"climbing out", "/../../etc/passwd"},
		{"climbing out from inside, with no leading ..", "/invoices/../../secrets"},
		// The download host's trap: decoded AFTER the check, it survives.
		{"climbing out, encoded", "%2e%2e/%2e%2e/secrets"},
		{"climbing out, encoded mid-path", "/invoices/%2e%2e/%2e%2e/secrets"},
		{"climbing to a sibling that shares our prefix", "/../v1beta/invoices"},
		{"a backslash", `\\elsewhere.example.com\steal`},
		{"a query in the path", "/invoices?limit=10"},
		{"a fragment in the path", "/invoices#top"},
	}
	for _, c := range refused {
		got, err := Join(base, c.given)
		if err == nil {
			t.Errorf("%s: Join(%q) was allowed and resolved to %q", c.name, c.given, got)
		}
	}
}

// NO PATH is the base address itself, and that is a deliberate answer rather
// than a gap.
//
// It was refused as "path is required", which is right for an API (a base is a
// prefix, a call names something under it) and wrong for a single endpoint. A
// webhook listener address has nothing to append to it, and refusing the empty
// path left a model one legal alternative, "/", which is a different address.
func TestNoPathIsTheBaseAddressItself(t *testing.T) {
	for _, c := range []struct{ base, want string }{
		// Byte for byte: no slash invented.
		{"https://api.example.com/listener/a1b2c3d4/e5f6a7b8", "https://api.example.com/listener/a1b2c3d4/e5f6a7b8"},
		// And none taken away from an administrator who typed one.
		{"https://api.example.com/listener/a1b2c3d4/", "https://api.example.com/listener/a1b2c3d4/"},
		// A bare host is a bare host.
		{"https://api.example.com", "https://api.example.com"},
	} {
		for _, given := range []string{"", "   "} {
			got, err := Join(c.base, given)
			if err != nil {
				t.Fatalf("Join(%q, %q): %v", c.base, given, err)
			}
			if got != c.want {
				t.Fatalf("Join(%q, %q) = %q, want %q", c.base, given, got, c.want)
			}
		}
	}
}

// A base with no path of its own still confines: the host is the boundary.
func TestABaseWithNoPathStillConfines(t *testing.T) {
	const base = "https://api.example.com"
	if got, err := Join(base, "/things"); err != nil || got != "https://api.example.com/things" {
		t.Fatalf("Join = %q, %v", got, err)
	}
	// Climbing above the root cannot reach another host, and must not produce
	// something odd either.
	got, err := Join(base, "/../../x")
	if err != nil {
		return // refused outright is fine
	}
	if got != "https://api.example.com/x" {
		t.Fatalf("climbing above the root produced %q", got)
	}
}

// The boundary is a path segment, not a string prefix.
//
// A base of /v1 must not admit /v1beta: they share five characters and are
// different APIs.
func TestTheBoundaryIsASegmentNotAPrefix(t *testing.T) {
	if !under("/v1", "/v1") {
		t.Error("the base itself is not under itself")
	}
	if !under("/v1", "/v1/x") {
		t.Error("a child is not under the base")
	}
	if under("/v1", "/v1beta") {
		t.Error("/v1beta was admitted under /v1, which is a different API")
	}
	if under("/v1", "/v2") {
		t.Error("/v2 was admitted under /v1")
	}
}
