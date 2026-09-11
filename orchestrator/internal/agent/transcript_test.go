package agent

import (
	"encoding/json"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
)

// Rebuilding the conversation is the one place where stored rows become the
// thing a vendor sees. If it is wrong, the model is lied to about what
// happened, so every rule it applies is tested here.

func userStep(seq int, text string) *model.AgentStep {
	return &model.AgentStep{Seq: seq, Kind: model.StepUser, Text: text}
}

func call(id, name, result, status string) *model.ToolCall {
	c := &model.ToolCall{ToolCallID: id, ToolName: name, Status: status, Args: json.RawMessage(`{}`)}
	if result != "" {
		c.Result = json.RawMessage(result)
	}
	return c
}

// A tool result is not a message: it belongs to the call that produced it, and
// the model-facing role=tool message is synthesized from that one copy.
func TestAToolResultIsSynthesizedFromItsCall(t *testing.T) {
	steps := []*model.AgentStep{
		userStep(0, "what time is it"),
		{
			Seq:       1,
			Kind:      model.StepAssistant,
			Reasoning: "I need the clock.",
			ToolCalls: []*model.ToolCall{
				call("c1", "current_time", `{"utc":"2026-07-12T00:00:00Z"}`, model.ToolCallCompleted),
			},
		},
		{Seq: 2, Kind: model.StepAssistant, Text: "It is midnight."},
	}

	messages := conversation("You are helpful.", steps, "")

	want := []provider.Role{
		provider.RoleSystem,
		provider.RoleUser,
		provider.RoleAssistant, // the tool-calling step, which said nothing
		provider.RoleTool,      // its result, synthesized
		provider.RoleAssistant,
	}
	if len(messages) != len(want) {
		t.Fatalf("expected %d messages, got %d: %+v", len(want), len(messages), messages)
	}
	for i, role := range want {
		if messages[i].Role != role {
			t.Fatalf("message %d is %q, want %q", i, messages[i].Role, role)
		}
	}

	// The tool-calling step carries its reasoning and its call, and no text.
	step := messages[2]
	if step.Content != "" {
		t.Fatalf("a tool-only step must not invent text: %q", step.Content)
	}
	if step.Reasoning != "I need the clock." {
		t.Fatalf("the step's reasoning was lost: %q", step.Reasoning)
	}
	if len(step.ToolCalls) != 1 || step.ToolCalls[0].ID != "c1" {
		t.Fatalf("the call did not survive: %+v", step.ToolCalls)
	}

	// The result is paired to the call by its id, which is what every vendor
	// validates.
	if messages[3].ToolCallID != "c1" {
		t.Fatalf("the result is not paired to its call: %+v", messages[3])
	}
	if messages[3].Content != `{"utc":"2026-07-12T00:00:00Z"}` {
		t.Fatalf("the stored result was not handed back: %q", messages[3].Content)
	}
}

// One assistant turn is several steps, each with its own reasoning and its own
// calls. The old shape could hold one of each, which is the case that never
// happens, and it silently dropped the rest.
func TestEveryStepKeepsItsOwnReasoningAndCalls(t *testing.T) {
	steps := []*model.AgentStep{
		userStep(0, "book it"),
		{
			Seq: 1, Kind: model.StepAssistant,
			Reasoning: "First, check availability.",
			ToolCalls: []*model.ToolCall{
				call("c1", "check", `{"free":true}`, model.ToolCallCompleted),
			},
		},
		{
			Seq: 2, Kind: model.StepAssistant,
			Text:      "It is free, booking now.",
			Reasoning: "Now book, and tell them.",
			ToolCalls: []*model.ToolCall{
				call("c2", "book", `{"ok":true}`, model.ToolCallCompleted),
				call("c3", "notify", `{"sent":true}`, model.ToolCallCompleted),
			},
		},
		{Seq: 3, Kind: model.StepAssistant, Text: "Booked."},
	}

	messages := conversation("", steps, "")

	// user, step1, c1, step2, c2, c3, step3
	if len(messages) != 7 {
		t.Fatalf("expected 7 messages, got %d: %+v", len(messages), messages)
	}
	if messages[1].Reasoning != "First, check availability." {
		t.Fatalf("the first step's reasoning was lost: %q", messages[1].Reasoning)
	}
	if messages[3].Reasoning != "Now book, and tell them." {
		t.Fatalf("the second step's reasoning was lost: %q", messages[3].Reasoning)
	}
	if messages[3].Content != "It is free, booking now." {
		t.Fatalf("a step that spoke AND called tools lost its text: %q", messages[3].Content)
	}
	// Two parallel calls in one step, each answered, in order.
	if len(messages[3].ToolCalls) != 2 {
		t.Fatalf("parallel calls were not kept together: %+v", messages[3].ToolCalls)
	}
	if messages[4].ToolCallID != "c2" || messages[5].ToolCallID != "c3" {
		t.Fatalf("parallel results are out of order: %q then %q",
			messages[4].ToolCallID, messages[5].ToolCallID)
	}
}

