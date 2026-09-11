// Package chat owns the streaming protocol between the runtime and every
// chat client (web, desktop, mobile).
//
// One definition of the frame vocabulary lives here, and both the server and
// the client are generated from the same understanding of it. The shape is
// inherited from the Flexie chat library so the existing UI plugs in, but it
// is ours to evolve: this runtime can do things the PHP original could not
// (real parallelism, long-lived sessions), and the protocol should grow to
// express them rather than stay pinned to what came before.
package chat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// FrameType enumerates everything that can appear on the wire. Adding a type
// here is a protocol change: the client must learn it too.
type FrameType string

const (
	// FrameDelta is one piece of the answer as it is produced.
	FrameDelta FrameType = "delta"
	// FrameReasoningDelta is one piece of the model's thinking, kept
	// separate from the answer at every vendor.
	FrameReasoningDelta FrameType = "reasoning_delta"

	// FrameToolPreparing announces a tool the model is about to call, so the
	// UI can say what is happening before the work starts.
	FrameToolPreparing FrameType = "tool_preparing"
	// FrameTool reports a finished tool call, with its outcome and how long
	// it took. Two frames per tool is deliberate: one to show intent, one to
	// record the result.
	FrameTool FrameType = "tool"

	// FrameConfirmRequest asks the user to approve an action. The stream
	// then ENDS: approval arrives as a new request, not on a held
	// connection.
	FrameConfirmRequest FrameType = "confirm_request"
	// FrameConfirmResolved reports the decision on a resumed stream.
	FrameConfirmResolved FrameType = "confirm_resolved"

	// FrameSaid reports something the person said WHILE this turn was running,
	// at the point in the transcript where it landed.
	//
	// It exists because the stored conversation and the one on screen would
	// otherwise disagree. The turn closes the answer it had given, writes the
	// person's words, and opens a new answer after them; a client streaming into
	// a single assistant message knows nothing of that, keeps appending, and puts
	// the words at the end. The conversation then rearranges itself on the next
	// reload, which is the transcript telling somebody they had a different
	// conversation from the one they remember.
	FrameSaid FrameType = "said"

	// FrameChatCreated carries the server-assigned chat id for a brand new
	// conversation, so a retry reuses the chat instead of creating a second.
	FrameChatCreated FrameType = "chat_created"

	// FrameHeartbeat keeps a long tool call from looking like a dead stream.
	FrameHeartbeat FrameType = "heartbeat"

	// FrameUsage reports one model call's exact, vendor-reported token spend
	// (and its cost). It carries no answer text: a background agent's chip
	// reads it to show a running total of what the task is consuming, updated
	// once per model call rather than per token.
	FrameUsage FrameType = "usage"

	// FrameAgentStart and FrameAgentEnd bracket an agent the Gateway
	// delegated to. Between them, the frames the agent produces carry
	// Agent = true, so a client shows them as a nested block and the inner
	// result cannot close the outer stream.
	FrameAgentStart FrameType = "agent_start"
	FrameAgentEnd   FrameType = "agent_end"

	// FrameResult ends a turn that produced an answer, and FrameError ends
	// one that failed. Exactly one of them is always the last frame.
	FrameResult FrameType = "result"
	FrameError  FrameType = "error"
)

// Frame is one event on the stream. Message carries the payload, whose shape
// depends on Type: a string for the deltas, a structured object for the rest.
type Frame struct {
	Type FrameType `json:"type"`
	// Final marks the last frame of a turn, so a client knows the stream is
	// over without inspecting the type.
	Final bool `json:"final"`
	// Agent marks a frame produced by a delegated agent rather than the
	// Gateway, so a client can render it apart and never mistake the inner
	// answer for the turn's answer. It rides on the deltas between a
	// agent_start and its agent_end. The wire name matches the chat
	// client's vocabulary (is_agent).
	Agent   bool `json:"is_agent,omitempty"`
	Message any  `json:"message,omitempty"`
	// ChatID is set on FrameChatCreated.
	ChatID string `json:"chat_id,omitempty"`
	// Index is the frame's position in its run, stamped as it is logged. A
	// client that loses the connection says which index it reached, and gets
	// everything after it: the run kept going, and kept the frames.
	Index int `json:"index,omitempty"`
}

// ToolMessage is the payload of FrameToolPreparing and FrameTool.
type ToolMessage struct {
	Name string `json:"tool_name"`
	// FriendlyName is what a person reads ("Look up the weather"), and is
	// the only tool text a UI should show.
	FriendlyName string `json:"friendly_name,omitempty"`
	// Narration is the present-tense label to show while the tool runs
	// ("Fetching external data"), in business language and never the tool's
	// name. Empty falls back to the friendly name.
	Narration string `json:"narration,omitempty"`
	// Done separates the two frames: false while preparing, true once the
	// call has finished.
	Done bool `json:"done"`
	// Status is "completed", "failed", or "rejected" on a done frame.
	Status     string          `json:"status,omitempty"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	// RequestedApproval records that this call went through a confirmation.
	RequestedApproval bool `json:"requested_approval,omitempty"`
	// ID is the stored call, so the row can be opened the moment it finishes
	// rather than only after a reload. Absent on a tool that must not be opened
	// (our own wiring) and on a call with no row yet, which is the same absence
	// the history answer uses: there is nothing to open, rather than a panel
	// that refuses.
	ID string `json:"id,omitempty"`
}

// AgentMessage is the payload of FrameAgentStart: which agent the
// Gateway handed the task to, and in which mode.
type AgentMessage struct {
	Agent string `json:"agent"`
	Name  string `json:"name,omitempty"`
	Mode  string `json:"mode,omitempty"`
}

// UsageMessage is the payload of FrameUsage: the exact token counts of one
// model call, straight from the vendor's own report, and the call's cost in
// dollars derived from the model's pricing. Totals are accumulated by whoever
// reads the stream (a background agent's progress sink), never here.
type UsageMessage struct {
	InputTokens  int64   `json:"input"`
	OutputTokens int64   `json:"output"`
	Cost         float64 `json:"cost,omitempty"`
}

// ConfirmRequest is the payload of FrameConfirmRequest: everything a user
// needs to decide, plus the token that resumes the turn.
type ConfirmRequest struct {
	Token       string `json:"token"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Severity lets a UI weight the warning: read_only through
	// destructive_action.
	Severity  string          `json:"severity"`
	Details   json.RawMessage `json:"details,omitempty"`
	ExpiresAt int64           `json:"expires_at"`
}

