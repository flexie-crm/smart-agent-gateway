package apispec

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"flexie.io/sag/internal/useragent"
)

// mostSpec bounds what will be read. Real specifications are large (a CRM's is
// 273 KB, a payment processor's several megabytes), so this is generous; it is
// here so that an address that answers with something endless cannot take the
// server with it.
const mostSpec = 16 << 20

// fetchTimeout bounds the whole fetch. A document this size is one request over
// a normal connection, and something that has not answered in half a minute is
// not going to.
const fetchTimeout = 30 * time.Second

// Fetch reads a specification from where it is published.
//
// Most specifications are hosted: pasting an address is what an administrator
// actually has, and downloading a file to upload it again is a step for nothing.
//
// There is deliberately NO SSRF guard here, which is a decision rather than an
// omission. That guard exists for http_request, where the address comes from
// the MODEL and must not be trusted. This address comes from an administrator
// who can already point a tool at any host, and whose own specification may
// well be published inside their network; refusing a private address would
// block the legitimate case while stopping nobody, since the same person
// configures the tool's base address unchecked. The same reasoning the MCP
// client's discovery fetch already follows.
//
// What IS bounded: the size, the time, and how far it will be redirected.
func Fetch(ctx context.Context, client *http.Client, address string) ([]byte, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, fmt.Errorf("the address of the specification is required")
	}
	parsed, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("that is not a valid address: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("the address must begin with http:// or https://")
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("the address has no host in it")
	}
	if client == nil {
		client = &http.Client{}
	}

	runCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(runCtx, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("the request could not be built: %w", err)
	}
	useragent.Set(req.Header)
	// Asked for by name, because a service that publishes both formats decides
	// by this, and a bare request often answers with a documentation PAGE.
	req.Header.Set("Accept", "application/json, application/yaml, text/yaml, text/plain;q=0.8, */*;q=0.5")

	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("that address could not be reached: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("that address answered %d, so there is no specification there", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, mostSpec+1))
	if err != nil {
		return nil, fmt.Errorf("the specification could not be read in full: %w", err)
	}
	if len(raw) > mostSpec {
		return nil, fmt.Errorf("that specification is larger than %d MB, which is more than this will read", mostSpec>>20)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, fmt.Errorf("that address answered with nothing")
	}
	// An HTML page is the usual mistake: somebody pastes the address of the
	// documentation rather than of the document behind it. Saying so is more
	// use than a parse error about an unexpected character.
	if looksLikeHTML(raw) {
		return nil, fmt.Errorf("that address answered with a web page rather than a specification. " +
			"Use the address of the JSON or YAML document itself, which a documentation page usually links to")
	}

	return raw, nil
}

func looksLikeHTML(raw []byte) bool {
	head := strings.ToLower(strings.TrimSpace(string(raw[:min(len(raw), 512)])))
	return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html")
}

// NOTE ON THE HOST, and why nothing here guesses one.
//
// It is tempting to complete a missing host from where the document was found:
// a 2.0 specification usually states basePath and schemes and no host, and one
// fetched from https://example.com/api.json looks like it belongs to
// example.com.
//
// It does not. A specification is published where the VENDOR publishes it,
// which is very often not where any customer's API runs: the CRM this was
// tested against serves one document from its marketing site while every
// installation answers on its own host. Completing it would have configured
// every tool with an address no call of theirs should go to, and the tool would
// have looked correctly set up while being wrong.
//
// So the PATH is kept (Spec.BasePath), which the document really does state,
// and the host is left to the administrator, who is the only one who knows it.
