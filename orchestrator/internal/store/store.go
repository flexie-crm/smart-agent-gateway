// Package store defines the persistence interfaces: pure interfaces here, the
// MariaDB implementation in sqlstore, tests run against both. Sub-store
// interfaces are added alongside their features, never speculatively.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"flexie.io/sag/internal/model"
)

// ErrNotFound is returned by every getter when the row does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrConflict is returned when a write violates a uniqueness rule (a
// duplicate email, group name, role name, ...).
var ErrConflict = errors.New("store: conflict")

// ErrInUse is returned when a row cannot be deleted because another row
// still depends on it (a vendor with models, for example).
var ErrInUse = errors.New("store: in use")

// ErrWrongBrain is returned when a write would carry a document into another
// brain. A brain is a closed graph: its documents relate to each other and to
// nothing outside, so a document may only move between categories of the brain
// it is in. Distinct from ErrConflict so a duplicate title and a forbidden
// move can be told apart.
var ErrWrongBrain = errors.New("store: document cannot leave its brain")

// ErrExpired is returned when a confirmation is answered too late. It is
// distinct from ErrNotFound so the user can be told the truth: the approval
// was real, it simply timed out.
var ErrExpired = errors.New("store: expired")

type Store interface {
	Ping(ctx context.Context) error
	Close() error

	Workspaces() WorkspaceStore
	Users() UserStore
	Settings() SettingStore
	Memory() MemoryStore
	Groups() GroupStore
	Roles() RoleStore
	Sessions() SessionStore
	Vendors() VendorStore
	Nodes() NodeStore
	NodeJoin() NodeJoinStore
	NodeAuthority() NodeAuthorityStore
	ModelLibrary() ModelLibraryStore
	AIModels() AIModelStore
	Attachments() AttachmentStore
	Agent() AgentStore
	OAuth() OAuthStore
	Runs() RunStore
	Stats() StatsStore
	Brains() BrainStore
	Tools() ToolStore
	Agents() AgentConfigStore
	Workflows() WorkflowStore
	MCPServers() MCPServerStore
	Jobs() JobStore
}

// JobStore is durable work: the row that says something must happen, and the
// claim that says who is doing it.
//
// The broker carries wake-ups; this carries the truth (KB/05). Every method
// here is written so that a worker dying at any point costs at most one
// duplicate delivery, never a lost job: a claim expires, an unfinished job goes
// back to pending, and a handler is expected to be idempotent on the id.
type JobStore interface {
	// Enqueue writes a pending job, minting its id and defaulting its attempts
	// and run_after. The broker signal is the CALLER's to publish, and only
	// after this returns: a wake-up for a row that is not committed yet is a
	// worker looking for work that does not exist (KB/05).
	//
	// It deliberately does not take a transaction. Where a job must commit with
	// the change that justifies it, the store method making that change writes
	// the job too, which is how this codebase already does atomic multi-writes
	// and what keeps this interface free of the implementation's types. None of
	// today's kinds needs it: a lost title job leaves a conversation a scan can
	// find, and a model pull IS its own row with nothing to commit alongside.
	Enqueue(ctx context.Context, j *model.Job) error

	// EnqueueMany writes a batch of jobs in ONE statement, the shape a dump
	// uses: one INSERT carrying every row rather than a round trip each. A
	// thousand agents is a thousand jobs, and writing them one at a time would
	// make dispatching a batch slower than running it.
	EnqueueMany(ctx context.Context, jobs []*model.Job) error

	// Claim takes up to limit runnable jobs whose subject begins with prefix,
	// marking them running under this claim. The claim string is
	// "<instance>#<token>" and must be unique per call, never reused.
	//
	// It is atomic against every other worker: a job is claimed by exactly one
	// caller, or by none. "Runnable" is decided by the DATABASE's clock, not the
	// caller's, so a node with a skewed clock cannot claim work early or hold a
	// lease longer than it was given.
	Claim(ctx context.Context, claim, subjectPrefix string, limit int) ([]*model.Job, error)

	// Heartbeat says the worker holding this claim is still alive and still
	// working, pushing its lease out. ErrNotFound means the claim is gone: the
	// job was reclaimed and is being run by somebody else, so the caller must
	// STOP rather than finish.
	//
	// Without it there is exactly one lease length for every kind of work, and
	// no length is right: sized for a title, a model pull is reclaimed and run
	// again on another node while the first is still downloading, until the
	// attempts run out and a job that is actively succeeding is marked dead.
	// Sized for the pull, a crashed title waits an hour.
	Heartbeat(ctx context.Context, id, claim string) error

	// Complete marks a claimed job done. It is scoped to the claim, so a worker
	// whose job was reclaimed underneath it cannot finish somebody else's work.
	Complete(ctx context.Context, id, claim string) error

	// Fail records an attempt that did not work. The job goes back to pending to
	// be tried after runAfter, unless its attempts are spent, in which case it
	// is left dead for a person to find.
	Fail(ctx context.Context, id, claim, reason string, runAfter time.Time) error

	// Release hands a claimed job back untouched, for a worker shutting down.
	// The attempt is given back too: being asked to stop is not a failure.
	Release(ctx context.Context, id, claim string) error

	// Reclaim returns jobs whose claim has not been heartbeaten within staleAfter
	// to pending, and reports how many. It is how a crashed worker's work is
	// picked up by another. The cutoff is measured against the database's clock.
	//
	// retryAfter delays what it releases: a job that killed its worker is
	// claimable again the instant it is released otherwise, so it takes the next
	// worker down at once and cycles through the fleet.
	Reclaim(ctx context.Context, staleAfter, retryAfter time.Duration) (int64, error)

	// Get reads one job. The console reads a download's progress through it.
	Get(ctx context.Context, id string) (*model.Job, error)
}

