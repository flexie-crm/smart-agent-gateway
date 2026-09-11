// Package link reaches a computer the gateway cannot.
//
// A customer's database or server is often not on the network the gateway runs
// on: an old finance application on a machine in an office, a server on an
// isolated VLAN. The chat application is already installed on a computer that
// CAN see those things, and it is already talking to us. So it carries the
// connection: the gateway opens a socket to somewhere it cannot reach, through
// the person's own machine, and the tool at the far end is none the wiser.
//
// The shape is a dial-back, because a laptop behind NAT cannot be dialled.
//
//   - The chat application holds one CONTROL socket, open for as long as it
//     runs, and does nothing on it but wait.
//   - When the gateway needs a connection it registers a pending stream, mints
//     a single-use ticket, and asks over that socket.
//   - The application dials a second socket back, quoting the ticket, and the
//     two halves are joined.
//
// One socket per connection, rather than every stream multiplexed over the
// control socket. It costs a round trip when a connection opens and it buys the
// absence of a whole class of problem: no stream ids, no credit windows, and no
// head-of-line blocking, where a large result set stalls an unrelated session
// sharing the same socket. The operating system does the flow control, which it
// is better at than we would be.
//
// This is deliberately NOT the notification hub (internal/ws). That hub
// addresses a PERSON and delivers to every socket they have open, and it drops
// a socket that falls behind: both are right for a notification and fatal for a
// byte stream, which must arrive whole, once, at one machine.
package link

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

var (
	// ErrNoMachine means nobody is there: this person's chat application is not
	// running on the computer the request came from, or it is and has not
	// linked. A plain, expected answer, and a tool says so in those words.
	ErrNoMachine = errors.New("the chat application is not connected on this computer")
	// ErrRefused is the machine answering that it would not open the connection:
	// it could not reach the address, or the person declined it.
	ErrRefused = errors.New("the chat application refused the connection")
	// ErrTimeout is the machine not answering in time, which is different from
	// refusing: the application may be asleep or the person away from it.
	ErrTimeout = errors.New("the chat application did not answer in time")
	// ErrTooMany is this machine already carrying as much as it will. Somebody's
	// computer is not a server, and a tool asking for more than a pool's worth
	// of connections is wrong rather than busy.
	ErrTooMany = errors.New("the chat application is already carrying too many connections")
)

const (
	// dialDeadline is how long a machine has to dial its side back. It covers a
	// round trip plus whatever the person's own network costs, and no more: a
	// tool call is waiting behind it. A machine that is THERE and misses one is
	// asked once more (call.go), because being busy for a moment is not the
	// same as being gone.
	dialDeadlineDefault = 20 * time.Second
)

// dialDeadline is that, and a test may shorten it.
var dialDeadline = dialDeadlineDefault

const (
	// maxStreams is how many connections one machine may be carrying at once.
	//
	// A person's computer is not a server, and this is the difference between a
	// tool that misbehaves and a laptop with ten thousand sockets open. A
	// database driver's pool is a handful; a tool opening more than this is
	// wrong, and being told so is better than being allowed to continue.
	maxStreams = 64
	// recheck is how often a live link is checked against its session. Holding
	// it open for as long as the socket happens to last would mean a person who
	// signed out, was disabled, or was removed from the workspace keeps a route
	// into their network open for as long as their laptop stays awake.
	recheckDefault = time.Minute
)

// recheck is that, and a test may shorten it.
var recheck = recheckDefault

// renewBefore is how long before a credential runs out the application is asked
// for a new one. Generous, because what it buys is that the renewal happens at
// a quiet moment rather than mid-call, and the cost of asking early is one
// cheap round trip an hour.
var renewBefore = 5 * time.Minute

// machine is one connected chat application: one installation, of one person,
// in one workspace.
//
// The DEVICE is part of the identity, and that is the whole point. A person may
// be signed in on a laptop and a desktop at once, and a tool that reaches their
// own network has to run on the computer they are sitting at. The request knows
// which application it came from, so it says; nothing here guesses.
type machine struct {
	workspaceID int64
	userID      int64
	deviceID    string
	// ask hands a request to the machine's control socket. It never blocks for
	// long: the socket's writer owns the wire.
	ask func(openRequest) error
	// seenAt is when this machine linked, which decides the newest.
	seenAt time.Time
	// drop closes this machine's control socket. Called when its session stops
	// being valid, which is how a link ends without waiting for a laptop to be
	// closed.
	drop func()
	// runs is what this application says it can do: each tool it knows, and
	// which version of that tool's arguments it speaks. Declared once, when it
	// links, so a gateway newer than somebody's application does not offer a
	// tool that installation has never heard of.
	runs map[string]int

	// carrying is every connection this machine holds open. They are tracked
	// because they must END with it: an application that goes away leaves
	// sockets that no longer have a far side, and a driver holding one waits
	// for an answer that cannot come.
	mu        sync.Mutex
	carrying  map[*stream]struct{}
	abandoned bool
}

