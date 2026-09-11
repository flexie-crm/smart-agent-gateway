package sshtool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// questionHandler builds the callback that answers a server's sign-in questions,
// given the configuration and whether the password was already taken.
type questionHandler func(cfg Config, offered *passwordOffered) ssh.KeyboardInteractiveChallenge

// Signing in to a configured server.
//
// The connection is verified in both directions before any work reaches it: the
// server proves itself with the host key the administrator stored, and we prove
// ourselves with the sealed credential. A server whose key does not match is a
// refusal, never a warning, because the whole point of this tool is that a
// command lands on the machine it was meant for.

// dial opens a verified connection to the configured server. The TCP dial is
// bounded by the configured connect timeout; the handshake gets the same, plus
// room for a verification code when the server asks for one, since that waits on
// a person. An unreachable or silent host still fails in seconds.
func dial(ctx context.Context, cfg Config, challenge questionHandler, codeAllowance time.Duration) (*ssh.Client, error) {
	offered := &passwordOffered{}
	auth, err := authMethods(cfg, offered)
	if err != nil {
		return nil, err
	}
	if challenge != nil {
		// Offered after the configured credential, so it is only reached on a
		// server that wants something more, and knows by then whether the password
		// was taken directly.
		auth = append(auth, ssh.KeyboardInteractive(challenge(cfg, offered)))
	}
	verify, err := hostKeyCallback(cfg.HostKey)
	if err != nil {
		return nil, err
	}

	timeout := time.Duration(cfg.Limits.ConnectSeconds) * time.Second
	address := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	// Through the person's own computer when the tool says so, and straight out
	// otherwise. Everything after this line is identical either way, which is
	// the point: the host key is still the real server's, checked by its bytes
	// (hostKeyCallback ignores the address), and the session is encrypted end to
	// end. What passes through the chat application is ciphertext it cannot read.
	dialTCP := cfg.reach
	if dialTCP == nil {
		dialTCP = func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", address)
		}
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, timeout)
	conn, err := dialTCP(dialCtx)
	dialCancel()
	if err != nil {
		return nil, fmt.Errorf("the server could not be reached: %w", err)
	}
	// The handshake needs a deadline of its own, or a server that accepts the
	// connection and then says nothing would hold the goroutine for as long as it
	// liked. It allows for a verification code, which waits on a person.
	handshake := timeout
	if challenge != nil {
		handshake += codeAllowance
	}
	if err := conn.SetDeadline(time.Now().Add(handshake)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("the connection could not be prepared: %w", err)
	}

	client, chans, reqs, err := ssh.NewClientConn(conn, address, &ssh.ClientConfig{
		User:            cfg.Username,
		Auth:            auth,
		HostKeyCallback: verify,
		Timeout:         timeout,
	})
	if err != nil {
		_ = conn.Close()
		return nil, signInError(err)
	}
	// Work on this connection has its own deadlines; the handshake's must go.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("the connection could not be prepared: %w", err)
	}
	return ssh.NewClient(client, chans, reqs), nil
}

// authMethods builds the sign-in from whichever credentials are configured: a
// private key, a password, or both offered in turn. A key that cannot be read is
// an error rather than a quiet fall through to the password, because a tool
// configured with a key was meant to use it.
//
// The password is offered through a callback rather than as a fixed value so we
// learn whether the server ever asked for it. A method is only tried when the
// server has advertised it, so the callback running means the server takes
// passwords directly. That is what tells the sign-in, later, whether a question
// it is asked is the password (which this tool answers) or a second factor
// (which only a person can answer). See offered.
func authMethods(cfg Config, offered *passwordOffered) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if key := strings.TrimSpace(cfg.Auth.PrivateKey); key != "" {
		var signer ssh.Signer
		var err error
		if cfg.Auth.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(key), []byte(cfg.Auth.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(key))
		}
		if err != nil {
			return nil, fmt.Errorf("the private key could not be read: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if cfg.Auth.Password != "" {
		password := cfg.Auth.Password
		methods = append(methods, ssh.PasswordCallback(func() (string, error) {
			offered.taken()
			return password, nil
		}))
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("a password or a private key is required to sign in")
	}
	return methods, nil
}

// passwordOffered records whether the server took the configured password
// through the password method.
//
// It is the whole basis for deciding what a sign-in question is. A server that
// accepts passwords directly has already had ours by the time it asks anything
// else, so whatever it asks is a second factor and belongs to a person. A server
// that does not accept them directly never runs the callback, so a question it
// asks is where the password itself goes, and the tool answers it. Neither case
// reads a word of the question, which is what makes this reliable in a way
// matching on the wording never was.
type passwordOffered struct {
	mu     sync.Mutex
	taken_ bool
}

func (p *passwordOffered) taken() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.taken_ = true
}

func (p *passwordOffered) wasTaken() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.taken_
}

// hostKeyCallback verifies the server against the stored key. A blank one skips
// verification, which the form says plainly is weaker. A key that is given but
// cannot be read is an error rather than a quiet fall-through to trusting
// anything: an administrator who meant to verify gets told, not ignored.
func hostKeyCallback(known string) (ssh.HostKeyCallback, error) {
	known = strings.TrimSpace(known)
	if known == "" {
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // opt-in: the administrator left the host key blank
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(known))
	if err != nil {
		return nil, fmt.Errorf("the server's host key could not be read: %w", err)
	}
	want := parsed.Marshal()
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if string(key.Marshal()) != string(want) {
			return fmt.Errorf("the server presented a different host key than the one configured")
		}
		return nil
	}, nil
}

// signInError turns a handshake failure into something an administrator can act
// on. A server that wants a verification code is the one case worth naming,
// because the settings look correct and the connection still fails.
func signInError(err error) error {
	if errors.Is(err, errVerificationNeeded) || errors.Is(err, errCodeWindowClosed) {
		return err
	}
	return fmt.Errorf("the server refused the sign-in: %w", err)
}
