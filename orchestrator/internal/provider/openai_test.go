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

	"flexie.io/sag/internal/model"
)

// The adapters are tested against servers that speak the real wire formats.
// Nothing here calls a vendor: the point is to prove we parse what they
// actually send, including the parts each vendor does differently.

type capturedRequest struct {
	path   string
	header http.Header
	query  string
	body   map[string]any
}

// sseServer replies with the given SSE lines and records what was sent to it.
func sseServer(t *testing.T, captured *capturedRequest, chunks ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		body := map[string]any{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("decode request body %q: %v", raw, err)
				return
			}
		}
		*captured = capturedRequest{
			path: r.URL.Path, header: r.Header.Clone(),
			query: r.URL.RawQuery, body: body,
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server cannot flush")
			return
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
			flusher.Flush()
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// collect drains a stream into its events, so assertions read plainly.
func collect(t *testing.T, events <-chan StreamEvent) (content, reasoning string, calls []ToolCall, usage Usage, errs []error) {
	t.Helper()
	for event := range events {
		switch event.Kind {
		case EventContentDelta:
			content += event.ContentDelta
		case EventReasoningDelta:
			reasoning += event.ReasoningDelta
		case EventToolCall:
			calls = append(calls, *event.ToolCall)
		case EventUsage:
			if event.Usage != nil {
				usage = *event.Usage
			}
		case EventError:
			errs = append(errs, event.Err)
		}
	}
	return content, reasoning, calls, usage, errs
}

// mustAdapter builds the CHAT adapter for a vendor, and refuses to build it for
// a vendor that no longer speaks that wire.
//
// The refusal is the point. These tests construct the adapter directly rather
// than through `Gateway.buildProvider`, so nothing here notices when a vendor is
// moved to another wire: every one of them would go on passing while proving
// something about a code path production stopped taking for that vendor. That is
// the pass-and-lie shape this codebase has been bitten by before, and a green
// suite is the worst possible way to find out.
func mustAdapter(t *testing.T, vendorKey, apiKey, baseURL string) *OpenAICompatible {
	t.Helper()
	dialect, ok := DialectFor(vendorKey)
	if !ok {
		t.Fatalf("no dialect for %s", vendorKey)
	}
	if dialect.Wire != WireChatCompletions {
		t.Fatalf("%s does not speak the chat wire any more, so this test proves nothing about it; "+
			"re-point it at a vendor that does, or move it to that wire's suite", vendorKey)
	}
	adapter, err := NewOpenAICompatible(dialect, apiKey, baseURL, nil)
	if err != nil {
		t.Fatalf("build adapter: %v", err)
	}
	return adapter
}

func TestOpenAIStreamParsesContentAndUsage(t *testing.T) {
	var got capturedRequest
	srv := sseServer(t, &got,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":", world"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`,
	)

	adapter := mustAdapter(t, model.VendorMistral, "test-key", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{
		Model:    "gpt-5",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	content, _, calls, usage, errs := collect(t, events)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if content != "Hello, world" {
		t.Fatalf("content not assembled from deltas: %q", content)
	}
	if len(calls) != 0 {
		t.Fatalf("unexpected tool calls: %+v", calls)
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 4 {
		t.Fatalf("usage not captured: %+v", usage)
	}
	// The request must carry the bearer token and ask for usage.
	if auth := got.header.Get("Authorization"); auth != "Bearer test-key" {
		t.Fatalf("wrong auth header: %q", auth)
	}
	if got.body["stream_options"] == nil {
		t.Fatal("stream_options was not requested, so usage would be lost")
	}
}

// Tool arguments arrive as fragments across chunks and must be reassembled
// into one valid JSON document before the tool loop sees them.
func TestOpenAIStreamAssemblesFragmentedToolCall(t *testing.T) {
	var got capturedRequest
	srv := sseServer(t, &got,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Berlin\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	adapter := mustAdapter(t, model.VendorMistral, "k", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{
		Model:    "gpt-5",
		Messages: []Message{{Role: RoleUser, Content: "weather?"}},
		Tools: []ToolDef{{
			Name:        "get_weather",
			Description: "Look up the weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
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
		t.Fatalf("expected exactly one tool call, got %+v", calls)
	}
	call := calls[0]
	if call.ID != "call_1" || call.Name != "get_weather" {
		t.Fatalf("tool identity lost: %+v", call)
	}
	var args struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("tool arguments are not valid JSON (%q): %v", call.Args, err)
	}
	if args.City != "Berlin" {
		t.Fatalf("tool arguments not reassembled: %q", call.Args)
	}
	// The tool definition must have reached the vendor.
	tools, ok := got.body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools not sent: %+v", got.body["tools"])
	}
}

// A vendor that returns no arguments must still yield valid JSON, or the
// tool loop cannot unmarshal the call.
func TestOpenAIStreamToolCallWithoutArguments(t *testing.T) {
	var got capturedRequest
	srv := sseServer(t, &got,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"ping","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	adapter := mustAdapter(t, model.VendorMistral, "k", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{Model: "gpt-5"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_, _, calls, _, _ := collect(t, events)
	if len(calls) != 1 || string(calls[0].Args) != "{}" {
		t.Fatalf("empty arguments must become {}, got %+v", calls)
	}
}

// Parallel tool calls must come back in the order the model produced them:
// their results are returned in the same order.
func TestOpenAIStreamParallelToolCallsKeepOrder(t *testing.T) {
	var got capturedRequest
	srv := sseServer(t, &got,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"b","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	adapter := mustAdapter(t, model.VendorMistral, "k", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{Model: "gpt-5"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_, _, calls, _, _ := collect(t, events)
	if len(calls) != 2 {
		t.Fatalf("expected two calls, got %+v", calls)
	}
	if calls[0].Name != "a" || calls[1].Name != "b" {
		t.Fatalf("tool calls out of order: %s then %s", calls[0].Name, calls[1].Name)
	}
}

// DeepSeek streams thinking in a non-standard reasoning_content field. It
// must be normalized to a reasoning delta, and never mixed into content.
func TestDeepSeekReasoningContentIsNormalized(t *testing.T) {
	var got capturedRequest
	srv := sseServer(t, &got,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"Let me think. "}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"The user wants a greeting."}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Hello!"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)

	adapter := mustAdapter(t, model.VendorDeepSeek, "k", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{
		Model: "deepseek-reasoner", Reasoning: true,
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	content, reasoning, _, _, errs := collect(t, events)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if reasoning != "Let me think. The user wants a greeting." {
		t.Fatalf("reasoning not normalized: %q", reasoning)
	}
	if content != "Hello!" {
		t.Fatalf("reasoning leaked into content: %q", content)
	}
}

// A self-hosted server streams thinking the same way, and the generic entry is
// what every one of them uses: our own machines, vLLM, llama.cpp, LM Studio.
//
// It was unset, so a model that thought had its thinking dropped: `content` is
// null on every thinking chunk and the reasoning field went unread, which on a
// model that thinks by default is most of the answer. "Generic" had been read as
// assume the least, where the honest default is read what arrives.
//
// The request side is untouched: ThinkingToggle is nil for this entry, so
// nothing new is asked of any server, and one that sends no reasoning_content
// yields no reasoning and no event.
func TestASelfHostedServerReasoningIsRead(t *testing.T) {
	var got capturedRequest
	srv := sseServer(t, &got,
		`{"choices":[{"index":0,"delta":{"content":null,"reasoning_content":"The user "}}]}`,
		`{"choices":[{"index":0,"delta":{"content":null,"reasoning_content":"wants a greeting."}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Hello!"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)

	adapter := mustAdapter(t, model.VendorOpenAICompatible, "k", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{
		Model:    "a-local-model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	content, reasoning, _, _, errs := collect(t, events)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if reasoning != "The user wants a greeting." {
		t.Fatalf("a self-hosted model's thinking was dropped: %q", reasoning)
	}
	if content != "Hello!" {
		t.Fatalf("reasoning leaked into content: %q", content)
	}
}

// Z.ai keeps thinking off unless it is switched on with a body field the SDK
// does not model.
func TestZAISendsThinkingParam(t *testing.T) {
	var got capturedRequest
	srv := sseServer(t, &got,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"thinking"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"done"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)

	adapter := mustAdapter(t, model.VendorZAI, "k", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{
		Model: "glm-4.6", Reasoning: true,
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_, reasoning, _, _, errs := collect(t, events)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if reasoning != "thinking" {
		t.Fatalf("reasoning not captured: %q", reasoning)
	}

	thinking, ok := got.body["thinking"].(map[string]any)
	if !ok || thinking["type"] != "enabled" {
		t.Fatalf("thinking was not switched on: %+v", got.body["thinking"])
	}

	// Without reasoning requested the field is still sent, as "disabled":
	// this vendor defaults thinking ON, so silence would mean yes. The
	// explicit off state is covered in reasoning_test.go.
	var plain capturedRequest
	plainSrv := sseServer(t, &plain, `{"choices":[{"index":0,"delta":{"content":"hi"}}]}`)
	adapter = mustAdapter(t, model.VendorZAI, "k", plainSrv.URL)
	events, err = adapter.Stream(context.Background(), GenerateRequest{Model: "glm-4.6"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	collect(t, events)
	thinking, ok = plain.body["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("thinking must be explicitly disabled when it is not requested: %+v", plain.body["thinking"])
	}
}

// Azure authenticates with its own header and pins every call to an
// api-version, which is why it cannot just be a base URL.
func TestAzureUsesItsOwnAuthAndAPIVersion(t *testing.T) {
	var got capturedRequest
	srv := sseServer(t, &got, `{"choices":[{"index":0,"delta":{"content":"hi"}}]}`)

	adapter := mustAdapter(t, model.VendorAzureOpenAI, "azure-secret", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{
		Model: "gpt-5-deployment", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	collect(t, events)

	if key := got.header.Get("Api-Key"); key != "azure-secret" {
		t.Fatalf("azure api-key header missing: %q", key)
	}
	if got.header.Get("Authorization") != "" {
		t.Fatal("azure must not send a bearer token")
	}
	if !strings.Contains(got.query, "api-version=") {
		t.Fatalf("azure api-version missing from %q", got.query)
	}
}

// A local server is reached with nothing but an endpoint: no key required.
func TestLocalServerNeedsOnlyAnEndpoint(t *testing.T) {
	var got capturedRequest
	srv := sseServer(t, &got,
		`{"choices":[{"index":0,"delta":{"content":"local reply"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)

	adapter := mustAdapter(t, model.VendorOpenAICompatible, "", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{
		Model: "qwen3:4b", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	content, _, _, _, errs := collect(t, events)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if content != "local reply" {
		t.Fatalf("content: %q", content)
	}
}

// The output cap is one normalized request field (MaxTokens); the adapter maps
// it to the wire field the VENDOR speaks. OpenAI's own API (o-series included)
// takes max_completion_tokens and rejects max_tokens; the cloned-older-API
// vendors still take max_tokens. This is a dialect fact, never a per-model
// branch, so a new OpenAI model needs no code change.
func TestMaxTokensFieldFollowsDialect(t *testing.T) {
	done := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	cases := []struct {
		vendor         string
		wantCompletion bool
	}{
		{model.VendorAzureOpenAI, true},
		{model.VendorAzureOpenAI, true},
		{model.VendorDeepSeek, false},
		{model.VendorMistral, false},
		{model.VendorOpenAICompatible, false},
	}
	for _, tc := range cases {
		t.Run(tc.vendor, func(t *testing.T) {
			var got capturedRequest
			srv := sseServer(t, &got, done)
			adapter := mustAdapter(t, tc.vendor, "k", srv.URL)
			events, err := adapter.Stream(context.Background(), GenerateRequest{
				Model:     "any-model",
				Messages:  []Message{{Role: RoleUser, Content: "hi"}},
				MaxTokens: 32,
			})
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			collect(t, events)

			capOld, hasOld := got.body["max_tokens"]
			capNew, hasNew := got.body["max_completion_tokens"]
			if tc.wantCompletion {
				if hasOld {
					t.Fatalf("%s must not send max_tokens (o-series rejects it), got %v", tc.vendor, capOld)
				}
				if capNew != float64(32) {
					t.Fatalf("%s must send max_completion_tokens=32, got %v", tc.vendor, capNew)
				}
			} else {
				if hasNew {
					t.Fatalf("%s must not send max_completion_tokens, got %v", tc.vendor, capNew)
				}
				if capOld != float64(32) {
					t.Fatalf("%s must send max_tokens=32, got %v", tc.vendor, capOld)
				}
			}
		})
	}
}

// A transcript with a past tool call must be rebuilt so the vendor sees the
// assistant turn that requested the tool and the result that answered it. A
// broken pairing is rejected by every vendor in this family.
func TestOpenAIRebuildsToolPairing(t *testing.T) {
	var got capturedRequest
	srv := sseServer(t, &got, `{"choices":[{"index":0,"delta":{"content":"ok"}}]}`)

	adapter := mustAdapter(t, model.VendorMistral, "k", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{
		Model: "gpt-5",
		Messages: []Message{
			{Role: RoleSystem, Content: "You are helpful."},
			{Role: RoleUser, Content: "weather?"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{
				ID: "call_1", Name: "get_weather", Args: json.RawMessage(`{"city":"Berlin"}`),
			}}},
			{Role: RoleTool, ToolCallID: "call_1", Content: `{"temp":21}`},
		},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	collect(t, events)

	messages, ok := got.body["messages"].([]any)
	if !ok || len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %+v", got.body["messages"])
	}
	assistant, _ := messages[2].(map[string]any)
	toolCalls, ok := assistant["tool_calls"].([]any)
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("assistant turn lost its tool call: %+v", assistant)
	}
	first, _ := toolCalls[0].(map[string]any)
	if first["id"] != "call_1" {
		t.Fatalf("tool call id lost: %+v", first)
	}
	toolResult, _ := messages[3].(map[string]any)
	if toolResult["role"] != "tool" || toolResult["tool_call_id"] != "call_1" {
		t.Fatalf("tool result not paired with its call: %+v", toolResult)
	}
}

// A vendor error must surface as an error event, not as silence that the
// caller mistakes for an empty answer.
func TestOpenAIStreamReportsVendorError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	}))
	t.Cleanup(srv.Close)

	adapter := mustAdapter(t, model.VendorMistral, "k", srv.URL)
	events, err := adapter.Stream(context.Background(), GenerateRequest{Model: "gpt-5"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_, _, _, _, errs := collect(t, events)
	if len(errs) == 0 {
		t.Fatal("a vendor error must be reported, not swallowed")
	}
}

// A cancelled context must stop the stream without reporting a spurious
// error: the caller hung up, nothing failed.
func TestOpenAIStreamCancellationIsNotAnError(t *testing.T) {
	var got capturedRequest
	srv := sseServer(t, &got,
		`{"choices":[{"index":0,"delta":{"content":"one"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"two"}}]}`,
	)

	adapter := mustAdapter(t, model.VendorMistral, "k", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	events, err := adapter.Stream(ctx, GenerateRequest{Model: "gpt-5"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	cancel()
	for range events { //nolint:revive // draining is the point
	}
}

// An attached file reaches the model, and a message with nothing attached is
// still a plain string.
//
// The plain-string form matters as much as the parts form: several
// OpenAI-compatible servers accept only a string there, and sending everyone a
// one-element array to be uniform would break them for no gain at all.
func TestAFileRidesWithTheWordsOrIsNotThereAtAll(t *testing.T) {
	plain, err := userMessage(Message{Role: RoleUser, Content: "hello"})
	if err != nil {
		t.Fatalf("userMessage: %v", err)
	}
	if plain.OfUser == nil {
		t.Fatal("a user message was not built")
	}
	if plain.OfUser.Content.OfString.Value != "hello" {
		t.Fatalf("a message with no files must stay a plain string, got %+v", plain.OfUser.Content)
	}

	withFile, err := userMessage(Message{
		Role: RoleUser, Content: "what is this?",
		Files: []FilePart{
			{FileName: "shot.png", MediaType: "image/png", Data: []byte{1, 2, 3}},
			{FileName: "deal.pdf", MediaType: "application/pdf", Data: []byte{4, 5}},
		},
	})
	if err != nil {
		t.Fatalf("userMessage: %v", err)
	}
	parts := withFile.OfUser.Content.OfArrayOfContentParts
	if len(parts) != 3 {
		t.Fatalf("expected two files and the words, got %d parts", len(parts))
	}
	if parts[0].OfImageURL == nil {
		t.Error("an image must go as an image part")
	}
	if parts[1].OfFile == nil {
		t.Error("a document must go as a file part")
	}
	// The words come last, after what they are about.
	if parts[2].OfText == nil || parts[2].OfText.Text != "what is this?" {
		t.Errorf("the words did not come last: %+v", parts[2])
	}
}

// A text file is text, and this is the one that was actually broken.
//
// The failure it pins: a person attached coordinator.txt to a Gateway whose
// file rules pointed clear text at a reasoning model, and the vendor refused
// the whole call because the document channel here takes PDF and nothing else
// ("unsupported MIME type 'text/plain'"). The extraction failed, and the person
// was told their file could not be read while their configuration was correct.
func TestATextFileIsSentAsTextAndNotAsBytesToDecode(t *testing.T) {
	msg, err := userMessage(Message{
		Role: RoleUser, Content: "what is this?",
		Files: []FilePart{
			{FileName: "coordinator.txt", MediaType: "text/plain", Data: []byte("hello from the file")},
		},
	})
	if err != nil {
		t.Fatalf("a plain text file was refused: %v", err)
	}
	parts := msg.OfUser.Content.OfArrayOfContentParts
	if len(parts) != 2 {
		t.Fatalf("expected the file and the words, got %d parts", len(parts))
	}
	if parts[0].OfFile != nil {
		t.Fatal("a text file went down the document channel, which takes PDF only")
	}
	if parts[0].OfText == nil {
		t.Fatalf("a text file must arrive as text, got %+v", parts[0])
	}
	// Its contents, verbatim: a text file needs no interpreting, and anything
	// less than all of it is the file arriving damaged.
	if !strings.Contains(parts[0].OfText.Text, "hello from the file") {
		t.Errorf("the file's contents did not reach the model: %q", parts[0].OfText.Text)
	}
	// And named, because the model is shown the file and the question together
	// and has no other way to tell where one ends.
	if !strings.Contains(parts[0].OfText.Text, "coordinator.txt") {
		t.Errorf("the file was not named: %q", parts[0].OfText.Text)
	}
}

func TestEveryShapeOfTextGoesAsText(t *testing.T) {
	// The same set the Anthropic adapter treats as plain text, so a file rule
	// pointed at one vendor and then at another behaves the same way.
	for _, mediaType := range []string{
		"text/plain", "text/markdown", "text/csv", "text/html",
		"application/json", "application/xml", "application/yaml",
		"application/vnd.api+json", "image/svg+xml",
	} {
		if !isText(mediaType) {
			t.Errorf("%s is text and was not treated as text", mediaType)
		}
	}
	for _, mediaType := range []string{
		"application/pdf", "image/png", "audio/mpeg", "application/octet-stream",
	} {
		if isText(mediaType) {
			t.Errorf("%s is not text and was treated as text", mediaType)
		}
	}
}

func TestAFileThisVendorCannotReadIsRefusedWhereItWasConfigured(t *testing.T) {
	// Rather than sent hopefully and rejected by the vendor with a complaint
	// about content[0].file.file_data, which names nothing a person can fix.
	// The Anthropic adapter has always done this; this one sent the bytes.
	_, err := userMessage(Message{
		Role: RoleUser, Content: "read this",
		Files: []FilePart{
			{FileName: "notes.docx", Data: []byte{1, 2, 3},
				MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		},
	})
	if err == nil {
		t.Fatal("a file this vendor cannot read was sent anyway")
	}
	if !errors.Is(err, ErrUnsupportedFile) {
		t.Errorf("a refusal callers can branch on was expected, got %v", err)
	}
	// It has to name the file and say what to do about it.
	if !strings.Contains(err.Error(), "notes.docx") {
		t.Errorf("the refusal does not name the file: %v", err)
	}
	if !strings.Contains(err.Error(), "point that file type at a model that can") {
		t.Errorf("the refusal does not say what to do: %v", err)
	}
}
