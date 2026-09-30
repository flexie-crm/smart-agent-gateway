package apitool

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// HOW A BODY IS SENT, which is the API's requirement and not an assumption.
//
// It used to be one: every body went out JSON-encoded under
// `Content-Type: application/json`, whatever the service asked for. That is
// right for most of the web and wrong for a great deal of it, and when it was
// wrong there was no way to say so: an endpoint wanting a form, or XML, or a
// line of plain text, simply could not be called by this tool. Same shape of
// defect as a required path on an address that has no path.
//
// The choice is per CALL rather than per tool, because it belongs to the
// endpoint: one API commonly takes JSON on its resources and a form on its
// token route. A call that says nothing gets JSON, so nothing that worked
// before changes.
const (
	BodyJSON = "json"
	BodyForm = "form"
	BodyText = "text"
	BodyXML  = "xml"
)

// BodyTypes are the encodings this tool knows how to produce, in the order
// they are offered to a model.
//
// Deliberately not "whatever content type you like": naming a type is not the
// same as producing it, and a tool that accepted `multipart/form-data` here
// would put a header on a JSON body and send something no server can read.
// Multipart is a real gap and is left as one, because it needs bytes to carry
// and this tool takes arguments.
func BodyTypes() []string { return []string{BodyJSON, BodyForm, BodyText, BodyXML} }

// encodeBody turns the body a call asked for into what goes on the wire, and
// the content type that says how to read it.
//
// No body is no body: no bytes, no content type, and not an empty JSON object,
// because a GET with `Content-Type: application/json` and nothing after it is a
// request some servers refuse outright.
func encodeBody(kind string, raw json.RawMessage) (payload []byte, contentType string, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, "", nil
	}
	switch kind = strings.ToLower(strings.TrimSpace(kind)); kind {
	case "", BodyJSON:
		return raw, "application/json", nil

	case BodyForm:
		// An object becomes name=value pairs, with a LIST becoming a repeated
		// field, which is the same rule the query string follows: one place
		// where a list means "more than one of this" rather than two.
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, "", fmt.Errorf("a form body must be an object of names and values, and this is not: %w", err)
		}
		form := url.Values{}
		for name, value := range fields {
			put(form, name, value)
		}
		return []byte(form.Encode()), "application/x-www-form-urlencoded", nil

	case BodyText, BodyXML:
		// A string, sent as its own characters. The JSON quoting is not part of
		// what the service is meant to read, so a body of "<a>1</a>" goes out
		// as eight characters and not as ten with quotes around them.
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, "", fmt.Errorf("a %s body must be a single string holding the %s to send, "+
				"and this is not: %w", kind, kind, err)
		}
		if kind == BodyXML {
			return []byte(text), "application/xml", nil
		}
		return []byte(text), "text/plain; charset=utf-8", nil
	}
	return nil, "", fmt.Errorf("body_type %q is not one this tool can produce: use %s",
		kind, strings.Join(BodyTypes(), ", "))
}
