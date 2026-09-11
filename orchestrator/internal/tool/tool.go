// Package tool defines the single tool contract every surface funnels
// through, chat, MCP, workflow automation, CLI (KB/02 §spine). A tool is a
// Schema + Handler pair; the Registry is the one source of truth consumed
// by the agent loop, the MCP server, and the admin UI.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Owner identifies the agent a tool's state belongs to.
//
// Most tools keep nothing between calls and never need this. One that does (an
// open session on a machine, a sign-in waiting for a code) must file it under
// the agent that opened it, and NOT under the conversation: a background
// agent runs with its Gateway's session id (KB/27), so keying on the
// conversation puts a Gateway and every agent it started on one entry, and
// one of them ends up typing into another's program.
//
// It is opaque. The app mints it where it builds an agent's loadout, and a tool
// only ever compares it.
type Owner string

// OwnerNone is a loadout built to be read rather than called: the roster the
// Gateway is shown lists an agent's tools by binding them and taking their
// names. Nothing in it runs, so it belongs to no agent.
const OwnerNone Owner = ""

// agentInstances numbers running agents for OwnerOfAgent. Process-lifetime
// unique is enough: what it keys is held in memory and dies with the process.
var agentInstances atomic.Int64

// OwnerOfSession is the Gateway's owner. There is one Gateway per conversation
// and its loadout is rebuilt every turn, so the conversation is what stays the
// same across them.
func OwnerOfSession(sessionID int64) Owner {
	return Owner("session:" + strconv.FormatInt(sessionID, 10))
}

// OwnerOfAgent mints an owner for one running agent. Its loadout is built
// once when it is resolved and lives for the whole delegation, so one token per
// resolution is exactly one per running agent, whether those agents are ten
// different agents or ten copies of the same one.
func OwnerOfAgent() Owner {
	return Owner("agent:" + strconv.FormatInt(agentInstances.Add(1), 10))
}

// Display is a tool's own account of what is worth reading in one of its calls.
//
// It is OURS and not the model's: only Name, Description and InputSchema reach
// a provider, so a tool can say "show the command and what it printed" without
// spending a token saying it. The chat asks for a call, the server applies
// this, and the client renders what it is given: no tool is named anywhere in
// the UI, and adding a tool that answers with six fields nobody reads is a
// change in the tool rather than in the chat.
//
// What it is FOR is the difference between a result the model needs and a
// result a person reads. `free -h` answers with output, an exit code, whether
// the session is still running and what it is running, because the model needs
// every one of them to decide what to do next. A person opening that call
// wants the command and what it printed.
//
// A call that FAILED ignores all of it and shows everything. There, every
// field is potentially the answer, and a panel that hides the exit code of a
// command that did not work is a panel that wastes somebody's afternoon.
type Display struct {
	// Where names the fields that say where or how the call ran: a folder, a
	// server, a database. They are shown once, above both halves, taken from
	// whichever side carries them, because a tool is often not TOLD where to
	// run and reports where it did.
	Where []string
	// Sent and Answered name the fields worth reading, in the order to read
	// them. A field on neither list is not shown.
	Sent     []Shown
	Answered []Shown
}

// Shown is one field, and what it is rather than what it is called.
type Shown struct {
	Field string
	// As says how it reads: a command the way a terminal draws one, text kept
	// exactly as it came, a result set as a table. Empty is an ordinary value.
	As ShownAs
	// With is the companion field a kind needs. A table is columns AND rows,
	// which a tool answers with as two fields; naming one from the other is
	// how they arrive as the one thing they are.
	With string
}

// ShownAs is the vocabulary a tool has for how a field reads. It describes the
// DRAWING, not the side: a query's statement and a terminal's output are both
// text kept as it came, and they sit on opposite halves of a call.
type ShownAs string

const (
	ShownPlain   ShownAs = ""
	ShownCommand ShownAs = "command"
	ShownText    ShownAs = "text"
	ShownSQL     ShownAs = "sql"
	ShownTable   ShownAs = "table"
	ShownBody    ShownAs = "body"
)

