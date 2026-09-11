package provider

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// A keep-alive must not end the stream.
//
// Reproduces the failure exactly: an inference node sending an SSE comment
// between every token killed the answer on the first one, with "unexpected end
// of JSON input". The comment is legal and the SDK ignores it correctly; the
// blank line after it then dispatched an EMPTY event, which was unmarshalled as
// a chunk.

func filtered(t *testing.T, raw string) string {
	t.Helper()
	out, err := io.ReadAll(withoutEmptyEvents(io.NopCloser(strings.NewReader(raw))))
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	return string(out)
}

func TestAKeepAliveDoesNotBecomeAnEmptyEvent(t *testing.T) {
	// Byte for byte what our own node sends: a comment and a blank line before
	// every token, because it thinks for whole seconds between them.
	raw := ":\n\n:\n\ndata: {\"id\":\"4\"}\n\n:\n\ndata: [DONE]\n\n"

	got := filtered(t, raw)
	if strings.Contains(got, ":\n") && !strings.Contains(got, "data:") {
		t.Fatalf("a comment survived: %q", got)
	}
	// Only the two real events remain, each still terminated.
	want := "data: {\"id\":\"4\"}\n\ndata: [DONE]\n\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestARealEventIsPassedThroughUntouched(t *testing.T) {
	// The filter must not edit anything it keeps: a chunk is JSON, and a stream
	// that rewrote it would be a worse bug than the one it fixes.
	raw := "event: message\ndata: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"
	if got := filtered(t, raw); got != raw {
		t.Errorf("got %q, want %q", got, raw)
	}
}

func TestAMultiLineEventSurvives(t *testing.T) {
	// SSE allows a data field to be split across lines, and the decoder joins
	// them. Dropping a blank line in the middle would merge two events into one.
	raw := "data: {\"a\":1,\n" + "data: \"b\":2}\n\n"
	if got := filtered(t, raw); got != raw {
		t.Errorf("got %q, want %q", got, raw)
	}
}

func TestNothingButKeepAlivesProducesNothing(t *testing.T) {
	// A server holding a connection open before it has anything to say. The
	// stream should be quiet, not a run of failures.
	if got := filtered(t, ":\n\n:\n\n: ping\n\n"); got != "" {
		t.Errorf("got %q, want nothing", got)
	}
}

func TestAStreamWithNoKeepAlivesIsUnchanged(t *testing.T) {
	// The overwhelming majority of streams. The filter must be invisible.
	raw := "data: {\"a\":1}\n\ndata: {\"a\":2}\n\ndata: [DONE]\n\n"
	if got := filtered(t, raw); got != raw {
		t.Errorf("got %q, want %q", got, raw)
	}
}

func TestALineLargerThanTheDefaultBufferSurvives(t *testing.T) {
	// A single chunk can carry a long tool call, well past bufio's 64KB
	// default. Dying on a big one would be a stream that fails only on the
	// interesting answers.
	big := "data: {\"x\":\"" + strings.Repeat("y", 300_000) + "\"}\n\n"
	got := filtered(t, big)
	if len(got) != len(big) {
		t.Errorf("a large chunk was truncated: %d bytes of %d", len(got), len(big))
	}
}

func TestTheFilterOnlyTouchesAnEventStream(t *testing.T) {
	// It is installed on every request the SDK makes, so an ordinary JSON body
	// must go past it untouched.
	for _, contentType := range []string{"application/json", "", "text/plain"} {
		res := &http.Response{Header: http.Header{}}
		if contentType != "" {
			res.Header.Set("Content-Type", contentType)
		}
		if isEventStream(res) {
			t.Errorf("%q was treated as a stream", contentType)
		}
	}

	stream := &http.Response{Header: http.Header{}}
	stream.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
	if !isEventStream(stream) {
		t.Error("an event stream was not recognised")
	}
}
