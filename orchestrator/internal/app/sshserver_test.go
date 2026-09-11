package app_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"strconv"
	"testing"

	"golang.org/x/crypto/ssh"
)

// A real SSH server for the app tests.
//
// A custom tool is not stored until it has been proved to connect, so a test of
// creating one needs somewhere for it to connect to. This is the smallest server
// that satisfies that: it takes one password and opens a session, which is
// exactly what the tool's own check asks of it.

type sshTestServer struct {
	Host     string
	Port     int
	HostKey  string
	Password string
}

func startSSHServer(t *testing.T) *sshTestServer {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}

	const password = "hunter2"
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, given []byte) (*ssh.Permissions, error) {
			if string(given) == password {
				return nil, nil
			}
			return nil, fmt.Errorf("password rejected")
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
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveSSH(conn, cfg)
		}
	}()

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("listener address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("listener port: %v", err)
	}
	return &sshTestServer{
		Host:     host,
		Port:     port,
		HostKey:  string(ssh.MarshalAuthorizedKey(signer.PublicKey())),
		Password: password,
	}
}

// serveSSH answers one connection: the handshake, then any session channel that
// is asked for. Nothing runs on it; opening it is the whole question.
func serveSSH(conn net.Conn, cfg *ssh.ServerConfig) {
	defer func() { _ = conn.Close() }()

	serverConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = serverConn.Close() }()
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			return
		}
		go serveChannel(channel, requests)
	}
}

// serveChannel answers what is asked on one channel. A server that accepted a
// channel and then never replied would leave the client waiting, which is not
// what a real one does.
func serveChannel(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer func() { _ = channel.Close() }()
	for request := range requests {
		switch request.Type {
		case "exec":
			_ = request.Reply(true, nil)
			_, _ = channel.Write([]byte("ok\n"))
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			return
		default:
			_ = request.Reply(false, nil)
		}
	}
}
