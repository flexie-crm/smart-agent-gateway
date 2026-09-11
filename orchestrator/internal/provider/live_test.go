package provider

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"flexie.io/sag/internal/model"
)

// Live tests call the real vendor APIs. They are the only proof that our
// translation matches what the vendors actually do, rather than what we think
// they do, but they cost money, need keys, and can fail for reasons that have
// nothing to do with our code. So they are opt-in:
//
//	SAG_LIVE_ANTHROPIC_KEY=... go test ./internal/provider/ -run Live -v
//
// CI does not set the keys, so it stays hermetic: the fake-server tests in
// openai_test.go and anthropic_test.go are what gate every commit.

func liveKey(t *testing.T, env string) string {
	t.Helper()
	key := os.Getenv(env)
	if key == "" {
		t.Skipf("%s not set; skipping live vendor test", env)
	}
	return key
}

func liveContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

var weatherTool = ToolDef{
	Name:        "get_weather",
	Description: "Get the current weather for a city.",
	InputSchema: json.RawMessage(
		`{"type":"object","properties":{"city":{"type":"string","description":"City name"}},"required":["city"]}`),
}

// liveTurn drives one streamed turn and reports what came back.
func liveTurn(t *testing.T, p Provider, req GenerateRequest) (content, reasoning string, calls []ToolCall, usage Usage) {
	t.Helper()
	events, err := p.Stream(liveContext(t), req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	content, reasoning, calls, usage, errs := collect(t, events)
	if len(errs) > 0 {
		t.Fatalf("vendor returned an error: %v", errs)
	}
	return content, reasoning, calls, usage
}

func TestLiveAnthropicChat(t *testing.T) {
	p := NewAnthropic(liveKey(t, "SAG_LIVE_ANTHROPIC_KEY"), "")

	content, _, _, usage := liveTurn(t, p, GenerateRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 64,
		Messages: []Message{
			{Role: RoleSystem, Content: "Answer with exactly one word."},
			{Role: RoleUser, Content: "What is the capital of France?"},
		},
	})
	if !strings.Contains(strings.ToLower(content), "paris") {
		t.Fatalf("unexpected answer: %q", content)
	}
	if usage.InputTokens == 0 || usage.OutputTokens == 0 {
		t.Fatalf("usage was not reported: %+v", usage)
	}
	t.Logf("anthropic: %q (in=%d out=%d)", content, usage.InputTokens, usage.OutputTokens)
}

func TestLiveAnthropicToolCall(t *testing.T) {
	p := NewAnthropic(liveKey(t, "SAG_LIVE_ANTHROPIC_KEY"), "")

	_, _, calls, _ := liveTurn(t, p, GenerateRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 256,
		Tools:     []ToolDef{weatherTool},
		Messages: []Message{
			{Role: RoleUser, Content: "Use the tool to check the weather in Berlin."},
		},
	})
	if len(calls) != 1 {
		t.Fatalf("expected one tool call, got %+v", calls)
	}
	var args struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal(calls[0].Args, &args); err != nil {
		t.Fatalf("tool arguments are not valid JSON (%q): %v", calls[0].Args, err)
	}
	if !strings.EqualFold(args.City, "Berlin") {
		t.Fatalf("wrong city: %q", args.City)
	}
	t.Logf("anthropic tool: %s(%s)", calls[0].Name, calls[0].Args)
}

// The whole round trip: the model asks for a tool, we answer, and it uses the
// result. This is what the agent loop will do, so it must work on the wire.
func TestLiveAnthropicToolResultRoundTrip(t *testing.T) {
	p := NewAnthropic(liveKey(t, "SAG_LIVE_ANTHROPIC_KEY"), "")

	_, _, calls, _ := liveTurn(t, p, GenerateRequest{
		Model: "claude-sonnet-4-5", MaxTokens: 256, Tools: []ToolDef{weatherTool},
		Messages: []Message{{Role: RoleUser, Content: "Use the tool to check the weather in Berlin."}},
	})
	if len(calls) != 1 {
		t.Fatalf("expected one tool call, got %+v", calls)
	}

	content, _, _, _ := liveTurn(t, p, GenerateRequest{
		Model: "claude-sonnet-4-5", MaxTokens: 256, Tools: []ToolDef{weatherTool},
		Messages: []Message{
			{Role: RoleUser, Content: "Use the tool to check the weather in Berlin."},
			{Role: RoleAssistant, ToolCalls: calls},
			{Role: RoleTool, ToolCallID: calls[0].ID, Content: `{"temp_c":21,"conditions":"sunny"}`},
		},
	})
	if !strings.Contains(content, "21") {
		t.Fatalf("the model did not use the tool result: %q", content)
	}
	t.Logf("anthropic round trip: %q", content)
}