// Paired is what a kind's companion field is called on the way to a page, so
// one shape reaches it per kind rather than each tool's own spelling: a table
// carries its column names, a body the type that says how to read it.
func (a ShownAs) Paired() string {
	switch a {
	case ShownTable:
		return "columns"
	case ShownBody:
		return "content_type"
	default:
		return ""
	}
}

// Value, Command, Text and Table build the kinds, so a declaration reads as a
// list of what the fields ARE. (Value rather than Field: a Field here is
// already a thing, the settings form's.)
func Value(name string) Shown   { return Shown{Field: name} }
func Command(name string) Shown { return Shown{Field: name, As: ShownCommand} }
func Text(name string) Shown    { return Shown{Field: name, As: ShownText} }

// SQL is a statement, which reads as a statement: its keywords, its strings and
// its names, told apart the way a database client tells them apart.
func SQL(name string) Shown { return Shown{Field: name, As: ShownSQL} }

// Table names the field holding the rows, and the one holding their column
// names, which is how a database tool answers.
func Table(rows, columns string) Shown {
	return Shown{Field: rows, As: ShownTable, With: columns}
}

// Body names a payload and the field saying what KIND it is, so JSON is read
// as JSON and XML as XML rather than as one long line either way. The kind is
// the service's own word for it (a content type), which beats guessing.
func Body(body, contentType string) Shown {
	return Shown{Field: body, As: ShownBody, With: contentType}
}

// Kind separates the four tool sources. All obey the same contract; the
// kind drives visibility and admin grouping only.
type Kind string

const (
	KindBuiltin  Kind = "builtin"  // code, server-side
	KindCustom   Kind = "custom"   // data-driven, workflow-backed
	KindClient   Kind = "client"   // per-request, executes on the user's client
	KindInternal Kind = "internal" // infrastructure (e.g. tool_guide): auto-loaded, invisible
	KindMCP      Kind = "mcp"      // projected from a remote MCP connection
)

// RiskLevel drives approval policy defaults (KB/04 §10.3).
type RiskLevel string

const (
	RiskReadOnly              RiskLevel = "read_only"
	RiskInternalWrite         RiskLevel = "internal_write"
	RiskExternalCommunication RiskLevel = "external_communication"
	RiskFinancialAction       RiskLevel = "financial_action"
	RiskDestructiveAction     RiskLevel = "destructive_action"
	RiskAdminAction           RiskLevel = "admin_action"
)

