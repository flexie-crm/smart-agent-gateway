package worker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/queue"
	"flexie.io/sag/internal/store"
)

// One worker goroutine, and the pool of them that is a worker process.
//
// This is the JOB loop, not an agent loop. It knows nothing about agents,
// models or conversations: it asks the queue for a message, claims the row it
// names, calls whatever handler is registered for that kind, and asks again. A
// goroutine that is working is not asking, so what is free and what is busy is
// a fact about who is at the door rather than a number configured in two
// places.
//
// What a handler DOES is the app's business. A fleet member is run by the one
// agent loop every other agent runs through (agent.Runner), resolved live with
// its own model, prompt, tools and brains: being on a worker changes where it
// runs, not what it is.

// Handler does one kind of job. It is given the job whole, because a worker
// cannot recompute what was true when the job was made.
//
// Returning an error means the job failed and should be tried again later, up
// to its attempts. Returning nil means it is finished with.
type Handler func(ctx context.Context, job *model.Job) error

// Pool is the set of goroutines a worker process runs.
type Pool struct {
	store    store.Store
	q        queue.Queue
	handlers map[string]Handler
	log      zerolog.Logger

	// instance names this process in a claim, so a row says which node holds it.
	instance string
	// prefix is the subject root this pool serves, and therefore the jobs it may
	// claim: "jobs.default." for a general worker, "jobs.model-host." for a box
	// with a GPU.
	prefix string

	// concurrency is the number of goroutines, which is also the most jobs this
	// process can hold at once.
	concurrency int
	// heartbeat is how often a running job says it is still alive, and stale is
	// how long a claim survives without one. The gap between them is what a
	// worker is allowed to stall for before its work is given away.
	heartbeat time.Duration
	stale     time.Duration
	// scanEvery is how often the process looks for work nothing has claimed.
	scanEvery time.Duration

	mu        sync.Mutex
	quiescing bool
	running   sync.WaitGroup
}

// New builds a pool. The capability decides both what it hears and what it may
// claim, so a node cannot be woken for one kind of work and take another.
func New(st store.Store, q queue.Queue, log zerolog.Logger, capability string, concurrency int) *Pool {
	if concurrency <= 0 {
		concurrency = 1
	}
	return &Pool{
		store:       st,
		q:           q,
		log:         log,
		handlers:    map[string]Handler{},
		instance:    instanceID(),
		prefix:      queue.SubjectRoot + "." + capability + ".",
		concurrency: concurrency,
		heartbeat:   DefaultHeartbeat,
		stale:       DefaultStale,
		scanEvery:   DefaultScanEvery,
	}
}

// instanceID names this process. A hostname plus a random tail, so two
// containers from one image are still told apart in a claim.
func instanceID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	return host + "-" + token()
}

// token is the unique half of a claim. It is what makes the statement that
// reads back a claim exact rather than trusting a worker never to repeat itself.
func token() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Only reachable if the system has no randomness at all, and a
		// timestamp is still unique enough to tell two claims apart.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// Defaults chosen so the numbers relate to each other rather than being three
// independent guesses: a claim survives four missed heartbeats, and the scan
// runs often enough that a lost message is a short delay rather than a stall.
const (
	DefaultHeartbeat = 10 * time.Second
	DefaultStale     = 40 * time.Second
	DefaultScanEvery = 15 * time.Second
)

// Handle registers the function for a kind of job. A kind with no handler is
// not an error at the process level: another node may serve it.
func (p *Pool) Handle(kind string, h Handler) {
	if p.handlers == nil {
		p.handlers = map[string]Handler{}
	}
	p.handlers[kind] = h
}

