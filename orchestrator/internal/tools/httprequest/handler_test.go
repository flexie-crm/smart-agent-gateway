package httprequest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolkit"
)

// capture records what the test server received, so a test can assert the tool
// built the outgoing request correctly.
type capture struct {
	method      string
	contentType string
	body        string
	userAgent   string
}

// testTool wires the tool to a local server, with the SSRF pre-flight relaxed
// (the real guard rightly blocks loopback, which is where httptest binds). The
// dialer guard is bypassed too by handing it the server's own client. Neither
// guard is weakened in production: New() still uses the real ones.
func testTool(t *testing.T, srv *httptest.Server) tool.Handler {
	t.Helper()
	d := deps{
		client:   func(time.Duration) *http.Client { return srv.Client() },
		checkURL: func(context.Context, string) toolkit.SSRFResult { return toolkit.SSRFResult{OK: true} },
	}
	return newTool(d).Handle
}

func invoke(t *testing.T, handle tool.Handler, args map[string]any) tool.Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := handle(context.Background(), tool.Call{Args: raw})
	if err != nil {
		t.Fatalf("handle returned a system error: %v", err)
	}
	return res
}

func decode(t *testing.T, res tool.Result) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(res.Content, &out); err != nil {
		t.Fatalf("decode result: %v (%s)", err, res.Content)
	}
	return out
}

// A plain GET returns the far end's status, content type, and body, unchanged.
func TestGetReturnsResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	res := invoke(t, testTool(t, srv), map[string]any{"url": srv.URL})
	if res.Failed() {
		t.Fatalf("a good GET failed: %s", res.Content)
	}
	out := decode(t, res)
	if out["status"].(float64) != 200 {
		t.Fatalf("status not returned: %v", out["status"])
	}
	if !strings.Contains(out["contentType"].(string), "application/json") {
		t.Fatalf("content type not returned: %v", out["contentType"])
	}
	if out["body"].(string) != `{"ok":true}` {
		t.Fatalf("body not returned: %v", out["body"])
	}
}

// A response larger than the cap is truncated, not returned whole.
func TestResponseBodyIsTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", maxResponseBytes*2))
	}))
	defer srv.Close()

	out := decode(t, invoke(t, testTool(t, srv), map[string]any{"url": srv.URL}))
	if got := len(out["body"].(string)); got != maxResponseBytes {
		t.Fatalf("body was not truncated to the cap: %d", got)
	}
}

// A JSON body is sent with the right content type, and the raw JSON on the wire.
func TestPostJSON(t *testing.T) {
	var got capture
	srv := captureServer(&got)
	defer srv.Close()

	res := invoke(t, testTool(t, srv), map[string]any{
		"url": srv.URL, "method": "POST",
		"json": map[string]any{"name": "thing", "qty": 3},
	})
	if res.Failed() {
		t.Fatalf("post json failed: %s", res.Content)
	}
	if got.method != "POST" {
		t.Fatalf("method: %s", got.method)
	}
	if !strings.Contains(got.contentType, "application/json") {
		t.Fatalf("content type: %s", got.contentType)
	}
	if !strings.Contains(got.body, `"name":"thing"`) || !strings.Contains(got.body, `"qty":3`) {
		t.Fatalf("json body on the wire: %s", got.body)
	}
	if got.userAgent == "" {
		t.Fatal("no User-Agent was sent")
	}
}

// A form body is URL-encoded with the right content type.
func TestPostForm(t *testing.T) {
	var got capture
	srv := captureServer(&got)
	defer srv.Close()

	invoke(t, testTool(t, srv), map[string]any{
		"url": srv.URL, "method": "POST",
		"form": map[string]any{"q": "pizza", "city": "NY"},
	})
	if !strings.Contains(got.contentType, "application/x-www-form-urlencoded") {
		t.Fatalf("content type: %s", got.contentType)
	}
	if !strings.Contains(got.body, "q=pizza") || !strings.Contains(got.body, "city=NY") {
		t.Fatalf("form body on the wire: %s", got.body)
	}
}

// json beats form beats body: the winning branch is the only one sent.
func TestBodyPrecedence(t *testing.T) {
	var got capture
	srv := captureServer(&got)
	defer srv.Close()

	body := "raw"
	invoke(t, testTool(t, srv), map[string]any{
		"url": srv.URL, "method": "POST",
		"json": map[string]any{"a": 1},
		"form": map[string]any{"b": "2"},
		"body": body,
	})
	if !strings.Contains(got.contentType, "application/json") {
		t.Fatalf("json did not win precedence: %s", got.contentType)
	}
	if strings.Contains(got.body, "b=2") || got.body == body {
		t.Fatalf("a losing body branch was sent: %s", got.body)
	}
}

// A 4xx is a real answer with a status, not a tool failure.
func TestHTTPErrorIsNotAToolFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, "nope")
	}))
	defer srv.Close()

	res := invoke(t, testTool(t, srv), map[string]any{"url": srv.URL})
	if res.Failed() {
		t.Fatalf("a 404 was treated as a tool failure: %+v", res)
	}
	if decode(t, res)["status"].(float64) != 404 {
		t.Fatal("the status was not passed through")
	}
}

// Calling the tool wrong is a bad-arguments failure, the one the loop learns
// from. A missing url, a bad scheme, and a bad method all qualify.
func TestBadArgumentsAreClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	handle := testTool(t, srv)

	cases := []map[string]any{
		{},                                     // no url
		{"url": "not a url"},                   // unparseable / no scheme
		{"url": "ftp://example.com"},           // wrong scheme
		{"url": srv.URL, "method": "TELEPORT"}, // bad method
	}
	for _, args := range cases {
		res := invoke(t, handle, args)
		if res.Err != tool.ErrorBadArguments {
			t.Fatalf("args %v: expected bad_arguments, got %q (%s)", args, res.Err, res.Content)
		}
	}
}

// A target the pre-flight refuses is Blocked, not retryable, and never dialled.
func TestBlockedTargetIsRefused(t *testing.T) {
	d := deps{
		client:   func(time.Duration) *http.Client { t.Fatal("a blocked target must not be dialled"); return nil },
		checkURL: func(context.Context, string) toolkit.SSRFResult { return toolkit.SSRFResult{Reason: "private range"} },
	}
	handle := newTool(d).Handle
	raw, _ := json.Marshal(map[string]any{"url": "http://example.com"})
	res, err := handle(context.Background(), tool.Call{Args: raw})
	if err != nil {
		t.Fatalf("system error: %v", err)
	}
	if res.Err != tool.ErrorBlocked {
		t.Fatalf("a blocked target was not classified blocked: %q", res.Err)
	}
}

// captureServer records the request it receives and answers 200.
func captureServer(into *capture) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		into.method = r.Method
		into.contentType = r.Header.Get("Content-Type")
		into.userAgent = r.Header.Get("User-Agent")
		into.body = string(body)
		w.WriteHeader(200)
	}))
}
