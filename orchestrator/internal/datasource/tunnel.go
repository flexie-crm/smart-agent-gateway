package datasource

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Reaching a database through an SSH bastion.
//
// Many databases are not exposed to the internet: you reach them by first
// connecting to a jump host over SSH and dialling the database from there. The
// tunnel here does exactly that, and it is driver-agnostic: it opens a local
// forwarder (a listener on 127.0.0.1) that carries every connection through the
// SSH bastion to the real database, and hands back the local address. A driver
// then connects to that local address knowing nothing about SSH. So one tunnel
// implementation serves every driver, present and future.

// SSHConfig is how to reach the bastion. Auth is a password or a private key
// (both sealed at rest). HostKey, when given, is the bastion's public key in
// authorized_keys form and is verified on connect; when empty, the host key is
// not verified, which is weaker and should be filled in for anything sensitive.
type SSHConfig struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	HostKey    string `json:"host_key,omitempty"`
}

// SSHFields are the connection settings a tunnel needs, for the admin form. They
// are common to every driver (the tunnel wraps the dial, whatever the database),
// so they live here rather than in a driver.
func SSHFields() []Field {
	return []Field{
		// The form flows top to bottom: where the bastion is (host, port), who
		// connects (user), then how to authenticate (a password, or a private key
		// with its passphrase), then how to trust the bastion (its host key).
		{Key: "ssh.host", Label: "SSH host", Type: FieldText, Span: 4, Help: "The bastion / jump host to tunnel through. Leave blank for a direct connection."},
		{Key: "ssh.port", Label: "SSH port", Type: FieldNumber, Default: "22", Span: 2},
		{Key: "ssh.user", Label: "SSH user", Type: FieldText, Span: 3},
		{Key: "ssh.password", Label: "SSH password", Type: FieldPassword, Secret: true, Span: 3, Help: "Use this or a private key."},
		{Key: "ssh.private_key", Label: "SSH private key", Type: FieldTextarea, Secret: true, Help: "PEM private key for the SSH user."},
		{Key: "ssh.passphrase", Label: "Key passphrase", Type: FieldPassword, Secret: true, Span: 6, Help: "Only if the private key is encrypted."},
		{Key: "ssh.host_key", Label: "Known host key", Type: FieldTextarea, Help: "The bastion's public key (authorized_keys form). Leave blank to skip host verification (less secure)."},
	}
}

// SSHSecretFields names the sealed SSH keys, so the app encrypts them at rest
// alongside a driver's own secrets.
func SSHSecretFields() []string {
	return []string{"ssh.password", "ssh.private_key", "ssh.passphrase"}
}

// tunnel is a live SSH connection with a local forwarder in front of it. The
// forwarder is the generic half (one listener, one dial function, a pipe per
// connection); what is SSH about it is the dialler and closing the client.
type tunnel struct {
	*forwarder
}

// openTunnel dials the bastion and starts forwarding a local address to
// remoteHost:remotePort on its far side. It returns the tunnel (to close) and
// the local host and port a driver should connect to instead.
//
// reach, when given, is how the BASTION itself is reached. The two settings
// compose rather than exclude: a jump host on somebody's office network is as
// unreachable from here as the database behind it, so the SSH connection is
// carried through the chat application and the tunnel is then built inside it.
// Everything SSH about it is unchanged, host key included, because what the
// reach replaces is one TCP dial and nothing else.
func openTunnel(cfg SSHConfig, remoteHost string, remotePort int, reach *Reach) (*tunnel, string, int, error) {
	auth, err := sshAuth(cfg)
	if err != nil {
		return nil, "", 0, err
	}
	hostKey, err := hostKeyCallback(cfg.HostKey)
	if err != nil {
		return nil, "", 0, err
	}

	port := cfg.Port
	if port == 0 {
		port = 22
	}
	address := net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	sshConfig := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auth,
		HostKeyCallback: hostKey,
		Timeout:         10 * time.Second,
	}

	var client *ssh.Client
	if reach == nil {
		client, err = ssh.Dial("tcp", address, sshConfig)
	} else {
		client, err = dialSSHThrough(*reach, cfg.Host, port, address, sshConfig)
	}
	if err != nil {
		return nil, "", 0, fmt.Errorf("connect to the SSH host: %w", err)
	}

	remote := net.JoinHostPort(remoteHost, strconv.Itoa(remotePort))

	// Probing is not optional here, and the reason is worth keeping. Forwarding
	// happens per connection in a goroutine with no caller, so a bastion that
	// signs us in and then refuses to forward (AllowTcpForwarding no, or a
	// PermitOpen that does not cover this address) produced a closed local
	// socket and nothing else. What the administrator saw was the driver saying
	// "connection reset by peer", which describes the symptom and names neither
	// the bastion nor the reason. Now they get the reason, on Test, before the
	// tool is ever used.
	dial := func(context.Context) (net.Conn, error) {
		conn, err := client.Dial("tcp", remote)
		if err != nil {
			return nil, fmt.Errorf("the SSH host would not open a connection to %s: %w", remote, err)
		}
		return conn, nil
	}

	f, host, port, err := forward(dial, true, client.Close)
	if err != nil {
		_ = client.Close()
		return nil, "", 0, err
	}
	return &tunnel{forwarder: f}, host, port, nil
}

// sshAuth builds the auth methods from the config: a private key if given
// (honouring a passphrase), a password if given, or an error if neither.
func sshAuth(cfg SSHConfig) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if key := strings.TrimSpace(cfg.PrivateKey); key != "" {
		var signer ssh.Signer
		var err error
		if cfg.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(key), []byte(cfg.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(key))
		}
		if err != nil {
			return nil, fmt.Errorf("the SSH private key could not be read: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if cfg.Password != "" {
		methods = append(methods, ssh.Password(cfg.Password))
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("the SSH connection needs a password or a private key")
	}
	return methods, nil
}

// hostKeyCallback verifies the bastion's key against the configured one. An
// empty configuration skips verification (documented as less secure); a
// malformed key is an error rather than a silent fall-through to skipping.
func hostKeyCallback(known string) (ssh.HostKeyCallback, error) {
	known = strings.TrimSpace(known)
	if known == "" {
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // opt-in: the admin left the known host key blank
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(known))
	if err != nil {
		return nil, fmt.Errorf("the known host key could not be read: %w", err)
	}
	want := parsed.Marshal()
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if string(key.Marshal()) != string(want) {
			return fmt.Errorf("the SSH host key does not match the configured one")
		}
		return nil
	}, nil
}

// dialSSHThrough signs in to a bastion over a connection somebody else carried.
//
// ssh.Dial is ssh.NewClientConn plus a net.Dial, and only the dial is being
// replaced: the handshake, the host key check and the encryption are the same
// code on the same bytes. The address is still the bastion's own, because that
// is what the host key is checked against.
func dialSSHThrough(reach Reach, host string, port int, address string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	conn, err := reach.Dial(ctx, host, port)
	if err != nil {
		return nil, fmt.Errorf("%s could not open a connection to %s: %w", reach.Describe, address, err)
	}
	if err := conn.SetDeadline(time.Now().Add(cfg.Timeout)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	client, chans, reqs, err := ssh.NewClientConn(conn, address, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	// The handshake is done; work on this connection has its own deadlines.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = client.Close()
		return nil, err
	}
	return ssh.NewClient(client, chans, reqs), nil
}
