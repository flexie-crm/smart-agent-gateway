package machine

import (
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/tool"
)

// The tool a caller reads, and the actions it takes.
//
// It holds Playwright's strings as LITERALS on purpose. A test that read the
// same source as the code could not disagree with it, and disagreeing is the
// whole job: this is what says which word changed when Playwright is bumped.
//
// Their descriptions come from the MCP tool definitions inside
// `playwright-core/lib/coreBundle`; the longer prose in the topics comes from
// the doc comments in `playwright-core/types/types.d.ts`.
func TestEveryActionIsPlaywrightsOwn(t *testing.T) {
	// Their tool name against the action that stands for it here, and their
	// one-line description of it.
	want := map[string]string{
		"navigate":         "Navigate to a URL",
		"navigate_back":    "Go back to the previous page in the history",
		"snapshot":         "Capture accessibility snapshot of the current page, this is better than screenshot",
		"click":            "Perform click on a web page",
		"type":             "Type text into editable element",
		"fill_form":        "Fill multiple form fields",
		"press_key":        "Press a key on the keyboard",
		"select_option":    "Select an option in a dropdown",
		"hover":            "Hover over element on page",
		"drag":             "Perform drag and drop between two elements",
		"file_upload":      "Upload one or multiple files",
		"wait_for":         "Wait for text to appear or disappear or a specified time to pass",
		"resize":           "Resize the browser window",
		"tabs":             "List, create, close, or select a browser tab",
		"handle_dialog":    "Handle a dialog",
		"console_messages": "Returns all console messages",
		"close":            "Close the page",
		// Ours, not Playwright's: exec_js is Flexie's (a browser tool that has
		// been in use a long time), and the two session actions have no
		// equivalent anywhere, because nothing else keeps a sign-in on disk.
		"exec_js":        "Run JavaScript in the page and return what it produces",
		"save_session":   "Keep this site signed in on this computer, past the end of the application",
		"forget_session": "Forget a site's saved sign-in, which is what signing out is",
		// Two of theirs carry an edit, and it is the one this file's header
		// names: a reference to one of their TOOL names becomes the action it
		// is here, because `browser_network_requests` is a thing a caller
		// cannot call and `network_requests` is the same thing they can.
		"network_requests": "Returns a numbered list of network requests since loading the page. Use network_request with the number to get full details.",
		"network_request":  "Returns full details (headers and body) of a single network request, or a single part if `part` is set. Use the number from network_requests.",
		// Their sentence for this one quotes two of its own parameter names,
		// and the quotes are what make it worth asserting whole: a JSON schema
		// literal is where a stray quote breaks the parse, which has already
		// happened here once.
		"drop": `Drop files or MIME-typed data onto an element, as if dragged from outside the page. At least one of "paths" or "data" must be provided.`,
	}

	var parsed struct {
		Properties struct {
			Action struct {
				Enum        []string `json:"enum"`
				Description string   `json:"description"`
			} `json:"action"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(offeredBrowser(t).InputSchema, &parsed); err != nil {
		t.Fatalf("the browser's schema does not parse: %v", err)
	}

	for action, description := range want {
		if !containsString(parsed.Properties.Action.Enum, action) {
			t.Errorf("%s is not one of the actions, so it can never be asked for", action)
		}
		// Their sentence, unchanged. This is the compatibility claim: a model
		// that knows Playwright's browser reads exactly this.
		if !strings.Contains(parsed.Properties.Action.Description, description) {
			t.Errorf("the action list does not describe %s as Playwright does (%q)",
				action, description)
		}
	}
	for _, offered := range parsed.Properties.Action.Enum {
		if _, theirs := want[offered]; !theirs {
			t.Errorf("%s is offered and is not one of Playwright's actions", offered)
		}
	}
}

// Every parameter Playwright declares is declared here, with their wording.
//
// The names are half the compatibility claim (a Playwright-aware model sends
// these words and no others) and the descriptions are the other half, so both
// are asserted to the character.
func TestEveryParameterIsPlaywrightsOwn(t *testing.T) {
	want := map[string]string{
		"url":         "The URL to navigate to (navigate). URL to navigate to in the new tab, used for new (tabs).",
		"target":      "Exact target element reference from the page snapshot, or a unique element selector",
		"element":     "Human-readable element description used to obtain permission to interact with the element",
		"text":        "Text to type into the element (type). The text to wait for (wait_for).",
		"submit":      "Whether to submit entered text (press Enter after)",
		"slowly":      "Whether to type one character at a time. Useful for triggering key handlers in the page. By default entire text is filled in at once.",
		"key":         "Name of the key to press or a character to generate, such as `ArrowLeft` or `a`",
		"values":      "Array of values to select in the dropdown. This can be a single value or multiple values.",
		"doubleClick": "Whether to perform a double click instead of a single click",
		"button":      "Button to click, defaults to left",
		"modifiers":   "Modifier keys to press",
		"depth":       "Limit the depth of the snapshot tree",
		"boxes":       "Include each element's bounding box as [box=x,y,width,height] in the snapshot. Coordinates are viewport-relative, in CSS pixels (Element.getBoundingClientRect)",

		"fields":       "Fields to fill in",
		"startElement": "Human-readable source element description used to obtain the permission to interact with the element",
		"startTarget":  "Exact target element reference from the page snapshot, or a unique element selector",
		"endElement":   "Human-readable target element description used to obtain the permission to interact with the element",
		"endTarget":    "Exact target element reference from the page snapshot, or a unique element selector",
		"paths":        "The absolute paths to the files to upload. Can be single file or multiple files. If omitted, file chooser is cancelled (file_upload). Absolute paths to files to drop onto the element (drop).",
		"data":         `Data to drop, as a map of MIME type to string value (e.g. {"text/plain": "hello", "text/uri-list": "https://example.com"}).`,
		"time":         "The time to wait in seconds",
		"textGone":     "The text to wait for to disappear",
		"width":        "Width of the browser window",
		"height":       "Height of the browser window",
		"accept":       "Whether to accept the dialog.",
		"promptText":   "The text of the prompt in case of a prompt dialog.",
		"level":        `Level of the console messages to return. Each level includes the messages of more severe levels. Defaults to "info".`,
		"all":          "Return all console messages since the beginning of the session, not just since the last navigation. Defaults to false.",
		"static":       "Whether to include successful static resources like images, fonts, scripts, etc. Defaults to false.",
		"filter":       `Only return requests whose URL matches this regexp (e.g. "/api/.*user").`,
		"part":         "Return only this part of the request. Omit to return full details.",

		// Flexie's own wording for the script contract, which is the part
		// callers get wrong: the IIFE sentence is there because wrapping the
		// snippet discards its value and returns null with NO error.
		"script": "JavaScript to run in the page. The snippet is the BODY of an async function: write plain statements and use a bare `return` to send back a JSON-serializable value, which arrives in `js_result`. `await` works at the top level. Do NOT wrap it in an IIFE: `(async () => { ... })()` executes but its value is discarded, so `js_result` comes back null with no error.",

		// The two that carry another name, with their own descriptions
		// unchanged. See the note at the top of browser.go: this tool takes
		// `action` already, and `index` cannot mean both a tab (counted from
		// zero) and a request (counted from one).
		"operation": "Operation to perform",
		"tab":       "Tab index, used for close/select. If omitted for close, current tab is closed.",
		"index":     "1-based index of the request, as printed by network_requests.",
	}

	var parsed struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(offeredBrowser(t).InputSchema, &parsed); err != nil {
		t.Fatalf("the browser's schema does not parse: %v", err)
	}

	for name, description := range want {
		declared, there := parsed.Properties[name]
		if !there {
			t.Errorf("%s is not declared, and Playwright's schema has it", name)
			continue
		}
		if declared.Description != description {
			t.Errorf("%s is described as\n  %q\nand Playwright describes it as\n  %q",
				name, declared.Description, description)
		}
	}
	for declared := range parsed.Properties {
		if declared == "action" {
			continue // ours: one tool needs to be told which of theirs to run
		}
		if _, theirs := want[declared]; !theirs {
			t.Errorf("%s is declared and Playwright's schema does not have it", declared)
		}
	}

	// Only `action` is required, because what else is needed depends on it: a
	// url for navigating, a key for pressing, neither for closing. A schema
	// cannot say that, so the handler does, and requiring more here would make
	// close uncallable.
	if len(parsed.Required) != 1 || parsed.Required[0] != "action" {
		t.Errorf("the required list is %v and should be exactly [action]", parsed.Required)
	}
}

// One of Playwright's parameters is deliberately left out.
//
// `filename` saves the snapshot to a markdown file, which is the file tools'
// job. Declared and ignored it would be worse than absent: a model would write
// a report to a path and find nothing there. Asserted rather than commented, so
// declaring it has to come with a handler that honours it.
func TestTheOnlyParameterLeftOutIsFilename(t *testing.T) {
	if strings.Contains(string(offeredBrowser(t).InputSchema), "filename") {
		t.Error("the browser declares filename, and nothing writes a file")
	}
}

// The schema parses, quotes and backticks and all.
//
// This is the failure that shipped: a description holding `name="Save"i` was
// pasted into a JSON schema literal, the schema did not parse, and the tool was
// DROPPED and offered to nobody. Nothing errors when that happens, which is why
// it is asserted rather than trusted, and it is why the schema is built from an
// interpreted string: Playwright's wording contains backticks, and a Go raw
// string is delimited by them.
func TestTheBrowserSchemaParses(t *testing.T) {
	schema := offeredBrowser(t)

	var parsed map[string]any
	if err := json.Unmarshal(schema.InputSchema, &parsed); err != nil {
		t.Fatalf("the browser has a schema that does not parse (%v): %s", err, schema.InputSchema)
	}
	if parsed["type"] != "object" {
		t.Error("the browser does not take an object")
	}
	if !strings.Contains(string(schema.InputSchema), "`ArrowLeft`") {
		t.Error("the key description lost the backticks Playwright's own has")
	}

	// A risk level is what the store stores and what approval reads. Empty is
	// not a value it has.
	if schema.Risk == "" {
		t.Error("the browser has no risk level, which the store will refuse")
	}
	// And an administrator has to be able to read what it is for, in words that
	// are not the model's.
	if schema.About == "" {
		t.Error("the browser has nothing for an administrator to read")
	}
	if schema.About == schema.Description {
		t.Error("the browser shows an administrator the model's own description")
	}
}

// The drilldown graph is reachable and its edges go somewhere.
//
// A topic that points at an id nothing defines is a dead end the model cannot
// see coming: tool_guide offers the edge, the model follows it, and there is
// nothing there.
func TestTheBrowserTopicsFormAWholeGraph(t *testing.T) {
	known := map[string]bool{}
	for _, topic := range browserTopics {
		if known[topic.ID] {
			t.Errorf("%s is defined twice", topic.ID)
		}
		known[topic.ID] = true
		if topic.Title == "" || topic.Body == "" {
			t.Errorf("%s has no title or no body", topic.ID)
		}
	}
	if len(browserTopics) < 16 {
		t.Errorf("only %d topics: twenty actions want more than that", len(browserTopics))
	}
	for _, topic := range browserTopics {
		for _, edge := range topic.Edges {
			if !known[edge.To] {
				t.Errorf("%s points at %s, which no topic defines", topic.ID, edge.To)
			}
			if edge.When == "" {
				t.Errorf("%s -> %s does not say when to follow it", topic.ID, edge.To)
			}
			if edge.Type == "" {
				t.Errorf("%s -> %s has no edge type", topic.ID, edge.To)
			}
		}
	}

	// Every action the tool takes has somewhere to read about it, because the
	// guide is where a caller goes after a refusal. Reading a topic body for
	// the word is enough: what this catches is an action added with no
	// documentation at all.
	whole := ""
	for _, topic := range browserTopics {
		whole += topic.ID + " " + topic.Title + " " + topic.Body + " "
	}
	for _, action := range []string{
		"snapshot", "click", "type", "fill_form", "press_key", "select_option",
		"drag", "drop", "file_upload", "wait_for", "resize", "tabs",
		"handle_dialog", "console_messages", "network_requests", "network_request",
		"exec_js", "save_session", "forget_session",
	} {
		if !strings.Contains(whole, action) {
			t.Errorf("no topic mentions %s, so a caller stuck on it has nowhere to go", action)
		}
	}

	// And the graph has to be carried by the tool, or it is documentation
	// nobody can find.
	schema := offeredBrowser(t)
	if len(schema.Topics) == 0 {
		t.Error("the browser carries no topics, so tool_guide can never offer them")
	}
	if len(schema.Guide) == 0 {
		t.Error("the browser carries no guide, so tool_guide has nothing flat to answer with")
	}
}

// One tool, not nine.
//
// The nine-tool shape was the first cut: nine rows in the tools table and nine
// grants for one capability an administrator either wants or does not. Asserted
// because the way back is easy and quiet.
func TestTheBrowserIsOneTool(t *testing.T) {
	var offered []string
	for _, it := range Tools(nil) {
		if strings.Contains(it.Schema.Name, "browser") {
			offered = append(offered, it.Schema.Name)
		}
	}
	if len(offered) != 1 || offered[0] != BrowserName {
		t.Errorf("the browser is offered as %v and should be exactly [%s]", offered, BrowserName)
	}
}

// The one tool, as the registry offers it.
func offeredBrowser(t *testing.T) tool.Schema {
	t.Helper()
	for _, it := range Tools(nil) {
		if it.Schema.Name == BrowserName {
			return it.Schema
		}
	}
	t.Fatal("the browser is not offered at all, which is what happens when its name has " +
		"nothing decided about it")
	return tool.Schema{}
}

func containsString(list []string, want string) bool {
	for _, have := range list {
		if have == want {
			return true
		}
	}
	return false
}
