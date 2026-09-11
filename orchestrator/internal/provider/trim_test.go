package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// Trimming has one rule that matters above all others: never hand a vendor a
// message list it will reject. A tool result with no assistant turn
// requesting it is exactly that, and it is what the naive "drop the oldest"
// approach produces mid tool loop.

func msg(role Role, content string) Message {
	return Message{Role: role, Content: content}
}

func assistantCall(id, name string) Message {
	return Message{
		Role:      RoleAssistant,
		ToolCalls: []ToolCall{{ID: id, Name: name, Args: json.RawMessage(`{}`)}},
	}
}

func toolResult(id, content string) Message {
	return Message{Role: RoleTool, ToolCallID: id, Content: content}
}

// validPairing reports whether every tool result is preceded by the assistant
// turn that asked for it. This is the invariant a vendor enforces.
func validPairing(messages []Message) bool {
	requested := map[string]bool{}
	for _, m := range messages {
		switch m.Role {
		case RoleAssistant:
			for _, call := range m.ToolCalls {
				requested[call.ID] = true
			}
		case RoleTool:
			if !requested[m.ToolCallID] {
				return false
			}
		}
	}
	return true
}

func TestTrimKeepsEverythingUnderBudget(t *testing.T) {
	messages := []Message{
		msg(RoleSystem, "sys"),
		msg(RoleUser, "hello"),
		msg(RoleAssistant, "hi"),
	}
	got := TrimToBudget(messages, 10_000)
	if len(got) != len(messages) {
		t.Fatalf("nothing should be trimmed under budget: %d", len(got))
	}
}

func TestTrimNeverDropsSystemPrompt(t *testing.T) {
	messages := []Message{msg(RoleSystem, "important system rules")}
	for i := 0; i < 40; i++ {
		messages = append(messages, msg(RoleUser, strings.Repeat("q", 200)))
		messages = append(messages, msg(RoleAssistant, strings.Repeat("a", 200)))
	}
	messages = append(messages, msg(RoleUser, "the live question"))

	got := TrimToBudget(messages, 1500)
	if len(got) == 0 || got[0].Role != RoleSystem {
		t.Fatalf("system prompt was dropped: %+v", got)
	}
	if got[len(got)-1].Content != "the live question" {
		t.Fatalf("the active turn was dropped: %+v", got[len(got)-1])
	}
	if size(got) > 1500 {
		t.Fatalf("still over budget: %d", size(got))
	}
}

// The bug this whole design exists to prevent: mid tool loop the newest
// message is a tool result, and dropping the assistant turn that requested it
// leaves an orphan the vendor rejects.
func TestTrimNeverOrphansAToolResult(t *testing.T) {
	messages := []Message{msg(RoleSystem, "sys")}
	// Plenty of old history, each turn including a tool round trip.
	for i := 0; i < 12; i++ {
		messages = append(messages,
			msg(RoleUser, strings.Repeat("old question ", 20)),
			assistantCall("call_old", "search"),
			toolResult("call_old", strings.Repeat("old result ", 30)),
			msg(RoleAssistant, strings.Repeat("old answer ", 20)),
		)
	}
	// The active turn: a user message, a tool call, and its result. This is
	// what the model is waiting on.
	messages = append(messages,
		msg(RoleUser, "the live question"),
		assistantCall("call_live", "search"),
		toolResult("call_live", "the live result"),
	)

	for _, budget := range []int{300, 800, 1500, 3000, 6000} {
		got := TrimToBudget(messages, budget)
		if !validPairing(got) {
			t.Fatalf("budget %d produced an orphaned tool result: %+v", budget, roles(got))
		}
		// The active turn survives whatever the budget.
		if !containsContent(got, "the live question") {
			t.Fatalf("budget %d dropped the active user message", budget)
		}
		if !containsToolResult(got, "call_live") {
			t.Fatalf("budget %d dropped the live tool result", budget)
		}
	}
}

