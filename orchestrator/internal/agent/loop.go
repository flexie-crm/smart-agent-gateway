// Package agent runs a turn: it streams the model, executes the tools the
// model asks for, feeds the results back, and repeats until the model
// answers. It is the one place that orchestrates a conversation, and every
// channel (chat, HTTP, MCP) enters through it.
package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/integrations"
)

// maxIterations bounds a single turn. A model that keeps calling tools
// without ever answering is a bug, a loop, or an attack; either way the turn
// must end rather than run forever on the user's money. The bound is per agent
// (model.DefaultMaxIterations when it sets none), read from the turn below.

// maxBackgroundChecks bounds how many times a Gateway may look at its running
// background tasks in one turn before the loop refuses and ends it. A legitimate
// "how is it going" is one look; more than a couple is a polling loop (KB/27).
const maxBackgroundChecks = 2

// heartbeatInterval keeps a long tool call from looking like a dead stream.
const heartbeatInterval = 10 * time.Second

// Runner executes turns.
//
// It holds no tool registry: the turn arrives already carrying the tools it
// may use. Deciding what an agent can do is configuration, and it belongs to
// the layer that resolves the profile (app.ResolveProfile), not to the loop
// that runs it. The runner's job is to run exactly what it was handed.
type Runner struct {
	store   store.Store
	gateway *provider.Gateway
	log     zerolog.Logger
	// memory is the owned background worker that rewrites what the assistant
	// remembers, off the turn's path. The server drains it (RunMemory).
	memory *memoryQueue
	// handoffs is one entry per way of running a delegation. The loop resolves
	// a mode and calls what it finds; it does not know which it has, which is
	// what stops a new mode being a new branch in every path that touches one.
	handoffs map[string]Handoff
}

func NewRunner(st store.Store, gw *provider.Gateway, log zerolog.Logger) *Runner {
	r := &Runner{store: st, gateway: gw, log: log}
	r.memory = newMemoryQueue(r.distill, log)
	// Registered here rather than discovered: what a build can do is a list
	// somebody can read, not something assembled by import side effects.
	r.handoffs = map[string]Handoff{
		model.HandoffContinue:   synchronousHandoff{r: r, mode: model.HandoffContinue},
		model.HandoffTerminal:   synchronousHandoff{r: r, mode: model.HandoffTerminal},
		model.HandoffBackground: backgroundHandoff{r: r},
		model.HandoffFleet:      fleetHandoff{r: r},
	}
	return r
}

// Turn is everything needed to run one exchange.
type Turn struct {
	WorkspaceID int64
	UserID      int64
	SessionID   int64
	// Show is what the person may be told of HOW this answer was reached: the
	// thinking, and the tools with what went in and came back.
	//
	// A pointer, so that "nobody said" is a state of its own. A zero Show means
	// showing nothing, and a turn built by a path that has no opinion (an
	// agent's inner loop, a background run, a test) must not silently become a
	// turn that hides everything.
	Show *chat.Show
	// DeviceID is which of the person's computers this turn came from, carried
	// so that a tool reaching their own network reaches the right one. It
	// travels with the turn rather than being looked up, because a background
	// turn started an hour later has to reach the same computer.
	DeviceID string
	ModelID  int64
	// Prompt is the user's message. It is empty on a resumed turn, where the
	// transcript already holds everything.
	Prompt string
	// Heard is anything the person has said SINCE this turn started, taken at
	// each step.
	//
	// A conversation answers one thing at a time, and that used to mean a
	// message typed mid-answer was refused outright. But somebody typing while
	// the assistant works is usually correcting what they asked for, not queuing
	// a second request, and making them wait for an answer they have already
	// changed their mind about is the wrong end of the trade. So it goes INTO
	// the turn, at the same point a tool result does, and the model sees it
	// before choosing what to do next.
	//
	// A seam rather than a dependency: what is running the turn knows how to
	// collect what was said, and the loop only knows that it can ask. Nil on
	// every path where nobody can say anything (an agent's inner loop, a
	// background run, a test).
	Heard func() []string
	// Attachments are the public ids of the files sent with this message. They
	// are stored on the user step, so every later turn of the conversation
	// carries the same files without the person resending them.
	Attachments []string
	// ReadAttachments turns those ids into the account of what the files hold,
	// which is what actually reaches the model. The Runner does not know how a
	// file is read; it knows that something can, and asks.
	//
	// It is a seam rather than a dependency because reading a file is an
	// application concern (which model, which rules) and the loop is not the
	// place that decides any of it.
	ReadAttachments func(ctx context.Context, workspaceID int64, ids []string) string
	// SystemPrompt and Tools are the resolved profile: what the layered
	// configuration decided this turn is, for this person, on this channel.
	SystemPrompt string
	Tools        tool.Loadout
	// Reasoning is the agent's single generic flag. The gateway drops it for
	// models that cannot reason.
	Reasoning bool
	// Settings are this agent's chosen values for what its vendor declares, most
	// notably how hard to think. They are the NEAREST bag: whatever is set here
	// beats the model's and the vendor's, because the agent is the narrowest
	// place the decision is made (KB/15).
	Settings model.Settings
	// MaxIterations bounds this turn's tool loop: how many model round-trips it
	// may take before it stops. Zero falls back to the code default, so a caller
	// with no opinion still runs bounded.
	MaxIterations int
	// MaxFleetAgents bounds how many agents ONE fleet call may start. Zero falls
	// back to the code default, so a caller with no opinion still runs bounded.
	MaxFleetAgents int
	// ApprovalTTL is how long a confirmation this turn raises stays
	// answerable. Zero falls back to the code default, so a caller that has no
	// opinion cannot accidentally park something that expires immediately.
	ApprovalTTL time.Duration
	// AutoApprove runs approval-gated tools without a card, for the Gateway and
	// every agent it starts. It is the conversation's own setting (a person
	// chose it for this session), never a default: nothing sets it unless a
	// person turned it on.
	AutoApprove bool
	// noApprovals stops any NEW approval card from being raised, and IS the
	// reason the model is given instead. Empty means cards can be raised.
	//
	// Two different situations set it, and they are the same rule: there is no
	// card to raise. A continuation that follows a rejection must not silently
	// re-card the very thing just refused; a fleet member has nobody to ask,
	// because it runs on a worker with no conversation waiting on it. Internal
	// to the runtime; a request never sets it.
	noApprovals string
	// NameConversation asks for a title once the turn is answered. It is set
	// on the first turn of a conversation and nowhere else: a chat is named
	// after what it turned out to be about, once.
	NameConversation bool
	// Resume carries an approved or rejected confirmation.
	Resume *Resume
	// Agent resolves an agent the Gateway may delegate to. It is set only
	// for a Gateway that has agents (the delegate tool is in its loadout);
	// nil everywhere else, which is what keeps delegation a single level deep.
	Agent AgentResolver
	// StartBackground hands a background-mode delegation to the app, which owns
	// the goroutine that runs the agent (Mode C, KB/27). It is a callback
	// the app sets, exactly like Agent: nil where background delegation is not
	// available, which makes delegate() refuse the mode rather than half-run it.
	StartBackground func(context.Context, BackgroundDelegation)
	// StartFleet hands a whole batch to the app, which owns the queue that
	// carries it to the workers and the join that brings the Gateway back once
	// (KB/35). Same seam as StartBackground and for the same reason: the loop
	// decides that a fleet should happen, the app knows how to make one happen.
	StartFleet func(context.Context, FleetRequest)
	// CompletedDelegationID marks a SERVER-INITIATED completion turn: a background
	// delegation finished, and this turn threads its result into the Gateway's
	// transcript and lets the Gateway narrate or act on it. Zero on every ordinary
	// turn; set only by the app when it schedules a completion (KB/27).
	CompletedDelegationID int64
	// CompletedFleetID is the same thing for a whole batch: every agent the
	// Gateway started in one fleet call is back, and this turn threads ALL of
	// their results onto that one call and lets the Gateway read them together.
	// It is the join, and it is why a fleet is not a loop over background
	// delegations: the Gateway is woken ONCE.
	CompletedFleetID int64
	// DelegationID is the row THIS turn is running as, when it is an agent's own
	// turn rather than the Gateway's. It is stamped onto any card the agent
	// raises, so an answer comes back to the member that asked (a batch's members
	// share one parent call, and nothing else tells them apart).
	DelegationID int64
}

