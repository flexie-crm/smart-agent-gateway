package model

import (
	"encoding/json"
	"strings"
	"time"
)

// Session statuses. A session waiting on a human is not running: nothing is
// consuming a worker, and it can be resumed hours later.
const (
	SessionRunning         = "running"
	SessionWaitingApproval = "waiting_approval"
	SessionCompleted       = "completed"
	SessionFailed          = "failed"
	SessionCancelled       = "cancelled"
)

// Agent handoff modes: how a delegation ends and where its result goes.
const (
	// HandoffContinue and HandoffTerminal are the synchronous modes
	// (defined near the delegate tool); HandoffBackground is Mode C: the
	// agent runs as a goroutine, the turn ends, and it reports back when
	// done (KB/27).
	HandoffBackground = "background"
)

// Delegation statuses. A background delegation is the only one that lives past
// the turn, so it is the only one that needs these.
const (
	DelegationRunning   = "running"
	DelegationDone      = "done"
	DelegationFailed    = "failed"
	DelegationCancelled = "cancelled"
)

// AgentDelegation is a durable record of one delegation. Synchronous delegations
// (continue/terminal) finish inside the turn and do not need it; a BACKGROUND
// delegation (Mode C, KB/27) outlives the turn, and this row is what the chip
// reads and the completion turn resolves.
type AgentDelegation struct {
	ID               int64
	SessionID        int64
	WorkspaceID      int64
	ParentToolCallID string
	AgentKey         string
	Mode             string
	// FleetID is set when this delegation is one of a batch the Gateway asked
	// for in a single call, and nil when it stands alone. It is the only thing
	// that makes a fleet member different from any other delegation.
	FleetID *int64
	// Task is the instruction the agent was given. Stored because it is the one
	// thing a killed agent cannot be started again without: everything else it
	// needs is already on the row, and the task used to live only in the
	// goroutine's arguments, so it died with the process.
	Task   string
	Status string
	// Attempts is how many times this delegation has been started, first run
	// included. It is what stops recovery becoming a loop: a task that kills the
	// process would otherwise be restarted by every boot for ever.
	Attempts int
	// Progress is the small, person-safe JSON the chip renders (activity, never
	// the agent's prose).
	Progress json.RawMessage
	// Result is the agent's technical result, threaded into the Gateway's
	// completion turn.
	Result      json.RawMessage
	ErrorText   string
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// MaxDelegationAttempts caps how many times a background agent is started again
// after the process died under it. Two: one retry is the difference between "a
// restart cost you your work" and "a restart cost you a minute", and a second
// tells you the task itself is the problem.
const MaxDelegationAttempts = 2

// Resumable reports whether this delegation should be started again rather than
// mourned. A hard kill leaves it running with its task on the row, so it can be;
// past the cap it cannot, because whatever it does may be what killed us.
func (d *AgentDelegation) Resumable() bool {
	return d.Task != "" && d.Attempts < MaxDelegationAttempts
}

// Terminal reports whether this delegation is over, however it ended.
func (d *AgentDelegation) Terminal() bool {
	return d.Status == DelegationDone || d.Status == DelegationFailed || d.Status == DelegationCancelled
}

// Waiting reports whether a still-running background delegation is currently
// blocked on a person's approval. The record status stays "running" while an
// agent is parked (a park is not terminal), so this sub-state is carried in
// the progress JSON; the chip and the Gateway's status tool read it so a reloaded
// conversation shows "waiting for approval", not a spinner.
//
// A delegation that is OVER is never waiting, whatever its progress says. The
// progress blob is a snapshot of the last thing the agent was doing, and when an
// agent dies mid-park (a restart kills the goroutine, boot recovery settles the
// row as failed) that snapshot is frozen with `waiting: true` on it forever. The
// chip then read "Waiting for your approval" for an agent that had been dead for
// hours, on a conversation whose park had long since been answered, so there was
// nothing to click and nothing to clear it. A status is a fact about the record;
// progress is a note about a goroutine that may not exist any more, and where the
// two disagree the record wins.
func (d *AgentDelegation) Waiting() bool {
	if d.Terminal() || len(d.Progress) == 0 {
		return false
	}
	var p struct {
		Waiting bool `json:"waiting"`
	}
	if err := json.Unmarshal(d.Progress, &p); err != nil {
		return false
	}
	return p.Waiting
}

// Approval modes for a conversation.
const (
	ApprovalManual = "manual"
	ApprovalAuto   = "auto"
)

// IsApprovalMode reports whether a name is an approval mode this build honours.
func IsApprovalMode(name string) bool {
	return name == ApprovalManual || name == ApprovalAuto
}

// Channels a session can arrive through. All of them enter the same runtime.
const (
	ChannelChat = "chat"
	ChannelHTTP = "http"
	ChannelMCP  = "mcp"
)

// IsChannel reports whether the name is a channel this build serves. A
// workflow assigned to a channel that does not exist would match nothing and
// say nothing about why, so the assignment is refused when it is written.
func IsChannel(name string) bool {
	switch name {
	case ChannelChat, ChannelHTTP, ChannelMCP:
		return true
	default:
		return false
	}
}

type AgentSession struct {
	ID int64
	// UID is the identifier the outside world uses. The numeric id never
	// leaves the server: a sequential id tells whoever holds one how many
	// conversations exist, and invites them to walk the range.
	UID               string
	WorkspaceID       int64
	UserID            int64
	AgentID           *int64
	WorkflowVersionID *int64
	ParentSessionID   *int64
	Channel           string
	ExternalKey       string
	Title             string
	Status            string
	// ApprovalMode is manual (ask every time, the default) or auto (the person
	// let this conversation run approval-gated actions without a card, for the
	// Gateway and every agent it starts). It is per session and reversible.
	ApprovalMode string
	// IsPinned, LastMessageAt and MessageCount belong to the chat list: it
	// sorts by them, so they are stored rather than derived on every read.
	IsPinned      bool
	LastMessageAt *time.Time
	MessageCount  int
	StartedAt     time.Time
	CompletedAt   *time.Time
}

// Transcript roles. These are the durable record; the provider layer maps
// them onto whatever each vendor wants.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// A transcript is an ordered list of STEPS.
//
// One assistant turn is several steps: the model thinks, calls tools, thinks
// again, calls more, and eventually answers. Reasoning and tool calls are
// properties of a step, which is why they are not columns on a message: a
// single reasoning field and a single tool_call_id could hold exactly one of
// each, and that is the case that never happens.
//
// A step is either the user speaking or the assistant working. An assistant
// step carries whatever it produced: text, or thinking, or tool calls, or all
// three. A step with tool calls and no text is a model that acted without
// narrating, which is normal and which the old shape could not represent.
type AgentStep struct {
	ID        int64
	SessionID int64
	Seq       int
	Kind      string
	Vendor    string
	Model     string
	// Partial marks a step still being streamed. The checkpoint writes it while
	// the answer is arriving and clears it when the step commits, so a process
	// that dies mid-answer leaves behind the text it had rather than nothing.
	Partial   bool
	CreatedAt time.Time

	// AgentKey and ParentToolCallID attribute a step to a delegation. Both
	// empty is a Gateway step, on the conversation's own timeline. Both set is a
	// agent's inner step: AgentKey is which agent produced it, and
	// ParentToolCallID is the Gateway's `delegate` call it runs under. The Gateway's
	// model transcript excludes these (the delegation is represented by the
	// `delegate` call and its result); the agent's own loop is rebuilt from
	// them on resume.
	AgentKey         string
	ParentToolCallID string

	// Attachments are the public ids of the files sent WITH this message, on a
	// user step. Ids rather than the account of what they contain: that account
	// lives on the attachment, written the one time the file was read, so a
	// conversation twenty turns long still calls the reading model once. The
	// person's own words stay their own here, which is what the chat shows.
	Attachments []string

	// Text is what the step said, if it said anything.
	Text string
	// Reasoning is what the model thought during this step.
	Reasoning string
	// ToolCalls are the calls this step made, with their results.
	ToolCalls []*ToolCall
}

// Step kinds.
const (
	StepUser      = "user"
	StepAssistant = "assistant"
)

// HasText reports whether the step produced a message row.
// HasText is whether this step actually SAID something.
//
// Trimmed, and that is the whole of a real defect. A model that is about to call
// a tool often emits "\n\n" as its text, and `s.Text != ""` is true of two
// newlines, so a message row was written holding nothing: 49 of them in one
// installation. The transcript then served them as messages, every reader had to
// know to skip them, and the chat drew each one as a 730x0 div that cost 24px of
// blank space between the rows either side of it.
//
// Every caller wants this same meaning: do not save a step that only produced
// whitespace, do not write a message row for it, and do not move a conversation
// up the list because a model pressed return twice.
func (s *AgentStep) HasText() bool { return strings.TrimSpace(s.Text) != "" }

// Tool call statuses.
const (
	ToolCallApprovalRequired = "approval_required"
	ToolCallRunning          = "running"
	ToolCallCompleted        = "completed"
	ToolCallFailed           = "failed"
	ToolCallRejected         = "rejected"
)

// ToolCall is one call and its result. The two live together because they are
// two halves of one fact: the result is not a message somebody wrote, it is
// what this call returned. The model-facing role=tool message is synthesized
// when the transcript is rebuilt, from here, so there is one copy and one
// owner.
type ToolCall struct {
	ID           int64
	StepID       int64
	SessionID    int64
	WorkspaceID  int64
	ToolCallID   string
	ToolName     string
	FriendlyName string
	Args         json.RawMessage
	Result       json.RawMessage
	Status       string
	ErrorText    string
	DurationMS   int64
	// ExecutionOrder orders the parallel calls of one step.
	ExecutionOrder int
	// RequestedApproval is sticky: a call that went through a confirmation card
	// says so forever, including after the approved call re-persists its real
	// result.
	RequestedApproval bool
	CreatedAt         time.Time
	CompletedAt       *time.Time
}

// Resolved reports whether the call has an outcome the model can be shown. A
// call still waiting on a person has not.
func (c *ToolCall) Resolved() bool {
	return c.Status != ToolCallApprovalRequired && c.Status != ToolCallRunning
}

// ModelCall records what a turn cost. Raw token counts are stored, never a
// precomputed price: cost is derived against the model's pricing row, which
// can change.
type ModelCall struct {
	ID           int64
	WorkspaceID  int64
	SessionID    int64
	ModelID      int64
	InputTokens  int64
	OutputTokens int64
	DurationMS   int64
	Status       string
	ErrorText    string
	CreatedAt    time.Time
}

// Park statuses. A background agent's card waits as Queued behind a live
// (Pending) one, so several finishing at once are answered one at a time (KB/27);
// it is released to Pending when the card before it is answered.
const (
	ParkQueued   = "queued"
	ParkPending  = "pending"
	ParkApproved = "approved"
	ParkRejected = "rejected"
	ParkExpired  = "expired"
)

// ParkSnapshot is a PREPARED CALL waiting on a person.
//
// The agent works out exactly what it is about to do, writes it down here with
// everything needed to run it, and only then shows the card. That order is the
// whole contract: an approved action must always be redeemable, so it is
// validated before anyone is asked (KB/02, park-and-resume).
//
// It does NOT carry the conversation. A frozen copy of the transcript would be
// a duplicate of rows that already exist and would silently drift from them.
// The conversation is rebuilt from the steps on resume, which is also what
// re-resolves the profile and the permissions live: revoking a tool while the
// card is on screen revokes it, and approving is not a way around that.
type ParkSnapshot struct {
	ID          int64
	TokenHash   string
	WorkspaceID int64
	SessionID   int64
	UserID      int64
	AgentID     *int64
	ModelID     int64
	// The prepared call: what will run, unchanged, the moment a person says yes.
	ToolName   string
	ToolCallID string
	ToolArgs   json.RawMessage
	// ActionHash is hash(tool name + canonical arguments). It is what "this
	// exact action was approved" means, and it is why an approval cannot be
	// replayed against a different call.
	ActionHash string
	// DefinitionHash pins WHAT the tool was when the card was shown. Set for
	// projected MCP tools: if the remote redefines the tool while the card
	// waits, the approval is stale and the resume refuses, because nobody
	// approved the new semantics.
	DefinitionHash string
	// AgentKey, ParentToolCallID, and HandoffMode point at the delegation a
	// parked call belongs to. All empty is a Gateway park: the call is the Gateway's
	// own, and resume runs it in the Gateway's loop. All set is an agent park:
	// the call is an agent's, ParentToolCallID is the Gateway's `delegate` call
	// it runs under, and HandoffMode is the handoff (continue/terminal) that
	// decides who speaks when the delegation finishes. The conversation is NOT
	// here: it is rebuilt from the durable transcript (KB/16).
	AgentKey         string
	ParentToolCallID string
	// DelegationID is which agent raised this card, when one did.
	//
	// The parent tool call used to be enough, and for a background agent it
	// still is: it has a `delegate` call of its own. A fleet's members share
	// ONE call, so without this a card cannot be matched back to the member
	// that raised it, and an approved action could be run against another
	// member's task.
	DelegationID *int64
	HandoffMode  string
	Status       string
	ExpiresAt    time.Time
	CreatedAt    time.Time
	ResolvedAt   *time.Time
}

// ChatListItem is a conversation as the sidebar shows it.
type ChatListItem struct {
	ID            int64
	UID           string
	Title         string
	IsPinned      bool
	LastMessageAt *time.Time
	MessageCount  int
}

// DefaultApprovalTTL is how long a confirmation stays answerable when nothing
// says otherwise: long enough that a user can step away, short enough that an
// approval cannot be redeemed days later against a world that has moved on.
//
// It is a default, not a rule. An assistant that files a ticket and one that
// moves money do not want the same window, and the configuration model is where
// that is said (KB/15).
const DefaultApprovalTTL = 24 * time.Hour

// The window an administrator may choose. A minute is the shortest that is not
// a trap for a person who looked away; a week is the longest that still means
// anything, because an approval older than that is a decision about a different
// world.
const (
	MinApprovalTTL = time.Minute
	MaxApprovalTTL = 7 * 24 * time.Hour
)

// ValidApprovalTTL reports whether a configured window is one we will honour.
func ValidApprovalTTL(ttl time.Duration) bool {
	return ttl >= MinApprovalTTL && ttl <= MaxApprovalTTL
}

// Bounds an administrator may set for the two run limits. One iteration is a
// pointless assistant; a thousand is well past any real task and into runaway.
// Half a minute is the least a background leg can do useful work in; an hour is
// the most we let one run before it is treated as stuck.
const (
	MinIterationLimit    = 1
	MaxIterationLimit    = 1000
	MinBackgroundTimeout = 30 * time.Second
	MaxBackgroundTimeout = time.Hour
)

// How many agents one batch may hold.
//
// Twenty is the default and not a law. What a batch costs depends entirely on
// where the models run: against a metered vendor twenty conversations at once is
// a bill and a rate limit, and on a farm of your own machines running your own
// models it is a Tuesday. So an administrator sets it, and this is only what
// they get if they do not.
//
// The floor is two, because a batch of one is a delegation and there is already
// a tool for that. The ceiling is a thousand, which is not a guess at when
// somebody has made a mistake: it is high enough that the limit is the hardware
// rather than this number, and what gives out first is the database and the
// queue, both of which can be given more.
const (
	DefaultMaxFleetAgents = 20
	MinFleetAgents        = 2
	MaxFleetAgents        = 1000
)

// ValidMaxFleetAgents reports whether a configured batch size is one we honour.
func ValidMaxFleetAgents(n int) bool {
	return n >= MinFleetAgents && n <= MaxFleetAgents
}

// ValidMaxIterations reports whether a configured tool-loop cap is one we honour.
func ValidMaxIterations(n int) bool {
	return n >= MinIterationLimit && n <= MaxIterationLimit
}

// ValidBackgroundTimeout reports whether a configured background time limit is
// one we honour.
func ValidBackgroundTimeout(d time.Duration) bool {
	return d >= MinBackgroundTimeout && d <= MaxBackgroundTimeout
}

// Run statuses.
const (
	RunRunning         = "running"
	RunCompleted       = "completed"
	RunFailed          = "failed"
	RunWaitingApproval = "waiting_approval"
	RunCancelled       = "cancelled"
	// RunInterrupted is a run whose process died. The conversation keeps what
	// the checkpoint saved, and the run says what happened rather than leaving
	// a row that claims to be running forever.
	RunInterrupted = "interrupted"
)

// Run is one turn executing, with a life of its own.
//
// It outlives the request that asked for it. The reader hanging up does not
// cancel the work, which is the whole reason this exists: an answer half
// written when a laptop closed is still an answer, and it is still being
// written when the laptop comes back.
type Run struct {
	ID          int64
	UID         string
	WorkspaceID int64
	SessionID   int64
	UserID      int64
	Status      string
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// --- what the workspace is doing, and what it has done ---------------------------

// Usage is the dashboard's answer. Every number in it is counted from the rows
// that recorded it: a counter kept alongside the rows is a claim about them, and
// a claim can drift.
type Usage struct {
	// WaitingApproval is how many turns are stopped, asking a person. They are
	// not running: they hold nothing and can sit for hours.
	WaitingApproval int
	Today           Period
	Total           Period
	Models          []ModelUsage
	Tools           []ToolUsage
	Configured      Configured
}

type Period struct {
	Runs         int64
	Failed       int64
	InputTokens  int64
	OutputTokens int64
	ToolCalls    int64
	// Refused is tool calls a person declined. Like ToolUsage, it is kept apart
	// from Failed: a refusal is the approval system doing its job, not a fault.
	Refused int64
}

type ModelUsage struct {
	ModelID      int64
	ModelKey     string
	VendorName   string
	Calls        int64
	InputTokens  int64
	OutputTokens int64
}

// ToolUsage separates a tool that BROKE from one a person REFUSED. A refusal is
// not a failure: it is the approval system doing its job, and a dashboard that
// counts them together teaches people to ignore both.
type ToolUsage struct {
	Name string
	// FriendlyName is the tool's current display name (from the tools registry),
	// falling back to Name when the tool is no longer registered. Name stays the
	// stable alias, used as a key; FriendlyName is what a person reads.
	FriendlyName string
	Calls        int64
	Failed       int64
	Refused      int64
}

// Configured is what the workspace has set up, so an empty dashboard can say why
// it is empty rather than just being blank.
type Configured struct {
	Vendors   int
	Models    int
	Agents    int
	Tools     int
	Workflows int
}

// A fleet is N agents asked to do N things, answered once, when every one of
// them is back.
//
// Its members are ordinary AgentDelegation rows carrying its id. That is
// deliberate rather than convenient: a fleet member is still a delegation, so
// it keeps the chip in the chat, the history on reload, the cancel path and the
// boot recovery without any of them learning a new idea.
//
// What the fleet row itself holds is only what a delegation cannot: how many
// were asked for, and whether somebody has already told the Gateway. There is
// no count of how many have come back, and there must not be: the terminal
// members are in the delegations table, so asking is exact, cannot drift from
// the thing it describes, and survives a restart with nothing to rebuild.
type AgentFleet struct {
	ID               int64
	SessionID        int64
	WorkspaceID      int64
	ParentToolCallID string
	// ModelID is the model the Gateway was running on when it asked for this
	// batch, so the completion turn runs on the same one even if the process
	// that started the fleet is gone. Zero when it was not recorded.
	ModelID int64
	// Size is how many were asked for. The fleet is finished when that many
	// members are terminal, however they ended.
	Size   int
	Status string
	// Deadline is when the Gateway is told what came back regardless. Nineteen
	// results and one that hung should end as nineteen results and a note, not
	// as a conversation waiting forever.
	Deadline    time.Time
	CreatedAt   time.Time
	CompletedAt *time.Time
}

const (
	FleetRunning   = "running"
	FleetDone      = "done"
	FleetCancelled = "cancelled"
)

// FleetDeadline is how long a batch may WORK before what has not come back is
// written off.
//
// It is the longest of its members' own configured time limits, not a number of
// its own. Every agent already carries what an administrator decided one leg of
// it may take (BackgroundTimeout, default ten minutes, 30s to an hour); a batch
// is over when the slowest thing in it would have been stopped anyway, so a
// second constant here could only ever disagree with the first.
//
// It bounds WORK, and only work. A member waiting on a person is not working,
// and its card stays answerable for a day (DefaultApprovalTTL), so a batch with
// a card on screen is never overdue: somebody at lunch must not cost a colleague
// their answers.
func FleetDeadline(timeouts []time.Duration) time.Duration {
	longest := time.Duration(0)
	for _, d := range timeouts {
		if d <= 0 {
			d = DefaultBackgroundTimeout
		}
		if d > longest {
			longest = d
		}
	}
	if longest <= 0 {
		return DefaultBackgroundTimeout
	}
	return longest
}
