package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/tool"
)

// What the assistant remembers, rewritten off the turn's path.
//
// The person's answer has already streamed by the time we reconsider memory, so
// this work must never make them wait, and must never break their chat if it
// fails. It is triggered two ways, both landing on the same owned background
// queue: on a cadence (the first turn of a conversation, then every few turns),
// and when the model itself decides a turn is worth remembering. The heavy part,
// reading the conversation and asking a model to distill it, happens in a
// background worker, never in the loop and never on the stream.
//
// This mirrors the reference CRM's background memory command: a one-shot
// summarization that merges what was already remembered with what just
// happened, kept short, and left unchanged when the conversation was trivial.
//
// The queue is an in-process, bounded worker today. It is the seam where the
// durable job queue slots in later: callers only ever Enqueue, so moving the
// work to a worker node is a change here, not at any call site.

// memoryCadence is how often the cadence trigger fires after the first turn:
// every memoryCadence-th user turn. Frequent enough to keep memory fresh,
// rare enough that most turns cost nothing extra.
const memoryCadence = 5

// distillTimeout bounds one background distillation. Like a title, a memory
// update is a nicety, and a nicety does not hold a goroutine open forever.
const distillTimeout = 60 * time.Second

// recentStepsForMemory is how many of the conversation's most recent steps the
// distiller reads. Memory is about what the person is working on lately, not
// the whole history, and a shorter window is a cheaper, sharper summary.
const recentStepsForMemory = 12

// memoryQueueBuffer bounds the pending work. A full queue drops the oldest
// intent rather than stalling a turn: a missed memory update is cosmetic, a
// stalled turn is not (the same drop-slow rule as the socket hub and the run
// fan-out).
const memoryQueueBuffer = 256

// memoryScope is which of the two memories a distillation should reconsider.
type memoryScope string

const (
	memoryScopeUser      memoryScope = "user"
	memoryScopeWorkspace memoryScope = "workspace"
)

// distillRequest is one unit of "reconsider what we remember". It carries the
// model that answered the turn, so the background work resolves the same model
// through the same gateway the conversation used, never a special path.
type distillRequest struct {
	WorkspaceID int64
	UserID      int64
	SessionID   int64
	ModelID     int64
	Scopes      []memoryScope
	// Hint is what the model asked to remember, when the model itself flagged
	// the turn. Empty on a cadence-triggered update, where the distiller decides
	// for itself what, if anything, is worth keeping.
	Hint string
	// Tools is the list the note is grounded in: the tools THIS agent actually
	// held on the turn (its loadout), not every tool the workspace has. Grounding
	// on the workspace inventory taught the note to reach for a tool the agent
	// was never given (a tool-less assistant "remembered" it could query a CRM).
	Tools string
}

// memoryFlag collects, within a single turn, whether the model asked to
// remember something and what it named. The loop populates it when it sees the
// memory tool; scheduleMemory reads it once the turn is answered. It carries no
// content of its own beyond the model's hint: the note is distilled from the
// conversation, not written by hand.
type memoryFlag struct {
	raised bool
	scopes []memoryScope
	hint   string
}

// raise records a call to the memory tool. Its scope argument, when given,
// narrows what the off-cadence distillation reconsiders; "person" and
// "workspace" are the model's words for the two memories.
func (f *memoryFlag) raise(scope, note string) {
	f.raised = true
	switch scope {
	case "person":
		f.scopes = appendScope(f.scopes, memoryScopeUser)
	case "workspace":
		f.scopes = appendScope(f.scopes, memoryScopeWorkspace)
	}
	if note = strings.TrimSpace(note); note != "" {
		if f.hint != "" {
			f.hint += "\n"
		}
		f.hint += note
	}
}

