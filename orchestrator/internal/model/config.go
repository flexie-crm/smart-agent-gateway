package model

import (
	"encoding/json"
	"strings"
	"time"
)

// The layered configuration model (KB/05).
//
// A turn is shaped by four layers, each overriding only the fields it has an
// opinion about:
//
//	code defaults  ->  the workspace default agent  ->  the winning workflow
//
// The product works with none of them configured, which is the point: a
// workspace with a vendor and a model can chat. Everything below is an
// override, never a prerequisite.

// DefaultAgentKey is the reserved key of the workspace's default agent: the
// "default package" of the configuration model. It is optional. When it is
// absent the code defaults apply, and a workspace that never opens the admin
// UI still works.
const DefaultAgentKey = "default"

// The Gateway (the default agent) delegates to an agent through one built-in
// tool. The agent is named by its key; the Gateway chooses the handoff mode
// per call.
const (
	// DelegateToolName is the tool the Gateway calls to route a task to a
	// agent. It is never given to an agent, which is what keeps
	// delegation a single level deep.
	DelegateToolName = "delegate"
	// FleetToolName is the tool the Gateway calls to start SEVERAL agents at
	// once and be answered when all of them are back. It is a second tool
	// rather than a mode of the first because it takes a list, and a schema
	// that says "either these two fields or that list" is one a model gets
	// wrong.
	FleetToolName = "delegate_fleet"
	// HandoffTerminal makes the agent's answer the turn's answer: it
	// streams to the person and the Gateway does not speak again.
	HandoffTerminal = "terminal"
	// HandoffContinue hands the agent's result back to the Gateway,
	// which reads it and answers itself.
	HandoffContinue = "continue"

	// HandoffFleet is Mode B: several agents are started at once on the
	// workers, and the Gateway is answered when every one of them is back.
	HandoffFleet = "fleet"
)

// FileRule is one line of the Gateway's file routing: the types it covers and
// the model that reads them. Types are lower case and without dots ("pdf",
// "docx"); an empty list matches every file, which is what makes one rule mean
// "one model for everything".
type FileRule struct {
	Types   []string `json:"types"`
	ModelID int64    `json:"model_id"`
}

// Matches reports whether this rule covers a file type. A rule with no types
// covers everything, which is how a catch-all is written.
func (r FileRule) Matches(fileType string) bool {
	if len(r.Types) == 0 {
		return true
	}
	for _, t := range r.Types {
		if strings.EqualFold(t, fileType) {
			return true
		}
	}
	return false
}

// Agent delegation modes: how an agent pins the way the Gateway runs it (KB/27).
// Auto leaves the choice to the Gateway; Background pins Mode C; Inline pins the
// synchronous modes (continue/terminal). A pinned agent takes no mode from
// the Gateway: the delegate tool stops offering one for it.
const (
	DelegationModeAuto       = "auto"
	DelegationModeBackground = "background"
	DelegationModeInline     = "inline"
	// DelegationModeFleet says this agent runs on a WORKER rather than in this
	// process, and that the Gateway asks for several of it at once: a list of
	// tasks in one call, answered when every one of them is back.
	//
	// It is a statement about where the work happens, like the other three are
	// statements about how. What makes it worth pinning per agent is that some
	// agents are worth fanning out and some are not, and the Gateway is told
	// which is which rather than guessing from the task.
	DelegationModeFleet = "fleet"
)

// The code defaults for the two per-agent run bounds, used when an agent leaves
// them unset (NULL). DefaultMaxIterations matches the CRM's tool-loop cap.
const (
	DefaultMaxIterations = 100
	// HistoryRows is how much of a conversation a person is given at a time,
	// counted in ROWS: a message or a tool call, because both are a line on the
	// screen and a piece of the page. The rest arrives as they scroll back for
	// it. Sending the whole thing on every open is a payload that grows without
	// end, and about half the cost of typing a character with it on screen.
	HistoryRows              = 100
	DefaultBackgroundTimeout = 10 * time.Minute
)

// BackgroundStatusToolName is the Gateway's read-only tool for looking at the
// background agents it started (KB/27), one by id or the whole picture. The
// turn loop caps how often it may be called, so a model that ignores "you rarely
// need this" cannot loop on it.
const BackgroundStatusToolName = "background_status"

// MemoryToolName is the tool a Gateway calls to flag that a turn is worth
// remembering. It is a signal, not a write: calling it does no work in the turn
// (the actual note is distilled in the background, off the stream), so the model
// can flag freely without slowing an answer down.
const MemoryToolName = "remember"