// ConfirmResolved is the payload of FrameConfirmResolved.
type ConfirmResolved struct {
	Token  string `json:"token"`
	Status string `json:"status"` // approved | rejected | expired
}

// Sink is where a turn's frames go.
//
// It is the seam that detaches a turn from whoever asked for it. The agent
// writes frames without knowing whether anyone is listening: today the sink is
// a live run, which fans out to however many readers are attached and remembers
// what it sent so a reader who comes back gets it all. An HTTP response is just
// one such reader.
type Sink interface {
	Write(Frame) error
}

// Stream is the frame vocabulary the runtime writes with. It is a thin,
// concurrency-safe wrapper over a Sink.
//
// It is safe for concurrent use because the runtime streams model output while
// heartbeats tick from another goroutine, and two writes interleaved mid-frame
// would corrupt the stream. After a final frame it goes quiet, so a late
// goroutine cannot append to a finished turn.
type Stream struct {
	mu     sync.Mutex
	sink   Sink
	closed bool
	// show is what this person may be shown of how the answer was reached.
	// The zero value shows everything, so anything that builds a stream without
	// an opinion (an agent's inner loop, a test) is unaffected.
	show Show
}

// Show is what a person may see of HOW an answer was reached: the model's
// thinking, and what a tool was sent and answered.
//
// Only the thinking is decided here. A tool's row is part of the conversation
// either way, and what it carried is asked for when somebody opens it, which is
// where that permission is checked.
//
// Withheld here rather than in the client, because a client cannot withhold
// anything: the frames are on the wire and the history endpoint answers the
// same questions. What is not sent is the only thing not seen.
//
// The TRANSCRIPT keeps everything either way. It is the record of what was
// asked, what the assistant did and what it was allowed to do; this decides who
// is shown that record, not whether it is kept.
type Show struct {
	Reasoning bool
	Tools     bool
}

// Everything is what a stream shows when nobody has said otherwise.
func Everything() Show { return Show{Reasoning: true, Tools: true} }

func NewStream(sink Sink) *Stream { return &Stream{sink: sink, show: Everything()} }

// NewStreamShowing builds a stream that carries only what this person may see.
func NewStreamShowing(sink Sink, show Show) *Stream {
	return &Stream{sink: sink, show: show}
}

func (s *Stream) Write(frame Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if s.withheld(frame) {
		// Dropped, not emptied. A row with nothing in it invites "why can it
		// not tell me?", where a conversation that simply reads as an answer
		// and its narration reads as a conversation.
		//
		// A final frame still closes the stream: what is withheld is what a
		// person sees, never the end of a turn.
		if frame.Final {
			s.closed = true
		}
		return nil
	}
	if frame.Final {
		s.closed = true
	}
	return s.sink.Write(frame)
}

// withheld reports whether this frame is one this person may not see.
func (s *Stream) withheld(frame Frame) bool {
	// Only the thinking. A tool's ROW still streams (its name, and how long it
	// took): what a person may not see is what went in and came back, and that
	// is fetched when they ask for it rather than carried on every turn.
	return frame.Type == FrameReasoningDelta && !s.show.Reasoning
}

// Delta is the hot path: one call per token.
func (s *Stream) Delta(text string) error {
	return s.Write(Frame{Type: FrameDelta, Message: text})
}

func (s *Stream) ReasoningDelta(text string) error {
	return s.Write(Frame{Type: FrameReasoningDelta, Message: text})
}

func (s *Stream) Heartbeat() error {
	return s.Write(Frame{Type: FrameHeartbeat})
}

// Result ends the turn with the complete answer. A client that missed deltas
// (a reconnect, a slow render) can rely on this alone.
func (s *Stream) Result(text string) error {
	return s.Write(Frame{Type: FrameResult, Final: true, Message: text})
}

// Error ends the turn with a message meant for a person. Internal detail must
// never reach here: the caller logs the cause and sends something the user can
// act on.
func (s *Stream) Error(text string) error {
	return s.Write(Frame{Type: FrameError, Final: true, Message: text})
}

// SSE writes frames onto an HTTP response as Server-Sent Events. It is one
// reader of a run, not the run itself: when it goes away, the turn carries on.
type SSE struct {
	mu      sync.Mutex
	w       io.Writer
	flusher http.Flusher
}

// NewSSE prepares the response for streaming. It fails when the response
// cannot be flushed, because buffered SSE is not streaming at all: the client
// would receive the whole turn at once, at the end.
func NewSSE(w http.ResponseWriter) (*SSE, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("chat: response writer cannot flush")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Tell any reverse proxy not to buffer: nginx will otherwise hold the
	// whole stream and defeat the point.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	return &SSE{w: w, flusher: flusher}, nil
}

func (s *SSE) Write(frame Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	payload, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("chat: encode frame: %w", err)
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", payload); err != nil {
		return fmt.Errorf("chat: write frame: %w", err)
	}
	s.flusher.Flush()
	return nil
}
