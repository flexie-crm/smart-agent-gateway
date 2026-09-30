package machine

import (
	"encoding/json"

	"flexie.io/sag/internal/tool"
)

// What the model is told about the browser, beyond its arguments.
//
// Separate from browser.go because it is somebody else's prose and there is a
// lot of it: the schema is a page, and this is ten topics. Both are read by the
// same tests.
//
// # Where every sentence came from
//
// Playwright's own documentation, in the package we ship. The per-action
// sections below are the doc comments from
// `playwright-core/types/types.d.ts` for the method each action runs: `click`,
// `fill`, `press`, `selectOption`, `hover`. They are their sentences, not a
// summary of them.
//
// Two kinds of edit, and no others. A clause naming an option this build does
// not offer (`force`, `position`, `noWaitAfter`, `timeout`) is removed, because
// their step is accurate for us without it and keeping it would promise an
// argument that does not exist. And their cross-references to the library API
// ("use locator-based ... instead") are removed, because the library is not what
// a model here is holding.
//
// The topics that are OURS are labelled as such in their bodies: the loop, how
// a ref works, and what is absent. Those are about this arrangement rather than
// about Playwright, so there was nothing of theirs to copy.

// The flat guide: what to do, and in what order.
var browserGuide = json.RawMessage(`{
	"summary": "A headless browser on the person's own computer. Nobody sees it. It runs Playwright, so the arguments and the behaviour are Playwright's.",
	"the_order_that_works": [
		"action: navigate, with a url.",
		"action: snapshot, which lists every control with a ref.",
		"action: click / type / fill_form / select_option / hover / drag, with target set to a ref from that snapshot.",
		"Every action that changes the page answers with a fresh snapshot, so you do not need to ask for one after acting."
	],
	"naming_an_element": {
		"target": "Playwright's own words: \"Exact target element reference from the page snapshot, or a unique element selector\". A ref (e12) is the one to use, because it names exactly one element and cannot be ambiguous. A CSS selector such as #email also works.",
		"element": "Playwright's own words: \"Human-readable element description used to obtain permission to interact with the element\". It finds nothing. It is the sentence a person reads when they are asked to approve the action, so write it for them."
	},
	"which_arguments_go_with_which_action": {
		"navigate": "url",
		"exec_js": "script",
		"save_session": "nothing. It keeps the site the page is on",
		"forget_session": "url, or nothing to forget the site the page is on",
		"navigate_back": "nothing",
		"snapshot": "target, depth, boxes (all optional)",
		"click": "target, and optionally element, doubleClick, button, modifiers",
		"type": "target and text, and optionally element, submit, slowly",
		"fill_form": "fields, a list of {target, name, type, value}",
		"press_key": "key. It takes no target: the key goes to whatever the page has focused",
		"select_option": "target and values",
		"hover": "target",
		"drag": "startTarget and endTarget, and optionally startElement, endElement",
		"drop": "target, and paths or data (at least one of them)",
		"file_upload": "paths. Only after something opened a file chooser",
		"wait_for": "one or more of time, text, textGone",
		"resize": "width and height",
		"tabs": "operation (list, new, close, select), and tab or url where the operation needs one",
		"handle_dialog": "accept, and promptText for a prompt. Only while a dialog is open",
		"console_messages": "level, all (both optional)",
		"network_requests": "static, filter (both optional)",
		"network_request": "index, and part (optional)",
		"close": "nothing"
	},
	"parameters": {
		"action": "Which of the twenty things to do. Everything else depends on this.",
		"operation": "The tabs action's own operation. It is Playwright's \"action\" parameter under another name, because this tool's action argument already means which of their tools to run.",
		"tab": "The tabs action's index, counted from 0. It is Playwright's \"index\" parameter under another name, because index here is the network one, counted from 1."
	},
	"notes": [
		"Local addresses work on purpose: a site being built on localhost, or an admin page on the office network, is a main use.",
		"Search engines refuse headless browsers. Google answers with an unusual-traffic page and DuckDuckGo with a captcha. Use this for an address you know.",
		"While a dialog or a file chooser is open, every other action refuses: the page runs nothing until it is answered.",
		"There are no screenshots, no network mocking or offline mode, and no tracing or video in this build."
	]
}`)

