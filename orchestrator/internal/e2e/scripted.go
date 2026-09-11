// Package e2e is the deterministic browser end-to-end harness. It boots a real
// orchestrator against a scratch database, wires a scripted model in place of a
// real vendor, and seeds just enough (a Gateway, one background agent) for
// Playwright to drive the full Mode C flow (KB/27) and assert the live behaviour
// unit tests cannot see: a socket push turning into a DOM change with no reload.
//
// Nothing here is compiled into the production binary; it is reached only by the
// cmd/sag-e2e harness the `make e2e` target runs.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
)

// The agent the E2E flow delegates to, and the one safe tool it calls. The
// tool is current_time (deterministic, no network) gated for approval through
// the agent's confirm set, so the flow parks on a real card without any
// dependency on the outside world.
const (
	AgentKey = "e2e-worker"
	// QuietAgentKey is the same agent with an ungated tool, for the specs that
	// are about a batch rather than about its cards.
	QuietAgentKey = "e2e-quiet"
	AgentTool     = "current_time"
	// scriptedResult is the agent's terminal answer, which becomes the
	// delegation result the Gateway narrates. A distinctive token so a test can
	// assert the completion narration actually carried the agent's work.
	scriptedResult = "E2E-RESULT-42"

	// The brain the Gateway writes to (a shared knowledge base) and its category,
	// plus the memory brain it manages. Constants so the seed, the script, and the
	// spec cannot drift. The distinctive save markers let a spec prove the write
	// landed and the Gateway narrated it.
	KnowledgeBrain    = "Team Notes"
	KnowledgeCategory = "Decisions"
	MemoryBrain       = "Field Notes"
	brainSavedMarker  = "BRAIN-E2E-SAVED"
	memorySavedMarker = "MEMORY-E2E-SAVED"

	// lingerMarker in a delegated task makes that agent's first model call take a
	// visible moment, so a spec can look at the chips WHILE the work is running.
	//
	// It exists because the scripted model has no latency: every other background
	// spec has to auto-approve, the agents finish in about a second, and a chip
	// that only ever appeared once the work was over would satisfy all of them.
	// That is the bug we shipped. A pause is the smallest thing that makes the
	// difference observable, and it is not a fiction: a real model takes seconds,
	// which is the whole reason the chips exist.
	lingerMarker = "E2E-LINGER"
	// Long enough to assert in, short enough not to be the suite's slowest spec.
	lingerFor = 6 * time.Second

	// FleetSize is how many agents the fleet flow starts. Three: enough that
	// the chip has to count rather than say "running", and enough that the join
	// is a real join rather than a rename of one delegation finishing.
	FleetSize = 3
	// fleetNarration is what the Gateway says once the whole batch is back. A
	// distinctive token, so a spec can prove the completion turn arrived LIVE,
	// which is the one thing about a fleet a unit test cannot see.
	fleetNarration = "E2E-FLEET-DONE"
)

// ScriptedProvider is a provider.Provider that answers from the shape of the
// request rather than a model, so the whole delegate/park/resume/complete flow
// is deterministic. It distinguishes the Gateway (holds the delegate tool) from
// the agent (holds the agent tool), and within each, where in the flow
// it is, from the messages it is given.
type ScriptedProvider struct{}

func (p *ScriptedProvider) Name() string { return "scripted" }

func (p *ScriptedProvider) Capabilities(context.Context, string) (provider.Capabilities, error) {
	return provider.Capabilities{
		SupportsTools:     true,
		SupportsStreaming: true,
		SupportsReasoning: false,
		ContextWindow:     100_000,
	}, nil
}

func (p *ScriptedProvider) ListModels(context.Context) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{ID: "scripted-1", Name: "Scripted"}}, nil
}

func (p *ScriptedProvider) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, provider.ErrUnsupported
}

func (p *ScriptedProvider) Generate(_ context.Context, req provider.GenerateRequest) (*provider.GenerateResponse, error) {
	msg := provider.Message{Role: provider.RoleAssistant}
	for _, ev := range p.script(req) {
		switch ev.Kind {
		case provider.EventContentDelta:
			msg.Content += ev.ContentDelta
		case provider.EventToolCall:
			msg.ToolCalls = append(msg.ToolCalls, *ev.ToolCall)
		}
	}
	return &provider.GenerateResponse{Message: msg}, nil
}

