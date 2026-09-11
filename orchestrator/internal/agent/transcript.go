package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/tool"
)

// checkpointInterval is how often an answer still being streamed is written
// down. A process that dies mid-sentence then loses at most this much, instead
// of losing the whole turn.
//
// It is a compromise, and it is deliberately a short one: two seconds of a
// vendor's output is a paragraph, and a paragraph is worth a write.
const checkpointInterval = 2 * time.Second

// --- reading -------------------------------------------------------------------

// notExecuted is the result given to a call that never ran: a turn that was
// abandoned while a tool was still pending. It exists because every vendor
// validates that each tool call has a result, and an unanswered call would make
// the whole conversation unusable from that point on. Saying "this did not run"
// is both true and recoverable.
var notExecuted = json.RawMessage(`{"success":false,"error":"This action was not carried out."}`)

// conversation turns stored steps into the messages a model is shown.
//
// The role=tool message is SYNTHESIZED here, from the call that produced the
// result. It is not stored as a message, because a tool result is not something
// somebody said: it belongs to the call, and storing it twice would mean
// storing two copies that nothing keeps in agreement.
//
// skip names a tool call whose result must be left out, which is the call a
// resumed turn is about to run. Every other unresolved call gets the
// not-executed result, so the pairing the vendors demand always holds.
func conversation(systemPrompt string, steps []*model.AgentStep, skip string) []provider.Message {
	messages := make([]provider.Message, 0, len(steps)+1)
	if systemPrompt != "" {
		// The system prompt comes from the agent configuration, never from
		// history: changing an agent must change how it behaves on the next
		// turn, not replay what it used to be.
		messages = append(messages, provider.Message{
			Role: provider.RoleSystem, Content: systemPrompt,
		})
	}

	for _, step := range steps {
		if step.Kind == model.StepUser {
			messages = append(messages, provider.Message{
				Role: provider.RoleUser, Content: step.Text,
			})
			continue
		}

		assistant := provider.Message{
			Role:      provider.RoleAssistant,
			Content:   step.Text,
			Reasoning: step.Reasoning,
		}
		for _, call := range step.ToolCalls {
			assistant.ToolCalls = append(assistant.ToolCalls, provider.ToolCall{
				ID: call.ToolCallID, Name: call.ToolName, Args: call.Args,
			})
		}
		if assistant.Content == "" && len(assistant.ToolCalls) == 0 {
			// A step that produced nothing at all is not a message. It can only
			// be the wreckage of an interrupted turn, and the model must not be
			// shown an empty assistant turn.
			continue
		}
		messages = append(messages, assistant)

		for _, call := range step.ToolCalls {
			if call.ToolCallID == skip {
				continue
			}
			result := call.Result
			if !call.Resolved() || len(result) == 0 {
				result = notExecuted
			}
			messages = append(messages, provider.Message{
				Role:       provider.RoleTool,
				ToolCallID: call.ToolCallID,
				Content:    string(result),
			})
		}
	}
	return messages
}

// --- writing -------------------------------------------------------------------

// stepBuffer accumulates one model call as it streams.
//
// The checkpoint reads it on a timer while the stream writes it, so it is
// guarded. It is the only shared state in the loop.
type stepBuffer struct {
	mu        sync.Mutex
	content   string
	reasoning string
	toolCalls []provider.ToolCall
}

func (b *stepBuffer) addContent(delta string) {
	b.mu.Lock()
	b.content += delta
	b.mu.Unlock()
}

func (b *stepBuffer) addReasoning(delta string) {
	b.mu.Lock()
	b.reasoning += delta
	b.mu.Unlock()
}

func (b *stepBuffer) addToolCall(call provider.ToolCall) {
	b.mu.Lock()
	b.toolCalls = append(b.toolCalls, call)
	b.mu.Unlock()
}

// spoken is how much the person has already been shown of this answer.
//
// It is what decides whether a broken stream can be tried again. Nothing shown
// means nobody would see the difference; something shown means a retry would say
// it twice, and half an answer followed by a whole one is worse than an honest
// stop.
func (b *stepBuffer) spoken() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.content) + len(b.toolCalls)
}

func (b *stepBuffer) text() (content, reasoning string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.content, b.reasoning
}