type Schema struct {
	Name         string `json:"name"`
	FriendlyName string `json:"friendly_name"`
	Description  string `json:"description"`
	// About is what an ADMINISTRATOR reads: what this tool is for, in the words
	// of the business rather than the words of the model. Description is written
	// for the model, and it shows: it is long, it names internal machinery
	// (tool_guide), and it talks about request bodies and byte caps. None of that
	// belongs on a screen where somebody is deciding whether a team should have
	// this ability. Empty falls back to Description, so a tool without one is
	// merely plain rather than broken.
	About string `json:"about,omitempty"`
	// Settings is the form this tool declares, when it has one to declare.
	//
	// A native tool with settings is not a contradiction: the terminal has one
	// list of what it may run, per workspace, and an administrator writes it by
	// opening the tool. What makes it native rather than a template is that
	// there is only ever ONE of it — one computer the person is sitting at —
	// where a template exists because each instance is a different remote
	// service to configure.
	//
	// The values live in the tool's row, in the same config column a custom
	// tool's do, and reach the handler by being rebound per turn.
	Settings []Section `json:"settings,omitempty"`
	// CheckSettings refuses settings the tool could not actually enforce, before
	// they are stored.
	//
	// A declared form can say a field is required and which values a select
	// takes; it cannot say that an allowlist with nothing in it is a terminal
	// that runs nothing. Only the tool knows that, and somebody who saved one
	// and was told it was saved has been told the opposite of what happened.
	// Approval equals success: the answer to a save is what the tool will do,
	// not whether the JSON parsed.
	//
	// Nil means there is nothing beyond the form to check.
	CheckSettings func(config json.RawMessage) error `json:"-"`

	InputSchema json.RawMessage `json:"input_schema"`
	Kind        Kind            `json:"kind"`
	Risk        RiskLevel       `json:"risk"`
	// RequiresApproval routes the call through confirmation
	// park-and-resume. Pre-park validation contract applies: approval
	// must equal success (KB/02 §confirmation).
	RequiresApproval bool `json:"requires_approval"`
	// ApprovalTitle and ApprovalPrompt are what the user reads on the
	// confirmation card. They are written by the tool author, because only
	// the tool knows what it is about to do; a card synthesized from a
	// schema would ask people to approve something they cannot picture.
	ApprovalTitle  string `json:"approval_title,omitempty"`
	ApprovalPrompt string `json:"approval_prompt,omitempty"`

	// FriendlyNarration is what the client shows while the tool runs ("Fetching
	// external data"), in business language, never the tool's name. Empty falls
	// back to the friendly name.
	FriendlyNarration string `json:"friendly_narration,omitempty"`

	// Shown says what a person sees when they open one of this tool's finished
	// calls in the chat. Empty means everything, which is right for a tool
	// nobody has thought about: the alternative is a panel quietly hiding the
	// one field somebody needed.
	Shown Display `json:"-"`

	// Hidden keeps a tool out of the admin catalog. It is a real, grantable tool
	// that still syncs, loads and runs, but it is infrastructure or a testing aid
	// rather than something an administrator should browse or assign, so it is not
	// advertised in the tools UI. (KindInternal is stronger: auto-loaded onto
	// every agent and never granted; Hidden is an ordinary tool simply not shown.)
	Hidden bool `json:"-"`

	// Async marks a tool the loop runs off its own goroutine, bounded by
	// AsyncTimeout, rather than inline: a tool that can block on the outside
	// world (a network call) should not hold the loop's goroutine on a syscall.
	// The stream stays alive either way (the heartbeat), so this is isolation
	// and a per-tool deadline, and the seam where the work moves to a worker
	// node later. A sync tool leaves both zero.
	Async bool `json:"async,omitempty"`
	// AsyncTimeout bounds one async run. Zero means the async runner's default.
	AsyncTimeout time.Duration `json:"-"`

	// Guide is the tool's deep, on-demand documentation: the full parameter
	// contract, precedence rules, limits, and worked examples that would bloat
	// the short Description if sent every turn. The tool_guide tool surfaces it
	// when the model asks. Empty for a tool that has only its short description.
	Guide json.RawMessage `json:"-"`

	// Topics is the tool's drill-down documentation graph: a set of focused
	// concepts the model can navigate one at a time, each teaching one thing
	// (often the mistake it prevents) and pointing at the related ones. Where
	// Guide is one flat reference page, Topics is a map the model walks, so it
	// pays for only the depth it needs. Empty for a tool with no graph. The
	// tool_guide tool navigates it: a tool name lists the topics, a topic id
	// returns its body and its edges.
	Topics []Topic `json:"-"`

	// DefinitionHash pins WHAT a projected MCP tool is (name, description,
	// input schema). Empty for tools whose definition ships with the code. A
	// parked approval records it, so a remote redefinition between the card
	// and the click is a refusal, not a surprise.
	DefinitionHash string `json:"-"`
}

// EdgeType classifies why one topic points to another, so the model knows what
// following the edge would do for it. The set is the reference CRM's: read this
// first (prerequisite), read it alongside (companion), it consumes this one's
// output (consumer), it produces what this one needs (producer), or it is a
// different way to do the same thing (alternative).
type EdgeType string