// BrainStore is the knowledge base: brains, their categories, their documents,
// and the graph between them.
//
// One store backs BOTH the admin console and the agent's tool. There is no second
// copy of this behind the tool, which is what stops the two drifting into
// disagreeing about what a brain is.
type BrainStore interface {
	Brains(ctx context.Context, workspaceID int64) ([]*model.Brain, error)
	Brain(ctx context.Context, workspaceID, id int64) (*model.Brain, error)
	CreateBrain(ctx context.Context, b *model.Brain) error
	UpdateBrain(ctx context.Context, b *model.Brain) error
	DeleteBrain(ctx context.Context, workspaceID, id int64) error

	Categories(ctx context.Context, workspaceID, brainID int64) ([]*model.BrainCategory, error)
	CreateCategory(ctx context.Context, workspaceID int64, c *model.BrainCategory) error
	UpdateCategory(ctx context.Context, workspaceID int64, c *model.BrainCategory) error
	DeleteCategory(ctx context.Context, workspaceID, id int64) error

	// Documents lists a category's documents WITHOUT their content: a category may
	// hold hundreds, and their bodies are Markdown nobody is reading yet.
	Documents(ctx context.Context, workspaceID, categoryID int64) ([]*model.BrainDocument, error)
	Document(ctx context.Context, workspaceID, id int64) (*model.BrainDocument, error)
	// DocumentByTitle locates a document by its title within a category, or
	// returns ErrNotFound. Titles are unique per category, so this is how a write
	// that means "save this, updating it if it already exists" finds the id to
	// update instead of colliding on the unique key.
	DocumentByTitle(ctx context.Context, workspaceID, categoryID int64, title string) (*model.BrainDocument, error)
	// SaveDocument writes a document and its links atomically. An id of zero
	// creates. The links are made symmetric, and confined to the same brain.
	SaveDocument(ctx context.Context, workspaceID int64, d *model.BrainDocument, related []int64) error
	DeleteDocument(ctx context.Context, workspaceID, id int64) error

	// Search finds documents by relevance, WITHIN the brains it is given. That
	// list is the allow-list, and an empty one finds nothing: an agent with no
	// brains assigned reaches no brains at all.
	Search(ctx context.Context, workspaceID int64, brainIDs []int64, query string, limit int) ([]model.BrainHit, error)

	// AgentBrains is the full brains an agent may read, for the tool loadout to
	// scope against. The assignment is WRITTEN by the agent config store (in the
	// same transaction as the agent's tools), so there is one writer of
	// agent_brains and this is only a read.
	AgentBrains(ctx context.Context, workspaceID, agentID int64) ([]*model.Brain, error)
}

// StatsStore answers what a workspace has done. It counts rows rather than
// reading counters: a counter is a claim about the rows and can drift from them.
type StatsStore interface {
	Usage(ctx context.Context, workspaceID int64) (*model.Usage, error)
	// WaitingApproval counts the turns stopped, asking a person to approve
	// something, right now. It is a cheap live read the dashboard's snapshot
	// leans on, separate from the heavier Usage aggregate.
	WaitingApproval(ctx context.Context, workspaceID int64) (int, error)
}

// RunStore is the durable record of a turn executing. A run outlives the
// request that asked for it, so the row is what says whether it is still going,
// and how it ended if it is not.
type RunStore interface {
	Create(ctx context.Context, r *model.Run) error
	SetStatus(ctx context.Context, id int64, status string) error
	GetByUID(ctx context.Context, workspaceID int64, uid string) (*model.Run, error)
	// MarkInterrupted closes out the runs a dead process left behind. It is
	// called at boot, when nothing can legitimately be running yet, so every
	// row that claims to be is a ghost.
	MarkInterrupted(ctx context.Context) (int64, error)
}