// ServerInitiated reports whether this turn has no request behind it.
//
// It matters to exactly one caller and for exactly one reason: nobody is
// listening to a turn nobody asked for, so the person's tabs have to be TOLD it
// started or the answer is written to a conversation they are still watching go
// quiet (KB/27). That caller should not have to know how many kinds of
// server-initiated turn there are, which is what went wrong when the fleet
// became the second one: the check named a field, the field was the other one,
// and the turn ran perfectly with nobody listening.
func (t Turn) ServerInitiated() bool {
	return t.CompletedDelegationID != 0 || t.CompletedFleetID != 0
}

// Resume is a turn continuing after a human answered a confirmation.
type Resume struct {
	Snapshot *model.ParkSnapshot
	Approved bool
	// Token is echoed back on the confirm_resolved frame, so the client can
	// match the decision to the card it is showing.
	Token string
}

// Run executes the turn, writing frames as it goes. It returns only when the
// turn is over: answered, parked for approval, or failed.
func (r *Runner) Run(ctx context.Context, turn Turn, out *chat.Stream) error {
	resolved, err := r.gateway.Resolve(ctx, turn.WorkspaceID, turn.ModelID)
	if err != nil {
		r.log.Error().Err(err).Int64("model_id", turn.ModelID).Msg("resolve model")
		return r.fail(ctx, turn, out, "This assistant is not available right now.")
	}

	handlers := turn.Tools.Handlers
	tools := toolDefs(turn.Tools.Schemas)
	byName := runnable(turn.Tools)

	flag := &memoryFlag{}
	var reply answer
	var messages []provider.Message

	switch {
	case turn.CompletedFleetID != 0:
		// Every agent of a fleet is back. Same server-initiated shape as a
		// single background completion, with one difference that is the whole
		// point: ALL of the results land on the one call the Gateway made, and
		// it is woken once rather than once per agent.
		reply, messages, err = r.runFleetCompletion(ctx, turn, resolved, handlers, tools, byName, flag, out)

	case turn.CompletedDelegationID != 0:
		// A background delegation finished. This turn is server-initiated: it
		// threads the agent's result into the Gateway's transcript and runs
		// the Gateway loop, which narrates the answer, acts on it, or delegates
		// again. It is the park/resume path with a goroutine as the trigger
		// instead of a person (KB/27).
		reply, messages, err = r.runCompletion(ctx, turn, resolved, handlers, tools, byName, flag, out)

	case turn.Resume != nil && turn.Resume.Snapshot.AgentKey != "":
		// The person answered a card a AGENT raised. The whole resume
		// happens inside that delegation: its call is finished with the
		// agent's own handlers, its inner conversation is rebuilt and
		// carried on, and then the Gateway reads the result (continue) or the
		// agent's answer stands (terminal). See resumeDelegation.
		reply, err = r.resumeDelegation(ctx, turn, resolved, handlers, tools, byName, flag, out)

	case turn.Resume != nil && !turn.Resume.Approved:
		// The person declined. The parked call is resolved as refused (the card
		// flips to rejected, the tool row records it), and then control is handed
		// BACK to the model to respond: acknowledge the refusal and either take a
		// different path or ask how to proceed, rather than going silent. It runs
		// WITH its tools (so DeepSeek does not spill raw protocol the way a
		// no-tools turn does), but noApprovals stops it from raising ANOTHER
		// card this turn: it cannot silently re-propose the very thing just
		// refused, it has to talk to the person.
		if err = r.completeParkedTool(ctx, turn, handlers, byName, out); err != nil {
			return err
		}
		messages, err = r.buildTranscript(ctx, turn)
		if err != nil {
			r.log.Error().Err(err).Msg("build transcript")
			return r.fail(ctx, turn, out, "Your conversation could not be loaded.")
		}
		turn.noApprovals = approvalsAfterRejection
		reply, err = r.loop(ctx, turn, resolved, messages, tools, handlers, byName, out, flag)

	default:
		// A resumed turn finishes the call that was waiting on the person FIRST,
		// writing its result against the very row the card was showing. Only
		// then is the conversation rebuilt, so the model sees a transcript in
		// which every call has an answer, in the order it happened.
		if turn.Resume != nil {
			if err = r.completeParkedTool(ctx, turn, handlers, byName, out); err != nil {
				return err
			}
		}
		messages, err = r.buildTranscript(ctx, turn)
		if err != nil {
			r.log.Error().Err(err).Msg("build transcript")
			return r.fail(ctx, turn, out, "Your conversation could not be loaded.")
		}
		reply, err = r.loop(ctx, turn, resolved, messages, tools, handlers, byName, out, flag)
	}
	if err != nil {
		if errors.Is(err, errParked) {
			// The turn is not finished: it is waiting on a person, and the
			// stream has already been closed with a confirmation card.
			return nil
		}
		return err
	}

	if err := r.store.Agent().SetSessionStatus(ctx, turn.SessionID, model.SessionCompleted); err != nil {
		r.log.Error().Err(err).Msg("complete session")
	}

	// A brand new conversation gets a name, from the same model that answered
	// it. This runs after the answer is sent, and the user never waits on it. A
	// resumed turn carries no prompt of its own, so the question being named is
	// read back from the conversation.
	if turn.NameConversation {
		r.NameConversation(turn, firstAsked(turn.Prompt, messages), reply.Content)
	}
	// Reconsider what the assistant remembers, on a cadence and when the model
	// flagged the turn. The ordinal is the number of the user's own turns in the
	// rebuilt transcript. A resumed delegation loaded its own transcripts rather
	// than the Gateway's here, so the Gateway's is read back for the count (the
	// prompt is already recorded; this is a pure read). Like the title, this is
	// scheduled after the answer is sent and the person never waits on it.
	if messages == nil {
		messages, _ = r.buildTranscript(ctx, turn)
	}
	r.scheduleMemory(turn, countUserTurns(messages), flag)
	return out.Result(reply.Content)
}