// A call left unanswered by an abandoned turn would make the conversation
// unusable from that point on: every vendor rejects a call with no result. It
// is told the truth instead, which is both accurate and recoverable.
func TestAnUnansweredCallIsToldItDidNotRun(t *testing.T) {
	steps := []*model.AgentStep{
		userStep(0, "delete it"),
		{
			Seq: 1, Kind: model.StepAssistant,
			ToolCalls: []*model.ToolCall{
				call("c1", "delete", "", model.ToolCallApprovalRequired),
			},
		},
	}

	messages := conversation("", steps, "")

	if len(messages) != 3 {
		t.Fatalf("expected the call to be answered, got %+v", messages)
	}
	if messages[2].Role != provider.RoleTool || messages[2].ToolCallID != "c1" {
		t.Fatalf("the pending call was left unpaired: %+v", messages[2])
	}
	if messages[2].Content != string(notExecuted) {
		t.Fatalf("the model was not told the action did not run: %q", messages[2].Content)
	}
}

// The call a resumed turn is about to run is left out: the turn resolves it
// first and the conversation is rebuilt afterwards, so its real result lands in
// the right place rather than after a placeholder.
func TestTheResumedCallIsSkipped(t *testing.T) {
	steps := []*model.AgentStep{
		userStep(0, "delete it"),
		{
			Seq: 1, Kind: model.StepAssistant,
			ToolCalls: []*model.ToolCall{
				call("c1", "delete", "", model.ToolCallApprovalRequired),
			},
		},
	}

	messages := conversation("", steps, "c1")

	if len(messages) != 2 {
		t.Fatalf("the skipped call was still answered: %+v", messages)
	}
}

// The system prompt comes from the configuration, never from history: changing
// an agent must change how it behaves next turn, not replay what it used to be.
func TestTheSystemPromptComesFromTheConfiguration(t *testing.T) {
	messages := conversation("You are the sales assistant.", []*model.AgentStep{
		userStep(0, "hi"),
	}, "")

	if len(messages) != 2 {
		t.Fatalf("expected a prompt and a message: %+v", messages)
	}
	if messages[0].Role != provider.RoleSystem || messages[0].Content != "You are the sales assistant." {
		t.Fatalf("the configured prompt did not lead the conversation: %+v", messages[0])
	}

	// And with no prompt configured, there is no system message at all.
	messages = conversation("", []*model.AgentStep{userStep(0, "hi")}, "")
	if len(messages) != 1 || messages[0].Role != provider.RoleUser {
		t.Fatalf("an empty prompt produced a system message: %+v", messages)
	}
}

// The wreckage of an interrupted turn is a step that produced nothing. A model
// shown an empty assistant turn is being told it once said nothing, which is
// both false and, for some vendors, a hard error.
func TestAnEmptyStepIsNotShownToTheModel(t *testing.T) {
	steps := []*model.AgentStep{
		userStep(0, "hi"),
		{Seq: 1, Kind: model.StepAssistant, Partial: true},
	}

	messages := conversation("", steps, "")

	if len(messages) != 1 {
		t.Fatalf("an empty step reached the model: %+v", messages)
	}
}

// A partial step is real text the user already saw. It is kept, because the
// alternative is an assistant that visibly said something and then denies it.
func TestAPartialStepIsStillPartOfTheConversation(t *testing.T) {
	steps := []*model.AgentStep{
		userStep(0, "hi"),
		{Seq: 1, Kind: model.StepAssistant, Text: "I was saying th", Partial: true},
	}

	messages := conversation("", steps, "")

	if len(messages) != 2 || messages[1].Content != "I was saying th" {
		t.Fatalf("the interrupted answer was dropped: %+v", messages)
	}
}
