package main

// A one-off: mint a client certificate from this installation's authority and
// call a machine with it, so what the node actually returns can be READ instead
// of inferred from a JSON parse error.
import (
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"flexie.io/sag/internal/crypto"
	"flexie.io/sag/internal/nodeca"
)

func main() {
	if len(os.Args) < 5 {
		fmt.Fprintln(os.Stderr, "usage: probe <kek-hex> <key-enc-hex> <cert-pem-file> <url> [body]")
		os.Exit(2)
	}
	ring, err := crypto.ParseKeyring("1:"+os.Args[1], "1")
	must(err)
	sealed, err := hex.DecodeString(os.Args[2])
	must(err)
	keyPEM, err := ring.Open(sealed)
	must(err)
	certPEM, err := os.ReadFile(os.Args[3])
	must(err)

	ca, err := nodeca.Load(keyPEM, certPEM)
	must(err)
	ours, err := ca.IssueClient("sag-orchestrator", time.Now())
	must(err)

	client := &http.Client{
		Timeout: 300 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			Certificates:       []tls.Certificate{ours},
			InsecureSkipVerify: true, //nolint:gosec // we are reading what it says, not trusting it
			MinVersion:         tls.VersionTLS13,
		}},
	}

	var req *http.Request
	if len(os.Args) > 5 {
		req, err = http.NewRequest(http.MethodPost, os.Args[4], strings.NewReader(os.Args[5]))
		must(err)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, err = http.NewRequest(http.MethodGet, os.Args[4], nil)
		must(err)
	}
	if key := os.Getenv("NODE_KEY"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	must(err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4000))
	fmt.Printf("HTTP %d %s\n", resp.StatusCode, resp.Header.Get("Content-Type"))
	fmt.Printf("--- body ---\n%s\n", body)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}