func TestLiveAnthropicReasoning(t *testing.T) {
	p := NewAnthropic(liveKey(t, "SAG_LIVE_ANTHROPIC_KEY"), "")

	content, reasoning, _, _ := liveTurn(t, p, GenerateRequest{
		Model:     "claude-sonnet-4-5",
		Reasoning: true,
		Messages: []Message{
			{Role: RoleUser, Content: "A bat and ball cost 1.10 together. The bat costs 1.00 more than the ball. What does the ball cost?"},
		},
	})
	if reasoning == "" {
		t.Fatal("reasoning was requested but no thinking came back")
	}
	if content == "" {
		t.Fatal("no answer came back alongside the reasoning")
	}
	if strings.Contains(content, reasoning) {
		t.Fatal("reasoning leaked into the answer content")
	}
	t.Logf("anthropic reasoning: %d chars of thinking, answer %q", len(reasoning), truncate(content, 80))
}

func TestLiveOpenAIChatAndTools(t *testing.T) {
	dialect, _ := DialectFor(model.VendorOpenAI)
	p, err := NewOpenAIResponses(dialect, liveKey(t, "SAG_LIVE_OPENAI_KEY"), "", nil)
	if err != nil {
		t.Fatalf("build adapter: %v", err)
	}

	content, _, _, usage := liveTurn(t, p, GenerateRequest{
		Model: "gpt-4o-mini", MaxTokens: 64,
		Messages: []Message{
			{Role: RoleSystem, Content: "Answer with exactly one word."},
			{Role: RoleUser, Content: "What is the capital of France?"},
		},
	})
	if !strings.Contains(strings.ToLower(content), "paris") {
		t.Fatalf("unexpected answer: %q", content)
	}
	if usage.InputTokens == 0 {
		t.Fatalf("usage was not reported: %+v", usage)
	}
	t.Logf("openai: %q (in=%d out=%d)", content, usage.InputTokens, usage.OutputTokens)

	_, _, calls, _ := liveTurn(t, p, GenerateRequest{
		Model: "gpt-4o-mini", MaxTokens: 256, Tools: []ToolDef{weatherTool},
		Messages: []Message{{Role: RoleUser, Content: "Use the tool to check the weather in Berlin."}},
	})
	if len(calls) != 1 {
		t.Fatalf("expected one tool call, got %+v", calls)
	}
	var args struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal(calls[0].Args, &args); err != nil {
		t.Fatalf("tool arguments are not valid JSON (%q): %v", calls[0].Args, err)
	}
	t.Logf("openai tool: %s(%s)", calls[0].Name, calls[0].Args)
}

func TestLiveOpenAIToolResultRoundTrip(t *testing.T) {
	dialect, _ := DialectFor(model.VendorOpenAI)
	p, err := NewOpenAIResponses(dialect, liveKey(t, "SAG_LIVE_OPENAI_KEY"), "", nil)
	if err != nil {
		t.Fatalf("build adapter: %v", err)
	}

	_, _, calls, _ := liveTurn(t, p, GenerateRequest{
		Model: "gpt-4o-mini", MaxTokens: 256, Tools: []ToolDef{weatherTool},
		Messages: []Message{{Role: RoleUser, Content: "Use the tool to check the weather in Berlin."}},
	})
	if len(calls) != 1 {
		t.Fatalf("expected one tool call, got %+v", calls)
	}

	content, _, _, _ := liveTurn(t, p, GenerateRequest{
		Model: "gpt-4o-mini", MaxTokens: 256, Tools: []ToolDef{weatherTool},
		Messages: []Message{
			{Role: RoleUser, Content: "Use the tool to check the weather in Berlin."},
			{Role: RoleAssistant, ToolCalls: calls},
			{Role: RoleTool, ToolCallID: calls[0].ID, Content: `{"temp_c":21,"conditions":"sunny"}`},
		},
	})
	if !strings.Contains(content, "21") {
		t.Fatalf("the model did not use the tool result: %q", content)
	}
	t.Logf("openai round trip: %q", content)
}