func (b *stepBuffer) calls() []provider.ToolCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]provider.ToolCall(nil), b.toolCalls...)
}

// step assembles the durable step from what has streamed so far. partial says
// whether the model is still talking. byName resolves each call's friendly name
// from the tool that will run it, so the row is complete the moment it is
// written, not patched later.
func (b *stepBuffer) step(turn Turn, seq int, resolved *provider.Resolved, byName map[string]tool.Schema, partial bool) *model.AgentStep {
	content, reasoning := b.text()
	step := &model.AgentStep{
		SessionID: turn.SessionID,
		Seq:       seq,
		Kind:      model.StepAssistant,
		Vendor:    resolved.Vendor.VendorKey,
		Model:     resolved.Model.ModelKey,
		Partial:   partial,
		Text:      content,
		Reasoning: reasoning,
	}
	for i, call := range b.calls() {
		// A call made through the connected-services ability is recorded as the
		// tool it runs, with that tool's arguments. The model's own call id is
		// kept, because that is what its next message answers to.
		name, args := call.Name, call.Args
		if wanted, wantedArgs, viaDiscovery := discovered(name, args, byName); viaDiscovery {
			name, args = wanted, wantedArgs
		}
		step.ToolCalls = append(step.ToolCalls, &model.ToolCall{
			SessionID:      turn.SessionID,
			WorkspaceID:    turn.WorkspaceID,
			ToolCallID:     call.ID,
			ToolName:       name,
			FriendlyName:   friendlyName(byName, name),
			Args:           args,
			Status:         model.ToolCallRunning,
			ExecutionOrder: i,
		})
	}
	return step
}

// checkpoint writes the in-flight step every checkpointInterval, and once more
// when the stream ends. Its writes are upserts on (session, seq), so the row is
// refined in place rather than accumulating one row per tick.
//
// The returned stop function ends it and waits, so no write can land after the
// step has been committed and overwrite the finished answer with a stale draft.
func (r *Runner) checkpoint(ctx context.Context, turn Turn, seq int, resolved *provider.Resolved, buf *stepBuffer) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)
		ticker := time.NewTicker(checkpointInterval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				content, reasoning := buf.text()
				if content == "" && reasoning == "" {
					// Nothing to save yet. A step that has produced no words is
					// not worth a row, and writing an empty one would put a
					// blank message in the timeline.
					continue
				}
				// A checkpoint carries no tool calls: they are written when the
				// step commits, because a half-parsed call has no arguments yet
				// and a row without arguments cannot be run or shown.
				step := &model.AgentStep{
					SessionID: turn.SessionID,
					Seq:       seq,
					Kind:      model.StepAssistant,
					Vendor:    resolved.Vendor.VendorKey,
					Model:     resolved.Model.ModelKey,
					Partial:   true,
					Text:      content,
					Reasoning: reasoning,
				}
				if err := r.store.Agent().SaveStep(ctx, step); err != nil {
					// A failed checkpoint is not worth failing the turn over:
					// the answer is still streaming to the user, and the step
					// will be written in full when it commits.
					r.log.Warn().Err(err).Int64("session_id", turn.SessionID).
						Msg("checkpoint failed")
				}
			}
		}
	}()

	// Idempotent: the caller stops the checkpoint explicitly on the paths that
	// then write the interrupted step, and again through a defer. Stopping
	// twice must be harmless, not fatal.
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-finished
		})
	}
}

// saveInterrupted writes down what a turn had produced when it died.
//
// It runs on a context of its own, because the reason the turn died is usually
// that the caller's context was cancelled, and a write on a cancelled context
// writes nothing. The whole point is to keep the words the user already saw.
func (r *Runner) saveInterrupted(turn Turn, seq int, resolved *provider.Resolved, buf *stepBuffer) {
	content, reasoning := buf.text()
	if content == "" && reasoning == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()

	step := buf.step(turn, seq, resolved, nil, true)
	step.ToolCalls = nil // A call that never ran is not a call that was made.
	if err := r.store.Agent().SaveStep(ctx, step); err != nil {
		r.log.Error().Err(err).Int64("session_id", turn.SessionID).
			Msg("could not save the interrupted answer")
	}
}
