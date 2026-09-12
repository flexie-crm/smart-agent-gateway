package chat

import "testing"

type captured struct{ frames []Frame }

func (c *captured) Write(f Frame) error { c.frames = append(c.frames, f); return nil }
func (c *captured) texts() []string {
	var out []string
	for _, f := range c.frames {
		if s, ok := f.Message.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// A blank that could only ever draw an empty row never leaves, and a blank that
// is a paragraph break always does.
//
// Both halves matter. Dropping every whitespace delta would run an answer
// together; keeping them all is the empty row this exists to stop.
func TestBlankTextNeverLeavesButParagraphBreaksDo(t *testing.T) {
	sink := &captured{}
	s := NewStream(sink)

	// Whitespace before a word: nothing to draw.
	_ = s.Write(Frame{Type: FrameDelta, Message: "\n\n"})
	if got := len(sink.texts()); got != 0 {
		t.Fatalf("a blank delta reached the client: %q", sink.texts())
	}

	// Words, then a break between them, then more words: all of it must land.
	_ = s.Write(Frame{Type: FrameDelta, Message: "First paragraph."})
	_ = s.Write(Frame{Type: FrameDelta, Message: "\n\n"})
	_ = s.Write(Frame{Type: FrameDelta, Message: "Second paragraph."})
	want := []string{"First paragraph.", "\n\n", "Second paragraph."}
	got := sink.texts()
	if len(got) != len(want) {
		t.Fatalf("the paragraph break was lost: %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("frame %d was %q, wanted %q", i, got[i], want[i])
		}
	}
}

// A tool call ends the text around it, so the newlines a model emits on its way
// into the NEXT one are as empty as the first were. This is the case that was
// actually seen: 49 stored rows of nothing but two newlines, every one written
// by a model about to call a tool.
func TestABlockStartsAgainAfterATool(t *testing.T) {
	sink := &captured{}
	s := NewStream(sink)

	_ = s.Write(Frame{Type: FrameDelta, Message: "Looking that up."})
	_ = s.Write(Frame{Type: FrameToolPreparing, Message: ToolMessage{Name: "query"}})
	_ = s.Write(Frame{Type: FrameTool, Message: ToolMessage{Name: "query", Done: true}})
	_ = s.Write(Frame{Type: FrameDelta, Message: "\n\n"})

	if got := sink.texts(); len(got) != 1 || got[0] != "Looking that up." {
		t.Fatalf("a blank after a tool reached the client: %q", got)
	}
}

// Reasoning is text on the same terms.
func TestReasoningIsHeldToTheSameRule(t *testing.T) {
	sink := &captured{}
	s := NewStream(sink)
	_ = s.Write(Frame{Type: FrameReasoningDelta, Message: "   "})
	if got := len(sink.texts()); got != 0 {
		t.Fatalf("blank reasoning reached the client: %q", sink.texts())
	}
}
