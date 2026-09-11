// Package httprequest is the API-request tool: a single HTTP/HTTPS round-trip
// the assistant can make to reach an external service. It is one tool in its
// own package, Schema beside Handler, leaning on internal/tools/toolkit for the
// SSRF guard and result shapes, the same shape every built-in tool follows.
package httprequest

import (
	"encoding/json"
	"time"

	"flexie.io/sag/internal/tool"
)

// Name is the tool's canonical name, exported so the registry and tests refer
// to one constant rather than a scattered string.
const Name = "http_request"

// requestTimeout bounds a single request end to end. It is the tool's async
// deadline: a request that has not answered by then is reported as a timeout
// the model can retry, rather than holding a goroutine open.
const requestTimeout = 60 * time.Second

// maxResponseBytes caps how much of a response body the model is shown. A model
// cannot use a megabyte of HTML, and a huge result read back every turn would
// cost far more than it is worth.
const maxResponseBytes = 30 * 1024

// schema is the tool's short contract: what the model sees every turn. The deep
// contract (precedence, limits, worked examples) lives in the guide, fetched on
// demand, so the short description stays short.
func schema() tool.Schema {
	return tool.Schema{
		Name:         Name,
		FriendlyName: "API request",
		// What a person sees when they open one of these calls: the request as
		// it was made, and the answer as the service formatted it. A body is
		// paired with the content type the service returned, so JSON is read as
		// JSON and XML as XML instead of as one long line either way.
		Shown: tool.Display{
			Sent: []tool.Shown{
				tool.Value("method"),
				tool.Value("url"),
				tool.Value("headers"),
				tool.Body("json", ""),
				tool.Value("form"),
				tool.Body("body", ""),
			},
			Answered: []tool.Shown{
				tool.Value("status"),
				tool.Body("body", "contentType"),
			},
		},
		About: "Lets the agent call an API: a service that answers a request with data, such as a rates feed, a " +
			"partner's system, a public data service, or a webhook that starts something off. It is not a browser " +
			"and it does not search the web, it talks to a named service. It can only reach services that are " +
			"public, never anything on your own network.",
		// What the client shows while it runs, in business terms, never the
		// tool's name.
		FriendlyNarration: "Fetching external data",
		Description: "Fetch from, or send to, any public web API: weather, rates, lookups, public " +
			"datasets, status pages, webhooks. One HTTP(S) request with any method, headers and a " +
			"json, form or raw body. Read its guide before a tricky call.",
		InputSchema:  inputSchema,
		Kind:         tool.KindBuiltin,
		Risk:         tool.RiskExternalCommunication,
		Async:        true,
		AsyncTimeout: requestTimeout + 15*time.Second, // a little past the request's own cap
		Guide:        guide,
		Topics:       topics,
	}
}

var inputSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"url": {
			"type": "string",
			"format": "uri",
			"description": "Fully-qualified http(s) URL. Query-string parameters stay on the URL; do not move them into form or body. Only http and https are allowed."
		},
		"method": {
			"type": "string",
			"enum": ["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"],
			"default": "GET",
			"description": "HTTP method to use. Defaults to GET."
		},
		"headers": {
			"type": "object",
			"additionalProperties": {"type": "string"},
			"description": "Optional request headers, e.g. {\"Authorization\": \"Bearer ...\"}. Do not set Host or Content-Length; they are handled for you."
		},
		"json": {
			"type": "object",
			"description": "Optional object, JSON-encoded and sent with Content-Type application/json unless you set one. Takes precedence over form and body."
		},
		"form": {
			"type": "object",
			"description": "Optional key/value pairs sent as application/x-www-form-urlencoded. Pass raw values; they are encoded for you. Ignored if json is present."
		},
		"body": {
			"type": "string",
			"description": "Optional raw request body for non-JSON, non-form payloads (XML, GraphQL, NDJSON). Set Content-Type yourself in headers. Ignored if json or form is present."
		}
	},
	"required": ["url"]
}`)

// guide is the tool's deep, on-demand documentation, surfaced by the tool_guide
// tool. It is the "how to use this well" material that would bloat the short
// description if sent every turn.
var guide = json.RawMessage(`{
	"summary": "http_request makes a single HTTP/HTTPS round-trip and returns {status, contentType, body}. Compose parameters from this guide; do not invent fields the schema does not list.",
	"parameters": {
		"url": "Full http(s) URL. Query-string params stay on the URL (?a=1&b=2); do NOT move them into form/body. http/https only; file:, data:, ftp: are rejected.",
		"method": "GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS. Defaults to GET.",
		"headers": "Object of header to value. Use for Authorization (Bearer/Basic/API-key), Accept, custom X- headers. Do NOT set Content-Length or Host. A User-Agent is added for you.",
		"json": "Object the tool JSON-encodes and sends with Content-Type application/json. Wins over form and body.",
		"form": "Object of field to value, URL-encoded and sent as application/x-www-form-urlencoded. Values may be strings, arrays, or nested objects. Pass raw values; do NOT pre-encode. Ignored if json is present.",
		"body": "Raw string body for everything else (XML, GraphQL, NDJSON, SOAP). You MUST set Content-Type in headers; none is added on this path. Ignored if json or form is present."
	},
	"body_precedence": [
		"json present -> form and body ignored; Content-Type forced to application/json unless you set one.",
		"form present -> body ignored; Content-Type forced to application/x-www-form-urlencoded unless you set one.",
		"body present -> sent verbatim; Content-Type comes from your headers.",
		"none present -> the request has no body."
	],
	"limits": {
		"timeout": "60 s per request. A long-poll or streaming endpoint will time out; split the work or do not use this tool.",
		"response_body": "Truncated to 30 KB. For more, narrow the query (server-side filter, fewer fields) or paginate.",
		"redirects": "Up to 3 followed; every redirect target is re-checked against the SSRF guard.",
		"ssrf": "Private, loopback, link-local and reserved addresses are blocked, including public hostnames that resolve to them (cloud-metadata included). On a 'blocked: private/reserved range' error do NOT retry; the target is unreachable from here."
	},
	"result_shape": {
		"success": "{ \"status\": 200, \"contentType\": \"application/json\", \"body\": \"<first 30KB>\" }",
		"http_error_codes": "4xx and 5xx are returned as success with the status set; inspect status yourself, they are not tool errors.",
		"transient_failure": "{ \"success\": false, \"error\": \"...\", \"retry\": true } for a timeout, DNS failure, or connection reset. You may retry, ideally with different params.",
		"permanent_failure": "{ \"success\": false, \"error\": \"...\" } for an invalid URL or an SSRF-blocked target. Do NOT retry."
	},
	"examples": [
		{
			"intent": "Authenticated GET with filtering.",
			"call": {"url": "https://api.example.com/v1/orders?status=open&limit=50", "headers": {"Authorization": "Bearer TOKEN"}}
		},
		{
			"intent": "POST a JSON body to a REST API.",
			"call": {"url": "https://api.example.com/v1/widgets", "method": "POST", "headers": {"Authorization": "Bearer TOKEN"}, "json": {"name": "thing", "qty": 3}}
		},
		{
			"intent": "POST form-urlencoded fields to a URL that also carries query-string params.",
			"call": {"url": "https://api.example.com/v1/book?tenant=T1", "method": "POST", "headers": {"Authorization": "Bearer TOKEN"}, "form": {"serviceId": "S1", "startTime": "2026-06-01T14:00:00-04:00"}},
			"note": "Tenant goes on the URL; booking fields go in form. Values are URL-encoded for you."
		}
	],
	"unsupported": [
		"multipart/form-data (file uploads)",
		"streaming responses / Server-Sent Events",
		"a cookie jar across calls (each request is stateless)",
		"WebSockets"
	]
}`)
