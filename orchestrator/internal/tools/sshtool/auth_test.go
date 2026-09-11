package sshtool

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"flexie.io/sag/internal/tool"
)

// A server that wants a verification code.
//
// The flow has to hold together across two calls and a person in between: the
// first call stops and says what the server asked, the connection stays open
// waiting, and the second call carries the answer and does the work. The
// assistant carries nothing but the code itself: which sign-in it answers
// follows from the tool and the conversation, neither of which it can get wrong.

// handlerWithCodeWindow shortens the wait for a code, so a test of what happens
// when nobody answers does not sit for a minute.
func handlerWithCodeWindow(t *testing.T, cfg Config, window time.Duration) *handler {
	t.Helper()
	p := newPoolWithCodeWindow(window)
	t.Cleanup(p.Close)
	open := newSessions()
	t.Cleanup(open.Close)
	return &handler{cfg: cfg, pool: p, sessions: open}
}

// codeServerConfig signs in the way a server with a second factor really is
// reached: the login itself by key, and the code from a person. A password
// configured against a server that has no password authentication could not log
// in at all, so no such tool exists to test.
func codeServerConfig(srv *testServer) Config {
	return serverConfig(srv, Auth{PrivateKey: srv.ClientKey})
}

// The first call comes back with the server's question, not with an error: it is
// the server waiting, not anything having gone wrong.
func TestExecuteAsksForAVerificationCode(t *testing.T) {
	seen := newRecorder(nil)
	srv := startServer(t, serverOptions{
		Password: "hunter2", Run: seen.run,
		Code:            "424242",
		CodeQuestion:    "One-time code: ",
		CodeInstruction: "A code has been sent to your phone.",
	})
	h := newHandler(t, codeServerConfig(srv))

	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if result.Failed() {
		t.Fatalf("waiting for a code was reported as a failure: %s", result.Content)
	}

	payload := decode(t, result)
	if payload["verification_required"] != true {
		t.Fatalf("the assistant was not told a code is needed: %v", payload)
	}
	// Nothing for the assistant to carry but the code: no identifier it could
	// copy wrongly, lose, or reuse.
	if _, present := payload["auth_id"]; present {
		t.Fatalf("the assistant was handed an identifier to keep track of: %v", payload)
	}
	// The server's own wording, so a person recognises what is being asked.
	if question, _ := payload["question"].(string); !strings.Contains(question, "One-time code") {
		t.Fatalf("question = %q, want the server's own", question)
	}
	if instruction, _ := payload["instruction"].(string); !strings.Contains(instruction, "sent to your phone") {
		t.Fatalf("instruction = %q, want the server's own", instruction)
	}
	message, _ := payload["message"].(string)
	if !strings.Contains(message, "Ask the person") || !strings.Contains(message, "Never guess") {
		t.Fatalf("the assistant is not told to ask a person and not to invent a code: %q", message)
	}
	if commands := seen.seen(); len(commands) != 0 {
		t.Fatalf("the command ran before the server had signed us in: %v", commands)
	}
}

// The second call carries the code, finishes the sign-in and does the work that
// was asked for the first time, in one go.
func TestVerificationCodeSignsInAndRunsTheCommand(t *testing.T) {
	seen := newRecorder(func(string) commandRun { return commandRun{Stdout: "up 4 days\n"} })
	srv := startServer(t, serverOptions{Password: "hunter2", Run: seen.run, Code: "424242"})

	cfg := codeServerConfig(srv)
	h := newHandler(t, cfg)

	if _, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1})); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// The second call is a new turn, so a new handler over the same connections,
	// which is exactly how it happens in a conversation. It carries the same
	// owner because it is the same agent: that is what lets the code find the
	// sign-in the previous turn started.
	next := &handler{cfg: cfg, pool: h.pool, owner: h.owner, sessions: h.sessions}
	result, err := next.handle(context.Background(), callWith(t, CallArgs{
		Command: "uptime", Input: "424242", Wait: 1,
	}))
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if result.Failed() {
		t.Fatalf("the code was given but the call failed: %s", result.Content)
	}
	if stdout, _ := decode(t, result)["output"].(string); stdout != "up 4 days\n" {
		t.Fatalf("stdout = %q, want the command's output", stdout)
	}
	if commands := seen.seen(); len(commands) != 1 || commands[0] != "uptime" {
		t.Fatalf("the server ran %v, want the command once", commands)
	}
}

