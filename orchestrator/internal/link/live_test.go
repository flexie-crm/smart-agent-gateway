package link

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// The two halves, against each other.
//
// Everything else in this package tests the server against a stand-in written
// here, in Go, from the same understanding of the protocol as the server. That
// proves the understanding is self-consistent and nothing more. The half that
// actually ships is Rust, on a different websocket library, and what has to
// agree between them is exactly what a shared assumption cannot check:
//
//   - whether an EMPTY binary message survives as a message rather than being
//     elided, which is the whole of half-close and therefore of a request that
//     finishes before its answer begins;
//   - whether a refusal written by one side parses on the other;
//   - whether a dial-back arrives at the ticket it was issued for.
//
// So this drives the REAL client: cargo builds the example, this starts it
// against a real server, and the assertions are made from here where the test
// helpers already are.
//
//	make link-e2e
//
// Deliberately outside `make ci`, for the reason `make node-ci` is: it needs a
// Rust toolchain, and not every machine that runs the Go suite has one.

func TestTheRustClientAndThisServerAgree(t *testing.T) {
	if os.Getenv("SAG_LINK_E2E") == "" {
		t.Skip("SAG_LINK_E2E not set; skipping the cross-language link gate (make link-e2e)")
	}

	// A service only the "machine" can see. Two behaviours, chosen by the port
	// the gateway asks for, because both are worth proving through the real
	// client: an echo, and one that answers only once the request has ended.
	echoHost, echoPort := liveEcho(t)
	answerHost, answerPort, answer := liveAnswerAfterEOF(t, 512*1024)

	r := NewRegistry(zerolog.Nop(), func(token string) (int64, int64, string, time.Time, bool) {
		if token == "a-real-looking-token" {
			return 11, 5, "the-laptop", time.Now().Add(time.Hour), true
		}
		return 0, 0, "", time.Now().Add(time.Hour), false
	}, []string{"*"})

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/link", r.ServeControl)
	mux.HandleFunc("/v1/link/stream", r.ServeStream)

	// Our own listener, on a port we keep, because one of these tests is a
	// gateway restart: the server has to go away and come back at the same
	// address, which is what a deploy does and what the client has to survive.
	gateway := newRestartableServer(t, mux)
	defer gateway.stop()

	client := startRustClient(t, gateway.url, "a-real-looking-token")
	defer client.stop()

	waitFor(t, "the real client to link", func() bool { return r.Online(5, 11, "the-laptop") })

	// 1. Bytes, both ways, through the client that ships.
	t.Run("bytes pass through the real client", func(t *testing.T) {
		conn, err := r.Dial(context.Background(), 5, 11, "the-laptop", echoHost, echoPort, "a test")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.Write([]byte("select 1")); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := make([]byte, 8)
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != "select 1" {
			t.Fatalf("got %q", got)
		}
	})

	// 2. The one a Go stand-in cannot prove: an empty binary message, written by
	// this server's library and read by the client's, meaning "I have finished
	// sending" rather than being dropped or read as a close.
	t.Run("a request may finish before its answer begins", func(t *testing.T) {
		conn, err := r.Dial(context.Background(), 5, 11, "the-laptop", answerHost, answerPort, "a test")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer func() { _ = conn.Close() }()

		if _, err := conn.Write([]byte("the whole request")); err != nil {
			t.Fatalf("write: %v", err)
		}
		half, ok := conn.(interface{ CloseWrite() error })
		if !ok {
			t.Fatal("a carried connection cannot say it has finished sending")
		}
		if err := half.CloseWrite(); err != nil {
			t.Fatalf("finish sending: %v", err)
		}

		got, err := io.ReadAll(conn)
		if err != nil {
			t.Fatalf("read the answer: %v", err)
		}
		if len(got) != len(answer) {
			t.Fatalf("the answer was cut off: %d bytes of %d", len(got), len(answer))
		}
		for i := range got {
			if got[i] != answer[i] {
				t.Fatalf("the answer differs at byte %d", i)
			}
		}
	})

	// 3. Many at once, each its own bytes, through one client.
	t.Run("connections do not mix", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				conn, err := r.Dial(context.Background(), 5, 11, "the-laptop", echoHost, echoPort, "a test")
				if err != nil {
					t.Errorf("dial %d: %v", n, err)
					return
				}
				defer func() { _ = conn.Close() }()
				mine := []byte("connection-" + strconv.Itoa(n) + "-" + strconv.Itoa(n*7919))
				if _, err := conn.Write(mine); err != nil {
					t.Errorf("write %d: %v", n, err)
					return
				}
				back := make([]byte, len(mine))
				if _, err := io.ReadFull(conn, back); err != nil {
					t.Errorf("read %d: %v", n, err)
					return
				}
				if string(back) != string(mine) {
					t.Errorf("connection %d was given %q, not its own %q", n, back, mine)
				}
			}(i)
		}
		wg.Wait()
	})

	// 4. A refusal written by the client, read here: the person's own machine
	// saying why, rather than this side waiting out a deadline.
	t.Run("a refusal crosses the wire with its reason", func(t *testing.T) {
		closed := freePort(t) // nothing is listening there
		start := time.Now()
		_, err := r.Dial(context.Background(), 5, 11, "the-laptop", "127.0.0.1", closed, "a test")
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("got %v after %s", err, time.Since(start))
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("the refusal took %s, which is a timeout wearing a reason", time.Since(start))
		}
		if !strings.Contains(strings.ToLower(err.Error()), "refused") {
			t.Fatalf("the reason did not survive: %v", err)
		}
	})

	// 5. A tool that runs on the machine, answered by the machine.
	//
	// The other half of what this link is for. A connection ends at something
	// on the person's network; a call ends at the application itself, which
	// does the work and answers. Same ticket, same identity check, same
	// cleanup: one mechanism, two far ends.
	t.Run("a tool runs on the machine and answers", func(t *testing.T) {
		// What the machine says it can do, declared when it linked.
		runs := r.Runs(5, 11, "the-laptop")
		if runs["machine_info"] == 0 {
			t.Fatalf("the application did not declare the tool it ships: %+v", runs)
		}

		answer, err := r.Call(context.Background(), 5, 11, "the-laptop",
			"machine_info", []byte(`{}`), "a test")
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !answer.OK {
			t.Fatalf("the machine refused: %s (%s)", answer.Message, answer.Kind)
		}
		var content struct {
			OperatingSystem string `json:"operating_system"`
			Architecture    string `json:"architecture"`
		}
		if err := json.Unmarshal(answer.Content, &content); err != nil {
			t.Fatalf("the answer could not be read: %v (%s)", err, answer.Content)
		}
		// The name a person uses, not the kernel's. Rust says "macos" where Go
		// says "darwin", and the tool reports the former deliberately: what
		// reads this is a model deciding how to talk about somebody's computer,
		// and "darwin" is an answer only an engineer would recognise.
		expected := map[string]string{"darwin": "macos"}[runtime.GOOS]
		if expected == "" {
			expected = runtime.GOOS
		}
		if content.OperatingSystem != expected {
			t.Fatalf("the machine says it runs %q; this one is %q", content.OperatingSystem, expected)
		}
		if content.Architecture == "" {
			t.Fatal("the answer is missing what the machine is")
		}
	})

	// A terminal on the machine, running a real command in a real folder.
	//
	// The whole point of the thing, end to end: the gateway asks, the
	// application runs it where the person said it may, and what it printed
	// comes back. Nothing here is scripted; a shell really runs.
	t.Run("a command runs on the machine, in the chosen folder", func(t *testing.T) {
		if runs := r.Runs(5, 11, "the-laptop"); runs["terminal"] == 0 {
			t.Skip("this build of the application has no terminal")
		}
		// The folder the application was told to work in, set for this test in
		// the environment it was started with.
		answer, err := r.Call(context.Background(), 5, 11, "the-laptop",
			"terminal", []byte(`{"command":"echo hello && pwd"}`), "a test")
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !answer.OK {
			t.Fatalf("the terminal refused: %s (%s)", answer.Message, answer.Kind)
		}
		var result struct {
			ExitCode  *int   `json:"exit_code"`
			Output    string `json:"output"`
			Directory string `json:"directory"`
		}
		if err := json.Unmarshal(answer.Content, &result); err != nil {
			t.Fatalf("the answer could not be read: %v (%s)", err, answer.Content)
		}
		if result.ExitCode == nil || *result.ExitCode != 0 {
			t.Fatalf("the command failed: %+v", result)
		}
		if !strings.Contains(result.Output, "hello") {
			t.Fatalf("what it printed was %q", result.Output)
		}
		// And it ran WHERE it was allowed to, not wherever the application
		// happened to start.
		if !strings.Contains(result.Output, result.Directory) {
			t.Fatalf("it ran in %q but printed %q", result.Directory, result.Output)
		}
	})

	// An absolute path reaches what it names.
	//
	// The chosen folder is where a command STARTS, not a fence around it: the
	// tool runs as the person, on their own computer, and they could type this
	// themselves. A jail here would be a pretence, since what actually bounds
	// the tool is the account it runs as and the commands the policy permits.
	t.Run("an absolute path reaches what it names", func(t *testing.T) {
		if runs := r.Runs(5, 11, "the-laptop"); runs["terminal"] == 0 {
			t.Skip("this build of the application has no terminal")
		}
		elsewhere := t.TempDir() // nowhere near the folder the client was given
		answer, err := r.Call(context.Background(), 5, 11, "the-laptop",
			"terminal", []byte(`{"command":"pwd","directory":`+quoted(elsewhere)+`}`), "a test")
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !answer.OK {
			t.Fatalf("an absolute path was refused: %s", answer.Message)
		}
		var result struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal(answer.Content, &result); err != nil {
			t.Fatalf("the answer could not be read: %v", err)
		}
		// Resolved, so a symlinked temporary directory compares as itself.
		if !strings.Contains(result.Output, filepath.Base(elsewhere)) {
			t.Fatalf("it ran somewhere else: %q, asked for %q", result.Output, elsewhere)
		}
	})

	// The link is down for a MINUTE, and the work still comes back.
	//
	// Written against what actually happens rather than against what is
	// convenient to simulate. A link went down here for eighty five seconds
	// while a credential was renewed, and the answer to every call in that
	// window was "your computer is not connected" about a computer that was
	// sitting there running the command. A minute is the reality this has to
	// survive, so a minute is what it is tested with.
	t.Run("the work comes back after a minute with no link", func(t *testing.T) {
		if testing.Short() {
			t.Skip("this one takes over a minute on purpose")
		}
		if runs := r.Runs(5, 11, "the-laptop"); runs["terminal"] == 0 {
			t.Skip("this build of the application has no terminal")
		}
		marks := filepath.Join(t.TempDir(), "ran.txt")

		// Cut it, and keep cutting it for a minute: every time the application
		// reconnects it is dropped again, which is what a network that is
		// having a bad minute looks like from here.
		stopCutting := make(chan struct{})
		go func() {
			deadline := time.Now().Add(60 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case <-stopCutting:
					return
				case <-time.After(2 * time.Second):
				}
				r.mu.Lock()
				m, linked := r.machines[key{5, 11, "the-laptop"}]
				r.mu.Unlock()
				if linked {
					m.drop()
				}
			}
			close(stopCutting)
		}()

		started := time.Now()
		answer, err := r.Call(context.Background(), 5, 11, "the-laptop", "terminal",
			// Longer than the outage on purpose: a command that finishes during
			// a gap in the cutting proves nothing about surviving one.
			[]byte(`{"command":"sleep 70; echo ran >> `+marks+`; echo finished","wait":180,"conversation":99}`),
			"a test")
		if err != nil {
			t.Fatalf("a call did not survive a minute without a link: %v", err)
		}
		if !answer.OK {
			t.Fatalf("the terminal refused: %s", answer.Message)
		}
		var content struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal(answer.Content, &content); err != nil {
			t.Fatalf("the answer could not be read: %v", err)
		}
		if !strings.Contains(content.Output, "finished") {
			t.Fatalf("the answer came back without the work in it: %+v", content)
		}
		if took := time.Since(started); took < 60*time.Second {
			t.Fatalf("it answered in %s, before the outage was over, so this proves less than it claims", took)
		}
		// And the command ran ONCE, through all of that.
		ran, err := os.ReadFile(marks)
		if err != nil {
			t.Fatalf("the command did not run: %v", err)
		}
		if count := strings.Count(string(ran), "ran"); count != 1 {
			t.Fatalf("the command ran %d times across the outage", count)
		}
	})

	// The application is QUIT while a command is running, and reopened.
	//
	// This is the interruption that happens most: somebody closes the chat
	// application, or a new version is installed over it. The work dies with
	// the process, and the honest answer is to say so rather than to hang, or
	// to pretend the command finished.
	t.Run("quitting the application mid-command is an answer, not a hang", func(t *testing.T) {
		if runs := r.Runs(5, 11, "the-laptop"); runs["terminal"] == 0 {
			t.Skip("this build of the application has no terminal")
		}
		go func() {
			time.Sleep(time.Second)
			client.quit()
			time.Sleep(2 * time.Second)
			client.reopen() // the person opens it again
		}()

		started := time.Now()
		answer, err := r.Call(context.Background(), 5, 11, "the-laptop", "terminal",
			[]byte(`{"command":"sleep 20; echo never","wait":60,"conversation":100}`), "a test")
		took := time.Since(started)

		// The work died with the process, so the honest answer is that it was
		// interrupted. What it must NEVER do is run the command AGAIN: for a
		// terminal that is a deploy that happens twice, or a directory removed
		// after somebody recreated it. This is what the first version of the
		// resume did, and this test is what found it.
		if err == nil && answer.OK {
			var content struct {
				Output string `json:"output"`
			}
			_ = json.Unmarshal(answer.Content, &content)
			t.Fatalf("a command that died with the application came back as a success: %+v", content)
		}
		if err == nil && !answer.OK && !strings.Contains(answer.Message, "interrupted") {
			t.Fatalf("the answer does not say what happened: %s", answer.Message)
		}
		if took > comesBackWithin+30*time.Second {
			t.Fatalf("it took %s to answer, which is a hang rather than an answer", took)
		}

		// And the application that came back works: the next call goes through.
		waitUntil(t, func() bool { return r.Online(5, 11, "the-laptop") })
		after, err := r.Call(context.Background(), 5, 11, "the-laptop", "terminal",
			[]byte(`{"command":"echo back","wait":30,"conversation":101}`), "a test")
		if err != nil || !after.OK {
			t.Fatalf("the reopened application does not work: %v %s", err, after.Message)
		}
	})

	// A call SURVIVES the socket carrying it.
	//
	// The failure this exists for: a link drops for a reason that has nothing
	// to do with the work (a network moves, a laptop sleeps, a credential is
	// renewed), and a command that has been installing for four minutes is
	// lost, with the person told their computer is not connected while it sits
	// in front of them.
	//
	// The command keeps running over there. The call has an id, the gateway
	// waits for the application to come back and asks again with the same id,
	// and the machine answers the work already going instead of starting it
	// again. Proved against the REAL client, and by counting: a command that
	// appends to a file once would say if it had run twice.
	t.Run("a call survives the link dropping under it", func(t *testing.T) {
		runs := r.Runs(5, 11, "the-laptop")
		if runs["terminal"] == 0 {
			t.Skip("this build of the application has no terminal")
		}
		marks := filepath.Join(t.TempDir(), "ran.txt")

		// The link is cut a second in, while the command is still going. Cut
		// the way production cuts one: the control socket is closed, which is
		// what a failed keepalive or a renewed credential does. The client is
		// left running and reconnects by itself, as it would.
		go func() {
			time.Sleep(time.Second)
			cutTheLink(t, r, key{5, 11, "the-laptop"})
		}()

		started := time.Now()
		answer, err := r.Call(context.Background(), 5, 11, "the-laptop", "terminal",
			[]byte(`{"command":"sleep 6; echo ran >> `+marks+`; echo done","wait":60,"conversation":96}`),
			"a test")
		if err != nil {
			t.Fatalf("a call whose link dropped was lost: %v", err)
		}
		if !answer.OK {
			t.Fatalf("the terminal refused: %s", answer.Message)
		}
		var content struct {
			Output  string `json:"output"`
			Running bool   `json:"running"`
		}
		if err := json.Unmarshal(answer.Content, &content); err != nil {
			t.Fatalf("the answer could not be read: %v", err)
		}
		if !strings.Contains(content.Output, "done") || content.Running {
			t.Fatalf("the answer did not survive the drop: %+v", content)
		}
		if took := time.Since(started); took < 6*time.Second {
			t.Fatalf("it answered in %s, before the command could have finished", took)
		}

		// And exactly once. A resumed call that RE-RAN the command would be
		// worse than a lost one: an install that half happened twice.
		ran, err := os.ReadFile(marks)
		if err != nil {
			t.Fatalf("the command did not run at all: %v", err)
		}
		if lines := strings.Count(string(ran), "ran"); lines != 1 {
			t.Fatalf("the command ran %d times; a resume must not repeat it", lines)
		}
	})

	// A call longer than any watchdog survives, against the REAL client.
	//
	// This is the regression that shipped: a call was kept alive by PINGING its
	// stream, and the application does not poll a call's socket while it is
	// running the tool. The ping went unanswered, the gateway concluded the
	// machine was gone, and every command over about forty seconds died: which
	// is every command anybody wanted a terminal for. It passed its unit test,
	// because the fake application in that test is a Go program that answers
	// pings whatever else it is doing.
	//
	// Forty five seconds, deliberately: longer than the watchdog that killed
	// it, and long enough that a new one would have to be very patient to hide.
	t.Run("a call outliving any watchdog comes back", func(t *testing.T) {
		if testing.Short() {
			t.Skip("this one takes 45 seconds on purpose")
		}
		if runs := r.Runs(5, 11, "the-laptop"); runs["terminal"] == 0 {
			t.Skip("this build of the application has no terminal")
		}
		started := time.Now()
		answer, err := r.Call(context.Background(), 5, 11, "the-laptop", "terminal",
			[]byte(`{"command":"sleep 45; echo survived","wait":90,"conversation":95}`), "a test")
		if err != nil {
			t.Fatalf("a call that took 45 seconds was cut off: %v", err)
		}
		if !answer.OK {
			t.Fatalf("the terminal refused: %s", answer.Message)
		}
		var content struct {
			Output  string `json:"output"`
			Running bool   `json:"running"`
		}
		if err := json.Unmarshal(answer.Content, &content); err != nil {
			t.Fatalf("the answer could not be read: %v", err)
		}
		if !strings.Contains(content.Output, "survived") || content.Running {
			t.Fatalf("it did not run to the end: %+v", content)
		}
		if took := time.Since(started); took < 45*time.Second {
			t.Fatalf("it answered in %s, which is before the command could have finished", took)
		}
	})

	// What the panel is told to show must be what the tools actually answer.
	//
	// Every defect in this area has been in that seam: a display declaration
	// written in Go against a payload built in Rust, each correct on its own.
	// A test with a fixture I wrote cannot catch the two drifting, because I
	// would write the fixture from the same belief that produced the bug. This
	// asks the REAL tools and checks the REAL answers against the declaration
	// that will be applied to them.
	t.Run("the tools answer with the fields the panel is told to show", func(t *testing.T) {
		runs := r.Runs(5, 11, "the-laptop")
		for _, name := range []string{"terminal", "read_file", "write_file", "find_files", "search_files"} {
			if runs[name] == 0 {
				t.Skipf("this build of the application has no %s", name)
			}
		}
		ask := func(tool, args string) map[string]json.RawMessage {
			t.Helper()
			answer, err := r.Call(context.Background(), 5, 11, "the-laptop", tool, []byte(args), "a test")
			if err != nil || !answer.OK {
				t.Fatalf("%s: %v %s", tool, err, answer.Message)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(answer.Content, &fields); err != nil {
				t.Fatalf("%s answered with something that is not an object: %v", tool, err)
			}
			return fields
		}
		has := func(tool string, fields map[string]json.RawMessage, named ...string) {
			t.Helper()
			for _, want := range named {
				if _, there := fields[want]; !there {
					have := make([]string, 0, len(fields))
					for name := range fields {
						have = append(have, name)
					}
					sort.Strings(have)
					t.Errorf("the panel shows %s's %q, which it does not answer with. It answers: %v",
						tool, want, have)
				}
			}
		}

		// Each list below is what that tool's tool.Display names, and the
		// point is that these strings live in two languages.
		has("write_file", ask("write_file", `{"path":"panel.txt","content":"hello","conversation":77}`),
			"path", "created", "bytes", "hash")
		has("read_file", ask("read_file", `{"path":"panel.txt","conversation":77}`),
			"path", "content", "more", "hash")
		has("find_files", ask("find_files", `{"pattern":"**/panel.txt"}`),
			"files", "count", "more")
		has("search_files", ask("search_files", `{"pattern":"hello","glob":"panel.txt"}`),
			"matches", "count", "more")
		has("terminal", ask("terminal", `{"command":"echo panel","wait":10,"conversation":77}`),
			"output", "running", "exit_code", "directory")
		// edit_file last, since it changes the file the others read.
		has("edit_file", ask("edit_file", `{"path":"panel.txt","find":"hello","replace":"goodbye","conversation":77}`),
			"path", "replacements", "hash", "lines")
	})

	// Two terminals in one conversation, working at the same time.
	//
	// Reported from use: parallel checks collided because one long command
	// blocked everything else. Proved here through the REAL client, and by the
	// clock: if they queued, the second would answer after the first, and the
	// whole thing would take as long as both.
	t.Run("two terminals in one conversation work at once", func(t *testing.T) {
		if runs := r.Runs(5, 11, "the-laptop"); runs["terminal"] == 0 {
			t.Skip("this build of the application has no terminal")
		}
		ask := func(args string) map[string]any {
			t.Helper()
			answer, err := r.Call(context.Background(), 5, 11, "the-laptop", "terminal", []byte(args), "a test")
			if err != nil || !answer.OK {
				t.Fatalf("terminal: %v %s", err, answer.Message)
			}
			var content map[string]any
			_ = json.Unmarshal(answer.Content, &content)
			return content
		}

		// One slow, left going.
		slow := ask(`{"command":"sleep 5; echo built","session":"build","wait":1,"conversation":98}`)
		if slow["running"] != true {
			t.Fatalf("the slow one should still be going: %+v", slow)
		}

		// The other answers at once, rather than waiting for it.
		started := time.Now()
		quick := ask(`{"command":"echo tested","session":"tests","wait":20,"conversation":98}`)
		took := time.Since(started)
		if quick["running"] != false {
			t.Fatalf("the second terminal was blocked by the first: %+v", quick)
		}
		if took > 3*time.Second {
			t.Fatalf("the second terminal waited %s for the first, so they are not independent", took)
		}

		// And one answer says what both are doing.
		state := ask(`{"status":true,"conversation":98}`)
		terminals, _ := state["terminals"].([]any)
		if len(terminals) != 2 {
			t.Fatalf("status reported %d terminals, want 2: %+v", len(terminals), state)
		}
		var building bool
		for _, one := range terminals {
			each, _ := one.(map[string]any)
			if each["session"] == "build" && each["running"] == true {
				building = true
			}
		}
		if !building {
			t.Fatalf("status does not say the build is still going: %+v", state)
		}

		ask(`{"stop":true,"session":"build","conversation":98}`)
		ask(`{"stop":true,"session":"tests","conversation":98}`)
	})

	// Patching a file, end to end: the hash, the line range, and a file whose
	// lines end the Windows way.
	//
	// Each of these was reported from use as fragile, and each is the kind of
	// thing that works in a unit test and fails across the wire, because what
	// crosses the wire is JSON with escaping in it: a carriage return and a
	// newline survive that trip or they do not.
	t.Run("a file is patched by hash, by line, and through Windows line endings", func(t *testing.T) {
		runs := r.Runs(5, 11, "the-laptop")
		if runs["edit_file"] == 0 || runs["read_file"] == 0 || runs["write_file"] == 0 {
			t.Skip("this build of the application has no file tools")
		}
		ask := func(tool, args string) (map[string]any, string) {
			t.Helper()
			answer, err := r.Call(context.Background(), 5, 11, "the-laptop", tool, []byte(args), "a test")
			if err != nil {
				t.Fatalf("%s: %v", tool, err)
			}
			var content map[string]any
			if len(answer.Content) > 0 {
				_ = json.Unmarshal(answer.Content, &content)
			}
			if !answer.OK {
				return content, answer.Message
			}
			return content, ""
		}

		// A file with WINDOWS line endings, written through the wire.
		if _, refusal := ask("write_file",
			`{"path":"win.txt","content":"const a = 1;\r\nconst b = 2;\r\nconst c = 3;\r\n","conversation":97}`); refusal != "" {
			t.Fatalf("write: %s", refusal)
		}
		read, refusal := ask("read_file", `{"path":"win.txt","conversation":97}`)
		if refusal != "" {
			t.Fatalf("read: %s", refusal)
		}
		hash, _ := read["hash"].(string)
		if hash == "" {
			t.Fatal("a read gave no hash, so nothing can be written against a version")
		}

		// Text copied from a model, with plain newlines, against a file whose
		// lines end the other way.
		edited, refusal := ask("edit_file",
			`{"path":"win.txt","find":"const b = 2;","replace":"const b = 22;","expect_hash":"`+hash+`","conversation":97}`)
		if refusal != "" {
			t.Fatalf("an edit failed on a file with Windows line endings: %s", refusal)
		}
		after, _ := edited["hash"].(string)
		if after == "" || after == hash {
			t.Fatalf("the edit did not answer with the file's new version: %+v", edited)
		}

		// The OLD hash must now be refused, which is the whole point of it.
		if _, refusal := ask("edit_file",
			`{"path":"win.txt","find":"const c = 3;","replace":"const c = 33;","expect_hash":"`+hash+`","conversation":97}`); refusal == "" {
			t.Fatal("an edit written against the old version was applied")
		} else if !strings.Contains(refusal, "has changed since") {
			t.Fatalf("the refusal does not say the file moved on: %s", refusal)
		}

		// By line number, with no text matching at all.
		if _, refusal := ask("edit_file",
			`{"path":"win.txt","start_line":1,"end_line":1,"replace":"const a = 11;","conversation":97}`); refusal != "" {
			t.Fatalf("a line edit failed: %s", refusal)
		}
		final, refusal := ask("read_file", `{"path":"win.txt","conversation":97}`)
		if refusal != "" {
			t.Fatalf("read: %s", refusal)
		}
		content, _ := final["content"].(string)
		for _, want := range []string{"const a = 11;", "const b = 22;", "const c = 3;"} {
			if !strings.Contains(content, want) {
				t.Fatalf("the file lost %q: %q", want, content)
			}
		}
		// And it is still a Windows file. Asked of the DISK rather than of
		// read_file, which hands back text with one kind of line ending on
		// purpose: a model copying from it should not have to carry a
		// character it cannot see. Whether the file kept its own is a question
		// only the bytes can answer.
		if runs["terminal"] == 0 {
			return
		}
		shown, err := r.Call(context.Background(), 5, 11, "the-laptop", "terminal",
			[]byte(`{"command":"cat -e win.txt","wait":20,"conversation":97}`), "a test")
		if err != nil || !shown.OK {
			t.Fatalf("looking at the file: %v %s", err, shown.Message)
		}
		var printed struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal(shown.Content, &printed); err != nil {
			t.Fatalf("the answer could not be read: %v", err)
		}
		// cat -e shows a carriage return as ^M. Three lines, three of them.
		if carriage := strings.Count(printed.Output, "^M$"); carriage != 3 {
			t.Fatalf("the file's Windows line endings were not kept: %d of 3 lines have one:\n%s",
				carriage, printed.Output)
		}
	})

	// The terminal REMEMBERS, end to end.
	//
	// This is the whole reason it stopped being a one-shot: what one command
	// changes about the shell, the next one sees. Proved through the real
	// client, because the memory lives on that side and a Go stand-in agreeing
	// with a Go server would prove nothing about it.
	t.Run("the terminal remembers between calls", func(t *testing.T) {
		if runs := r.Runs(5, 11, "the-laptop"); runs["terminal"] == 0 {
			t.Skip("this build of the application has no terminal")
		}
		ask := func(args string) map[string]any {
			t.Helper()
			answer, err := r.Call(context.Background(), 5, 11, "the-laptop", "terminal", []byte(args), "a test")
			if err != nil {
				t.Fatalf("terminal: %v", err)
			}
			if !answer.OK {
				t.Fatalf("the terminal refused: %s (%s)", answer.Message, answer.Kind)
			}
			var content map[string]any
			if err := json.Unmarshal(answer.Content, &content); err != nil {
				t.Fatalf("the answer could not be read: %v (%s)", err, answer.Content)
			}
			return content
		}

		ask(`{"command":"export SAG_LIVE_CHECK=carried","conversation":91}`)
		said := ask(`{"command":"echo $SAG_LIVE_CHECK","conversation":91}`)
		if output, _ := said["output"].(string); !strings.Contains(output, "carried") {
			t.Fatalf("the terminal forgot a variable between calls: %q", output)
		}

		// And another conversation has its own, which is what keeps two chats
		// from typing into one terminal.
		other := ask(`{"command":"echo [$SAG_LIVE_CHECK]","conversation":92}`)
		if output, _ := other["output"].(string); strings.Contains(output, "carried") {
			t.Fatalf("one conversation's terminal saw another's: %q", output)
		}
	})

	// Something slower than the wait comes back with what it has, and says it
	// is still going. The ninety second ceiling that used to kill an install is
	// gone: what bounds a call now is asking for more, or not.
	t.Run("something slow comes back and keeps going", func(t *testing.T) {
		if runs := r.Runs(5, 11, "the-laptop"); runs["terminal"] == 0 {
			t.Skip("this build of the application has no terminal")
		}
		ask := func(args string) map[string]any {
			t.Helper()
			answer, err := r.Call(context.Background(), 5, 11, "the-laptop", "terminal", []byte(args), "a test")
			if err != nil {
				t.Fatalf("terminal: %v", err)
			}
			var content map[string]any
			_ = json.Unmarshal(answer.Content, &content)
			return content
		}

		going := ask(`{"command":"echo starting; sleep 2; echo finished","wait":1,"conversation":93}`)
		if going["running"] != true {
			t.Fatalf("a two second command was not still going after one: %+v", going)
		}
		if output, _ := going["output"].(string); !strings.Contains(output, "starting") {
			t.Fatalf("what it had printed so far did not come back: %q", output)
		}
		rest := ask(`{"wait":15,"conversation":93}`)
		if rest["running"] != false {
			t.Fatalf("it never finished: %+v", rest)
		}
		if output, _ := rest["output"].(string); !strings.Contains(output, "finished") {
			t.Fatalf("the rest of it did not come back: %q", output)
		}
	})

	// The file tools, end to end, in the order somebody uses them: write a
	// file, find it, read it back, change part of it, search for the change.
	//
	// One test rather than five, because what is being proved is that the
	// chain holds against the REAL client on a REAL disk: five tests each
	// setting up their own file would prove the same thing five times and
	// never that an edit lands where a read looked.
	t.Run("files are written, found, read, changed and searched", func(t *testing.T) {
		runs := r.Runs(5, 11, "the-laptop")
		for _, name := range []string{"write_file", "find_files", "read_file", "edit_file", "search_files"} {
			if runs[name] == 0 {
				t.Skipf("this build of the application has no %s", name)
			}
		}
		ask := func(tool, args string) json.RawMessage {
			t.Helper()
			answer, err := r.Call(context.Background(), 5, 11, "the-laptop", tool, []byte(args), "a test")
			if err != nil {
				t.Fatalf("%s: %v", tool, err)
			}
			if !answer.OK {
				t.Fatalf("%s refused: %s (%s)", tool, answer.Message, answer.Kind)
			}
			return answer.Content
		}

		// Written into the folder the application was told to work in.
		var written struct {
			Path    string `json:"path"`
			Created bool   `json:"created"`
		}
		if err := json.Unmarshal(ask("write_file",
			`{"path":"notes/plan.md","content":"# Plan\n\nfirst step\nsecond step\n"}`),
			&written); err != nil {
			t.Fatalf("the write could not be read: %v", err)
		}
		if !written.Created {
			t.Fatalf("the file was not created: %+v", written)
		}

		// Found by name, without being told where it is.
		var found struct {
			Files []string `json:"files"`
		}
		if err := json.Unmarshal(ask("find_files", `{"pattern":"**/plan.md"}`), &found); err != nil {
			t.Fatalf("the listing could not be read: %v", err)
		}
		if len(found.Files) == 0 {
			t.Fatal("the file that was just written was not found")
		}

		// Read back, whole.
		var read struct {
			Content    string `json:"content"`
			TotalLines int    `json:"total_lines"`
		}
		if err := json.Unmarshal(ask("read_file", `{"path":"notes/plan.md"}`), &read); err != nil {
			t.Fatalf("the read could not be read: %v", err)
		}
		if !strings.Contains(read.Content, "first step") || read.TotalLines != 4 {
			t.Fatalf("what came back was %q (%d lines)", read.Content, read.TotalLines)
		}

		// Changed by exact text, which is what the read just gave us.
		var edited struct {
			Replacements int `json:"replacements"`
		}
		if err := json.Unmarshal(ask("edit_file",
			`{"path":"notes/plan.md","find":"second step","replace":"second step, revised"}`),
			&edited); err != nil {
			t.Fatalf("the edit could not be read: %v", err)
		}
		if edited.Replacements != 1 {
			t.Fatalf("the edit replaced %d things", edited.Replacements)
		}

		// And the change is really on the disk, found by searching for it.
		var searched struct {
			Matches []struct {
				File string `json:"file"`
				Line int    `json:"line"`
				Text string `json:"text"`
			} `json:"matches"`
		}
		if err := json.Unmarshal(ask("search_files", `{"pattern":"second step, revised"}`), &searched); err != nil {
			t.Fatalf("the search could not be read: %v", err)
		}
		if len(searched.Matches) != 1 {
			t.Fatalf("the search found %d of the edit", len(searched.Matches))
		}
		if searched.Matches[0].Line != 4 || !strings.Contains(searched.Matches[0].File, "plan.md") {
			t.Fatalf("the search found it in the wrong place: %+v", searched.Matches[0])
		}
	})

	// Nothing is overwritten blind, end to end: the gateway carries which
	// conversation is asking and the machine holds the memory, so this is the
	// one rule whose two halves are on different computers.
	t.Run("a file is not replaced unless this conversation has read it", func(t *testing.T) {
		runs := r.Runs(5, 11, "the-laptop")
		if runs["write_file"] == 0 || runs["read_file"] == 0 {
			t.Skip("this build of the application has no file tools")
		}
		ask := func(tool, args string) (Result, error) {
			return r.Call(context.Background(), 5, 11, "the-laptop", tool, []byte(args), "a test")
		}
		// Made in one conversation.
		if _, err := ask("write_file", `{"path":"theirs.txt","content":"work somebody did","conversation":41}`); err != nil {
			t.Fatalf("write: %v", err)
		}
		// Another conversation may not replace it without looking.
		blind, err := ask("write_file", `{"path":"theirs.txt","content":"mine","conversation":42}`)
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if blind.OK {
			t.Fatal("a file this conversation had never read was overwritten")
		}
		if blind.Kind != "bad_arguments" {
			t.Fatalf("a refusal the assistant can correct came back as %q", blind.Kind)
		}
		// Having read it, it may.
		if answer, err := ask("read_file", `{"path":"theirs.txt","conversation":42}`); err != nil || !answer.OK {
			t.Fatalf("read: %v %+v", err, answer)
		}
		after, err := ask("write_file", `{"path":"theirs.txt","content":"mine","conversation":42}`)
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if !after.OK {
			t.Fatalf("a file this conversation HAD read was refused: %s", after.Message)
		}
	})

	// An edit that could mean four things is refused rather than guessed at,
	// and the file is left exactly as it was.
	t.Run("an edit that is not unique changes nothing", func(t *testing.T) {
		runs := r.Runs(5, 11, "the-laptop")
		if runs["write_file"] == 0 || runs["edit_file"] == 0 || runs["read_file"] == 0 {
			t.Skip("this build of the application has no file tools")
		}
		call := func(tool, args string) (Result, error) {
			return r.Call(context.Background(), 5, 11, "the-laptop", tool, []byte(args), "a test")
		}
		if _, err := call("write_file", `{"path":"repeats.txt","content":"item\nitem\nitem\n"}`); err != nil {
			t.Fatalf("write: %v", err)
		}
		answer, err := call("edit_file", `{"path":"repeats.txt","find":"item","replace":"thing"}`)
		if err != nil {
			t.Fatalf("edit: %v", err)
		}
		if answer.OK {
			t.Fatal("an edit matching three lines was applied")
		}
		if answer.Kind != "bad_arguments" {
			t.Fatalf("a refusal the assistant can correct came back as %q", answer.Kind)
		}
		back, err := call("read_file", `{"path":"repeats.txt"}`)
		if err != nil || !back.OK {
			t.Fatalf("read back: %v %+v", err, back)
		}
		var read struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(back.Content, &read); err != nil {
			t.Fatalf("the read could not be read: %v", err)
		}
		if read.Content != "item\nitem\nitem" {
			t.Fatalf("a refused edit changed the file: %q", read.Content)
		}
	})

	// A folder that is not there is a mistake the assistant can correct, told
	// in those words rather than as a rule it has run into.
	t.Run("a folder that is not there says so", func(t *testing.T) {
		if runs := r.Runs(5, 11, "the-laptop"); runs["terminal"] == 0 {
			t.Skip("this build of the application has no terminal")
		}
		answer, err := r.Call(context.Background(), 5, 11, "the-laptop",
			"terminal", []byte(`{"command":"pwd","directory":"/no/such/place/at/all"}`), "a test")
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if answer.OK {
			t.Fatal("a folder that does not exist was accepted")
		}
		if !strings.Contains(answer.Message, "not a folder") {
			t.Fatalf("the reason is not the one: %q", answer.Message)
		}
	})

	// And a tool this build has never heard of is a failure the assistant can
	// read, not a hang: the gateway offers only what was declared, so reaching
	// here at all means something is wrong and saying so is the useful answer.
	t.Run("an unknown tool says so", func(t *testing.T) {
		answer, err := r.Call(context.Background(), 5, 11, "the-laptop",
			"nothing_like_this", []byte(`{}`), "a test")
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if answer.OK || answer.Message == "" {
			t.Fatalf("an unknown tool was not refused: %+v", answer)
		}
	})

	// 6. What one round trip costs, with no driver in the way.
	//
	// A database conversation is thousands of small questions, so this is the
	// number that decides whether the route is usable for a chatty protocol.
	// Measured against the same service reached directly, so what is reported is
	// the ROUTE's cost and not the machine's mood.
	t.Run("a round trip costs what a hop costs", func(t *testing.T) {
		const trips = 300
		message := []byte("are you there?")
		reply := make([]byte, len(message))

		straight, err := net.Dial("tcp", net.JoinHostPort(echoHost, strconv.Itoa(echoPort)))
		if err != nil {
			t.Fatalf("dial directly: %v", err)
		}
		defer func() { _ = straight.Close() }()
		if tcp, ok := straight.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
		}

		carried, err := r.Dial(context.Background(), 5, 11, "the-laptop", echoHost, echoPort, "a test")
		if err != nil {
			t.Fatalf("dial through the client: %v", err)
		}
		defer func() { _ = carried.Close() }()

		// The FASTEST round trip, not the average.
		//
		// An average measures the machine as much as the route: a build running
		// alongside adds outliers and the test fails for a reason that has
		// nothing to do with the code. What is being asked here is what a round
		// trip COSTS, and the cheapest one observed is the honest answer to
		// that. It is also the right shape for what this catches: something
		// waiting on a timer (Nagle, a delayed acknowledgement) raises the floor
		// too, while load only raises the ceiling.
		measure := func(conn net.Conn) time.Duration {
			best := time.Hour
			for i := 0; i < trips; i++ {
				start := time.Now()
				if _, err := conn.Write(message); err != nil {
					t.Fatalf("write: %v", err)
				}
				if _, err := io.ReadFull(conn, reply); err != nil {
					t.Fatalf("read: %v", err)
				}
				if took := time.Since(start); took < best {
					best = took
				}
			}
			return best
		}
		_, _ = measure(straight), measure(carried) // warm both
		direct := measure(straight)
		through := measure(carried)

		t.Logf("a round trip costs %s direct, %s through the client (%s added)",
			direct.Round(time.Microsecond), through.Round(time.Microsecond),
			(through - direct).Round(time.Microsecond))

		// A ceiling that catches a WAIT rather than a slow machine. Two extra
		// hops and two process wake-ups on loopback are worth tens of
		// microseconds; anything near a millisecond means something is being
		// held back for a timer.
		if added := through - direct; added > 500*time.Microsecond {
			t.Fatalf("the route adds %s to a round trip, which is a wait rather than a hop", added)
		}
	})

	// 7. And the client survives the gateway going away and comes back on its
	// own, which is what every deploy does to it.
	t.Run("it reconnects after the gateway restarts", func(t *testing.T) {
		gateway.restart(t)
		waitFor(t, "the link to drop", func() bool { return !r.Online(5, 11, "the-laptop") })
		waitFor(t, "the client to come back", func() bool { return r.Online(5, 11, "the-laptop") })

		conn, err := r.Dial(context.Background(), 5, 11, "the-laptop", echoHost, echoPort, "a test")
		if err != nil {
			t.Fatalf("dial after reconnect: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.Write([]byte("again")); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := make([]byte, 5)
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != "again" {
			t.Fatalf("got %q", got)
		}
	})
}

// restartableServer is a gateway that can go away and come back where it was.
type restartableServer struct {
	url      string
	address  string
	handler  http.Handler
	mu       sync.Mutex
	listener net.Listener
	server   *http.Server
	tracking *trackingListener
}

func newRestartableServer(t *testing.T, handler http.Handler) *restartableServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &restartableServer{
		address: listener.Addr().String(),
		handler: handler,
	}
	s.url = "http://" + s.address
	s.serve(listener)
	return s
}