// DeepSeek is where the reasoning quirk lives: thinking defaults ON, and the
// thinking comes back in a non-standard field. Both halves are checked here
// against the real API.
func TestLiveDeepSeekReasoningOnAndOff(t *testing.T) {
	dialect, _ := DialectFor(model.VendorDeepSeek)
	p, err := NewOpenAICompatible(dialect, liveKey(t, "SAG_LIVE_DEEPSEEK_KEY"), "", nil)
	if err != nil {
		t.Fatalf("build adapter: %v", err)
	}

	question := Message{Role: RoleUser, Content: "What is 17 times 23? Answer briefly."}

	content, reasoning, _, usage := liveTurn(t, p, GenerateRequest{
		Model: "deepseek-chat", MaxTokens: 512, Reasoning: true,
		Messages: []Message{question},
	})
	if content == "" {
		t.Fatal("no answer came back")
	}
	t.Logf("deepseek reasoning on: %d chars of thinking, answer %q (in=%d out=%d)",
		len(reasoning), truncate(content, 60), usage.InputTokens, usage.OutputTokens)

	// With reasoning off, the vendor must produce no thinking at all. If our
	// disabled toggle were missing, the vendor default (on) would apply and
	// this would come back non-empty.
	content, reasoning, _, _ = liveTurn(t, p, GenerateRequest{
		Model: "deepseek-chat", MaxTokens: 512, Reasoning: false,
		Messages: []Message{question},
	})
	if reasoning != "" {
		t.Fatalf("reasoning was switched off but the vendor still produced thinking: %q", truncate(reasoning, 120))
	}
	if content == "" {
		t.Fatal("no answer came back with reasoning off")
	}
	t.Logf("deepseek reasoning off: no thinking, answer %q", truncate(content, 60))
}

func TestLiveDeepSeekToolCall(t *testing.T) {
	dialect, _ := DialectFor(model.VendorDeepSeek)
	p, err := NewOpenAICompatible(dialect, liveKey(t, "SAG_LIVE_DEEPSEEK_KEY"), "", nil)
	if err != nil {
		t.Fatalf("build adapter: %v", err)
	}

	_, _, calls, _ := liveTurn(t, p, GenerateRequest{
		Model: "deepseek-chat", MaxTokens: 256, Tools: []ToolDef{weatherTool},
		Messages: []Message{{Role: RoleUser, Content: "Use the tool to check the weather in Berlin."}},
	})
	if len(calls) != 1 {
		t.Fatalf("expected one tool call, got %+v", calls)
	}
	var args struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal(calls[0].Args, &args); err != nil {
		t.Fatalf("tool arguments are not valid JSON (%q): %v", calls[0].Args, err)
	}
	t.Logf("deepseek tool: %s(%s)", calls[0].Name, calls[0].Args)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// The combination that forced this adapter to exist: a current reasoning model,
// thinking, WITH tools attached. On the chat wire it is refused outright
// ("Function tools with reasoning_effort are not supported"), which is why no
// agent could use one. Nothing hermetic can prove this: only the vendor can say
// whether it accepts what we send.
//
// The model is read from the environment because naming one here would be the
// per-model knowledge this design refuses, and because the frontier moves.
func TestLiveOpenAIReasoningModelWithTools(t *testing.T) {
	name := os.Getenv("SAG_LIVE_OPENAI_REASONING_MODEL")
	if name == "" {
		t.Skip("SAG_LIVE_OPENAI_REASONING_MODEL not set; skipping the reasoning-plus-tools test")
	}
	dialect, _ := DialectFor(model.VendorOpenAI)
	p, err := NewOpenAIResponses(dialect, liveKey(t, "SAG_LIVE_OPENAI_KEY"), "", nil)
	if err != nil {
		t.Fatalf("build adapter: %v", err)
	}

	content, reasoning, calls, usage := liveTurn(t, p, GenerateRequest{
		Model: name, MaxTokens: 4000, Reasoning: true, Tools: []ToolDef{weatherTool},
		Messages: []Message{{Role: RoleUser, Content: "Use the tool to check the weather in Berlin."}},
	})
	if len(calls) != 1 {
		t.Fatalf("a reasoning model with tools made no call: content=%q reasoning=%d chars",
			content, len(reasoning))
	}
	t.Logf("%s: tool=%s(%s) thinking=%d chars answer=%q (in=%d out=%d)",
		name, calls[0].Name, calls[0].Args, len(reasoning), truncate(content, 60),
		usage.InputTokens, usage.OutputTokens)
}
