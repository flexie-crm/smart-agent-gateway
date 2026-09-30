package apitool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
	"flexie.io/sag/internal/tools/toolkit"
	"flexie.io/sag/internal/useragent"
)

// mostBody bounds what one response may put in the model's context.
//
// NOT the size feature that was deliberately left out (a per-action ceiling and
// a field selector): pagination is how a caller asks for less, and that belongs
// to the API. This is the floor under it, so a service answering with a
// gigabyte cannot take the gateway down with it. Generous on purpose, and it
// says when it bites rather than handing back a truncated body that parses as
// something smaller.
const mostBody = 5 << 20

// call is what the model sends.
type call struct {
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Query    map[string]any  `json:"query"`
	Body     json.RawMessage `json:"body"`
	BodyType string          `json:"body_type"`
}

// Handler is the live tool for one configured API.
//
// The credentials are here and never leave: they go onto the outgoing request
// and are not echoed into the result. The model supplies a method, a path under
// the configured address, and parameters.
func Handler(s Settings, client *http.Client, cache *tokens, grant template.Grant,
	machines template.Machines) tool.Handler {
	if client == nil {
		client = newClient()
	}
	if cache == nil {
		cache = newTokens(client)
	}
	return func(ctx context.Context, c tool.Call) (tool.Result, error) {
		// Whose network this call is made from. Through the chat application
		// when the tool says so, which needs a client of its own (see
		// reach.go): a pooled connection is keyed by host and port, and two
		// people's computers answer for the same host name.
		use, from := client, ""
		if s.ThroughChat {
			if machines == nil {
				return toolkit.Failed("this tool reaches its API through the chat application, " +
					"and this installation cannot do that")
			}
			if strings.TrimSpace(c.DeviceID) == "" {
				return toolkit.Failed("this tool reaches its API through the chat application, " +
					"so it can only be used from one: this request came from somewhere else")
			}
			if !machines.Online(c.WorkspaceID, c.UserID, c.DeviceID) {
				return toolkit.Transient("no chat application is connected for this person, " +
					"so the API on their network cannot be reached right now")
			}
			use = reachClient(machines, c, s.Timeout)
			// Which computer, so a token obtained over one person's network is
			// never handed to somebody whose own network answers for the same
			// host name.
			from = fmt.Sprintf("%d/%d/%s", c.WorkspaceID, c.UserID, c.DeviceID)
		}
		var in call
		if len(c.Args) > 0 {
			if err := json.Unmarshal(c.Args, &in); err != nil {
				return toolkit.BadArguments("the arguments are not readable: " + err.Error())
			}
		}
		verb := strings.ToUpper(strings.TrimSpace(in.Method))
		if verb == "" {
			return toolkit.BadArguments("method is required: GET, POST, PUT, PATCH, DELETE, HEAD or OPTIONS")
		}
		// The administrator's rule, before anything is built or sent.
		if ok, why := s.Policy.Permits(verb); !ok {
			return toolkit.Blocked(why)
		}
		// Where this call goes, and what may be shown of it. One rule, in
		// path.go, so the handler is about the transport and nothing else.
		u, shown, err := Target(s, in.Query, in.Path)
		if err != nil {
			return toolkit.BadArguments(err.Error())
		}

		payload, contentType, err := encodeBody(in.BodyType, in.Body)
		if err != nil {
			return toolkit.BadArguments(err.Error())
		}
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}

		// Where this call, and anything it is redirected to, may go.
		runCtx, cancel := context.WithTimeout(withinBase(ctx, s.BaseURL), s.Timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(runCtx, verb, u.String(), body)
		if err != nil {
			return toolkit.Failed("the request could not be built: " + err.Error())
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		req.Header.Set("Accept", "application/json")
		// Who we are. Go names itself in this header by default, which reaches
		// somebody else's API log and says which runtime we are written in.
		// Set before the extras, so an administrator who needs a particular
		// one (a vendor that keys a rate limit on it) can say so and win.
		req.Header.Set("User-Agent", useragent.Name)
		s.Auth.onHeader(req.Header)
		// The extras are not the key, so they are sent whatever the
		// authentication is: a version header belongs on a public API too.
		for _, extra := range s.HeaderPairs {
			req.Header.Set(extra.Name, extra.Value)
		}

		// A granted token, if that is how this API is proved to. Fetched here
		// rather than in onHeader because it is a network call, and a failure
		// to get one is about the CREDENTIAL rather than about the request: it
		// is passing, and says so, because an authorization server that is
		// briefly down should not read as a broken tool.
		if s.Auth.Kind == AuthClientCredentials || s.Auth.Kind == AuthJWTBearer {
			token, tokenErr := cache.For(ctx, s.Auth, use, from)
			if tokenErr != nil {
				return toolkit.Transient(scrub(tokenErr.Error(), s.Auth, s.HeaderPairs, s.QueryPairs))
			}
			// A signed token goes where the service wants it: most read an
			// Authorization bearer, and some want a header of their own (one
			// asks for it under "Token", with the token alone as the value).
			carry(req.Header, s.Auth, token)
		}
		// A sign-in a PERSON gave. Not having one is reported as what it is
		// rather than as a broken call: "this tool has not been connected yet"
		// is something an administrator can act on, where the service refusing
		// an absent token would only ever say 401.
		var signedIn string
		if s.Auth.Kind == AuthAuthorizationCode {
			if grant == nil {
				return toolkit.Failed("this tool signs in to a service and has not been connected yet")
			}
			token, tokenErr := grant.Token(ctx)
			if tokenErr != nil {
				return toolkit.Failed(scrub(tokenErr.Error(), s.Auth, s.HeaderPairs, s.QueryPairs))
			}
			signedIn = token
			carry(req.Header, s.Auth, token)
		}

		started := time.Now()
		res, err := use.Do(req)
		// A token that was accepted a minute ago and is refused now has been
		// rotated or revoked, and the held one is worthless. Asking once for a
		// fresh one turns an hour of failure into a retry nobody notices.
		//
		// Safe on any verb, including POST, because a 401 means the request was
		// REFUSED rather than performed: there is nothing to do twice. That is
		// the reason this is bounded to 401 and not extended to 5xx, where the
		// server may well have acted.
		if err == nil && res.StatusCode == http.StatusUnauthorized &&
			(s.Auth.Kind == AuthClientCredentials || s.Auth.Kind == AuthJWTBearer) {
			_ = res.Body.Close()
			cache.forget(s.Auth, from)
			token, tokenErr := cache.For(ctx, s.Auth, use, from)
			if tokenErr != nil {
				return toolkit.Transient(scrub(tokenErr.Error(), s.Auth, s.HeaderPairs, s.QueryPairs))
			}
			// The body needs no rewinding, which is worth writing down because
			// it looks like it would: NewRequestWithContext sets GetBody for a
			// *bytes.Reader, Clone carries it, and the transport uses it. Kept
			// as a comment rather than as a defensive copy because the copy was
			// there first and MEASURED redundant: with it removed the API
			// received {"name":"kept"} on both attempts.
			retry := req.Clone(runCtx)
			// The SAME rule as the first attempt, through the same function.
			// It used to set Authorization here whatever the tool was
			// configured with, so a JWT tool whose service wants its token
			// under a name of its own (Token, X-Api-Token) sent it to the
			// wrong header on the retry and left the stale one in place: the
			// one request that exists to recover from a refusal was the one
			// guaranteed to be refused again.
			carry(retry.Header, s.Auth, token)
			res, err = use.Do(retry)
		}
		// The same recovery for a sign-in a person gave, which a service can
		// refuse before the time it was issued for. Renewed through the app,
		// which renews from the stored refresh token and not at all if another
		// call already has. Retried only with a DIFFERENT token: the same one
		// back means there is nothing better, and the refusal is the answer.
		if err == nil && res.StatusCode == http.StatusUnauthorized &&
			s.Auth.Kind == AuthAuthorizationCode && grant != nil {
			token, tokenErr := grant.Renew(ctx, signedIn)
			if tokenErr != nil {
				_ = res.Body.Close()
				return toolkit.Failed(scrub(tokenErr.Error(), s.Auth, s.HeaderPairs, s.QueryPairs))
			}
			if token != signedIn {
				_ = res.Body.Close()
				retry := req.Clone(runCtx)
				carry(retry.Header, s.Auth, token)
				res, err = use.Do(retry)
			}
		}
		if err != nil {
			// Where the API pointed is not a passing condition: it is its
			// configuration against ours, and it will point there again. Said
			// as what it is, so nothing retries it.
			if errors.Is(err, ErrRedirectRefused) {
				return toolkit.Failed(scrub(err.Error(), s.Auth, s.HeaderPairs, s.QueryPairs))
			}
			// A transport failure is passing, and says so: the model may try
			// again. A refusal by the API itself is NOT this, and comes back as
			// an answer below.
			if errors.Is(err, context.DeadlineExceeded) {
				return toolkit.Transient(fmt.Sprintf("the API did not answer within %s", s.Timeout))
			}
			return toolkit.Transient("the API could not be reached: " + scrub(err.Error(), s.Auth, s.HeaderPairs, s.QueryPairs))
		}
		defer func() { _ = res.Body.Close() }()

		raw, readErr := io.ReadAll(io.LimitReader(res.Body, mostBody+1))
		cut := len(raw) > mostBody
		if cut {
			raw = raw[:mostBody]
		}

		// A 4xx or a 5xx is an ANSWER, not a tool failure. 404 means it is not
		// there, 429 means wait, 422 says which field was wrong, and every one
		// of those is something the model must read and act on. Reporting them
		// as a broken tool would draw a red row over the API working exactly as
		// designed, which is the same mistake the guide tools made.
		out := map[string]any{
			"status":      res.StatusCode,
			"ok":          res.StatusCode >= 200 && res.StatusCode < 300,
			"took_ms":     time.Since(started).Milliseconds(),
			"request_url": shown.Redacted(),
			// Everything this tool attached, with the configured values masked.
			// An administrator who set an account header can see that it went.
			"request_headers": requestHeaders(req.Header, s.Auth),
		}
		if headers := worthShowing(res.Header); len(headers) > 0 {
			out["headers"] = headers
		}
		if readErr != nil {
			out["body_error"] = "the response could not be read in full: " + readErr.Error()
		}
		if cut {
			out["truncated"] = true
			out["truncated_note"] = fmt.Sprintf(
				"the response was longer than %d bytes and was cut; ask for less with the API's own paging parameters", mostBody)
		}
		placeBody(out, res.Header.Get("Content-Type"), raw)
		return toolkit.Success(out)
	}
}

// placeBody puts the response where the model can use it: parsed when it is
// JSON, text when it is text, and described rather than dumped when it is
// neither, because bytes in a context window are noise nobody can read.
//
// ONE field, whichever it turns out to be. A parsed object and a plain string
// both land in "body", because the tool's Display names one field and a second
// name would mean a text response was shown to nobody. content_type is what
// tells them apart, and it is what the chat renders the body by.
// carry puts a fetched token where this tool's service wants it.
//
// One function for every attempt, because there are two: the request and the
// retry after a 401. They disagreed, and the retry was the one that was wrong.
func carry(h http.Header, a Auth, token string) {
	name, value := tokenHeader(a, token)
	h.Set(name, value)
}

func placeBody(out map[string]any, contentType string, raw []byte) {
	kind := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if kind != "" {
		out["content_type"] = kind
	}
	if len(raw) == 0 {
		return
	}
	if kind == "application/json" || strings.HasSuffix(kind, "+json") {
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err == nil {
			out["body"] = parsed
			return
		}
		// Said to be JSON and is not: the text is more use than a parse error,
		// and both are kept because which one matters depends on why.
		out["body"] = string(raw)
		out["body_error"] = "the API said this was JSON and it did not parse"
		return
	}
	if kind == "" || strings.HasPrefix(kind, "text/") ||
		kind == "application/xml" || kind == "application/javascript" {
		out["body"] = string(raw)
		return
	}
	out["body_note"] = fmt.Sprintf("the response is %s, %d bytes, which is not text and is not shown", kind, len(raw))
}

// worthShowing is the response headers a caller can act on, and nothing else.
//
// Named rather than filtered by a denylist: a header this does not know about
// cannot leak through it. Link is here because it is how most APIs page, and
// the rate-limit headers because they are how a model learns to wait.
func worthShowing(h http.Header) map[string]string {
	wanted := []string{
		"Content-Type", "Link", "Location", "Retry-After",
		"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset",
		"RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset",
		"X-Total-Count", "X-Next-Page", "X-Page", "X-Per-Page",
	}
	out := map[string]string{}
	for _, name := range wanted {
		if v := h.Get(name); v != "" {
			out[name] = v
		}
	}
	return out
}

// scalar renders a query value the way a URL wants it.
func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		// Whole numbers without the ".000000", which no API expects.
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// onHeader puts the credential on the request, replacing anything already
// there under the same name.
func (a Auth) onHeader(h http.Header) {
	switch a.Kind {
	case AuthBearer:
		h.Set("Authorization", "Bearer "+a.Token)
	case AuthBasic:
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(a.User+":"+a.Password)))
	case AuthAPIKey:
		if a.Placement != InHeader {
			return
		}
		h.Set(a.Name, a.Key)
	}
}

// onQuery is the other placement, for the APIs that take the key in the URL.
func (a Auth) onQuery(q url.Values) {
	if a.Kind != AuthAPIKey || a.Placement != InQuery {
		return
	}
	q.Set(a.Name, a.Key)
}

// scrub keeps a credential out of an error we are about to hand the model.
//
// A transport error can carry the URL, and a key placed in the QUERY is in the
// URL. The model is never given the credential deliberately, so it must not
// arrive by accident either.
func scrub(text string, a Auth, extra ...[]tool.Pair) string {
	// The private key too: it appears in a signing error, and an error body
	// somebody pastes into a ticket is the last place it should be.
	secrets := []string{a.Key, a.Token, a.Password, a.ClientSecret, a.PrivateKey}
	for _, list := range extra {
		for _, pair := range list {
			secrets = append(secrets, pair.Value)
		}
	}
	for _, secret := range secrets {
		if len(secret) < 4 {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[removed]")
	}
	return text
}
