// Package provider defines the model gateway contract: SAG's own narrow
// vendor abstraction (see KB/05 §model gateway). One adapter per vendor,
// built on the vendor's official Go SDK; the shared orchestrator owns the
// tool loop, retries, trimming, and streaming, adapters own only the wire
// protocol. Local models are served through the OpenAI-compatible adapter.
package provider

import (
	"context"
	"encoding/json"
	"errors"

	"flexie.io/sag/internal/model"
)

// ErrUnsupported is returned by an adapter asked for a capability the vendor
// does not have (embeddings from a chat-only vendor, for example). It is
// reported rather than silently substituted, so a caller cannot end up
// talking to a provider it did not choose.
var ErrUnsupported = errors.New("provider: unsupported capability")

const (
	// streamBuffer keeps a fast vendor from blocking on a slow consumer for
	// every single token.
	streamBuffer = 64

	// defaultMaxTokens applies when a caller does not set a limit. Vendors
	// require one, so there is no "unlimited" to pass through.
	defaultMaxTokens = 4096

	// defaultThinkingBudget is the reasoning allowance when a caller asks
	// for reasoning without naming a budget.
	defaultThinkingBudget = 2048
)

// emit sends an event unless the caller has gone away, so an abandoned
// stream can never wedge an adapter goroutine on a full channel.
func emit(ctx context.Context, out chan<- StreamEvent, event StreamEvent) {
	select {
	case out <- event:
	case <-ctx.Done():
	}
}

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn in the model-visible transcript.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content,omitempty"`
	// ToolCalls is set on assistant messages that requested tools.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID pairs a RoleTool result with its originating call. The
	// assistant(tool_calls) ↔ tool pairing must never be broken by
	// trimming (KB/02 §context trimming).
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Reasoning carries vendor reasoning content when the vendor requires
	// it echoed back on input (e.g. DeepSeek).
	Reasoning string `json:"reasoning,omitempty"`
	// Files are what the person attached, sent alongside the words on a user
	// message. Only a model that takes one is ever handed one: what may be
	// attached at all is decided by the Gateway's file rules, and the model
	// named in the matching rule is the model this is sent to.
	Files []FilePart `json:"files,omitempty"`
}

// ErrTranscriptionUnsupported is a vendor with no way to turn a recording into
// words. It is reported by Media in advance, so nobody is offered a microphone
// that cannot work; this is the answer if one is asked anyway.
var ErrTranscriptionUnsupported = errors.New("provider: this vendor does not transcribe audio")

// ErrUnsupportedFile is a model being handed a file it cannot read. It is an
// error rather than a silent omission because WHICH MODEL READS WHICH FILE IS
// THE ADMINISTRATOR'S DECISION: a rule pointing a file type at a model that
// cannot read it is a configuration mistake, and one that shows itself is one
// somebody can fix.
var ErrUnsupportedFile = errors.New("provider: this model cannot read that kind of file")

// FilePart is one attached file on its way to a model.
//
// The bytes are carried rather than a path or a URL: a vendor is not going to
// reach into our filesystem, and handing one a link would mean publishing the
// file to be read by something outside. MediaType is the IANA type the vendor
// is told, worked out from the file's own type rather than from anything a
// browser claimed.
type FilePart struct {
	FileName  string `json:"file_name"`
	MediaType string `json:"media_type"`
	Data      []byte `json:"-"`
}

// ToolCall is a model-requested tool invocation.
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// ToolDef is the lean per-turn tool schema sent to the model. Deep guidance
// stays out of it (two-tier docs, KB/02 §tool architecture).
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type GenerateRequest struct {
	Model       string
	Messages    []Message
	Tools       []ToolDef
	MaxTokens   int
	Temperature *float64
	// Reasoning asks the vendor for thinking output where supported;
	// adapters translate to the vendor-specific mechanism.
	Reasoning bool
	// Settings are the values chosen for the keys this vendor's dialect
	// declares, already resolved from agent, model and vendor. An adapter reads
	// what it needs and ignores the rest; a key nothing declared never reaches
	// here at all.
	Settings model.Settings
}

type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

type GenerateResponse struct {
	Message Message
	Usage   Usage
}

// StreamEventKind enumerates gateway stream events. They map 1:1 onto the
// client-facing SSE frame vocabulary (KB/02 §SSE protocol); the chat layer
// does the framing, adapters only normalize the vendor stream.
type StreamEventKind string

const (
	EventContentDelta   StreamEventKind = "content_delta"
	EventReasoningDelta StreamEventKind = "reasoning_delta"
	EventToolCall       StreamEventKind = "tool_call"
	EventUsage          StreamEventKind = "usage"
	EventDone           StreamEventKind = "done"
	EventError          StreamEventKind = "error"
)

type StreamEvent struct {
	Kind           StreamEventKind
	ContentDelta   string
	ReasoningDelta string
	ToolCall       *ToolCall
	Usage          *Usage
	Err            error
}

type Capabilities struct {
	SupportsTools     bool
	SupportsStreaming bool
	SupportsReasoning bool
	ContextWindow     int
}

// What a vendor can be asked to do with something that is not words.
//
// These are properties of the ADAPTER, not of a model: whether this code knows
// how to hand a vendor a file at all, and whether it knows how to ask one for a
// transcription. Which model can actually make sense of a spreadsheet is a
// different question, and not ours: an administrator picks the model, and
// picking one that cannot read what they pointed at it is their mistake to make
// and to see.
//
// What this is for is telling the truth earlier. A vendor this cannot send a
// file to at all will never work however good the model is, and saying so on
// the screen where somebody chose it beats a failure on the first upload.
type Media struct {
	// ReadsFiles is whether a file can be sent to this vendor with a message.
	ReadsFiles bool
	// Transcribes is whether this vendor can turn a recording into words.
	Transcribes bool
}

// ModelInfo is one model a vendor says it offers. Name is what a person should
// read; it is empty for the vendors that publish only an id.
type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// ErrListingUnsupported means the vendor has no endpoint that says what it
// offers, so nothing can be concluded from asking.
//
// It must never be confused with an empty list. An empty list is a vendor that
// offers nothing; this is a vendor that will not say. Azure is the case: what
// you name there is a deployment you created, not a model the vendor publishes,
// so no catalog can confirm or deny it.
var ErrListingUnsupported = errors.New("provider: this vendor does not list its models")

// Provider is implemented once per vendor adapter.
type Provider interface {
	// Name is the registry vendor key, e.g. "anthropic", "openai",
	// "openai-compatible".
	Name() string
	Capabilities(ctx context.Context, model string) (Capabilities, error)
	// ListModels asks the vendor what it offers, so that a model id can be
	// checked against the only authority on it. ErrListingUnsupported when the
	// vendor cannot be asked.
	ListModels(ctx context.Context) ([]ModelInfo, error)
	// Media is what this adapter can do beyond words: send a file, transcribe a
	// recording. It describes the adapter rather than a model, and is fixed, so
	// it takes no context and cannot fail.
	Media() Media
	Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)
	// Transcribe turns a recording into words. ErrTranscriptionUnsupported when
	// the vendor has no such endpoint, which Media reports in advance.
	Transcribe(ctx context.Context, model, fileName string, audio []byte) (string, error)
	// Stream sends events until Done or Error; the channel is closed by
	// the adapter. Cancellation via ctx.
	Stream(ctx context.Context, req GenerateRequest) (<-chan StreamEvent, error)
	Embed(ctx context.Context, model string, inputs []string) ([][]float32, error)
}