// flagToolMistake turns a bad-arguments tool failure into a workspace memory
// event: the assistant just called a tool wrong, and the correct way to call it
// is knowable (its description and, when it has one, its deep guide). The hint
// hands the distiller the tool, the mistake, and the correct usage, so the
// working note it writes is a concrete "how to use this tool right" lesson that
// the next turn reads back and follows. It is always workspace-scoped: the
// lesson is about the work, not the person.
func flagToolMistake(flag *memoryFlag, schema tool.Schema, result tool.Result) {
	if flag == nil {
		return
	}
	name := schema.FriendlyName
	if name == "" {
		name = schema.Name
	}

	var b strings.Builder
	fmt.Fprintf(&b, "While using the %q ability you called it incorrectly and it failed. ", name)
	if reason := errorReason(result.Content); reason != "" {
		fmt.Fprintf(&b, "The error was: %s. ", reason)
	}
	if desc := strings.TrimSpace(schema.Description); desc != "" {
		fmt.Fprintf(&b, "What it does and how to call it: %s ", desc)
	}
	if len(schema.Guide) > 0 {
		fmt.Fprintf(&b, "Its full usage guide, with the correct parameters and examples, is: %s ", string(schema.Guide))
	}
	b.WriteString("Record a short working note on the correct way to use this ability so this mistake is not repeated.")

	flag.raise("workspace", b.String())
}

// errorReason pulls the model-safe error text out of a failed tool result,
// which toolkit shapes as {"success":false,"error":"..."}.
func errorReason(content []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(content, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Error)
}

// noteMemoryFlag reads a memory-tool call's arguments into the flag. Malformed
// arguments still raise the flag with no scope or hint: the model's intent to
// remember is clear even if it botched the shape, and a cadence-style sweep of
// both memories is the safe reading.
func noteMemoryFlag(flag *memoryFlag, args []byte) {
	var parsed struct {
		Scope string `json:"scope"`
		Note  string `json:"note"`
	}
	_ = json.Unmarshal(args, &parsed)
	flag.raise(parsed.Scope, parsed.Note)
}

func appendScope(scopes []memoryScope, s memoryScope) []memoryScope {
	for _, existing := range scopes {
		if existing == s {
			return scopes
		}
	}
	return append(scopes, s)
}

// memoryQueue is the owned, bounded background worker. It holds the pending
// intents and drains them one at a time; the distillation itself is a callback,
// so the queue knows nothing about models or the store.
type memoryQueue struct {
	reqs chan distillRequest
	run  func(ctx context.Context, req distillRequest)
	log  zerolog.Logger
}

func newMemoryQueue(run func(context.Context, distillRequest), log zerolog.Logger) *memoryQueue {
	return &memoryQueue{
		reqs: make(chan distillRequest, memoryQueueBuffer),
		run:  run,
		log:  log,
	}
}

// enqueue schedules a distillation. It never blocks: a full queue drops the
// request and says so, because scheduling a memory update must not be able to
// stall the turn that scheduled it.
func (q *memoryQueue) enqueue(req distillRequest) {
	select {
	case q.reqs <- req:
	default:
		q.log.Warn().Int64("session_id", req.SessionID).
			Msg("memory queue full; dropping a memory update")
	}
}

// run drains the queue until ctx is cancelled. One worker is enough:
// distillation is not latency-sensitive, and serializing it keeps the number of
// concurrent one-shot model calls bounded without any extra machinery. Each job
// gets its OWN context with a fresh timeout, because the queue's ctx is the
// process lifetime, not a request's.
func (q *memoryQueue) drain(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-q.reqs:
			jobCtx, cancel := context.WithTimeout(context.Background(), distillTimeout)
			q.run(jobCtx, req)
			cancel()
		}
	}
}

// RunMemory drains the background memory queue until ctx is cancelled. The
// server owns it (one goroutine, started alongside the socket hub); the worker,
// which serves no turns, never starts it.
func (r *Runner) RunMemory(ctx context.Context) {
	r.memory.drain(ctx)
}

