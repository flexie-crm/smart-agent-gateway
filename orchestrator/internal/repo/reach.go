package repo

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Proving a machine can be reached, from the one position that can prove it:
// the outside.
//
// A GPU server is installed by somebody sitting on it, and everything they can
// see says it works. The listener is up, `curl https://localhost:19443` answers,
// the service is enabled. None of that is the question. The question is whether
// a packet from the internet arrives, and the machine itself is the one place
// that cannot answer it: a firewall it cannot see, a security group nobody
// opened, a NAT with no forward, a cloud provider's own default deny. Every one
// of those looks like a perfect install right up to the moment somebody pastes
// the address into the console and the machine reads "off, with a reason".
//
// So the installer asks us, and we try to open a connection back. That is the
// whole idea, and it is worth stating what makes it honest: WE are not the party
// that has to reach the node in production, the gateway is. This proves the port
// is open to the public internet, which is necessary and is not sufficient. A
// deployment on a private network still has to check from where the gateway
// actually sits, and the console does that when the machine is added.
//
// # Why this cannot be turned into a port scanner
//
// The address is never taken from the request. It is the address the request
// CAME FROM, so the only host anybody can aim this at is the one they are
// already talking to us from. Someone who wants to scan a host has to first
// make that host send us an HTTP request, at which point they could have
// scanned it themselves. The port is theirs to choose and is the only input,
// bounded below to keep it off the privileged range.
//
// Private and loopback sources are refused as well. A caller inside our own
// network could otherwise use this to knock on machines that are not exposed at
// all, and the answer would be worthless to them anyway: this endpoint exists to
// report what the PUBLIC internet can see.

// probeTimeout is short on purpose. A port that is open answers in milliseconds
// and a port that is filtered never answers at all, so the only thing a longer
// wait buys is a slower "no" and a request held open while somebody stares at an
// installer.
const probeTimeout = 5 * time.Second

type reachResult struct {
	Address   string `json:"address"`
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	Reachable bool   `json:"reachable"`
	TLS       bool   `json:"tls"`
	Detail    string `json:"detail"`
}

// clientIP is the address the request came from, which behind the edge proxy is
// not RemoteAddr.
//
// The proxy appends the client to X-Forwarded-For, so with one proxy in front
// the client is the LAST entry, and the earlier ones are whatever the client
// chose to send us. Reading the first entry, which is the common mistake, would
// let a caller name any address it liked and have us probe it: the header is
// attacker-controlled up to the point our own proxy touched it.
//
// When the header is absent this is a direct connection and RemoteAddr is the
// truth.
func clientIP(r *http.Request, behindProxy bool) (string, error) {
	if behindProxy {
		if fwd := r.Header.Values("X-Forwarded-For"); len(fwd) > 0 {
			// Values() gives one string per header line; a single line may
			// still carry a comma-separated list.
			var last string
			for _, line := range fwd {
				for _, part := range splitComma(line) {
					if part != "" {
						last = part
					}
				}
			}
			if last != "" {
				if ip := net.ParseIP(last); ip != nil {
					return ip.String(), nil
				}
				return "", fmt.Errorf("the edge gave an address that is not one: %q", last)
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "", fmt.Errorf("no address on this request: %w", err)
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	return "", fmt.Errorf("no address on this request")
}

func splitComma(s string) []string {
	out := []string{}
	cur := ""
	for _, c := range s {
		switch c {
		case ',':
			out = append(out, trimSpace(cur))
			cur = ""
		default:
			cur += string(c)
		}
	}
	return append(out, trimSpace(cur))
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// routableSource refuses to probe anything that is not a public address.
//
// Both halves matter. A loopback or private source means the caller is not
// coming from the internet, so an answer about what the internet can see would
// be a lie; and it stops this from being pointed at machines inside a network
// that never exposed themselves.
func routableSource(ip net.IP) error {
	switch {
	case ip.IsLoopback():
		return fmt.Errorf("this request came from the machine serving it, so there is nothing to prove")
	case ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return fmt.Errorf("this request came from a private address (%s), which the internet cannot reach either way", ip)
	case ip.IsUnspecified(), ip.IsMulticast():
		return fmt.Errorf("that is not an address a machine answers on")
	}
	return nil
}

// handleIP answers with the address we saw the request come from.
//
// A machine behind NAT does not know the address that reaches it, and this is
// the only way it can find out: ask something outside.
func (s *Server) handleIP(w http.ResponseWriter, r *http.Request) {
	ip, err := clientIP(r, s.behindProxy)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if r.URL.Query().Get("plain") != "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, ip)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ip": ip})
}

// handleReachable opens a connection back to the caller and says what happened.
func (s *Server) handleReachable(w http.ResponseWriter, r *http.Request) {
	ip, err := clientIP(r, s.behindProxy)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that is not an address"})
		return
	}
	if err := routableSource(parsed); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"ip": ip, "error": err.Error()})
		return
	}

	port := 19443
	if raw := r.URL.Query().Get("port"); raw != "" {
		port, err = strconv.Atoi(raw)
		if err != nil || port < 1024 || port > 65535 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "port must be a number between 1024 and 65535",
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, s.probe(parsed, port))
}

// probe is the connection itself: TCP first, then a TLS hello.
//
// The two are reported separately because they fail for different reasons and
// need different advice. No TCP is a firewall, a security group or a missing
// port forward. TCP but no TLS means something is listening that is not the
// node, which is nearly always a different service already on that port.
//
// The certificate is deliberately NOT verified. It was issued by the
// deployment's own authority, which is not ours to trust from here, and
// verifying it is not what this answers: the question is whether packets
// arrive, and a handshake starting is proof of that. Trust is settled later, by
// the gateway, against the authority that issued it.
func (s *Server) probe(ip net.IP, port int) reachResult {
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	out := reachResult{Address: addr, IP: ip.String(), Port: port}

	conn, err := net.DialTimeout("tcp", addr, probeTimeout)
	if err != nil {
		out.Detail = "nothing accepted a connection on that port. The service may not be " +
			"running, or a firewall, security group or missing port forward is stopping it."
		return out
	}
	defer func() { _ = conn.Close() }()
	out.Reachable = true

	_ = conn.SetDeadline(time.Now().Add(probeTimeout))
	tlsConn := tls.Client(conn, &tls.Config{
		// See above: identity is not what is being asked here, arrival is.
		InsecureSkipVerify: true, //nolint:gosec
		ServerName:         ip.String(),
	})
	if err := tlsConn.Handshake(); err != nil {
		out.Detail = "something is listening on that port, but it did not answer as the " +
			"inference node does. Another service may already be using the port."
		return out
	}
	out.TLS = true
	out.Detail = "the inference node answered. This machine can be reached from the internet."
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
