package sshtool

import (
	"context"
	"strings"
	"testing"
	"time"

	"flexie.io/sag/internal/cmdpolicy"
	"flexie.io/sag/internal/tool"
)

// Driving a program that stops and asks.
//
// The whole point is that the assistant reads what the program said and answers
// it, so what has to hold is: the question comes back, the answer reaches the
// program, the program's next question comes back, and the session is given up
// when the program is done. Plus the part that is not about capability at all,
// which is what may be started this way and what may be typed into it.

// interactiveConfig permits a program the ordinary way: what may be started in a
// session is what may be run, one list for both.
func interactiveConfig(srv *testServer, allowed string) Config {
	cfg := serverConfig(srv, Auth{Password: "hunter2"})
	cfg.Policy = cmdpolicy.Policy{Mode: cmdpolicy.PolicyAllowlist, Allowed: allowed, Sudo: cmdpolicy.SudoDeny}
	return cfg
}

func passwdServer(t *testing.T) *testServer {
	t.Helper()
	return startServer(t, serverOptions{
		Password: "hunter2",
		Run: func(string) commandRun {
			return commandRun{Prompts: []Prompt{
				{Ask: "New password: ", Reply: "s3cret"},
				{Ask: "Retype new password: ", Reply: "s3cret"},
			}}
		},
	})
}

// A program is started, asks, is answered, asks again, and finishes.
func TestInteractiveSessionAnswersAProgram(t *testing.T) {
	srv := passwdServer(t)
	h := newHandler(t, interactiveConfig(srv, "passwd"))

	started, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "passwd deploy", Wait: 1}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if decode(t, started)["running"] != true {
		t.Fatalf("the program did not start: %s", started.Content)
	}
	if started.Failed() {
		t.Fatalf("the program could not be started: %s", started.Content)
	}
	payload := decode(t, started)
	if output, _ := payload["output"].(string); !strings.Contains(output, "New password") {
		t.Fatalf("the program's question did not come back: %q", output)
	}
	if payload["running"] != true {
		t.Fatalf("the session was not left open: %v", payload)
	}

	// The answer reaches the program, and its next question comes back.
	next, err := h.handle(context.Background(), callWith(t, CallArgs{Input: "s3cret", Wait: 1}))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if output, _ := decode(t, next)["output"].(string); !strings.Contains(output, "Retype") {
		t.Fatalf("the second question did not come back: %q", output)
	}

	// Answering the last one finishes it, and the session is given up.
	last, err := h.handle(context.Background(), callWith(t, CallArgs{Input: "s3cret", Wait: 1}))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	final := decode(t, last)
	if output, _ := final["output"].(string); !strings.Contains(output, "done") {
		t.Fatalf("the program did not finish: %q", output)
	}
	if final["running"] != false {
		t.Fatalf("the session is still open after the program ended: %v", final)
	}
	if _, ok := h.sessions.get(testAgent, h.server()); ok {
		t.Fatal("a finished program left its session behind")
	}
}

// What is typed is not echoed back as if the program had said it: the terminal
// is asked for with echo off, or every answer would appear twice.
func TestInteractiveOutputDoesNotEchoWhatWasTyped(t *testing.T) {
	srv := passwdServer(t)
	h := newHandler(t, interactiveConfig(srv, "passwd"))

	started, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "passwd deploy", Wait: 1}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if decode(t, started)["running"] != true {
		t.Fatalf("the program did not start: %s", started.Content)
	}

	next, err := h.handle(context.Background(), callWith(t, CallArgs{Input: "s3cret", Wait: 1}))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if output, _ := decode(t, next)["output"].(string); strings.Contains(output, "s3cret") {
		t.Fatalf("what was typed came back in the output: %q", output)
	}
}