// ToolStore holds what an administrator decided about a tool. What a tool IS
// lives in the code registry; this is the workspace's overlay on top of it.
type ToolStore interface {
	// Sync reconciles the workspace's rows with the tools the code offers. It
	// creates rows for tools the workspace has never seen and refreshes the
	// code-owned columns (the schema, the description, the risk) on the rest,
	// while leaving the admin-owned ones (status, approval, grants) exactly as
	// they were. A deploy that renames a description must not re-enable a tool
	// somebody switched off.
	Sync(ctx context.Context, workspaceID int64, tools []*model.Tool) error

	List(ctx context.Context, workspaceID int64) ([]*model.Tool, error)
	GetByID(ctx context.Context, workspaceID, id int64) (*model.Tool, error)
	// Update writes only the admin-owned columns, and replaces the grants.
	Update(ctx context.Context, t *model.Tool) error

	// CreateCustom inserts a custom tool (kind='custom'): a self-describing row
	// (name, friendly name, description, input schema, risk) plus its template
	// and its config JSON (with secrets already sealed). It is not reconciled by
	// Sync, which only ever touches the built-in tools it is given.
	CreateCustom(ctx context.Context, t *model.Tool) error
	// UpdateCustom rewrites a custom tool's own columns (label, description,
	// input schema, risk, config), leaving status and grants to Update.
	UpdateCustom(ctx context.Context, t *model.Tool) error
	// Delete removes a custom tool row. It refuses a non-custom tool, so a
	// built-in or a projected MCP tool can never be deleted this way.
	Delete(ctx context.Context, workspaceID, id int64) error

	// ListForUser returns the tools the user is allowed to reach: active, and
	// either ungranted (open to the workspace) or granted to a group they are
	// in. Authorization is a grant, not tool metadata (KB/04). A projected
	// tool the remote no longer offers is excluded: a model must not be
	// promised a tool that cannot answer.
	ListForUser(ctx context.Context, workspaceID, userID int64) ([]*model.Tool, error)

	// ListByMCPServer returns one connection's projection, missing included.
	ListByMCPServer(ctx context.Context, workspaceID, serverID int64) ([]*model.Tool, error)
	// SyncMCPTools reconciles a connection's projection with what the remote
	// offered: new tools arrive requiring approval, tools no longer offered
	// are flagged missing (never deleted), and a changed definition takes
	// requires_approval back on, because a semantic change resets the trust
	// decision.
	SyncMCPTools(ctx context.Context, workspaceID, serverID int64, offered []*model.Tool) (model.MCPSyncResult, error)
}

// MCPServerStore is the MCP client's connection registry: the third-party
// tool servers a workspace consumes. All secrets are sealed before they get
// here and are never returned in plaintext by any query.
type MCPServerStore interface {
	Create(ctx context.Context, m *model.MCPServer) error
	GetByID(ctx context.Context, workspaceID, id int64) (*model.MCPServer, error)
	List(ctx context.Context, workspaceID int64) ([]*model.MCPServer, error)
	// Update writes the admin-owned columns; a nil secret leaves the stored
	// one alone. The tool prefix is immutable: grants hang off the names
	// under it.
	Update(ctx context.Context, m *model.MCPServer) error
	// Delete removes the connection; the cascades take its projected tools.
	Delete(ctx context.Context, workspaceID, id int64) error

	// SetOAuthClient records the client the remote authorization server
	// issued us, plus the cached discovery documents.
	SetOAuthClient(ctx context.Context, id int64, clientID string, clientSecret []byte, metadata json.RawMessage) error
	// SetOAuthMetadata refreshes only the cached discovery documents, leaving
	// the issued client and its secret untouched: a connect attempt that
	// reuses an existing client must never rewrite the credentials.
	SetOAuthMetadata(ctx context.Context, id int64, metadata json.RawMessage) error
	// SetTokens writes an obtained or refreshed token pair (gateway-owned).
	SetTokens(ctx context.Context, id int64, access, refresh []byte, expires *time.Time) error
	// SetSyncState records what the last sync did, error included.
	SetSyncState(ctx context.Context, id int64, syncedAt time.Time, lastError string) error

	// GetSettings reads our MCP server's configuration for one workspace.
	// ErrNotFound means unconfigured, which exposes the whole catalog.
	GetSettings(ctx context.Context, workspaceID int64) (*model.MCPSettings, error)
	// PutSettings writes the configuration wholesale (upsert).
	PutSettings(ctx context.Context, s *model.MCPSettings) error
}

// AgentConfigStore holds configured agents, including the workspace's default
// package (the agent whose key is model.DefaultAgentKey).
type AgentConfigStore interface {
	Create(ctx context.Context, a *model.Agent) error
	GetByID(ctx context.Context, workspaceID, id int64) (*model.Agent, error)
	// GetByKey returns store.ErrNotFound when the workspace has not configured
	// that agent. The default package is optional: its absence is a normal
	// state, not an error to log.
	GetByKey(ctx context.Context, workspaceID int64, key string) (*model.Agent, error)
	List(ctx context.Context, workspaceID int64) ([]*model.Agent, error)
	Update(ctx context.Context, a *model.Agent) error
	Delete(ctx context.Context, workspaceID, id int64) error
}

// WorkflowStore holds workflows, their immutable versions, and the conditions
// that decide who gets them.
type WorkflowStore interface {
	Create(ctx context.Context, w *model.Workflow) error
	GetByID(ctx context.Context, workspaceID, id int64) (*model.Workflow, error)
	List(ctx context.Context, workspaceID int64) ([]*model.Workflow, error)
	Update(ctx context.Context, w *model.Workflow) error
	Delete(ctx context.Context, workspaceID, id int64) error

	// CreateVersion appends a version. The version number is assigned here,
	// under the workflow's own lock, so two administrators saving at once
	// cannot produce two version 4s.
	CreateVersion(ctx context.Context, workspaceID int64, v *model.WorkflowVersion) error
	ListVersions(ctx context.Context, workspaceID, workflowID int64) ([]*model.WorkflowVersion, error)
	// Publish makes one version the live one and demotes the previous, in a
	// single transaction: there is never a moment with two published versions,
	// nor a moment with none.
	Publish(ctx context.Context, workspaceID, workflowID, versionID int64) error

	// SetAssignments replaces the workflow's conditions wholesale. Editing
	// them one row at a time would let a workspace pass through a state the
	// administrator never asked for.
	SetAssignments(ctx context.Context, workspaceID, workflowID int64, assignments []model.WorkflowAssignment) error
	ListAssignments(ctx context.Context, workspaceID, workflowID int64) ([]model.WorkflowAssignment, error)

	// Candidates returns every published workflow with a published version,
	// together with its conditions: everything the matcher needs, in one
	// query, because it runs on every turn.
	Candidates(ctx context.Context, workspaceID int64) ([]*model.WorkflowCandidate, error)
}

