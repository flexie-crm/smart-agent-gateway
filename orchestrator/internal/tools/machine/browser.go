package machine

import (
	"encoding/json"

	"flexie.io/sag/internal/tool"
)

// The browser: reading and using web pages, on the person's own computer.
//
// ONE tool, with the operation as an argument, which is the shape the terminal
// already has. Nine separate tools was the first cut and it was wrong for this:
// nine rows in the tools table and nine grants, for one capability that an
// administrator either wants or does not.
//
// # The words are Playwright's
//
// What this runs is Playwright's own automation script, so what the model is
// told is Playwright's own documentation, taken from the two places it lives in
// the package we ship:
//
//   - `playwright-core/types/types.d.ts`, the full doc comment for every method
//     (what `click` waits for, what `fill` does to a field, which keys `press`
//     understands);
//   - their MCP tool definitions inside `playwright-core/lib/coreBundle`, which
//     is where the parameter names and their one-line descriptions come from.
//
// Their sentences are kept as they wrote them. Three kinds of edit and no
// others: a clause naming an option this build does not offer is gone rather
// than reworded (their step is accurate without it); a cross-reference to the
// Playwright library API is gone, because a model here is not holding the
// library; and a reference to one of their TOOL names becomes the action it is
// here, since `browser_network_requests` is a thing a caller cannot call and
// `network_requests` is the same thing they can. Nothing here is a paraphrase.
//
// # Two parameters carry another name
//
// One tool with an `action` argument leaves nowhere for a second one to go, and
// `browser_tabs` is the one tool of the twenty whose own schema declares
// `action` (checked: it is the only one). It also declares `index`, which
// `browser_network_request` declares too, counting differently (tabs from zero,
// requests from one), so one name could not carry both meanings. So on the tabs
// action only, their `action` is `operation` and their `index` is `tab`. Their
// DESCRIPTIONS of both are kept word for word, and nothing else moves.
//
// `browser_test.go` holds every one of those strings as a literal and fails
// when this file drifts from them, which is also where to look when bumping
// Playwright: it says which word changed.

const BrowserName = "browser"

// The version of this tool's arguments.
// Three, because the arguments grew again: an application on an older version
// is offered a browser it would refuse half the calls to.
const browserVersion = 3

func browserSchema() tool.Schema {
	return tool.Schema{
		Name:         BrowserName,
		FriendlyName: "Browser",
		// Short, and the depth is one tool_guide away. What belongs here is
		// what the tool is and the one thing a caller must know before reaching
		// for the guide, which is the order: look at the page, then act on
		// something it named.
		Description: "Reads and uses web pages in a browser on the person's own computer. Take a " +
			"snapshot first: it lists every control on the page with a ref, and a ref is what the " +
			"other actions point at. Every action answers with a fresh snapshot, so you can see " +
			"what changed without asking again. It runs Playwright, so its behaviour and its " +
			"arguments are Playwright's. Read its guide before using it.",
		About: "Lets the assistant read and use web pages: open an address, see what is on the " +
			"page, and click, type and choose on it. The browser runs on the person's own " +
			"computer and has no window, so nobody sees it. It keeps its own cookies, separate " +
			"from the person's own browser. Local addresses work, so it can be pointed at a site " +
			"being built or an admin page on the office network.",
		Kind: tool.KindBuiltin,
		// The same level as http_request, and for the same reason: what this
		// does is reach a place outside the workspace. It fetches pages, and
		// it clicks and types on them on somebody's behalf.
		//
		// It was destructive_action, and that was wrong in a way a person read:
		// the approval card is keyed to this, and destructive_action makes it
		// say "This deletes or overwrites something, and it cannot be undone",
		// which a browser does not do. external_communication makes it say
		// "This sends something to a place outside your workspace. Read what it
		// would send before you allow it", which is what is actually true.
		//
		// A form it submits CAN delete something at the far end, and that is
		// equally true of an http_request with a DELETE method. The level
		// describes what the tool is, not the worst thing a remote service
		// might do with it, and one tool rather than nine means this level
		// covers the reading as well as the acting.
		Risk:        tool.RiskExternalCommunication,
		InputSchema: browserArguments,
		Guide:       browserGuide,
		Topics:      browserTopics,
		Shown: tool.Display{
			Where: []string{"action", "element", "url"},
			Sent: []tool.Shown{
				tool.Value("target"), tool.Text("text"), tool.Value("key"),
				tool.Value("values"), tool.Value("submit"), tool.Value("slowly"),
				tool.Value("doubleClick"), tool.Value("button"), tool.Value("modifiers"),
				tool.Value("depth"), tool.Value("boxes"),
				tool.Value("fields"), tool.Value("startTarget"), tool.Value("endTarget"),
				tool.Value("paths"), tool.Value("data"), tool.Value("time"),
				tool.Value("textGone"), tool.Value("width"), tool.Value("height"),
				tool.Value("operation"), tool.Value("tab"), tool.Value("accept"),
				tool.Value("promptText"), tool.Value("level"), tool.Value("all"),
				tool.Value("static"), tool.Value("filter"), tool.Value("index"),
				tool.Value("part"),
				// As code, not as text. It is the one field here a person has
				// to READ before allowing it, and text renders it as one
				// colour.
				tool.JavaScript("script"),
			},
			Answered: []tool.Shown{
				tool.Value("url"), tool.Value("title"), tool.Value("selected"),
				tool.Value("closed"),
				// The page outline is YAML: Playwright's ariaSnapshot answers
				// with a tree of roles and names. As text it is a paragraph of
				// identifiers; as YAML the shape is what a person scans.
				tool.YAML("snapshot"),
				tool.Value("filled"), tool.Value("dragged"), tool.Value("dropped"),
				tool.Value("uploaded"), tool.Value("waited"), tool.Value("width"),
				tool.Value("height"), tool.Value("tabs"), tool.Value("handled"),
				tool.Value("accepted"), tool.Value("messages"), tool.Value("requests"),
				tool.Value("status"), tool.Value("note"),
				tool.Value("js_result"), tool.Value("saved"), tool.Value("forgotten"),
				tool.Value("signed_in"),
			},
		},
	}
}