// settleUnreached closes off the calls a parked turn never got to. They did not
// run and never will: the model reissues what it still wants when the person has
// answered, so these are finished, with nothing to show for them.
func (r *Runner) settleUnreached(ctx context.Context, turn Turn, calls []*model.ToolCall) {
	for _, call := range calls {
		r.resolveCall(ctx, turn, call, model.ToolCallCompleted, notExecuted, "", 0, false)
	}
}

// firstAsked is the question a title should be about: this turn's prompt, or,
// for a turn that has none because it resumed one that stopped to ask a person,
// the first thing the person said in the conversation.
func firstAsked(prompt string, messages []provider.Message) string {
	if strings.TrimSpace(prompt) != "" {
		return prompt
	}
	for _, message := range messages {
		if message.Role == provider.RoleUser && strings.TrimSpace(message.Content) != "" {
			return message.Content
		}
	}
	return ""
}

// errParked ends the loop without ending the turn: a confirmation is pending.
var errParked = errors.New("agent: turn parked for approval")

type answer struct {
	Content   string
	Reasoning string
}

// loop is the tool loop: stream, execute what the model asked for, repeat.
func (r *Runner) loop(
	ctx context.Context,
	turn Turn,
	resolved *provider.Resolved,
	messages []provider.Message,
	tools []provider.ToolDef,
	handlers map[string]tool.Handler,
	byName map[string]tool.Schema,
	out *chat.Stream,
	flag *memoryFlag,
) (answer, error) {
	// An approval is consumed once. Within this turn, the same action must
	// not ask again after the user already said yes.
	approved := map[string]bool{}
	if turn.Resume != nil && turn.Resume.Approved {
		approved[turn.Resume.Snapshot.ActionHash] = true
	}

	// How many times the Gateway has looked at its background tasks this turn. A
	// model that ignores "do not poll" would otherwise loop on the check tool and
	// flood the transcript, so past a small cap the loop refuses and ends (KB/27).
	checkCalls := 0

	limit := turn.MaxIterations
	if limit <= 0 {
		limit = model.DefaultMaxIterations
	}
	for iteration := 0; iteration < limit; iteration++ {
		// Anything the person said while this was running, folded in before the
		// model is asked what to do next. Here rather than anywhere else because
		// this is a STEP BOUNDARY: nothing is half-done, the transcript is
		// consistent, and a correction arrives in time to change what happens
		// rather than after it has happened.
		if err := r.foldInWhatWasSaid(ctx, turn, &messages, out); err != nil {
			r.log.Error().Err(err).Msg("could not add what was said mid-turn")
		}

		req := resolved.Prepare(provider.GenerateRequest{
			Messages:  messages,
			Tools:     tools,
			Reasoning: turn.Reasoning,
			Settings:  turn.Settings,
		})

		seq, err := r.store.Agent().NextSeq(ctx, turn.SessionID)
		if err != nil {
			r.log.Error().Err(err).Msg("next step")
			return answer{}, r.fail(ctx, turn, out, "Your conversation could not be continued.")
		}

		step, unknown, err := r.streamStep(ctx, turn, resolved, req, byName, seq, out)
		if err != nil {
			// A RETRY THAT WORKED IS NOT A FAILURE. settleStreamBreak writes the
			// retried step back through those pointers and returns nil, and this
			// used to return answer{} with that nil anyway: the retried step was
			// never committed, its tool calls never ran, and the turn ended empty
			// with no error logged, while the person watched the retried text
			// stream in and then stop. Carry on round the loop with what the
			// retry produced, which is what the pointer parameters were for.
			if broke := r.settleStreamBreak(ctx, turn, resolved, req, byName, seq, out, err, &step, &unknown); broke != nil {
				return answer{}, broke
			}
		}

		// The step is committed BEFORE its tools run. What the model asked for
		// is a fact the moment it asked, and a crash between the request and
		// the result must not erase the request: the tool rows are written here
		// with their arguments, and each one is resolved in place as it runs.
		// A step made of nothing but hallucinated calls has no words and no real
		// tool rows: it is not written down, because as far as the person and the
		// transcript are concerned it never happened.
		if step.HasText() || step.Reasoning != "" || len(step.ToolCalls) > 0 {
			if err := r.store.Agent().SaveStep(ctx, step); err != nil {
				r.log.Error().Err(err).Msg("save step")
				return answer{}, r.fail(ctx, turn, out, "Your conversation could not be saved.")
			}
			if step.HasText() {
				if err := r.store.Agent().TouchChat(ctx, turn.SessionID); err != nil {
					r.log.Error().Err(err).Msg("touch chat")
				}
			}
		}

		// Nothing real was asked for and nothing to correct: the model answered.
		if len(step.ToolCalls) == 0 && len(unknown) == 0 {
			// The model has said its piece and asked for nothing more. Before
			// this turn ends, one last look at whether the person said anything
			// while it was talking.
			//
			// This is what closes the gap the whole feature would otherwise leave
			// open. Words are accepted while a conversation is answering, so they
			// can arrive on its very last step, after the check at the top of this
			// iteration and before the turn is over. Rather than reconstruct a
			// turn to carry them, the turn simply does not end while something is
			// unheard: the answer just given is already in the transcript, the new
			// words go in after it, and the model gets another pass. Bounded by
			// the same iteration limit as everything else here.
			messages = append(messages, provider.Message{Role: provider.RoleAssistant, Content: step.Text})
			heardLate := len(messages)
			if err := r.foldInWhatWasSaid(ctx, turn, &messages, out); err != nil {
				r.log.Error().Err(err).Msg("could not add what was said at the end of a turn")
			}
			if len(messages) > heardLate {
				continue
			}
			return answer{Content: step.Text, Reasoning: step.Reasoning}, nil
		}

		// The assistant turn goes back to the model before any results, or the
		// pairing every vendor validates is broken. Any hallucinated calls ride
		// along here paired with an error result below, so the model sees a
		// well-formed exchange and can recover, yet they never touched the UI or
		// the transcript.
		messages = append(messages, assistantMessageWithUnknown(step, unknown))
		for _, u := range unknown {
			messages = append(messages, provider.Message{
				Role:       provider.RoleTool,
				ToolCallID: u.ID,
				Content:    unavailableToolNote(u.Name),
			})
		}

		stop := false
		for index, call := range step.ToolCalls {
			// Delegation is not an ordinary tool: the agent runs its own
			// loop and, in terminal mode, its answer ends the turn. So it is
			// handled here, where turn control lives, not through a handler.
			if call.ToolName == model.DelegateToolName || call.ToolName == model.FleetToolName {
				var (
					result         tool.Result
					terminalAnswer string
					terminal       bool
					err            error
				)
				if call.ToolName == model.FleetToolName {
					result, terminalAnswer, terminal, err = r.delegateFleetCall(ctx, turn, call, out)
				} else {
					result, terminalAnswer, terminal, err = r.delegate(ctx, turn, call, out)
				}
				// Both of these leave the loop in the MIDDLE of the step's
				// calls, which is what makes them different from every other
				// branch here. SaveStep wrote all of them as running before any
				// of them ran, so whatever the model asked for after the
				// delegation is a row nothing will ever come back to.
				//
				// Skipping them is right: a terminal handoff means the
				// specialist's answer IS the answer and the turn is over, and a
				// park means the turn is suspended and resumes from the park
				// rather than from here. What was missing is saying so. A call
				// left running is drawn as a spinner on reload, forever, for
				// work nothing is doing.
				if err != nil {
					r.settleUnreached(ctx, turn, step.ToolCalls[index+1:])
					return answer{}, err
				}
				if terminal {
					r.settleUnreached(ctx, turn, step.ToolCalls[index+1:])
					return answer{Content: terminalAnswer}, nil
				}
				messages = append(messages, provider.Message{
					Role:       provider.RoleTool,
					ToolCallID: call.ToolCallID,
					Content:    string(result.Content),
				})
				continue
			}

			// Remembering is not an ordinary tool either: it does no work in the
			// turn. It only flags that this conversation is worth a background
			// memory update, so the model can call it without slowing the answer.
			if call.ToolName == model.MemoryToolName {
				noteMemoryFlag(flag, call.Args)
				// The signal did its work the moment it was called, so it is
				// resolved right here. Without this the call row is left in its
				// initial "running" state (see stepBuffer.step): the turn moves
				// on, but a reload rebuilds the transcript from that row and the
				// chip spins forever. It is a finished, zero-duration result.
				ack := json.RawMessage(`{"acknowledged":true}`)
				r.resolveCall(ctx, turn, call, model.ToolCallCompleted, ack, "", 0, false)
				if err := r.emitToolDone(out, call, model.ToolCallCompleted, ack, 0, ourOwnWiring); err != nil {
					return answer{}, err
				}
				messages = append(messages, provider.Message{
					Role:       provider.RoleTool,
					ToolCallID: call.ToolCallID,
					Content:    string(ack),
				})
				continue
			}

			// Cap the background-status check: a couple of looks are legitimate (the
			// person asked), but past that it is a polling loop, so the loop stops
			// checking and ends the turn rather than flood the person with it.
			if call.ToolName == model.BackgroundStatusToolName {
				if checkCalls++; checkCalls > maxBackgroundChecks {
					note := json.RawMessage(`{"note":"Stop checking. You are brought back automatically the moment a background task finishes. End your turn now and tell the person it is still running; do not check again."}`)
					r.resolveCall(ctx, turn, call, model.ToolCallCompleted, note, "", 0, false)
					if err := r.emitToolDone(out, call, model.ToolCallCompleted, note, 0, ourOwnWiring); err != nil {
						return answer{}, err
					}
					messages = append(messages, provider.Message{
						Role: provider.RoleTool, ToolCallID: call.ToolCallID, Content: string(note),
					})
					stop = true
					continue
				}
			}

			result, err := r.executeTool(ctx, turn, resolved, call, handlers, byName, approved, out, flag)
			if err != nil {
				if errors.Is(err, errParked) {
					// The turn stops here to ask a person. The calls the model asked
					// for after this one are never reached, and their rows were
					// written when the step was committed (KB/16), so without this
					// they stay "running" and a reloaded conversation shows them
					// spinning for work nothing is doing. The model already reads
					// them as not carried out (see notExecuted); this tells the
					// transcript the same thing.
					r.settleUnreached(ctx, turn, step.ToolCalls[index+1:])
				}
				return answer{}, err
			}
			messages = append(messages, provider.Message{
				Role:       provider.RoleTool,
				ToolCallID: call.ToolCallID,
				Content:    string(result.Content),
			})
			if result.StopToolLoop {
				stop = true
			}
		}

		if stop {
			// A tool asked the loop to end. The model gets one last pass with
			// no tools, so it can put the result into words.
			return r.finalPass(ctx, turn, resolved, messages, out)
		}
	}

	// The model never stopped calling tools. Say so plainly rather than
	// pretending the turn succeeded, and say what can be done about it: this is
	// a configured limit, and the person reading it is usually the person who
	// can raise it.
	r.log.Warn().Int64("session_id", turn.SessionID).Int("limit", limit).
		Msg("tool loop hit its limit")
	return r.stoppedShort(ctx, turn, limit)
}

