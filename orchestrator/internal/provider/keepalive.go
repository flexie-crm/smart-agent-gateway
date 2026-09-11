package provider

import (
	"bufio"
	"bytes"
	"io"
	"net/http"

	"github.com/openai/openai-go/v3/option"
)

// A keep-alive must not end the stream.
//
// A server streaming a slow answer sends SSE COMMENT lines to hold the
// connection open: a line beginning with ":", then a blank line. That is legal
// and ordinary, and a locally hosted model does it constantly because it thinks
// for whole seconds between tokens.
//
// The SDK's decoder ignores the comment correctly, and then dispatches an event
// anyway when it reaches the blank line, carrying an EMPTY data buffer. The
// stream then unmarshals that empty string as a chunk and fails with
// "unexpected end of JSON input", killing a perfectly healthy answer on its
// first keep-alive.
//
// The specification is explicit that this should not happen: on dispatch, "if
// the data buffer is an empty string, set the data buffer and the event type
// buffer to the empty string and return" (WHATWG HTML, server-sent events),
// which is to say do not dispatch at all. We cannot change the decoder, so the
// bytes reaching it are filtered instead: comments are dropped, and a blank line
// that would dispatch nothing is dropped with them. Every real event is passed
// through untouched, byte for byte.
//
// It was found against our own inference node, whose engine sends a keep-alive
// between every token. It is NOT specific to that: any server may do this, and
// an answer that dies the moment a model pauses to think is the kind of failure
// that reads as "the vendor is flaky".

// withoutEmptyEvents filters an SSE body so no empty event is ever dispatched.
func withoutEmptyEvents(body io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()

	go func() {
		defer func() { _ = body.Close() }()

		scanner := bufio.NewScanner(body)
		// A single SSE line can carry a whole chunk of JSON, which for a long
		// tool call is well past the default 64KB. The stream must not die on a
		// large one.
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

		// Whether anything has been written since the last blank line. A blank
		// line only means "dispatch" if there is something to dispatch.
		pending := false

		for scanner.Scan() {
			line := scanner.Bytes()

			switch {
			case len(line) == 0:
				// The end of an event. Passed on only when the event has a body,
				// so a lone keep-alive never becomes an empty chunk.
				if !pending {
					continue
				}
				pending = false
			case line[0] == ':':
				// A comment. Dropped entirely, so the blank line after it has
				// nothing to terminate.
				continue
			default:
				pending = true
			}

			if _, err := writer.Write(append(bytes.Clone(line), '\n')); err != nil {
				return
			}
		}
		_ = writer.CloseWithError(scanner.Err())
	}()

	return reader
}

// keepAliveTolerance is the request option that installs the filter.
//
// Applied to every OpenAI-compatible client, hosted or local: a keep-alive is
// not a property of who is answering, and a vendor that starts sending them
// should not become a vendor that starts failing.
var keepAliveTolerance = option.WithMiddleware(
	func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		res, err := next(req)
		if err != nil || res == nil || res.Body == nil {
			return res, err
		}
		// Only a stream can carry one. Leaving an ordinary JSON body alone keeps
		// this out of the path of every other call the SDK makes.
		if !isEventStream(res) {
			return res, nil
		}
		res.Body = withoutEmptyEvents(res.Body)
		return res, nil
	},
)

func isEventStream(res *http.Response) bool {
	return bytes.HasPrefix([]byte(res.Header.Get("Content-Type")), []byte("text/event-stream"))
}
