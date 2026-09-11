package sshtool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"flexie.io/sag/internal/cmdpolicy"
	"flexie.io/sag/internal/tool"
)

// One call, end to end: the policy decides, the pool provides a channel, the
// command runs on a real server, and what comes back is what the assistant will
// read. These are the tests that would notice the tool quietly doing nothing.

func newHandler(t *testing.T, cfg Config) *handler {
	t.Helper()
	return newHandlers(t, cfg, testAgent)[0]
}

// newHandlers builds several bound tools over ONE pool and ONE set of sessions,
// which is how several agents in a process hold the same tool. Giving them
// different owners is what a Gateway and its background agents are: the
// same tool on the same machine in the same conversation, run by different
// agents.
func newHandlers(t *testing.T, cfg Config, owners ...tool.Owner) []*handler {
	t.Helper()
	p := newPool()
	t.Cleanup(p.Close)
	open := newSessions()
	t.Cleanup(open.Close)
	hs := make([]*handler, 0, len(owners))
	for _, owner := range owners {
		hs = append(hs, &handler{cfg: cfg, pool: p, owner: owner, sessions: open})
	}
	return hs
}

func callWith(t *testing.T, args CallArgs) tool.Call {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("encode arguments: %v", err)
	}
	return tool.Call{WorkspaceID: 1, UserID: 2, SessionID: 3, Name: "ssh_production", Args: raw}
}

// decode reads a result's payload, which is the JSON the model is handed.
func decode(t *testing.T, result tool.Result) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(result.Content, &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return payload
}

func TestExecuteRunsAPermittedCommand(t *testing.T) {
	seen := newRecorder(func(command string) commandRun {
		return commandRun{Stdout: "up 4 days\n", Stderr: "a warning", Exit: 0}
	})
	srv := startServer(t, serverOptions{Password: "hunter2", Run: seen.run})

	cfg := serverConfig(srv, Auth{Password: "hunter2"})
	h := newHandler(t, cfg)

	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 2}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if result.Failed() {
		t.Fatalf("a permitted command failed: %s", result.Content)
	}

	// Output and errors come back as one stream, because that is what a terminal
	// gives and what a person reading the screen would see.
	payload := decode(t, result)
	output, _ := payload["output"].(string)
	if !strings.Contains(output, "up 4 days") {
		t.Fatalf("output = %q, want the command's output", output)
	}
	if !strings.Contains(output, "a warning") {
		t.Fatalf("output = %q, want the command's error output in the same stream", output)
	}
	if payload["running"] != false {
		t.Fatalf("a command that finished came back still running: %v", payload)
	}
	if payload["exit_code"] != float64(0) {
		t.Fatalf("exit_code = %v, want 0", payload["exit_code"])
	}
	if commands := seen.seen(); len(commands) != 1 || commands[0] != "uptime" {
		t.Fatalf("the server was asked to run %v, want [uptime]", commands)
	}
}

// A command that fails is not a failure of the tool: the assistant is given the
// exit code and the output and decides what they mean.
func TestExecuteReportsANonZeroExitAsAResult(t *testing.T) {
	srv := startServer(t, serverOptions{
		Password: "hunter2",
		Run:      func(string) commandRun { return commandRun{Stderr: "not running", Exit: 3} },
	})
	h := newHandler(t, serverConfig(srv, Auth{Password: "hunter2"}))

	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 2}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if result.Failed() {
		t.Fatalf("a command that exited non-zero was reported as a tool failure: %s", result.Content)
	}
	if payload := decode(t, result); payload["exit_code"] != float64(3) {
		t.Fatalf("exit_code = %v, want 3", payload["exit_code"])
	}
}

// A refused command never reaches the server, and the refusal is not the kind of
// failure the assistant should try to correct by rewording.
func TestExecuteRefusesWhatThePolicyForbids(t *testing.T) {
	seen := newRecorder(nil)
	srv := startServer(t, serverOptions{Password: "hunter2", Run: seen.run})

	cfg := serverConfig(srv, Auth{Password: "hunter2"})
	cfg.Policy = cmdpolicy.Policy{Mode: cmdpolicy.PolicyAllowlist, Allowed: "uptime", Sudo: cmdpolicy.SudoDeny}
	h := newHandler(t, cfg)

	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime; rm -rf /", Wait: 2}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if result.Err != tool.ErrorBlocked {
		t.Fatalf("error kind = %q, want blocked", result.Err)
	}
	if commands := seen.seen(); len(commands) != 0 {
		t.Fatalf("a refused command reached the server: %v", commands)
	}
}