const (
	EdgePrerequisite EdgeType = "prerequisite"
	EdgeCompanion    EdgeType = "companion"
	EdgeConsumer     EdgeType = "consumer"
	EdgeProducer     EdgeType = "producer"
	EdgeAlternative  EdgeType = "alternative"
)

// TopicEdge is one link out of a topic: where it goes, why, and when the model
// should follow it. The "when" is the point, it turns a static cross-reference
// into a decision the model can make ("follow this if your last call failed
// with X").
type TopicEdge struct {
	To   string   `json:"to"`
	Type EdgeType `json:"type"`
	When string   `json:"when"`
}

// Topic is one node in a tool's documentation graph: a focused piece of
// teaching and its edges to related topics. The id is globally unique and
// namespaced by its tool ("<tool>/<slug>", e.g. "http_request/auth"), so the
// model can name a topic to drill into without also naming its tool.
type Topic struct {
	ID    string      `json:"id"`
	Title string      `json:"title"`
	Body  string      `json:"body"`
	Edges []TopicEdge `json:"edges,omitempty"`
}

// ConfirmTitle is the card heading, falling back to the tool's friendly name
// so a tool that forgot to write one is still answerable.
func (s Schema) ConfirmTitle() string {
	if s.ApprovalTitle != "" {
		return s.ApprovalTitle
	}
	if s.FriendlyName != "" {
		return s.FriendlyName
	}
	return s.Name
}

// ConfirmDescription is the one line a person reads under the card heading. It
// is NEVER the tool's own Description: that is written for the model, long, and
// may name internal machinery (tool_guide, and the like) that must not reach a
// person. A tool can write its own ApprovalPrompt; otherwise a concise line
// keyed to the risk says what matters, and the parameters below say the rest.
func (s Schema) ConfirmDescription() string {
	if s.ApprovalPrompt != "" {
		return s.ApprovalPrompt
	}
	switch s.Risk {
	case RiskExternalCommunication:
		return "This sends something to a place outside your workspace. Read what it would send before you allow it."
	case RiskFinancialAction:
		return "This can move money or change what you are billed. Read it before you allow it."
	case RiskDestructiveAction:
		return "This deletes or overwrites something, and it cannot be undone. Read it before you allow it."
	case RiskAdminAction:
		return "This changes settings or who can do what. Read it before you allow it."
	case RiskInternalWrite:
		return "This changes something saved in your workspace. Read it before you allow it."
	default:
		return "Read this before you allow it."
	}
}

// AdminDescription is what the console shows about a tool. Same rule as
// ConfirmDescription: the model's own Description is a last resort, never a
// first choice.
func (s Schema) AdminDescription() string {
	if s.About != "" {
		return s.About
	}
	return s.Description
}

// Call is one tool invocation. UserID is THE permission checkpoint, every
// downstream check (policy, ownership, audit) flows from it, identically on
// every surface.
type Call struct {
	WorkspaceID int64
	UserID      int64
	SessionID   int64
	// DeviceID is the installation this call came from, when it came from one:
	// a tool that reaches the person's own computer has to reach the one they
	// are sitting at, and a person may be signed in on two. Empty from a
	// browser, and from anything with no person in front of it.
	DeviceID string
	Name     string
	Args     json.RawMessage
	// NoConfirm is set by non-interactive callers (worker, MCP, CLI);
	// an approval-requiring tool must then be pre-authorized by policy.
	NoConfirm bool
}

// ErrorKind classifies a failed tool result so the loop can react to it. Most
// failures the model just reads and works around; a bad-arguments failure is
// different, it is the model having called the tool wrong, which is a chance to
// learn the correct usage so it does not happen again.
type ErrorKind string

