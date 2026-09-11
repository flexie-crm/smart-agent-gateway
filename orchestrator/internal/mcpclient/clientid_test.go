package mcpclient

import "testing"

// Three ways to be a client, and which one a server will take is a property of
// the server. These pin the reading of what it advertises, because getting it
// wrong produces a connection that fails at the far end, where the person
// cannot see why.

func TestAServerThatOnlyTakesPublicClientsIsRegisteredAsOne(t *testing.T) {
	public := &Discovery{TokenEndpointAuthMethods: []string{"none"}}
	if !public.PrefersPublicClient() {
		t.Fatal("a server advertising only \"none\" was read as confidential")
	}
	if got := public.RegistrationAuthMethod(); got != "none" {
		t.Fatalf("registering as %q with a server that issues no secret", got)
	}
}

func TestAServerThatTakesASecretStillGetsAConfidentialClient(t *testing.T) {
	for _, methods := range [][]string{
		{"client_secret_basic"},
		{"client_secret_post"},
		{"none", "client_secret_basic"},
		nil, // said nothing: the OAuth default is client_secret_basic
	} {
		d := &Discovery{TokenEndpointAuthMethods: methods}
		if d.PrefersPublicClient() {
			t.Fatalf("%v was read as public-only", methods)
		}
		if got := d.RegistrationAuthMethod(); got != "client_secret_basic" {
			t.Fatalf("%v registered as %q", methods, got)
		}
	}
}

// A local installation borrows a published identity rather than serving one:
// nothing outside can fetch a laptop, however its address is spelled. The answer
// comes from the FLAG (sag dev, sag personal), not from reading the address.
func TestALocalInstallationBorrowsThePublishedIdentity(t *testing.T) {
	// No configuration at all: the compiled-in default, because somebody running
	// the personal edition is never going to set one.
	if got := ClientMetadataURL("http://localhost:8080", true, ""); got != DefaultClientMetadataURL {
		t.Fatalf("a local install presented %q, want the shared identity", got)
	}
	// And a deployment that hosts its own for its own installs is honoured.
	const own = "https://sag.acme.example/connect/mcp/client-metadata.json"
	if got := ClientMetadataURL("http://localhost:8080", true, own); got != own {
		t.Fatalf("a configured identity was ignored: %q", got)
	}
}

// A deployment publishes and presents its own.
func TestADeploymentPresentsItsOwnDocument(t *testing.T) {
	const base = "https://sag.example.com"
	got := ClientMetadataURL(base, false, "")
	if got != base+"/connect/mcp/client-metadata.json" {
		t.Fatalf("client id %q", got)
	}

	// The draft has the document name its own address, so a server can confirm
	// it is reading about the client it was told about.
	doc := NewClientMetadata(got, base+"/connect/mcp/callback", "mcp")
	if doc.ClientID != got {
		t.Fatalf("the document calls itself %q, not %q", doc.ClientID, got)
	}
	// A public client by construction: this flow has no secret, so asking for
	// one would be asking for something we could not keep.
	if doc.TokenEndpointAuthMethod != "none" {
		t.Fatalf("auth method %q", doc.TokenEndpointAuthMethod)
	}
	// And it names the loopback callbacks, which is what lets one published
	// document be the identity of every installation on somebody's own machine.
	// Without them an authorization server, which matches redirect URIs exactly,
	// would refuse every local install using it.
	var loopback int
	for _, u := range doc.RedirectURIs {
		if u == "http://127.0.0.1:8080/connect/mcp/callback" || u == "http://localhost:8080/connect/mcp/callback" {
			loopback++
		}
	}
	if loopback != 2 {
		t.Fatalf("redirect URIs %v: a local install could not use this document", doc.RedirectURIs)
	}
}