// A command that has not finished when the wait runs out is not stopped and not
// a failure: it is reported as still running, with what it printed, and it is
// still there for the next call.
func TestACommandThatDoesNotFinishComesBackRunning(t *testing.T) {
	srv := startServer(t, serverOptions{
		Password: "hunter2",
		Run: func(string) commandRun {
			return commandRun{Prompts: []Prompt{{Ask: "working, continue? ", Reply: "yes"}}}
		},
	})
	h := newHandler(t, serverConfig(srv, Auth{Password: "hunter2"}))

	// A short wait, because this one is expected to stop and ask: what is wanted
	// is the question, not the end of the command.
	started := time.Now()
	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatalf("the call took %s for a one second wait", waited)
	}
	if result.Failed() {
		t.Fatalf("an unfinished command was reported as a failure: %s", result.Content)
	}
	payload := decode(t, result)
	if payload["running"] != true {
		t.Fatalf("the result does not say it is still running: %v", payload)
	}
	if output, _ := payload["output"].(string); !strings.Contains(output, "working, continue?") {
		t.Fatalf("output = %q, want what it printed before it went quiet", output)
	}
	if note, _ := payload["note"].(string); !strings.Contains(note, "input") || !strings.Contains(note, "stop") {
		t.Fatalf("the result does not say what to do about it: %q", note)
	}

	// And it is still there: reading it again reaches the same program.
	if _, ok := h.sessions.get(h.owner, h.server()); !ok {
		t.Fatal("the unfinished command was not kept for the next call")
	}
}

// Output is capped, and a capped result says so: a truncated answer read as the
// whole of it is worse than no answer.
func TestExecuteCapsOutputAndSaysSo(t *testing.T) {
	srv := startServer(t, serverOptions{
		Password: "hunter2",
		Run: func(string) commandRun {
			return commandRun{Stdout: strings.Repeat("x", sessionOutputLimit+5000)}
		},
	})
	h := newHandler(t, serverConfig(srv, Auth{Password: "hunter2"}))

	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 2}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	payload := decode(t, result)
	kept, _ := payload["output"].(string)
	if len(kept) > sessionOutputLimit {
		t.Fatalf("kept %d bytes, want no more than the cap of %d", len(kept), sessionOutputLimit)
	}
	if payload["output_dropped"] != true {
		t.Fatalf("the result does not say it was cut short: %v", payload)
	}
}

// A server with every slot busy tells the assistant to try again, which is a
// retryable answer rather than a failure it should give up on.
func TestExecuteTellsTheAssistantToWaitWhenTheServerIsBusy(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	cfg := serverConfig(srv, Auth{Password: "hunter2"})
	cfg.Limits.MaxChannels = 1

	h := newHandler(t, cfg)
	server, err := h.pool.server(cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	server.slots <- struct{}{} // the one slot, taken by work already running
	defer func() { <-server.slots }()

	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 2}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if result.Err != tool.ErrorTransient {
		t.Fatalf("error kind = %q, want transient", result.Err)
	}
	payload := decode(t, result)
	if payload["retry"] != true {
		t.Fatalf("the assistant was not told it may retry: %v", payload)
	}
	if text, _ := payload["error"].(string); !strings.Contains(text, "busy") {
		t.Fatalf("the answer does not say the server is busy: %q", text)
	}
}

// A slot is given back however the command ended, or a server would run out of
// them after a few failures.
func TestExecuteReleasesItsSlot(t *testing.T) {
	srv := startServer(t, serverOptions{
		Password: "hunter2",
		Run:      func(string) commandRun { return commandRun{Exit: 1} },
	})
	cfg := serverConfig(srv, Auth{Password: "hunter2"})
	cfg.Limits.MaxChannels = 1
	h := newHandler(t, cfg)

	for i := 0; i < 3; i++ {
		result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 2}))
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if result.Err == tool.ErrorTransient {
			t.Fatalf("call %d found the server busy, so a slot was not given back", i)
		}
	}
}