// Agent is a configured assistant: what it is told, what it may use, and how
// it thinks. Every field is optional except the key and the name, because an
// agent is an override of the defaults, not a replacement for them.
type Agent struct {
	ID           int64  `json:"id"`
	WorkspaceID  int64  `json:"workspace_id"`
	Key          string `json:"key"`
	Name         string `json:"name"`
	Instructions string `json:"instructions,omitempty"`
	ModelID      *int64 `json:"model_id,omitempty"`
	Reasoning    bool   `json:"reasoning"`
	// Settings are chosen values for what this agent's vendor declares, most
	// notably how hard to think.
	//
	// It lives HERE and not on the model because how hard to think is a property
	// of the job, not of the weights: one model serves a classifier and a
	// researcher, and they do not want the same answer. That is the layered
	// order the product already runs on (KB/15), where the narrower decision
	// wins, and the agent is the narrower one. A model may still carry a bag as
	// a default for anything nobody chose.
	Settings Settings `json:"settings,omitempty"`
	Status   string   `json:"status"`
	// DelegationMode pins how the Gateway runs this agent (auto|background|
	// inline). Empty is treated as auto. Only meaningful for an agent, not the
	// main agent.
	DelegationMode string    `json:"delegation_mode,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	// FileRules is which model reads which uploaded file, in order: the first
	// rule whose types match wins, and a rule with no types is the catch-all. A
	// person hands over a screenshot or a contract, and neither is a prompt, so
	// something turns it into text before the Gateway thinks about it.
	//
	// No rules means no file can be uploaded at all, and the chat reads that
	// absence rather than being told separately. A nil slice means no opinion
	// and leaves what is stored alone; an empty slice clears it.
	FileRules []FileRule `json:"file_rules,omitempty"`

	// AudioModelID is the model that turns a recording into words, so somebody
	// can talk instead of typing. Nil means the Gateway takes no audio. It is
	// separate from FileRules because reading audio is transcription, a
	// different call to a different kind of model.
	AudioModelID *int64 `json:"audio_model_id,omitempty"`

	// Brains lists the knowledge bases this agent may read (the ids of the
	// assigned brains). A nil slice means no opinion; the resolver scopes the
	// brain tool to exactly this set, so a brain nobody assigned is one no agent
	// can reach.
	Brains []int64 `json:"brains"`

	// MemoryBrainID is the one brain this agent writes back to as its long-term
	// memory, off the stream, on its own model. Nil means the agent keeps no
	// long-term memory, which is the normal state. It must be an unlocked brain,
	// enforced where it is set.
	MemoryBrainID *int64 `json:"memory_brain_id,omitempty"`

	// ApprovalTTL is how long this agent's confirmations stay answerable. Nil
	// means it has no opinion and inherits the deployment's.
	ApprovalTTL *time.Duration `json:"-"`

	// MaxIterations bounds this agent's tool loop: how many model round-trips it
	// may take before it must stop. Nil inherits the code default
	// (DefaultMaxIterations). Applies to the main agent and agents alike.
	MaxIterations *int `json:"-"`
	// MaxFleetAgents bounds how many agents this agent may start in ONE batch.
	// Only the Gateway starts batches, so only the Gateway's is read. Nil
	// inherits DefaultMaxFleetAgents.
	MaxFleetAgents *int `json:"-"`

	// BackgroundTimeout bounds one working leg of a background agent (KB/27).
	// If its goroutine runs past this, the run is cancelled and the Gateway is told
	// it timed out. Nil inherits the code default (DefaultBackgroundTimeout). Only
	// meaningful for an agent, the only thing that runs in the background.
	BackgroundTimeout *time.Duration `json:"-"`

	// Tools names the tools this agent may use, explicitly. An empty slice means
	// it deliberately has none (an assistant that only talks); the store returns
	// a non-nil slice for a configured agent, so a main agent with nothing
	// selected reaches the model with nothing rather than inheriting the code
	// defaults. Nil is "no opinion" and only occurs on an agent built in code
	// that said nothing about tools; the resolver leaves the layer below then.
	Tools []string `json:"tools"`

	// ConfirmTools names the tools this agent must stop and confirm before
	// running. It is the admin-added friction that used to live on the tool
	// row: "which of my tools are worth a pause" is a decision about an
	// assistant, not about a tool. It is a subset of Tools (gating a tool the
	// agent cannot call is meaningless), and it only ADDS confirmation: a tool
	// the code declares dangerous stays dangerous whether or not it is listed
	// here. Approval resolves as (code OR MCP drift OR this).
	ConfirmTools []string `json:"confirm_tools"`
}

// Tool is the workspace's row for a tool. The registry in code owns what a
// tool IS (its name, its schema, how dangerous it is). This row owns what the
// admin decides ABOUT it: whether it is on, whether it needs a human, and who
// may reach it.
type Tool struct {
	ID          int64  `json:"id"`
	WorkspaceID int64  `json:"workspace_id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	// Template names the code recipe a native custom tool was instantiated from
	// (e.g. "query"). Empty for a built-in, an MCP tool, or a workflow-backed
	// custom tool. It is how the loadout finds the handler and the deep guide for
	// a custom tool whose behaviour lives in code.
	Template     string          `json:"template,omitempty"`
	FriendlyName string          `json:"friendly_name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	Risk         string          `json:"risk"`

	// RequiresApproval is the tool row's own approval bit. It is the builtin's
	// code floor (written by Sync) and, for an MCP tool, the sync's drift lock
	// that an admin may re-trust. It is NOT a general admin toggle: for a builtin
	// or custom tool the admin cannot add friction here. That decision moved to
	// the agent (Agent.ConfirmTools), because "which tools are worth a pause" is
	// a decision about an assistant. Approval resolves as (code floor OR this OR
	// the agent asked for it), never taking a floor away.
	RequiresApproval bool `json:"requires_approval"`

	// Config is a custom tool's full configuration as JSON, including the
	// connection auth (for a query tool: driver, host, port, database, and the
	// credentials). Secret values inside it are sealed field by field, so the
	// column round-trips as valid JSON while a password never sits in it in the
	// clear.
	Config json.RawMessage `json:"config,omitempty"`
	// Guide is a custom tool's own AI guide, prefilled from its template and then
	// editable per instance. Empty means the template's guide is used.
	Guide  string `json:"guide,omitempty"`
	Status string `json:"status"`

	// Grants lists the groups allowed to use the tool. Empty means everyone
	// in the workspace, so a fresh workspace works; adding the first grant is
	// what turns the list exclusive.
	Grants []int64 `json:"grants"`

	// The projection of a remote MCP tool (kind 'mcp'): which connection
	// offers it, what the remote calls it, a hash of its definition so drift
	// is a visible event, and whether the last sync still saw it. Zero on
	// every other kind.
	MCPServerID         *int64     `json:"mcp_server_id,omitempty"`
	RemoteName          string     `json:"remote_name,omitempty"`
	DefinitionHash      string     `json:"definition_hash,omitempty"`
	RemoteMissing       bool       `json:"remote_missing,omitempty"`
	DefinitionChangedAt *time.Time `json:"definition_changed_at,omitempty"`
}

// Workflow statuses. Only a published workflow can shape a turn: a draft is
// someone thinking out loud, and it must not reach a user.
const (
	WorkflowDraft     = "draft"
	WorkflowPublished = "published"
	WorkflowArchived  = "archived"
)

type Workflow struct {
	ID          int64     `json:"id"`
	WorkspaceID int64     `json:"workspace_id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	CreatedBy   int64     `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// WorkflowVersion is an immutable definition. Editing a published workflow
// creates a new version rather than mutating the running one, so a turn that
// is mid-flight keeps the configuration it started with and an audit row can
// name exactly what shaped an answer.
type WorkflowVersion struct {
	ID          int64           `json:"id"`
	WorkflowID  int64           `json:"workflow_id"`
	Version     int             `json:"version"`
	Definition  json.RawMessage `json:"definition"`
	IsPublished bool            `json:"is_published"`
	CreatedBy   int64           `json:"created_by"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Match types: the dimensions a workflow can be conditioned on.
const (
	MatchUser    = "user"
	MatchGroup   = "group"
	MatchRole    = "role"
	MatchChannel = "channel"
)

// WorkflowAssignment is one condition. A workflow with no assignments applies
// to nobody: publishing must never silently capture a whole workspace.
type WorkflowAssignment struct {
	ID         int64  `json:"id"`
	WorkflowID int64  `json:"workflow_id"`
	MatchType  string `json:"match_type"`
	MatchValue string `json:"match_value"`
	Priority   int    `json:"priority"`
}

// WorkflowCandidate is a published workflow with its published definition and
// its conditions: everything the matcher needs to decide whether this workflow
// shapes this turn, loaded in one query because that decision is made on every
// message.
type WorkflowCandidate struct {
	WorkflowID  int64
	VersionID   int64
	Definition  json.RawMessage
	Assignments []WorkflowAssignment
}

// Definition is the content of a workflow version. Kind discriminates, so the
// canvas can land later as another kind rather than as a schema migration of
// this one.
type Definition struct {
	Kind    string           `json:"kind"`
	Profile *ProfileOverride `json:"profile,omitempty"`
}

// DefinitionProfile is the only kind Phase 1 understands: a set of overrides
// on the default package.
const DefinitionProfile = "profile"

// ProfileOverride is a partial configuration. Every field is a pointer,
// because "the workflow does not care about the model" and "the workflow
// wants no tools at all" are different instructions and a zero value cannot
// tell them apart.
type ProfileOverride struct {
	ModelID      *int64    `json:"model_id,omitempty"`
	SystemPrompt *string   `json:"system_prompt,omitempty"`
	Reasoning    *bool     `json:"reasoning,omitempty"`
	Tools        *[]string `json:"tools,omitempty"`
	// ApprovalTTLSeconds is how long this workflow's confirmations stay
	// answerable. Seconds, and not a duration string, because a string in a
	// stored document is a parser waiting to fail on a row nobody validated.
	ApprovalTTLSeconds *int `json:"approval_ttl_seconds,omitempty"`
}

// Profile is the resolved configuration for one turn: what the layers agreed
// on, with nothing left to decide.
type Profile struct {
	ModelID int64
	// SystemPrompt is the fully assembled instruction the model runs on,
	// built at the end of resolution from the structured base (identity,
	// capabilities, communication rules), the live context (the person, the
	// memory, the agents), and Instructions appended at the end.
	SystemPrompt string
	// Instructions is what an administrator or workflow wrote for this
	// assistant, and nothing else. It is APPENDED to the base, never a
	// replacement for it: the house prompt shapes the assistant, it does not
	// get to drop the rules that keep it safe and in character.
	Instructions string
	Reasoning    bool
	// Settings are the resolved agent's own chosen values, carried so the turn
	// can hand them to the gateway as the nearest bag in the layered order.
	Settings Settings
	Tools    []string
	// ConfirmTools names the tools this turn must stop and confirm before
	// running, over and above the code floor and any MCP drift lock. It comes
	// from the resolved agent (and later a workflow), the same way Tools does.
	ConfirmTools []string
	// ApprovalTTL is how long a confirmation raised by this turn stays
	// answerable.
	ApprovalTTL time.Duration
	// MaxIterations bounds this turn's tool loop. Zero falls back to the code
	// default (DefaultMaxIterations) in the loop.
	MaxIterations int
	// MaxFleetAgents is how many agents this Gateway may start in one batch.
	// Zero means the code default; only the Gateway's is ever read, because
	// only the Gateway starts a batch.
	MaxFleetAgents int

	// ModelPinned means a configured layer chose the model, so the caller's
	// preference does not apply. A workflow that pins a model is an
	// instruction, not a suggestion: the picker in the chat UI cannot
	// override what an administrator decided this conversation runs on.
	ModelPinned bool

	// AgentID and WorkflowVersionID record what shaped this turn. They are
	// written onto the session, so an answer can always be traced back to the
	// configuration that produced it.
	AgentID           *int64
	WorkflowVersionID *int64

	// Brains are the ids of the knowledge bases this turn's agent may reach, and
	// MemoryBrainID the one brain it manages as its own long-term memory. They are
	// resolved from the agent and scope the brain tools: the read and write
	// handlers are bound over exactly these ids, so a brain nobody assigned is one
	// no turn can touch.
	Brains        []int64
	MemoryBrainID *int64
}

// Attachment is a file somebody uploaded, before or alongside the message that
// carries it.
//
// FileName is kept for showing back and is never used to decide where anything
// is written: the bytes live under PublicID (KB/31, internal/filestore).
// FileType is the lower-case extension, without a dot, and is what the
// Gateway's file rules match on.
//
// Extraction is what the routed model made of the file: the text the Gateway
// actually reads. It is stored rather than recomputed, so the same PDF is not
// re-read on every turn at a different price and a different answer.
type Attachment struct {
	ID          int64      `json:"-"`
	PublicID    string     `json:"id"`
	WorkspaceID int64      `json:"-"`
	UserID      int64      `json:"-"`
	FileName    string     `json:"file_name"`
	FileType    string     `json:"file_type"`
	SizeBytes   int64      `json:"size_bytes"`
	Extraction  string     `json:"-"`
	ExtractedAt *time.Time `json:"-"`
	CreatedAt   time.Time  `json:"created_at"`
}

// MediaType is the IANA type for an uploaded file, worked out from the file's
// own type rather than from anything a browser claimed about it.
//
// A browser's Content-Type on a multipart part is a guess it made from the
// name, and it is trivially set to anything at all; a vendor told the wrong one
// either refuses the file or reads it as something it is not. So the mapping is
// ours, from the extension the upload gate already established, and a type not
// on this list has no media type rather than a guessed one.
func MediaType(fileType string) string {
	switch fileType {
	case "png":
		return "image/png"
	case "jpg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "heic":
		return "image/heic"
	case "pdf":
		return "application/pdf"
	case "txt", "md":
		return "text/plain"
	case "csv":
		return "text/csv"
	case "json":
		return "application/json"
	case "xml":
		return "application/xml"
	case "rtf":
		return "application/rtf"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	}
	return ""
}
