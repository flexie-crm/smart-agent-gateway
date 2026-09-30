package oauthclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// metadataServer is a service publishing RFC 8414 metadata, remembering how it
// was asked so the probe itself can be asserted.
type metadataServer struct {
	ts *httptest.Server

	mu      sync.Mutex
	methods []string
	pkce    []string
}

func newMetadataServer(t *testing.T, pkce []string) *metadataServer {
	t.Helper()
	m := &metadataServer{pkce: pkce}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{
			"issuer":                 m.ts.URL,
			"authorization_endpoint": m.ts.URL + "/oauth/authorize",
			"token_endpoint":         m.ts.URL + "/oauth/token",
			"scopes_supported":       []string{"read", "write"},
		}
		if m.pkce != nil {
			doc["code_challenge_methods_supported"] = m.pkce
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	// Everything else, the base address included, records how it was asked.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.methods = append(m.methods, r.Method)
		m.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	})
	m.ts = httptest.NewServer(mux)
	t.Cleanup(m.ts.Close)
	return m
}

func (m *metadataServer) asked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.methods...)
}

// The point of the whole thing: a service that publishes its metadata is not
// typed in. Somebody configuring a tool gives the base address and the client
// their application was issued, and the two sign-in addresses are found.
func TestAnOrdinaryServicesEndpointsAreFoundRatherThanTyped(t *testing.T) {
	service := newMetadataServer(t, []string{"S256"})

	found, err := DiscoverEndpoints(context.Background(), service.ts.URL+"/v1")
	if err != nil {
		t.Fatalf("a service publishing its metadata was not read: %v", err)
	}
	if found.AuthorizationEndpoint != service.ts.URL+"/oauth/authorize" {
		t.Fatalf("the sign-in address was not found: %q", found.AuthorizationEndpoint)
	}
	if found.TokenEndpoint != service.ts.URL+"/oauth/token" {
		t.Fatalf("the token address was not found: %q", found.TokenEndpoint)
	}
	// No resource indicator: that is an MCP notion and this service published none.
	if found.Resource != "" {
		t.Fatalf("a resource was invented for an ordinary service: %q", found.Resource)
	}
}

// The divergence from MCP, and the reason DiscoverEndpoints exists at all.
//
// The MCP specification DEFINES an absent code_challenge_methods_supported as
// "this server does not support PKCE", and a client must refuse. Out here the
// same absence means the document does not mention it, which is the case for
// Microsoft's, whose servers do support PKCE. Refusing there would make a
// service unconnectable for a property it has.
//
// Both halves are asserted against ONE document, so this cannot pass by the two
// paths quietly becoming the same thing.
func TestSilenceAboutPKCEStopsMCPAndNotAnOrdinaryService(t *testing.T) {
	service := newMetadataServer(t, nil) // publishes no code_challenge_methods_supported

	if _, err := Discover(context.Background(), service.ts.URL+"/mcp"); err == nil {
		t.Fatal("an MCP server silent about PKCE was accepted, which the specification forbids")
	}

	found, err := DiscoverEndpoints(context.Background(), service.ts.URL+"/v1")
	if err != nil {
		t.Fatalf("an ordinary service silent about PKCE was refused: %v", err)
	}
	if found.TokenEndpoint == "" {
		t.Fatal("the endpoints were not read")
	}
}

// A base address somebody typed is not ours to POST to. The MCP probe opens
// with an initialize call, which is right for an MCP endpoint and wrong for an
// arbitrary address, where a POST is not always the no-op it is there.
func TestAnOrdinaryServiceIsOnlyEverAsked(t *testing.T) {
	service := newMetadataServer(t, []string{"S256"})

	if _, err := DiscoverEndpoints(context.Background(), service.ts.URL+"/v1"); err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	for _, method := range service.asked() {
		if method != http.MethodGet {
			t.Fatalf("an address somebody typed was sent a %s: %v", method, service.asked())
		}
	}

	// And the MCP probe still opens with its POST, so this did not quietly
	// change the protocol client's behaviour.
	mcp := newMetadataServer(t, []string{"S256"})
	_, _ = Discover(context.Background(), mcp.ts.URL+"/mcp")
	sawPost := false
	for _, method := range mcp.asked() {
		if method == http.MethodPost {
			sawPost = true
		}
	}
	if !sawPost {
		t.Fatalf("the MCP probe stopped opening with initialize: %v", mcp.asked())
	}
}

// A service that publishes nothing is a message naming what to do, not a crash.
func TestAServiceThatPublishesNothingIsReported(t *testing.T) {
	quiet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer quiet.Close()

	if _, err := DiscoverEndpoints(context.Background(), quiet.URL+"/v1"); err == nil {
		t.Fatal("a service publishing nothing was reported as discovered")
	}
}
