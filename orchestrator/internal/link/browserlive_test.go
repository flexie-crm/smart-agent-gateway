package link

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/linktest"
)

// The browser tools, from this side of the link.
//
// Everything else that tests them runs INSIDE the Rust client: it calls its own
// dispatcher, which proves the handlers work and proves nothing at all about
// the journey. This is the journey. A tool call is built here, sent over a real
// websocket to the real client, and its answer is read back here.
//
// It exists because this exact gap has already cost a shipped defect. The
// gateway sent `null` for an optional list, the client refused it, and no test
// in either half could see it: the Go side was sure it had sent valid arguments
// and the Rust side was sure it had been given bad ones, both correctly, about
// two different things. Only traffic between the two showed it (KB/29, KB/42).
//
// So what is asserted here is deliberately not "does clicking work". It is:
// does the name arrive, do the arguments survive the wire as the shapes the
// handler expects, does an answer come back readable, and does a refusal come
// back classified.
//
//	make link-e2e
//
// The tool's name, spelled out rather than taken from the package that
// declares it: `internal/tools/machine` imports this package, so importing it
// back would be a cycle. It is not an unguarded literal, because both halves
// are already held to `desktop/link-tools.json` (a Go test there and a Rust
// test in the client), so a rename that missed one of them fails before this
// file runs.
const browserTool = "browser"

