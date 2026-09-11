package httprequest

import "flexie.io/sag/internal/tool"

// The http_request drill-down graph. Each topic teaches one concept, usually
// the mistake it prevents, and its edges tell the model which topic to open
// next and when. The model walks it one topic at a time (via tool_guide), so a
// hard call costs only the depth it actually needs. Topic ids are namespaced by
// the tool, so an edge can be followed without also naming the tool.
var topics = []tool.Topic{
	{
		ID:    "http_request/overview",
		Title: "What the ability does, and the shape it returns",
		Body: "One HTTP or HTTPS round-trip. You give a url and optionally a method (GET by default), headers, and a " +
			"body; you get back {status, contentType, body}, with the body capped at 30KB.\n\n" +
			"A 4xx or 5xx is a real answer with its status set, NOT a failure: read the status yourself. A failure comes " +
			"back as {success:false, error, retry?} and means the request never got a clean answer.\n\n" +
			"Reach for a topic below when your call is not a plain GET.",
		Edges: []tool.TopicEdge{
			{To: "http_request/auth", Type: tool.EdgeCompanion, When: "the endpoint needs credentials"},
			{To: "http_request/body-shapes", Type: tool.EdgeCompanion, When: "you are sending a payload (any non-GET with data)"},
			{To: "http_request/errors", Type: tool.EdgeCompanion, When: "a call came back with an error shape"},
		},
	},
	{
		ID:    "http_request/auth",
		Title: "Sending credentials: bearer, basic, and API keys",
		Body: "Credentials go in headers, never in the body.\n\n" +
			"- Bearer token: headers {\"Authorization\": \"Bearer <token>\"}.\n" +
			"- Basic auth: headers {\"Authorization\": \"Basic <base64(user:pass)>\"}; you must base64-encode it yourself.\n" +
			"- API key in a header: headers {\"X-Api-Key\": \"<key>\"} (use the header name the service documents).\n" +
			"- API key in the query string: put it on the url (?api_key=<key>), not in headers or a body.\n\n" +
			"Do not set Host or Content-Length; they are handled for you.",
		Edges: []tool.TopicEdge{
			{To: "http_request/body-shapes", Type: tool.EdgeCompanion, When: "the authenticated request also sends a payload"},
		},
	},
	{
		ID:    "http_request/body-shapes",
		Title: "The three body shapes, and which one wins",
		Body: "There are three ways to send a body, and exactly one is used, in this order of precedence:\n\n" +
			"1. json: an object, sent as application/json. Use it for a REST API that takes JSON.\n" +
			"2. form: an object, sent as application/x-www-form-urlencoded. Use it for classic form posts.\n" +
			"3. body: a raw string for anything else (XML, GraphQL, NDJSON); you must set Content-Type in headers.\n\n" +
			"If you pass more than one, json beats form beats body and the others are ignored. The winning shape sets " +
			"its Content-Type for you unless you set one. Query-string parameters stay on the url; do not move them into " +
			"form or body.",
		Edges: []tool.TopicEdge{
			{To: "http_request/form-encoding", Type: tool.EdgeCompanion, When: "you are sending form fields, especially arrays or nested objects"},
			{To: "http_request/auth", Type: tool.EdgePrerequisite, When: "you have not yet added the credentials the endpoint needs"},
		},
	},
	{
		ID:    "http_request/form-encoding",
		Title: "How form fields are put on the wire",
		Body: "The form object is URL-encoded for you; pass raw values, never pre-encoded ones.\n\n" +
			"- A scalar: {\"q\": \"pizza place\"} becomes q=pizza+place.\n" +
			"- An array: {\"tags\": [\"a\", \"b\"]} becomes tags[0]=a&tags[1]=b (bracketed indices).\n" +
			"- A nested object: {\"filter\": {\"status\": \"open\"}} becomes filter[status]=open.\n\n" +
			"If the service needs the repeated-key style without brackets (tags=a&tags=b) or comma-joined values, form " +
			"cannot produce it: build the string yourself, pass it as body, and set Content-Type: " +
			"application/x-www-form-urlencoded in headers.",
		Edges: []tool.TopicEdge{
			{To: "http_request/body-shapes", Type: tool.EdgePrerequisite, When: "you have not yet seen how form relates to json and body"},
		},
	},
	{
		ID:    "http_request/large-responses",
		Title: "When the answer is bigger than 30KB",
		Body: "The response body is truncated to 30KB. A truncated body is not an error, but it is incomplete, so do not " +
			"parse it as if it were whole.\n\n" +
			"Get less back instead of more: ask the endpoint to filter server-side (a status or date filter, fewer " +
			"fields), request a specific record rather than a list, or page through results (a limit/offset or a cursor " +
			"the API documents). Several small requests beat one that comes back cut off.",
		Edges: []tool.TopicEdge{
			{To: "http_request/errors", Type: tool.EdgeCompanion, When: "you are not sure whether a short body was an error or a truncation"},
		},
	},
	{
		ID:    "http_request/errors",
		Title: "Reading failures, and knowing when to retry",
		Body: "Two things look like failure but are not the same:\n\n" +
			"- An HTTP 4xx/5xx comes back as a normal result with that status. Inspect the status and body; fix your " +
			"request (auth, the endpoint, the payload) rather than retrying blindly.\n" +
			"- A tool failure is {success:false, error, retry?}. If retry is true (a timeout, a reset), you may try " +
			"again, ideally with different parameters. If retry is absent, do not: the address was invalid or the " +
			"target was refused.",
		Edges: []tool.TopicEdge{
			{To: "http_request/blocked", Type: tool.EdgeCompanion, When: "the error said the target was blocked or in a private range"},
			{To: "http_request/large-responses", Type: tool.EdgeCompanion, When: "the body came back cut off"},
		},
	},
	{
		ID:    "http_request/blocked",
		Title: "Blocked targets: what they are and why not to retry",
		Body: "Requests to private, loopback, link-local, or reserved addresses are refused, including public names that " +
			"resolve to them and redirects that turn inward. This is deliberate: those addresses are internal to the " +
			"environment and off-limits.\n\n" +
			"A blocked result is permanent for that target: do not retry it, and do not try to reach an internal service " +
			"another way. Report to the person that the address is not reachable from here, and, if it helps, ask them " +
			"for a public endpoint instead.",
	},
}
