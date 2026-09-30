package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/store"
)

// Compacting a conversation: a summary the model reads instead of the steps it
// covers.
//
// A conversation is sent whole on every turn, tool calls and what they answered
// included, and the only thing that ever shortens it is the trim that keeps a
// request under the model's context window. Compacting is the person choosing to
// shorten it: the model that would answer the next turn writes a summary of
// everything so far, and from then on a turn is built from that summary plus the
// steps that came after it. Nothing is deleted. The chat still shows every step,
// because the steps are what happened and the summary is only what the model is
// told about them.

// ErrNothingToCompact is a conversation with nothing said since its last
// summary, or nothing said at all.
var ErrNothingToCompact = errors.New("agent: nothing new to compact")

// CompactRequest is one conversation to summarize.
type CompactRequest struct {
	WorkspaceID int64
	SessionID   int64
	// ModelID is the model that writes the summary. The caller passes the one
	// the conversation's next turn would run on, so the summary is written by
	// the model that is going to read it.
	ModelID int64
	// By is who asked, recorded on the summary.
	By model.Actor
	// ReadAttachments is what a turn uses to fold the account of an attached
	// file into what the person said, so the summary covers what they sent as
	// well as what they typed. Nil leaves attachments out.
	ReadAttachments func(ctx context.Context, workspaceID int64, ids []string) string
}

// Compact writes and keeps a summary of the conversation so far.
func (r *Runner) Compact(ctx context.Context, req CompactRequest) (*model.Compaction, error) {
	resolved, err := r.gateway.Resolve(ctx, req.WorkspaceID, req.ModelID)
	if err != nil {
		return nil, fmt.Errorf("resolve the model: %w", err)
	}
	return r.compact(ctx, req, resolved)
}

func (r *Runner) compact(ctx context.Context, req CompactRequest, resolved *provider.Resolved) (*model.Compaction, error) {
	steps, err := r.store.Agent().Transcript(ctx, req.SessionID)
	if err != nil {
		return nil, fmt.Errorf("load the conversation: %w", err)
	}
	// What the Gateway sees, and only from where its last summary stopped: the
	// new summary is written from that one plus what came after, so it covers
	// everything without anything being read twice.
	previous, steps, err := r.sinceSummary(ctx, req.SessionID, GatewaySteps(steps))
	if err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return nil, ErrNothingToCompact
	}
	r.withAttachments(ctx, Turn{WorkspaceID: req.WorkspaceID, ReadAttachments: req.ReadAttachments}, steps)
	through := steps[len(steps)-1].Seq

	// Everything, whole: what was said, every tool call with its arguments and
	// every answer a tool gave. Which of it matters is for the model reading it
	// to decide, and a length limit applied before anybody has read it decides
	// that blind: a database schema cut at its first 2,000 characters is a
	// summary that never saw most of the columns.
	//
	// What has to fit is each REQUEST, not the conversation. So a conversation
	// longer than the model can read at once is read in parts, each with the
	// summary so far, and each summary replaces the one before, until every part
	// has been read. Nothing is dropped to make it fit.
	budget := provider.BudgetFor(resolved.Model, compactMaxTokens)
	if budget > 0 {
		budget -= provider.RequestChars([]provider.Message{
			{Role: provider.RoleSystem, Content: compactPrompt},
			{Role: provider.RoleUser},
		}, nil)
	}
	turn := Turn{WorkspaceID: req.WorkspaceID, SessionID: req.SessionID}
	summary := previous
	for pending := stepTexts(steps); len(pending) > 0; {
		var part string
		part, pending, err = nextPart(summary, pending, budget)
		if err != nil {
			return nil, err
		}
		if summary, err = r.summarize(ctx, turn, resolved, part); err != nil {
			return nil, err
		}
	}
	c := &model.Compaction{
		SessionID:  req.SessionID,
		ThroughSeq: through,
		Summary:    summary,
		Model:      resolved.Model.ModelKey,
	}
	if resolved.Vendor != nil {
		c.Vendor = resolved.Vendor.VendorKey
	}
	c.Made(req.By)
	if err := r.store.Agent().SaveCompaction(ctx, c); err != nil {
		return nil, fmt.Errorf("keep the summary: %w", err)
	}
	return c, nil
}