// ChatQuery asks for one user's conversations. Query is what a person typed;
// empty means "list them all".
type ChatQuery struct {
	WorkspaceID int64
	UserID      int64
	Query       string
	Limit       int
}

// ChatUpdate changes only what it names: a nil field is left alone, so
// renaming a chat cannot silently unpin it.
type ChatUpdate struct {
	Title    *string
	IsPinned *bool
}

// AgentStore persists everything a turn produces: the session, the
// transcript, what each tool and model call did, and any turn parked waiting
// on a human.
type AgentStore interface {
	CreateSession(ctx context.Context, s *model.AgentSession) error
	GetSession(ctx context.Context, workspaceID, id int64) (*model.AgentSession, error)
	// GetSessionByUID is how a request names a conversation: nothing from
	// outside the server arrives holding a row id.
	GetSessionByUID(ctx context.Context, workspaceID int64, uid string) (*model.AgentSession, error)
	SetSessionStatus(ctx context.Context, id int64, status string) error
	SetSessionTitle(ctx context.Context, id int64, title string) error
	// SetSessionApprovalMode switches a conversation between manual approval and
	// auto-approving for the rest of the session.
	SetSessionApprovalMode(ctx context.Context, id int64, mode string) error

	// SaveStep writes a step and everything it produced, atomically. It is an
	// upsert on (session, seq), so a checkpoint refining itself and a resumed
	// turn replaying a step both land in place rather than twice.
	SaveStep(ctx context.Context, step *model.AgentStep) error
	// MarkToolCallsInterrupted closes out every tool call still claiming to be
	// running. A boot sweep: nothing can be running in a process that has just
	// started, so such a row is a ghost of the last shutdown.
	MarkToolCallsInterrupted(ctx context.Context) (int64, error)
	// ResolveToolCall records what a call finally did. It is how a parked call
	// stops being pending, against the very row the card was showing.
	ResolveToolCall(ctx context.Context, c *model.ToolCall) error
	// ToolCall reads one finished call, so somebody opening it in the chat can
	// be shown what it was sent and what it answered.
	//
	// Scoped by workspace, like every read here: an id from another workspace is
	// not refused, it does not exist.
	ToolCall(ctx context.Context, workspaceID, id int64) (*model.ToolCall, error)
	// Transcript is the single read path for a conversation: the model-facing
	// history, the chat timeline, and a resumed turn are all built from it, so
	// they cannot disagree about what happened.
	Transcript(ctx context.Context, sessionID int64) ([]*model.AgentStep, error)
	// TranscriptPage is the same conversation as a PERSON reads it: the newest
	// rows, and older ones when they ask. before is the seq to read back from
	// (zero for the newest), rows is the budget counted in messages and tool
	// calls together. It answers whether there is more behind what it returned.
	TranscriptPage(ctx context.Context, sessionID int64, before, rows int) ([]*model.AgentStep, bool, error)
	NextSeq(ctx context.Context, sessionID int64) (int, error)

	RecordModelCall(ctx context.Context, c *model.ModelCall) error

	// ListChats powers the sidebar. An empty query lists; a query searches
	// the titles AND what was said, because a conversation is remembered by
	// what it was about.
	ListChats(ctx context.Context, q ChatQuery) ([]*model.ChatListItem, error)
	UpdateChat(ctx context.Context, workspaceID, userID int64, uid string, update ChatUpdate) error
	DeleteChat(ctx context.Context, workspaceID, userID int64, uid string) error
	// TouchChat records that something was said, which is what the list
	// sorts by.
	TouchChat(ctx context.Context, sessionID int64) error

	CreatePark(ctx context.Context, p *model.ParkSnapshot) error
	// CreateParkSequenced creates a park that waits its turn behind any live card
	// of the same session: it is inserted 'queued' (reporting live false) when a
	// pending card already exists, else 'pending' (reporting live true). It is how
	// several background agents parking at once are shown one card at a time
	// (KB/27); the check and insert share a transaction so none jumps the queue.
	CreateParkSequenced(ctx context.Context, p *model.ParkSnapshot) (bool, error)
	// ReleaseNextQueuedPark promotes a session's oldest queued park to live, so
	// answering one card surfaces the next. ErrNotFound when the queue is empty.
	ReleaseNextQueuedPark(ctx context.Context, sessionID int64) (*model.ParkSnapshot, error)
	// QueuedParks returns a session's queued parks oldest first, so "approve all"
	// can drain them without showing a card.
	QueuedParks(ctx context.Context, sessionID int64) ([]*model.ParkSnapshot, error)
	// OpenParks returns every approval a session still has outstanding, live
	// card and queue alike. Recovery needs both: they are the same thing to an
	// agent that is gone, and one session can hold more than one live park.
	OpenParks(ctx context.Context, sessionID int64) ([]*model.ParkSnapshot, error)
	// ResolveParkByID marks a park resolved by id (no token), for the queued cards
	// "approve all" drains without surfacing them.
	ResolveParkByID(ctx context.Context, parkID int64, decision string) error

	// ResolveQueuedParks answers every card a conversation still has waiting, in
	// ONE statement, and returns them so the caller can act on each.
	//
	// It is what "approve, and stop asking" means: the person said yes to all of
	// them, so all of them are answered together rather than one write and one
	// round trip at a time.
	ResolveQueuedParks(ctx context.Context, sessionID int64, decision string) ([]*model.ParkSnapshot, error)
	// ClaimPark atomically resolves a pending snapshot, so two clicks cannot
	// redeem the same approval twice. Unknown, already-resolved, and
	// expired are deliberately indistinguishable to the caller except that
	// expiry returns ErrExpired.
	ClaimPark(ctx context.Context, tokenHash, decision string) (*model.ParkSnapshot, error)
	// ReopenPark puts a claimed park back, token and all, when the resume that
	// claimed it could not start. Nothing ran, so the question stands.
	ReopenPark(ctx context.Context, parkID int64) error
	// PendingPark returns the one unresolved, unexpired snapshot a session is
	// waiting on, so a reloaded conversation can show its card again. ErrNotFound
	// when nothing is pending.
	PendingPark(ctx context.Context, sessionID int64) (*model.ParkSnapshot, error)
	// RotateParkToken replaces a snapshot's token hash. A reload cannot recover
	// the original token (only its hash was ever stored), so it mints a fresh one
	// and rebinds the card to it, keeping the "a leaked database cannot approve"
	// invariant intact.
	RotateParkToken(ctx context.Context, parkID int64, tokenHash string) error

	// CreateFleet records a batch the Gateway asked for in one call. Its members
	// are ordinary delegations carrying its id.
	CreateFleet(ctx context.Context, f *model.AgentFleet) error

	// Fleet reads one.
	Fleet(ctx context.Context, id int64) (*model.AgentFleet, error)

	// FleetTally reports how many of a fleet's members have finished, however
	// they finished, and how many were asked for.
	//
	// It is a COUNT rather than a column somebody decrements, and that is the
	// design: the members are already in the database, so the answer cannot
	// drift from the thing it describes, two reports arriving together both
	// count correctly, and a restart mid-fleet needs nothing rebuilt.
	FleetTally(ctx context.Context, id int64) (done, size int, err error)

	// CloseFleet marks a fleet finished, and reports whether THIS caller was the
	// one that did it.
	//
	// Two members reporting at the same instant will both see a full tally, and
	// exactly one of them must tell the Gateway. False means somebody else got
	// there first and this caller should do nothing.
	CloseFleet(ctx context.Context, id int64, status string) (bool, error)

	// OverdueFleets returns fleets past their deadline that are still running,
	// so a conversation is never left waiting on one member that hung.
	OverdueFleets(ctx context.Context) ([]*model.AgentFleet, error)

	// SettledFleets returns running fleets whose members have ALL finished.
	//
	// It exists because a report is a doorbell and not the record: a worker's
	// message can be lost, and the fleet would then sit complete-but-open until
	// its deadline. This is the same relationship the job scan has with the job
	// signal, and it is what makes the broker an optimisation rather than the
	// thing the join depends on.
	SettledFleets(ctx context.Context) ([]*model.AgentFleet, error)

	// SessionFleets returns a conversation's fleets, oldest first, so the chips a
	// reload draws are grouped by the batch they belong to.
	SessionFleets(ctx context.Context, sessionID int64) ([]*model.AgentFleet, error)

	// CreateFleetMembers writes a whole batch's delegations in ONE statement,
	// stamping each with its id. A thousand agents is a thousand rows, and a
	// thousand round trips to write them would make this process the bottleneck
	// rather than the database.
	CreateFleetMembers(ctx context.Context, fleetID int64, members []*model.AgentDelegation) error

	// FleetMembers returns a fleet's delegations in the order they were asked
	// for, which is the order the Gateway reads the results back in.
	FleetMembers(ctx context.Context, fleetID int64) ([]*model.AgentDelegation, error)

	// AbandonFleetMembers settles every member of a fleet that is still running,
	// and reports how many it settled. It is what a deadline does: the fleet is
	// answered with what came back, and what did not come back is recorded as
	// not having, rather than left claiming to be working forever.
	AbandonFleetMembers(ctx context.Context, fleetID int64, reason string) (int, error)

	// CreateDelegation records a background delegation as running (KB/27). It
	// stamps the id on the passed record.
	CreateDelegation(ctx context.Context, d *model.AgentDelegation) error
	// UpdateDelegationProgress refreshes the small progress JSON the chip reads,
	// only while the delegation is still running.
	// RestartDelegation puts a delegation the process died under back to
	// running for another attempt, atomically and only up to the cap.
	RestartDelegation(ctx context.Context, id int64) (bool, error)
	UpdateDelegationProgress(ctx context.Context, id int64, progress json.RawMessage) error
	// CompleteDelegation resolves a delegation to a terminal status, recording its
	// result or error and stamping completed_at, only from running.
	CompleteDelegation(ctx context.Context, id int64, status string, result json.RawMessage, errText string) error
	// SessionDelegations returns every background delegation of a session (any
	// status), for the Gateway's runtime-info tool to picture the whole set.
	SessionDelegations(ctx context.Context, sessionID int64) ([]*model.AgentDelegation, error)
	// RunningDelegations returns the delegations a session still has in flight, so
	// a reload can rebuild their chips.
	RunningDelegations(ctx context.Context, sessionID int64) ([]*model.AgentDelegation, error)
	// GetDelegation loads one delegation by id.
	GetDelegation(ctx context.Context, id int64) (*model.AgentDelegation, error)
	// AllRunningDelegations returns every still-running background delegation across
	// all sessions. A background agent is an in-process goroutine (KB/27), so a
	// restart kills it while its record still says running; called at boot, before
	// anything can legitimately run, every running row is a ghost of the last
	// shutdown. The app layer settles each and fires a completion turn, so the
	// person is told the task did not finish rather than left with a dead chip.
	AllRunningDelegations(ctx context.Context) ([]*model.AgentDelegation, error)
}

