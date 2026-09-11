package httprequest

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The form encoder is what a model relies on to send urlencoded fields without
// pre-encoding them. Scalars, arrays (bracketed indices), and nested objects
// (bracketed keys) all have a defined wire format, and the output is
// deterministic so a test can assert on it.
func TestEncodeForm(t *testing.T) {
	got, err := encodeForm(map[string]any{
		"q":      "pizza place",
		"tags":   []any{"vegan", "gluten-free"},
		"filter": map[string]any{"status": "open"},
		"n":      float64(3),
		"active": true,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, want := range []string{
		"q=pizza+place",           // scalar, space encoded
		"tags%5B0%5D=vegan",       // tags[0]=vegan
		"tags%5B1%5D=gluten-free", // tags[1]=gluten-free
		"filter%5Bstatus%5D=open", // filter[status]=open
		"n=3",                     // number
		"active=true",             // bool
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("encoded form missing %q:\n%s", want, got)
		}
	}
}

// The request builder sets a default Content-Type only when the caller did not,
// drops headers the client must own, and always identifies itself.
func TestBuildRequestHeaders(t *testing.T) {
	req, err := buildRequest(context.Background(), http.MethodPost, request{
		URL:     "https://api.example.com/x",
		Headers: map[string]string{"Authorization": "Bearer T", "Host": "evil", "Content-Length": "999"},
		JSON:    []byte(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if req.Header.Get("Authorization") != "Bearer T" {
		t.Fatal("a caller header was dropped")
	}
	if req.Header.Get("Host") != "" || req.Header.Get("Content-Length") != "" {
		t.Fatal("a client-owned header was allowed through")
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("the json content type was not defaulted: %q", req.Header.Get("Content-Type"))
	}
	if req.Header.Get("User-Agent") == "" {
		t.Fatal("no User-Agent was set")
	}
}

// A caller's own Content-Type wins over the default.
func TestBuildRequestKeepsCallerContentType(t *testing.T) {
	req, err := buildRequest(context.Background(), http.MethodPost, request{
		URL:     "https://api.example.com/x",
		Headers: map[string]string{"Content-Type": "application/vnd.api+json"},
		JSON:    []byte(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if req.Header.Get("Content-Type") != "application/vnd.api+json" {
		t.Fatalf("the caller's content type was overridden: %q", req.Header.Get("Content-Type"))
	}
}
