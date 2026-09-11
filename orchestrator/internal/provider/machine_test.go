package provider

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/nodeca"
)

// Inference to a machine we run ourselves.
//
// Everything else the gateway calls is a public endpoint reached with an API
// key. A machine is not: it is our hardware, possibly on a public address, and
// the connection to it is authenticated at BOTH ends with certificates from the
// deployment's own authority (KB/35). That difference lives in one branch of
// buildProvider, and these are what say it is still there.

// aMachine stands up something that behaves like one: it serves with a
// certificate from an authority and refuses anybody who cannot present one back.
func aMachine(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *nodeca.Authority) {
	t.Helper()
	authority, err := nodeca.Create(time.Now())
	if err != nil {
		t.Fatalf("authority: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: "nd-1"}}, key)
	if err != nil {
		t.Fatalf("certificate request: %v", err)
	}
	certPEM, err := authority.Issue(csr, "nd-1", []string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	identity, err := tls.X509KeyPair(certPEM,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	server := httptest.NewUnstartedServer(handler)
	server.TLS = authority.ServerTLS(identity)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, authority
}

func TestInferenceToAMachineIsMutuallyAuthenticated(t *testing.T) {
	var caller string
	server, authority := aMachine(t, func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) > 0 {
			caller = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"a-model","object":"model"}]}`))
	})

	ours, err := authority.IssueClient("sag-orchestrator", time.Now())
	if err != nil {
		t.Fatalf("our identity: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: authority.ClientTLS(ours, ""),
	}}

	g := NewGateway(nil, nil)
	g.UseMachineClient(func(int64) (*http.Client, error) { return client, nil })

	vendor := &model.AIVendor{
		VendorKey: model.VendorOpenAICompatible,
		BaseURL:   server.URL + "/v1",
		NodeID:    7,
	}
	p, err := g.buildProvider(vendor, "a-key")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("the machine could not be called: %v", err)
	}
	if len(models) != 1 || models[0].ID != "a-model" {
		t.Fatalf("got %+v", models)
	}
	// The machine knew who called, which is the half that stops anything else on
	// the network driving our hardware.
	if caller != "sag-orchestrator" {
		t.Errorf("the machine saw the caller as %q", caller)
	}
}

func TestAMachineRefusesAnOrdinaryClient(t *testing.T) {
	// The other half of the same claim: without our certificate the connection
	// does not happen at all, so this is not a check somebody can forget to make.
	server, _ := aMachine(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	})

	g := NewGateway(nil, nil)
	g.UseMachineClient(func(int64) (*http.Client, error) {
		// Trusts the machine, presents nothing. An eavesdropper with the address
		// and the vendor key has exactly this.
		return &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}, //nolint:gosec // the point of the test
		}}, nil
	})

	p, err := g.buildProvider(&model.AIVendor{
		VendorKey: model.VendorOpenAICompatible,
		BaseURL:   server.URL + "/v1",
		NodeID:    7,
	}, "a-key")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := p.ListModels(context.Background()); err == nil {
		t.Fatal("a caller with no certificate was served")
	}
}

func TestAMachineVendorIsNotCallableWithoutTheWayToReachOne(t *testing.T) {
	// A wiring mistake on our side, not a misconfigured vendor. Saying so here
	// beats failing later as a connection error nobody can explain.
	g := NewGateway(nil, nil)
	_, err := g.buildProvider(&model.AIVendor{
		VendorKey: model.VendorOpenAICompatible,
		BaseURL:   "https://10.0.0.9:8081/v1",
		NodeID:    7,
	}, "a-key")
	if !errors.Is(err, ErrMachineUnreachable) {
		t.Fatalf("err = %v, want ErrMachineUnreachable", err)
	}
}

func TestAHostedVendorIsUntouchedByAnyOfThis(t *testing.T) {
	// Most vendors are a public endpoint and an API key, and they must not start
	// needing a certificate because machines do.
	g := NewGateway(nil, nil)
	g.UseMachineClient(func(int64) (*http.Client, error) {
		t.Error("a hosted vendor asked for the machine client")
		return nil, nil
	})
	if _, err := g.buildProvider(&model.AIVendor{
		VendorKey: model.VendorOpenAI,
	}, "a-key"); err != nil {
		t.Fatalf("a hosted vendor could not be built: %v", err)
	}
}

// The gateway must be told WHICH machine, because they are not all reached the
// same way.
//
// A machine that joined chains to our authority under the fleet name. One added
// by hand signs its own certificate and is verified by holding those exact
// bytes. The callback took no argument at all, so every machine got the fleet
// client, and an added-by-hand machine answered the control plane perfectly
// (which always knew the id) and then failed every inference call with
// "certificate is valid for <its own hostname>, not machine.sag.internal".
func TestTheGatewaySaysWhichMachineItIsReaching(t *testing.T) {
	var asked []int64
	g := NewGateway(nil, nil)
	g.UseMachineClient(func(nodeID int64) (*http.Client, error) {
		asked = append(asked, nodeID)
		return &http.Client{}, nil
	})

	// A machine of ours: the id has to arrive.
	if _, err := g.buildProvider(&model.AIVendor{
		VendorKey: model.VendorOpenAICompatible,
		BaseURL:   "https://machine.example/v1",
		NodeID:    42,
	}, "k"); err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(asked) != 1 || asked[0] != 42 {
		t.Fatalf("the gateway asked for %v, want the machine it is calling (42)", asked)
	}

	// A hosted vendor never reaches this at all.
	if _, err := g.buildProvider(&model.AIVendor{
		VendorKey: model.VendorOpenAICompatible,
		BaseURL:   "https://api.example/v1",
	}, "k"); err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(asked) != 1 {
		t.Fatalf("a hosted vendor asked for a machine client: %v", asked)
	}
}