// scheduleMemory decides, once a turn is answered, whether to reconsider memory
// and enqueues the background work if so. It is the only place the two triggers
// meet: the cadence, and whatever the model flagged during the turn.
//
// Nothing here does the work; it only schedules it. A conversation with no model
// (a resumed turn that failed to resolve) is skipped, because the background
// work would have nothing to run on.
func (r *Runner) scheduleMemory(turn Turn, userTurns int, flag *memoryFlag) {
	if turn.ModelID == 0 {
		return
	}

	onCadence := userTurns == 1 || (memoryCadence > 0 && userTurns > 0 && userTurns%memoryCadence == 0)
	flagged := flag != nil && flag.raised

	if !onCadence && !flagged {
		return
	}

	// A cadence turn sweeps both memories; an off-cadence flag reconsiders only
	// the scope(s) it named. Either way, a hint the model or a tool mistake left
	// is carried through: dropping it on a turn that also happened to be a
	// cadence turn would lose the very lesson worth remembering.
	scopes := []memoryScope{memoryScopeUser, memoryScopeWorkspace}
	if !onCadence && flagged && len(flag.scopes) > 0 {
		scopes = flag.scopes
	}
	hint := ""
	if flagged {
		hint = flag.hint
	}

	r.memory.enqueue(distillRequest{
		WorkspaceID: turn.WorkspaceID,
		UserID:      turn.UserID,
		SessionID:   turn.SessionID,
		ModelID:     turn.ModelID,
		Scopes:      scopes,
		Hint:        hint,
		// The tools THIS agent actually held, captured at the source, so the note
		// is grounded in what the assistant can really do, not the workspace's
		// whole toolbox.
		Tools: formatLoadoutTools(turn.Tools),
	})
}

// countUserTurns counts the person's own turns in a rebuilt transcript. One
// user message is one turn, so this is an exact ordinal for the cadence, taken
// from the messages the loop already assembled.
func countUserTurns(messages []provider.Message) int {
	n := 0
	for _, m := range messages {
		if m.Role == provider.RoleUser {
			n++
		}
	}
	return n
}

// distill is the background work: for each scope, read the recent conversation
// and the memory as it stands, ask the model for an updated note, and store it.
// It is best-effort by contract. Every failure is logged and swallowed, because
// a memory update that goes wrong must never surface to the person whose chat
// triggered it.
func (r *Runner) distill(ctx context.Context, req distillRequest) {
	steps, err := r.store.Agent().Transcript(ctx, req.SessionID)
	if err != nil {
		r.log.Warn().Err(err).Int64("session_id", req.SessionID).Msg("memory: load transcript")
		return
	}
	summary := recentConversation(steps, recentStepsForMemory)
	if summary == "" {
		return
	}

	resolved, err := r.gateway.Resolve(ctx, req.WorkspaceID, req.ModelID)
	if err != nil {
		r.log.Warn().Err(err).Int64("session_id", req.SessionID).Msg("memory: resolve model")
		return
	}

	// The agent's OWN tools (captured on the turn) ground every scope's note in
	// what THIS assistant can do, never the workspace's whole toolbox.
	for _, scope := range req.Scopes {
		if err := r.distillScope(ctx, req, scope, resolved, summary, req.Tools); err != nil {
			r.log.Warn().Err(err).Int64("session_id", req.SessionID).
				Str("scope", string(scope)).Msg("memory: distill")
		}
	}
}

func (r *Runner) distillScope(ctx context.Context, req distillRequest, scope memoryScope, resolved *provider.Resolved, summary, tools string) error {
	existing, err := r.readMemory(ctx, req, scope)
	if err != nil {
		return fmt.Errorf("read memory: %w", err)
	}

	updated, err := r.summarizeMemory(ctx, resolved, req, scope, existing, summary, tools)
	if err != nil {
		return err
	}
	updated = strings.TrimSpace(updated)
	if updated == "" || updated == strings.TrimSpace(existing) {
		// The model judged there was nothing new worth keeping. Leaving the note
		// exactly as it was is a correct outcome, not a failure.
		return nil
	}
	updated = capMemory(updated)

	return r.writeMemory(ctx, req, scope, updated)
}