// stoppedShort ends a turn that ran out of steps, and RECORDS that it did.
//
// Recording is the point. The message used to be streamed and never written
// down, so it existed for as long as the person was watching: reload the
// conversation and it stopped mid-task with no explanation, which reads as a
// disconnection rather than as a limit somebody can change. It is written as an
// ordinary assistant step, because that is what it is from the transcript's
// point of view: the last thing the assistant said.
func (r *Runner) stoppedShort(ctx context.Context, turn Turn, limit int) (answer, error) {
	said := fmt.Sprintf(
		"I stopped before finishing: this turn reached its limit of %d steps. "+
			"Ask me to carry on and I will pick up where I left off, or raise the limit "+
			"for this assistant in the console.", limit)

	seq, err := r.store.Agent().NextSeq(ctx, turn.SessionID)
	if err != nil {
		// Not a reason to leave the person with silence: the words still go out
		// on the stream, they just will not survive a reload.
		r.log.Error().Err(err).Int64("session_id", turn.SessionID).Msg("next step for the limit notice")
		return answer{Content: said}, nil
	}
	step := &model.AgentStep{
		SessionID: turn.SessionID,
		Seq:       seq,
		Kind:      model.StepAssistant,
		Text:      said,
		CreatedAt: time.Now().UTC(),
	}
	if err := r.store.Agent().SaveStep(ctx, step); err != nil {
		r.log.Error().Err(err).Int64("session_id", turn.SessionID).Msg("record the limit notice")
	}
	return answer{Content: said}, nil
}