func (p *ScriptedProvider) Stream(ctx context.Context, req provider.GenerateRequest) (<-chan provider.StreamEvent, error) {
	events := p.script(req)
	ch := make(chan provider.StreamEvent, len(events)+2)
	go func() {
		defer close(ch)
		if wait := linger(req); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		if wait := dawdle(req); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		gap := pace(req)
		for _, ev := range events {
			select {
			case <-ctx.Done():
				return
			case ch <- ev:
			}
			if gap > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(gap):
				}
			}
		}
		// Every real provider reports token usage at a call's end; the scripted
		// one does too, so a background agent's chip shows a running spend.
		ch <- provider.StreamEvent{Kind: provider.EventUsage, Usage: &provider.Usage{InputTokens: 1000, OutputTokens: 250}}
		ch <- provider.StreamEvent{Kind: provider.EventDone}
	}()
	return ch, nil
}

// linger says how long this call should take before it answers.
//
// Only an agent's FIRST call, and only one carrying the marker. The Gateway is
// never held up: a spec that watches an agent work needs the turn that started
// it to have ended, and holding a delegation's narration would only make the
// spec slower. The second call is not held either, because the pause is there
// to be looked at once, not to be paid twice.
func linger(req provider.GenerateRequest) time.Duration {
	if hasTool(req.Tools, "delegate") || hasRole(req.Messages, provider.RoleTool) {
		return 0
	}
	if !mentions(req.Messages, lingerMarker) {
		return 0
	}
	return lingerFor
}

// dawdle keeps a turn OPEN, by taking a moment over every step of it.
//
// A turn that is still running is the whole precondition for saying something
// into one, and the scripted model is otherwise instant: it calls a tool, gets
// the result, calls it again, and is finished before a browser can type a word.
// A spec written against that proves nothing, and one of them did: it passed
// while the message was in fact being sent as an ordinary second turn.
func dawdle(req provider.GenerateRequest) time.Duration {
	if !mentions(req.Messages, "keep checking") {
		return 0
	}
	return 700 * time.Millisecond
}

// pace says how long to leave between one piece of an answer and the next.
//
// An answer that arrives in a single frame cannot be watched ARRIVING, and
// whether the conversation follows one as it grows is a question about the
// frames in between. Only for the script that exists to be watched.
func pace(req provider.GenerateRequest) time.Duration {
	if mentions(req.Messages, "talk for a long time") {
		// 120 pieces at 60ms is about seven seconds, which is what it takes to
		// scroll up inside a live answer and then watch it for a while.
		return 60 * time.Millisecond
	}
	if !mentions(req.Messages, "think out loud") {
		return 0
	}
	return 40 * time.Millisecond
}

// script is the whole decision: given a request, what the model "says". It is a
// pure function of the request so the flow is reproducible. It is a tiny
// interpreter: the Gateway is steered by keywords in the person's prompt, so a
// spec drives the flow it wants; the agent and the completion answer from
// the shape of their context.
func (p *ScriptedProvider) script(req provider.GenerateRequest) []provider.StreamEvent {
	// A completion turn frames a finished delegation to the Gateway with a
	// synthetic system marker; the Gateway narrates it (KB/27). A declined task is
	// said plainly, not as a result.
	// A whole batch is back. It is a DIFFERENT wake-up from a single agent's
	// (KB/27), and the harness keys off the real marker so it cannot drift from
	// what the runtime writes.
	if isFleetCompletion(req.Messages) {
		return text(fleetNarration + " All " + strconv.Itoa(FleetSize) + " agents reported: " + resultToken(req.Messages) + ".")
	}
	if isCompletion(req.Messages) {
		if mentions(req.Messages, "declined") || mentions(req.Messages, "could not be completed") {
			return text("The background task did not complete: the action was declined.")
		}
		return text("The background task finished. Result: " + resultToken(req.Messages) + ".")
	}

	if hasTool(req.Tools, "delegate") {
		return p.Gateway(req)
	}
	return p.agent(req)
}

