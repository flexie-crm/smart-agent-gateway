package toolkit

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// The SSRF guard. A tool that fetches a URL the model chose is a way to reach
// whatever that URL points at, including addresses that only exist inside the
// network this server runs in: the metadata endpoint that hands out cloud
// credentials, a database admin port, a neighbouring service with no auth. The
// guard refuses those. It resolves the host and checks every address it
// resolves to, so a public hostname pointing at a private IP is blocked too,
// and it is re-run on every redirect target because a public URL can redirect
// inward.

// SSRFResult is the outcome of a check: ok, or the host/ip that failed it.
type SSRFResult struct {
	OK   bool
	Host string
	IP   string
	// Reason is model-safe text explaining a refusal.
	Reason string
}

// CheckURL validates a URL before it is fetched. It allows only http and https,
// rejects a URL with no host, resolves the host, and refuses if any resolved
// address is loopback, private, link-local, or otherwise not a normal public
// address. DNS is resolved here (not left to the HTTP client) so the decision
// is made on the addresses that will actually be dialled; the context bounds
// that lookup.
func CheckURL(ctx context.Context, raw string) SSRFResult {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return SSRFResult{Reason: "the address could not be understood"}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return SSRFResult{Reason: "only http and https addresses are allowed"}
	}
	host := u.Hostname()
	if host == "" {
		return SSRFResult{Reason: "the address has no host"}
	}

	// A literal IP is checked directly; a name is resolved and every address it
	// answers with is checked, so one private answer among several is a refusal.
	var ips []net.IP
	if literal := net.ParseIP(host); literal != nil {
		ips = []net.IP{literal}
	} else {
		resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(resolved) == 0 {
			return SSRFResult{Host: host, Reason: "the host could not be resolved"}
		}
		for _, addr := range resolved {
			ips = append(ips, addr.IP)
		}
	}

	for _, ip := range ips {
		if !isPublic(ip) {
			return SSRFResult{
				Host:   host,
				IP:     ip.String(),
				Reason: "the target is in a private or reserved range and cannot be reached from here",
			}
		}
	}
	return SSRFResult{OK: true, Host: host}
}

// isPublic reports whether an address is a normal, routable public one: not
// loopback, not private (RFC1918 / ULA), not link-local (which includes the
// 169.254.169.254 cloud-metadata address), not multicast, not unspecified.
func isPublic(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() ||
		ip.IsInterfaceLocalMulticast() {
		return false
	}
	// Explicitly refuse a few reserved ranges Go's helpers do not cover: the
	// IPv4 "this network" 0.0.0.0/8, the shared CGNAT range 100.64.0.0/10, and
	// benchmarking 198.18.0.0/15.
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0:
			return false
		case v4[0] == 100 && v4[1]&0xc0 == 64:
			return false
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19):
			return false
		}
	}
	return true
}

// BlockedError renders a refusal as model-safe text, with the offending host or
// ip when there is one.
func (r SSRFResult) BlockedError() string {
	detail := r.IP
	if detail == "" {
		detail = r.Host
	}
	if detail == "" {
		return "Request blocked: " + r.Reason + "."
	}
	return fmt.Sprintf("Request blocked: %s (%s).", r.Reason, detail)
}