// VendorStore persists provider accounts. Credentials arrive already sealed
// by the app layer: no plaintext secret ever reaches SQL.
type VendorStore interface {
	Create(ctx context.Context, v *model.AIVendor) error
	GetByID(ctx context.Context, workspaceID, id int64) (*model.AIVendor, error)
	// ByNodeID finds this workspace's pointer at a machine, if it has one. A
	// workspace given models from a machine has exactly one (KB/35), whatever
	// how many of that machine's models it uses.
	ByNodeID(ctx context.Context, workspaceID, nodeID int64) (*model.AIVendor, error)
	List(ctx context.Context, workspaceID int64) ([]*model.AIVendor, error)
	// Update writes credentials only when v.Credentials is non-nil, so
	// editing a name cannot blank the stored secret.
	Update(ctx context.Context, v *model.AIVendor) error
	ClearCredentials(ctx context.Context, workspaceID, id int64) error
	// Delete returns ErrInUse while models still reference the vendor.
	Delete(ctx context.Context, workspaceID, id int64) error
}

// AttachmentStore keeps what somebody uploaded. The bytes are not here: they
// live in internal/filestore under the attachment's own public id.
type AttachmentStore interface {
	Create(ctx context.Context, a *model.Attachment) error
	// ByPublicID is scoped to the uploader: another user's attachment does not
	// exist as far as this is concerned.
	ByPublicID(ctx context.Context, workspaceID, userID int64, publicID string) (*model.Attachment, error)
	// ByID is the same file without the uploader check, for a turn rebuilding a
	// conversation: the message it hangs off was already established as this
	// person's, and the uploader of a file sent months ago may have left.
	ByID(ctx context.Context, workspaceID int64, publicID string) (*model.Attachment, error)
	SaveExtraction(ctx context.Context, id int64, text string) error
}