// CheckCompactable answers ErrNothingToCompact for a conversation with nothing
// said since its last summary, before anything is started for it. The
// compaction itself asks the same question again, because a conversation can
// change between the two.
func (r *Runner) CheckCompactable(ctx context.Context, sessionID int64) error {
	steps, err := r.store.Agent().Transcript(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load the conversation: %w", err)
	}
	_, after, err := r.sinceSummary(ctx, sessionID, GatewaySteps(steps))
	if err != nil {
		return err
	}
	if len(after) == 0 {
		return ErrNothingToCompact
	}
	return nil
}

// summarize asks the model for a summary of one part, which carries the summary
// so far when there is one, and answers what it wrote.
func (r *Runner) summarize(ctx context.Context, turn Turn, resolved *provider.Resolved, part string) (string, error) {
	started := time.Now()
	prepared := resolved.Prepare(provider.GenerateRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: compactPrompt},
			{Role: provider.RoleUser, Content: part},
		},
		MaxTokens: compactMaxTokens,
		Reasoning: false,
	})
	resp, err := resolved.Provider.Generate(ctx, prepared)
	if err != nil {
		r.recordModelFailure(ctx, turn, resolved, started, err)
		return "", fmt.Errorf("write the summary: %w", err)
	}
	r.recordModelCall(ctx, turn, resolved, started, resp.Usage, prepared)

	summary := strings.TrimSpace(resp.Message.Content)
	if summary == "" {
		// Kept as nothing rather than as an empty summary, which would stand in
		// for the whole conversation and tell the model it had never happened.
		return "", errors.New("agent: the model wrote an empty summary")
	}
	return summary, nil
}

// sinceSummary is a conversation's newest summary and the steps after it. With
// no summary it is no text and every step, which is how a conversation that was
// never compacted reads exactly as it always has.
func (r *Runner) sinceSummary(ctx context.Context, sessionID int64, steps []*model.AgentStep) (string, []*model.AgentStep, error) {
	latest, err := r.store.Agent().LatestCompaction(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return "", steps, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("load the summary: %w", err)
	}
	after := make([]*model.AgentStep, 0, len(steps))
	for _, step := range steps {
		if step.Seq > latest.ThroughSeq {
			after = append(after, step)
		}
	}
	return latest.Summary, after, nil
}

// withSummary puts a summary where the steps it covers would have been: after
// the system prompt and before the first step it did not cover.
//
// As a system message, because the trim keeps every system message wherever it
// sits and drops history oldest first (provider.TrimToBudget). Anywhere else the
// summary would be the oldest history there is, and the first thing a long
// conversation lost.
func withSummary(messages []provider.Message, summary string) []provider.Message {
	if summary == "" {
		return messages
	}
	at := 0
	for at < len(messages) && messages[at].Role == provider.RoleSystem {
		at++
	}
	out := make([]provider.Message, 0, len(messages)+1)
	out = append(out, messages[:at]...)
	out = append(out, provider.Message{Role: provider.RoleSystem, Content: summaryHeading + summary})
	return append(out, messages[at:]...)
}

// summaryHeading is what the model is told the summary is, so it reads it as
// the earlier part of this conversation rather than as an instruction.
const summaryHeading = "The earlier part of this conversation was summarized to save space. " +
	"This summary stands in for it; everything after it is the conversation since, in full.\n\n"

// compactMaxTokens caps the summary call. It is the TOTAL budget, which on a
// reasoning model also has to cover the thinking it does first, so it is not
// the length of the summary: the instruction asks for something much shorter.
const compactMaxTokens = 8000

// compactPrompt is the instruction for writing a summary. What it asks to keep
// is what the next turn needs to carry on without asking again.
const compactPrompt = "You are compacting a long conversation between a person and an assistant, so the assistant can carry on " +
	"in much less space. Write the summary the assistant will read INSTEAD of the conversation below. It is all the " +
	"assistant will know about what happened, so keep everything the rest of the work may need:\n" +
	"- what the person wants, and why, including anything they changed their mind about\n" +
	"- every decision, agreement and instruction, and the person's preferences about how to work\n" +
	"- facts that were established: names, numbers, identifiers, dates, file paths, addresses, settings and values\n" +
	"- what the assistant did, and what came of it, including what a tool answered when it still matters\n" +
	"- what is unfinished, what was promised, and what the person was last asking about\n" +
	"Leave out greetings, repetition, dead ends that no longer matter, and tool output nobody will need again. " +
	"If an earlier summary is included, fold it in: the result replaces it. " +
	"Write in the language the person uses. Output ONLY the summary, with no preamble and no code fences."