// A session belongs to the AGENT that opened it, and is not reachable from
// another agent, even one in the same conversation. That pairing is the whole
// point: a Gateway and the agents it starts in the background all run under
// one conversation, so keying a session on the conversation would put them on
// one program, and one of them would type into another's.
func TestInteractiveSessionBelongsToItsAgent(t *testing.T) {
	srv := passwdServer(t)
	agents := newHandlers(t, interactiveConfig(srv, "passwd"), testAgent, testOtherAgent)
	h, other := agents[0], agents[1]

	started, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "passwd deploy", Wait: 1}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if decode(t, started)["running"] != true {
		t.Fatalf("the program did not start: %s", started.Content)
	}

	stray, err := other.handle(context.Background(), callWith(t, CallArgs{Input: "s3cret", Wait: 1}))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if stray.Err != tool.ErrorBadArguments {
		t.Fatalf("another agent reached the session: %s", stray.Content)
	}

	// And reading finds nothing: the other agent's program is not theirs to see,
	// so they are simply told they have none open.
	read, err := other.handle(context.Background(), callWith(t, CallArgs{}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.Err != tool.ErrorBadArguments {
		t.Fatalf("another agent read the session: %s", read.Content)
	}

	// The agent that opened it still has it, which is the other half of the
	// claim: isolation, not everybody losing the session.
	mine, err := h.handle(context.Background(), callWith(t, CallArgs{}))
	if err != nil {
		t.Fatalf("read own: %v", err)
	}
	if mine.Failed() {
		t.Fatalf("the agent that opened the session lost it: %s", mine.Content)
	}
}

// What may be started in a session is what may be run: one list, not two. A
// program the tool does not permit cannot be reached by opening it instead of
// running it.
func TestInteractiveStartObeysTheSameList(t *testing.T) {
	srv := passwdServer(t)
	h := newHandler(t, interactiveConfig(srv, "passwd"))

	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if result.Err != tool.ErrorBlocked {
		t.Fatalf("a program the tool does not permit was started: %s", result.Content)
	}

	// And a session with no program at all is a login shell, which has to be
	// permitted like anything else rather than arriving by omission.
	bare, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "sh", Wait: 1}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if bare.Err != tool.ErrorBlocked {
		t.Fatalf("a shell was opened on a tool that does not permit one: %s", bare.Content)
	}
}

// A denied program cannot be started interactively even if somebody put it on
// the interactive list as well.
func TestInteractiveStartRefusesADeniedProgram(t *testing.T) {
	srv := passwdServer(t)
	cfg := interactiveConfig(srv, "bash")
	cfg.Policy = cmdpolicy.Policy{Mode: cmdpolicy.PolicyDenylist, Denied: "bash", Sudo: cmdpolicy.SudoDeny}
	h := newHandler(t, cfg)

	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "bash", Wait: 1}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if result.Err != tool.ErrorBlocked {
		t.Fatalf("a denied program was started interactively: %s", result.Content)
	}
}