// A call that cannot be read, or asks for something this tool does not do, is
// the one failure worth learning from, so it comes back classified as such.
func TestBadArgumentsAreClassifiedForTheLoop(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	h := newHandler(t, serverConfig(srv, Auth{Password: "hunter2"}))

	cases := []tool.Call{
		// Not readable at all.
		{Args: json.RawMessage(`{"command":`)},
		// Nothing running, so there is nothing to read, answer, or stop.
		{Args: json.RawMessage(`{}`)},
		{Args: json.RawMessage(`{"input":"yes"}`)},
		{Args: json.RawMessage(`{"stop":true}`)},
		// stop ends what is running; it cannot also start something.
		{Args: json.RawMessage(`{"stop":true,"command":"uptime"}`)},
	}
	for _, call := range cases {
		result, err := h.handle(context.Background(), call)
		if err != nil {
			t.Fatalf("handle: %v", err)
		}
		if result.Err != tool.ErrorBadArguments {
			t.Fatalf("%s gave error kind %q, want bad_arguments", call.Args, result.Err)
		}
	}
}

// A server that cannot be reached is reported without the plumbing underneath it.
func TestUnreachableServerIsReportedPlainly(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	cfg := serverConfig(srv, Auth{Password: "wrong"})
	h := newHandler(t, cfg)

	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 2}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !result.Failed() {
		t.Fatal("a server that refused the sign-in was reported as a success")
	}
	text, _ := decode(t, result)["error"].(string)
	if strings.Contains(text, "handshake") || strings.Contains(text, "ssh:") {
		t.Fatalf("the plumbing reached the assistant: %q", text)
	}
}

// Input reaches the running program, which is what replaced passing standard
// input up front: the command runs, stops for an answer, and the next call
// answers it.
func TestInputReachesTheRunningCommand(t *testing.T) {
	srv := startServer(t, serverOptions{
		Password: "hunter2",
		Run: func(string) commandRun {
			return commandRun{Prompts: []Prompt{{Ask: "proceed? ", Reply: "yes"}}}
		},
	})
	h := newHandler(t, serverConfig(srv, Auth{Password: "hunter2"}))

	started, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 2}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if decode(t, started)["running"] != true {
		t.Fatalf("a command waiting on its input did not come back running: %s", started.Content)
	}

	answered, err := h.handle(context.Background(), callWith(t, CallArgs{Input: "yes", Wait: 2}))
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	// The program accepted the answer and ran to the end, which is the proof the
	// input reached it: a wrong answer makes this server say so instead.
	payload := decode(t, answered)
	if output, _ := payload["output"].(string); !strings.Contains(output, "done") {
		t.Fatalf("output = %q, want the program to have accepted the answer", output)
	}
	if payload["running"] != false {
		t.Fatalf("the program did not finish after being answered: %v", payload)
	}
	if strings.Contains(payload["output"].(string), "yes") {
		t.Fatalf("what was typed came back as though the program had said it: %q", payload["output"])
	}
}

// A command that says nothing at all is not waiting for anybody: it just has not
// finished. The call comes back when the wait runs out and the command keeps
// going.
func TestASilentCommandComesBackWhenTheWaitRunsOut(t *testing.T) {
	srv := startServer(t, serverOptions{
		Password: "hunter2",
		Run:      func(string) commandRun { return commandRun{Stdout: "eventually", Delay: 30 * time.Second} },
	})
	h := newHandler(t, serverConfig(srv, Auth{Password: "hunter2"}))

	started := time.Now()
	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatalf("the call took %s for a one second wait", waited)
	}
	payload := decode(t, result)
	if payload["running"] != true {
		t.Fatalf("a command still running came back finished: %v", payload)
	}
	if payload["output"] != "" {
		t.Fatalf("output = %v, want nothing: it had not printed anything", payload["output"])
	}
}