// Gateway is the top agent: on a fresh turn it reads the person's prompt for a
// keyword; on a continuation (a tool has just run) it reports and ends.
func (p *ScriptedProvider) Gateway(req provider.GenerateRequest) []provider.StreamEvent {
	if hasRole(req.Messages, provider.RoleTool) {
		switch lastToolCallName(req.Messages) {
		case "current_time":
			// A turn that keeps working, so there are step boundaries for
			// something said mid-answer to arrive at. If the person said
			// something, the model repeats it back, which is how a spec can tell
			// that the model SAW it rather than that it was merely stored.
			if said := saidMidTurn(req.Messages); said != "" {
				return text("HEARD " + said)
			}
			// Bounded, or this runs until the loop's own limit and the turn ends
			// in "I stopped before finishing" seventy seconds later, having said
			// nothing. Six steps is several seconds at this pace: long enough to
			// type into, short enough to be a test.
			if countToolResults(req.Messages) >= 6 {
				return text("Nothing new to report.")
			}
			return []provider.StreamEvent{
				toolCall(fmt.Sprintf("call_e2e_tick_%d", countToolResults(req.Messages)), "current_time", map[string]any{}),
			}
		case "background_status":
			return text("Your background task is still running; I'll report when it's done.")
		case "http_request":
			// The recovery after a hallucinated call: the loop handed the model a
			// "no such tool" note, and it answers in words instead. A distinctive
			// token so the spec can assert the recovery, not a card, is what shows.
			return text("I can't do that directly. GHOST-RECOVERED")
		case "brain_write":
			// The knowledge write ran (the person approved its card).
			return text("Saved to the knowledge base. " + brainSavedMarker)
		case "memory":
			// The memory write ran, with no card: the agent's own note-taking.
			return text("Noted for next time. " + memorySavedMarker)
		}
		return text("Started in the background. I'll let you know when it's done.")
	}

	prompt := strings.ToLower(lastUserContent(req.Messages))
	switch {
	case strings.Contains(prompt, "ghost"):
		// The model asks for a tool it was never given (the Gateway holds no
		// http_request). The loop must treat it as a hallucination: no card, no
		// failed tool row, a "no such tool" fed back, and the model recovers above.
		return []provider.StreamEvent{
			toolCall("call_e2e_ghost", "http_request", map[string]any{"url": "https://example.com"}),
		}
	case strings.Contains(prompt, "keep checking"):
		// Works for a while, one step at a time, until it is told something.
		return []provider.StreamEvent{toolCall("call_e2e_tick_0", "current_time", map[string]any{})}
	case strings.Contains(prompt, "talk for a long time"):
		// An answer long enough and slow enough to SCROLL INSIDE while it is
		// still arriving, which is the state a person reported and no fixture
		// could reach: "think out loud" runs 60 events at 40ms, so it is over in
		// 2.4 seconds, and half of that is reasoning before a line of the answer
		// exists. Every attempt to scroll a live stream either found nothing on
		// the page yet or found the answer already finished.
		//
		// No reasoning phase, for the same reason: the window has to open
		// immediately, not after the longer half has gone by.
		return talkingAtLength()
	case strings.Contains(prompt, "think out loud"):
		// A long answer that arrives a piece at a time, thinking first and then
		// speaking, so a spec can watch whether the conversation FOLLOWS an
		// answer as it grows. It is the one thing a virtual list does not do on
		// its own: an answer arrives by growing the last row rather than adding
		// one, and a list follows rows, not rows getting taller. Reasoning is
		// half of it and the longer half, since while the model is thinking
		// there is no content at all.
		return thinkingAloud()
	case strings.Contains(prompt, "hello"), strings.Contains(prompt, "hi "), prompt == "hi":
		// A plain greeting, so a spec can create the conversation (and reveal the
		// per-conversation approval toggle) without starting a delegation.
		return text("Hi! Ask me to run a background job.")
	case strings.Contains(prompt, "status"):
		return []provider.StreamEvent{toolCall("call_e2e_check", "background_status", map[string]any{})}
	case strings.Contains(prompt, "knowledge"):
		// A write to a shared knowledge base: brain_write parks on a real card,
		// because it is in the Gateway's confirm set.
		return []provider.StreamEvent{
			toolCall("call_e2e_brain", "brain_write", map[string]any{
				"operation": "save_document", "brain": KnowledgeBrain, "category": KnowledgeCategory,
				"title": "Launch decision", "content": "We ship on Friday.",
			}),
		}
	case strings.Contains(prompt, "remember"):
		// A write to the agent's OWN memory brain: the memory tool is internal and
		// never parks, so this runs to completion with no card.
		return []provider.StreamEvent{
			toolCall("call_e2e_memory", "memory", map[string]any{
				"operation": "save", "category": "Procedures",
				"title": "E2E procedure", "content": "Do the thing next time.",
			}),
		}
	case strings.Contains(prompt, "fleet that lingers"):
		// A batch whose members take a moment, so its one chip can be looked at
		// while the batch is running rather than only once it is over. Ahead of
		// the plain "fleet" case, which this also matches.
		return append(
			text("Starting a batch."),
			toolCall("call_e2e_fleet_slow", "delegate_fleet", fleetTasksWith(QuietAgentKey, " "+lingerMarker)),
		)
	case strings.Contains(prompt, "inline and then"):
		// A delegation ALONGSIDE another call, in one step. Nothing stops a
		// model doing this, and it is the case that left the sibling call
		// running for ever: the loop leaves at the handoff and never comes back
		// to what followed.
		return append(
			text("Asking the specialist."),
			toolCall("call_e2e_inline_first", "delegate",
				delegateInline("what time is it", model.HandoffTerminal)),
			toolCall("call_e2e_inline_after", "current_time", map[string]any{}),
		)
	case strings.Contains(prompt, "inline continue"):
		// The compositional case: the agent answers, the result comes back as a
		// tool result, and the GATEWAY writes the final word.
		return append(
			text("Asking the specialist."),
			toolCall("call_e2e_inline_cont", "delegate",
				delegateInline("what time is it", model.HandoffContinue)),
		)
	case strings.Contains(prompt, "inline"):
		// A synchronous handoff: the agent's answer IS the turn's answer.
		return append(
			text("Handing this over."),
			toolCall("call_e2e_inline", "delegate",
				delegateInline("what time is it", model.HandoffTerminal)),
		)
	case strings.Contains(prompt, "fleet that asks"):
		// The batch whose members stop for approval: the gated agent.
		return append(
			text("Starting a batch."),
			toolCall("call_e2e_fleet_ask", "delegate_fleet", fleetTasks(AgentKey)),
		)
	case strings.Contains(prompt, "fleet"):
		// A batch, through the tool that takes a list. It is the fleet tool and
		// not `delegate` with a mode, because that is what the Gateway is given:
		// a schema whose whole argument is the list.
		return append(
			text("Starting a batch."),
			toolCall("call_e2e_fleet", "delegate_fleet", fleetTasks(QuietAgentKey)),
		)
	case strings.Contains(prompt, "slow"):
		// Two background agents that take a moment, so their chips can be looked
		// at while they are working rather than only once they are over. Ahead of
		// the "two" case, which this prompt also matches.
		return append(
			text("Sure, I'll run two in the background."),
			toolCall("call_e2e_slow_a", "delegate", delegate("first task "+lingerMarker)),
			toolCall("call_e2e_slow_b", "delegate", delegate("second task "+lingerMarker)),
		)
	case strings.Contains(prompt, "two"):
		return append(
			text("Sure, I'll run two in the background."),
			toolCall("call_e2e_agent_a", "delegate", delegate("first task")),
			toolCall("call_e2e_agent_b", "delegate", delegate("second task")),
		)
	case strings.Contains(prompt, "four"):
		// Four background agents at once, so several completion turns finish
		// close together: the case where a result message can fail to paint (KB/29).
		return append(
			text("Kicked off four background jobs."),
			toolCall("call_e2e_sub_a", "delegate", delegate("first task")),
			toolCall("call_e2e_sub_b", "delegate", delegate("second task")),
			toolCall("call_e2e_sub_c", "delegate", delegate("third task")),
			toolCall("call_e2e_sub_d", "delegate", delegate("fourth task")),
		)
	default:
		return append(
			text("Sure, I'll run that in the background."),
			toolCall("call_e2e_agent", "delegate", delegate("check the current time")),
		)
	}
}

