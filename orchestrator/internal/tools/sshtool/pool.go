package sshtool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"flexie.io/sag/internal/tool"
)

// One connection to a server, shared by everything that works on it.
//
// SSH carries many independent channels over a single connection, so a tool that
// signs in once can run several things at a time. That is what is pooled here:
// not connections, but the channels on one. Signing in repeatedly would be
// slower, would multiply the credential's exposure, and, on a server that asks
// for a verification code, would ask a person for one every time.
//
// The pool cannot live in the handler: a handler is rebuilt for every turn, and
// a connection that died with the turn that opened it would never be reused. It
// belongs to the template, which is built once at wiring time and closed at
// shutdown, and is keyed by the settings, so editing a tool leaves the old
// connection to idle out instead of handing back a connection to the old server.

const (
	// slotWait is how long a call waits for a free slot before giving the answer
	// back to the assistant. Long enough for a quick command ahead of it to
	// finish, short enough that the assistant is told rather than left hanging.
	slotWait = 5 * time.Second
	// reapInterval is how often idle connections are looked for. Idle timeouts
	// are in minutes, so this is fine-grained enough.
	reapInterval = 30 * time.Second
)

// errBusy is every slot on a server being in use. The handler turns it into a
// retryable answer that says how many there are.
var errBusy = errors.New("every slot on this server is in use")

// errPoolClosed is the process shutting down. A call that arrives then is
// answered rather than served a half-open connection.
var errPoolClosed = errors.New("the gateway is shutting down")

// pool holds the live connection to each configured server. It owns the reaper
// goroutine that closes the ones nothing is using.
type pool struct {
	mu      sync.Mutex
	servers map[string]*server
	closed  bool

	stop chan struct{}
	done chan struct{}
	// ctx is the parent of every sign-in, so a shutdown ends one that is in
	// progress instead of leaving it to run out its own clock.
	ctx    context.Context
	cancel context.CancelFunc
	// codeWindow is how long a sign-in waits for a verification code. It lives
	// here rather than in a package variable so a test can shorten it without
	// reaching into state another test is reading.
	codeWindow time.Duration

	// reaping brings the reaper up on first use, and reaperRunning records that
	// it did, so a shutdown knows whether there is a goroutine to wait for.
	reaping       sync.Once
	reaperRunning bool
}

func newPool() *pool { return newPoolWithCodeWindow(defaultCodeWindow) }

func newPoolWithCodeWindow(codeWindow time.Duration) *pool {
	ctx, cancel := context.WithCancel(context.Background())
	return &pool{
		servers:    map[string]*server{},
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
		codeWindow: codeWindow,
	}
}

// startReaper brings the reaper up the first time there is anything to reap. A
// build whose tools nobody uses, and every test that builds an app without
// touching a server, runs no goroutine at all.
func (p *pool) startReaper() {
	p.reaping.Do(func() {
		p.reaperRunning = true
		go p.reap()
	})
}

// server is one configured server: its settings, the connection when there is
// one, and the slots that bound how much may run on it at a time.
type server struct {
	cfg Config
	// slots is the number of operations that may be in flight. A send takes a
	// slot, a receive gives it back, so the buffer's length is what is in use.
	slots chan struct{}

	// parent is the pool's context: a sign-in in progress ends when the process
	// does, rather than outliving it.
	parent context.Context
	// codeWindow is how long this server's sign-in waits for a verification code.
	codeWindow time.Duration

	mu sync.Mutex
	// conns is every sign-in to this server that is still open. There is usually
	// one. There is more than one when the work asked for exceeds what a single
	// sign-in may carry, which is the server's own limit and not ours.
	conns    []*connection
	lastUsed time.Time
	// dialing is the sign-in in progress, if there is one. Everything that wants
	// the connection waits on it, so ten calls arriving at once on a closed
	// connection produce one sign-in and not ten, and a server that asks for a
	// verification code asks for one code and not ten.
	dialing *signIn
}

// maxTakeAttempts bounds how many times one call will go round looking for room
// before saying the server is busy. Generous: losing the race repeatedly takes a
// crowd, and the answer it ends on is retryable anyway.
const maxTakeAttempts = 8