// Run starts the goroutines and the scan, and blocks until ctx is cancelled.
//
// Shutdown mirrors the server's (KB/27) with one advantage a server does not
// have: a job can be handed back. Anything claimed but not started is released,
// and the attempt goes back with it, so three restarts during a deploy do not
// exhaust a job that never ran.
func (p *Pool) Run(ctx context.Context, group string) error {
	subject := p.prefix + ">"

	// The scan is not a backstop here, it is part of the mechanism: the message
	// is acked on arrival, so nothing redelivers a crashed worker's job, and a
	// process that dies between the ack and the claim leaves a row with nothing
	// pointing at it. This is what finds it. It is also what makes a worker with
	// no broker connection useful at all.
	p.running.Add(1)
	go func() {
		defer p.running.Done()
		p.scan(ctx)
	}()

	for i := 0; i < p.concurrency; i++ {
		sub, err := p.q.Subscribe(ctx, subject, group)
		if err != nil {
			return fmt.Errorf("subscribe worker %d: %w", i, err)
		}
		p.running.Add(1)
		go func(n int, sub queue.Subscription) {
			defer p.running.Done()
			defer func() { _ = sub.Close() }()
			p.serve(ctx, n, sub)
		}(i, sub)
	}

	<-ctx.Done()
	p.running.Wait()
	return nil
}

// serve is one goroutine's whole life.
// serve is one goroutine's whole life.
//
// It is a select, and that is the point: the goroutine owns its own loop. It
// sits on a LIVE connection to the broker, so a job published now arrives now,
// with nothing running on a timer to notice it. And because it is a select
// rather than a call it can hear other things at the same time, in full control
// of what it does next.
//
//	a message  -> claim the row it woke us for, and do the work
//	a sweep    -> look for work whose signal never arrived at all
//	shutdown   -> stop, having claimed nothing
//
// The sweep is the FLOOR, not the mechanism. A message is acked on arrival, so
// a process that dies between the ack and the claim leaves a row nothing points
// at; the sweep is what finds it, and it is deliberately slow because it is
// covering an accident rather than doing the job.
func (p *Pool) serve(ctx context.Context, n int, sub queue.Subscription) {
	claimer := fmt.Sprintf("%s#%d", p.instance, n)
	// Staggered, so a pool of sixteen does not sweep as one.
	sweep := time.NewTicker(p.scanEvery + jitter(p.scanEvery))
	defer sweep.Stop()

	for {
		// Free, so ask for one. Nothing is fetched from the broker until this
		// is said, which is what keeps an unstarted job in the broker rather
		// than in this process while the goroutine is busy.
		sub.Want()

		select {
		case <-ctx.Done():
			return

		case task, ok := <-sub.Tasks():
			if !ok {
				return // the subscription is over
			}
			p.log.Debug().Str("job", task.ID).Str("kind", task.Kind).Msg("worker took a job from the queue")
			if p.stopping() {
				// Nothing claimed, so nothing to hand back: the row is still
				// pending and whoever comes next will find it.
				return
			}
			// The message named a job, and we claim by CAPABILITY rather than by
			// that id: the row is the truth, and if the sweep or another worker
			// has already taken the one we were told about, the next one waiting
			// is just as good. A wake-up is a wake-up, not an assignment.
			p.take(ctx, claimer)

		case <-sweep.C:
			if p.stopping() {
				return
			}
			p.take(ctx, claimer)
		}
	}
}

// take claims one job and runs it, reporting whether there was one.
func (p *Pool) take(ctx context.Context, claimer string) bool {
	claim := claimer + "#" + token()
	jobs, err := p.store.Jobs().Claim(ctx, claim, p.prefix, 1)
	if err != nil {
		p.log.Error().Err(err).Msg("claim a job")
		return false
	}
	// Nothing to claim is the ordinary case, not a failure: the scan may have
	// taken it already, or another worker beat us to the door. A wake-up for
	// work somebody else has is simply discarded.
	if len(jobs) == 0 {
		return false
	}
	p.work(ctx, claim, jobs[0])
	return true
}

