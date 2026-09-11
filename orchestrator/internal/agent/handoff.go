package agent

import (
	"context"
	"time"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
)

// How a delegation runs, as one thing rather than a condition spread out.
//
// There are three answers to "what happens when the Gateway hands work to an
// agent", and there will be a fourth: run it here and wait (synchronous), start
// it and end the turn (background), start many and answer when they are all
// back (fleet). They differ in almost everything, and they used to differ as
// branches: `if mode == background` here, another check there, a third in the
// resume path. A fourth mode meant finding every one of them, and the mode
// after that meant finding them again.
//
// So a mode is a Handoff. The loop resolves one and calls it, and does not know
// which it has. Adding one is writing a file, not auditing the branches.
type Handoff interface {
	// Mode is the handoff this implements (model.HandoffContinue and friends).
	Mode() string

	// Run carries out the delegation and says what the Gateway gets back.
	//
	// It is given everything a mode could need and no more: the turn it belongs
	// to, the tool call that asked for it, the agent, the task, and the stream.
	// A mode that does not need the stream does not use it; none of them reach
	// past this struct for context.
	Run(ctx context.Context, d Delegation) (Outcome, error)
}

// Delegation is one request to hand work to an agent: everything a mode needs
// to carry it out.
type Delegation struct {
	Turn Turn
	Call *model.ToolCall
	// Sub and Task are the ONE agent and the ONE thing it was asked to do. Every
	// mode but fleet has exactly this.
	Sub  AgentProfile
	Task string
	// Tasks is the fleet form: several agents asked for several things in one
	// call. It is set only by the fleet tool, and only the fleet handoff reads
	// it; a fleet given no list falls back to Sub and Task, which is a fleet of
	// one and costs nothing to allow.
	Tasks   []FleetTask
	Out     *chat.Stream
	Started time.Time
}

// FleetTask is one line of a fleet request: who, and what.
//
// It is what the model writes, so the agent is a key rather than a resolved
// profile; the fleet handoff resolves every one of them before it writes
// anything down.
type FleetTask struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

// Detached reports whether a mode runs the agent away from the turn that asked
// for it, so the Gateway answers now and hears back later.
//
// It is what a fleet member has to be: a member runs somewhere else and reports
// when it is done, so an agent an administrator pinned to a synchronous mode
// cannot be one, and saying so is better than quietly running it another way.
func Detached(mode string) bool {
	return mode == model.HandoffBackground || mode == model.HandoffFleet
}

// Outcome is what a mode gives back to the loop.
//
// The three fields are three genuinely different things, and keeping them apart
// is what lets the loop stay ignorant of the mode: what the MODEL sees as the
// tool result, what the TURN answers with if this ends it, and whether it does
// end it.
type Outcome struct {
	// Result is the tool result the model reads. For a synchronous handoff it
	// carries the agent's answer; for a background one it says "started", which
	// is the whole difference between waiting and not.
	Result tool.Result
	// Answer is the agent's prose, used when this delegation IS the turn's
	// answer (a terminal handoff) and ignored otherwise.
	Answer string
	// Terminal says the agent's answer is the turn's answer and the Gateway
	// says nothing more.
	Terminal bool
}

// ResolveHandoff decides which mode a delegation actually runs in, from what
// the Gateway asked for and what the agent pins.
//
// The agent's pin wins, because an administrator who set it was giving an
// instruction rather than a default (KB/27). `auto` leaves the choice with the
// Gateway, defaulting to continue: a model that says nothing about how it wants
// work done wants an answer back.
func ResolveHandoff(requested, pinned string) string {
	switch pinned {
	case model.DelegationModeBackground:
		return model.HandoffBackground
	case model.DelegationModeFleet:
		return model.HandoffFleet
	case model.DelegationModeInline:
		// Inline pins the synchronous modes. A Gateway asking for background is
		// overruled back to continue; asking for terminal is still honoured,
		// because terminal is synchronous too and it is about who answers, not
		// about where the work happens.
		if requested == model.HandoffTerminal {
			return model.HandoffTerminal
		}
		return model.HandoffContinue
	default:
		switch requested {
		case model.HandoffTerminal:
			return model.HandoffTerminal
		case model.HandoffBackground:
			return model.HandoffBackground
		case model.HandoffFleet:
			return model.HandoffFleet
		default:
			return model.HandoffContinue
		}
	}
}

// approvalsAfterRejection is what a model is told INSTEAD of a card it may not
// raise: the person just said no, and it has been handed back control to
// respond rather than to ask again.
//
// It is the only reason left. A fleet member used to have one of its own ("there
// is nobody to ask"), which was a way of avoiding the question: a worker has no
// socket, but a card is a durable row and the server has the socket, so the
// member parks and the server surfaces it like any other.
const approvalsAfterRejection = "The person just declined an action, so you cannot raise another approval this turn. " +
	"Do not retry it silently. Acknowledge their decision, and either do what you can WITHOUT this action " +
	"or ask them how they would like to proceed."