const (
	// ErrorNone is a successful result.
	ErrorNone ErrorKind = ""
	// ErrorBadArguments means the model called the tool with wrong or malformed
	// arguments. It is the one failure worth remembering: the loop turns it into
	// a working note on the tool's correct usage (see agent, tool-error memory).
	ErrorBadArguments ErrorKind = "bad_arguments"
	// ErrorDenied is a permission refusal. The person may not do this; there is
	// nothing to learn about calling the tool better.
	ErrorDenied ErrorKind = "denied"
	// ErrorBlocked is a policy refusal (an SSRF-guarded host, a forbidden
	// target). Not retryable, and not a usage mistake.
	ErrorBlocked ErrorKind = "blocked"
	// ErrorTransient is a passing failure (a timeout, a network reset). The
	// model may retry; nothing to learn.
	ErrorTransient ErrorKind = "transient"
	// ErrorFailed is a generic failure with no more specific classification.
	ErrorFailed ErrorKind = "failed"
)

// Result carries the tool output plus loop control signals (KB/02 §tool
// loop). Content must be model-safe: never echo raw internal errors.
type Result struct {
	Content json.RawMessage
	// StopToolLoop forces loop exit; the model gets one final tool-less
	// pass to produce prose.
	StopToolLoop bool
	// SkipFinalPass short-circuits without the final pass (used when
	// parking for confirmation).
	SkipFinalPass bool
	// Err classifies a failed call. ErrorNone (the zero value) is success, so a
	// tool that never sets it behaves exactly as before. It drives what the loop
	// does beyond handing the content back: only ErrorBadArguments teaches.
	Err ErrorKind
}

// Failed reports whether the result is an error of any kind.
func (r Result) Failed() bool { return r.Err != ErrorNone }

type Handler func(ctx context.Context, call Call) (Result, error)

// Validation is a tool's verdict on a call, produced BEFORE the loop decides to
// park it. It is how a tool keeps the promise that an approved action succeeds:
// the arguments are checked first, so a confirmation card is only ever shown for
// a call that will actually run (KB/02, approval must equal success).
type Validation struct {
	// OK reports the call is valid and may proceed, to a card or straight to the
	// handler. When false, Result is returned now and NO card is shown.
	OK bool
	// Result is the outcome to return when OK is false: a bad-arguments (or
	// denied) result the model reads and corrects. Ignored when OK is true.
	Result Result
	// ApprovalTitle and ApprovalPrompt override the card's copy for THIS call, so
	// a person reads what will actually happen ("Save 'X' to Y / Z") rather than
	// a line keyed only to the tool. Empty falls back to the schema's. Used only
	// when OK and the call needs approval.
	ApprovalTitle  string
	ApprovalPrompt string
}

// Validator pre-checks a call before the loop parks it. A tool without one is
// unchanged: no pre-check, and its handler validates its own arguments (which is
// fine for a tool that never asks for approval). A tool that CAN park should
// have one, so a card is never shown for a call that would then fail.
type Validator func(ctx context.Context, call Call) Validation

type Tool struct {
	Schema Schema
	Handle Handler
	// Validate, when set, runs before the loop decides whether to park this
	// tool's call: the pre-park check that makes approval equal success.
	Validate Validator
}

// Loadout is the tool set of one turn: the schemas the model is shown, and
// the handlers that back them. They travel together because a schema without
// its handler is a promise the loop cannot keep.
type Loadout struct {
	// Schemas are OFFERED: they go to the model with the turn, and they are
	// what it may call.
	Schemas  []Schema
	Handlers map[string]Handler
	// OnDemand are RUNNABLE but not offered: the turn holds them, the model is
	// not handed them, and it reaches one by discovering it first.
	//
	// The distinction exists because a tool costs tokens on every turn merely by
	// being described, and a connected service can project dozens of them. On a
	// real installation three projected tools were 23,462 bytes of a 45,748-byte
	// tool list, sent on every turn of every conversation whether or not that
	// service was ever going to be touched, and paid for each time.
	//
	// Everything else about them is unchanged: they are granted, approved,
	// recorded and version-checked exactly as an offered tool is, because what
	// changes here is only whether the model is told about them up front.
	OnDemand []Schema
	// Validators are the per-tool pre-park checks, bound per turn like Handlers.
	// Only tools that have one appear here; the rest need no pre-check.
	Validators map[string]Validator
}