// NodeStore holds the machines that run models we own (KB/35).
//
// PLATFORM scoped, with no workspace anywhere in it: a machine is bought once
// and racked once, and every workspace on the deployment may have models on it.
// What a workspace owns is the `ai_models` row that routes to one.
type NodeStore interface {
	List(ctx context.Context) ([]*model.InferenceNode, error)
	Get(ctx context.Context, id int64) (*model.InferenceNode, error)
	// ByNodeID finds the machine that minted this id, which is how one that
	// comes back is recognised as the same machine rather than added twice.
	ByNodeID(ctx context.Context, nodeID string) (*model.InferenceNode, error)
	Create(ctx context.Context, n *model.InferenceNode) error
	Update(ctx context.Context, n *model.InferenceNode) error
	// Delete removes a machine, and with it every vendor row pointing at it and
	// every model routing through those. That is the truth: the weights were on
	// that machine.
	Delete(ctx context.Context, id int64) error
}

// NodeJoinToken is one person's unspent invitation for a machine.
type NodeJoinToken struct {
	ID        int64
	UserID    int64
	Sealed    []byte
	ExpiresAt time.Time
}

// NodeJoinStore holds the invitations a machine presents to join, one per
// person and none for long.
//
// It was one row for the deployment, held forever and read back on demand. That
// is a credential admitting a machine to the fleet with no owner, no expiry and
// no end to how often it works, and rotating it because one copy leaked took it
// out from under everybody else. One row per person, an hour, one use.
//
// SEALED rather than hashed, and this is the one credential here where that is
// not a compromise: a machine PROVES it holds the token by signing its request
// with it and never sends it, so the server has to recover the secret to
// recompute that signature. A hash cannot be an HMAC key. What stands in for "we
// cannot read it" is that there is almost never one to read.
type NodeJoinStore interface {
	// Mint writes this person's token, replacing the one they had.
	//
	// One statement, because two tabs open on the same dialog would otherwise
	// both find nothing, both insert, and one would fail on the unique key
	// believing it had minted.
	Mint(ctx context.Context, userID int64, sealed []byte, expiresAt, now time.Time) error
	// Live returns every token that has not expired.
	//
	// Every one, because the machine presenting it does not say whose it is: it
	// sends a signature, and the only way to learn which token made it is to try
	// them. Expiry is what keeps that list short.
	Live(ctx context.Context, now time.Time) ([]NodeJoinToken, error)
	// Spend removes a token and reports whether it was still there to remove.
	//
	// The boolean is the whole point: two machines started with the same token at
	// the same moment both verify, and exactly one of them deletes a row. The one
	// that deletes nothing is refused, so single-use survives a race rather than
	// depending on nobody racing.
	Spend(ctx context.Context, id int64, now time.Time) (bool, error)
	// Sweep deletes what has expired, so an abandoned invitation does not sit in
	// the table until its owner happens to ask for another.
	Sweep(ctx context.Context, now time.Time) error
}