// connection is one sign-in, and how much of it is in use.
//
// A server carries only so many jobs at once on a single connection (sshd's
// MaxSessions, ten by default) and refuses the rest, which is a hard failure
// arriving in the middle of somebody's work. Counting what we have handed out
// lets the tool sign in again before reaching that, so the number of jobs an
// administrator asks for is the number they get.
type connection struct {
	client *ssh.Client
	inUse  int
}

// fingerprint identifies a configured server by its settings. Two tools pointing
// at the same server with the same credential share one connection; changing any
// setting yields a new one, so an edited tool never keeps talking to the old
// machine.
func fingerprint(cfg Config) string {
	raw, err := json.Marshal(cfg)
	if err != nil {
		// A config that cannot be encoded has already failed to parse; give it an
		// identity of its own rather than sharing one.
		return fmt.Sprintf("unencodable-%s-%d", cfg.Host, cfg.Port)
	}
	sum := sha256.Sum256(raw)
	// The reach is not in the JSON (it is nobody's setting), and it MUST be in
	// the identity: without it, the first person to use a chat-reached tool
	// would open a connection through their computer that everybody else then
	// shared.
	return hex.EncodeToString(sum[:]) + cfg.reachKey
}

// server returns the shared server for a configuration, creating it the first
// time it is asked for.
func (p *pool) server(cfg Config) (*server, error) {
	key := fingerprint(cfg)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errPoolClosed
	}
	if s, ok := p.servers[key]; ok {
		return s, nil
	}
	s := &server{
		cfg:      cfg,
		slots:    make(chan struct{}, cfg.Limits.MaxChannels),
		lastUsed: time.Now(),
		parent:   p.ctx,

		codeWindow: p.codeWindow,
	}
	p.servers[key] = s
	p.startReaper()
	return s, nil
}

// acquire takes a slot on a server and makes sure it is connected. The slot is
// held until the lease is released, which is what stops a server being asked for
// more at once than it was configured to give.
func (p *pool) acquire(ctx context.Context, cfg Config, owner tool.Owner) (*lease, error) {
	s, err := p.server(cfg)
	if err != nil {
		return nil, err
	}
	return s.acquire(ctx, owner)
}

// submitCode gives a verification code to the sign-in waiting on a server.
func (p *pool) submitCode(cfg Config, owner tool.Owner, code string) error {
	s, err := p.server(cfg)
	if err != nil {
		return err
	}
	return s.submitCode(owner, code)
}

func (s *server) acquire(ctx context.Context, owner tool.Owner) (*lease, error) {
	timer := time.NewTimer(slotWait)
	defer timer.Stop()

	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errBusy
	}

	conn, err := s.take(ctx, owner)
	if err != nil {
		<-s.slots
		return nil, err
	}
	return &lease{server: s, conn: conn}, nil
}

// inUse is how many slots are held right now, for telling the assistant what the
// server's state is when it is asked to wait.
func (s *server) inUse() int { return len(s.slots) }

// take reserves room for one job on a connection to this server, opening
// another connection when the ones we have are as full as the server allows.
func (s *server) take(ctx context.Context, owner tool.Owner) (*connection, error) {
	// A loop, because of the cold start: ten agents can arrive on a server with
	// no connection at all, and they all wait on the SAME sign-in (which is what
	// makes a server asking for a verification code ask once and not ten times).
	// When it completes they take room on it in turn, and whoever finds it full
	// goes round again and signs in for more. Each turn either finds room or
	// makes some, so this ends; the bound is only there so that a server allowing
	// fewer than it was told is answered with "busy" rather than being asked to
	// sign in without end.
	for attempt := 0; attempt < maxTakeAttempts; attempt++ {
		if conn := s.spare(); conn != nil {
			return conn, nil
		}
		client, err := s.connect(ctx, owner)
		if err != nil {
			return nil, err
		}
		if conn := s.attach(client); conn != nil {
			return conn, nil
		}
	}
	return nil, errBusy
}

// spare hands out room on a connection that has some, and counts it as used.
func (s *server) spare() *connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, conn := range s.conns {
		if conn.inUse < s.cfg.Limits.JobsPerConnection {
			conn.inUse++
			return conn
		}
	}
	return nil
}