func TestTheBrowserToolsWorkThroughTheRealLink(t *testing.T) {
	linktest.Skip(t)
	browser := browserForTheGate(t)

	r := NewRegistry(zerolog.Nop(), func(token string) (int64, int64, string, time.Time, bool) {
		if token == "a-real-looking-token" {
			return 11, 5, "the-laptop", time.Now().Add(time.Hour), true
		}
		return 0, 0, "", time.Now().Add(time.Hour), false
	}, []string{"*"})

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/link", r.ServeControl)
	mux.HandleFunc("/v1/link/stream", r.ServeStream)
	gateway := newRestartableServer(t, mux)
	defer gateway.stop()

	client := startRustClient(t, gateway.url, "a-real-looking-token")
	defer client.Stop()
	waitFor(t, "the real client to link", func() bool { return r.Online(5, 11, "the-laptop") })

	// The browser goes where the client looks for it, which is under the state
	// directory it was told about. Linked rather than downloaded: the gate is
	// given one, and a hundred megabyte fetch per run is a gate people stop
	// running.
	installBrowser(t, client.State, browser)

	// A real web server, not a file:// page, and that is not tidiness: a
	// file:// page has no cookie jar at all, so save_session could not be
	// driven over it, and it makes no network requests worth listing. The
	// browser is on this machine, so a loopback server is reachable from it.
	site := fixtureSite(t)
	page := site.URL + "/page"
	second := site.URL + "/second"
	// A different HOST on the same server, which is a different origin and a
	// different cookie domain while being the same fixture. What a control for
	// the session work needs.
	elsewhere := strings.Replace(page, "127.0.0.1", "localhost", 1)

	// This is the assertion the version contract cannot make. It agrees on the
	// NAME; this says the client will actually take a call under it.
	runs := r.Runs(5, 11, "the-laptop")
	if runs[browserTool] == 0 {
		t.Fatalf("this build of the application does not offer %s, so nothing below "+
			"would be tested rather than skipped", browserTool)
	}

	// One tool with the action as an argument, which is what the gateway sends.
	// The action goes in here rather than at each call site below, so every
	// line of this file sends exactly what the real thing sends.
	ask := func(t *testing.T, action string, args map[string]any) Result {
		t.Helper()
		args["conversation"] = 4242
		args["action"] = action
		tool := browserTool
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("the arguments could not be written: %v", err)
		}
		answer, err := r.Call(context.Background(), 5, 11, "the-laptop", tool, encoded, "a test")
		if err != nil {
			t.Fatalf("%s did not reach the computer: %v", tool, err)
		}
		return answer
	}

	must := func(t *testing.T, action string, args map[string]any) map[string]any {
		t.Helper()
		answer := ask(t, action, args)
		if !answer.OK {
			t.Fatalf("%s failed: %s", action, answer.Message)
		}
		var content map[string]any
		if err := json.Unmarshal(answer.Content, &content); err != nil {
			t.Fatalf("%s answered something unreadable (%v): %s", action, err, answer.Content)
		}
		return content
	}

	// 1. A page opens, and the answer carries the outline with references in it.
	//    The outline is what every other call depends on, so its absence would
	//    make everything below meaningless rather than failing.
	t.Run("a page opens and answers with an outline", func(t *testing.T) {
		got := must(t, "navigate", map[string]any{"url": page})
		if got["title"] != "Through the link" {
			t.Fatalf("the page did not load: %v", got["title"])
		}
		outline, _ := got["snapshot"].(string)
		if !strings.Contains(outline, "[ref=e") {
			t.Fatalf("the outline carries no references:\n%s", outline)
		}
		// The outline has to name what the rest of this file points at. A
		// reference that cannot be found here is a broken outline, not a
		// broken click, and finding that out now says which.
		referenceFor(t, outline, `button "Press me"`)
	})

	// 2. Every argument type the schemas declare, over the wire.
	//
	//    This is what the gap was about. A string, a number, a boolean and an
	//    ARRAY all have to survive being marshalled here and deserialised
	//    there, and the array is the one that has bitten this product before.
	t.Run("every argument type survives the wire", func(t *testing.T) {
		// number and boolean, on snapshot.
		got := must(t, "snapshot", map[string]any{"depth": 1, "boxes": true})
		outline, _ := got["snapshot"].(string)
		if !strings.Contains(outline, "[box=") {
			t.Fatalf("boxes did not arrive as a boolean:\n%s", outline)
		}

		// A string array, on select_option. The shape that shipped broken once.
		options := must(t, "snapshot", map[string]any{})
		picker := referenceFor(t, options["snapshot"].(string), "combobox")
		chose := must(t, "select_option", map[string]any{
			"target": picker, "values": []string{"two"},
		})
		selected, ok := chose["selected"].([]any)
		if !ok || len(selected) != 1 || selected[0] != "two" {
			t.Fatalf("the values array did not arrive intact: %v", chose["selected"])
		}

		// An array of enums, and two booleans, on click. Untested anywhere
		// before this: they are declared, so a model will use them.
		here := must(t, "snapshot", map[string]any{})
		target := referenceFor(t, here["snapshot"].(string), `button "Press me"`)
		clicked := must(t, "click", map[string]any{
			"element":     "the button",
			"target":      target,
			"doubleClick": true,
			"button":      "left",
			"modifiers":   []string{"Shift"},
		})
		if clicked["clicked"] != true {
			t.Fatalf("a click with every option did not happen: %v", clicked)
		}
		// The page records what it saw, so this proves the options ARRIVED
		// rather than merely being accepted.
		seen := must(t, "snapshot", map[string]any{})
		outline, _ = seen["snapshot"].(string)
		if !strings.Contains(outline, "shift=true") {
			t.Fatalf("the modifier did not reach the page:\n%s", outline)
		}
		if !strings.Contains(outline, "detail=2") {
			t.Fatalf("doubleClick did not reach the page as a double click:\n%s", outline)
		}
	})

	// 3. Typing, with the two flags, and the text read back.
	t.Run("typing arrives and lands in the field", func(t *testing.T) {
		here := must(t, "snapshot", map[string]any{})
		field := referenceFor(t, here["snapshot"].(string), `textbox "Your name"`)
		if got := must(t, "type", map[string]any{
			"target": field, "text": "Maren", "submit": false, "slowly": false,
		}); got["typed"] != true {
			t.Fatalf("typing did not happen: %v", got)
		}
		back := must(t, "snapshot", map[string]any{"target": field})
		if !strings.Contains(back["snapshot"].(string), "Maren") {
			t.Fatalf("the text did not land in the field:\n%s", back["snapshot"])
		}
	})

	// 4. A key, into whatever has focus, with no target at all.
	t.Run("a key press arrives", func(t *testing.T) {
		// A CHORD over the wire, which is the argument most likely to arrive
		// mangled: it is one string carrying three things.
		if got := must(t, "press_key", map[string]any{"key": "Control+Shift+T"}); got["pressed"] != "Control+Shift+T" {
			t.Fatalf("a chord did not survive the journey: %v", got["pressed"])
		}
		if got := must(t, "press_key", map[string]any{"key": "Tab"}); got["pressed"] != "Tab" {
			t.Fatalf("the key did not arrive: %v", got)
		}
	})

	// 5. Hover, back, and close: the three with no element to act on.
	t.Run("hover, back and close arrive", func(t *testing.T) {
		here := must(t, "snapshot", map[string]any{})
		over := referenceFor(t, here["snapshot"].(string), `button "Hover me"`)
		if got := must(t, "hover", map[string]any{"target": over}); got["hovered"] != true {
			t.Fatalf("hover did not happen: %v", got)
		}
		// Back, from a page that has somewhere to go: navigate twice first.
		must(t, "navigate", map[string]any{"url": page})
		if got := must(t, "navigate_back", map[string]any{}); got["title"] == nil {
			t.Fatalf("going back answered nothing: %v", got)
		}
		if got := must(t, "close", map[string]any{}); got["closed"] != true {
			t.Fatalf("closing did not happen: %v", got)
		}
	})

	// 6. The argument SHAPES the new actions added survive the wire.
	//
	//    This is the half a test inside the Rust client cannot reach. The nine
	//    original actions send strings, numbers and lists of strings. These
	//    send a list of OBJECTS (fill_form's fields) and a MAP (drop's data),
	//    and a `null` for an optional list has already shipped broken once on
	//    this link, invisible to both halves and visible only in the traffic
	//    between them.
	t.Run("the new argument shapes survive the wire", func(t *testing.T) {
		must(t, "navigate", map[string]any{"url": page})

		// A list of objects, each with four fields of its own.
		filled := must(t, "fill_form", map[string]any{
			"fields": []map[string]any{
				{"target": "#who", "name": "Your name", "type": "textbox", "value": "Ada"},
				{"target": "#agree", "name": "I agree", "type": "checkbox", "value": "true"},
				{"target": "#pick", "name": "Pick one", "type": "combobox", "value": "Two"},
			},
		})
		if done, ok := filled["filled"].([]any); !ok || len(done) != 3 {
			t.Fatalf("the list of fields did not arrive whole: %v", filled)
		}
		here := must(t, "snapshot", map[string]any{})
		outline, _ := here["snapshot"].(string)
		if !strings.Contains(outline, "Ada") || !strings.Contains(outline, "[checked]") {
			t.Fatalf("the fields did not reach the page:\n%s", outline)
		}

		// A map of strings, which is drop's `data`.
		dropped := must(t, "drop", map[string]any{
			"target": "#zone",
			"data":   map[string]any{"text/plain": "carried over"},
		})
		if dropped["dropped"] != true {
			t.Fatalf("the drop did not happen: %v", dropped)
		}
		landed := must(t, "snapshot", map[string]any{"target": "#dropped"})
		if got, _ := landed["snapshot"].(string); !strings.Contains(got, "carried over") {
			t.Fatalf("the map of data did not arrive whole:\n%s", got)
		}

		// Numbers, and a change the page can see for itself.
		must(t, "resize", map[string]any{"width": 500, "height": 400})
		sized := must(t, "snapshot", map[string]any{"target": "#size"})
		if got, _ := sized["snapshot"].(string); !strings.Contains(got, "500x400") {
			t.Fatalf("resizing did not reach the page:\n%s", got)
		}

		// A record kept on the far side and carried back as a list of objects.
		said := must(t, "console_messages", map[string]any{"level": "error"})
		lines, _ := said["messages"].([]any)
		if len(lines) == 0 {
			t.Fatal("the console record came back empty")
		}
		first, _ := lines[0].(map[string]any)
		if text, _ := first["text"].(string); !strings.Contains(text, "a line for the record") {
			t.Fatalf("the console line did not survive the journey: %v", lines[0])
		}

		// And the tabs action, whose two parameters carry names of our own and
		// are the ones most likely to be sent under Playwright's by mistake.
		//
		// Which tab is closed is what this asserts, not how many. Closing with
		// no index closes the CURRENT one, so a test that opens a tab and
		// closes index 1 passes just as well when the index is ignored: that
		// is what the first version of this did. So the second tab is a
		// different page, the first is made current, and the assertion is on
		// WHICH one is left.
		opened := must(t, "tabs", map[string]any{"operation": "new", "url": second})
		tabs, _ := opened["tabs"].([]any)
		if len(tabs) != 2 {
			t.Fatalf("a new tab did not appear: %v", opened)
		}
		must(t, "tabs", map[string]any{"operation": "select", "tab": 0})
		shut := must(t, "tabs", map[string]any{"operation": "close", "tab": 1})
		left, _ := shut["tabs"].([]any)
		if len(left) != 1 {
			t.Fatalf("closing a tab by index did not work: %v", shut)
		}
		only, _ := left[0].(map[string]any)
		if title, _ := only["title"].(string); !strings.Contains(title, "Through the link") {
			t.Fatalf("the wrong tab was closed: %q is what is left, so the index was "+
				"ignored and the current tab went instead", title)
		}
	})

	// 7. Every action that had no journey of its own.
	//
	//    Nine of the twenty-three were proven only INSIDE the Rust client,
	//    which tests the handler and says nothing about the wire. That is the
	//    gap this closes: the `null` for an optional list that shipped broken
	//    was invisible to both halves and visible only in the traffic between
	//    them, and there is no reason the next one would be different.
	t.Run("drag arrives", func(t *testing.T) {
		must(t, "navigate", map[string]any{"url": page})
		here := must(t, "snapshot", map[string]any{})
		outline, _ := here["snapshot"].(string)
		from := referenceFor(t, outline, "drag me")
		to := referenceFor(t, outline, "drop here")
		if got := must(t, "drag", map[string]any{"startTarget": from, "endTarget": to}); got["dragged"] != true {
			t.Fatalf("the drag did not happen: %v", got)
		}
		// What the drop handler WRITES, not a word the page already said. The
		// Rust half's version of this test looked for "dropped" while its page
		// started out saying "nothing dropped", so it passed with drag doing
		// nothing; this gate is what caught it.
		landed := must(t, "snapshot", map[string]any{"target": "#dropped"})
		if got, _ := landed["snapshot"].(string); !strings.Contains(got, "text=the box") {
			t.Fatalf("the drag did not carry the dragged data to the drop zone:\n%s", got)
		}
	})

	t.Run("exec_js arrives and its value comes back", func(t *testing.T) {
		must(t, "navigate", map[string]any{"url": page})
		got := must(t, "exec_js", map[string]any{
			"script": "document.title = 'renamed'; return {rows: document.querySelectorAll('p').length, ok: true};",
		})
		result, _ := got["js_result"].(map[string]any)
		if result == nil || result["ok"] != true {
			t.Fatalf("the script's value did not survive the journey: %v", got)
		}
		if rows, _ := result["rows"].(float64); rows < 1 {
			t.Fatalf("the script did not run against the real page: %v", got)
		}
		// And it really ran IN the page, not somewhere that merely answered.
		after := must(t, "snapshot", map[string]any{})
		_ = after
		if title := must(t, "exec_js", map[string]any{"script": "return document.title;"}); title["js_result"] != "renamed" {
			t.Fatalf("the script's effect did not stay in the page: %v", title)
		}

		// A throw is NOT a failure: it comes back as __error and the page is
		// still there. That contract is the whole reason the try/catch is
		// inside the wrapper rather than left to the protocol.
		thrown := must(t, "exec_js", map[string]any{"script": "throw new Error('deliberate');"})
		inside, _ := thrown["js_result"].(map[string]any)
		if inside == nil || !strings.Contains(fmt.Sprint(inside["__error"]), "deliberate") {
			t.Fatalf("a throw should arrive as __error rather than a failure: %v", thrown)
		}
	})

	t.Run("wait_for arrives", func(t *testing.T) {
		must(t, "navigate", map[string]any{"url": page})
		if got := must(t, "wait_for", map[string]any{"text": "it arrived", "textGone": "not yet"}); got["waited"] == nil {
			t.Fatalf("the wait said nothing about what it waited for: %v", got)
		}
		here := must(t, "snapshot", map[string]any{})
		outline, _ := here["snapshot"].(string)
		if !strings.Contains(outline, "it arrived") || strings.Contains(outline, "not yet") {
			t.Fatalf("the wait returned before the page changed:\n%s", outline)
		}
	})

	t.Run("the network record crosses the wire", func(t *testing.T) {
		must(t, "navigate", map[string]any{"url": page})
		listed := must(t, "network_requests", map[string]any{})
		requests, _ := listed["requests"].([]any)
		if len(requests) == 0 {
			t.Fatalf("the network record came back empty: %v", listed)
		}
		var number float64
		var found bool
		for _, one := range requests {
			row, _ := one.(map[string]any)
			if url, _ := row["url"].(string); strings.Contains(url, "/api/thing") {
				number, _ = row["index"].(float64)
				found = true
			}
		}
		if !found {
			t.Fatalf("the fetch the page made is missing: %v", requests)
		}
		// A successful image is held back and the answer says how many, which
		// is a number computed on the far side and carried here.
		if hidden, _ := listed["hidden_static"].(float64); hidden < 1 {
			t.Fatalf("the static count did not survive: %v", listed)
		}

		// And one request in full, by the number the list printed, which is
		// the pairing between the two actions.
		whole := must(t, "network_request", map[string]any{"index": number})
		if status, _ := whole["status"].(float64); status != 200 {
			t.Fatalf("the request's status did not come back: %v", whole)
		}
		if body, _ := whole["responseBody"].(string); !strings.Contains(body, "42") {
			t.Fatalf("the response body did not come back: %v", whole)
		}
		// One part on its own, which is a different shape of answer.
		part := must(t, "network_request", map[string]any{"index": number, "part": "response-headers"})
		if part["headers"] == nil {
			t.Fatalf("asking for one part returned nothing: %v", part)
		}
	})

	t.Run("a dialog blocks and is answered across the wire", func(t *testing.T) {
		must(t, "navigate", map[string]any{"url": page})
		here := must(t, "snapshot", map[string]any{})
		outline, _ := here["snapshot"].(string)
		button := referenceFor(t, outline, `"Ask"`)

		// The click that opens it is not a failure and does not hang: the page
		// has stopped to ask something, which is what the answer says.
		opening := must(t, "click", map[string]any{"target": button})
		if opened, _ := opening["opened"].(string); !strings.Contains(opened, "confirm") {
			t.Fatalf("opening a dialog should say what the page is asking: %v", opening)
		}

		// Everything else refuses while it is open, and the refusal names the
		// action that answers it. This is what stops a snapshot hanging until
		// the protocol gives up.
		blocked := ask(t, "snapshot", map[string]any{})
		if blocked.OK {
			t.Fatal("a snapshot ran while a dialog was open")
		}
		if !strings.Contains(blocked.Message, "handle_dialog") {
			t.Fatalf("the refusal should name what answers it: %s", blocked.Message)
		}

		answered := must(t, "handle_dialog", map[string]any{"accept": true})
		if answered["accepted"] != true || answered["handled"] != "confirm" {
			t.Fatalf("the dialog was not answered: %v", answered)
		}
		said := must(t, "snapshot", map[string]any{"target": "#answered"})
		if got, _ := said["snapshot"].(string); !strings.Contains(got, "answered true") {
			t.Fatalf("the page was not told the dialog was accepted:\n%s", got)
		}
	})

	t.Run("a file is uploaded from this computer", func(t *testing.T) {
		must(t, "navigate", map[string]any{"url": page})
		at := filepath.Join(client.Folder, "through-the-link.txt")
		if err := os.WriteFile(at, []byte("hello from the gateway side"), 0o644); err != nil {
			t.Fatalf("write the file: %v", err)
		}

		// The order that works: click the thing that asks for a file first.
		must(t, "click", map[string]any{"target": "#file"})
		got := must(t, "file_upload", map[string]any{"paths": []string{at}})
		if given, _ := got["uploaded"].([]any); len(given) != 1 {
			t.Fatalf("the path did not arrive: %v", got)
		}
		chosen := must(t, "snapshot", map[string]any{"target": "#chosen"})
		if seen, _ := chosen["snapshot"].(string); !strings.Contains(seen, "through-the-link.txt") {
			t.Fatalf("the page was not given the file:\n%s", seen)
		}
	})

	// 8. A sign-in kept on the computer, over the wire, and the controls that
	//    say it is the folder doing it.
	t.Run("a sign-in is kept and forgotten across the wire", func(t *testing.T) {
		must(t, "navigate", map[string]any{"url": page})
		who := must(t, "snapshot", map[string]any{"target": "#who-am-i"})
		if seen, _ := who["snapshot"].(string); !strings.Contains(seen, "SIGNED OUT") {
			t.Fatalf("the site should start signed out:\n%s", seen)
		}

		// Sign in the way a site does, through a script over the link.
		must(t, "exec_js", map[string]any{
			"script": "document.cookie = 'sid=OVER-THE-WIRE; path=/';" +
				"localStorage.setItem('jwt', 'WIRE-TOKEN'); return true;",
		})

		// The CONTROL: a different origin on the same server, signed in the
		// same way and never saved.
		must(t, "navigate", map[string]any{"url": elsewhere})
		must(t, "exec_js", map[string]any{
			"script": "localStorage.setItem('jwt', 'CONTROL'); return true;",
		})

		must(t, "navigate", map[string]any{"url": page})
		kept := must(t, "save_session", map[string]any{})
		if saved, _ := kept["saved"].(string); !strings.Contains(saved, "127.0.0.1") {
			t.Fatalf("it should say which site was kept: %v", kept)
		}
		// The folder is on the CLIENT's disk, which is the point: the gateway
		// asked and the computer wrote it.
		folder := filepath.Join(client.State, "browser-auth")
		kept_dirs, err := os.ReadDir(folder)
		if err != nil || len(kept_dirs) != 1 {
			t.Fatalf("exactly one site should have a folder on the computer: %v %v", kept_dirs, err)
		}
		// And no cookie came back to this side, which is the whole reason it
		// is a folder. Checked against the ANSWER, because that is what would
		// be stored and shown.
		if strings.Contains(fmt.Sprint(kept), "OVER-THE-WIRE") {
			t.Fatalf("a session cookie came back in the answer: %v", kept)
		}

		// Close the pages and the browser, which is what closing the
		// application does, then come back.
		must(t, "close", map[string]any{})
		restartBrowser(t, client)

		back := must(t, "navigate", map[string]any{"url": page})
		if back["signed_in"] == nil {
			t.Fatalf("the answer should say a sign-in was restored: %v", back)
		}
		seen, _ := back["snapshot"].(string)
		if !strings.Contains(seen, "OVER-THE-WIRE") || !strings.Contains(seen, "WIRE-TOKEN") {
			t.Fatalf("the saved sign-in did not come back:\n%s", seen)
		}

		// The control, never saved, is signed out. Without this the test
		// passes on a browser that simply kept its profile.
		control := must(t, "navigate", map[string]any{"url": elsewhere})
		if got, _ := control["snapshot"].(string); !strings.Contains(got, "SIGNED OUT") {
			t.Fatalf("a site that was never saved came back signed in:\n%s", got)
		}

		gone := must(t, "forget_session", map[string]any{"url": page})
		if gone["forgotten"] != true {
			t.Fatalf("forgetting did not happen: %v", gone)
		}
		if left, _ := os.ReadDir(folder); len(left) != 0 {
			t.Fatalf("the folder should be gone after forgetting: %v", left)
		}
	})

	// 9. The two rules that bound what the browser costs, over the wire.
	t.Run("the tab pool holds across the wire", func(t *testing.T) {
		// Twenty agents, each with its own owner the way the gateway sends a
		// fleet, each opening a page and stopping.
		for agent := 1; agent <= 20; agent++ {
			args := map[string]any{"url": page, "action": "navigate", "conversation": -agent}
			encoded, err := json.Marshal(args)
			if err != nil {
				t.Fatalf("arguments: %v", err)
			}
			answer, err := r.Call(context.Background(), 5, 11, "the-laptop", browserTool, encoded, "a fleet")
			if err != nil || !answer.OK {
				t.Fatalf("agent %d could not open a page: %v %v", agent, err, answer.Message)
			}
		}
		// Summed by ASKING each agent what it has left, which is the only
		// honest count available from this side. Counting processes was tried
		// and does not work: only the parent carries the browser's own path
		// and the children show the resolved symlink, so pgrep finds one
		// whatever is open. The same trap as the profile path in KB/39.
		//
		// The tabs action never opens a page, so asking an agent that has none
		// answers none rather than making one.
		open := 0
		for agent := 1; agent <= 20; agent++ {
			open += tabsOf(t, r, -agent)
		}
		if open > 10 {
			t.Fatalf("twenty agents left %d tabs open, and the cap is 10", open)
		}
		if open < 2 {
			t.Fatalf("only %d tabs: something closed them that should not have", open)
		}
	})

	t.Run("one page is one tab across the wire", func(t *testing.T) {
		must(t, "navigate", map[string]any{"url": page})
		two := must(t, "tabs", map[string]any{"operation": "new", "url": second})
		if tabs, _ := two["tabs"].([]any); len(tabs) != 2 {
			t.Fatalf("a second tab on a different address should open: %v", two)
		}
		again := must(t, "tabs", map[string]any{"operation": "new", "url": page})
		tabs, _ := again["tabs"].([]any)
		if len(tabs) != 2 {
			t.Fatalf("asking for a tab on a page already open opened another: %v", again)
		}
		if note, _ := again["note"].(string); !strings.Contains(note, "already open") {
			t.Fatalf("it should say why there is no new tab: %v", again)
		}
	})

	// 10. A refusal comes back CLASSIFIED, not just as a failure.
	//
	//    The loop treats the two differently: bad_arguments is something the
	//    model corrects, a failure is something it reports. If the kind does
	//    not survive the wire, every correctable mistake becomes a dead end.
	t.Run("a refusal keeps its kind across the wire", func(t *testing.T) {
		must(t, "navigate", map[string]any{"url": page})

		correctable := ask(t, "press_key", map[string]any{"key": "Ctrl+A"})
		if correctable.OK {
			t.Fatal("a modifier that does not exist was accepted")
		}
		if correctable.Kind != "bad_arguments" {
			t.Fatalf("a correctable mistake arrived as %q: %s", correctable.Kind, correctable.Message)
		}
		if !strings.Contains(correctable.Message, "Ctrl") {
			t.Fatalf("the refusal lost what was wrong with it: %s", correctable.Message)
		}

		// And an action that is not one of the nine, which one tool makes
		// reachable in a way nine tools did not: the action is a string the
		// model chose, so getting it wrong has to be correctable.
		wrong := ask(t, "screenshot", map[string]any{})
		if wrong.OK {
			t.Fatal("an action that does not exist was accepted")
		}
		if wrong.Kind != "bad_arguments" {
			t.Fatalf("a wrong action arrived as %q: %s", wrong.Kind, wrong.Message)
		}
		if !strings.Contains(wrong.Message, "snapshot") {
			t.Fatalf("the refusal did not list the actions there are: %s", wrong.Message)
		}

		failed := ask(t, "click", map[string]any{"target": "#nothing-like-this"})
		if failed.OK {
			t.Fatal("clicking nothing succeeded")
		}
		if failed.Kind == "bad_arguments" {
			t.Fatalf("a page-state failure arrived as a correctable mistake: %s", failed.Message)
		}
		if !strings.Contains(failed.Message, "nothing on this page matches") {
			t.Fatalf("the refusal lost its reason: %s", failed.Message)
		}
	})
}