// Once signed in, the connection is shared: the code is asked for once, not
// before every command.
func TestVerificationIsAskedForOncePerConnection(t *testing.T) {
	seen := newRecorder(nil)
	srv := startServer(t, serverOptions{Password: "hunter2", Run: seen.run, Code: "424242"})

	h := newHandler(t, codeServerConfig(srv))

	if _, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1})); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := h.handle(context.Background(), callWith(t, CallArgs{
		Command: "uptime", Input: "424242", Wait: 1,
	})); err != nil {
		t.Fatalf("second call: %v", err)
	}

	third, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1}))
	if err != nil {
		t.Fatalf("third call: %v", err)
	}
	if payload := decode(t, third); payload["verification_required"] == true {
		t.Fatal("a code was asked for again on a connection already signed in")
	}
	if commands := seen.seen(); len(commands) != 2 {
		t.Fatalf("the server ran %v, want two commands", commands)
	}
}

// Several calls in one conversation arriving while a sign-in waits must not each
// ask for a code: there is one connection, one question, one answer.
func TestConcurrentCallsInOneConversationAskOnce(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2", Code: "424242"})
	h := newHandler(t, codeServerConfig(srv))

	asked := make([]bool, 3)
	var wait sync.WaitGroup
	for i := range asked {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1}))
			if err != nil {
				return
			}
			asked[i] = decode(t, result)["verification_required"] == true
		}(i)
	}
	wait.Wait()

	for i, wasAsked := range asked {
		if !wasAsked {
			t.Fatalf("call %d did not come back with the question", i)
		}
	}
	// One code answers all of them, because there is one sign-in.
	result, err := h.handle(context.Background(), callWith(t, CallArgs{
		Command: "uptime", Input: "424242", Wait: 1,
	}))
	if err != nil {
		t.Fatalf("answering: %v", err)
	}
	if result.Failed() {
		t.Fatalf("the one answer did not complete the sign-in: %s", result.Content)
	}
}

// A question belongs to the agent it was raised by. Another agent on the same
// machine is told to wait rather than being asked for a code it has no way to
// get, and a code from it cannot complete somebody else's sign-in. The two share
// a conversation here, which is exactly what a Gateway and a background
// agent do.
func TestASignInBelongsToTheAgentThatRaisedIt(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2", Code: "424242"})
	agents := newHandlers(t, codeServerConfig(srv), testAgent, testOtherAgent)
	h, other := agents[0], agents[1]

	asked, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1}))
	if err != nil {
		t.Fatalf("first agent: %v", err)
	}
	if decode(t, asked)["verification_required"] != true {
		t.Fatalf("no code was asked for: %s", asked.Content)
	}

	// A second agent waits: asking two people for two codes to open one
	// connection would be one code too many.
	waiting, err := other.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1}))
	if err != nil {
		t.Fatalf("second agent: %v", err)
	}
	if payload := decode(t, waiting); payload["verification_required"] == true {
		t.Fatalf("a second person was asked for a code as well: %v", payload)
	}
	if waiting.Err != tool.ErrorTransient {
		t.Fatalf("error kind = %q, want transient so it waits and retries", waiting.Err)
	}

	// And a code from the wrong agent does not complete the sign-in.
	stray, err := other.handle(context.Background(), callWith(t, CallArgs{
		Command: "uptime", Input: "424242", Wait: 1,
	}))
	if err != nil {
		t.Fatalf("stray answer: %v", err)
	}
	if !stray.Failed() {
		t.Fatalf("a code from another agent completed the sign-in: %s", stray.Content)
	}

	// The agent that was asked can still answer.
	done, err := h.handle(context.Background(), callWith(t, CallArgs{
		Command: "uptime", Input: "424242", Wait: 1,
	}))
	if err != nil {
		t.Fatalf("answering: %v", err)
	}
	if done.Failed() {
		t.Fatalf("the agent that was asked could not answer: %s", done.Content)
	}
}

// An answer arriving with nothing waiting for it is never applied to whatever
// happens to be open later. Nothing is running and no question was asked, so the
// call was simply wrong, and the assistant is told what to send instead.
func TestAnAnswerWithNothingWaitingIsRefused(t *testing.T) {
	seen := newRecorder(nil)
	srv := startServer(t, serverOptions{Password: "hunter2", Run: seen.run, Code: "424242"})
	h := newHandler(t, codeServerConfig(srv))

	result, err := h.handle(context.Background(), callWith(t, CallArgs{
		Command: "uptime", Input: "424242", Wait: 1,
	}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if result.Err != tool.ErrorBadArguments {
		t.Fatalf("error kind = %q, want bad_arguments so the loop learns from it", result.Err)
	}
	if text, _ := decode(t, result)["error"].(string); !strings.Contains(text, "nothing on this server is waiting") {
		t.Fatalf("the answer does not say what was wrong with the call: %q", text)
	}
	// And it did not quietly run the command anyway.
	if commands := seen.seen(); len(commands) != 0 {
		t.Fatalf("a call that was refused still reached the server: %v", commands)
	}
}

// Nobody answers: the server stops waiting, the sign-in is abandoned, and the
// next call raises a fresh question rather than finding a stuck connection.
func TestVerificationWindowClosesAndTheNextCallAsksAgain(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2", Code: "424242"})
	h := handlerWithCodeWindow(t, codeServerConfig(srv), 300*time.Millisecond)

	if _, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1})); err != nil {
		t.Fatalf("first call: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1}))
		if err != nil {
			t.Fatalf("later call: %v", err)
		}
		if decode(t, result)["verification_required"] == true {
			return
		}
	}
	t.Fatal("the abandoned sign-in was never replaced, so the server would stay stuck on it")
}

