package datasource

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testKey generates an ed25519 SSH key pair for the auth and host-key tests: a
// PEM private key and the matching authorized_keys public line.
func testKey(t *testing.T) (pemKey string, authorizedLine string, pub ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	signer, err := ssh.NewSignerFromSigner(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return string(pem.EncodeToMemory(block)), string(ssh.MarshalAuthorizedKey(signer.PublicKey())), signer.PublicKey()
}

// The auth builder chooses methods from what is given, and refuses when nothing
// is: a tunnel with no credentials is a misconfiguration, not a silent
// anonymous connection.
func TestSSHAuth(t *testing.T) {
	pemKey, _, _ := testKey(t)

	if _, err := sshAuth(SSHConfig{}); err == nil {
		t.Fatal("a tunnel with no password or key should be refused")
	}
	if m, err := sshAuth(SSHConfig{Password: "pw"}); err != nil || len(m) != 1 {
		t.Fatalf("password auth: %v %d", err, len(m))
	}
	if m, err := sshAuth(SSHConfig{PrivateKey: pemKey}); err != nil || len(m) != 1 {
		t.Fatalf("key auth: %v %d", err, len(m))
	}
	if _, err := sshAuth(SSHConfig{PrivateKey: "not a key"}); err == nil {
		t.Fatal("a malformed private key should be refused")
	}
}

// Host-key verification is the tunnel's protection against a man in the middle
// on the bastion. When a key is configured it must be enforced exactly; when it
// is absent, verification is skipped (the documented, weaker opt-in), but a
// malformed configured key is an error, never a silent fall-through to skipping.
func TestHostKeyCallback(t *testing.T) {
	_, authorized, pub := testKey(t)
	_, _, otherPub := testKey(t)

	// Configured and matching: accepted.
	cb, err := hostKeyCallback(authorized)
	if err != nil {
		t.Fatalf("build callback: %v", err)
	}
	if err := cb("bastion:22", &net.TCPAddr{}, pub); err != nil {
		t.Fatalf("the matching host key was rejected: %v", err)
	}
	// Configured but a different key presented: rejected.
	if err := cb("bastion:22", &net.TCPAddr{}, otherPub); err == nil {
		t.Fatal("a mismatched host key was accepted")
	}
	// Absent: a callback that skips (no error), the documented weaker mode.
	if _, err := hostKeyCallback(""); err != nil {
		t.Fatalf("an empty host key should skip verification, not error: %v", err)
	}
	// Malformed: an error, not a silent skip.
	if _, err := hostKeyCallback("garbage not a key"); err == nil {
		t.Fatal("a malformed host key should be an error")
	}
}

func TestSSHSecretFields(t *testing.T) {
	secrets := map[string]bool{}
	for _, k := range SSHSecretFields() {
		secrets[k] = true
	}
	for _, want := range []string{"ssh.password", "ssh.private_key", "ssh.passphrase"} {
		if !secrets[want] {
			t.Fatalf("SSH secret field %q is not sealed", want)
		}
	}
	// The host and host_key are not secret: one is an address, the other a
	// public key.
	for _, f := range SSHFields() {
		if (f.Key == "ssh.host" || f.Key == "ssh.host_key") && f.Secret {
			t.Fatalf("%q must not be marked secret", f.Key)
		}
	}
}

// An SSH bastion, in process, that forwards what it is asked to.
//
// Written because the composition below cannot be proved without one: whether a
// jump host is itself reached through the chat application is a question about
// a real SSH handshake carried over a connection somebody else opened, and a
// fake would be answering it with itself.
type testBastion struct {
	addr    string
	hostKey string
}

func newTestBastion(t *testing.T, password string) *testBastion {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, given []byte) (*ssh.Permissions, error) {
			if string(given) != password {
				return nil, fmt.Errorf("no")
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			go serveBastion(raw, cfg)
		}
	}()

	line := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	return &testBastion{addr: listener.Addr().String(), hostKey: strings.TrimSpace(line)}
}