// The arguments, with Playwright's own descriptions.
//
// An interpreted string rather than a raw one, because several of their
// descriptions contain backticks (`ArrowLeft`, `<input>`) and a Go raw string is
// delimited by them. Assembled with the encoder would have been an option and
// is worse: a reader of this file could not then see what the model is told.
var browserArguments = json.RawMessage(`{
	"type": "object",
	"properties": {
		"action": {
			"type": "string",
			"enum": ["navigate", "navigate_back", "snapshot", "click", "type", "fill_form",
			         "press_key", "select_option", "hover", "drag", "drop", "file_upload",
			         "wait_for", "resize", "tabs", "handle_dialog", "console_messages",
			         "network_requests", "network_request", "exec_js", "save_session",
		         "forget_session", "close"],
			"description": "What to do. navigate: Navigate to a URL. navigate_back: Go back to the previous page in the history. snapshot: Capture accessibility snapshot of the current page, this is better than screenshot. click: Perform click on a web page. type: Type text into editable element. fill_form: Fill multiple form fields. press_key: Press a key on the keyboard. select_option: Select an option in a dropdown. hover: Hover over element on page. drag: Perform drag and drop between two elements. drop: Drop files or MIME-typed data onto an element, as if dragged from outside the page. At least one of \"paths\" or \"data\" must be provided. file_upload: Upload one or multiple files. wait_for: Wait for text to appear or disappear or a specified time to pass. resize: Resize the browser window. tabs: List, create, close, or select a browser tab. handle_dialog: Handle a dialog. console_messages: Returns all console messages. network_requests: Returns a numbered list of network requests since loading the page. Use network_request with the number to get full details. network_request: Returns full details (headers and body) of a single network request, or a single part if ` + "`" + `part` + "`" + ` is set. Use the number from network_requests. exec_js: Run JavaScript in the page and return what it produces. save_session: Keep this site signed in on this computer, past the end of the application. forget_session: Forget a site's saved sign-in, which is what signing out is. close: Close the page."
		},
		"url": {
			"type": "string",
			"description": "The URL to navigate to (navigate). URL to navigate to in the new tab, used for new (tabs)."
		},
		"target": {
			"type": "string",
			"description": "Exact target element reference from the page snapshot, or a unique element selector"
		},
		"element": {
			"type": "string",
			"description": "Human-readable element description used to obtain permission to interact with the element"
		},
		"text": {
			"type": "string",
			"description": "Text to type into the element (type). The text to wait for (wait_for)."
		},
		"submit": {
			"type": "boolean",
			"description": "Whether to submit entered text (press Enter after)"
		},
		"slowly": {
			"type": "boolean",
			"description": "Whether to type one character at a time. Useful for triggering key handlers in the page. By default entire text is filled in at once."
		},
		"fields": {
			"type": "array",
			"description": "Fields to fill in",
			"items": {
				"type": "object",
				"properties": {
					"element": {"type": "string", "description": "Human-readable element description used to obtain permission to interact with the element"},
					"target": {"type": "string", "description": "Exact target element reference from the page snapshot, or a unique element selector"},
					"name": {"type": "string", "description": "Human-readable field name"},
					"type": {"type": "string", "enum": ["textbox", "checkbox", "radio", "combobox", "slider"], "description": "Type of the field"},
					"value": {"type": "string", "description": "Value to fill in the field. If the field is a checkbox, the value should be ` + "`" + `true` + "`" + ` or ` + "`" + `false` + "`" + `. If the field is a combobox, the value should be the text of the option."}
				},
				"required": ["target", "name", "type", "value"]
			}
		},
		"key": {
			"type": "string",
			"description": "Name of the key to press or a character to generate, such as ` + "`" + `ArrowLeft` + "`" + ` or ` + "`" + `a` + "`" + `"
		},
		"values": {
			"type": "array",
			"items": {"type": "string"},
			"description": "Array of values to select in the dropdown. This can be a single value or multiple values."
		},
		"doubleClick": {
			"type": "boolean",
			"description": "Whether to perform a double click instead of a single click"
		},
		"button": {
			"type": "string",
			"enum": ["left", "right", "middle"],
			"description": "Button to click, defaults to left"
		},
		"modifiers": {
			"type": "array",
			"items": {"type": "string", "enum": ["Alt", "Control", "ControlOrMeta", "Meta", "Shift"]},
			"description": "Modifier keys to press"
		},
		"startElement": {
			"type": "string",
			"description": "Human-readable source element description used to obtain the permission to interact with the element"
		},
		"startTarget": {
			"type": "string",
			"description": "Exact target element reference from the page snapshot, or a unique element selector"
		},
		"endElement": {
			"type": "string",
			"description": "Human-readable target element description used to obtain the permission to interact with the element"
		},
		"endTarget": {
			"type": "string",
			"description": "Exact target element reference from the page snapshot, or a unique element selector"
		},
		"paths": {
			"type": "array",
			"items": {"type": "string"},
			"description": "The absolute paths to the files to upload. Can be single file or multiple files. If omitted, file chooser is cancelled (file_upload). Absolute paths to files to drop onto the element (drop)."
		},
		"data": {
			"type": "object",
			"additionalProperties": {"type": "string"},
			"description": "Data to drop, as a map of MIME type to string value (e.g. {\"text/plain\": \"hello\", \"text/uri-list\": \"https://example.com\"})."
		},
		"time": {
			"type": "number",
			"description": "The time to wait in seconds"
		},
		"textGone": {
			"type": "string",
			"description": "The text to wait for to disappear"
		},
		"width": {
			"type": "number",
			"description": "Width of the browser window"
		},
		"height": {
			"type": "number",
			"description": "Height of the browser window"
		},
		"operation": {
			"type": "string",
			"enum": ["list", "new", "close", "select"],
			"description": "Operation to perform"
		},
		"tab": {
			"type": "number",
			"description": "Tab index, used for close/select. If omitted for close, current tab is closed."
		},
		"accept": {
			"type": "boolean",
			"description": "Whether to accept the dialog."
		},
		"promptText": {
			"type": "string",
			"description": "The text of the prompt in case of a prompt dialog."
		},
		"level": {
			"type": "string",
			"enum": ["error", "warning", "info", "debug"],
			"description": "Level of the console messages to return. Each level includes the messages of more severe levels. Defaults to \"info\"."
		},
		"all": {
			"type": "boolean",
			"description": "Return all console messages since the beginning of the session, not just since the last navigation. Defaults to false."
		},
		"static": {
			"type": "boolean",
			"description": "Whether to include successful static resources like images, fonts, scripts, etc. Defaults to false."
		},
		"filter": {
			"type": "string",
			"description": "Only return requests whose URL matches this regexp (e.g. \"/api/.*user\")."
		},
		"index": {
			"type": "number",
			"description": "1-based index of the request, as printed by network_requests."
		},
		"part": {
			"type": "string",
			"enum": ["request-headers", "request-body", "response-headers", "response-body"],
			"description": "Return only this part of the request. Omit to return full details."
		},
		"script": {
			"type": "string",
			"description": "JavaScript to run in the page. The snippet is the BODY of an async function: write plain statements and use a bare ` + "`" + `return` + "`" + ` to send back a JSON-serializable value, which arrives in ` + "`" + `js_result` + "`" + `. ` + "`" + `await` + "`" + ` works at the top level. Do NOT wrap it in an IIFE: ` + "`" + `(async () => { ... })()` + "`" + ` executes but its value is discarded, so ` + "`" + `js_result` + "`" + ` comes back null with no error."
		},
		"depth": {
			"type": "number",
			"description": "Limit the depth of the snapshot tree"
		},
		"boxes": {
			"type": "boolean",
			"description": "Include each element's bounding box as [box=x,y,width,height] in the snapshot. Coordinates are viewport-relative, in CSS pixels (Element.getBoundingClientRect)"
		}
	},
	"required": ["action"]
}`)

// browserTools is the one, kept as a list because that is what the registry
// takes and because a second one may yet be right (an eval that is its own
// grant, for instance).
func browserTools(machines Machines) []tool.Tool {
	schema := browserSchema()
	return []tool.Tool{{Schema: schema, Handle: Dispatch(machines, schema, schema.Name)}}
}
