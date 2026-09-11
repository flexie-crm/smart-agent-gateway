package sshtool

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// A real SSH server, inside the test process.
//
// What this tool does is a verified handshake followed by work on a channel, and
// neither can be proven against a stub: the tests dial this server exactly the
// way the tool dials a customer's, so a mistake in host verification, in the
// credential, or in opening a channel fails here rather than in production.

type testServer struct {
	Host string
	Port int
	// HostKey is the server's public key in authorized_keys form: what an
	// administrator would paste into the form.
	HostKey string
	// ClientKey is the PEM private key this server accepts, generated for it
	// whenever it asks a question at sign-in: a server with a second factor still
	// has to authenticate the login itself, which in practice is a key.
	ClientKey string
}

type serverOptions struct {
	// Password, when set, is the only password the server accepts.
	Password string
	// AuthorizedKey, when set, is the only public key the server accepts.
	AuthorizedKey ssh.PublicKey
	// Run answers a command. Left nil, the server echoes the command back and
	// exits cleanly, which is enough for a test that only cares that it arrived.
	Run func(command string) commandRun

	// Code, when set, makes the server ask for a verification code at sign-in and
	// accept only this one, the way a server with a second factor does.
	Code string
	// CodeQuestion overrides what the server asks for the code.
	CodeQuestion string
	// CodeInstruction is what the server says alongside the question.
	CodeInstruction string
	// AsksForPasswordInteractively makes the server ask for the password through
	// the same mechanism, which is common and must not reach a person: the
	// password is already configured.
	AsksForPasswordInteractively bool
	// PasswordQuestion overrides how the server words its request for the
	// password, which is what an administrator configures the tool to recognise.
	PasswordQuestion string
	// RefusePty makes the server turn down a terminal, the way one configured
	// with PermitTTY no does.
	RefusePty bool
	// OnSignIn is called once per connection accepted, so a test can prove how
	// many times the tool signed in rather than inferring it.
	OnSignIn func()
}

// Prompt is one question a scripted interactive program asks, and the answer it
// expects. The test server plays the program: it prints Ask, waits for a line,
// and moves on.
type Prompt struct {
	Ask   string
	Reply string
}

// commandRun is what the server pretends a command did.
type commandRun struct {
	// Prompts makes this an interactive program: each is printed in turn and
	// waits for a line back, which is what passwd or an installer does.
	Prompts []Prompt
	Stdout  string
	Stderr  string
	Exit    int
	// Delay holds the answer back, for testing what happens to a command that
	// outlives its deadline.
	Delay time.Duration
	// Drip prints Stdout one line at a time with this long between them, which is
	// what ping, a build, or any periodic command looks like: quiet between
	// lines, and not finished.
	Drip time.Duration
}

func echoCommand(command string) commandRun { return commandRun{Stdout: command} }

// startServer runs an SSH server on a loopback port for the life of the test.
func startServer(t *testing.T, opts serverOptions) *testServer {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}

	cfg := &ssh.ServerConfig{}
	cfg.AddHostKey(hostSigner)
	if opts.Password != "" {
		cfg.PasswordCallback = func(_ ssh.ConnMetadata, given []byte) (*ssh.Permissions, error) {
			if string(given) == opts.Password {
				return nil, nil
			}
			return nil, fmt.Errorf("password rejected")
		}
	}
	if opts.AuthorizedKey != nil {
		cfg.PublicKeyCallback = func(_ ssh.ConnMetadata, given ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(given.Marshal(), opts.AuthorizedKey.Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("key rejected")
		}
	}

	// A server with a second factor. Where it asks for the password in a question
	// too, keyboard-interactive is all it offers. Otherwise it is the real shape:
	// the key authenticates the login and is only a PARTIAL success, so the client
	// must then answer the question as well, which is what "publickey then
	// keyboard-interactive" means on a server that requires both.
	var loginKey string
	if opts.Code != "" || opts.AsksForPasswordInteractively {
		cfg.PasswordCallback = nil
		if opts.AsksForPasswordInteractively {
			cfg.KeyboardInteractiveCallback = interactiveAuth(opts)
		} else {
			private, public := clientKey(t)
			loginKey = private
			interactive := interactiveAuth(opts)
			cfg.PublicKeyCallback = func(_ ssh.ConnMetadata, given ssh.PublicKey) (*ssh.Permissions, error) {
				if !bytes.Equal(given.Marshal(), public.Marshal()) {
					return nil, fmt.Errorf("key rejected")
				}
				return nil, &ssh.PartialSuccessError{
					Next: ssh.ServerAuthCallbacks{KeyboardInteractiveCallback: interactive},
				}
			}
		}
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	run := opts.Run
	if run == nil {
		run = echoCommand
	}
	go serve(listener, cfg, run, opts.RefusePty, opts.OnSignIn)

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("listener address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("listener port: %v", err)
	}
	return &testServer{
		Host:      host,
		Port:      port,
		HostKey:   string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey())),
		ClientKey: loginKey,
	}
}

