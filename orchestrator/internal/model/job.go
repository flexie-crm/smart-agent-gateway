package model

import "time"

// A job is work that must happen, written down before it starts.
//
// The distinction that decides whether something belongs here is the same one
// the event bus draws from the other side (KB/30): the bus carries **reactions**
// and may drop them, a job is an **obligation** and may not be lost. A title
// that never gets generated leaves a conversation called "New chat" forever; a
// memory note dropped from a full channel is a thing the agent was told and
// then silently was not.
//
// So the row is the truth and the broker is only a wake-up (KB/05). Every job
// is findable by a database scan, which is what makes recovery after a crash a
// query rather than a hope, and what lets a worker with no broker at all still
// make progress.
type Job struct {
	ID          string
	WorkspaceID int64
	// Kind is what to do: the handler is looked up by it.
	Kind string
	// Subject is where it was announced, capability-scoped
	// ("jobs.default.chat.title"). A worker claims by the prefix of the
	// capabilities it serves, so what a node will pick up is a property of how
	// it was started rather than a list kept in the database.
	Subject string
	// Payload is everything the handler needs. It travels WITH the job on
	// purpose: a worker cannot recompute what was true when the job was made,
	// and some of it is deliberately a snapshot (an agent's loadout at the
	// moment its turn ended, KB/25).
	Payload []byte

	Status      JobStatus
	Attempts    int
	MaxAttempts int
	LastError   string

	// RunAfter is when this becomes claimable. It is what a retry backoff and a
	// scheduled task both write to, so there is one mechanism for "later".
	RunAfter time.Time
	// LockedBy is the claim, not the worker: "<instance>#<token>". The instance
	// half says which node holds it, for the reclaim scan and for a person
	// reading the table; the token half is unique per claim, so the statement
	// that reads back what it just took cannot pick up a sibling's row.
	LockedBy string
	LockedAt *time.Time

	CreatedAt   time.Time
	CompletedAt *time.Time
}

// JobStatus is the lifecycle, and it has four states rather than the five the
// column allows.
//
// A job that failed with attempts to spare goes back to **pending** with its
// reason in `last_error` and its next attempt in `run_after`, because that is
// what it is: work waiting to be done again. `dead` is the one that ran out of
// attempts and will not be retried, which is a genuinely different thing and
// the reason it has its own word: dead rows are the ones somebody has to look
// at.
//
// The schema's enum also carries `failed`, and nothing writes it. Adding it
// later would need care: it matches neither the claim (which wants pending) nor
// the reclaim (which wants running), so a row in it would never move again.
type JobStatus string

const (
	JobPending   JobStatus = "pending"
	JobRunning   JobStatus = "running"
	JobCompleted JobStatus = "completed"
	JobDead      JobStatus = "dead"
)

// DefaultJobAttempts is how many times a job is tried before it is left dead.
// Five is the schema's own default, kept here so a caller that does not care
// does not have to know the column.
const DefaultJobAttempts = 5

// Terminal reports whether this job is finished with, either way.
func (j *Job) Terminal() bool { return j.Status == JobCompleted || j.Status == JobDead }

// JobUIDPrefix is four characters because the column is char(26) and the
// random half is 22: the id is exactly as long as the schema declares.
const JobUIDPrefix = "job_"

// NewJobUID mints a job's identifier. It is also the idempotency key a handler
// dedupes on, since delivery is at-least-once.
func NewJobUID() (string, error) { return newUID(JobUIDPrefix) }

// Job kinds. A kind is looked up to find its handler, so it is a constant on
// both sides rather than a literal a worker and an enqueuer could spell
// differently.
const (
	// JobKindAgentRun runs ONE agent on ONE task, away from the conversation
	// that asked for it: a fleet member (Mode B, KB/27). Everything about the
	// work is on its delegation row; the job carries only which row.
	JobKindAgentRun = "agent.run"
	// JobKindAgentResume carries a person's answer to a card a fleet member
	// raised, back to a worker. The agent that asked is gone (its goroutine
	// ended when it parked), so continuing it is a new piece of work rather
	// than a resumption of the old job.
	JobKindAgentResume = "agent.resume"
	// JobKindModelPull watches a download an inference node is doing, and
	// registers the model when it lands (KB/35).
	//
	// The download itself is NOT this job: it is the node's, it carries on
	// whether or not anybody is watching, and its progress is read off the node
	// rather than copied into a row. What is an obligation, and therefore what
	// is written down here, is that a model which finishes downloading at three
	// in the morning is routable at nine.
	JobKindModelPull = "model.pull"
)