// Typing into a shell is typing commands, so it gets exactly the check a command
// gets: the parser, the whole AST, every program the line would run.
//
// The lines are built from the denylist too, in each shape the AST is there to
// see through: a second command, a pipeline, a substitution, an argument, a path.
func TestInputToAShellIsCheckedAsACommand(t *testing.T) {
	denied := []string{"rm", "shutdown", "dd"}

	shapes := []func(string) string{
		func(p string) string { return p + " -rf /" },
		func(p string) string { return "uptime; " + p + " -rf /" },
		func(p string) string { return "uptime && " + p },
		func(p string) string { return "cat /etc/hosts | " + p },
		func(p string) string { return "(cd /tmp && " + p + " .)" },
		func(p string) string { return "xargs " + p },
		func(p string) string { return "/bin/" + p + " -rf /" },
		func(p string) string { return "find . -exec " + p + " {} ;" },
	}

	srv := startServer(t, serverOptions{
		Password: "hunter2",
		Run: func(string) commandRun {
			return commandRun{Prompts: []Prompt{{Ask: "$ ", Reply: "uptime"}}}
		},
	})
	cfg := interactiveConfig(srv, "bash")
	cfg.Policy = cmdpolicy.Policy{Mode: cmdpolicy.PolicyDenylist, Sudo: cmdpolicy.SudoDeny, Denied: strings.Join(denied, "\n")}
	h := newHandler(t, cfg)

	started, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "bash", Wait: 1}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if decode(t, started)["running"] != true {
		t.Fatalf("the shell did not start: %s", started.Content)
	}

	for _, program := range denied {
		for _, shape := range shapes {
			line := shape(program)
			result, err := h.handle(context.Background(), callWith(t, CallArgs{Input: line, Wait: 1}))
			if err != nil {
				t.Fatalf("send %q: %v", line, err)
			}
			if result.Err != tool.ErrorBlocked {
				t.Fatalf("%q was typed into a shell: %s", line, result.Content)
			}
		}
	}

	// A command that is not denied still runs, or a shell session would be
	// useless.
	result, err := h.handle(context.Background(), callWith(t, CallArgs{Input: "uptime", Wait: 1}))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.Err == tool.ErrorBlocked {
		t.Fatalf("an ordinary command was refused in a shell: %s", result.Content)
	}
}

// Into a program that is not a shell there is nothing to parse, so what is left
// is refusing a line that names a denied program however it is dressed up.
//
// The programs and the escapes are generated from the denylist rather than
// written out, so this proves the mechanism rather than one string: whatever an
// administrator denies is what gets refused, in every shape a program's own
// escape takes.
func TestInputToAProgramRefusesAnyDeniedProgramItNames(t *testing.T) {
	denied := []string{"rm", "shutdown", "mkfs.ext4", "dd"}

	// The ways a program that is not a shell offers to run something: less and
	// ftp take !, vi takes :!, mysql takes \!, and any of them can be given a
	// path instead of a name.
	escapes := []func(string) string{
		func(p string) string { return "!" + p + " -rf /" },
		func(p string) string { return ":!" + p },
		func(p string) string { return `\! ` + p + " /var" },
		func(p string) string { return "!/bin/" + p },
		func(p string) string { return "  !   /usr/local/sbin/" + p + "   --force" },
		func(p string) string { return "%!" + p },
	}

	srv := passwdServer(t)
	cfg := interactiveConfig(srv, "passwd")
	cfg.Policy = cmdpolicy.Policy{Mode: cmdpolicy.PolicyDenylist, Sudo: cmdpolicy.SudoDeny, Denied: strings.Join(denied, "\n")}
	h := newHandler(t, cfg)

	started, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "passwd deploy", Wait: 1}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if decode(t, started)["running"] != true {
		t.Fatalf("the program did not start: %s", started.Content)
	}

	for _, program := range denied {
		for _, escape := range escapes {
			line := escape(program)
			result, err := h.handle(context.Background(), callWith(t, CallArgs{Input: line, Wait: 1}))
			if err != nil {
				t.Fatalf("send %q: %v", line, err)
			}
			if result.Err != tool.ErrorBlocked {
				t.Fatalf("%q reached the program: %s", line, result.Content)
			}
			// And the refusal names what it objected to, so the assistant can
			// report it rather than cast about for another way through.
			if text, _ := decode(t, result)["error"].(string); !strings.Contains(text, program) {
				t.Fatalf("the refusal for %q does not say what was refused: %q", line, text)
			}
		}
	}

	// Ordinary answers still go through, which is the whole point of the session:
	// a program that asks a question has to be answerable.
	for _, answer := range []string{"y", "yes", "n", "3", "s3cret", "/etc/hosts", "Europe/Tirane"} {
		result, err := h.handle(context.Background(), callWith(t, CallArgs{Input: answer, Wait: 1}))
		if err != nil {
			t.Fatalf("send %q: %v", answer, err)
		}
		if result.Err == tool.ErrorBlocked {
			t.Fatalf("the answer %q was refused: %s", answer, result.Content)
		}
	}
}