// endBrowsersUnder ends any browser running out of this directory.
//
// Matched on the path it was started from, which is unique to this test: a
// scratch directory nothing else has a reason to be inside. Killing the parent
// is enough, its helpers go within a few seconds of losing it.
func endBrowsersUnder(state string) {
	if runtime.GOOS == "windows" {
		return // the gate does not run here
	}
	// A failure is the ordinary case: it means there was nothing to end.
	_ = exec.Command("pkill", "-f", filepath.Join(state, "browser")).Run()
}

// browserForTheGate is the browser this gate drives, or a skip.
func browserForTheGate(t *testing.T) string {
	t.Helper()
	at := os.Getenv("SAG_BROWSER_PATH")
	if at == "" {
		t.Skip("SAG_BROWSER_PATH is not set; the browser half of the link gate needs one")
	}
	if _, err := os.Stat(at); err != nil {
		t.Fatalf("SAG_BROWSER_PATH is not a browser: %v", err)
	}
	return at
}

// installBrowser puts the browser where the client looks for it.
//
// The WHOLE directory, not the program: Chromium resolves its resources beside
// its own executable, so a lone link gives it a folder holding one symlink and
// it exits on "icudtl.dat not found in bundle". Linking the directory is also
// the shape a real download leaves.
func installBrowser(t *testing.T, state, browser string) {
	t.Helper()
	// Whoever puts a browser here takes it away again.
	//
	// It cannot be left to the client: the gate ends that by cancelling its
	// context, which is a kill it gets no chance to tidy up after, and that is
	// the point, since a crashed or force-quit application is what this is
	// modelling. Nor can it be left to a process group, which was tried and
	// measured: the browser puts itself in a group of its own (pgid equal to
	// its own pid), so it is not in the client's group and ending that group
	// reaches nothing.
	//
	// So it is done by where the browser LIVES, which is this test's own
	// directory and nobody else's. Four processes per run were being left
	// behind otherwise, and a session of them takes the machine down.
	t.Cleanup(func() { endBrowsersUnder(state) })

	version := pinnedBrowser(t)
	home := filepath.Join(state, "browser", version)
	if err := os.MkdirAll(filepath.Dir(home), 0o755); err != nil {
		t.Fatalf("make the browser's folder: %v", err)
	}
	if err := os.Symlink(filepath.Dir(browser), home); err != nil {
		t.Fatalf("link the browser into place: %v", err)
	}
}