// assistantMessage is the model-facing shape of a step the loop just produced.
// It is built the same way a reloaded step is (see conversation), so a turn
// that continues in memory and a turn rebuilt from the database show the model
// exactly the same conversation.
func assistantMessage(step *model.AgentStep) provider.Message {
	message := provider.Message{
		Role:      provider.RoleAssistant,
		Content:   step.Text,
		Reasoning: step.Reasoning,
	}
	for _, call := range step.ToolCalls {
		message.ToolCalls = append(message.ToolCalls, provider.ToolCall{
			ID: call.ToolCallID, Name: call.ToolName, Args: call.Args,
		})
	}
	return message
}

// assistantMessageWithUnknown is assistantMessage plus the calls the model made
// up (tools it was never given). They are added ONLY to the model-facing
// message, never the transcript: pairing them here with an error result lets the
// model see a well-formed exchange and correct itself, without the hallucination
// ever becoming a card or a saved tool row.
func assistantMessageWithUnknown(step *model.AgentStep, unknown []provider.ToolCall) provider.Message {
	message := assistantMessage(step)
	for _, u := range unknown {
		message.ToolCalls = append(message.ToolCalls, provider.ToolCall{
			ID: u.ID, Name: u.Name, Args: u.Args,
		})
	}
	return message
}

// unavailableToolNote is the result handed back for a hallucinated tool call. It
// names the tool so the model stops trying it and tells it to use what it has or
// answer directly. It is deliberately terse: it is read by a model, not a person.
func unavailableToolNote(name string) string {
	note := map[string]any{
		"success": false,
		"error": "There is no tool named " + name + " available to you. " +
			"Do not try to call it. Use only the tools you were given, or answer the person directly.",
	}
	payload, _ := json.Marshal(note)
	return string(payload)
}

// settleStreamBreak decides what a broken stream means, and tries once more when
// it can.
//
// A stream that stopped having said NOTHING is almost always the network: a
// connection dropped, or a message arrived as a fragment. Nobody saw anything,
// so asking again costs a moment and nothing else, and the alternative is
// telling a person their assistant failed because a packet went missing.
//
// A stream that stopped MID-SENTENCE cannot be retried. The words are already on
// screen, and a second attempt would say them again: an answer that stutters and
// restarts is worse than one that stops and says so.
func (r *Runner) settleStreamBreak(
	ctx context.Context,
	turn Turn,
	resolved *provider.Resolved,
	req provider.GenerateRequest,
	byName map[string]tool.Schema,
	seq int,
	out *chat.Stream,
	err error,
	step **model.AgentStep,
	unknown *[]provider.ToolCall,
) error {
	var broken *streamBroken
	if !errors.As(err, &broken) {
		return err // not a break: a cancelled turn, or something already reported
	}
	if !broken.retryable() {
		r.log.Warn().Err(broken.err).Int64("session_id", turn.SessionID).Int("spoken", broken.spoken).
			Msg("model stream broke mid-answer, not retrying")
		return r.fail(ctx, turn, out, "The assistant could not finish this response.")
	}

	r.log.Warn().Err(broken.err).Int64("session_id", turn.SessionID).
		Str("vendor", resolved.Vendor.VendorKey).Msg("model stream broke before it said anything, trying once more")

	retried, retriedUnknown, rerr := r.streamStep(ctx, turn, resolved, req, byName, seq, out)
	if rerr != nil {
		var again *streamBroken
		if errors.As(rerr, &again) {
			r.log.Error().Err(again.err).Int64("session_id", turn.SessionID).
				Msg("model stream broke twice")
			return r.fail(ctx, turn, out, "The assistant could not finish this response.")
		}
		return rerr
	}
	*step, *unknown = retried, retriedUnknown
	return nil
}