func (r *Runner) readMemory(ctx context.Context, req distillRequest, scope memoryScope) (string, error) {
	switch scope {
	case memoryScopeUser:
		return r.store.Memory().UserMemory(ctx, req.WorkspaceID, req.UserID)
	case memoryScopeWorkspace:
		return r.store.Memory().WorkspaceMemory(ctx, req.WorkspaceID)
	default:
		return "", fmt.Errorf("unknown memory scope %q", scope)
	}
}

func (r *Runner) writeMemory(ctx context.Context, req distillRequest, scope memoryScope, text string) error {
	switch scope {
	case memoryScopeUser:
		return r.store.Memory().SetUserMemory(ctx, req.WorkspaceID, req.UserID, text)
	case memoryScopeWorkspace:
		return r.store.Memory().SetWorkspaceMemory(ctx, req.WorkspaceID, text)
	default:
		return fmt.Errorf("unknown memory scope %q", scope)
	}
}

// summarizeMemory runs the one-shot distillation call. It goes through the same
// gateway as the conversation, with reasoning off and a tight token budget: this
// is a rewrite of a short note, not a fresh answer.
func (r *Runner) summarizeMemory(ctx context.Context, resolved *provider.Resolved, req distillRequest, scope memoryScope, existing, conversation, tools string) (string, error) {
	started := time.Now()
	system := memorySystemPrompt(scope)
	task := memoryTask(existing, conversation, req.Hint, tools)

	prepared := resolved.Prepare(provider.GenerateRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: system},
			{Role: provider.RoleUser, Content: task},
		},
		MaxTokens: memoryMaxTokens,
		Reasoning: false,
	})

	resp, err := resolved.Provider.Generate(ctx, prepared)
	if err != nil {
		return "", err
	}
	r.recordModelCall(ctx, Turn{
		WorkspaceID: req.WorkspaceID,
		SessionID:   req.SessionID,
		ModelID:     req.ModelID,
	}, resolved, started, resp.Usage)

	return resp.Message.Content, nil
}

// memoryMaxTokens caps a memory distillation call. It is the TOTAL budget, which
// on a reasoning model (OpenAI's o-series, gpt-5) must also cover the thinking it
// does before writing anything, or a tight cap returns an empty note. The stored
// note stays small regardless: maxMemoryChars is the hard ceiling on what is kept.
const memoryMaxTokens = 2000

// maxMemoryChars is the hard ceiling on a stored note, a backstop for a model
// that ignored the instruction to keep it compact. The instruction does the
// real work; this only stops a runaway note from ever reaching the prompt.
const maxMemoryChars = 2000

func capMemory(s string) string {
	return truncate(s, maxMemoryChars)
}