// pinnedBrowser is the version the client will look for, read from the one
// file that decides it.
func pinnedBrowser(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find this test's own path")
	}
	root := filepath.Join(filepath.Dir(here), "..", "..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "desktop", "browser.json"))
	if err != nil {
		t.Fatalf("the browser pin is missing: %v", err)
	}
	var pin struct {
		Chrome string `json:"chrome"`
	}
	if err := json.Unmarshal(raw, &pin); err != nil {
		t.Fatalf("the browser pin is not readable: %v", err)
	}
	if pin.Chrome == "" {
		t.Fatal("the pin names no browser version")
	}
	return pin.Chrome
}

// fixturePage writes the page this gate drives and answers its address.
//
// It records what it SAW of each event, in the page, so an argument can be
// proved to have arrived rather than merely accepted: a modifier that was
// dropped on the way and a modifier that was never sent look identical from
// here otherwise.
// fixtureSite is the web server this gate drives.
//
// A real server rather than files on disk, for three reasons that are each on
// their own sufficient: a file:// page has NO cookie jar, so the session
// actions cannot be driven over one; it makes no network requests, so the
// network actions have nothing to list; and it cannot be fetched from, so a
// script that calls an endpoint has nowhere to call.
func fixtureSite(t *testing.T) *httptest.Server {
	t.Helper()
	const page = `<!doctype html><meta charset="utf-8"><title>Through the link</title>
<style>#menu{display:none} #hoverme:hover + #menu{display:block}
  #zone{width:200px;height:60px;border:1px solid #999} #box{width:50px;height:30px;background:#ccc}
  #later{display:none}</style>
<label for="who">Your name</label><input id="who">
<label for="pick">Pick one</label>
<select id="pick">
  <option value="one">One</option>
  <option value="two">Two</option>
</select>
<button id="press" onclick="
  document.getElementById('saw').textContent =
    'shift=' + event.shiftKey + ' detail=' + event.detail;
">Press me</button>
<p id="saw">nothing yet</p>
<button id="hoverme">Hover me</button>
<p id="menu">the menu</p>
<input id="agree" type="checkbox">
<div id="box" draggable="true">drag me</div>
<div id="zone">drop here</div>
<p id="dropped">no drop yet</p>
<p id="size">?</p>
<input id="file" type="file"><p id="chosen">no file</p>
<p id="soon">not yet</p><p id="later">it arrived</p>
<p id="answered"><button id="ask">Ask</button></p>
<p id="who-am-i">?</p>
<script>
const zone = document.getElementById('zone'), box = document.getElementById('box');
box.addEventListener('dragstart', e => e.dataTransfer.setData('text/plain', 'the box'));
zone.addEventListener('dragover', e => e.preventDefault());
zone.addEventListener('drop', e => {
  e.preventDefault();
  document.getElementById('dropped').textContent =
    'files=' + e.dataTransfer.files.length + ' text=' + e.dataTransfer.getData('text/plain');
});
let down = false;
box.addEventListener('mousedown', () => { down = true; });
zone.addEventListener('mouseup', () => {
  if (down) document.getElementById('dropped').textContent = 'dropped by mouse';
  down = false;
});
document.getElementById('file').addEventListener('change', e => {
  document.getElementById('chosen').textContent =
    'chose ' + [...e.target.files].map(f => f.name).join(', ');
});
setTimeout(() => {
  document.getElementById('soon').style.display = 'none';
  document.getElementById('later').style.display = 'block';
}, 600);
function showSize(){ document.getElementById('size').textContent = window.innerWidth + 'x' + window.innerHeight; }
showSize(); window.addEventListener('resize', showSize);
document.getElementById('ask').addEventListener('click', () => {
  const said = confirm('Really?');
  document.getElementById('answered').textContent = 'answered ' + said;
});
// Who the site thinks you are, decided ON LOAD, which is what makes the
// restore ordering matter.
var jar = document.cookie.match(/sid=([^;]*)/), tok = localStorage.getItem('jwt');
document.getElementById('who-am-i').textContent =
  (jar || tok) ? ('signed in ' + (jar ? jar[1] : '') + ' ' + (tok || '')) : 'SIGNED OUT';
console.error('a line for the record');
fetch('/api/thing').then(r => r.json()).then(() => {});
fetch('/api/missing').then(() => {});
const img = new Image(); img.src = '/picture.png';
</script>
`
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
	mux.HandleFunc("/second", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>Another page</title><h1>Another</h1>`))
	})
	mux.HandleFunc("/api/thing", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":42}`))
	})
	mux.HandleFunc("/api/missing", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	mux.HandleFunc("/picture.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		// The smallest real PNG there is, so the browser calls it an image
		// rather than something that failed, which is what the static filter
		// is written in terms of.
		_, _ = w.Write([]byte{
			0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
			0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
			0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00,
			0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
			0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49,
			0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
		})
	})

	// Bound to a real address rather than a random one, because the browser
	// has to reach it and `localhost` has to resolve to the same server for
	// the session control below.
	site := httptest.NewServer(mux)
	t.Cleanup(site.Close)
	return site
}