// streamBroken is a model stream that stopped before the model was finished:
// the connection dropped, or what arrived was a fragment of a message rather
// than a message.
//
// It is a TYPE rather than a failed turn because the two are different things
// and only the caller can tell them apart. A stream that broke having said
// nothing can simply be asked again, and nobody is any the wiser. One that broke
// half way through a sentence cannot: asking again would say the first half
// twice, and a person watching an answer stutter and restart trusts it less than
// one that stops honestly.
type streamBroken struct {
	err error
	// spoken is how much had already reached the person when it broke.
	spoken int
}

func (e *streamBroken) Error() string { return e.err.Error() }
func (e *streamBroken) Unwrap() error { return e.err }

// retryable reports whether this break can be tried again without the person
// seeing the same words twice.
func (e *streamBroken) retryable() bool { return e.spoken == 0 }

// streamStep runs one model call, forwarding its output to the client as it
// arrives, checkpointing it to the database as it grows, and returning the step
// it produced.
func (r *Runner) streamStep(
	ctx context.Context,
	turn Turn,
	resolved *provider.Resolved,
	req provider.GenerateRequest,
	byName map[string]tool.Schema,
	seq int,
	out *chat.Stream,
) (*model.AgentStep, []provider.ToolCall, error) {
	started := time.Now()

	r.log.Debug().Int64("session_id", turn.SessionID).Int64("model_id", resolved.Model.ID).
		Int("messages", len(req.Messages)).Int("tools", len(req.Tools)).Msg("calling model")

	events, err := resolved.Provider.Stream(ctx, req)
	if err != nil {
		r.recordModelFailure(ctx, turn, resolved, started, err)
		return nil, nil, r.fail(ctx, turn, out, "The assistant could not be reached.")
	}

	buf := &stepBuffer{}
	// From here the answer is being written down as it arrives. A process that
	// dies mid-sentence leaves the sentence behind.
	stopCheckpoint := r.checkpoint(ctx, turn, seq, resolved, buf)
	defer stopCheckpoint()

	announced := map[string]bool{}
	// Calls the model made up: a name it was never handed. They are not tool
	// usage, so they never announce, never buffer, and never reach the
	// transcript; the loop feeds the model a plain "no such tool" from these so
	// it can correct itself.
	var unknown []provider.ToolCall
	for event := range events {
		switch event.Kind {
		case provider.EventContentDelta:
			buf.addContent(event.ContentDelta)
			if err := out.Delta(event.ContentDelta); err != nil {
				// The listener is gone, but the answer is not: what has arrived
				// so far is kept, so a reader who comes back finds it.
				stopCheckpoint()
				r.saveInterrupted(turn, seq, resolved, buf)
				return nil, nil, err
			}

		case provider.EventReasoningDelta:
			buf.addReasoning(event.ReasoningDelta)
			if err := out.ReasoningDelta(event.ReasoningDelta); err != nil {
				stopCheckpoint()
				r.saveInterrupted(turn, seq, resolved, buf)
				return nil, nil, err
			}

		case provider.EventToolCall:
			if announced[event.ToolCall.ID] {
				break // one call id is handled once
			}
			announced[event.ToolCall.ID] = true
			// A tool the model was not given is a hallucination, not an action.
			// Collect it for the correction and move on: no card, no row.
			if _, ok := byName[event.ToolCall.Name]; !ok {
				unknown = append(unknown, *event.ToolCall)
				break
			}
			buf.addToolCall(*event.ToolCall)
			// Announce the tool before it runs, so the UI can say what is
			// happening rather than going silent. A call made through discovery
			// announces the tool it will actually run, for the same reason its
			// row records that tool: a chip saying "Connected services" while a
			// service's tool runs describes the plumbing, not the work.
			announced, _, viaDiscovery := discovered(event.ToolCall.Name, event.ToolCall.Args, byName)
			if !viaDiscovery {
				announced = event.ToolCall.Name
			}
			if err := out.Write(chat.Frame{
				Type: chat.FrameToolPreparing,
				Message: chat.ToolMessage{
					Name:         announced,
					FriendlyName: friendlyName(byName, announced),
					Narration:    narration(byName, announced),
				},
			}); err != nil {
				stopCheckpoint()
				r.saveInterrupted(turn, seq, resolved, buf)
				return nil, nil, err
			}

		case provider.EventUsage:
			if event.Usage != nil {
				r.recordModelCall(ctx, turn, resolved, started, *event.Usage)
			}

		case provider.EventError:
			r.recordModelFailure(ctx, turn, resolved, started, event.Err)
			// Everything known about the break, in one line, because from here
			// the only evidence is what this says. `spoken` is what the person
			// had already been shown: it decides whether this can be tried
			// again, and it is the difference between a blip and a lost answer.
			r.log.Error().Err(event.Err).
				Int64("session_id", turn.SessionID).Int64("model_id", resolved.Model.ID).
				Str("vendor", resolved.Vendor.VendorKey).Str("model", resolved.Model.ModelKey).
				Int("messages", len(req.Messages)).
				Int("spoken", buf.spoken()).
				Dur("after", time.Since(started)).
				Msg("model stream failed")
			stopCheckpoint()
			// Whatever the model managed to say before it broke is kept, and
			// marked partial, rather than thrown away because the end never came.
			r.saveInterrupted(turn, seq, resolved, buf)
			return nil, nil, &streamBroken{err: event.Err, spoken: buf.spoken()}

		case provider.EventDone:
		}
	}

	// The stream ended. If the context died while it was arriving, the model
	// did NOT finish: what it managed to say is a fragment, and committing it
	// as a finished answer would put words in its mouth. It stays partial, and
	// the turn ends as what it was: stopped.
	if err := ctx.Err(); err != nil {
		stopCheckpoint()
		r.saveInterrupted(turn, seq, resolved, buf)
		return nil, nil, err
	}

	// The step is complete: it stops being partial here, and this is the write
	// the caller commits.
	return buf.step(turn, seq, resolved, byName, false), unknown, nil
}

