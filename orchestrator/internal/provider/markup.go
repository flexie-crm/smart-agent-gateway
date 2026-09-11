package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Some vendors (DeepSeek) sometimes "talk" a tool call: instead of returning it
// through the function-calling channel, they emit a block of markup in the
// content stream. Left alone it does two kinds of harm at once: the raw markup
// leaks into the chat as text, and the tool loop never sees the call (so a cap
// that limits, say, background polling can never count it). markupScanner reads
// the content stream, hands back the real prose to emit, and salvages each
// markup block into a proper ToolCall.
//
// The block shape is the invoke/parameter form, delimited by a vendor marker:
//
//	<｜｜DSML｜｜tool_calls>
//	<｜｜DSML｜｜invoke name="http_request">
//	<｜｜DSML｜｜parameter name="method" string="true">GET</｜｜DSML｜｜parameter>
//	</｜｜DSML｜｜invoke>
//	</｜｜DSML｜｜tool_calls>
//
// The marker uses the fullwidth vertical line U+FF5C, not the ASCII pipe.
const markupMarker = "｜｜DSML｜｜"

var (
	markupBlockOpen  = "<" + markupMarker + "tool_calls>"
	markupBlockClose = "</" + markupMarker + "tool_calls>"
	markupInvokeOpen = "<" + markupMarker + "invoke name=\""
	markupInvokeEnd  = "</" + markupMarker + "invoke>"
	markupParamOpen  = "<" + markupMarker + "parameter name=\""
	markupParamClose = "</" + markupMarker + "parameter>"
)

// markupScanner is a streaming state machine: content arrives in fragments, and
// a tool-call block can straddle any number of them. It buffers only what it
// cannot yet safely emit (a tail that might be the start of a marker) so prose
// still streams to the reader with minimal delay.
type markupScanner struct {
	buf     strings.Builder // unprocessed tail carried between pushes
	inBlock bool
	block   strings.Builder // accumulated body of the open block
	seq     int             // synthesises a unique id per salvaged call
}

// push feeds one content delta. It returns prose to emit now and any tool calls
// whose block completed within this delta.
func (s *markupScanner) push(delta string) (emit string, calls []*ToolCall) {
	s.buf.WriteString(delta)
	for {
		cur := s.buf.String()
		if !s.inBlock {
			idx := strings.Index(cur, markupBlockOpen)
			if idx < 0 {
				// No open marker in view. Emit everything except a tail that
				// could be a marker split across the next delta.
				keep := partialSuffixLen(cur, markupBlockOpen)
				emit += cur[:len(cur)-keep]
				s.reset(cur[len(cur)-keep:])
				return emit, calls
			}
			emit += cur[:idx]
			s.inBlock = true
			s.block.Reset()
			s.reset(cur[idx+len(markupBlockOpen):])
			continue
		}
		idx := strings.Index(cur, markupBlockClose)
		if idx < 0 {
			// Block still open. Hold the body, minus a possible partial close.
			keep := partialSuffixLen(cur, markupBlockClose)
			s.block.WriteString(cur[:len(cur)-keep])
			s.reset(cur[len(cur)-keep:])
			return emit, calls
		}
		s.block.WriteString(cur[:idx])
		calls = append(calls, s.parseBlock(s.block.String())...)
		s.inBlock = false
		s.reset(cur[idx+len(markupBlockClose):])
	}
}

// flush is called when the stream ends. A well-formed block always closes, so
// anything left in an open block is a truncated call: its body is returned as
// prose rather than silently dropped, because losing content is worse than an
// ugly tail on the rare cut-off turn.
func (s *markupScanner) flush() string {
	if s.inBlock {
		s.inBlock = false
		return markupBlockOpen + s.block.String() + s.buf.String()
	}
	return s.buf.String()
}

func (s *markupScanner) reset(tail string) {
	s.buf.Reset()
	s.buf.WriteString(tail)
}

// parseBlock turns one tool_calls block body into calls. A block may carry more
// than one invoke.
func (s *markupScanner) parseBlock(body string) []*ToolCall {
	var calls []*ToolCall
	for {
		i := strings.Index(body, markupInvokeOpen)
		if i < 0 {
			return calls
		}
		rest := body[i+len(markupInvokeOpen):]
		q := strings.Index(rest, "\"")
		if q < 0 {
			return calls
		}
		name := rest[:q]
		rest = rest[q+1:]

		var inner string
		if end := strings.Index(rest, markupInvokeEnd); end < 0 {
			inner, body = rest, ""
		} else {
			inner, body = rest[:end], rest[end+len(markupInvokeEnd):]
		}
		args, err := json.Marshal(parseMarkupParams(inner))
		if err != nil {
			args = json.RawMessage("{}")
		}
		s.seq++
		calls = append(calls, &ToolCall{
			ID:   fmt.Sprintf("markup_%d", s.seq),
			Name: name,
			Args: args,
		})
	}
}

// parseMarkupParams reads the parameter tags inside one invoke into an argument
// map. A parameter is a string unless it is tagged string="false", in which
// case its text is a JSON value (number, bool, object, array).
func parseMarkupParams(inner string) map[string]any {
	args := map[string]any{}
	for {
		i := strings.Index(inner, markupParamOpen)
		if i < 0 {
			return args
		}
		rest := inner[i+len(markupParamOpen):]
		q := strings.Index(rest, "\"")
		if q < 0 {
			return args
		}
		name := rest[:q]
		rest = rest[q+1:]

		gt := strings.Index(rest, ">")
		if gt < 0 {
			return args
		}
		attrs := rest[:gt]
		rest = rest[gt+1:]

		var val string
		if end := strings.Index(rest, markupParamClose); end < 0 {
			val, inner = rest, ""
		} else {
			val, inner = rest[:end], rest[end+len(markupParamClose):]
		}
		args[name] = coerceMarkupValue(val, attrs)
	}
}

// coerceMarkupValue keeps a string parameter as text and parses a non-string
// one as JSON, falling back to the raw text if that parse fails.
func coerceMarkupValue(val, attrs string) any {
	if strings.Contains(attrs, `string="true"`) {
		return val
	}
	var v any
	if err := json.Unmarshal([]byte(strings.TrimSpace(val)), &v); err == nil {
		return v
	}
	return val
}

// partialSuffixLen returns the length of the longest suffix of s that is also a
// proper prefix of marker: the bytes to hold back in case the marker is
// completing on the next delta.
func partialSuffixLen(s, marker string) int {
	max := len(marker) - 1
	if max > len(s) {
		max = len(s)
	}
	for k := max; k > 0; k-- {
		if strings.HasPrefix(marker, s[len(s)-k:]) {
			return k
		}
	}
	return 0
}
