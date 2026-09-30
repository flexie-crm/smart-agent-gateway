package apitool

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// WHERE A CALL GOES, in one rule, because it used to be three.
//
// A base address is a URL with COMPONENTS, and a call is a path and some
// parameters. The address that goes on the wire is:
//
//   - scheme, host and port: the base's, always. A call cannot name a host.
//   - path: the base's own path with the call's path resolved UNDER it, and
//     confined to it. No path means the base's path exactly as configured,
//     trailing slash and all.
//   - query: merged, lowest precedence first: what the CALL asked for, then the
//     ADDRESS's own parameters, then the administrator's EXTRAS, then the
//     CREDENTIAL. A later one overwrites an earlier one and can never be
//     displaced by it.
//   - fragment: never. A fragment is not sent to a server, so one on the base
//     is refused when it is configured.
//
// Nothing here is string concatenation. Every part is set on a parsed URL and
// the result is re-encoded, so a base with a query keeps it when a path is
// added ("https://h/?t=1" + "/v1/x" is "https://h/v1/x?t=1"), and a parameter
// with a space or an ampersand in it is escaped once and correctly.
//
// It returns TWO addresses, and they differ in one way: the one a person and the
// model may see carries every parameter that was sent, with the values that came
// from the CONFIGURATION masked (see masked.go). Name visible, value hidden, so
// an administrator can tell that their own additional parameters were attached
// without a key being readable by anybody.
//
// The shown one used to be built BEFORE the extras were applied, which is why
// they were invisible: not a missing feature, an ordering. url.Redacted() would
// not do this job either, and that is worth saying since it looks like it
// would: it hides userinfo (user:pass@host) and nothing else, so a key placed
// in the query came back in plain sight.
func Target(s Settings, asked map[string]any, given string) (send, show *url.URL, err error) {
	base, err := url.Parse(s.BaseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("this tool's base address is unusable: %w", err)
	}
	resolved, err := Join(s.BaseURL, given)
	if err != nil {
		return nil, nil, err
	}
	send, err = url.Parse(resolved)
	if err != nil {
		return nil, nil, fmt.Errorf("the address could not be built: %w", err)
	}

	// Lowest precedence first.
	q := url.Values{}
	for name, value := range asked {
		put(q, name, value)
	}
	// The address's own parameters. Part of the address, so a call naming the
	// same one does not replace them.
	for name, values := range base.Query() {
		q[name] = values
	}
	for _, extra := range s.QueryPairs {
		q.Set(extra.Name, extra.Value)
	}

	s.Auth.onQuery(q)
	send.RawQuery = q.Encode()

	// What may be seen: the same address, with every value that came from the
	// configuration masked. Built from what was SENT rather than assembled a
	// second time, so the two cannot disagree, and the credential's name is
	// visible while its value is not.
	shown := *send
	shown.RawQuery = maskedQuery(q, s.Auth).Encode()
	show = &shown
	return send, show, nil
}

// put writes one query parameter a call asked for.
//
// A LIST becomes a repeated parameter (?id=1&id=2), which is how an API that
// takes more than one of something expects to receive it. It used to be
// JSON-encoded and sent as the literal text "[1,2]", which no service reads: an
// assistant asking for two of anything got brackets and an error back, and
// there was no other way to say it.
//
// An empty string is a value, because "?q=" is a real request and is not the
// same as not asking. A nil is not, and is dropped.
func put(q url.Values, name string, value any) {
	if list, ok := value.([]any); ok {
		for _, item := range list {
			if item == nil {
				continue
			}
			q.Add(name, scalar(item))
		}
		return
	}
	if value == nil {
		return
	}
	q.Set(name, scalar(value))
}