// take records a connection this machine is carrying, refusing when it is
// already carrying too many, or has gone.
func (m *machine) take(s *stream) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.abandoned {
		return ErrNoMachine
	}
	if len(m.carrying) >= maxStreams {
		return ErrTooMany
	}
	if m.carrying == nil {
		m.carrying = make(map[*stream]struct{}, 8)
	}
	m.carrying[s] = struct{}{}
	return nil
}

// release forgets a connection that has ended on its own.
func (m *machine) release(s *stream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.carrying, s)
}

// abandon ends everything this machine was carrying. A connection whose far
// side is gone is not a connection, and leaving it open only means a driver
// waits for an answer instead of being told.
func (m *machine) abandon() {
	m.mu.Lock()
	held := m.carrying
	m.carrying, m.abandoned = nil, true
	m.mu.Unlock()
	for s := range held {
		_ = s.Close()
	}
}

// count is how many connections this machine is carrying, for the tests and
// for anything that reports on a link.
func (m *machine) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.carrying)
}

// openRequest is what the gateway asks a machine to do: dial this address and
// bring the socket back quoting this ticket.
type openRequest struct {
	Type   string `json:"type"`
	Ticket string `json:"ticket"`
	// Kind is what the stream is for: a connection to something on this
	// person's network, or a tool running on the computer itself.
	//
	// One mechanism for both, because a tool call has the same shape as a
	// connection: a thing that takes a while, may say a lot, and must not hold
	// up anything else. It inherits the ticket, the identity check, the limit
	// on how many at once, and the cleanup when a machine goes away.
	Kind string `json:"kind,omitempty"`
	// Host and Port are the address to connect to, for a connection.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// Reason is what the person is told this connection is for, if they are
	// asked. It is ours to write, never the model's.
	Reason string `json:"reason"`
}

// What a stream is for.
const (
	kindConnection = "tcp"
	kindCall       = "call"
)

// pending is a connection the gateway is waiting for, keyed by its ticket.
type pending struct {
	// machine is who was asked, so the connection it dials back is counted
	// against that machine and ends when it does.
	machine *machine
	// who is that machine's key, so a dial-back that arrives after the
	// application has RECONNECTED is counted against the record it now has.
	//
	// The old record is abandoned the moment its socket goes, and a ticket held
	// by the application outlives that: it reconnects, dials the ticket back,
	// and the connection was refused with "the chat application is not
	// connected on this computer" about an application that had just linked.
	// The ticket belongs to a person and a computer, not to a socket.
	who  key
	conn chan net.Conn
	// fail carries the machine's own refusal, so the caller gets the reason the
	// person's computer gave rather than a timeout that explains nothing.
	fail chan string
}

// Registry holds the connected machines and the streams being opened. One
// process, one registry; it is wired at boot and lives as long as the server.
type Registry struct {
	log zerolog.Logger
	// validate answers who a link token belongs to, and origins is the same
	// allow-list the rest of the socket surface uses in development.
	validate Validator
	origins  []string

	mu       sync.Mutex
	machines map[key]*machine
	waiting  map[string]*pending
}

// key is one installation: the workspace, the person, and which of their
// computers. All three, so two computers are two machines rather than one that
// keeps changing its mind.
type key struct {
	workspaceID, userID int64
	deviceID            string
}

func NewRegistry(log zerolog.Logger, validate Validator, originPatterns []string) *Registry {
	return &Registry{
		log:      log.With().Str("component", "link").Logger(),
		validate: validate,
		origins:  originPatterns,
		machines: make(map[key]*machine),
		waiting:  make(map[string]*pending),
	}
}

// Online reports whether this person has a chat application linked. It is what
// a tool's Test asks before promising an administrator that the tool works.
func (r *Registry) Online(workspaceID, userID int64, deviceID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.machines[key{workspaceID, userID, deviceID}]
	return ok
}

// add registers a machine's control socket, replacing an older one for the same
// person: two computers, and the one that just linked is the one they are at.
func (r *Registry) add(m *machine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.machines[key{m.workspaceID, m.userID, m.deviceID}] = m
	// The device and what it says it can do, because without them a link that
	// works and a link the tools cannot find look identical from here: a
	// mismatched device is an empty tool list and no error anywhere.
	r.log.Info().Int64("workspace_id", m.workspaceID).Int64("user_id", m.userID).
		Str("device_id", m.deviceID).Interface("runs", m.runs).
		Msg("a chat application linked")
}