// agent calls its one approval-gated tool, then answers terminally once it
// has the result. Its terminal answer becomes the delegation result.
func (p *ScriptedProvider) agent(req provider.GenerateRequest) []provider.StreamEvent {
	if hasRole(req.Messages, provider.RoleTool) {
		return text("Done. " + scriptedResult)
	}
	// A unique call id per agent (real models emit unique ids): two
	// agents running the same tool at once must not collide on the tool-call
	// row or the card id, or the second is lost.
	return append(
		text("Checking the time."),
		toolCall("call_e2e_tool_"+slug(lastUserContent(req.Messages)), AgentTool, map[string]any{}),
	)
}

// slug makes a short id-safe suffix from a task string, so each agent's
// tool call is distinct.
func slug(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		case r == ' ' || r == '-' || r == '_':
			b.WriteByte('_')
		}
		if b.Len() >= 24 {
			break
		}
	}
	if b.Len() == 0 {
		return "task"
	}
	return b.String()
}

func delegate(task string) map[string]any {
	return map[string]any{"agent": AgentKey, "task": task, "mode": "background"}
}

// delegateInline hands off SYNCHRONOUSLY: the agent runs inside this turn and
// its answer ends it, rather than the Gateway saying "started" and finishing.
//
// Every other scripted delegation here is background, which is how the inline
// path came to have no browser coverage at all while background and fleet had
// plenty, and how a defect in it survived from 27 July. The mode is the whole
// difference: `terminal` is what makes the loop leave in the middle of a step.
func delegateInline(task string, mode string) map[string]any {
	return map[string]any{"agent": QuietAgentKey, "task": task, "mode": mode}
}

