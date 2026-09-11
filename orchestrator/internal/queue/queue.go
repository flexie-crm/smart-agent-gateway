// Package queue defines job distribution between the orchestrator and
// workers. The broker (NATS JetStream) carries wake-up signals only,
// every job is a durable, auditable MariaDB row and recovery is always a
// DB scan (KB/05 §queue). Consumers are at-least-once and must be
// idempotent on Task.ID.
//
// Subjects are capability-scoped: "jobs.default.<kind>", "jobs.gpu.<kind>",
// "jobs.model-host.<kind>". A worker subscribes only to the subjects
// matching its declared capabilities.
package queue

import (
	"context"
	"encoding/json"
)

type Task struct {
	// ID is the jobs-table row id and the idempotency key.
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	WorkspaceID int64           `json:"workspace_id"`
	Payload     json.RawMessage `json:"payload"`
	Attempt     int             `json:"attempt"`
}

type Queue interface {
	// Enqueue publishes a signal for an already-committed job row.
	// Callers must commit the row first (enqueue-after-commit rule).
	Enqueue(ctx context.Context, subject string, t Task) error
	// Subscribe joins a group on a subject. Every member of the group races for
	// a message and exactly one gets it.
	//
	// The subscription holds a connection to the broker OPEN for as long as it
	// lives, and messages arrive on it as they are published. Nothing here runs
	// on a timer and nothing asks again in a loop.
	Subscribe(ctx context.Context, subject, group string) (Subscription, error)

	// SubscribeAll joins a group and delivers everything, as fast as it arrives.
	//
	// The opposite end of Subscribe, and for the opposite reason. A worker takes
	// ONE job because taking a second would mean holding work it is not doing;
	// the process that hears about finished work has nothing to hold, and a
	// hundred workers reporting in the same second are a hundred things it
	// should be getting on with at once rather than a hundred things in a line.
	//
	// The reader is what makes that true: it has to handle them in parallel, or
	// the line has simply moved from the broker into one goroutine.
	SubscribeAll(ctx context.Context, subject, group string) (Subscription, error)

	// Broadcast joins a subject as an INDEPENDENT listener: every process that
	// subscribes hears every message, rather than racing for it.
	//
	// It is the opposite of Subscribe, and the difference is what the message
	// is. A job must be done once, so it goes to one worker. An operation is
	// about whoever happens to be doing something ("stop what you are doing on
	// this fleet"), and nobody knows which process that is, so it goes to all
	// of them and the ones it does not concern ignore it.
	//
	// The listener is ephemeral and hears only what is published after it
	// joined. A process that was down did not miss an instruction it could have
	// carried out: it was not running the work the instruction was about.
	Broadcast(ctx context.Context, subject string) (Subscription, error)

	Close() error
}

// Subscription is a live connection to the broker, delivering on a channel.
//
// A channel rather than a call, so a goroutine reading it is in FULL CONTROL of
// its own loop: it selects over this, its shutdown, and whatever else it has to
// do, and it is never inside a call that has taken the decision away from it.
// Nothing polls, and nothing wakes on an interval to ask whether anything
// happened; the connection stays open and a message published now arrives now.
type Subscription interface {
	// Tasks delivers messages as the broker publishes them, each ALREADY
	// acknowledged.
	//
	// Unbuffered, and that is the backpressure: a send waits for a reader, so
	// the process pulls another message only when somebody is free to take one.
	// Nothing accumulates here, so a crash loses nothing that was not already
	// being worked on.
	//
	// The ack is on arrival, before the work, and that is the design rather than
	// an oversight: the job's row carries the lease (claimed, heartbeated,
	// reclaimed when stale), and a second lease in the broker with its own
	// timeout would eventually disagree with it. So a message is a doorbell.
	// Lose it and the row is still there; the sweep finds it.
	//
	// It closes when the subscription is closed or its context ends.
	Tasks() <-chan Task

	// Want says the reader is free for one message.
	//
	// A worker calls it each time round its loop, and that is what makes "one at
	// a time" exact: nothing is fetched until somebody is free to take it, so a
	// worker busy for ten minutes leaves its next message in the BROKER, where
	// losing this process cannot lose it. A listener that only reacts is always
	// free and never has to call it.
	Want()

	Close() error
}