// attach counts a job onto the connection that was just signed in. It returns
// nil when there is no room on it after all, which is what happens to the
// eleventh of eleven callers waiting on one sign-in: they must not all be put
// onto it past what the server allows, or the server refuses them itself, in
// the middle of their work. The caller signs in again instead.
func (s *server) attach(client *ssh.Client) *connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, conn := range s.conns {
		if conn.client == client {
			if conn.inUse >= s.cfg.Limits.JobsPerConnection {
				return nil
			}
			conn.inUse++
			return conn
		}
	}
	// A sign-in records its connection, so not finding it here means it has since
	// been dropped. Going round again is the answer.
	return nil
}

// done gives back the room one job was holding.
func (s *server) done(conn *connection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if conn.inUse > 0 {
		conn.inUse--
	}
	s.lastUsed = time.Now()
}

// connect signs in and returns the connection. Callers that arrive together all
// wait on the same sign-in, so ten calls on a server that wants a verification
// code ask for one code and not ten.
//
// The sign-in itself runs on its own goroutine rather than here, because a
// server may stop to ask for a verification code and a person cannot be waited
// on inside a tool call. When that happens this returns the question, and the
// call that carries the answer back finds the same sign-in still waiting.
func (s *server) connect(ctx context.Context, owner tool.Owner) (*ssh.Client, error) {
	s.mu.Lock()
	attempt := s.dialing
	if attempt == nil {
		attempt = newSignIn()
		s.dialing = attempt
		go s.signIn(attempt, owner)
	}
	s.mu.Unlock()

	select {
	case <-attempt.done:
		return attempt.result()
	case <-attempt.asked:
		if !attempt.answered() {
			if !attempt.askedOf(owner) {
				// Somebody else was asked. Waiting is the right answer here: asking
				// a second person for a second code would be one code too many.
				return nil, errSigningIn
			}
			return nil, attempt.question()
		}
		// The code is already in; this call simply waits for the sign-in to
		// finish rather than asking for a second one.
		select {
		case <-attempt.done:
			return attempt.result()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// signIn connects to the server and publishes the outcome to everything waiting
// on it. It is bounded: the connection's own timeout, plus the window a person
// has to give a verification code, and no longer.
func (s *server) signIn(attempt *signIn, owner tool.Owner) {
	budget := time.Duration(s.cfg.Limits.ConnectSeconds)*time.Second + s.codeWindow + reapInterval
	ctx, cancel := context.WithTimeout(s.parent, budget)
	defer cancel()

	client, err := dial(ctx, s.cfg, attempt.challenge(s.codeWindow, owner), s.codeWindow)

	s.mu.Lock()
	s.dialing = nil
	if err == nil {
		// Record it here, holding nothing, so it is found by the next call that
		// wants room. A sign-in that finished but was never recorded is worse than
		// a failed one: the connection is open and unreachable, and the next call
		// signs in again, which on a server that wants a verification code means
		// asking for a second one.
		s.conns = append(s.conns, &connection{client: client})
		s.lastUsed = time.Now()
	}
	s.mu.Unlock()

	attempt.finish(client, err)
	if err == nil {
		// A connection the far end drops must not be handed out again. Waiting on
		// it is how we learn, and the goroutine ends with the connection it
		// watches.
		go s.watch(client)
	}
}

// submitCode hands a verification code to the sign-in waiting for one and waits
// for it to finish, so the call that carried the code can go straight on to the
// work it came to do.
func (s *server) submitCode(owner tool.Owner, code string) error {
	s.mu.Lock()
	attempt := s.dialing
	s.mu.Unlock()
	if attempt == nil {
		return errNoPendingSignIn
	}
	if err := attempt.deliver(owner, code); err != nil {
		return err
	}

	timer := time.NewTimer(time.Duration(s.cfg.Limits.ConnectSeconds)*time.Second + reapInterval)
	defer timer.Stop()
	select {
	case <-attempt.done:
		_, err := attempt.result()
		return err
	case <-timer.C:
		return errCodeWindowClosed
	}
}

// current is the first connection this server has, for the tests that ask
// whether it reused one rather than signing in again.
func (s *server) current() *ssh.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) == 0 {
		return nil
	}
	return s.conns[0].client
}

// connections is how many sign-ins this server is holding.
func (s *server) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (s *server) watch(client *ssh.Client) {
	_ = client.Wait()
	s.forget(client)
}

// forget drops a connection if it is still the current one. It is safe to call
// for a connection that has already been replaced: the newer one is kept.
func (s *server) forget(client *ssh.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, conn := range s.conns {
		if conn.client == client {
			s.conns = append(s.conns[:i], s.conns[i+1:]...)
			return
		}
	}
}

// discard closes a connection and forgets it, for one found to be unusable.
func (s *server) discard(client *ssh.Client) {
	s.forget(client)
	_ = client.Close()
}

// lease is one held slot on a server. Every path that takes one must release it,
// which is why release is safe to call more than once.
type lease struct {
	server *server
	// conn is the connection this job was given room on. Which one matters: the
	// room has to be given back to the same one, or a connection would look
	// fuller or emptier than it is and the tool would either refuse work it
	// could do or push past what the server allows.
	conn *connection
	once sync.Once
}

// session opens a channel to run work on. A connection that has gone stale
// between calls fails here rather than at the far end, so it is dropped and the
// sign-in retried once: the assistant should not see a failure for a connection
// that simply aged out.
func (l *lease) session(ctx context.Context, owner tool.Owner) (*ssh.Session, error) {
	session, err := l.conn.client.NewSession()
	if err == nil {
		return session, nil
	}

	// The connection is no good: drop it, take room on another (signing in again
	// if that is what it takes), and try once more.
	l.server.discard(l.conn.client)
	conn, err := l.server.take(ctx, owner)
	if err != nil {
		return nil, err
	}
	l.conn = conn
	session, err = l.conn.client.NewSession()
	if err != nil {
		// A fresh connection that still will not open one is the server saying it
		// is at its limit, whatever that limit is configured to be here. That is
		// something to wait out and retry, not a failure to report: telling the
		// agent to give up on a busy server is how work gets abandoned that would
		// have run a second later.
		return nil, errBusy
	}
	return session, nil
}

func (l *lease) release() {
	l.once.Do(func() {
		l.server.done(l.conn)
		<-l.server.slots
	})
}

// reap closes connections nothing has used for their configured idle time. It is
// the pool's own goroutine and ends when the pool is closed.
func (p *pool) reap() {
	defer close(p.done)
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.closeIdle(time.Now())
		}
	}
}

