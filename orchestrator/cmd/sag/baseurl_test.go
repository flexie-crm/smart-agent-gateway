package main

import "testing"

// The callback a service sends somebody back to has to be this server, on the
// port it is really listening on. A working copy started anywhere but :8080 used
// to hand out a default that pointed at a port with nothing behind it.
func TestTheCallbackFollowsTheListenAddress(t *testing.T) {
	for _, c := range []struct{ explicit, addr, want string }{
		{"", ":8080", "http://localhost:8080"},
		{"", ":9000", "http://localhost:9000"},
		{"", "127.0.0.1:8080", "http://127.0.0.1:8080"},
		// Somebody who says where this is reachable means it, and it wins.
		{"https://sag.example.com", ":8080", "https://sag.example.com"},
	} {
		if got := personalBaseURL(c.explicit, c.addr); got != c.want {
			t.Fatalf("personalBaseURL(%q, %q) = %q, want %q", c.explicit, c.addr, got, c.want)
		}
	}
}
