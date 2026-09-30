package apitool

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
)

// REACHING AN API THE GATEWAY CANNOT, through the computer of whoever is using
// the tool.
//
// The same one checkbox the database and server tools offer, for the same
// reason and over the same mechanism (KB/39): plenty of APIs exist only on an
// office network, and the chat application is installed on a machine that can
// see them and is already connected to us.
//
// A CLIENT PER CALL, where every other call in this process shares one. That is
// deliberate rather than an oversight: a pooled connection is keyed by host and
// port, and through here two people's computers answer for the same host name
// while being different machines entirely. Reusing one would hand somebody
// else's network to whoever asked second. The cost is a connection per call,
// which for an API on a local network is nothing.
func reachClient(machines template.Machines, call tool.Call, timeout time.Duration) *http.Client {
	dial := func(ctx context.Context, _, address string) (net.Conn, error) {
		host, port, err := splitHostPort(address)
		if err != nil {
			return nil, err
		}
		return machines.Dial(ctx, call.WorkspaceID, call.UserID, call.DeviceID, host, port,
			"a request to "+host)
	}
	return &http.Client{
		// TLS is untouched: the transport still does the handshake itself, with
		// the address's own host name, so a certificate is verified exactly as
		// it is on a direct call. All that changes is who opens the socket.
		Transport: &http.Transport{
			DialContext:           dial,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableKeepAlives:     true,
		},
		Timeout:       timeout,
		CheckRedirect: checkRedirect,
	}
}

// splitHostPort takes the address the transport is about to dial.
//
// It arrives with a port always, because net/http fills in 80 or 443 from the
// scheme before it dials, and the far end needs a number rather than a scheme.
func splitHostPort(address string) (string, int, error) {
	host, text, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, fmt.Errorf("the address to dial could not be read: %w", err)
	}
	port, err := strconv.Atoi(text)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("%q is not a port", text)
	}
	return strings.TrimSpace(host), port, nil
}