// A code that is wrong is the server's decision, and it comes back as a failed
// sign-in rather than as a connection that half exists.
func TestWrongVerificationCodeFailsTheSignIn(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2", Code: "424242"})
	h := newHandler(t, codeServerConfig(srv))

	if _, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1})); err != nil {
		t.Fatalf("first call: %v", err)
	}
	result, err := h.handle(context.Background(), callWith(t, CallArgs{
		Command: "uptime", Input: "000000", Wait: 1,
	}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !result.Failed() {
		t.Fatalf("a wrong code was treated as a sign-in: %s", result.Content)
	}
}

// A server that asks for the password rather than taking it directly is answered
// by the tool. A login credential is the tool's to hold: it is never put to a
// person, and never goes near the assistant. The wording is not read at all, in
// any language, because what decides it is whether the server took the password
// through the password method.
func TestTheLoginPasswordIsAnsweredByTheTool(t *testing.T) {
	seen := newRecorder(nil)
	srv := startServer(t, serverOptions{
		Password: "hunter2", Run: seen.run,
		AsksForPasswordInteractively: true,
		PasswordQuestion:             "Passwort: ",
	})
	h := newHandler(t, serverConfig(srv, Auth{Password: "hunter2"}))

	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if payload := decode(t, result); payload["verification_required"] == true {
		t.Fatalf("somebody was asked for the login password: %v", payload)
	}
	if result.Failed() {
		t.Fatalf("the sign-in did not complete: %s", result.Content)
	}
	if commands := seen.seen(); len(commands) != 1 {
		t.Fatalf("the server ran %v, want the command", commands)
	}
}

// The password answers the first thing asked and no more: a server that wants
// the password and then a code is answered by the tool for the one and by a
// person for the other, in one round.
func TestPasswordThenCodeInOneRound(t *testing.T) {
	seen := newRecorder(nil)
	srv := startServer(t, serverOptions{
		Password: "hunter2", Run: seen.run,
		Code:                         "424242",
		AsksForPasswordInteractively: true,
	})
	h := newHandler(t, serverConfig(srv, Auth{Password: "hunter2"}))

	asked, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1}))
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	payload := decode(t, asked)
	if payload["verification_required"] != true {
		t.Fatalf("the code was not asked for: %v", payload)
	}
	// The person is asked for the code, not for the password the tool holds.
	if question, _ := payload["question"].(string); !strings.Contains(question, "Verification") {
		t.Fatalf("question = %q, want the code question and not the password one", question)
	}

	result, err := h.handle(context.Background(), callWith(t, CallArgs{
		Command: "uptime", Input: "424242", Wait: 1,
	}))
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if result.Failed() {
		t.Fatalf("the sign-in failed: %s", result.Content)
	}
	if commands := seen.seen(); len(commands) != 1 {
		t.Fatalf("the server ran %v, want the command", commands)
	}
}

// The code is written into the connection and nowhere else. It must not come
// back in a result, where it would be written into the transcript twice over.
func TestTheCodeIsNotEchoedBack(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2", Code: "424242"})
	h := newHandler(t, codeServerConfig(srv))

	if _, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1})); err != nil {
		t.Fatalf("first call: %v", err)
	}
	result, err := h.handle(context.Background(), callWith(t, CallArgs{
		Command: "uptime", Input: "424242", Wait: 1,
	}))
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if strings.Contains(string(result.Content), "424242") {
		t.Fatalf("the code came back in the result: %s", result.Content)
	}
}

// The administrator's Test reaches the question and stops there. Everything it
// can check has checked out: the address, the host key, and the credential taken
// far enough for the server to ask for more.
func TestTestPassesWhenTheServerAsksForACode(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2", Code: "424242"})

	config, err := json.Marshal(codeServerConfig(srv))
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := (sshTemplate{}).Test(context.Background(), config); err != nil {
		t.Fatalf("a server that asks for a code failed its test: %v", err)
	}
}
