package sshtool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"flexie.io/sag/internal/cmdpolicy"
)

// serverConfig is the stored config for a live test server, with whichever
// credential the test wants to sign in with.
func serverConfig(srv *testServer, auth Auth) Config {
	return Config{
		Driver:   VariantSSH,
		Host:     srv.Host,
		Port:     srv.Port,
		Username: "deploy",
		Auth:     auth,
		HostKey:  srv.HostKey,
		Limits:   Limits{}.withDefaults(),
		Policy:   cmdpolicy.Policy{Mode: cmdpolicy.PolicyAllowlist, Allowed: "uptime", Sudo: cmdpolicy.SudoDeny},
	}
}

func TestDialWithAPassword(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})

	client, err := dial(context.Background(), serverConfig(srv, Auth{Password: "hunter2"}), nil, 0)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("open a channel: %v", err)
	}
	_ = session.Close()
}

func TestDialWithAKey(t *testing.T) {
	private, public := clientKey(t)
	srv := startServer(t, serverOptions{AuthorizedKey: public})

	client, err := dial(context.Background(), serverConfig(srv, Auth{PrivateKey: private}), nil, 0)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = client.Close()
}

func TestDialRefusesAWrongPassword(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})

	_, err := dial(context.Background(), serverConfig(srv, Auth{Password: "wrong"}), nil, 0)
	if err == nil {
		t.Fatal("a wrong password was accepted")
	}
	if !strings.Contains(err.Error(), "refused the sign-in") {
		t.Fatalf("got %q, want a sign-in refusal", err)
	}
}

// The host key is the whole reason a command can be trusted to land on the
// machine it was meant for. A server presenting a different one is refused, not
// warned about.
func TestDialRefusesAnUnknownHostKey(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	other := startServer(t, serverOptions{Password: "hunter2"})

	cfg := serverConfig(srv, Auth{Password: "hunter2"})
	cfg.HostKey = other.HostKey

	_, err := dial(context.Background(), cfg, nil, 0)
	if err == nil {
		t.Fatal("a server presenting a different host key was accepted")
	}
	if !strings.Contains(err.Error(), "different host key") {
		t.Fatalf("got %q, want a host key refusal", err)
	}
}

func TestDialRefusesAnUnreadableHostKey(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	cfg := serverConfig(srv, Auth{Password: "hunter2"})
	cfg.HostKey = "not a key"

	_, err := dial(context.Background(), cfg, nil, 0)
	if err == nil {
		t.Fatal("an unreadable host key was treated as trust")
	}
	if !strings.Contains(err.Error(), "host key could not be read") {
		t.Fatalf("got %q, want an unreadable-key error", err)
	}
}

func TestDialRefusesAnUnreadablePrivateKey(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})

	_, err := dial(context.Background(), serverConfig(srv, Auth{PrivateKey: "not a key"}), nil, 0)
	if err == nil {
		t.Fatal("an unreadable private key was accepted")
	}
	if !strings.Contains(err.Error(), "private key could not be read") {
		t.Fatalf("got %q, want an unreadable-key error", err)
	}
}

// An address that accepts the connection and then says nothing must not hold the
// caller: the handshake carries the same budget as the dial.
func TestDialGivesUpOnASilentServer(t *testing.T) {
	silent := startSilentListener(t)

	cfg := Config{
		Driver: VariantSSH, Host: silent.host, Port: silent.port, Username: "deploy",
		Auth:    Auth{Password: "hunter2"},
		HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample",
		Limits:  Limits{MaxChannels: 1, IdleMinutes: 1, ConnectSeconds: 3},
	}

	start := time.Now()
	if _, err := dial(context.Background(), cfg, nil, 0); err == nil {
		t.Fatal("a silent server was treated as connected")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the dial hung for %s; the handshake deadline did not apply", elapsed)
	}
}

// Test is what an administrator presses before saving, so it must fail on a
// configuration that cannot connect and pass on one that can.
func TestTemplateTestChecksTheRealConnection(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	cfg := serverConfig(srv, Auth{Password: "hunter2"})

	good, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := (sshTemplate{}).Test(context.Background(), good); err != nil {
		t.Fatalf("a working connection failed its test: %v", err)
	}

	cfg.Auth.Password = "wrong"
	bad, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := (sshTemplate{}).Test(context.Background(), bad); err == nil {
		t.Fatal("a broken connection passed its test")
	}
}

// The host key follows the bastion settings the database tools collect: given,
// it is verified; blank, it is not, which the form says plainly is weaker. What
// must not happen is a key that is given being quietly ignored.
func TestDialSkipsVerificationWhenNoHostKeyIsConfigured(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	cfg := serverConfig(srv, Auth{Password: "hunter2"})
	cfg.HostKey = ""

	client, err := dial(context.Background(), cfg, nil, 0)
	if err != nil {
		t.Fatalf("a server was refused although no host key was configured: %v", err)
	}
	_ = client.Close()
}