func (s *restartableServer) serve(listener net.Listener) {
	// Every accepted connection is kept, because closing the server does not
	// close them: a websocket is HIJACKED, and a hijacked connection is no
	// longer the server's to manage. What ends one in production is the process
	// exiting, and that is what this has to imitate. Without it the test closed
	// the server, the client noticed nothing at all, and the whole reconnection
	// path went untested while reporting a timeout on the wrong line.
	tracking := &trackingListener{Listener: listener}
	server := &http.Server{Handler: s.handler, ReadHeaderTimeout: 5 * time.Second}
	s.mu.Lock()
	s.listener, s.server, s.tracking = listener, server, tracking
	s.mu.Unlock()
	go func() { _ = server.Serve(tracking) }()
}

// trackingListener remembers what it handed out, so it can be closed the way a
// process exiting closes it.
type trackingListener struct {
	net.Listener
	mu   sync.Mutex
	open []net.Conn
	shut bool
}

func (l *trackingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shut {
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	l.open = append(l.open, conn)
	return conn, nil
}

// closeAll ends every connection this listener ever accepted, hijacked ones
// included.
func (l *trackingListener) closeAll() {
	l.mu.Lock()
	open, _ := l.open, l.open
	l.open, l.shut = nil, true
	l.mu.Unlock()
	for _, conn := range open {
		_ = conn.Close()
	}
}

// restart drops everything and listens again on the same address, which is a
// deploy from the client's side.
func (s *restartableServer) restart(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	server, tracking := s.server, s.tracking
	s.mu.Unlock()
	// Close, not Shutdown: a deploy does not wait for a websocket to finish, and
	// what is being tested is the client's answer to being cut off.
	_ = server.Close()
	tracking.closeAll()

	for i := 0; i < 100; i++ {
		listener, err := net.Listen("tcp", s.address)
		if err == nil {
			s.serve(listener)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the gateway could not listen again on %s", s.address)
}

func (s *restartableServer) stop() {
	s.mu.Lock()
	server, tracking := s.server, s.tracking
	s.mu.Unlock()
	_ = server.Close()
	tracking.closeAll()
}

// rustClient is the shipped client, running as its own process.
type rustClient struct {
	cmd *exec.Cmd
	// What it was started with, so it can be started AGAIN the way a person
	// reopening the application starts it: same folder, same state, same
	// identity.
	start func() *exec.Cmd
}

// startRustClientRenewing is the same client, given the credential it should
// ask for when the gateway says the first one is nearly out, and watched.
//
// In the application the page mints that credential; here it is handed over,
// because a gate cannot sign in. What it SAYS is captured, because the property
// under test is not only that the work survived: it is that the link never went
// down, and the client is the only half that can say so.
func startRustClientRenewing(t *testing.T, base, token, next string) (*rustClient, *said) {
	t.Helper()
	t.Setenv("SAG_LINK_TOKEN_NEXT", next)
	heard := &said{}
	client := startRustClientSaying(t, base, token, heard)
	return client, heard
}

// said collects what the client printed, WITH WHEN, safely, while it runs.
//
// The times are the point. Whether the link went down during a renewal says
// nothing (a renewal is a reconnection: it goes down and comes back), and
// whether it was asked for one says nothing either (it is always asked
// eventually, if only after being refused). What separates the fixed product
// from the broken one is WHEN: before the credential ran out, or after.
type said struct {
	mu    sync.Mutex
	lines []heard
}

type heard struct {
	at   time.Time
	line string
}

func (s *said) Write(p []byte) (int, error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		s.lines = append(s.lines, heard{at: now, line: line})
	}
	os.Stderr.Write(p) // and still visible when a test fails
	return len(p), nil
}

func (s *said) contains(what string) bool {
	_, found := s.firstAt(what)
	return found
}

// firstAt is when the client first said something, and whether it said it.
func (s *said) firstAt(what string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, line := range s.lines {
		if strings.Contains(line.line, what) {
			return line.at, true
		}
	}
	return time.Time{}, false
}

func (s *said) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, line := range s.lines {
		b.WriteString(line.at.Format("15:04:05.000") + "  " + line.line + "\n")
	}
	return b.String()
}

func startRustClient(t *testing.T, base, token string) *rustClient {
	return startRustClientSaying(t, base, token, nil)
}

func startRustClientSaying(t *testing.T, base, token string, heard *said) *rustClient {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("find the repository: %v", err)
	}
	binary := filepath.Join(root, "desktop", "target", "debug", "examples", "link_client")
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("the client has not been built (%v); make link-e2e builds it", err)
	}

	// A folder for it to work in, and somewhere to remember it. In the
	// application a person chooses one with their own folder dialog; a test
	// cannot open a dialog, so it is given.
	state := t.TempDir()
	folder := t.TempDir()
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		"SAG_LINK_URL="+base, "SAG_LINK_TOKEN="+token,
		"SAG_LINK_STATE="+state, "SAG_LINK_FOLDER="+folder)
	var out io.Writer = os.Stderr // its own words, where a failing test shows them
	if heard != nil {
		out = heard
	}
	cmd.Stdout = out
	cmd.Stderr = out
	start := func() *exec.Cmd {
		again := exec.Command(binary)
		again.Env = cmd.Env
		again.Stdout = out
		again.Stderr = out
		if err := again.Start(); err != nil {
			t.Fatalf("start the client: %v", err)
		}
		return again
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the client: %v", err)
	}
	return &rustClient{cmd: cmd, start: start}
}