// NodeAuthorityStore holds the ONE authority that machines and orchestrator
// authenticate each other with.
//
// One for the deployment, like the join token, and made the first time it is
// needed rather than at install: a deployment that never runs a machine of its
// own should not have a signing key sitting in it. The key is sealed; the
// certificate is not, because it is public by construction and every machine
// that joins is handed a copy.
type NodeAuthorityStore interface {
	// Get returns the sealed key and the certificate, or ErrNotFound when no
	// authority has been made yet.
	Get(ctx context.Context) (sealedKey []byte, certPEM []byte, err error)
	// Create writes the authority, and refuses if there already is one. NOT an
	// upsert: replacing it would silently orphan every machine that holds a
	// certificate from the old one, so a caller that wants that has to say so.
	Create(ctx context.Context, sealedKey, certPEM []byte) error
}

// ModelLibraryStore holds the ONE credential this deployment uses to fetch
// weights that are published under terms somebody had to accept.
//
// One row, not one per machine, because the credential belongs to a PERSON'S
// account with the library rather than to a box: the same token is what every
// machine would present, and holding it per machine means pasting it again for
// every GPU racked and rotating it in as many places.
//
// Sealed, and never readable back through any route. What a caller can ask is
// whether there IS one and who put it there, which is everything a screen needs
// and nothing an attacker can use.
type ModelLibraryStore interface {
	// Token returns the sealed credential, or ErrNotFound when none is set.
	Token(ctx context.Context) (sealed []byte, err error)
	// Describe reports whether one is set and who set it, without reading it.
	Describe(ctx context.Context) (updatedBy *int64, updatedAt time.Time, err error)
	// Set replaces it. An upsert, unlike the authority above: this is a
	// credential that expires and is meant to be rotated, and refusing to
	// replace it would be refusing the only thing anybody does with it.
	Set(ctx context.Context, sealed []byte, by int64) error
	// Clear removes it, which is how a deployment stops being able to fetch
	// gated weights at all.
	Clear(ctx context.Context) error
}

type AIModelStore interface {
	Create(ctx context.Context, m *model.AIModel) error
	GetByID(ctx context.Context, workspaceID, id int64) (*model.AIModel, error)
	List(ctx context.Context, workspaceID int64) ([]*model.AIModel, error)
	Update(ctx context.Context, m *model.AIModel) error
	Delete(ctx context.Context, workspaceID, id int64) error
}

type WorkspaceStore interface {
	Create(ctx context.Context, w *model.Workspace) error
	GetByID(ctx context.Context, id int64) (*model.Workspace, error)
	GetBySlug(ctx context.Context, slug string) (*model.Workspace, error)
	List(ctx context.Context) ([]*model.Workspace, error)
	Update(ctx context.Context, w *model.Workspace) error
	// Delete removes the workspace and, through the database's own cascades,
	// everything scoped to it: memberships, groups, roles, vendors, models,
	// agents, workflows, brains, and every conversation.
	Delete(ctx context.Context, id int64) error

	// Membership decides where a person may act. ListForUser is what the
	// switcher offers, and IsMember is what the server checks before it
	// hands out a token for a workspace: a switch is an authorization, not
	// a preference.
	ListForUser(ctx context.Context, userID int64) ([]*model.Workspace, error)
	IsMember(ctx context.Context, workspaceID, userID int64) (bool, error)
	SetMembers(ctx context.Context, userID int64, workspaceIDs []int64) error
	// AddMember grants one person one workspace, idempotently. SetMembers
	// replaces a person's whole list; this is for the cases that must not
	// (the creator of a new workspace joining it).
	AddMember(ctx context.Context, workspaceID, userID int64) error
	// AllMemberships is every membership in the tenant, keyed by user. The
	// admin console draws a workspaces column for a list of people, and one
	// query for the table beats one query per row.
	AllMemberships(ctx context.Context) (map[int64][]int64, error)
}

type UserStore interface {
	Create(ctx context.Context, u *model.User) error
	// GetByEmail takes no workspace: an email addresses a person in the
	// tenant, which is the identity they think they have.
	GetByEmail(ctx context.Context, email string) (*model.User, error)
	GetByID(ctx context.Context, id int64) (*model.User, error)
	List(ctx context.Context) ([]*model.User, error)
	Update(ctx context.Context, u *model.User) error
	UpdatePassword(ctx context.Context, userID int64, passwordHash string) error
	Delete(ctx context.Context, userID int64) error
	// EffectivePermissions resolves user -> groups -> roles -> permissions
	// and returns the distinct set. It is the single source of truth for
	// every authorization decision.
	EffectivePermissions(ctx context.Context, userID int64) ([]string, error)
}

// SettingStore keeps one person's preferences. A key means whatever the code
// that wrote it means; the store only promises to give back what it was given.
type SettingStore interface {
	Get(ctx context.Context, userID int64, key string) (string, error)
	All(ctx context.Context, userID int64) ([]*model.UserSetting, error)
	Set(ctx context.Context, userID int64, key, value string) error
	Delete(ctx context.Context, userID int64, key string) error
}