// serveBastion answers one SSH connection, forwarding every direct-tcpip
// channel to the address the client asked for. That is the whole of what a jump
// host does for a database tunnel.
func serveBastion(raw net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		_ = raw.Close()
		return
	}
	defer func() { _ = conn.Close() }()
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "direct-tcpip" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only forwarding")
			continue
		}
		// Every field named: ssh.Unmarshal fills them by reflection and cannot
		// write to a blank one. The last two are the origin, which a bastion is
		// told and does not need.
		var request struct {
			Host       string
			Port       uint32
			OriginHost string
			OriginPort uint32
		}
		if err := ssh.Unmarshal(newChannel.ExtraData(), &request); err != nil {
			_ = newChannel.Reject(ssh.ConnectionFailed, "unreadable")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go ssh.DiscardRequests(requests)
		go func() {
			defer func() { _ = channel.Close() }()
			far, err := net.Dial("tcp", net.JoinHostPort(request.Host, strconv.Itoa(int(request.Port))))
			if err != nil {
				return
			}
			defer func() { _ = far.Close() }()
			go func() { _, _ = io.Copy(far, channel) }()
			_, _ = io.Copy(channel, far)
		}()
	}
}

// newEchoServer is the "database": it sends back whatever it is sent.
func newEchoServer(t *testing.T) (host string, port int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	h, p, _ := net.SplitHostPort(listener.Addr().String())
	number, _ := strconv.Atoi(p)
	return h, number
}

// A bastion on somebody else's network is reached through the chat application,
// and the tunnel is then built inside that connection.
//
// The two settings compose: a jump host in an office is exactly as unreachable
// from a server as the database behind it, and a tool set to both used to pick
// one and ignore the other.
func TestABastionIsItselfReachedThroughTheChatApplication(t *testing.T) {
	bastion := newTestBastion(t, "secret")
	targetHost, targetPort := newEchoServer(t)

	// The chat application: the only thing in this test that can reach the
	// bastion, and it records what it was asked for.
	var asked struct {
		sync.Mutex
		host string
		port int
	}
	reach := Reach{
		Describe: "the chat application",
		Dial: func(ctx context.Context, host string, port int) (net.Conn, error) {
			asked.Lock()
			asked.host, asked.port = host, port
			asked.Unlock()
			return (&net.Dialer{}).DialContext(ctx, "tcp", bastion.addr)
		},
	}

	tun, localHost, localPort, err := openTunnel(SSHConfig{
		Host: "jump.office.invalid", Port: 2222, User: "someone",
		Password: "secret", HostKey: bastion.hostKey,
	}, targetHost, targetPort, &reach)
	if err != nil {
		t.Fatalf("open the tunnel through the application: %v", err)
	}
	defer func() { _ = tun.Close() }()

	// It signed in to the BASTION through the application, at the bastion's own
	// address: not the database, and not a local one.
	asked.Lock()
	gotHost, gotPort := asked.host, asked.port
	asked.Unlock()
	if gotHost != "jump.office.invalid" || gotPort != 2222 {
		t.Fatalf("the application was asked for %s:%d, which is not the bastion", gotHost, gotPort)
	}

	// And the whole chain carries bytes: driver -> local end -> application ->
	// bastion -> database.
	conn, err := net.Dial("tcp", net.JoinHostPort(localHost, strconv.Itoa(localPort)))
	if err != nil {
		t.Fatalf("dial the local end: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("select 1")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 8)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "select 1" {
		t.Fatalf("what came back through the chain was %q", buf)
	}
}

// The host key is still the bastion's own, checked on a connection somebody
// else carried. A hop that could present its own key would be a hop that could
// read the session.
func TestACarriedConnectionStillChecksTheBastionsHostKey(t *testing.T) {
	bastion := newTestBastion(t, "secret")
	targetHost, targetPort := newEchoServer(t)

	_, wrongKey, _ := testKey(t)
	reach := Reach{
		Describe: "the chat application",
		Dial: func(ctx context.Context, _ string, _ int) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", bastion.addr)
		},
	}
	_, _, _, err := openTunnel(SSHConfig{
		Host: "jump.office.invalid", Port: 2222, User: "someone",
		Password: "secret", HostKey: wrongKey,
	}, targetHost, targetPort, &reach)
	if err == nil {
		t.Fatal("a bastion presenting a different host key was accepted")
	}
	if !strings.Contains(err.Error(), "host key") {
		t.Fatalf("the refusal does not name the host key: %v", err)
	}
}