// memorySystemPrompt is the distiller's instruction, in the same anti-leak,
// business-language spirit as the assistant's own communication rules. The two
// scopes want different notes: one about the person, one about doing the work.
func memorySystemPrompt(scope memoryScope) string {
	common := "Output ONLY the updated note, with no preamble, no explanation, and no code fences. " +
		"Merge what is worth keeping from the existing note with anything genuinely new from the conversation. " +
		"If the conversation was trivial (a greeting, a one-off lookup) and adds nothing lasting, reply with the existing note unchanged. " +
		"Keep the whole note compact: a handful of short bullet points, well under 120 words. It is read on EVERY future turn, so " +
		"space is precious, actively maintain it rather than letting it grow. When adding something would push it past that budget, keep " +
		"only the most useful and durable items and drop the least, and always prune anything stale, redundant, superseded, or one-off. " +
		"Never include secrets, passwords, tokens, or raw identifiers. Be concrete. " +
		"Ground the note in what exists: the assistant's real tools are listed in the task under \"Tools that exist\". " +
		"Reference a tool only by a name that appears there. Never invent, and always prune, any mention of a tool, MCP, " +
		"command, or capability that is not listed, it no longer exists. If that list is empty, do not name specific tools at all. " +
		"NEVER write about what the assistant CANNOT do, what tools or capabilities it LACKS, or any instruction to avoid or stop attempting something. " +
		"You are shown only this agent's own tools, not the agents it can hand work to, so you cannot know the full set of what is possible, " +
		"and what the assistant can do is decided fresh every turn from its real tools, not from this note. A person declining an action once is their " +
		"choice in that moment, never a missing capability, so never turn a refusal into 'do not attempt X' or 'we have no X'. A mistake worth remembering " +
		"is a wrong WAY of doing something (wrong arguments, wrong order), never a claim that a capability is absent."

	switch scope {
	case memoryScopeUser:
		return "You maintain a concise profile of a person, so an assistant can help them better next time. " +
			"Capture who they are, what they work on, and how they like their answers, written in the third person (\"Prefers...\", \"Works on...\"). " +
			common
	case memoryScopeWorkspace:
		return "You maintain an assistant's own working notes for a workspace: what it has learned about doing the work well here. " +
			"Capture the order to do things in, steps that are easy to get wrong, and mistakes not to repeat, so the assistant improves over time. " +
			"This is not about any one person; it is about the work. " + common
	default:
		return common
	}
}

// memoryTask lays out what the distiller is working from: the note as it stands,
// the recent conversation, and, when the model flagged the turn, what it asked
// to remember.
func memoryTask(existing, conversation, hint, tools string) string {
	var b strings.Builder
	if e := strings.TrimSpace(existing); e != "" {
		b.WriteString("## Existing note\n")
		b.WriteString(e)
		b.WriteString("\n\n")
	}
	// The real tools ground the note: the distiller keeps references only to
	// these and prunes any that name something gone (a replaced MCP, say).
	b.WriteString("## Tools that exist\n")
	if t := strings.TrimSpace(tools); t != "" {
		b.WriteString(t)
	} else {
		b.WriteString("(none)")
	}
	b.WriteString("\n\n")
	if h := strings.TrimSpace(hint); h != "" {
		b.WriteString("## What the assistant asked to remember\n")
		b.WriteString(h)
		b.WriteString("\n\n")
	}
	b.WriteString("## Recent conversation\n")
	b.WriteString(conversation)
	b.WriteString("\n\n---\nProduce the updated note now, or the existing note unchanged if nothing lasting was learned.")
	return b.String()
}

// formatLoadoutTools renders the tools an agent actually held on the turn (its
// loadout, internal infrastructure excluded) as a compact list for the distiller
// to ground the note in. It is the AGENT's own tools, never the whole
// workspace's: a note must not learn to reach for a tool this assistant was
// never given (a tool-less agent kept "remembering" it could query a CRM,
// because the note was grounded in the workspace inventory, not its loadout).
// An empty loadout yields an empty string, and the distiller prompt then forbids
// naming any tool and prunes the ones already there.
func formatLoadoutTools(loadout tool.Loadout) string {
	var b strings.Builder
	for _, s := range loadout.Schemas {
		if s.Kind == tool.KindInternal {
			continue
		}
		b.WriteString("- ")
		b.WriteString(s.Name)
		if s.FriendlyName != "" {
			b.WriteString(" (")
			b.WriteString(s.FriendlyName)
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// recentConversation renders the last n user and assistant steps into a compact
// summary for the distiller: only what was said, truncated, most recent last.
// Reasoning and tool mechanics are left out; memory is about the exchange, not
// how it was carried out.
func recentConversation(steps []*model.AgentStep, n int) string {
	var lines []string
	for _, step := range steps {
		if step.Partial {
			continue
		}
		var role string
		switch step.Kind {
		case model.StepUser:
			role = "User"
		case model.StepAssistant:
			role = "Assistant"
		default:
			continue
		}
		text := strings.TrimSpace(step.Text)
		if text == "" {
			continue
		}
		lines = append(lines, role+": "+truncate(text, 500))
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n\n")
}