// MemoryStore holds what the assistant remembers: a person's context, and the
// workspace's working notes (its own technical memory of how to use its tools).
// A missing memory is empty, not an error: a fresh scope has nothing yet.
type MemoryStore interface {
	UserMemory(ctx context.Context, workspaceID, userID int64) (string, error)
	SetUserMemory(ctx context.Context, workspaceID, userID int64, content string) error
	WorkspaceMemory(ctx context.Context, workspaceID int64) (string, error)
	SetWorkspaceMemory(ctx context.Context, workspaceID int64, content string) error
}

type GroupStore interface {
	Create(ctx context.Context, g *model.Group) error
	GetByID(ctx context.Context, workspaceID, id int64) (*model.Group, error)
	List(ctx context.Context, workspaceID int64) ([]*model.Group, error)
	Update(ctx context.Context, g *model.Group) error
	Delete(ctx context.Context, workspaceID, id int64) error

	AddMember(ctx context.Context, groupID, userID int64) error
	RemoveMember(ctx context.Context, groupID, userID int64) error
	ListMembers(ctx context.Context, groupID int64) ([]*model.User, error)
	ListForUser(ctx context.Context, userID int64) ([]*model.Group, error)
	// SetForUser makes the person's memberships among THIS workspace's groups
	// exactly the given list, atomically. Groups of other workspaces are not
	// touched: the caller is standing in one workspace and speaks only for it.
	SetForUser(ctx context.Context, workspaceID, userID int64, groupIDs []int64) error

	AssignRole(ctx context.Context, groupID, roleID int64) error
	UnassignRole(ctx context.Context, groupID, roleID int64) error
	ListRoles(ctx context.Context, groupID int64) ([]*model.Role, error)
}

type RoleStore interface {
	// Create persists the role and its permissions atomically.
	Create(ctx context.Context, r *model.Role) error
	GetByID(ctx context.Context, workspaceID, id int64) (*model.Role, error)
	List(ctx context.Context, workspaceID int64) ([]*model.Role, error)
	// Update replaces the role name and its full permission set.
	Update(ctx context.Context, r *model.Role) error
	Delete(ctx context.Context, workspaceID, id int64) error
}

type SessionStore interface {
	Create(ctx context.Context, s *model.UserSession) error
	GetByHash(ctx context.Context, hash string) (*model.UserSession, error)
	MarkUsed(ctx context.Context, id int64) error
	Revoke(ctx context.Context, id int64) error
	RevokeAllForUser(ctx context.Context, userID int64) error
	// RevokeFamily revokes every live session in a rotation family, the
	// containment for a replayed (reused) refresh token.
	RevokeFamily(ctx context.Context, familyID string) error
	// RevokeUnusedInFamily kills every unspent token in a family, which is how
	// a rotation nobody received is cleaned up before a replacement is issued.
	RevokeUnusedInFamily(ctx context.Context, familyID string) error
	// ClaimReuseGrace consumes the one recovery a spent token is allowed and
	// reports whether this caller got it. Atomic, so racers cannot all claim it.
	ClaimReuseGrace(ctx context.Context, id int64, notBefore time.Time) (bool, error)
	// ValidateAccess re-checks live that an access token's session is still good:
	// not revoked, the user active, and the membership in the token's workspace
	// intact. It is what makes an access token revocable within its short life.
	ValidateAccess(ctx context.Context, sessionID, userID, workspaceID int64) (bool, error)
}

type OAuthStore interface {
	CreateClient(ctx context.Context, c *model.OAuthClient) error
	GetClientByClientID(ctx context.Context, clientID string) (*model.OAuthClient, error)

	// The admin surface: a workspace manages its own clients plus the
	// platform-level (DCR) ones. Deleting a client cascades its tokens,
	// which is the service token's kill switch.
	ListClients(ctx context.Context, workspaceID int64) ([]*model.OAuthClient, error)
	GetClientForWorkspace(ctx context.Context, workspaceID, id int64) (*model.OAuthClient, error)
	UpdateClient(ctx context.Context, c *model.OAuthClient) error
	DeleteClient(ctx context.Context, workspaceID, id int64) error

	InsertAuthCode(ctx context.Context, c *model.OAuthAuthCode) error
	// ConsumeAuthCode atomically deletes and returns the code row,
	// single use is enforced by the delete's row count, so a replayed
	// code can never succeed twice.
	ConsumeAuthCode(ctx context.Context, codeHash string) (*model.OAuthAuthCode, error)

	InsertAccessToken(ctx context.Context, t *model.OAuthAccessToken) error
	GetAccessTokenByHash(ctx context.Context, hash string) (*model.OAuthAccessToken, error)
	RevokeAccessToken(ctx context.Context, id int64) error

	InsertRefreshToken(ctx context.Context, t *model.OAuthRefreshToken) error
	GetRefreshTokenByHash(ctx context.Context, hash string) (*model.OAuthRefreshToken, error)
	MarkRefreshTokenUsed(ctx context.Context, id int64) error
	// RevokeFamily revokes every refresh token in the family AND every
	// access token the family minted (reuse-detection response).
	RevokeFamily(ctx context.Context, familyID string) error

	GetConsent(ctx context.Context, userID, clientPK int64) (*model.OAuthConsent, error)
	UpsertConsent(ctx context.Context, c *model.OAuthConsent) error
}