// finalPass lets the model turn a tool result into prose, with no tools
// offered so it cannot start another round. It is a step like any other, and
// it is written down like any other.
func (r *Runner) finalPass(
	ctx context.Context,
	turn Turn,
	resolved *provider.Resolved,
	messages []provider.Message,
	out *chat.Stream,
) (answer, error) {
	req := resolved.Prepare(provider.GenerateRequest{
		Messages:  messages,
		Reasoning: turn.Reasoning,
		Settings:  turn.Settings,
	})

	seq, err := r.store.Agent().NextSeq(ctx, turn.SessionID)
	if err != nil {
		return answer{}, r.fail(ctx, turn, out, "Your conversation could not be continued.")
	}
	// No tools are offered here, so any call the model makes is a hallucination:
	// streamStep filters it out and it is discarded (finalPass only wants prose).
	final, unknown, err := r.streamStep(ctx, turn, resolved, req, nil, seq, out)
	if err != nil {
		// The same decision as the main loop: a break with nothing said is
		// worth one more go, and one mid-sentence is not. Never a raw stream
		// error to the person either way. And, as in the main loop, a retry
		// that WORKED carries on: `final` now holds it.
		if broke := r.settleStreamBreak(ctx, turn, resolved, req, nil, seq, out, err, &final, &unknown); broke != nil {
			return answer{}, broke
		}
	}
	if err := r.store.Agent().SaveStep(ctx, final); err != nil {
		r.log.Error().Err(err).Msg("save final step")
	}
	if final.HasText() {
		if err := r.store.Agent().TouchChat(ctx, turn.SessionID); err != nil {
			r.log.Error().Err(err).Msg("touch chat")
		}
	}
	return answer{Content: final.Text, Reasoning: final.Reasoning}, nil
}

// errFailed ends a turn that could not be finished. It is what fail returns, so
// every caller that does `return X, r.fail(...)` STOPS: the error frame is
// already streamed, and there is nothing more to run.
var errFailed = errors.New("agent: turn failed")

// fail ends the turn with a message a person can act on. The real cause is
// logged, never streamed: an internal error is not the user's problem to read.
//
// It returns errFailed, NOT the frame's write result: a successful write returns
// nil, and a nil error would tell the loop to carry on as if nothing was wrong,
// which is how a model overload once became a nil step and a panic.
func (r *Runner) fail(ctx context.Context, turn Turn, out *chat.Stream, message string) error {
	if err := r.store.Agent().SetSessionStatus(ctx, turn.SessionID, model.SessionFailed); err != nil {
		r.log.Error().Err(err).Msg("mark session failed")
	}
	if err := out.Error(message); err != nil {
		r.log.Error().Err(err).Int64("session_id", turn.SessionID).Msg("stream failure frame")
	}
	return errFailed
}

func (r *Runner) recordModelCall(ctx context.Context, turn Turn, resolved *provider.Resolved, started time.Time, usage provider.Usage) {
	err := r.store.Agent().RecordModelCall(ctx, &model.ModelCall{
		WorkspaceID:  turn.WorkspaceID,
		SessionID:    turn.SessionID,
		ModelID:      resolved.Model.ID,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		DurationMS:   time.Since(started).Milliseconds(),
		Status:       "completed",
	})
	if err != nil {
		r.log.Error().Err(err).Msg("record model call")
	}
}

func (r *Runner) recordModelFailure(ctx context.Context, turn Turn, resolved *provider.Resolved, started time.Time, cause error) {
	err := r.store.Agent().RecordModelCall(ctx, &model.ModelCall{
		WorkspaceID: turn.WorkspaceID,
		SessionID:   turn.SessionID,
		ModelID:     resolved.Model.ID,
		DurationMS:  time.Since(started).Milliseconds(),
		Status:      "failed",
		ErrorText:   cause.Error(),
	})
	if err != nil {
		r.log.Error().Err(err).Msg("record model failure")
	}
}

// --- transcript ---------------------------------------------------------------

// buildTranscript loads the conversation and records the new prompt.
//
// The stored steps are the source of truth. A live turn and a reloaded one are
// foldInWhatWasSaid adds anything the person said mid-turn to the transcript,
// both the model's copy and the stored one.
//
// Stored as an ordinary user step, because that is what it is: they said it, in
// this conversation, at this point. A reload has to show it in the same place,
// and the next turn has to see it in the same order.
func (r *Runner) foldInWhatWasSaid(ctx context.Context, turn Turn, messages *[]provider.Message, out *chat.Stream) error {
	if turn.Heard == nil {
		return nil
	}
	said := turn.Heard()
	if len(said) == 0 {
		return nil
	}
	for _, text := range said {
		seq, err := r.store.Agent().NextSeq(ctx, turn.SessionID)
		if err != nil {
			return err
		}
		if err := r.store.Agent().SaveStep(ctx, &model.AgentStep{
			SessionID: turn.SessionID,
			Seq:       seq,
			Kind:      model.StepUser,
			Text:      text,
		}); err != nil {
			return err
		}
		*messages = append(*messages, provider.Message{Role: provider.RoleUser, Content: text})
		// Told to whoever is listening, at the point it landed, so the screen and
		// the stored conversation agree about where it was said.
		if out != nil {
			if err := out.Write(chat.Frame{Type: chat.FrameSaid, Message: text}); err != nil {
				return err
			}
		}
	}
	if err := r.store.Agent().TouchChat(ctx, turn.SessionID); err != nil {
		return err
	}
	return nil
}

// built by the same function from the same rows, so they cannot disagree about
// what was said.
func (r *Runner) buildTranscript(ctx context.Context, turn Turn) ([]provider.Message, error) {
	if turn.Prompt != "" {
		seq, err := r.store.Agent().NextSeq(ctx, turn.SessionID)
		if err != nil {
			return nil, err
		}
		if err := r.store.Agent().SaveStep(ctx, &model.AgentStep{
			SessionID:   turn.SessionID,
			Seq:         seq,
			Kind:        model.StepUser,
			Text:        turn.Prompt,
			Attachments: turn.Attachments,
		}); err != nil {
			return nil, err
		}
		if err := r.store.Agent().TouchChat(ctx, turn.SessionID); err != nil {
			r.log.Error().Err(err).Msg("touch chat")
		}
	}

	steps, err := r.store.Agent().Transcript(ctx, turn.SessionID)
	if err != nil {
		return nil, err
	}
	// The Gateway sees the conversation's own timeline, never an agent's inner
	// steps: a delegation is represented to the model by the `delegate` call and
	// its result, and the agent's steps (now durable, so they can rebuild an
	// interrupted delegation) would be a second, confusing copy of the same work.
	steps = GatewaySteps(steps)
	// What the person attached becomes part of what they said, for the model.
	// It is composed here rather than stored on the step so the chat keeps
	// showing the person's own words; the account it is built from was written
	// once, when the file was first read, so this costs a row and never a
	// second reading.
	r.withAttachments(ctx, turn, steps)
	// A resumed turn has already written the result of the call it was waiting
	// on, so nothing needs skipping: the transcript is whole.
	return conversation(turn.SystemPrompt, steps, ""), nil
}

