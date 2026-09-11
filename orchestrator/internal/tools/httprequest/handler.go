package httprequest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolkit"
)

// New builds the API-request tool. It acts only on the arguments it is handed
// and reaches only the public internet, guarded by the toolkit's SSRF-safe
// client and its pre-flight URL check.
func New() tool.Tool {
	return newTool(deps{client: toolkit.SafeHTTPClient, checkURL: toolkit.CheckURL})
}

// deps are the two pieces the handler talks to the outside world through: the
// HTTP client, and the pre-flight SSRF check. They are injectable so a test can
// point the tool at a local server (which the real guard would rightly block)
// without weakening either guard in production.
type deps struct {
	client   func(timeout time.Duration) *http.Client
	checkURL func(ctx context.Context, raw string) toolkit.SSRFResult
}

func newTool(d deps) tool.Tool {
	return tool.Tool{Schema: schema(), Handle: d.handle}
}

var methods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true,
	"DELETE": true, "HEAD": true, "OPTIONS": true,
}

type request struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	// JSON is captured raw so its mere presence (not just a non-empty value)
	// decides body precedence.
	JSON json.RawMessage `json:"json"`
	Form map[string]any  `json:"form"`
	// Body is a pointer so an empty string is still "present" for precedence.
	Body *string `json:"body"`
}

func (d deps) handle(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args request
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return toolkit.BadArguments("the arguments were not understood")
	}

	// A bad URL or method is the model calling the tool wrong: a bad-arguments
	// failure, which the loop turns into a lesson so it is not repeated.
	u, err := url.Parse(strings.TrimSpace(args.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return toolkit.BadArguments("provide a full http(s) URL, e.g. https://api.example.com/path")
	}
	method := strings.ToUpper(strings.TrimSpace(args.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !methods[method] {
		return toolkit.BadArguments("method must be one of GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS")
	}

	// A pre-flight check for a clean, early refusal of a plainly internal
	// target. The socket-level guard in the client is the real defence against a
	// name that rebinds or a redirect that turns inward.
	if res := d.checkURL(ctx, args.URL); !res.OK {
		return toolkit.Blocked(res.BlockedError())
	}

	req, err := buildRequest(ctx, method, args)
	if err != nil {
		return toolkit.BadArguments(err.Error())
	}

	resp, err := d.client(requestTimeout).Do(req)
	if err != nil {
		return classifyRequestError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return toolkit.Transient("the response could not be read in full")
	}

	// A 4xx/5xx is a real answer, not a tool failure: the model inspects the
	// status itself.
	return toolkit.Success(map[string]any{
		"status":      resp.StatusCode,
		"contentType": resp.Header.Get("Content-Type"),
		"body":        string(body),
	})
}

// buildRequest assembles the outgoing request, applying body precedence
// (json > form > body) and setting a default Content-Type only on the winning
// branch and only when the caller did not set one.
func buildRequest(ctx context.Context, method string, args request) (*http.Request, error) {
	var body io.Reader
	contentType := ""
	switch {
	case len(args.JSON) > 0:
		body = bytes.NewReader(args.JSON)
		contentType = "application/json"
	case args.Form != nil:
		encoded, err := encodeForm(args.Form)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(encoded)
		contentType = "application/x-www-form-urlencoded"
	case args.Body != nil:
		body = strings.NewReader(*args.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, args.URL, body)
	if err != nil {
		return nil, fmt.Errorf("the request could not be built")
	}

	// Caller headers first, so they win; then the default Content-Type only if
	// the winning body branch has one and the caller did not set it.
	for k, v := range args.Headers {
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue // handled by the client
		}
		req.Header.Set(k, v)
	}
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "SAG assistant")
	}
	return req, nil
}

// encodeForm turns the form object into an application/x-www-form-urlencoded
// string. Scalars encode directly; an array encodes with bracketed indices
// (key[0], key[1]); a nested object encodes with bracketed keys (key[inner]).
// Keys are sorted so the output is deterministic.
func encodeForm(form map[string]any) (string, error) {
	values := url.Values{}
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := addFormValue(values, k, form[k]); err != nil {
			return "", err
		}
	}
	return values.Encode(), nil
}

func addFormValue(values url.Values, key string, v any) error {
	switch val := v.(type) {
	case nil:
		values.Add(key, "")
	case string:
		values.Add(key, val)
	case bool:
		values.Add(key, fmt.Sprintf("%t", val))
	case float64:
		values.Add(key, strconv.FormatFloat(val, 'f', -1, 64))
	case []any:
		for i, item := range val {
			if err := addFormValue(values, fmt.Sprintf("%s[%d]", key, i), item); err != nil {
				return err
			}
		}
	case map[string]any:
		for innerKey, item := range val {
			if err := addFormValue(values, fmt.Sprintf("%s[%s]", key, innerKey), item); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("form field %q has a value the tool cannot encode", key)
	}
	return nil
}

// classifyRequestError sorts a client error into what the model should do about
// it: a blocked target it must not retry, or a passing failure it may.
func classifyRequestError(err error) (tool.Result, error) {
	msg := err.Error()
	if strings.Contains(msg, "blocked:") || strings.Contains(msg, "redirect blocked") {
		return toolkit.Blocked("the target is in a private or reserved range and cannot be reached from here")
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "Timeout") || strings.Contains(msg, "timeout") {
		return toolkit.Transient("the request timed out with no response from the other end")
	}
	if strings.Contains(msg, "stopped after 3 redirects") {
		return toolkit.Transient("the request bounced through too many redirects")
	}
	return toolkit.Transient("the request could not reach the target; check the address is reachable")
}
