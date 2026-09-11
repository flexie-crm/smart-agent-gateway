package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The broker, over NATS JetStream.
//
// It carries WAKE-UPS, and that is the whole of its job. A job's truth is its
// database row (KB/05): what it is, whose it is, how many attempts it has spent
// and whether it is finished are all read from and written to MariaDB, and a
// worker with no broker at all still makes progress by scanning. So every
// decision here is made in favour of "the message is a hint" over "the message
// is the record".
//
// The most consequential of those: a message is acknowledged **on receipt**,
// before the work is done, because the job row's claim is already a lease and
// two leases with different timeouts eventually disagree. The broker would
// redeliver a message whose row still says the first worker owns it, and the
// second worker would find nothing to claim and do nothing, which is merely
// wasteful; or worse, the row's claim would expire first and the work would run
// twice for reasons nobody can see from either side alone. One lease. It is the
// database's.
const (
	// StreamName holds every job signal. Product-named, like every other schema
	// object we create (CLAUDE.md): nothing here says "nats".
	StreamName = "SAG_JOBS"
	// SubjectRoot is the prefix every job subject starts with. Subjects are
	// capability-scoped below it: jobs.default.chat.title, jobs.model-host.model.pull.
	SubjectRoot = "jobs"
	// ReportSubject is the way back: a worker says a task finished and the
	// orchestrator, which owns the conversation and the sockets, reacts.
	ReportSubject = "reports"
	// OpsSubject carries instructions rather than work: "stop the fleet", and
	// whatever else has to reach a process that is already busy. Everything
	// under it is broadcast, so it never becomes a second way to hand out jobs.
	OpsSubject = "ops"
	// CapabilityDefault is the capability ordinary work is announced under: work
	// any worker can do, needing nothing of the machine it lands on. A node with
	// a GPU declares more; every node declares this one.
	CapabilityDefault = "default"
)

// NATS is the Queue over a JetStream stream.
type NATS struct {
	conn   *nats.Conn
	js     jetstream.JetStream
	stream jetstream.Stream
}

// Connect dials the broker and makes sure the stream exists.
//
// Creating it here rather than in an operator's runbook is deliberate: a signal
// published to a stream that does not exist is silently dropped by the server,
// which would turn a missing piece of setup into jobs that are enqueued, sit in
// the database, and are simply never woken.
func Connect(ctx context.Context, url string) (*NATS, error) {
	conn, err := nats.Connect(url,
		nats.Name("sag"),
		// Reconnect forever. The broker being down must never be the reason a
		// process gives up: it degrades to scanning the database, which is
		// slower and entirely correct.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to the queue: %w", err)
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("open the queue: %w", err)
	}

	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     StreamName,
		Subjects: []string{SubjectRoot + ".>", ReportSubject + ".>", OpsSubject + ".>"},
		// A signal is worth nothing once its row has been claimed and run, and
		// the row outlives it either way. An hour is long enough to cover a
		// worker fleet being down and short enough that the stream never
		// becomes a second history of the work.
		MaxAge:    time.Hour,
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.FileStorage,
		Discard:   jetstream.DiscardOld,
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("declare the job stream: %w", err)
	}
	return &NATS{conn: conn, js: js, stream: stream}, nil
}

// Enqueue publishes the wake-up for a row that is already committed.
//
// The caller must have written the job first. A signal for a row that does not
// exist yet is a worker looking for work it cannot find, and the enqueue-after-
// commit rule (KB/05) is what keeps that from happening.
func (n *NATS) Enqueue(ctx context.Context, subject string, t Task) error {
	body, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode task: %w", err)
	}
	// Deduplicated on the job id by the server, so a retried publish after an
	// ambiguous failure does not wake two workers for one row.
	_, err = n.js.Publish(ctx, subject, body, jetstream.WithMsgID(t.ID))
	if err != nil {
		return fmt.Errorf("publish task: %w", err)
	}
	return nil
}

// Subscribe joins a group on a subject and hands back a puller.
//
// The consumer is durable and shared by name, which is what makes many workers
// one queue: they race for each message and exactly one gets it. Nothing is
// delivered until somebody asks, so a process holding twenty goroutines of
// which nineteen are busy has one outstanding request, not twenty.
func (n *NATS) Subscribe(ctx context.Context, subject, group string) (Subscription, error) {
	consumer, err := n.stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       consumerName(group),
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		// Short, because the ack comes on arrival rather than after the work.
		// This window covers a process dying between delivery and ack, which is
		// milliseconds, not the length of a job: a job's lease is its row's.
		AckWait:       30 * time.Second,
		MaxDeliver:    5,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("declare consumer %q: %w", group, err)
	}
	return newSubscription(ctx, consumer, prefetchOne)
}

// SubscribeAll is Subscribe with the brakes off: everything, as fast as it comes.
func (n *NATS) SubscribeAll(ctx context.Context, subject, group string) (Subscription, error) {
	consumer, err := n.stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       consumerName(group),
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    5,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("declare consumer %q: %w", group, err)
	}
	return newSubscription(ctx, consumer, prefetchMany)
}

