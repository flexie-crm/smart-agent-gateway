package repo

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The address probed is never the caller's to choose.
//
// This is the property the whole endpoint rests on, so it is tested from the
// header the client controls rather than by calling clientIP with a string.
func TestClientIPTakesTheLastForwardedHop(t *testing.T) {
	cases := []struct {
		name    string
		header  []string
		remote  string
		want    string
		wantErr bool
	}{
		{
			// The client sent a forged chain; our edge appended the address it
			// actually saw. Reading the FIRST entry, which is the common
			// mistake, hands a caller any address it likes.
			name:   "a forged chain does not win",
			header: []string{"9.9.9.9, 203.0.113.7"},
			remote: "10.0.0.5:41000",
			want:   "203.0.113.7",
		},
		{
			name:   "several header lines still yield the last hop",
			header: []string{"9.9.9.9", "198.51.100.4"},
			remote: "10.0.0.5:41000",
			want:   "198.51.100.4",
		},
		{
			name:   "no header means a direct connection",
			header: nil,
			remote: "203.0.113.9:1234",
			want:   "203.0.113.9",
		},
		{
			name:    "a header that is not an address is refused, not guessed",
			header:  []string{"not-an-address"},
			remote:  "10.0.0.5:41000",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/ip", nil)
			r.RemoteAddr = tc.remote
			for _, h := range tc.header {
				r.Header.Add("X-Forwarded-For", h)
			}
			got, err := clientIP(r, true)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected a refusal, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Without a proxy in front, the header is ignored entirely: it is the client's
// to write, and there is no hop we trust to have corrected it.
func TestClientIPIgnoresTheHeaderWhenNotBehindAProxy(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/ip", nil)
	r.RemoteAddr = "203.0.113.9:1234"
	r.Header.Set("X-Forwarded-For", "9.9.9.9")
	got, err := clientIP(r, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "203.0.113.9" {
		t.Fatalf("got %q, want the connecting address", got)
	}
}

// A source we would not probe, and why each one is refused.
func TestRoutableSourceRefusesWhatIsNotPublic(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.9", "172.16.0.4", "169.254.1.1", "0.0.0.0"} {
		if err := routableSource(net.ParseIP(ip)); err == nil {
			t.Fatalf("%s should be refused", ip)
		}
	}
	for _, ip := range []string{"203.0.113.7", "8.8.8.8"} {
		if err := routableSource(net.ParseIP(ip)); err != nil {
			t.Fatalf("%s should be allowed: %v", ip, err)
		}
	}
}

// The endpoint refuses a private caller rather than probing it, which is what
// stops somebody inside a network using this to knock on machines that never
// exposed themselves.
func TestReachableRefusesAPrivateCaller(t *testing.T) {
	s := &Server{behindProxy: true}
	r := httptest.NewRequest(http.MethodGet, "/v1/reachable?port=19443", nil)
	r.Header.Set("X-Forwarded-For", "10.4.5.6")
	w := httptest.NewRecorder()
	s.handleReachable(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "private address") {
		t.Fatalf("the refusal should say why: %s", w.Body.String())
	}
}

func TestReachableRefusesAPortOutsideTheRange(t *testing.T) {
	for _, p := range []string{"22", "80", "1023", "70000", "-1", "nope"} {
		s := &Server{behindProxy: true}
		r := httptest.NewRequest(http.MethodGet, "/v1/reachable?port="+p, nil)
		r.Header.Set("X-Forwarded-For", "203.0.113.7")
		w := httptest.NewRecorder()
		s.handleReachable(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("port %s: got %d, want 400", p, w.Code)
		}
	}
}

// The probe against something real: a TLS listener, a plain TCP listener, and a
// closed port, which are the three answers it must tell apart.
func TestProbeTellsTheThreeCasesApart(t *testing.T) {
	s := &Server{}
	loopback := net.ParseIP("127.0.0.1")

	t.Run("a TLS listener is the node answering", func(t *testing.T) {
		ln := tlsListener(t)
		defer func() { _ = ln.Close() }()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() { _, _ = c.Write(nil); _ = c.Close() }()
			}
		}()
		got := s.probe(loopback, port(t, ln))
		if !got.Reachable || !got.TLS {
			t.Fatalf("expected reachable and TLS, got %+v", got)
		}
	})

	t.Run("plain TCP is something else on the port", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				// Answer as a plain HTTP server would, so the TLS handshake
				// fails on content rather than on silence.
				go func() { _, _ = c.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); _ = c.Close() }()
			}
		}()
		got := s.probe(loopback, port(t, ln))
		if !got.Reachable {
			t.Fatalf("TCP connected, so it is reachable: %+v", got)
		}
		if got.TLS {
			t.Fatalf("a plain listener is not the node: %+v", got)
		}
		if !strings.Contains(got.Detail, "Another service") {
			t.Fatalf("the detail should name the likely cause: %q", got.Detail)
		}
	})

	t.Run("a closed port is not reachable", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := port(t, ln)
		_ = ln.Close() // nothing is listening now
		got := s.probe(loopback, p)
		if got.Reachable || got.TLS {
			t.Fatalf("expected unreachable, got %+v", got)
		}
		if !strings.Contains(got.Detail, "firewall") {
			t.Fatalf("the detail should name the likely causes: %q", got.Detail)
		}
	})
}

// /v1/ip is what a machine behind NAT uses to learn its own public address.
func TestIPAnswersWhatWeSaw(t *testing.T) {
	s := &Server{behindProxy: true}
	r := httptest.NewRequest(http.MethodGet, "/v1/ip", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	w := httptest.NewRecorder()
	s.handleIP(w, r)

	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["ip"] != "203.0.113.7" {
		t.Fatalf("got %v", got)
	}
}

func port(t *testing.T, ln net.Listener) int {
	t.Helper()
	_, p, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A listener with a certificate nobody trusts, which is exactly the case in
// production: the node's certificate comes from the deployment's own authority,
// and the probe deliberately does not verify it. Minted here rather than
// checked in, so no test starts failing on an expiry date.
func tlsListener(t *testing.T) net.Listener {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-node"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ln
}
