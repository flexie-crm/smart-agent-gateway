package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Anthropic uses named SSE events rather than bare data lines, and streams
// tool arguments as partial JSON. This server speaks that format.
func anthropicServer(t *testing.T, captured *capturedRequest, events ...[2]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		body := map[string]any{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("decode body %q: %v", raw, err)
				return
			}
		}
		*captured = capturedRequest{path: r.URL.Path, header: r.Header.Clone(), body: body}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server cannot flush")
			return
		}
		for _, event := range events {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event[0], event[1])
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAnthropicStreamParsesTextAndUsage(t *testing.T) {
	var got capturedRequest
	srv := anthropicServer(t, &got,
		[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":12,"output_tokens":1}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", world"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)

	adapter := NewAnthropic("test-key", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{
		Model:    "claude-sonnet-5",
		Messages: []Message{{Role: RoleSystem, Content: "Be brief."}, {Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	content, _, calls, usage, errs := collect(t, events)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if content != "Hello, world" {
		t.Fatalf("content not assembled: %q", content)
	}
	if len(calls) != 0 {
		t.Fatalf("unexpected tool calls: %+v", calls)
	}
	if usage.InputTokens != 12 || usage.OutputTokens != 8 {
		t.Fatalf("usage not accumulated across events: %+v", usage)
	}
	if key := got.header.Get("X-Api-Key"); key != "test-key" {
		t.Fatalf("api key header missing: %q", key)
	}
	// The system prompt is a separate field for this vendor, not a message.
	if got.body["system"] == nil {
		t.Fatalf("system prompt was not sent as its own field: %+v", got.body)
	}
	messages, ok := got.body["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("system prompt leaked into messages: %+v", got.body["messages"])
	}
}

// Thinking arrives as its own block type and must never be mixed into the
// answer text.
func TestAnthropicStreamNormalizesThinking(t *testing.T) {
	var got capturedRequest
	srv := anthropicServer(t, &got,
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Considering it."}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Answer."}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)

	adapter := NewAnthropic("k", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{
		Model: "claude-sonnet-5", Reasoning: true,
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	content, reasoning, _, _, errs := collect(t, events)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if reasoning != "Considering it." {
		t.Fatalf("thinking not normalized: %q", reasoning)
	}
	if content != "Answer." {
		t.Fatalf("thinking leaked into content: %q", content)
	}
	if got.body["thinking"] == nil {
		t.Fatalf("thinking was not requested: %+v", got.body)
	}
	// Thinking and temperature cannot be sent together for this vendor.
	if _, present := got.body["temperature"]; present {
		t.Fatalf("temperature must not be sent alongside thinking: %+v", got.body)
	}
}

func TestAnthropicStreamAssemblesToolCall(t *testing.T) {
	var got capturedRequest
	srv := anthropicServer(t, &got,
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":9,"output_tokens":0}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"ci"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"ty\":\"Berlin\"}"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)

	adapter := NewAnthropic("k", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{
		Model:    "claude-sonnet-5",
		Messages: []Message{{Role: RoleUser, Content: "weather?"}},
		Tools: []ToolDef{{
			Name:        "get_weather",
			Description: "Look up the weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	_, _, calls, _, errs := collect(t, events)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(calls) != 1 {
		t.Fatalf("expected one tool call, got %+v", calls)
	}
	if calls[0].ID != "toolu_1" || calls[0].Name != "get_weather" {
		t.Fatalf("tool identity lost: %+v", calls[0])
	}
	var args struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal(calls[0].Args, &args); err != nil {
		t.Fatalf("partial JSON not reassembled (%q): %v", calls[0].Args, err)
	}
	if args.City != "Berlin" {
		t.Fatalf("wrong arguments: %q", calls[0].Args)
	}
	if got.body["tools"] == nil {
		t.Fatalf("tools not sent: %+v", got.body)
	}
}

// A tool result is a user-role block carrying the tool_use id. The pairing is
// what the vendor validates, so it must survive the round trip.
func TestAnthropicRebuildsToolPairing(t *testing.T) {
	var got capturedRequest
	srv := anthropicServer(t, &got,
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)

	adapter := NewAnthropic("k", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{
		Model: "claude-sonnet-5",
		Messages: []Message{
			{Role: RoleUser, Content: "weather?"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{
				ID: "toolu_1", Name: "get_weather", Args: json.RawMessage(`{"city":"Berlin"}`),
			}}},
			{Role: RoleTool, ToolCallID: "toolu_1", Content: `{"temp":21}`},
		},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	collect(t, events)

	messages, ok := got.body["messages"].([]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %+v", got.body["messages"])
	}
	assistant, _ := messages[1].(map[string]any)
	blocks, ok := assistant["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("assistant turn lost its tool use: %+v", assistant)
	}
	toolUse, _ := blocks[0].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "toolu_1" {
		t.Fatalf("tool_use block malformed: %+v", toolUse)
	}
	result, _ := messages[2].(map[string]any)
	resultBlocks, ok := result["content"].([]any)
	if !ok || len(resultBlocks) != 1 {
		t.Fatalf("tool result missing: %+v", result)
	}
	resultBlock, _ := resultBlocks[0].(map[string]any)
	if resultBlock["type"] != "tool_result" || resultBlock["tool_use_id"] != "toolu_1" {
		t.Fatalf("tool result not paired with its call: %+v", resultBlock)
	}
}

func TestAnthropicStreamReportsVendorError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
	}))
	t.Cleanup(srv.Close)

	adapter := NewAnthropic("k", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_, _, _, _, errs := collect(t, events)
	if len(errs) == 0 {
		t.Fatal("a vendor error must be reported, not swallowed")
	}
}

// The vendor has no embeddings endpoint, and must say so rather than
// pretending or routing elsewhere.
func TestAnthropicEmbedIsUnsupported(t *testing.T) {
	adapter := NewAnthropic("k", "http://127.0.0.1:1")
	if _, err := adapter.Embed(context.Background(), "any", []string{"x"}); err == nil {
		t.Fatal("embeddings must be reported as unsupported")
	}
}

// The same for this vendor, which distinguishes an image from a document
// itself: the file's own media type decides which block it becomes.
func TestAFileBecomesTheRightKindOfBlock(t *testing.T) {
	blocks, err := userBlocks(Message{
		Role: RoleUser, Content: "what is this?",
		Files: []FilePart{
			{FileName: "shot.png", MediaType: "image/png", Data: []byte{1, 2, 3}},
			{FileName: "deal.pdf", MediaType: "application/pdf", Data: []byte{4, 5}},
			{FileName: "rows.csv", MediaType: "text/csv", Data: []byte("a,b\n1,2")},
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(blocks) != 4 {
		t.Fatalf("expected three files and the words, got %d blocks", len(blocks))
	}
	if blocks[0].OfImage == nil {
		t.Error("an image must become an image block")
	}
	if blocks[1].OfDocument == nil || blocks[1].OfDocument.Source.OfBase64 == nil {
		t.Error("a PDF must become a document block carrying bytes")
	}
	if blocks[2].OfDocument == nil || blocks[2].OfDocument.Source.OfText == nil {
		t.Error("text must go as text, not as bytes to be decoded")
	}
	if blocks[3].OfText == nil || blocks[3].OfText.Text != "what is this?" {
		t.Errorf("the words did not come last: %+v", blocks[3])
	}

	// A type this vendor cannot read is an ERROR, not a file quietly left out.
	// Which model reads which file is the administrator's decision, and a rule
	// pointing a type at a model that cannot read it is a mistake worth
	// surfacing: dropped silently, the Gateway would answer about a document
	// nobody gave it and nothing would say why.
	_, err = userBlocks(Message{
		Role: RoleUser, Content: "and this?",
		Files: []FilePart{{FileName: "book.xlsx", MediaType: "application/vnd.ms-excel", Data: []byte{1}}},
	})
	if !errors.Is(err, ErrUnsupportedFile) {
		t.Fatalf("an unreadable file was swallowed instead of reported: %v", err)
	}
	if !strings.Contains(err.Error(), "book.xlsx") {
		t.Errorf("the refusal should name the file: %v", err)
	}

	// A message with no words at all still has to be a message.
	empty, err := userBlocks(Message{Role: RoleUser})
	if err != nil || len(empty) != 1 {
		t.Fatalf("an empty user message must still produce a block: %+v %v", empty, err)
	}
}