// Join resolves the path the model asked for UNDER the configured base address,
// and refuses anything that would leave it.
//
// This is the floor the whole tool stands on. The administrator chose one API;
// the model names a path within it. If a path can escape, the tool is not "an
// API this assistant may use", it is "any address on the internet, with your
// credentials attached", which is a different product and not one anybody
// agreed to.
//
// Four routes out, all closed here:
//
//   - An ADDRESS instead of a path ("https://elsewhere/x"). Refused on the
//     scheme, not resolved and hoped about.
//   - A SCHEME-RELATIVE address ("//elsewhere/x"), which url.Parse reads as a
//     host and which does not look like an address to a person reading it.
//   - CLIMBING OUT ("../../other"), refused after resolving rather than by
//     looking for the characters, because "a/../../b" climbs once while
//     containing no leading "..".
//   - CLIMBING OUT ENCODED ("%2e%2e/x"). The path is decoded ONCE before it is
//     resolved, because that is the depth our own client sends at: what we hand
//     to net/http is what it puts on the wire. This is the same trap the
//     download host had (KB/43), where %2e%2e survived routing and was decoded
//     afterwards.
//
// A query belongs in the query parameter, not in the path, and saying so is
// better than quietly escaping a "?" into a literal character the API will not
// recognise.
func Join(base, given string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("this tool's base address is unusable: %w", err)
	}
	given = strings.TrimSpace(given)
	// NO PATH MEANS THE BASE ADDRESS ITSELF, byte for byte.
	//
	// This used to be refused as "path is required", on the assumption that a
	// base is a prefix and a call names something under it. That is true of an
	// API and false of a single endpoint, which is what a webhook listener is:
	//
	//	https://example.com/listener/a1b2c3d4/e5f6a7b8
	//
	// There is nothing to append to that address. Refusing the empty path left
	// a model with one legal alternative, "/", which is a DIFFERENT address:
	// the listener never saw the call, the site answered its own front page,
	// and the tool read as broken. Reported exactly that way.
	//
	// The base is returned unchanged rather than resolved, so a trailing slash
	// the administrator typed survives and one they did not type is not
	// invented. Nothing to check for escape either: there is no input left to
	// escape with.
	if given == "" {
		return b.String(), nil
	}
	if strings.Contains(given, "://") {
		return "", fmt.Errorf("path must be a path under %s, not a whole address", base)
	}
	if strings.HasPrefix(given, "//") {
		return "", fmt.Errorf("path must be a path under %s; a path starting with // names another host", base)
	}
	if i := strings.IndexAny(given, "?#"); i >= 0 {
		return "", fmt.Errorf("path carries a %q: put parameters in `query` instead, so they are encoded properly",
			string(given[i]))
	}
	decoded, err := url.PathUnescape(given)
	if err != nil {
		return "", fmt.Errorf("path is not encoded correctly: %w", err)
	}
	// A backslash is not a separator in a URL, and a path that uses one is
	// either a mistake or an attempt to look like something it is not.
	if strings.Contains(decoded, `\`) {
		return "", fmt.Errorf(`path must not contain a backslash`)
	}
	if !strings.HasPrefix(decoded, "/") {
		decoded = "/" + decoded
	}
	// Join cleans, so ".." is resolved here rather than searched for.
	joined := path.Join(b.Path, decoded)
	if !under(b.Path, joined) {
		return "", fmt.Errorf("path resolves outside %s, which this tool may not reach", base)
	}
	// A trailing slash is meaningful to some APIs, and path.Join eats it.
	if strings.HasSuffix(decoded, "/") && !strings.HasSuffix(joined, "/") {
		joined += "/"
	}
	out := *b
	out.Path = joined
	return out.String(), nil
}

// under says whether a resolved path is still inside the base's own.
//
// The boundary matters: a base of "/v1" must not admit "/v1beta", which shares
// its characters and is a different API.
func under(basePath, resolved string) bool {
	// Without a trailing slash, on both sides. The slash is part of the
	// ADDRESS and is kept in the configuration (see BaseURL), and it has never
	// meant anything to confinement: "/hooks/" and "/hooks" bound the same
	// subtree. Comparing with it left "/hooks/inbound" outside a base of
	// "/hooks/", which refuses every call a tool like that could make.
	basePath = strings.TrimSuffix(basePath, "/")
	if basePath == "" {
		return strings.HasPrefix(resolved, "/")
	}
	return resolved == basePath || strings.HasPrefix(resolved, basePath+"/")
}
