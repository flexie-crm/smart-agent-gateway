package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/tool"
)

// newTestQueue builds a runner whose only wired part is the memory queue, with a
// no-op distiller. It is enough to test what scheduling decides and enqueues,
// which is all logic and no model call.
func newTestQueue() *Runner {
	r := &Runner{log: zerolog.Nop()}
	r.memory = newMemoryQueue(func(context.Context, distillRequest) {}, zerolog.Nop())
	return r
}

// drainQueue reads everything currently queued without blocking.
func drainQueue(q *memoryQueue) []distillRequest {
	var out []distillRequest
	for {
		select {
		case req := <-q.reqs:
			out = append(out, req)
		default:
			return out
		}
	}
}

// The cadence is the baseline trigger: the first turn (to bootstrap memory) and
// then every few turns, and nothing in between. This is what keeps most turns
// free of any memory work at all.
func TestScheduleMemoryCadence(t *testing.T) {
	cases := []struct {
		userTurns int
		want      bool
	}{
		{1, true},  // bootstrap on the first turn
		{2, false}, // quiet in between
		{4, false},
		{5, true}, // every fifth
		{6, false},
		{10, true},
	}
	for _, tc := range cases {
		r := newTestQueue()
		r.scheduleMemory(Turn{WorkspaceID: 1, UserID: 2, SessionID: 3, ModelID: 4}, tc.userTurns, &memoryFlag{})
		got := drainQueue(r.memory)
		if (len(got) == 1) != tc.want {
			t.Fatalf("turn %d: scheduled=%v, want %v", tc.userTurns, len(got) == 1, tc.want)
		}
		if tc.want {
			// A cadence sweep reconsiders both memories, with no hint.
			if len(got[0].Scopes) != 2 || got[0].Hint != "" {
				t.Fatalf("turn %d: cadence should sweep both scopes with no hint: %+v", tc.userTurns, got[0])
			}
		}
	}
}

// The model can force an update off the cadence by flagging the turn. Then only
// the scope it named is reconsidered, and its hint rides along to the distiller.
func TestScheduleMemoryModelFlagOffCadence(t *testing.T) {
	r := newTestQueue()
	flag := &memoryFlag{}
	flag.raise("person", "works in UTC, wants terse answers")

	// Turn 2 is not a cadence turn, so only the flag can schedule this.
	r.scheduleMemory(Turn{WorkspaceID: 1, UserID: 2, SessionID: 3, ModelID: 4}, 2, flag)

	got := drainQueue(r.memory)
	if len(got) != 1 {
		t.Fatalf("a flagged turn was not scheduled: %+v", got)
	}
	if len(got[0].Scopes) != 1 || got[0].Scopes[0] != memoryScopeUser {
		t.Fatalf("the flag's scope was not honoured: %+v", got[0].Scopes)
	}
	if !strings.Contains(got[0].Hint, "terse answers") {
		t.Fatalf("the model's hint did not reach the distiller: %q", got[0].Hint)
	}
}

// On a cadence turn, a flag does not shrink the sweep: both memories are still
// reconsidered, and the narrower scope of the flag is ignored in favour of the
// periodic sweep.
func TestScheduleMemoryCadenceWinsOverFlagScope(t *testing.T) {
	r := newTestQueue()
	flag := &memoryFlag{}
	flag.raise("person", "a hint")

	r.scheduleMemory(Turn{WorkspaceID: 1, UserID: 2, SessionID: 3, ModelID: 4}, 5, flag)

	got := drainQueue(r.memory)
	if len(got) != 1 || len(got[0].Scopes) != 2 {
		t.Fatalf("a cadence turn should still sweep both scopes: %+v", got)
	}
}

// A turn with no resolved model has nothing for the background work to run on,
// so it is never scheduled.
func TestScheduleMemorySkipsWhenNoModel(t *testing.T) {
	r := newTestQueue()
	r.scheduleMemory(Turn{WorkspaceID: 1, UserID: 2, SessionID: 3, ModelID: 0}, 1, &memoryFlag{})
	if got := drainQueue(r.memory); len(got) != 0 {
		t.Fatalf("a turn with no model was scheduled: %+v", got)
	}
}

// Scheduling must never block the turn that schedules it. A full queue drops the
// request and moves on, the same drop-slow rule the socket hub uses.
func TestMemoryQueueDropsWhenFull(t *testing.T) {
	q := newMemoryQueue(func(context.Context, distillRequest) {}, zerolog.Nop())
	// Fill it to capacity.
	for i := 0; i < memoryQueueBuffer; i++ {
		q.enqueue(distillRequest{SessionID: int64(i)})
	}
	// One more must not block and must not grow the queue past its bound.
	q.enqueue(distillRequest{SessionID: -1})
	if len(q.reqs) != memoryQueueBuffer {
		t.Fatalf("a full queue grew past its bound: %d", len(q.reqs))
	}
}