// GatewaySteps keeps only the conversation's own steps, dropping the inner steps
// of any delegation (those attributed to an agent).
func GatewaySteps(steps []*model.AgentStep) []*model.AgentStep {
	kept := steps[:0:0]
	for _, step := range steps {
		if step.AgentKey == "" {
			kept = append(kept, step)
		}
	}
	return kept
}

// --- helpers ---------------------------------------------------------------------

func toolDefs(schemas []tool.Schema) []provider.ToolDef {
	defs := make([]provider.ToolDef, 0, len(schemas))
	for _, s := range schemas {
		defs = append(defs, provider.ToolDef{
			Name:        s.Name,
			Description: s.Description,
			InputSchema: s.InputSchema,
		})
	}
	return defs
}

// discovered resolves a call made through the connected-services ability into
// the tool it names: its real name and its real arguments.
//
// Applied where a call is BUILT, not where it runs, and that placement is the
// whole of it. Everything about a tool call is decided from its name at the
// moment the row is written: what the transcript records, what the chat shows,
// what a confirmation card says, which definition hash is checked for drift
// (KB/20), and what a person sees when they open it. Resolving later ran the
// right tool while every one of those said "Connected services", which is a
// call recorded as something it was not.
//
// It answers false for everything else, including the ability's own reading
// operations, which are its handler's business and nobody else's. A tool this
// turn does not hold is left alone too, so it fails the way any invented tool
// name fails rather than being rewritten into a call to nothing.
func discovered(name string, args json.RawMessage, byName map[string]tool.Schema) (string, json.RawMessage, bool) {
	if name != integrations.Name {
		return "", nil, false
	}
	var asked struct {
		Operation string          `json:"operation"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(args, &asked); err != nil {
		return "", nil, false
	}
	if !strings.EqualFold(strings.TrimSpace(asked.Operation), integrations.CallOperation) {
		return "", nil, false
	}
	wanted := strings.TrimSpace(asked.Tool)
	if _, held := byName[wanted]; !held {
		return "", nil, false
	}
	arguments := asked.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	return wanted, arguments, true
}

// runnable is every schema a turn can RUN: what it was offered, and what it
// holds on demand for the model to discover (tool.Loadout). Anything deciding
// what a call means, its approval, its risk, its drift check, must see both, or
// a discovered tool would run with no schema at all.
func runnable(loadout tool.Loadout) map[string]tool.Schema {
	byName := schemaByName(loadout.Schemas)
	for _, schema := range loadout.OnDemand {
		byName[schema.Name] = schema
	}
	return byName
}

func schemaByName(schemas []tool.Schema) map[string]tool.Schema {
	byName := make(map[string]tool.Schema, len(schemas))
	for _, s := range schemas {
		byName[s.Name] = s
	}
	return byName
}

func friendlyName(byName map[string]tool.Schema, name string) string {
	if schema, ok := byName[name]; ok && schema.FriendlyName != "" {
		return schema.FriendlyName
	}
	return name
}

// narration is the present-tense label shown while a tool runs. A tool that
// wrote one gets it; otherwise the client falls back to the friendly name, so
// this stays empty rather than repeating it.
func narration(byName map[string]tool.Schema, name string) string {
	if schema, ok := byName[name]; ok {
		return schema.FriendlyNarration
	}
	return ""
}

// newToken mints a confirmation token and the hash stored against it. The
// token itself is never persisted, so a leaked database cannot approve
// anything.
// NewToken mints a fresh confirmation token and its hash. It is how a reload
// re-issues a card whose original token was never stored, only hashed.
func NewToken() (token, hash string, err error) { return newToken() }

func newToken() (token, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("agent: read random: %w", err)
	}
	token = "sag_cf_" + base64.RawURLEncoding.EncodeToString(raw)
	return token, HashToken(token), nil
}

// HashToken is how a presented confirmation token is looked up.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// actionHash identifies one action: the same tool with the same arguments.
// It is what stops a single approval from authorizing a different action, and
// what stops the same action from asking twice in one turn.
func actionHash(name string, args json.RawMessage) string {
	// Arguments are canonicalized so that formatting differences cannot
	// produce two hashes for what is really one action.
	var canonical any
	if err := json.Unmarshal(args, &canonical); err == nil {
		if encoded, err := json.Marshal(canonical); err == nil {
			args = encoded
		}
	}
	sum := sha256.Sum256(append([]byte(name+"\x00"), args...))
	return hex.EncodeToString(sum[:])
}

// approvalTTL is how long this turn's confirmations stay answerable. A turn
// that says nothing gets the code default rather than a window of zero, because
// a card that has already expired when it is drawn is worse than no card.
func (t Turn) approvalTTL() time.Duration {
	if model.ValidApprovalTTL(t.ApprovalTTL) {
		return t.ApprovalTTL
	}
	return model.DefaultApprovalTTL
}

// withAttachments folds the account of each attached file into the message it
// was sent with, in place, for the model's copy of the transcript only.
//
// Every turn of a conversation goes through here, so a file attached twenty
// turns ago still travels with its message. That is the point: the person
// attached it once and should not have to mention it again. The reading model
// is not involved, having been called the one time the file arrived.
func (r *Runner) withAttachments(ctx context.Context, turn Turn, steps []*model.AgentStep) {
	if turn.ReadAttachments == nil {
		return
	}
	for _, step := range steps {
		if step.Kind != model.StepUser || len(step.Attachments) == 0 {
			continue
		}
		if text := turn.ReadAttachments(ctx, turn.WorkspaceID, step.Attachments); text != "" {
			step.Text += text
		}
	}
}