// A denylist entry of several words denies the program, not just that exact
// line, once it is typed into something this cannot parse: denying "systemctl
// stop database" refuses any input naming systemctl.
//
// That is over-refusal, and deliberate. Outside a shell there is no way to tell
// which systemctl a line would reach, so the conservative reading is the only
// honest one. It is a test because it is a decision, not an accident. In a shell,
// where the line can be parsed, the denial is exact again.
func TestAMultiWordDenialRefusesTheProgramInInput(t *testing.T) {
	srv := passwdServer(t)
	cfg := interactiveConfig(srv, "passwd")
	cfg.Policy = cmdpolicy.Policy{Mode: cmdpolicy.PolicyDenylist, Sudo: cmdpolicy.SudoDeny, Denied: "systemctl stop database"}
	h := newHandler(t, cfg)

	started, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "passwd deploy", Wait: 1}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if decode(t, started)["running"] != true {
		t.Fatalf("the program did not start: %s", started.Content)
	}

	for _, line := range []string{"!systemctl stop database", "!systemctl restart web"} {
		result, err := h.handle(context.Background(), callWith(t, CallArgs{Input: line, Wait: 1}))
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		if result.Err != tool.ErrorBlocked {
			t.Fatalf("%q reached the program: %s", line, result.Content)
		}
	}

	// In a shell the same denial is exact: the command that was denied is
	// refused, and another use of the same program is not.
	shell := newHandler(t, cfg)
	shellStart, err := shell.handle(context.Background(), callWith(t, CallArgs{Command: "bash", Wait: 1}))
	if err != nil {
		t.Fatalf("start shell: %v", err)
	}
	if decode(t, shellStart)["running"] != true {
		t.Fatalf("the shell did not start: %s", shellStart.Content)
	}

	refused, err := shell.handle(context.Background(), callWith(t, CallArgs{Input: "systemctl stop database", Wait: 1}))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if refused.Err != tool.ErrorBlocked {
		t.Fatalf("the denied command ran in a shell: %s", refused.Content)
	}

	allowed, err := shell.handle(context.Background(), callWith(t, CallArgs{Input: "systemctl restart web", Wait: 1}))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if allowed.Err == tool.ErrorBlocked {
		t.Fatalf("a command that was not denied was refused in a shell: %s", allowed.Content)
	}
}

// A session is closed when it is finished with, and the channel it was holding
// goes back.
func TestClosingASessionGivesBackItsSlot(t *testing.T) {
	srv := passwdServer(t)
	cfg := interactiveConfig(srv, "passwd")
	cfg.Limits.MaxChannels = 1
	h := newHandler(t, cfg)

	started, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "passwd deploy", Wait: 1}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if decode(t, started)["running"] != true {
		t.Fatalf("the program did not start: %s", started.Content)
	}

	closed, err := h.handle(context.Background(), callWith(t, CallArgs{
		Stop: true,
	}))
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.Failed() {
		t.Fatalf("the session would not close: %s", closed.Content)
	}

	// The one channel is free again, so an ordinary command runs.
	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Err == tool.ErrorTransient {
		t.Fatalf("the closed session did not give its channel back: %s", result.Content)
	}
}

// Sessions nobody is using are closed, so a forgotten one does not hold a
// channel open on the server for the rest of the day.
func TestIdleSessionsAreClosed(t *testing.T) {
	srv := passwdServer(t)
	h := newHandler(t, interactiveConfig(srv, "passwd"))

	started, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "passwd deploy", Wait: 1}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if decode(t, started)["running"] != true {
		t.Fatalf("the program did not start: %s", started.Content)
	}

	h.sessions.closeStale(time.Now().Add(sessionIdle + time.Minute))
	if _, ok := h.sessions.get(testAgent, h.server()); ok {
		t.Fatal("a session nobody had touched was left open")
	}
}