// ErrNoRoomToRead is a summary grown so large that the model's window has no
// room left beside it to read the rest of the conversation.
var ErrNoRoomToRead = errors.New("agent: the summary leaves no room in this model's window to read the rest")

// minPartChars is the least of the conversation a part carries beside the
// summary so far. Less than this and reading on would take a call for every
// few lines, so the compaction stops and says why instead.
const minPartChars = 2000

// Where a step too long to read in one part is split, and how each half says so,
// so the model reads one step in two halves and not two unrelated fragments.
const (
	continuesNote = "\n[this continues in the next part]\n\n"
	continuedNote = "[continued from the previous part]\n"
)

// nextPart is the next part of the conversation to read, and what is left after
// it. It carries the summary so far, then as many whole steps as fit in budget
// characters, measured as the request measures them (escaped, the way they are
// sent). A step too long to fit beside the summary on its own is split: as much
// of it as fits now, the rest at the front of what is left. Zero budget is a
// model with no window set, and everything is one part.
func nextPart(summary string, pending []string, budget int) (string, []string, error) {
	head := ""
	if summary != "" {
		head = "Summary of the conversation before this part:\n" + summary + "\n\nThe conversation since:\n\n"
	}
	if budget <= 0 {
		return head + strings.Join(pending, ""), nil, nil
	}
	room := budget - escapedLen(head)
	if room < minPartChars {
		return "", nil, ErrNoRoomToRead
	}

	var b strings.Builder
	used, taken := 0, 0
	for taken < len(pending) && used+escapedLen(pending[taken]) <= room {
		b.WriteString(pending[taken])
		used += escapedLen(pending[taken])
		taken++
	}
	if taken > 0 {
		return head + b.String(), pending[taken:], nil
	}

	// The next step alone is bigger than the room: send the part of it that fits
	// and keep the rest, whole, for the next part.
	step := pending[0]
	fits, cut := escapedLen(continuesNote), 0
	for i, r := range step {
		if fits+escapedRuneLen(r) > room {
			cut = i
			break
		}
		fits += escapedRuneLen(r)
	}
	rest := append([]string{continuedNote + step[cut:]}, pending[1:]...)
	return head + step[:cut] + continuesNote, rest, nil
}

// escapedLen is how long s is once it is in a request: JSON-escaped, the way
// provider.RequestChars counts it.
func escapedLen(s string) int {
	n := 0
	for _, r := range s {
		n += escapedRuneLen(r)
	}
	return n
}

// escapedRuneLen is one character's length once JSON-escaped, as encoding/json
// writes it (measured against it in the test): a two-byte escape for a quote, a
// backslash and the five control characters with a short form, six bytes for
// the other control characters, for what it escapes to be safe inside HTML, and
// for the two line separators, and the character's own UTF-8 length otherwise.
func escapedRuneLen(r rune) int {
	switch {
	case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t' || r == '\b' || r == '\f':
		return 2
	case r < 0x20 || r == '<' || r == '>' || r == '&' || r == 0x2028 || r == 0x2029:
		return 6
	default:
		return utf8.RuneLen(r)
	}
}

// stepTexts is each step written out whole, as a readable account: who said
// what, what was done with which arguments, and what it answered. Thinking is
// left out: it is how an answer was reached, and the answer and what was done
// are already here.
func stepTexts(steps []*model.AgentStep) []string {
	var texts []string
	for _, step := range steps {
		var b strings.Builder
		if text := strings.TrimSpace(step.Text); text != "" {
			who := "Assistant: "
			if step.Kind == model.StepUser {
				who = "Person: "
			}
			b.WriteString(who + text + "\n\n")
		}
		for _, call := range step.ToolCalls {
			name := call.FriendlyName
			if name == "" {
				name = call.ToolName
			}
			b.WriteString("Assistant used " + name + " with " + string(call.Args) + "\n")
			switch {
			case call.Status == model.ToolCallRejected:
				b.WriteString("The person declined it.\n\n")
			case !call.Resolved() || len(call.Result) == 0:
				b.WriteString("It did not run.\n\n")
			default:
				b.WriteString("It answered: " + string(call.Result) + "\n\n")
			}
		}
		if b.Len() > 0 {
			texts = append(texts, b.String())
		}
	}
	return texts
}