// Broadcast makes a listener of its own, so every process hears the message.
//
// Ephemeral and DeliverNew, both deliberately. Ephemeral, because a durable
// consumer per process would accumulate on the server as containers come and
// go; DeliverNew, because an instruction is about work in progress, and a
// process that has just started is not doing any of it.
func (n *NATS) Broadcast(ctx context.Context, subject string) (Subscription, error) {
	consumer, err := n.stream.CreateConsumer(ctx, jetstream.ConsumerConfig{
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		DeliverPolicy: jetstream.DeliverNewPolicy,
		// Cleaned up shortly after the process holding it stops asking, so a
		// killed worker does not leave a consumer behind for the server to keep.
		InactiveThreshold: time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("declare a broadcast listener on %q: %w", subject, err)
	}
	return newSubscription(ctx, consumer, prefetchMany)
}

// subscription is a live connection to the broker, pumped onto a channel.
//
// The iterator is what holds the connection open with a request outstanding, so
// the server delivers the moment something is published rather than answering a
// question somebody asked on a timer. One goroutine sits on it and hands each
// message to whoever is reading; because the channel is unbuffered, it does not
// ask for the next until somebody has taken this one, which is prefetch-one
// stated in Go rather than in a broker setting.
type subscription struct {
	msgs  jetstream.MessagesContext
	tasks chan Task
	// ready is how a reader says it is free. The pump does not fetch until it
	// is asked, which is the difference between a reader holding one message
	// and a reader holding one message plus one it has not looked at yet.
	ready  chan struct{}
	closes sync.Once
}

// How many messages the broker may have in flight to one subscription.
//
// One is a worker: it holds a job while it works, and asking for a second would
// mean holding work nobody is doing. Many is a listener that only reacts, where
// a queue in the broker helps nobody and the reader is going to handle them all
// at once anyway.
const (
	prefetchOne  = 1
	prefetchMany = 256
)

func newSubscription(ctx context.Context, consumer jetstream.Consumer, prefetch int) (Subscription, error) {
	msgs, err := consumer.Messages(jetstream.PullMaxMessages(prefetch))
	if err != nil {
		return nil, fmt.Errorf("open the message stream: %w", err)
	}
	s := &subscription{msgs: msgs, tasks: make(chan Task), ready: make(chan struct{}, prefetch)}
	// A listener that only reacts is free from the start and stays free: it is
	// asked for nothing and takes everything. A worker asks each time it
	// finishes, so the first Want below is what starts it.
	for i := 0; i < prefetch && prefetch > 1; i++ {
		s.ready <- struct{}{}
	}
	go s.pump(ctx)
	return s, nil
}

// Want says a reader is free for one message.
//
// It is what makes "one at a time" true rather than nearly true. Without it the
// pump fetches the next message the moment the last one is taken, so a worker
// busy for ten minutes has a message sitting in this process the whole time:
// acknowledged, invisible in the broker, and gone if the process dies. That
// costs a job its wake-up, and the sweep is left to find a row nothing pointed
// at. Asking first means the broker holds it instead, where it is safe.
func (s *subscription) Want() {
	select {
	case s.ready <- struct{}{}:
	default: // already asked, or a listener that never has to
	}
}

// pump is the one goroutine that touches the iterator. It blocks on the open
// connection, hands what arrives to a reader, and blocks again.
func (s *subscription) pump(ctx context.Context) {
	defer close(s.tasks)
	for {
		select {
		case <-s.ready:
		case <-ctx.Done():
			return
		}
		msg, err := s.msgs.Next(jetstream.NextContext(ctx))
		if err != nil {
			// The context ended, or the iterator was stopped. Either way this
			// subscription is over; a reader sees the channel close.
			return
		}
		// Acked before it is even parsed. It is a doorbell: what it is about is
		// in the database, and a message we cannot read is one we can drop.
		if err := msg.Ack(); err != nil {
			continue
		}
		var t Task
		if err := json.Unmarshal(msg.Data(), &t); err != nil {
			// A signal we cannot read is one we drop; the row it was about is
			// still in the database and the sweep finds it.
			continue
		}
		select {
		case s.tasks <- t:
		case <-ctx.Done():
			return
		}
	}
}

func (s *subscription) Tasks() <-chan Task { return s.tasks }

func (s *subscription) Close() error {
	s.closes.Do(s.msgs.Stop)
	return nil
}

func (n *NATS) Close() error {
	if err := n.conn.Drain(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
		return fmt.Errorf("drain the queue: %w", err)
	}
	return nil
}

// consumerName is the durable name a group shares. It is derived rather than
// free-form so two processes serving the same capability cannot accidentally
// create two consumers and each receive every message.
func consumerName(group string) string {
	return strings.NewReplacer(".", "_", "*", "_", ">", "_").Replace(group)
}
