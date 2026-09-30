package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
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
		// The newest step, the call the model is waiting on and its answer,
		// is kept whatever the budget.
		if !containsToolResult(got, "call_live") {
			t.Fatalf("budget %d dropped the live tool result", budget)
		}
	}
}

// When the newest step does not fit even on its own, everything older goes
// and what its tool answered is cut short. The pairing is never broken.
func TestTrimCutsTheNewestStepOnlyWhenItAloneDoesNotFit(t *testing.T) {
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
	if strings.Join(roles(got), ",") != "system,assistant,tool" {
		t.Fatalf("kept %v, want the system prompt and the newest step", roles(got))
	}
	result := got[2]
	if len(result.Content) >= len(huge) {
		t.Fatal("the oversized tool result was not truncated")
	}
	if !strings.HasSuffix(result.Content, truncationNotice) {
		t.Fatalf("a truncated result must say so: %q", tail(result.Content))
	}
	if size(got) > 2000 {
		t.Fatalf("still over budget: %d", size(got))
	}
	// The original is untouched: trimming must not mutate the caller's data.
	if len(messages[3].Content) != len(huge) {
		t.Fatal("TrimToBudget mutated its input")
	}
}

// The case that reached production: one question, then a long run of tool
// steps whose bulk is the tool calls' own arguments, or thinking, rather than
// what the tools answered. Only the newest steps that fit are sent, whatever
// kind of message the bulk is, and at every window the request fits.
func TestTrimDropsTheOldestWhateverItIs(t *testing.T) {
	build := func(argChars, resultChars, reasoningChars int) []Message {
		messages := []Message{msg(RoleSystem, strings.Repeat("s", 20_000)), msg(RoleUser, "fix the build")}
		for i := 0; i < 100; i++ {
			id := fmt.Sprintf("call_%03d", i)
			args, _ := json.Marshal(map[string]string{"path": id, "content": strings.Repeat("a", argChars)})
			messages = append(messages,
				Message{Role: RoleAssistant, Reasoning: strings.Repeat("r", reasoningChars),
					ToolCalls: []ToolCall{{ID: id, Name: "write_file", Args: args}}},
				toolResult(id, strings.Repeat("o", resultChars)))
		}
		return messages
	}
	for _, shape := range []struct {
		name                     string
		args, results, reasoning int
	}{
		{"tool results", 100, 10_000, 0},
		{"tool-call arguments", 10_000, 100, 0},
		{"thinking", 100, 100, 10_000},
	} {
		messages := build(shape.args, shape.results, shape.reasoning)
		for _, window := range []int{200_000, 50_000} {
			budget := BudgetChars(window, 8000, GuessCharsPerToken)
			got := TrimToBudget(messages, budget)
			if size(got) > budget {
				t.Fatalf("%s, window %d: sent %d characters over a budget of %d", shape.name, window, size(got), budget)
			}
			if !validPairing(got) {
				t.Fatalf("%s, window %d: pairing broken", shape.name, window)
			}
			if got[0].Role != RoleSystem || !containsToolResult(got, "call_099") {
				t.Fatalf("%s, window %d: the system prompt or the newest step was dropped", shape.name, window)
			}
			// What is kept is the newest run, untouched: the same messages the
			// conversation ends with, in order, and nothing cut in them.
			kept := got[1:]
			tailOf := messages[len(messages)-len(kept):]
			for i := range kept {
				if kept[i].Content != tailOf[i].Content || kept[i].Reasoning != tailOf[i].Reasoning ||
					kept[i].ToolCallID != tailOf[i].ToolCallID {
					t.Fatalf("%s, window %d: message %d is not the conversation's own newest run", shape.name, window, i)
				}
			}
			if containsContent(got, "fix the build") {
				t.Fatalf("%s, window %d: the oldest message was kept while newer ones were dropped", shape.name, window)
			}
		}
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
	if got := BudgetChars(1000, 200, 3); got != 2280 { // 800 tokens x 3 x 0.95
		t.Fatalf("budget: %d, want 2,280", got)
	}
	// Reserving more than the window leaves nothing, not a negative budget.
	if got := BudgetChars(100, 500, 3); got != 0 {
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

// What the chat shows and what the trim does are one measure: a request at
// 100% is sent whole, and the smallest step past it is where the oldest
// messages start to go. If these two ever disagreed, the meter would say there
// is room while messages were being dropped, or the other way round.
func TestFullIsWhereTheTrimStarts(t *testing.T) {
	m := &model.AIModel{ContextWindow: 20_000}
	tools := []ToolDef{{Name: "t", Description: repeat("d", 3000), InputSchema: json.RawMessage(`{}`)}}
	room := BudgetFor(m, reserveTokens)

	// Grow a conversation one message at a time until the next one would pass
	// 100%, then take that next one.
	var messages []Message
	for i := 0; ; i++ {
		next := append(append([]Message{}, messages...), msg(RoleUser, fmt.Sprintf("message %03d %s", i, repeat("x", 200))))
		if RequestChars(next, tools) > room {
			full, _ := Fullness(m, RequestChars(messages, tools))
			over, _ := Fullness(m, RequestChars(next, tools))
			if full > 100 || over <= 100 {
				t.Fatalf("the meter says %d%% just before the line and %d%% just past it", full, over)
			}
			if got := TrimToBudget(messages, messageBudget(m, tools)); len(got) != len(messages) {
				t.Fatalf("at %d%% the trim dropped %d messages", full, len(messages)-len(got))
			}
			if got := TrimToBudget(next, messageBudget(m, tools)); len(got) == len(next) {
				t.Fatalf("past 100%% (%d%%) the trim dropped nothing", over)
			}
			return
		}
		messages = next
	}
}

func TestFullnessIsAgainstTheModelsWindow(t *testing.T) {
	m := &model.AIModel{ContextWindow: 100_000}
	room := BudgetFor(m, reserveTokens) // (100,000 - 8,000) x 2 x 0.95 = 174,800
	for chars, want := range map[int]int{0: 0, room / 2: 50, room - 1: 100, room: 100, room + 1: 101, room * 3 / 2: 150} {
		if got, ok := Fullness(m, chars); !ok || got != want {
			t.Fatalf("%d characters of %d came out %d%% (%v), want %d%%", chars, room, got, ok, want)
		}
	}
	if _, ok := Fullness(&model.AIModel{}, 1000); ok {
		t.Fatal("a model with no window was given a percentage")
	}
	if _, ok := Fullness(nil, 1000); ok {
		t.Fatal("no model was given a percentage")
	}
}