// restartBrowser ends the browser the way closing the application does, so the
// next call has to start a new one.
//
// The pages go with it and so does everything the client held in memory,
// including which sites had already had a sign-in restored. That is what makes
// the next navigate a genuinely new run rather than the same one continuing.
func restartBrowser(t *testing.T, client *rustClient) {
	t.Helper()
	// Which browser is running now, so the one that comes back can be shown to
	// be a DIFFERENT one. Without this the test rests on "the old one went
	// away", and a browser that never died would look like a sign-in that
	// survived: the cookie would still be in the live jar and nothing here
	// would notice.
	was := browserPid(t, client)
	if was == "" {
		t.Fatal("no browser is running, so there is nothing to restart")
	}

	// The client stays up. What is being tested is the browser coming back, not
	// the link, and killing the client would test something else.
	_ = exec.Command("pkill", "-f", filepath.Join(client.State, "browser")).Run()
	// Its profile too, so nothing can come back through it: the folder on disk
	// has to be the only thing left that could carry a sign-in.
	_ = os.RemoveAll(filepath.Join(client.State, "browser-profile"))
	waitFor(t, "the browser to be gone", func() bool { return browserPid(t, client) == "" })

	// Starting one is the next call's job, so this only proves it went. What
	// proves a new one arrived is the pid AFTER, checked by the caller through
	// browserPid once it has navigated again.
	t.Cleanup(func() {
		if now := browserPid(t, client); now != "" && now == was {
			t.Errorf("the browser was never restarted: still process %s", now)
		}
	})
}