// thinkingAloud is a reasoning run and then an answer, both long enough to fill
// several screens and both delivered in pieces.
func thinkingAloud() []provider.StreamEvent {
	out := []provider.StreamEvent{}
	for i := 0; i < 30; i++ {
		out = append(out, provider.StreamEvent{
			Kind:           provider.EventReasoningDelta,
			ReasoningDelta: fmt.Sprintf("Working through part %d of the problem, which takes a paragraph to say properly.\n\n", i),
		})
	}
	for i := 0; i < 30; i++ {
		out = append(out, provider.StreamEvent{
			Kind: provider.EventContentDelta,
			// Long enough that the answer is several screens deep, which is the
			// whole point of it: a spec that scrolls up inside a streaming answer
			// cannot say anything if the answer fits on one screen. It did not,
			// and the spec sat pinned against the top of the content reporting
			// that nothing moved.
			ContentDelta: fmt.Sprintf(
				"Point %d, stated at the length a real answer runs to, which is several lines "+
					"rather than one, because an answer that fits on a line is not an answer "+
					"anybody scrolls through. It goes on for long enough to wrap three or four "+
					"times at any sensible width, and then says something else after that.\n\n", i),
		})
	}
	return out
}

// talkingAtLength is many screens of answer, delivered slowly enough that a spec
// can scroll up inside it and then sit still for seconds with the stream still
// running.
func talkingAtLength() []provider.StreamEvent {
	out := make([]provider.StreamEvent, 0, 120)
	for i := 0; i < 120; i++ {
		out = append(out, provider.StreamEvent{
			Kind: provider.EventContentDelta,
			ContentDelta: fmt.Sprintf(
				"Paragraph %d of an answer that goes on, stated at the length a real one runs "+
					"to, which is several lines rather than one. It wraps three or four times at "+
					"any sensible width and then says something else after that.\n\n", i),
		})
	}
	return out
}

func text(s string) []provider.StreamEvent {
	return []provider.StreamEvent{{Kind: provider.EventContentDelta, ContentDelta: s}}
}

