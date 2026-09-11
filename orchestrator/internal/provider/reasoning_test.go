package provider

import (
	"context"
	"testing"

	"flexie.io/sag/internal/model"
)

// Reasoning is ONE generic flag. An agent (main or agent) says yes or
// no, and the gateway decides what that means for the model in front of it.
// These tests pin down the two halves of that promise: the flag reaches the
// vendors that can use it, and it is silently dropped for models that cannot.

// Thinking defaults ON at DeepSeek and Z.ai, so "off" has to be said out
// loud. Omitting the field would bill an agent for reasoning it switched off.
func TestThinkingIsExplicitlyDisabledWhenReasoningIsOff(t *testing.T) {
	for _, vendorKey := range []string{model.VendorDeepSeek, model.VendorZAI} {
		t.Run(vendorKey, func(t *testing.T) {
			var got capturedRequest
			srv := sseServer(t, &got, `{"choices":[{"index":0,"delta":{"content":"hi"}}]}`)

			adapter := mustAdapter(t, vendorKey, "k", srv.URL)
			events, err := adapter.Stream(context.Background(), GenerateRequest{
				Model:     "m",
				Reasoning: false,
				Messages:  []Message{{Role: RoleUser, Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			collect(t, events)

			thinking, ok := got.body["thinking"].(map[string]any)
			if !ok {
				t.Fatalf("thinking must be sent even when off, or the vendor default (on) applies: %+v", got.body)
			}
			if thinking["type"] != "disabled" {
				t.Fatalf("reasoning off must send disabled, got %+v", thinking)
			}
		})
	}
}

func TestThinkingIsEnabledWhenReasoningIsOn(t *testing.T) {
	for _, vendorKey := range []string{model.VendorDeepSeek, model.VendorZAI} {
		t.Run(vendorKey, func(t *testing.T) {
			var got capturedRequest
			srv := sseServer(t, &got, `{"choices":[{"index":0,"delta":{"content":"hi"}}]}`)

			adapter := mustAdapter(t, vendorKey, "k", srv.URL)
			events, err := adapter.Stream(context.Background(), GenerateRequest{
				Model: "m", Reasoning: true,
				Messages: []Message{{Role: RoleUser, Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			collect(t, events)

			thinking, ok := got.body["thinking"].(map[string]any)
			if !ok || thinking["type"] != "enabled" {
				t.Fatalf("reasoning on must send enabled, got %+v", got.body["thinking"])
			}
		})
	}
}

// Prepare no longer second-guesses a model's capabilities: whatever the agent
// asked for is what goes on the wire. A model that cannot honour tools or
// reasoning returns a vendor error the loop surfaces, rather than a stored flag
// guessing on the model's behalf.
func TestPreparePassesToolsAndReasoningThroughUnchanged(t *testing.T) {
	resolved := &Resolved{
		Model: &model.AIModel{ModelKey: "gpt-4o", ContextWindow: 100_000},
	}

	on := resolved.Prepare(GenerateRequest{
		Reasoning: true,
		Tools:     []ToolDef{{Name: "search"}},
	})
	if !on.Reasoning {
		t.Fatal("reasoning the agent asked for must be sent, not dropped")
	}
	if len(on.Tools) != 1 {
		t.Fatalf("tools the agent gave must be sent, not dropped: %+v", on.Tools)
	}
	if on.Model != "gpt-4o" {
		t.Fatalf("the model key must come from the registry: %q", on.Model)
	}

	// An agent that switched reasoning off keeps it off.
	if off := resolved.Prepare(GenerateRequest{Reasoning: false}); off.Reasoning {
		t.Fatal("reasoning must stay off when the agent switched it off")
	}
}

// Preparation is also where the transcript is fitted to the model's window,
// so no caller can forget to trim.
func TestPrepareTrimsToTheModelContextWindow(t *testing.T) {
	long := make([]Message, 0, 200)
	long = append(long, Message{Role: RoleSystem, Content: "sys"})
	for i := 0; i < 100; i++ {
		long = append(long, Message{Role: RoleUser, Content: repeat("q", 200)})
		long = append(long, Message{Role: RoleAssistant, Content: repeat("a", 200)})
	}
	long = append(long, Message{Role: RoleUser, Content: "live"})

	resolved := &Resolved{
		Model: &model.AIModel{ModelKey: "m", ContextWindow: 9000},
	}
	got := resolved.Prepare(GenerateRequest{Messages: long})
	if len(got.Messages) >= len(long) {
		t.Fatal("an oversized transcript must be trimmed")
	}
	if got.Messages[len(got.Messages)-1].Content != "live" {
		t.Fatal("the active turn must survive trimming")
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
