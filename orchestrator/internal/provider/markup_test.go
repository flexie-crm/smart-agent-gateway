package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// feed pushes each delta and returns all prose and all salvaged calls, exactly
// as the stream loop consumes them.
func feed(deltas ...string) (string, []*ToolCall) {
	s := &markupScanner{}
	var prose strings.Builder
	var calls []*ToolCall
	for _, d := range deltas {
		p, c := s.push(d)
		prose.WriteString(p)
		calls = append(calls, c...)
	}
	prose.WriteString(s.flush())
	return prose.String(), calls
}

func TestMarkupPlainProsePassesThrough(t *testing.T) {
	prose, calls := feed("Hello ", "world, no markup here.")
	if prose != "Hello world, no markup here." {
		t.Fatalf("prose = %q", prose)
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %d, want 0", len(calls))
	}
}

func TestMarkupSingleCallNoParams(t *testing.T) {
	block := markupBlockOpen + "\n" + markupInvokeOpen + "background_status\">\n" +
		markupInvokeEnd + "\n" + markupBlockClose
	prose, calls := feed("Let me check. ", block, " done.")
	if strings.Contains(prose, "DSML") {
		t.Fatalf("markup leaked into prose: %q", prose)
	}
	if prose != "Let me check.  done." {
		t.Fatalf("prose = %q", prose)
	}
	if len(calls) != 1 || calls[0].Name != "background_status" {
		t.Fatalf("calls = %+v", calls)
	}
	if string(calls[0].Args) != "{}" {
		t.Fatalf("args = %s, want {}", calls[0].Args)
	}
	if calls[0].ID == "" {
		t.Fatal("salvaged call must carry a non-empty id")
	}
}

func TestMarkupCallWithParams(t *testing.T) {
	block := markupBlockOpen +
		markupInvokeOpen + "http_request\">" +
		markupParamOpen + "method\" string=\"true\">GET" + markupParamClose +
		markupParamOpen + "url\" string=\"true\">https://api.example.com/x?a=1" + markupParamClose +
		markupParamOpen + "timeout\" string=\"false\">30" + markupParamClose +
		markupInvokeEnd + markupBlockClose
	_, calls := feed(block)
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	var got map[string]any
	if err := json.Unmarshal(calls[0].Args, &got); err != nil {
		t.Fatalf("args not json: %v (%s)", err, calls[0].Args)
	}
	if got["method"] != "GET" {
		t.Fatalf("method = %v", got["method"])
	}
	if got["url"] != "https://api.example.com/x?a=1" {
		t.Fatalf("url = %v", got["url"])
	}
	// string="false" is a JSON value, so a number stays a number.
	if got["timeout"] != float64(30) {
		t.Fatalf("timeout = %v (%T), want 30", got["timeout"], got["timeout"])
	}
}

func TestMarkupMultipleInvokesInOneBlock(t *testing.T) {
	block := markupBlockOpen +
		markupInvokeOpen + "a\">" + markupInvokeEnd +
		markupInvokeOpen + "b\">" + markupInvokeEnd +
		markupBlockClose
	_, calls := feed(block)
	if len(calls) != 2 || calls[0].Name != "a" || calls[1].Name != "b" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].ID == calls[1].ID {
		t.Fatalf("ids collide: %s", calls[0].ID)
	}
}

func TestMarkupBlockSplitAcrossManyDeltas(t *testing.T) {
	block := markupBlockOpen + markupInvokeOpen + "http_request\">" +
		markupParamOpen + "url\" string=\"true\">https://x.test" + markupParamClose +
		markupInvokeEnd + markupBlockClose
	full := "before " + block + " after"
	// Split into single runes to prove no boundary assumption survives.
	var deltas []string
	for _, r := range full {
		deltas = append(deltas, string(r))
	}
	prose, calls := feed(deltas...)
	if strings.Contains(prose, "DSML") {
		t.Fatalf("markup leaked: %q", prose)
	}
	if prose != "before  after" {
		t.Fatalf("prose = %q", prose)
	}
	if len(calls) != 1 || calls[0].Name != "http_request" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestMarkupUnterminatedBlockIsSurfacedNotDropped(t *testing.T) {
	// The stream is cut off mid-block: no close marker ever arrives.
	partial := "working " + markupBlockOpen + markupInvokeOpen + "http_request\">"
	prose, calls := feed(partial)
	if len(calls) != 0 {
		t.Fatalf("a truncated block is not a call: %+v", calls)
	}
	if !strings.HasPrefix(prose, "working ") {
		t.Fatalf("prose lost the real prefix: %q", prose)
	}
	if !strings.Contains(prose, "http_request") {
		t.Fatalf("truncated body was dropped: %q", prose)
	}
}

func TestMarkupBareLessThanIsNotHeldForever(t *testing.T) {
	// A lone '<' that never becomes a marker must still be emitted.
	prose, calls := feed("a < b < c")
	if prose != "a < b < c" {
		t.Fatalf("prose = %q", prose)
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestPartialSuffixLen(t *testing.T) {
	m := markupBlockOpen
	if got := partialSuffixLen("hello <", m); got != 1 {
		t.Fatalf("trailing < prefix len = %d, want 1", got)
	}
	if got := partialSuffixLen("hello", m); got != 0 {
		t.Fatalf("no-prefix len = %d, want 0", got)
	}
	if got := partialSuffixLen("x"+m[:5], m); got != 5 {
		t.Fatalf("5-byte prefix len = %d, want 5", got)
	}
}