// Schema returns the schema for a tool by name, if the turn has it: offered or
// on demand, because a turn can RUN either and only the offering differs.
func (l Loadout) Schema(name string) (Schema, bool) {
	for _, s := range l.Schemas {
		if s.Name == name {
			return s, true
		}
	}
	for _, s := range l.OnDemand {
		if s.Name == name {
			return s, true
		}
	}
	return Schema{}, false
}

// Lookup returns a registered tool's schema by name, including internal ones.
// It is how the tool_guide tool finds the deep guide of an ability the model
// names, from the one registry that is the source of truth for what a tool is.
func (r *Registry) Lookup(name string) (Schema, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return Schema{}, false
	}
	return t.Schema, true
}

// Guided lists the names of registered tools that carry deep documentation, a
// flat guide or a topic graph, sorted. It lets the tool_guide tool tell the
// model which of its abilities it can look up.
func (r *Registry) Guided() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for name, t := range r.tools {
		if len(t.Schema.Guide) > 0 || len(t.Schema.Topics) > 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// LookupTopic finds a topic by its globally-unique id across every registered
// tool, returning the topic and the name of the tool that owns it. The id
// carries its own tool prefix, so the model can follow an edge to another
// topic without having to also name its tool.
func (r *Registry) LookupTopic(id string) (Topic, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for name, t := range r.tools {
		for _, topic := range t.Schema.Topics {
			if topic.ID == id {
				return topic, name, true
			}
		}
	}
	return Topic{}, "", false
}

// All returns every registered tool, so the workspace's tool table can be
// reconciled with what the code actually offers. Internal tools are excluded:
// they are infrastructure, and there is nothing for an administrator to decide
// about them.
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tools := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		if t.Schema.Kind == KindInternal {
			continue
		}
		tools = append(tools, t)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Schema.Name < tools[j].Schema.Name })
	return tools
}

// Registry is the process-wide tool table. Load applies an allow-list and
// returns lean schemas plus handlers; internal tools are always included
// when the allow-list is non-empty and are never listed to admin UIs.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

func (r *Registry) Register(t Tool) error {
	if t.Handle == nil {
		return fmt.Errorf("tool %q: a handler is required", t.Schema.Name)
	}
	// Checked at the door, because a name a vendor refuses does not cost this
	// tool, it costs EVERY tool: they go up as one array and the whole request
	// is rejected. Registration happens at boot, so a name that cannot work
	// stops the server rather than waiting to be discovered on somebody's turn.
	if err := ValidName(t.Schema.Name); err != nil {
		return fmt.Errorf("tool %q: %w", t.Schema.Name, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[t.Schema.Name]; exists {
		return fmt.Errorf("tool %q: already registered", t.Schema.Name)
	}
	r.tools[t.Schema.Name] = t
	return nil
}

// Load returns schemas and handlers for the allowed tool names. Unknown
// names are skipped: the allow-list is configuration, the registry is code,
// and configuration may lag a deploy.
func (r *Registry) Load(allow []string) Loadout {
	r.mu.RLock()
	defer r.mu.RUnlock()

	allowed := make(map[string]bool, len(allow))
	for _, name := range allow {
		allowed[name] = true
	}

	var schemas []Schema
	handlers := make(map[string]Handler)
	validators := make(map[string]Validator)
	for name, t := range r.tools {
		if t.Schema.Kind == KindInternal {
			if len(allow) == 0 {
				continue
			}
		} else if !allowed[name] {
			continue
		}
		schemas = append(schemas, t.Schema)
		handlers[name] = t.Handle
		if t.Validate != nil {
			validators[name] = t.Validate
		}
	}
	sort.Slice(schemas, func(i, j int) bool { return schemas[i].Name < schemas[j].Name })
	return Loadout{Schemas: schemas, Handlers: handlers, Validators: validators}
}