// closeIdle disconnects from every server with nothing running that has been
// quiet for longer than its idle timeout. The next call signs in again, which
// nothing above this has to think about.
func (p *pool) closeIdle(now time.Time) {
	p.mu.Lock()
	servers := make([]*server, 0, len(p.servers))
	for _, s := range p.servers {
		servers = append(servers, s)
	}
	p.mu.Unlock()

	for _, s := range servers {
		if s.inUse() > 0 {
			continue
		}
		s.mu.Lock()
		var stale []*ssh.Client
		if now.Sub(s.lastUsed) >= time.Duration(s.cfg.Limits.IdleMinutes)*time.Minute {
			// Every connection goes, not just the first: an extra one opened for a
			// busy moment must not outlive the moment.
			for _, conn := range s.conns {
				stale = append(stale, conn.client)
			}
			s.conns = nil
		}
		s.mu.Unlock()

		for _, client := range stale {
			_ = client.Close()
		}
	}
}

// Close disconnects from every server and stops the reaper. It is called once,
// at shutdown, by whoever built the pool.
func (p *pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	reaping := p.reaperRunning
	servers := make([]*server, 0, len(p.servers))
	for _, s := range p.servers {
		servers = append(servers, s)
	}
	p.mu.Unlock()

	if reaping {
		close(p.stop)
		<-p.done
	}
	// A sign-in still waiting on a person ends here too, rather than holding a
	// goroutine open for the rest of its window.
	p.cancel()

	for _, s := range servers {
		s.mu.Lock()
		conns := s.conns
		s.conns = nil
		s.mu.Unlock()
		for _, conn := range conns {
			_ = conn.client.Close()
		}
	}
}