// browserPid is the browser this client started, or empty if there is none.
//
// Matched on the browser's own directory, which is this test's scratch and
// nobody else's. Only the PARENT carries that path (its children show the
// resolved symlink, measured), which is exactly what is wanted here: the
// parent is the one holding the protocol endpoint and the cookie jar.
func browserPid(t *testing.T, client *rustClient) string {
	t.Helper()
	out, _ := exec.Command("pgrep", "-f", filepath.Join(client.State, "browser")).Output()
	return strings.TrimSpace(string(out))
}

// tabsOf is how many tabs one agent has open, asked of the far side.
//
// Counting processes was the first attempt and it does not work: only the
// PARENT process carries the browser's own path, and its children show the
// resolved symlink, so pgrep answers one however many tabs are open. The same
// trap as counting by the profile path in KB/39.
func tabsOf(t *testing.T, r *Registry, owner int) int {
	t.Helper()
	args, err := json.Marshal(map[string]any{
		"action": "tabs", "operation": "list", "conversation": owner,
	})
	if err != nil {
		t.Fatalf("arguments: %v", err)
	}
	answer, err := r.Call(context.Background(), 5, 11, "the-laptop", browserTool, args, "counting")
	if err != nil || !answer.OK {
		t.Fatalf("could not list an agent's tabs: %v %v", err, answer.Message)
	}
	var content struct {
		Tabs []any `json:"tabs"`
	}
	if err := json.Unmarshal(answer.Content, &content); err != nil {
		t.Fatalf("unreadable tab list: %v", err)
	}
	return len(content.Tabs)
}

// referenceFor is the reference the outline gave something, found the way a
// model reading it would.
func referenceFor(t *testing.T, outline, naming string) string {
	t.Helper()
	for _, line := range strings.Split(outline, "\n") {
		if !strings.Contains(line, naming) {
			continue
		}
		_, after, found := strings.Cut(line, "[ref=")
		if !found {
			t.Fatalf("%s carries no reference: %s", naming, line)
		}
		reference, _, _ := strings.Cut(after, "]")
		return reference
	}
	t.Fatalf("%s is not in the outline:\n%s", naming, outline)
	return ""
}