func toolCall(id, name string, args map[string]any) provider.StreamEvent {
	raw, _ := json.Marshal(args)
	return provider.StreamEvent{
		Kind:     provider.EventToolCall,
		ToolCall: &provider.ToolCall{ID: id, Name: name, Args: raw},
	}
}

func hasTool(tools []provider.ToolDef, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func hasRole(messages []provider.Message, role provider.Role) bool {
	for _, m := range messages {
		if m.Role == role {
			return true
		}
	}
	return false
}

// lastUserContent is the most recent thing the person actually typed, which is
// what the Gateway's fresh-turn keywords read.
func lastUserContent(messages []provider.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == provider.RoleUser {
			return messages[i].Content
		}
	}
	return ""
}

// lastToolCallName is the name of the tool the most recent assistant message
// invoked, so a continuation turn knows which tool it is reporting on.
func lastToolCallName(messages []provider.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == provider.RoleAssistant && len(messages[i].ToolCalls) > 0 {
			return messages[i].ToolCalls[len(messages[i].ToolCalls)-1].Name
		}
	}
	return ""
}

// mentions reports whether any message content contains the substring.
func mentions(messages []provider.Message, sub string) bool {
	for _, m := range messages {
		if strings.Contains(m.Content, sub) {
			return true
		}
	}
	return false
}

// isCompletion reports whether this is a background-completion turn, which the
// runtime frames to the Gateway with a bracketed system marker (KB/27). It keys off
// the real marker constant so the harness cannot drift from the production text.
func isCompletion(messages []provider.Message) bool {
	for _, m := range messages {
		if strings.Contains(m.Content, agent.CompletionWakeMarker) {
			return true
		}
	}
	return false
}

// fleetTasks is the batch, all to one agent, each with its own task so the
// members are told apart by what they were asked rather than by luck.
func fleetTasks(agentKey string) map[string]any { return fleetTasksWith(agentKey, "") }

// fleetTasksWith is the same batch with something appended to every task, which
// is how one is asked to take a moment (lingerMarker) without slowing the specs
// that only care about the batch.
func fleetTasksWith(agentKey, suffix string) map[string]any {
	tasks := make([]map[string]any, FleetSize)
	for i := range tasks {
		tasks[i] = map[string]any{"agent": agentKey, "task": "look up part " + strconv.Itoa(i+1) + suffix}
	}
	return map[string]any{"tasks": tasks}
}

// isFleetCompletion reports whether a whole batch just finished. Its own marker,
// because a fleet wakes the Gateway once with every result rather than once per
// agent, and a harness that could not tell them apart would pass while the two
// were wired to the same place.
func isFleetCompletion(messages []provider.Message) bool {
	for _, m := range messages {
		if strings.Contains(m.Content, agent.FleetWakeMarker) {
			return true
		}
	}
	return false
}

// resultToken echoes the agent's result back out of the completion message
// so the narration carries it, proving the result actually reached the Gateway.
func resultToken(messages []provider.Message) string {
	for _, m := range messages {
		if strings.Contains(m.Content, scriptedResult) {
			return scriptedResult
		}
	}
	return "(no result)"
}

// Media: the scripted provider answers as a vendor that can do both, so the E2E
// surfaces are exercised rather than hidden behind a capability check.
func (*ScriptedProvider) Media() provider.Media {
	return provider.Media{ReadsFiles: true, Transcribes: true}
}

// Transcribe answers with a fixed line, so a recording flow can be driven
// without a microphone or a vendor.
func (*ScriptedProvider) Transcribe(context.Context, string, string, []byte) (string, error) {
	return "this is a scripted transcription", nil
}

// saidMidTurn is anything the person said AFTER the message that started this
// turn, which is what the loop folds in at a step boundary.
func saidMidTurn(messages []provider.Message) string {
	seen := 0
	for _, m := range messages {
		if m.Role != provider.RoleUser {
			continue
		}
		seen++
		if seen > 1 {
			return m.Content
		}
	}
	return ""
}

// countToolResults keeps each tool call in a turn distinctly named.
func countToolResults(messages []provider.Message) int {
	n := 0
	for _, m := range messages {
		if m.Role == provider.RoleTool {
			n++
		}
	}
	return n
}
