package toolkit

import (
	"context"
	"strings"
	"testing"
)

// The SSRF guard is security-critical, so its refusals are pinned precisely: a
// public address is allowed, and every shape of internal or reserved address is
// blocked, whether given as a literal or reached through a name that resolves to
// one. A hole here is a path to the metadata endpoint.
func TestCheckURLBlocksInternalTargets(t *testing.T) {
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"public literal", "https://8.8.8.8/path", true},
		{"public literal with port", "https://1.1.1.1:8443/x", true},
		{"loopback literal", "http://127.0.0.1/", false},
		{"loopback name", "http://localhost/", false},
		{"ipv6 loopback", "http://[::1]/", false},
		{"private 10", "http://10.0.0.5/", false},
		{"private 192.168", "http://192.168.1.10/admin", false},
		{"private 172.16", "http://172.16.0.1/", false},
		{"link-local metadata", "http://169.254.169.254/latest/meta-data/", false},
		{"cgnat shared", "http://100.64.0.1/", false},
		{"this-network 0.x", "http://0.0.0.0/", false},
		{"benchmark 198.18", "http://198.18.0.1/", false},
		{"bad scheme ftp", "ftp://example.com/", false},
		{"bad scheme file", "file:///etc/passwd", false},
		{"no host", "http:///path", false},
		{"garbage", "::::not a url", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckURL(context.Background(), tc.url)
			if got.OK != tc.ok {
				t.Fatalf("CheckURL(%q).OK = %v, want %v (reason: %q)", tc.url, got.OK, tc.ok, got.Reason)
			}
			if !got.OK && got.Reason == "" {
				t.Fatalf("a refusal must carry a reason: %q", tc.url)
			}
		})
	}
}

func TestBlockedErrorNamesTheTarget(t *testing.T) {
	res := CheckURL(context.Background(), "http://169.254.169.254/")
	msg := res.BlockedError()
	if msg == "" {
		t.Fatal("a blocked result must render an error")
	}
	// The offending address is named, so a log or the model can see what was refused.
	if !strings.Contains(msg, "169.254.169.254") {
		t.Fatalf("the blocked error did not name the target: %q", msg)
	}
}