// The flag accumulates what the model named across calls within one turn: two
// scopes flagged means both are reconsidered, and the hints are gathered.
func TestMemoryFlagAccumulates(t *testing.T) {
	f := &memoryFlag{}
	f.raise("person", "prefers terse answers")
	f.raise("workspace", "list models before disabling")
	f.raise("person", "prefers terse answers") // a duplicate scope is not added twice

	if !f.raised {
		t.Fatal("the flag was not raised")
	}
	if len(f.scopes) != 2 {
		t.Fatalf("scopes were not de-duplicated across calls: %+v", f.scopes)
	}
	if !strings.Contains(f.hint, "terse") || !strings.Contains(f.hint, "list models") {
		t.Fatalf("the hints were not gathered: %q", f.hint)
	}
}

// Malformed arguments still raise the flag: the model's intent to remember is
// clear even when it botched the shape, and a both-scopes sweep is the safe read.
func TestNoteMemoryFlagToleratesBadArgs(t *testing.T) {
	f := &memoryFlag{}
	noteMemoryFlag(f, []byte(`not json at all`))
	if !f.raised {
		t.Fatal("a malformed memory call did not raise the flag")
	}
	if len(f.scopes) != 0 || f.hint != "" {
		t.Fatalf("a malformed call invented a scope or hint: %+v", f)
	}
}

func TestNoteMemoryFlagReadsScopeAndNote(t *testing.T) {
	f := &memoryFlag{}
	noteMemoryFlag(f, []byte(`{"scope":"workspace","note":"always confirm before disabling a model"}`))
	if len(f.scopes) != 1 || f.scopes[0] != memoryScopeWorkspace {
		t.Fatalf("scope not read: %+v", f.scopes)
	}
	if !strings.Contains(f.hint, "confirm before disabling") {
		t.Fatalf("note not read: %q", f.hint)
	}
}

// The distiller reads only what was said, most-recent last, within a bounded
// window, so a long conversation still yields a compact, current summary.
func TestRecentConversation(t *testing.T) {
	var steps []*model.AgentStep
	for i := 0; i < 20; i++ {
		steps = append(steps,
			&model.AgentStep{Kind: model.StepUser, Text: "question " + string(rune('a'+i))},
			&model.AgentStep{Kind: model.StepAssistant, Text: "answer " + string(rune('a'+i))},
		)
	}
	// A partial (still streaming) step and a reasoning-only step are left out.
	steps = append(steps, &model.AgentStep{Kind: model.StepAssistant, Text: "streaming", Partial: true})

	got := recentConversation(steps, 6)
	lines := strings.Split(got, "\n\n")
	if len(lines) != 6 {
		t.Fatalf("the window was not respected: %d lines", len(lines))
	}
	if strings.Contains(got, "streaming") {
		t.Fatalf("a partial step leaked into the summary:\n%s", got)
	}
	if !strings.HasPrefix(lines[0], "User:") && !strings.HasPrefix(lines[0], "Assistant:") {
		t.Fatalf("a line lost its speaker: %q", lines[0])
	}
}

func TestCountUserTurns(t *testing.T) {
	messages := []provider.Message{
		{Role: provider.RoleSystem, Content: "sys"},
		{Role: provider.RoleUser, Content: "one"},
		{Role: provider.RoleAssistant, Content: "..."},
		{Role: provider.RoleUser, Content: "two"},
		{Role: provider.RoleTool, Content: "{}"},
	}
	if got := countUserTurns(messages); got != 2 {
		t.Fatalf("countUserTurns = %d, want 2", got)
	}
}

// The two scopes ask for genuinely different notes: one about the person, one
// about doing the work. Both carry the same anti-leak, no-secrets discipline.
func TestMemorySystemPromptDiffersByScope(t *testing.T) {
	person := memorySystemPrompt(memoryScopeUser)
	workspace := memorySystemPrompt(memoryScopeWorkspace)

	if !strings.Contains(person, "profile of a person") {
		t.Fatalf("the person prompt lost its subject:\n%s", person)
	}
	if !strings.Contains(workspace, "working notes") {
		t.Fatalf("the workspace prompt lost its subject:\n%s", workspace)
	}
	for _, p := range []string{person, workspace} {
		if !strings.Contains(p, "Never include secrets") {
			t.Fatalf("a scope prompt dropped the no-secrets rule:\n%s", p)
		}
		if !strings.Contains(p, "unchanged") {
			t.Fatalf("a scope prompt dropped the leave-trivial-unchanged rule:\n%s", p)
		}
		// Every scope grounds the note in the real tools and prunes phantom ones.
		if !strings.Contains(p, "Tools that exist") || !strings.Contains(p, "prune") {
			t.Fatalf("a scope prompt dropped the tool-grounding rule:\n%s", p)
		}
	}
}