// A server that will not give a terminal still runs commands. Refusing a
// terminal is a real configuration (PermitTTY no), and the work does not depend
// on having one: only a program that insists on it is affected, and on such a
// server it could never have run at all.
func TestACommandRunsOnAServerThatRefusesATerminal(t *testing.T) {
	srv := startServer(t, serverOptions{
		Password:  "hunter2",
		RefusePty: true,
		Run:       func(string) commandRun { return commandRun{Stdout: "up 4 days\n"} },
	})
	h := newHandler(t, serverConfig(srv, Auth{Password: "hunter2"}))

	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 2}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if result.Failed() {
		t.Fatalf("a server that refused a terminal refused the work as well: %s", result.Content)
	}
	payload := decode(t, result)
	if output, _ := payload["output"].(string); !strings.Contains(output, "up 4 days") {
		t.Fatalf("output = %q, want the command's output", output)
	}
	if payload["running"] != false {
		t.Fatalf("the command did not finish: %v", payload)
	}
}

// Answering while repeating the command is not a mistake to punish.
//
// The verification-code instruction asks for exactly that shape ("the same call
// again, with the code in input"), so a model that has read it sends the
// command alongside the answer. Refusing that taught two different rules for one
// field, and cost a whole round trip to say so. Only a DIFFERENT command is
// genuinely ambiguous.
func TestAnsweringMayRepeatTheCommandItIsAnswering(t *testing.T) {
	srv := startServer(t, serverOptions{
		Password: "hunter2",
		Run: func(string) commandRun {
			return commandRun{Prompts: []Prompt{{Ask: "your name? ", Reply: "Sam"}}}
		},
	})
	h := newHandler(t, serverConfig(srv, Auth{Password: "hunter2"}))

	started, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 2}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if decode(t, started)["running"] != true {
		t.Fatalf("the program did not stop to ask: %s", started.Content)
	}

	// The command repeated with the answer: the caller naming what it is
	// answering, which is what our own instruction asks for elsewhere.
	answered, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Input: "Sam", Wait: 2}))
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if answered.Failed() {
		t.Fatalf("answering while naming the command was refused: %s", answered.Content)
	}
	if output, _ := decode(t, answered)["output"].(string); !strings.Contains(output, "done") {
		t.Fatalf("output = %q, want the program to have taken the answer", output)
	}
}

// A DIFFERENT command alongside an answer is the one that stays refused: it
// cannot be told from "stop that and run this", and guessing would type into a
// program the caller had stopped thinking about.
func TestADifferentCommandAlongsideAnAnswerIsRefused(t *testing.T) {
	srv := passwdServer(t)
	h := newHandler(t, interactiveConfig(srv, "passwd"))

	if _, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "passwd deploy", Wait: 2})); err != nil {
		t.Fatalf("start: %v", err)
	}
	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Input: "s3cret", Wait: 2}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if result.Err != tool.ErrorBadArguments {
		t.Fatalf("error kind = %q, want bad_arguments", result.Err)
	}
	if text, _ := decode(t, result)["error"].(string); !strings.Contains(text, "passwd deploy") {
		t.Fatalf("the refusal does not name what is running: %q", text)
	}
}

// Nothing has to be remembered between calls.
//
// An assistant that must recall what it left running will eventually not, and
// send a new command into a machine still busy with the last one. So every
// answer that comes back running NAMES what is running, and says the three
// calls that can follow. The decision left is only which one, never how.
func TestAnUnfinishedCommandNamesItselfAndWhatToDoNext(t *testing.T) {
	srv := startServer(t, serverOptions{
		Password: "hunter2",
		Run: func(string) commandRun {
			return commandRun{Prompts: []Prompt{{Ask: "continue? ", Reply: "yes"}}}
		},
	})
	h := newHandler(t, serverConfig(srv, Auth{Password: "hunter2"}))

	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 2}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	payload := decode(t, result)
	if payload["running_command"] != "uptime" {
		t.Fatalf("the answer does not name what is running: %v", payload["running_command"])
	}
	note, _ := payload["note"].(string)
	for _, choice := range []string{"input", "nothing", "stop"} {
		if !strings.Contains(note, choice) {
			t.Fatalf("the note does not offer %q as a next call: %q", choice, note)
		}
	}

	// A finished command names nothing, so "is something running" is answered by
	// the result rather than by remembering.
	answered, err := h.handle(context.Background(), callWith(t, CallArgs{Input: "yes", Wait: 2}))
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	done := decode(t, answered)
	if done["running"] != false {
		t.Fatalf("the command did not finish: %v", done)
	}
	if _, named := done["running_command"]; named {
		t.Fatalf("a finished command still names itself as running: %v", done)
	}
	if _, noted := done["note"]; noted {
		t.Fatalf("a finished command still tells the assistant what to do about it: %v", done)
	}
}

