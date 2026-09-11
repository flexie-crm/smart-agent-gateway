package toolkit

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// A hardened HTTP client for tools that fetch a URL the model chose.
//
// The pre-flight CheckURL is not enough on its own: between resolving a name
// and dialling it, the answer can change (DNS rebinding), and a redirect can
// point somewhere new. So the real guard is here, at the socket: the dialer's
// Control hook runs for every address actually dialled, including each redirect
// target, and refuses any that is not a normal public address. Because the
// dialer still does its own name resolution and TLS still verifies the
// hostname, this closes the rebinding window without weakening TLS.

// SafeHTTPClient builds an HTTP client that refuses to connect to private or
// reserved addresses, follows a bounded number of redirects (re-checking each),
// and stops after the given total timeout.
func SafeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 10 * time.Second,
		// Control runs after the dialer has resolved the name, with the concrete
		// ip:port about to be connected. Refusing here catches a name that
		// resolved to a private address and a redirect that aimed inward alike.
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("blocked: unparseable dial address")
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("blocked: dial address is not an IP")
			}
			if !isPublic(ip) {
				return fmt.Errorf("blocked: %s is in a private or reserved range", ip)
			}
			return nil
		},
	}

	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("stopped after 3 redirects")
			}
			// The socket Control hook checks the IP; this checks the URL shape
			// (scheme, a host that is not a literal private range) before the
			// redirect is followed at all.
			if res := CheckURL(req.Context(), req.URL.String()); !res.OK {
				return fmt.Errorf("redirect blocked: %s", res.Reason)
			}
			return nil
		},
	}
}