// interactiveAuth is a server asking its questions at sign-in: the password
// where it is configured to, the verification code where there is one. It is how
// a server with a second factor behaves, and the whole point of the relay under
// test is that the questions are the server's, not ours.
func interactiveAuth(opts serverOptions) func(ssh.ConnMetadata, ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
	return func(_ ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
		passwordQuestion := opts.PasswordQuestion
		if passwordQuestion == "" {
			passwordQuestion = "Password: "
		}
		var questions []string
		if opts.AsksForPasswordInteractively {
			questions = append(questions, passwordQuestion)
		}
		if opts.Code != "" {
			question := opts.CodeQuestion
			if question == "" {
				question = "Verification code: "
			}
			questions = append(questions, question)
		}

		echos := make([]bool, len(questions))
		answers, err := challenge("", opts.CodeInstruction, questions, echos)
		if err != nil {
			return nil, err
		}
		if len(answers) != len(questions) {
			return nil, fmt.Errorf("expected %d answers, got %d", len(questions), len(answers))
		}
		for i, question := range questions {
			want := opts.Code
			if question == passwordQuestion {
				want = opts.Password
			}
			if answers[i] != want {
				return nil, fmt.Errorf("answer to %q rejected", question)
			}
		}
		return nil, nil
	}
}

// serve accepts connections until the listener closes, which the test's cleanup
// does. A connection that fails to handshake is dropped: the client is the one
// under test and it will report the failure itself.
func serve(listener net.Listener, cfg *ssh.ServerConfig, run func(string) commandRun, refusePty bool, onSignIn func()) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			serverConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
			if err != nil {
				return
			}
			if onSignIn != nil {
				onSignIn()
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
				go serveSession(channel, requests, run, refusePty)
			}
		}()
	}
}

// serveSession answers one channel: it runs whatever is asked of it and reports
// an exit status, which is what the client reads the command's outcome from.
func serveSession(channel ssh.Channel, requests <-chan *ssh.Request, run func(string) commandRun, refusePty bool) {
	defer func() { _ = channel.Close() }()
	for request := range requests {
		switch request.Type {
		case "pty-req":
			// A terminal, which a program that asks questions expects. A server can
			// be configured to refuse one (PermitTTY no), and the tool has to work on
			// one that does.
			_ = request.Reply(!refusePty, nil)
			continue
		case "shell":
			// A login shell: no command, so the program is whatever the test's
			// answer function makes of an empty one.
			_ = request.Reply(true, nil)
		case "exec":
		default:
			_ = request.Reply(false, nil)
			continue
		}

		var payload struct{ Command string }
		if request.Type == "exec" {
			if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
				_ = request.Reply(false, nil)
				return
			}
			_ = request.Reply(true, nil)
		}

		result := run(payload.Command)
		if result.Delay > 0 {
			time.Sleep(result.Delay)
		}
		if len(result.Prompts) > 0 {
			converse(channel, result.Prompts)
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(result.Exit)}))
			return
		}
		if result.Drip > 0 {
			for _, line := range strings.SplitAfter(result.Stdout, "\n") {
				if line == "" {
					continue
				}
				_, _ = io.WriteString(channel, line)
				time.Sleep(result.Drip)
			}
		} else {
			_, _ = io.WriteString(channel, result.Stdout)
		}
		_, _ = io.WriteString(channel.Stderr(), result.Stderr)
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(result.Exit)}))
		return
	}
}

// converse plays a program that asks a series of questions: it prints one, reads
// a line, and prints the next.
func converse(channel ssh.Channel, prompts []Prompt) {
	reader := bufio.NewReader(channel)
	for _, prompt := range prompts {
		_, _ = io.WriteString(channel, prompt.Ask)
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if strings.TrimSpace(line) != prompt.Reply {
			_, _ = io.WriteString(channel, "\nrejected\n")
			return
		}
	}
	_, _ = io.WriteString(channel, "\ndone\n")
}

// recorder collects the commands a server was asked to run, so a test can prove
// what did and did not reach it.
type recorder struct {
	mu       sync.Mutex
	commands []string
	answer   func(string) commandRun
}

func newRecorder(answer func(string) commandRun) *recorder {
	if answer == nil {
		answer = echoCommand
	}
	return &recorder{answer: answer}
}

func (r *recorder) run(command string) commandRun {
	r.mu.Lock()
	r.commands = append(r.commands, command)
	r.mu.Unlock()
	return r.answer(command)
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.commands...)
}

// silentListener accepts connections and then says nothing, which is how a
// wedged host behaves: the point is that the client gives up rather than waiting
// on it forever.
type silentListener struct {
	host string
	port int
}

func startSilentListener(t *testing.T) silentListener {
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
			// Held, never spoken to, until the test's cleanup closes the listener.
			t.Cleanup(func() { _ = conn.Close() })
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
	return silentListener{host: host, port: port}
}

// clientKey generates a key pair and returns the PEM private key an
// administrator would paste, with its public half for the server to authorize.
func clientKey(t *testing.T) (privatePEM string, public ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	signerPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("client public key: %v", err)
	}
	return string(pem.EncodeToMemory(block)), signerPub
}