// The inventory lists the AGENT'S OWN loadout tools by the name the model calls
// and its friendly name, excludes internal infrastructure, and is empty when
// there are none, so the prompt shows "(none)" and the distiller is grounded in
// what this agent can really do, not the whole workspace's toolbox.
func TestFormatLoadoutTools(t *testing.T) {
	out := formatLoadoutTools(tool.Loadout{Schemas: []tool.Schema{
		{Name: "query_demo24_db", FriendlyName: "Demo CRM"},
		{Name: "http_request"},
		{Name: "tool_guide", Kind: tool.KindInternal},
	}})
	if !strings.Contains(out, "query_demo24_db (Demo CRM)") {
		t.Fatalf("a tool's name and friendly name are missing:\n%s", out)
	}
	if !strings.Contains(out, "http_request") || strings.Contains(out, "http_request (") {
		t.Fatalf("a tool without a friendly name is rendered wrong:\n%s", out)
	}
	if strings.Contains(out, "tool_guide") {
		t.Fatalf("internal infrastructure must not appear in the inventory:\n%s", out)
	}
	if formatLoadoutTools(tool.Loadout{}) != "" {
		t.Fatal("no tools should render as empty")
	}
}

// The task lays out existing note, hint, and conversation for the distiller,
// and omits the sections that are empty so the model is not fed blank headings.
func TestMemoryTaskComposition(t *testing.T) {
	full := memoryTask("old note", "conversation here", "remember this", "- query_demo24_db (Demo CRM)")
	for _, want := range []string{
		"## Existing note", "old note",
		"## Tools that exist", "query_demo24_db (Demo CRM)",
		"## What the assistant asked to remember", "remember this",
		"## Recent conversation", "conversation here",
	} {
		if !strings.Contains(full, want) {
			t.Fatalf("the task is missing %q:\n%s", want, full)
		}
	}

	// The existing-note and hint sections drop when empty; the tools section
	// always shows (with "(none)"), so the model is always told what exists.
	bare := memoryTask("", "conversation here", "", "")
	if strings.Contains(bare, "## Existing note") || strings.Contains(bare, "asked to remember") {
		t.Fatalf("empty sections were emitted:\n%s", bare)
	}
	if !strings.Contains(bare, "## Tools that exist\n(none)") {
		t.Fatalf("the tools section should always show, as (none) when empty:\n%s", bare)
	}
}

// A bad-arguments tool failure becomes a workspace-scoped memory flag whose hint
// carries three things the distiller needs to write a real lesson: which tool,
// what the error was, and how to use it correctly (its guide). This is the loop
// learning from its own mistakes.
func TestFlagToolMistakeBuildsAWorkspaceLesson(t *testing.T) {
	flag := &memoryFlag{}
	schema := tool.Schema{
		Name:         "http_request",
		FriendlyName: "API request",
		Description:  "Make an HTTP request.",
		Guide:        []byte(`{"summary":"query params stay on the url"}`),
	}
	result := tool.Result{
		Content: []byte(`{"success":false,"error":"provide a full http(s) URL"}`),
		Err:     tool.ErrorBadArguments,
	}

	flagToolMistake(flag, schema, result)

	if !flag.raised {
		t.Fatal("a bad-arguments failure did not raise the flag")
	}
	// It teaches the workspace, not the person.
	if len(flag.scopes) != 1 || flag.scopes[0] != memoryScopeWorkspace {
		t.Fatalf("the lesson was not workspace-scoped: %+v", flag.scopes)
	}
	for _, want := range []string{"API request", "provide a full http(s) URL", "query params stay on the url"} {
		if !strings.Contains(flag.hint, want) {
			t.Fatalf("the hint is missing %q:\n%s", want, flag.hint)
		}
	}
}

// A nil flag (a path that does not collect one) must not panic.
func TestFlagToolMistakeToleratesNoFlag(t *testing.T) {
	flagToolMistake(nil, tool.Schema{Name: "x"}, tool.Result{Err: tool.ErrorBadArguments})
}