// work runs one job to its end, saying it is still alive as it goes.
func (p *Pool) work(ctx context.Context, claim string, job *model.Job) {
	h, ok := p.handlers[job.Kind]
	if !ok {
		// This node cannot do it. Hand it straight back rather than failing it:
		// another node serves this kind, and burning an attempt for arriving at
		// the wrong door would eventually kill work nothing is wrong with.
		if err := p.store.Jobs().Release(ctx, job.ID, claim); err != nil {
			p.log.Error().Err(err).Str("job", job.ID).Msg("release a job this node cannot do")
		}
		return
	}

	// The heartbeat runs for as long as the work does. Losing it is not a
	// warning, it is the answer: somebody else has the job now, so this one
	// stops rather than finishing work that will be recorded twice.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go p.beat(runCtx, stop, claim, job.ID)

	err := h(runCtx, job)

	// Written down on a context that did not just get cancelled, the same rule
	// a background delegation settles under (KB/27): the recording must survive
	// the thing being recorded.
	done := context.WithoutCancel(ctx)
	switch {
	case err == nil:
		if err := p.store.Jobs().Complete(done, job.ID, claim); err != nil {
			p.log.Error().Err(err).Str("job", job.ID).Msg("complete a job")
		}
	case errors.Is(err, context.Canceled) && p.stopping():
		// Being asked to stop is not a failure. Hand it back whole.
		if err := p.store.Jobs().Release(done, job.ID, claim); err != nil {
			p.log.Error().Err(err).Str("job", job.ID).Msg("release a job on shutdown")
		}
	default:
		if err := p.store.Jobs().Fail(done, job.ID, claim, err.Error(), p.backoff(job.Attempts)); err != nil {
			p.log.Error().Err(err).Str("job", job.ID).Msg("record a failed job")
		}
	}
}

// beat says the worker is still here, and stops the work when it learns it is
// not the owner any more.
func (p *Pool) beat(ctx context.Context, stop context.CancelFunc, claim, id string) {
	t := time.NewTicker(p.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			err := p.store.Jobs().Heartbeat(ctx, id, claim)
			if errors.Is(err, store.ErrNotFound) {
				// The claim is gone: this job was given to somebody else while
				// we were stalled. Stop, or two workers finish one job.
				p.log.Info().Str("job", id).Msg("this job was taken over, stopping")
				stop()
				return
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				p.log.Error().Err(err).Str("job", id).Msg("heartbeat")
			}
		}
	}
}

// scan is the half that does not depend on the broker: it returns work whose
// worker died to the pool, and finds rows nothing ever announced.
func (p *Pool) scan(ctx context.Context) {
	t := time.NewTicker(p.scanEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if p.stopping() {
				return
			}
			// Released work waits a moment before it is claimable again, so a
			// job that kills whatever picks it up does not take the next worker
			// down the instant it is let go.
			n, err := p.store.Jobs().Reclaim(ctx, p.stale, p.scanEvery)
			if err != nil {
				p.log.Error().Err(err).Msg("reclaim stale jobs")
				continue
			}
			if n > 0 {
				p.log.Info().Int("jobs", int(n)).Msg("returned work whose worker stopped")
			}
		}
	}
}

// Quiesce stops the pool taking anything new. Work already under way is left to
// finish; the caller's context is what eventually ends it.
func (p *Pool) Quiesce() {
	p.mu.Lock()
	p.quiescing = true
	p.mu.Unlock()
}

func (p *Pool) stopping() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.quiescing
}

// backoff grows with the attempts already spent, with a little jitter so a
// fleet of jobs that failed together does not come back together and fail
// together again.
func (p *Pool) backoff(attempts int) time.Time {
	wait := time.Duration(1<<min(attempts, 6)) * time.Second
	return time.Now().UTC().Add(wait + jitter(wait/2))
}

// jitter is a random slice of d, from the same source as the claim token. It
// uses crypto/rand not because a backoff is a secret but because the file
// already has it, and reaching for math/rand would mean explaining to the
// linter why this one is harmless: a suppression nobody has to read is better
// than a suppression with a good reason.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return time.Duration(binary.BigEndian.Uint64(b[:]) % uint64(d))
}
