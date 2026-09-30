package events

// The concrete events. Each is a small value carrying only what a subscriber
// needs to react; Kind is product vocabulary a subscriber matches on. Add a new
// event here and Publish it where it happens; a listener that cares subscribes
// to its Kind, and nothing else in the system has to change.

// Kinds. Kept as constants so a subscriber and a publisher cannot disagree on
// the spelling.
const (
	KindTurnStarted      = "turn.started"
	KindTurnEnded        = "turn.ended"
	KindApprovalPending  = "approval.pending"
	KindApprovalResolved = "approval.resolved"
	KindPresenceChanged  = "presence.changed"
	KindAgentStepChanged = "agent.step_changed"
	KindFleetChanged     = "fleet.changed"
)

// WorkspaceEvent is an event that belongs to a workspace. Every event here is
// one; a listener that reacts per workspace (like the live dashboard) reads the
// workspace off this without caring which concrete event it got.
type WorkspaceEvent interface {
	Event
	Workspace() int64
}

// TurnStarted announces that a turn began answering in a workspace.
type TurnStarted struct{ WorkspaceID int64 }

func (TurnStarted) Kind() string       { return KindTurnStarted }
func (e TurnStarted) Workspace() int64 { return e.WorkspaceID }

// TurnEnded announces that a turn finished (for any reason).
type TurnEnded struct{ WorkspaceID int64 }

func (TurnEnded) Kind() string       { return KindTurnEnded }
func (e TurnEnded) Workspace() int64 { return e.WorkspaceID }

// ApprovalPending announces that a turn stopped to ask a person: an approval is
// now waiting to be answered.
type ApprovalPending struct{ WorkspaceID int64 }

func (ApprovalPending) Kind() string       { return KindApprovalPending }
func (e ApprovalPending) Workspace() int64 { return e.WorkspaceID }

// ApprovalResolved announces that a waiting approval was answered (approved,
// rejected, or expired) or otherwise no longer waits.
type ApprovalResolved struct{ WorkspaceID int64 }

func (ApprovalResolved) Kind() string       { return KindApprovalResolved }
func (e ApprovalResolved) Workspace() int64 { return e.WorkspaceID }

// PresenceChanged announces that the set of people connected to a workspace
// changed (someone joined or left).
type PresenceChanged struct{ WorkspaceID int64 }

func (PresenceChanged) Kind() string       { return KindPresenceChanged }
func (e PresenceChanged) Workspace() int64 { return e.WorkspaceID }

// AgentStepChanged announces that one of a detached agent's steps was written,
// or that one of its tool calls finished. Somebody watching that agent is shown
// the step as it now stands.
type AgentStepChanged struct {
	WorkspaceID  int64
	UserID       int64
	SessionID    int64
	DelegationID int64
	StepID       int64
	// FleetID is the batch the agent is one of, zero for one on its own: a
	// member's step also moves what its batch has spent.
	FleetID int64
}

func (AgentStepChanged) Kind() string       { return KindAgentStepChanged }
func (e AgentStepChanged) Workspace() int64 { return e.WorkspaceID }

// FleetChanged announces that a batch of agents moved: one started, finished,
// stopped to ask, carried on, or the batch was stopped. Somebody watching the
// batch is shown where each of its agents now stands.
type FleetChanged struct {
	WorkspaceID int64
	UserID      int64
	FleetID     int64
}

func (FleetChanged) Kind() string       { return KindFleetChanged }
func (e FleetChanged) Workspace() int64 { return e.WorkspaceID }