// remove drops a machine, unless it has already been replaced by a newer one
// from the same person: an application that reconnects before its old socket is
// noticed as dead must not have its new link torn out from under it.
//
// Whatever it was carrying ends with it, either way. Those connections had one
// far side and it is gone.
func (r *Registry) remove(m *machine) {
	r.mu.Lock()
	k := key{m.workspaceID, m.userID, m.deviceID}
	if current, ok := r.machines[k]; ok && current == m {
		delete(r.machines, k)
		r.log.Info().Int64("workspace_id", m.workspaceID).Int64("user_id", m.userID).
			Int("carrying", m.count()).Msg("a chat application went away")
	}
	r.mu.Unlock()
	m.abandon()
}

// Dial opens a connection to host:port through this person's chat application.
//
// It is the whole point of the package, and it is an ordinary dialler from the
// caller's side: what comes back is a net.Conn, and everything above it (a
// database driver, an SSH handshake) works exactly as it does on a socket this
// machine opened itself.
func (r *Registry) Dial(ctx context.Context, workspaceID, userID int64, deviceID, host string, port int, reason string) (net.Conn, error) {
	return r.stream(ctx, key{workspaceID, userID, deviceID}, func(ticket string) openRequest {
		return openRequest{
			Type: "open", Kind: kindConnection, Ticket: ticket,
			Host: host, Port: port, Reason: reason,
		}
	})
}

// stream asks a machine for one socket and waits for it to arrive, whatever the
// socket is going to carry.
func (r *Registry) stream(ctx context.Context, k key, request func(ticket string) openRequest) (net.Conn, error) {
	r.mu.Lock()
	m, ok := r.machines[k]
	r.mu.Unlock()
	if !ok {
		return nil, ErrNoMachine
	}

	ticket, err := newTicket()
	if err != nil {
		return nil, err
	}
	slot := &pending{machine: m, who: k, conn: make(chan net.Conn, 1), fail: make(chan string, 1)}

	r.mu.Lock()
	r.waiting[ticket] = slot
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.waiting, ticket)
		r.mu.Unlock()
	}()

	if err := m.ask(request(ticket)); err != nil {
		return nil, ErrNoMachine
	}

	wait, cancel := context.WithTimeout(ctx, dialDeadline)
	defer cancel()
	select {
	case conn := <-slot.conn:
		return conn, nil
	case why := <-slot.fail:
		return nil, fmt.Errorf("%w: %s", ErrRefused, why)
	case <-wait.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrTimeout
	}
}

// deliver hands a dialled-back socket to whoever is waiting for that ticket.
//
// A ticket is spent by the delivery, so a second socket quoting it finds
// nothing; and it is only honoured for the person it was issued to, so holding
// one is not enough to receive somebody else's connection.
func (r *Registry) deliver(ticket string, conn *stream, workspaceID, userID int64, deviceID string) bool {
	r.mu.Lock()
	slot, ok := r.waiting[ticket]
	if ok && (slot.machine.workspaceID != workspaceID || slot.machine.userID != userID ||
		slot.machine.deviceID != deviceID) {
		// Not the machine this was sent to. The ticket is NOT spent: the machine
		// it belongs to may still be about to dial back, and letting a stranger
		// invalidate somebody's connection by guessing would be a way to stop
		// every tool call that uses this route.
		r.mu.Unlock()
		r.log.Warn().Int64("workspace_id", workspaceID).Int64("user_id", userID).
			Msg("a stream was offered a ticket issued to somebody else")
		return false
	}
	if ok {
		delete(r.waiting, ticket)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	// Counted against the machine before it is handed over, so a run of dials
	// cannot get past the limit by all arriving at once, and so a machine that
	// disappears takes this connection with it.
	// Counted against whichever record this person's computer has NOW. Usually
	// the one that was asked; after a reconnection, the one that replaced it.
	carrier := slot.machine
	err := carrier.take(conn)
	if errors.Is(err, ErrNoMachine) {
		// That record is gone. If this person's computer has linked again, the
		// dial-back belongs to the record it has now: same person, same
		// computer, same ticket, one reconnection in between.
		r.mu.Lock()
		live, relinked := r.machines[slot.who]
		r.mu.Unlock()
		if relinked && live != carrier {
			carrier = live
			err = carrier.take(conn)
		}
	}
	if err != nil {
		slot.fail <- err.Error()
		return false
	}
	conn.onClose = func() { carrier.release(conn) }
	slot.conn <- conn
	return true
}

// refuse hands the machine's own reason to whoever is waiting, so the tool can
// say what the person's computer said.
func (r *Registry) refuse(ticket, why string) {
	r.mu.Lock()
	slot, ok := r.waiting[ticket]
	if ok {
		delete(r.waiting, ticket)
	}
	r.mu.Unlock()
	if ok {
		slot.fail <- why
	}
}