// A command that prints on a slow drip comes back ONCE, with all of it.
//
// ping -i 1, a build, any periodic command: quiet for a second between lines,
// and nowhere near finished. Treating that quiet as "done talking" made a
// fifteen-second command cost six calls, one to start it and five to watch it,
// each a round trip through the model. What separates it from a program waiting
// for an answer is not the pause, it is that a prompt stops MID-LINE and output
// does not.
func TestASlowDripComesBackOnceWithAllOfIt(t *testing.T) {
	srv := startServer(t, serverOptions{
		Password: "hunter2",
		Run: func(string) commandRun {
			return commandRun{Stdout: "line one\nline two\nline three\n", Drip: 250 * time.Millisecond}
		},
	})
	h := newHandler(t, serverConfig(srv, Auth{Password: "hunter2"}))

	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 2}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	payload := decode(t, result)
	if payload["running"] != false {
		t.Fatalf("a command that finished came back running, so the model has to poll for the rest: %v", payload)
	}
	output, _ := payload["output"].(string)
	for _, line := range []string{"line one", "line two", "line three"} {
		if !strings.Contains(output, line) {
			t.Fatalf("output = %q, want every line in the one answer", output)
		}
	}
}

// A program that stops to ask comes back with the question when the wait runs
// out. There is no rule that spots a question, deliberately: nothing on the wire
// says whether a program is waiting for a person, so what decides is the number
// the caller gave, and a caller expecting a question gives a small one.
func TestAQuestionComesBackWhenTheWaitRunsOut(t *testing.T) {
	srv := startServer(t, serverOptions{
		Password: "hunter2",
		Run: func(string) commandRun {
			return commandRun{Prompts: []Prompt{{Ask: "your name? ", Reply: "Sam"}}}
		},
	})
	h := newHandler(t, serverConfig(srv, Auth{Password: "hunter2"}))

	started := time.Now()
	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 1}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatalf("a one second wait took %s to come back", waited)
	}
	payload := decode(t, result)
	if payload["running"] != true {
		t.Fatalf("the program is waiting to be answered but came back finished: %v", payload)
	}
	if output, _ := payload["output"].(string); !strings.Contains(output, "your name?") {
		t.Fatalf("output = %q, want the question it stopped on", output)
	}
}

// The cap bounds one ANSWER, not one read of the buffer.
//
// collect() makes several takes and joins them, and each take was capped on its
// own, so a program printing faster than the poll interval handed back a
// multiple of the limit. This drives that case deliberately rather than waiting
// for the timing to happen: several bursts, each under the cap, summing to well
// over it.
func TestOutputIsCappedAcrossSeveralReads(t *testing.T) {
	const burst = 20 * 1024
	srv := startServer(t, serverOptions{
		Password: "hunter2",
		Run: func(string) commandRun {
			// Six lines of 20 KiB, dripped: each read is comfortably under the
			// 64 KiB cap and together they are nearly twice it, which is the
			// case a per-read cap does not catch.
			line := strings.Repeat("x", burst) + "\n"
			return commandRun{Stdout: strings.Repeat(line, 6), Drip: 50 * time.Millisecond}
		},
	})
	h := newHandler(t, serverConfig(srv, Auth{Password: "hunter2"}))

	result, err := h.handle(context.Background(), callWith(t, CallArgs{Command: "uptime", Wait: 3}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	payload := decode(t, result)
	kept, _ := payload["output"].(string)
	if len(kept) > sessionOutputLimit {
		t.Fatalf("kept %d bytes across several reads, want no more than %d", len(kept), sessionOutputLimit)
	}
	if payload["output_dropped"] != true {
		t.Fatalf("the result does not say it was cut short: %v", payload)
	}
}
