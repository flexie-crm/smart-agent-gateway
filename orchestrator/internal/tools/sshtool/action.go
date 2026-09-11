package sshtool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"flexie.io/sag/internal/tools/template"

	"flexie.io/sag/internal/tool"
)

// What an administrator can do with this tool from the console.
//
// Testing a connection is not always one step: a server that wants a
// verification code cannot be checked without somebody reading one out. So the
// test signs in as far as it can, and when the server asks a question it hands
// the question back to be put to the administrator, in the same form vocabulary
// the settings are collected with. Nothing between here and the screen knows
// that any of this is about verification codes.

// actionVerify carries an administrator's answer back to a test that stopped to
// ask for one.
const actionVerify = "verify"

// consoleOwner marks a sign-in raised by an administrator testing a connection
// rather than by an agent. A test's question is identified by the token the
// console carries back, so there is no agent for it to belong to.
const consoleOwner tool.Owner = "console"

// pendingTests holds the test sign-ins that are waiting on an answer. They are
// short lived by construction: the sign-in gives up when its window closes, and
// a test never leaves a connection behind either way.
type pendingTests struct {
	mu       sync.Mutex
	byToken  map[string]*pendingTest
	deadline time.Duration
}

type pendingTest struct {
	attempt *signIn
	expires time.Time
}

func newPendingTests(window time.Duration) *pendingTests {
	return &pendingTests{byToken: map[string]*pendingTest{}, deadline: window}
}

// add records a waiting test and clears out any that nobody came back to.
func (p *pendingTests) add(token string, attempt *signIn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	for key, pending := range p.byToken {
		if now.After(pending.expires) {
			delete(p.byToken, key)
		}
	}
	p.byToken[token] = &pendingTest{attempt: attempt, expires: now.Add(p.deadline)}
}

// take returns the waiting test for a token and forgets it: an answer is used
// once, so a token that leaks cannot be replayed against a later question.
func (p *pendingTests) take(token string) (*signIn, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	pending, ok := p.byToken[token]
	if !ok {
		return nil, false
	}
	delete(p.byToken, token)
	if time.Now().After(pending.expires) {
		return nil, false
	}
	return pending.attempt, true
}

// actionBody is what the console sends: the token of the question being
// answered, and the answers keyed by the fields that were asked for.
type actionBody struct {
	Token  string            `json:"token"`
	Values map[string]string `json:"values"`
}

// Action runs one of this tool's own operations. The configuration arrives with
// its secrets already opened, exactly as Test's does.
func (t *sshTemplate) Action(ctx context.Context, config json.RawMessage, action string, body json.RawMessage) (template.ActionResult, error) {
	cfg, err := ParseConfig(config)
	if err != nil {
		return template.ActionResult{OK: false, Message: err.Error()}, nil
	}

	var payload actionBody
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			return template.ActionResult{}, fmt.Errorf("the request could not be read")
		}
	}

	switch action {
	case template.ActionTest:
		return t.startTest(ctx, cfg)
	case actionVerify:
		return t.finishTest(ctx, payload)
	default:
		return template.ActionResult{}, fmt.Errorf("unknown action %q", action)
	}
}

// startTest signs in to the configured server. It reports success, the reason it
// could not, or the question the server stopped on.
func (t *sshTemplate) startTest(ctx context.Context, cfg Config) (template.ActionResult, error) {
	attempt := newSignIn()
	// Deliberately not the request's context: when the server stops to ask for a
	// code, this request returns the question and the answer arrives on a later
	// one. A sign-in tied to the request that started it would be cancelled
	// before anybody could answer. It ends at shutdown instead, and on its own
	// deadline before that.
	go t.testSignIn(cfg, attempt) //nolint:contextcheck // the sign-in outlives the request by design

	select {
	case <-attempt.done:
		return testOutcome(attempt)
	case <-attempt.asked:
		question := attempt.question()
		t.tests.add(question.ID, attempt)
		return template.ActionResult{
			OK: false,
			Message: "The connection reached the server and it signed in as far as it could. " +
				"The server is asking for a verification code to finish.",
			Prompt: &template.ActionPrompt{
				Action: actionVerify,
				Token:  question.ID,
				Title:  "The server is asking for a verification code",
				Hint:   codeHint(question.Instruction),
				Submit: "Finish the test",
				Fields: []template.Field{{
					Key:      "code",
					Label:    strings.TrimSuffix(strings.TrimSpace(question.Question()), ":"),
					Type:     template.FieldPassword,
					Required: true,
					Help:     "The code the server is asking you for, right now. It is used once and never stored.",
				}},
			},
		}, nil
	case <-ctx.Done():
		return template.ActionResult{OK: false, Message: "The server did not answer in time."}, nil
	}
}

// finishTest hands the administrator's code to the sign-in waiting for it.
func (t *sshTemplate) finishTest(ctx context.Context, payload actionBody) (template.ActionResult, error) {
	code := strings.TrimSpace(payload.Values["code"])
	if code == "" {
		return template.ActionResult{OK: false, Message: "Enter the code the server is asking for."}, nil
	}
	attempt, ok := t.tests.take(payload.Token)
	if !ok {
		return template.ActionResult{
			OK:      false,
			Message: "The server stopped waiting for that code. Test the connection again to get a fresh one.",
		}, nil
	}
	if err := attempt.deliver(consoleOwner, code); err != nil {
		return template.ActionResult{
			OK:      false,
			Message: "The server stopped waiting for that code. Test the connection again to get a fresh one.",
		}, nil
	}

	select {
	case <-attempt.done:
		return testOutcome(attempt)
	case <-ctx.Done():
		return template.ActionResult{OK: false, Message: "The server did not answer in time."}, nil
	}
}

// testSignIn connects and publishes the outcome. A test proves the settings and
// then gets out of the way: the connection it opened is closed here, never
// pooled, so testing a tool nobody saves leaves nothing behind.
func (t *sshTemplate) testSignIn(cfg Config, attempt *signIn) {
	budget := time.Duration(cfg.Limits.ConnectSeconds)*time.Second + defaultCodeWindow + reapInterval
	// The pool's context, so a test still waiting on a person ends when the
	// process does rather than holding a goroutine open past it.
	ctx, cancel := context.WithTimeout(t.pool.ctx, budget)
	defer cancel()

	client, err := dial(ctx, cfg, attempt.challenge(defaultCodeWindow, consoleOwner), defaultCodeWindow)
	if client != nil {
		_ = client.Close()
	}
	attempt.finish(nil, err)
}

// testOutcome turns a finished sign-in into what the administrator reads.
func testOutcome(attempt *signIn) (template.ActionResult, error) {
	if _, err := attempt.result(); err != nil {
		return template.ActionResult{OK: false, Message: connectionFailure(err)}, nil
	}
	return template.ActionResult{OK: true, Message: "Connected and signed in successfully."}, nil
}

// connectionFailure is what an administrator reads when a test did not work. It
// keeps our own wording and drops the sockets-and-handshakes detail underneath,
// which belongs in a log rather than on a form.
func connectionFailure(err error) string {
	message := err.Error()
	if cut := strings.Index(message, ": "); cut > 0 {
		message = message[:cut]
	}
	if message == "" {
		return "The connection did not work."
	}
	return strings.ToUpper(message[:1]) + message[1:] + "."
}

// codeHint passes on whatever the server said alongside its question, which is
// often where a person is told where to look for the code.
func codeHint(instruction string) string {
	if instruction = strings.TrimSpace(instruction); instruction != "" {
		return instruction
	}
	return "This server asks for a second factor as well as a password or key."
}