// When the active turn alone exceeds the budget, the pairing still cannot be
// broken: the only safe move is to shorten tool results in place.
func TestTrimTruncatesToolResultsRatherThanBreakPairing(t *testing.T) {
	huge := strings.Repeat("x", 20_000)
	messages := []Message{
		msg(RoleSystem, "sys"),
		msg(RoleUser, "question"),
		assistantCall("call_1", "dump"),
		toolResult("call_1", huge),
	}

	got := TrimToBudget(messages, 2000)
	if !validPairing(got) {
		t.Fatalf("pairing broken: %+v", roles(got))
	}
	if len(got) != 4 {
		t.Fatalf("messages were dropped instead of truncated: %+v", roles(got))
	}
	result := got[3]
	if len(result.Content) >= len(huge) {
		t.Fatal("the oversized tool result was not truncated")
	}
	if !strings.HasSuffix(result.Content, truncationNotice) {
		t.Fatalf("a truncated result must say so: %q", tail(result.Content))
	}
	// The original is untouched: trimming must not mutate the caller's data.
	if len(messages[3].Content) != len(huge) {
		t.Fatal("TrimToBudget mutated its input")
	}
}

// The largest result is cut first, so the fewest results are damaged.
func TestTrimTruncatesTheLargestResultFirst(t *testing.T) {
	small := strings.Repeat("s", 400)
	large := strings.Repeat("L", 8000)
	messages := []Message{
		msg(RoleUser, "question"),
		assistantCall("call_1", "a"),
		toolResult("call_1", small),
		assistantCall("call_2", "b"),
		toolResult("call_2", large),
	}

	got := TrimToBudget(messages, 2500)
	if !validPairing(got) {
		t.Fatalf("pairing broken: %+v", roles(got))
	}
	if got[2].Content != small {
		t.Fatalf("the small result was cut even though a larger one existed: %d chars", len(got[2].Content))
	}
	if len(got[4].Content) >= len(large) {
		t.Fatal("the large result was not cut")
	}
}

// A conversation with no user message at all (a system prompt plus a
// half-finished turn) must not panic or lose its pairing.
func TestTrimHandlesTranscriptWithNoUserMessage(t *testing.T) {
	messages := []Message{
		msg(RoleSystem, "sys"),
		assistantCall("call_1", "a"),
		toolResult("call_1", strings.Repeat("r", 5000)),
	}
	got := TrimToBudget(messages, 1000)
	if !validPairing(got) {
		t.Fatalf("pairing broken: %+v", roles(got))
	}
	if len(got) != 3 {
		t.Fatalf("messages dropped where truncation was the only safe move: %+v", roles(got))
	}
}

// A budget of zero means "do not trim": an unconfigured context window must
// not silently destroy a conversation.
func TestTrimWithoutBudgetDoesNothing(t *testing.T) {
	messages := []Message{
		msg(RoleSystem, "sys"),
		msg(RoleUser, strings.Repeat("q", 5000)),
		msg(RoleAssistant, strings.Repeat("a", 5000)),
	}
	got := TrimToBudget(messages, 0)
	if len(got) != 3 || len(got[1].Content) != 5000 {
		t.Fatal("a zero budget must leave the transcript alone")
	}
}

func TestBudgetForContextWindow(t *testing.T) {
	if got := BudgetChars(1000, 200); got != 800*CharsPerToken {
		t.Fatalf("budget: %d", got)
	}
	// Reserving more than the window leaves nothing, not a negative budget.
	if got := BudgetChars(100, 500); got != 0 {
		t.Fatalf("budget must not go negative: %d", got)
	}
}

// --- helpers -------------------------------------------------------------

func roles(messages []Message) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		out = append(out, string(m.Role))
	}
	return out
}

func containsContent(messages []Message, content string) bool {
	for _, m := range messages {
		if m.Content == content {
			return true
		}
	}
	return false
}

func containsToolResult(messages []Message, callID string) bool {
	for _, m := range messages {
		if m.Role == RoleTool && m.ToolCallID == callID {
			return true
		}
	}
	return false
}

func tail(s string) string {
	if len(s) < 40 {
		return s
	}
	return s[len(s)-40:]
}