// The drilldown graph. Ten topics, because the questions that strand a caller
// come after a refusal rather than before the first call.
var browserTopics = []tool.Topic{{
	ID:    "browser/order",
	Title: "The order that works",
	Body: "This part is about this tool rather than about Playwright.\n\n" +
		"Take a snapshot before acting. It names every control on the page and gives each a ref, " +
		"and a ref is what target wants. Acting without looking means guessing a selector.\n\n" +
		"Every action answers with a fresh snapshot of its own, so after clicking something you " +
		"can already see what changed and do not need a second call to find out.\n\n" +
		"The page stays open between calls with its own cookies, so a site signed into once stays " +
		"signed in for the rest of the conversation. Use close when finished with a site.\n\n" +
		"NAVIGATE REPLACES what is in your page: it does not open another one, the same way " +
		"typing an address replaces what a browser window is showing. So navigate as often as " +
		"you like, including back to a page you have already been on. You have one page unless " +
		"you deliberately ask for another.",
	Edges: []tool.TopicEdge{
		{To: "browser/target", Type: tool.EdgePrerequisite, When: "before your first action: it is how you say which element"},
		{To: "browser/snapshot", Type: tool.EdgeCompanion, When: "to read what the snapshot is telling you"},
		{To: "browser/refusals", Type: tool.EdgeConsumer, When: "an action came back refused"},
		{To: "browser/blocked", Type: tool.EdgeConsumer, When: "every action is refusing and the reason mentions waiting"},
		{To: "browser/debugging", Type: tool.EdgeAlternative, When: "the page is not doing what it should and you want to know why"},
	},
}, {
	ID:    "browser/target",
	Title: "target and element",
	Body: "Playwright describes target as \"Exact target element reference from the page snapshot, " +
		"or a unique element selector\". It takes either.\n\n" +
		"A REF is the one to use. The snapshot marks each control [ref=e12], and passing e12 " +
		"resolves to exactly that element: it cannot be ambiguous and it cannot be a guess, which " +
		"is why every refusal about matching several things is avoidable by using one.\n\n" +
		"A SELECTOR works for something the snapshot does not list: a CSS selector such as " +
		"#email, or one of Playwright's own. It is the fallback rather than the default.\n\n" +
		"element is described as \"Human-readable element description used to obtain permission to " +
		"interact with the element\". It finds nothing. It is the sentence a person reads when " +
		"they are asked to approve the action, so write it for them: \"the Save button\", not a " +
		"selector.",
	Edges: []tool.TopicEdge{
		{To: "browser/snapshot", Type: tool.EdgeProducer, When: "to get the refs in the first place"},
		{To: "browser/refusals", Type: tool.EdgeConsumer, When: "your target matched nothing, or matched several"},
	},
}, {
	ID:    "browser/snapshot",
	Title: "The snapshot",
	Body: "Playwright calls it \"accessibility snapshot of the current page, this is better than " +
		"screenshot\".\n\n" +
		"It reads as a tree:\n\n" +
		"    - textbox \"Your name\" [ref=e2]\n" +
		"    - button \"Save\" [disabled] [ref=e3]\n" +
		"    - button \"Cancel\" [ref=e4]\n\n" +
		"The role and the name are what a person would see, the brackets say what state something " +
		"is in, and the ref is what target wants.\n\n" +
		"target narrows it to one part of the page. depth is \"Limit the depth of the snapshot " +
		"tree\". boxes is \"Include each element's bounding box as [box=x,y,width,height] in the " +
		"snapshot. Coordinates are viewport-relative, in CSS pixels\".\n\n" +
		"An element that cannot be seen is not in the snapshot at all, which is deliberate: it " +
		"could not be acted on anyway.",
	Edges: []tool.TopicEdge{
		{To: "browser/target", Type: tool.EdgeConsumer, When: "you have the snapshot and want to act on something in it"},
		{To: "browser/absent", Type: tool.EdgeAlternative, When: "you wanted a picture of the page rather than its structure"},
	},
}, {
	ID:    "browser/actionability",
	Title: "Actionability: what is checked first",
	Body: "Playwright checks an element can actually be used before acting on it, which is why a " +
		"call can be refused about something plainly visible in the snapshot. The states are " +
		"visible, stable (it has stopped moving), enabled, and for typing, editable.\n\n" +
		"It also verifies the action would land on the element you named rather than on something " +
		"covering it, which is the check that catches a cookie banner, an overlay or a modal you " +
		"had not noticed.",
	Edges: []tool.TopicEdge{
		{To: "browser/refusals", Type: tool.EdgeConsumer, When: "to read what a particular refusal means"},
		{To: "browser/click", Type: tool.EdgeCompanion, When: "you are about to click something"},
	},
}, {
	ID:    "browser/refusals",
	Title: "Reading a refusal",
	Body: "Each one wants a different next move.\n\n" +
		"\"it is not visible\" means the element is in the page and cannot be seen, so a person " +
		"could not have clicked it either. Something has to be opened first.\n\n" +
		"\"it is not enabled\" is a disabled control. Something else usually has to be done first, " +
		"often filling a required field.\n\n" +
		"\"it is not stable\" means it is still moving, so an animation is running. Take another " +
		"snapshot and act again.\n\n" +
		"\"it is not editable\" is a read-only field, or not a field at all.\n\n" +
		"\"strict mode violation\" means the selector matched several elements, so nothing was " +
		"done rather than guessing which. Use a ref from the snapshot.\n\n" +
		"\"nothing on this page matches\" means take another snapshot: the page has probably " +
		"navigated, or the control only appears after something else.",
	Edges: []tool.TopicEdge{
		{To: "browser/target", Type: tool.EdgePrerequisite, When: "the refusal was about matching nothing or several"},
		{To: "browser/actionability", Type: tool.EdgePrerequisite, When: "the refusal named a state such as visible or enabled"},
	},
}, {
	ID:    "browser/click",
	Title: "click",
	Body: "Playwright's own account of what it does:\n\n" +
		"This method clicks the element by performing the following steps:\n" +
		"1. Wait for actionability checks on the element.\n" +
		"2. Scroll the element into view if needed.\n" +
		"3. Use the mouse to click in the center of the element.\n" +
		"4. Wait for initiated navigations to either succeed or fail.\n\n" +
		"If the element is detached from the DOM at any moment during the action, this method " +
		"throws.\n\n" +
		"So a click that causes a page to load has already waited for that load by the time you " +
		"are answered.\n\n" +
		"doubleClick is \"Whether to perform a double click instead of a single click\": a page " +
		"listening for a double click hears nothing from two single ones. button is \"Button to " +
		"click, defaults to left\". modifiers is \"Modifier keys to press\", from Alt, Control, " +
		"ControlOrMeta, Meta and Shift; ControlOrMeta means the shortcut key people use on this " +
		"computer, which is Command on a Mac and Control elsewhere.\n\n" +
		"A checkbox is clicked rather than set, and clicking one that is already ticked turns it " +
		"off, so read its state in the snapshot first.",
	Edges: []tool.TopicEdge{
		{To: "browser/actionability", Type: tool.EdgePrerequisite, When: "before clicking, to know what is checked"},
		{To: "browser/type", Type: tool.EdgeCompanion, When: "you are filling in a form"},
	},
}, {
	ID:    "browser/type",
	Title: "type",
	Body: "Playwright's own account of what it does:\n\n" +
		"This method waits for actionability checks, focuses the element, fills it and triggers " +
		"an `input` event after filling. Note that you can pass an empty string to clear the " +
		"input field.\n\n" +
		"If the target element is not an `<input>`, `<textarea>` or `[contenteditable]` element, " +
		"this method throws an error. However, if the element is inside the `<label>` element " +
		"that has an associated control, the control will be filled instead.\n\n" +
		"So it REPLACES what is in the field rather than appending, an empty string clears it, " +
		"and pointing it at a label works.\n\n" +
		"submit is \"Whether to submit entered text (press Enter after)\", which is how most " +
		"forms are sent and saves a separate press_key.\n\n" +
		"slowly is \"Whether to type one character at a time. Useful for triggering key handlers " +
		"in the page. By default entire text is filled in at once.\" Reach for it when a page " +
		"reacts as you type: a search box that filters, a field that validates each character. " +
		"Otherwise leave it, because it is one round trip per character.",
	Edges: []tool.TopicEdge{
		{To: "browser/press_key", Type: tool.EdgeAlternative, When: "you want a key press rather than text"},
		{To: "browser/refusals", Type: tool.EdgeConsumer, When: "it said the element is not editable"},
	},
}, {
	ID:    "browser/press_key",
	Title: "press_key",
	Body: "Playwright's own account of what it does:\n\n" +
		"Focuses the element, and then uses keyboard.down(key) and keyboard.up(key).\n\n" +
		"`key` can specify the intended keyboardEvent.key value or a single character to " +
		"generate the text for. Examples of the keys are:\n\n" +
		"`F1` - `F12`, `Digit0`- `Digit9`, `KeyA`- `KeyZ`, `Backquote`, `Minus`, `Equal`, " +
		"`Backslash`, `Backspace`, `Tab`, `Delete`, `Escape`, `ArrowDown`, `End`, `Enter`, " +
		"`Home`, `Insert`, `PageDown`, `PageUp`, `ArrowRight`, `ArrowUp`, etc.\n\n" +
		"Following modification shortcuts are also supported: `Shift`, `Control`, `Alt`, `Meta`, " +
		"`ShiftLeft`, `ControlOrMeta`.\n\n" +
		"Holding down `Shift` will type the text that corresponds to the `key` in the upper " +
		"case.\n\n" +
		"If `key` is a single character, it is case-sensitive, so the values `a` and `A` will " +
		"generate different respective texts.\n\n" +
		"Shortcuts such as `key: \"Control+o\"`, `key: \"Control++` or `key: \"Control+Shift+T\"` " +
		"are supported as well. When specified with the modifier, modifier is pressed and being " +
		"held while the subsequent key is being pressed.\n\n" +
		"This action takes no target: the key goes to whatever the page has focused, so focus " +
		"something first by typing into it or clicking it. It is also the one action where the " +
		"person approving cannot be shown what it will land on.",
	Edges: []tool.TopicEdge{
		{To: "browser/type", Type: tool.EdgeAlternative, When: "you want to enter text rather than press one key"},
		{To: "browser/click", Type: tool.EdgeCompanion, When: "the shortcut needs the mouse as well"},
	},
}, {
	ID:    "browser/select_option",
	Title: "select_option",
	Body: "Playwright's own account of what it does:\n\n" +
		"This method waits for actionability checks, waits until all specified options are " +
		"present in the `<select>` element and selects these options.\n\n" +
		"If the target element is not a `<select>` element, this method throws an error. " +
		"However, if the element is inside the `<label>` element that has an associated control, " +
		"the control will be used instead.\n\n" +
		"Returns the array of option values that have been successfully selected.\n\n" +
		"Triggers a `change` and `input` event once all the provided options have been " +
		"selected.\n\n" +
		"values is \"Array of values to select in the dropdown. This can be a single value or " +
		"multiple values.\" A value matches either the option's underlying value or the label a " +
		"person reads, so use whichever the snapshot showed. Several values only work on a " +
		"dropdown that allows several.",
	Edges: []tool.TopicEdge{
		{To: "browser/snapshot", Type: tool.EdgePrerequisite, When: "to see which options a dropdown has"},
	},
}, {
	ID:    "browser/absent",
	Title: "What this browser cannot do",
	Body: "This part is about this build rather than about Playwright.\n\n" +
		"It cannot search the web. Search engines refuse headless browsers: Google answers with " +
		"an unusual-traffic page and DuckDuckGo with a captcha, measured, with and without a " +
		"realistic browser identity. Ordinary sites are fine. Use it for an address you know.\n\n" +
		"Nobody can see it. There is no window, so it cannot show somebody something, and a page " +
		"that needs a human to sign in or solve a captcha cannot be handed over.\n\n" +
		"There are no screenshots, no network mocking or offline mode, and no tracing or " +
		"video. Playwright's MCP server has tools for those; they are absent here rather than " +
		"present and failing, so do not reach for them.\n\n" +
		"What it is for: reading and using a page at an address you know, including one only this " +
		"computer can reach. A site being built on localhost and an admin page on the office " +
		"network are main cases, and are deliberately not restricted.",
	Edges: []tool.TopicEdge{
		{To: "browser/order", Type: tool.EdgeAlternative, When: "you have an address and want to get on with it"},
	},
}, {
	ID:    "browser/forms",
	Title: "fill_form",
	Body: "Playwright's own description: \"Fill multiple form fields\".\n\n" +
		"One call instead of one per field, which saves a round trip each AND saves a whole " +
		"page outline coming back with every one of them. A form of eight fields is one call " +
		"and one snapshot at the end.\n\n" +
		"fields is a list, and each entry is:\n\n" +
		"    target   the ref from the snapshot\n" +
		"    name     \"Human-readable field name\"\n" +
		"    type     textbox, checkbox, radio, combobox or slider\n" +
		"    value    \"Value to fill in the field. If the field is a checkbox, the value\n" +
		"             should be true or false. If the field is a combobox, the value\n" +
		"             should be the text of the option.\"\n\n" +
		"The type is what decides how it is filled, and the difference matters. A textbox and " +
		"a slider are FILLED, replacing what was there. A checkbox and a radio are SET, which " +
		"is not the same as clicked: clicking one that is already ticked turns it off, so a " +
		"form filled twice would come out wrong, and setting it twice is the same as setting " +
		"it once. A combobox picks the option whose LABEL is the value, which is the text a " +
		"person reads rather than the value in the markup.\n\n" +
		"A field that fails stops the rest, and the answer says how many were done and which " +
		"one failed. Carrying on would leave a form half filled with no way to tell which half.",
	Edges: []tool.TopicEdge{
		{To: "browser/type", Type: tool.EdgeAlternative, When: "there is only one field"},
		{To: "browser/snapshot", Type: tool.EdgePrerequisite, When: "to get a ref for each field"},
	},
}, {
	ID:    "browser/drag",
	Title: "drag and drop",
	Body: "Two different things with similar names.\n\n" +
		"DRAG is \"Perform drag and drop between two elements\", both of them on the page. It " +
		"takes startTarget and endTarget rather than target, and both are resolved before " +
		"anything moves, because a page part way through a drag is not a page you can look " +
		"things up in.\n\n" +
		"DROP is \"Drop files or MIME-typed data onto an element, as if dragged from outside " +
		"the page. At least one of \\\"paths\\\" or \\\"data\\\" must be provided.\" There is no source " +
		"element because the drag began somewhere the browser is not: a folder on the " +
		"person's computer, or nothing at all. It takes target plus paths (absolute paths to " +
		"real files) or data (a map of MIME type to text, such as {\"text/plain\": \"hello\"}).\n\n" +
		"Reach for drop when a page has a drop zone. Reach for file_upload when it has a file " +
		"button: they are different mechanisms and a page usually accepts only the one it was " +
		"built for.",
	Edges: []tool.TopicEdge{
		{To: "browser/files", Type: tool.EdgeAlternative, When: "the page has a file button rather than a drop zone"},
		{To: "browser/actionability", Type: tool.EdgePrerequisite, When: "either end came back refused"},
	},
}, {
	ID:    "browser/files",
	Title: "file_upload",
	Body: "Playwright's own description: \"Upload one or multiple files\".\n\n" +
		"The ORDER is the whole of it, and it is not the obvious one. Click the button that " +
		"asks for a file FIRST. That leaves the page waiting on a file chooser, every other " +
		"action refuses while it is, and file_upload is what answers it. Calling file_upload " +
		"before anything asked for a file is refused, because there is nothing to answer.\n\n" +
		"paths is \"The absolute paths to the files to upload. Can be single file or multiple " +
		"files. If omitted, file chooser is cancelled.\" They are paths on the person's own " +
		"computer, and they are checked before the browser is told, so a path that is wrong " +
		"is said plainly rather than failing inside the page.\n\n" +
		"A field that takes one file refuses several, and says so.",
	Edges: []tool.TopicEdge{
		{To: "browser/blocked", Type: tool.EdgePrerequisite, When: "to understand why everything else refuses first"},
		{To: "browser/drag", Type: tool.EdgeAlternative, When: "the page has a drop zone instead"},
	},
}, {
	ID:    "browser/waiting",
	Title: "wait_for",
	Body: "Playwright's own description: \"Wait for text to appear or disappear or a specified " +
		"time to pass\".\n\n" +
		"Three arguments and at least one is needed: time (\"The time to wait in seconds\"), " +
		"text (\"The text to wait for\"), textGone (\"The text to wait for to disappear\").\n\n" +
		"Prefer text over time. A wait of three seconds is a guess that is too long on a fast " +
		"day and too short on a slow one; a wait for the words that appear when the thing has " +
		"happened is right on both.\n\n" +
		"When both text and textGone are given, GONE is waited for first. That is deliberate " +
		"and it is Playwright's order: on a page that swaps one message for another, waiting " +
		"for the new one first would return the moment it appeared, with the old one still on " +
		"screen.\n\n" +
		"The text is matched the way a person would read it: any part of it, ignoring case, " +
		"with runs of spaces treated as one. Nothing waits longer than 30 seconds, which is " +
		"Playwright's own ceiling; ask for more and you get 30 and are told so.",
	Edges: []tool.TopicEdge{
		{To: "browser/refusals", Type: tool.EdgeConsumer, When: "the wait ran out"},
		{To: "browser/order", Type: tool.EdgeCompanion, When: "every action already waits for its own navigation"},
	},
}, {
	ID:    "browser/window",
	Title: "resize",
	Body: "Playwright's own description: \"Resize the browser window\". width is \"Width of the " +
		"browser window\" and height is \"Height of the browser window\".\n\n" +
		"There is no window, so what changes is the size the page believes it has. That is " +
		"the same thing as far as the page is concerned: every media query, every vh and " +
		"every responsive layout is measured against it, which is what makes this the way to " +
		"see a site at phone width without owning a phone.\n\n" +
		"It is also what changes which elements are in a snapshot, because an element that is " +
		"hidden at one width is not in the outline at all.",
	Edges: []tool.TopicEdge{
		{To: "browser/snapshot", Type: tool.EdgeConsumer, When: "to see what the page looks like at the new size"},
	},
}, {
	ID:    "browser/tabs",
	Title: "tabs",
	Body: "Playwright's own description: \"List, create, close, or select a browser tab.\"\n\n" +
		"Every other action works on the CURRENT tab, so most of the time there is nothing to " +
		"think about: one tab opens by itself and stays current.\n\n" +
		"MOST WORK NEEDS NO TABS AT ALL. Navigating replaces what is in the page you have, so " +
		"walking through twenty pages is one tab and twenty navigations. Open a second tab only " +
		"when you genuinely need two pages at the SAME time, such as comparing them or keeping " +
		"a list open while reading one of its entries. Going back to a page you have already " +
		"seen is a navigate, not a new tab.\n\n" +
		"ONE PAGE IS ONE TAB. Asking for a new tab on an address you already have open gives " +
		"you back the tab that is already open, made current, and says so. Two tabs showing one " +
		"page has no use: the same controls twice over, and two sets of refs for one thing with " +
		"no way to tell which snapshot a ref came from. Navigating onto an address another of " +
		"your tabs is holding closes that one for the same reason.\n\n" +
		"There are ten tabs for everybody, so one left open is one somebody else cannot have. " +
		"The oldest unused tab is closed when the eleventh is needed, and the agent that lost " +
		"it is told to navigate again.\n\n" +
		"operation is one of list, new, close, select. Their description of it is \"Operation " +
		"to perform\". It is Playwright's own \"action\" parameter under another name, because " +
		"this tool's action argument already says which of their tools to run.\n\n" +
		"tab is the index, counted from 0, and their description of it is \"Tab index, used " +
		"for close/select. If omitted for close, current tab is closed.\" It is Playwright's " +
		"own \"index\" parameter under another name, because index here belongs to " +
		"network_request and is counted from 1. Two numbers that count differently could not " +
		"share a name.\n\n" +
		"new takes an optional url and makes the new tab current. Every operation answers " +
		"with the whole list, each with its index, title, address and whether it is the " +
		"current one.\n\n" +
		"Listing works even when a tab is stuck on a dialog, because the titles come from the " +
		"browser rather than from the pages.",
	Edges: []tool.TopicEdge{
		{To: "browser/blocked", Type: tool.EdgeAlternative, When: "one tab is stuck and you want to leave it"},
	},
}, {
	ID:    "browser/blocked",
	Title: "When the page is waiting on something",
	Body: "A page can stop and wait for an answer, and while it does it runs NOTHING. Two " +
		"things do it: a dialog (alert, confirm, prompt, or the one a page shows when you " +
		"try to leave it) and a file chooser.\n\n" +
		"While one is open, every action refuses and says which one to use. That is " +
		"Playwright's rule, and the reason is that a page showing a dialog runs no " +
		"JavaScript at all, so nothing you could ask it would be answered.\n\n" +
		"The action that OPENED the dialog is not a failure. It answers saying what the page " +
		"is now asking, because the click landed, which is exactly why the page is asking. " +
		"Do not try it again.\n\n" +
		"handle_dialog answers a dialog. accept is \"Whether to accept the dialog.\" and " +
		"promptText is \"The text of the prompt in case of a prompt dialog.\" Accepting a " +
		"confirm is pressing OK and refusing it is pressing Cancel.\n\n" +
		"file_upload answers a file chooser.\n\n" +
		"Calling either when the page is NOT waiting is refused too, because there is nothing " +
		"to answer.\n\n" +
		"Two ways out if you are stuck: close ends the page, and works even then, because it " +
		"ends the page rather than asking it anything. tabs works too, for the same reason.",
	Edges: []tool.TopicEdge{
		{To: "browser/files", Type: tool.EdgeConsumer, When: "what is open is a file chooser"},
		{To: "browser/tabs", Type: tool.EdgeAlternative, When: "you would rather leave the page than answer it"},
	},
}, {
	ID:    "browser/debugging",
	Title: "Why a page is not working",
	Body: "This part is about what to reach for rather than about one action.\n\n" +
		"The snapshot says what is on the page. It does not say what went wrong getting it " +
		"there, and that is usually the question: a button that does nothing, a list that " +
		"stays empty, a form that will not save.\n\n" +
		"console_messages is the page's own account of itself, and it carries two things a " +
		"page never prints for itself: an uncaught error, and what the BROWSER thought (a " +
		"blocked request, a refused certificate, a content policy that stopped a script). " +
		"Those are usually the answer.\n\n" +
		"network_requests is what the page asked for and what came back. Start there for " +
		"anything that should have loaded and did not.\n\n" +
		"Read them in that order. An error in the console names the line; a 500 in the " +
		"network list names the endpoint. Between them they cover nearly every case where a " +
		"page looks right and behaves wrong.",
	Edges: []tool.TopicEdge{
		{To: "browser/console", Type: tool.EdgeProducer, When: "to read the console properly"},
		{To: "browser/network", Type: tool.EdgeProducer, When: "to read the requests properly"},
	},
}, {
	ID:    "browser/console",
	Title: "console_messages",
	Body: "Playwright's own description: \"Returns all console messages\".\n\n" +
		"level is \"Level of the console messages to return. Each level includes the messages " +
		"of more severe levels. Defaults to \\\"info\\\".\" The four are error, warning, info, " +
		"debug, most severe first, and asking for one gives you it and everything above it: " +
		"warning gives warnings AND errors, debug gives everything, error gives only errors.\n\n" +
		"all is \"Return all console messages since the beginning of the session, not just " +
		"since the last navigation. Defaults to false.\" So by default you get this page, " +
		"which is nearly always what you want.\n\n" +
		"Each message carries its level, the console method that produced it, its words, and " +
		"where in the source it came from.\n\n" +
		"Two sources are in here and only one of them is the page. The other is the browser " +
		"itself, which is where a blocked request or a refused certificate shows up, and a " +
		"page cannot print those for itself.\n\n" +
		"A page that has been open a long time keeps only its most recent lines. When older " +
		"ones have been dropped the answer says how many, rather than quietly starting part " +
		"way through.",
	Edges: []tool.TopicEdge{
		{To: "browser/network", Type: tool.EdgeCompanion, When: "the console blames something that failed to load"},
		{To: "browser/debugging", Type: tool.EdgeProducer, When: "for which of the two to read first"},
	},
}, {
	ID:    "browser/network",
	Title: "network_requests and network_request",
	Body: "Two actions, and the second needs the first.\n\n" +
		"network_requests is \"Returns a numbered list of network requests since loading the " +
		"page.\" Each line has a number, and that number is what the other action takes.\n\n" +
		"static is \"Whether to include successful static resources like images, fonts, " +
		"scripts, etc. Defaults to false.\" Left alone, you get what the page ASKED FOR " +
		"itself plus anything that failed, which is nearly always the interesting part: a " +
		"page loads a hundred images and none of them is why the button does nothing. The " +
		"answer says how many were hidden.\n\n" +
		"filter is \"Only return requests whose URL matches this regexp (e.g. \\\"/api/.*user\\\").\" " +
		"It is a regular expression, not a substring, and one that does not compile is " +
		"refused with the reason.\n\n" +
		"network_request takes index, \"1-based index of the request, as printed by " +
		"network_requests\", and optionally part, one of request-headers, request-body, " +
		"response-headers, response-body. With no part you get all of it.\n\n" +
		"The numbers are stable: number 7 stays the same request however you filter the list " +
		"afterwards.\n\n" +
		"A response BODY is fetched from the browser at the moment you ask, not kept, so a " +
		"body from much earlier can be gone. When it is, the answer says so rather than " +
		"handing back an empty one, because those are different facts.",
	Edges: []tool.TopicEdge{
		{To: "browser/console", Type: tool.EdgeCompanion, When: "nothing failed to load and the page still misbehaves"},
		{To: "browser/debugging", Type: tool.EdgeProducer, When: "for which of the two to read first"},
	},
}, {
	ID:    "browser/exec_js",
	Title: "exec_js",
	Body: "Runs a JavaScript snippet in the page. Reach for it when:\n\n" +
		"  - The content appears only after JS runs (waits, background fetches).\n" +
		"  - You need to click, scroll, or fill something before reading the page.\n" +
		"  - You need rendered state the markup does not carry: getComputedStyle, " +
		"getBoundingClientRect, which CSS rule actually won.\n" +
		"  - The page's own JS holds the data (window.__NEXT_DATA__, a store).\n" +
		"  - You need to call the site's own endpoints with the full session.\n\n" +
		"THE CONTRACT. The snippet is the body of an async function. Write plain statements " +
		"and return a JSON-serializable value. await works at the top level.\n\n" +
		"    var el = document.querySelector('#total');\n" +
		"    return { text: el ? el.textContent : null, rows: document.querySelectorAll('.row').length };\n\n" +
		"The return value lands in js_result.\n\n" +
		"WAITING FOR CONTENT. Poll on a bounded timer and await it. Nothing may run longer " +
		"than 30 seconds, and a snippet that has already clicked something leaves you unsure " +
		"whether it went through.\n\n" +
		"    var settle = function (test, ms) {\n" +
		"      return new Promise(function (resolve) {\n" +
		"        var waited = 0;\n" +
		"        var step = function () {\n" +
		"          if (test()) { return resolve(true); }\n" +
		"          if (waited >= ms) { return resolve(false); }\n" +
		"          waited += 200;\n" +
		"          setTimeout(step, 200);\n" +
		"        };\n" +
		"        step();\n" +
		"      });\n" +
		"    };\n\n" +
		"    var ready = await settle(function () { return !!document.querySelector('.results'); }, 15000);\n" +
		"    if (!ready) { return { error: 'timeout' }; }\n\n" +
		"Prefer a real readiness marker over a fixed delay: an element existing, or a " +
		"container having dropped its hidden attribute.\n\n" +
		"ERRORS. A thrown exception is captured and returned under js_result.__error. It does " +
		"NOT abort the call, so the page is still there and the snapshot still comes back: a " +
		"script that got half way leaves something worth looking at.\n\n" +
		"PITFALLS.\n" +
		"  - Wrapping the snippet in an IIFE: the value is discarded and js_result is null, " +
		"with no error. The snippet is a function body; return directly.\n" +
		"  - Returning a DOM node, a function, or a circular object: nothing usable comes " +
		"back. Return summaries, not subtrees.\n" +
		"  - A synchronous snippet against async content: it returns before the content " +
		"exists. Wait for a marker.\n" +
		"  - Reading window globals too early: the site's own JS may not have run yet.\n" +
		"  - Unbounded or stacked waits: the script is stopped at 30 seconds.",
	Edges: []tool.TopicEdge{
		{To: "browser/waiting", Type: tool.EdgeAlternative, When: "all you need is to wait for some text"},
		{To: "browser/snapshot", Type: tool.EdgeAlternative, When: "the content is already in the page and you only want to read it"},
		{To: "browser/sessions", Type: tool.EdgeCompanion, When: "the script is signing you in"},
	},
}, {
	ID:    "browser/sessions",
	Title: "Staying signed in",
	Body: "This part is about this build rather than about Playwright.\n\n" +
		"A site you sign into stays signed in for as long as the application is open, because " +
		"the page keeps its cookies. That is free and needs nothing.\n\n" +
		"save_session is for the other case: keeping the sign-in AFTER the application is " +
		"closed. It writes that site's cookies and stored data to this computer, and the next " +
		"time the site is opened they are put back before the page loads, so it knows who you " +
		"are. Call it once, on the site's own page, after signing in successfully.\n\n" +
		"forget_session is signing out: the saved sign-in is deleted. Give it a url, or call " +
		"it on the site's page. The page open right now stays signed in until it is closed or " +
		"navigated away from.\n\n" +
		"YOU NEVER SEE A COOKIE. There is no action that hands one back, and that is " +
		"deliberate: a session cookie is a live credential, and anything returned to you is " +
		"stored and shown in the chat. Say which site to keep; the tool holds the rest.\n\n" +
		"Fifty sites are kept, and saving a fifty-first forgets the one used longest ago.\n\n" +
		"When a saved sign-in is put back, navigate says so. If the page still shows you " +
		"signed out after that, the session has expired at the far end: sign in again and " +
		"save again.",
	Edges: []tool.TopicEdge{
		{To: "browser/exec_js", Type: tool.EdgeCompanion, When: "signing in needs a script rather than a form"},
		{To: "browser/forms", Type: tool.EdgeCompanion, When: "the sign-in is an ordinary form"},
	},
}}