// quit ends the application the way quitting it does, and reopen starts it
// again. Together they are the most common interruption there is: somebody
// closing the chat application, or a new version being installed.
func (c *rustClient) quit() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_, _ = c.cmd.Process.Wait()
	}
}

func (c *rustClient) reopen() {
	c.cmd = c.start()
}

func (c *rustClient) stop() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
}

// liveEcho is a service that sends back whatever it is sent.
func liveEcho(t *testing.T) (host string, port int) {
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
	return splitAddr(t, listener.Addr().String())
}

// liveAnswerAfterEOF only replies once the request has ended, which is the
// shape that needs half-close to survive the whole route.
func liveAnswerAfterEOF(t *testing.T, size int) (host string, port int, answer []byte) {
	t.Helper()
	answer = make([]byte, size)
	for i := range answer {
		answer[i] = byte(i % 251)
	}
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
				if _, err := io.Copy(io.Discard, conn); err != nil {
					return
				}
				_, _ = conn.Write(answer)
			}()
		}
	}()
	host, port = splitAddr(t, listener.Addr().String())
	return host, port, answer
}

// freePort is a port nothing is listening on, for proving a refusal.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port := splitAddr(t, listener.Addr().String())
	_ = listener.Close()
	return port
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port %q: %v", portText, err)
	}
	return host, port
}

// waitFor polls for something the other process does, and says what it was
// waiting for when it does not happen.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// quoted is a JSON string, for building a call by hand.
func quoted(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// cutTheLink closes a machine's control socket, which is what a failed
// keepalive, a renewed credential or a moved network does. The application is
// left running and reconnects on its own.
func cutTheLink(t *testing.T, r *Registry, k key) {
	t.Helper()
	r.mu.Lock()
	m, linked := r.machines[k]
	r.mu.Unlock()
	if !linked {
		t.Fatal("there is no link to cut")
	}
	m.drop()
}
