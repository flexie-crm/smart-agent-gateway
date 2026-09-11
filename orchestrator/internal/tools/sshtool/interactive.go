package sshtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"flexie.io/sag/internal/cmdpolicy"
	"flexie.io/sag/internal/tool"
)

// Running a command, whether or not it finishes.
//
// EVERY command runs here, in a session with a terminal, because whether one
// finishes is not something to decide in advance. Most do, and the session is
// closed in the same call: that is an ordinary command, indistinguishable from
// how it used to work. The rest are the ones a single blocking call could never
// have served, and they are the same shape as each other:
//
//   - passwd wants the new password twice, an installer wants a choice: it
//     printed a question and is waiting to be answered;
//   - tail -f, journalctl -f, a build: it is still printing, and reading it
//     again is how it is followed;
//   - a shell: cd, an exported variable, an activated environment all stick,
//     which no command that starts fresh can offer.
//
// All three are "it has not finished". The assistant reads what came back and
// answers it, reads more, or stops it. A session holds a channel while it is
// open, which is why one is closed when idle and closed for good after an hour.

const (
	// sessionIdle ends a session nobody has typed into. A forgotten one must not
	// hold a channel open on the server for the rest of the day.
	sessionIdle = 10 * time.Minute
	// sessionLifetime ends a session however busy it has been.
	sessionLifetime = time.Hour
	// sessionOutputLimit is how much of one session's output is kept. It is a
	// window on the end of it: what a program is asking now matters, what it
	// printed a thousand lines ago does not. How many sessions there may be is
	// not decided here: each holds one of the connection's channels, so the
	// pool's own budget is the only limit, and one number bounds everything.
	sessionOutputLimit = 64 * 1024
)

// server is this tool's machine, as the sessions are keyed by it.
func (h *handler) server() string { return fingerprint(h.cfg)[:12] }

// interactive is one running program that can be typed into.
type interactive struct {
	// key is the agent and the server together. That pairing IS the session: an
	// agent drives one program at a time on one machine (the loop runs its tool
	// calls one after another), so there is no identifier for anybody to carry,
	// lose, or mix up.
	key     string
	owner   tool.Owner
	command string
	// shell records that this session reads commands, decided when it was
	// started. It is what makes typing into it checked exactly as a command is.
	shell   bool
	started time.Time

	session *ssh.Session
	stdin   io.WriteCloser
	lease   *lease

	mu       sync.Mutex
	output   []byte
	dropped  bool
	lastUsed time.Time
	done     bool
	exit     error
}

// append adds what the program printed, keeping the end of it.
func (s *interactive) append(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.output = append(s.output, chunk...)
	if len(s.output) > sessionOutputLimit {
		s.output = s.output[len(s.output)-sessionOutputLimit:]
		s.dropped = true
	}
}

// take returns everything printed since it was last taken, so two reads never
// show the same prompt twice.
func (s *interactive) take() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, dropped := string(s.output), s.dropped
	s.output, s.dropped = nil, false
	s.lastUsed = time.Now()
	return clean(out), dropped
}

func (s *interactive) finished() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done, s.exit
}

func (s *interactive) finish(err error) {
	s.mu.Lock()
	s.done, s.exit = true, err
	s.mu.Unlock()
}

func (s *interactive) idleSince() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUsed
}

// close ends the program and gives back the channel it was holding.
func (s *interactive) close() {
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.session != nil {
		_ = s.session.Close()
	}
	if s.lease != nil {
		s.lease.release()
	}
}

// sessions holds the interactive sessions open on every server, and owns the
// goroutine that ends the ones nobody is using.
type sessions struct {
	mu   sync.Mutex
	open map[string]*interactive

	stop    chan struct{}
	done    chan struct{}
	reaping sync.Once
	running bool
}

func newSessions() *sessions {
	return &sessions{open: map[string]*interactive{}, stop: make(chan struct{}), done: make(chan struct{})}
}

// sessionKey pairs an agent with a server. It is what a session is known by, so
// nothing has to be handed out and handed back.
func sessionKey(owner tool.Owner, server string) string {
	return string(owner) + "@" + server
}

func (m *sessions) add(s *interactive) {
	m.mu.Lock()
	m.open[s.key] = s
	m.mu.Unlock()
	m.startReaper()
}

// get returns the session an agent has open on a server, if it has one.
func (m *sessions) get(owner tool.Owner, server string) (*interactive, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.open[sessionKey(owner, server)]
	return s, ok
}

func (m *sessions) remove(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.open, key)
}

func (m *sessions) startReaper() {
	m.reaping.Do(func() {
		m.running = true
		go m.reap()
	})
}

func (m *sessions) reap() {
	defer close(m.done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.closeStale(time.Now())
		}
	}
}

// closeStale ends sessions nobody has typed into, and ones that have been open
// too long whatever has been happening in them.
func (m *sessions) closeStale(now time.Time) {
	m.mu.Lock()
	var stale []*interactive
	for key, s := range m.open {
		if now.Sub(s.idleSince()) >= sessionIdle || now.Sub(s.started) >= sessionLifetime {
			stale = append(stale, s)
			delete(m.open, key)
		}
	}
	m.mu.Unlock()

	for _, s := range stale {
		s.close()
	}
}

// Close ends every open session, at shutdown.
func (m *sessions) Close() {
	m.mu.Lock()
	running := m.running
	open := make([]*interactive, 0, len(m.open))
	for _, s := range m.open {
		open = append(open, s)
	}
	m.open = map[string]*interactive{}
	m.mu.Unlock()

	if running {
		close(m.stop)
		<-m.done
	}
	for _, s := range open {
		s.close()
	}
}

// open starts a program in a session that stays open. The channel it takes is
// held until the session is closed, which is why a server allows only a few.
func (h *handler) open(ctx context.Context, command string) (*interactive, error) {
	lease, err := h.pool.acquire(ctx, h.cfg, h.owner)
	if err != nil {
		return nil, err
	}

	session, err := lease.session(ctx, h.owner)
	if err != nil {
		lease.release()
		return nil, err
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		lease.release()
		return nil, err
	}
	// With a terminal the program's output and its errors come back as one
	// stream, which is what a person would see, so it is read as one. A server
	// that sends the error stream separately anyway is read as well, into the
	// same place: what matters is that nothing it printed is lost.
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		lease.release()
		return nil, err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		_ = session.Close()
		lease.release()
		return nil, err
	}

	// Asking for a terminal and starting the program both wait on the server, so
	// both are bounded: one that accepts the channel and then says nothing must
	// not hold this open.
	if err := h.begin(session, command); err != nil {
		_ = session.Close()
		lease.release()
		return nil, err
	}

	live := &interactive{
		key:      sessionKey(h.owner, h.server()),
		owner:    h.owner,
		command:  command,
		shell:    cmdpolicy.IsShell(command),
		started:  time.Now(),
		lastUsed: time.Now(),
		session:  session,
		stdin:    stdin,
		lease:    lease,
	}

	// One goroutine per stream, reading until the program ends. The session is
	// finished only once BOTH have ended, so nothing printed on the way out is
	// missed.
	var reading sync.WaitGroup
	reading.Add(2)
	for _, stream := range []io.Reader{stdout, stderr} {
		go func(r io.Reader) {
			defer reading.Done()
			buf := make([]byte, 4096)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					live.append(buf[:n])
				}
				if err != nil {
					return
				}
			}
		}(stream)
	}
	go func() {
		reading.Wait()
		live.finish(session.Wait())
	}()

	return live, nil
}

// begin asks for a terminal and starts the program, within the time the server
// is given to answer anything. Both are requests the server replies to, and one
// that accepts the channel and then says nothing must not hold this open.
func (h *handler) begin(session *ssh.Session, command string) error {
	done := make(chan error, 1)
	go func() {
		// A terminal, because a program that asks questions expects one, with echo
		// off so what is typed is not read back as if the program had said it.
		//
		// A DUMB one on purpose. Every command runs on a terminal now, and a
		// program that believes it has a real one starts colouring its output and,
		// worse, paging it: systemctl and git would pipe themselves into a pager
		// and wait forever for a keypress nobody is going to press. Both check for
		// a dumb terminal and behave when they see one.
		//
		// A server may refuse a terminal outright (PermitTTY no), and that is no
		// reason to refuse the work: the command runs without one. Only a program
		// that insists on a terminal is affected, and on such a server it could
		// never have run at all.
		modes := ssh.TerminalModes{ssh.ECHO: 0, ssh.ECHOCTL: 0, ssh.ICRNL: 1, ssh.ONLCR: 1}
		_ = session.RequestPty("dumb", 48, 160, modes)
		done <- session.Start(command)
	}()

	timer := time.NewTimer(time.Duration(h.cfg.Limits.ConnectSeconds) * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("the program could not be started: %w", err)
		}
		return nil
	case <-timer.C:
		return fmt.Errorf("the server did not answer when asked to start the program")
	}
}

// clean strips the control sequences a terminal program writes to move the
// cursor and colour its output. They are for a screen; what is wanted here is
// what a person would have read.
func clean(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) {
			// A control sequence: skip to the byte that ends it.
			j := i + 1
			if s[j] == '[' || s[j] == ']' {
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
			}
			i = j + 1
			continue
		}
		if s[i] == '\r' {
			i++
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// write types a line into the program, which is what a person pressing return
// would send.
func (s *interactive) write(input string) error {
	line := input
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	_, err := s.stdin.Write([]byte(line))
	return err
}

// collect gathers what the command prints until it EXITS or the wait runs out.
//
// Those are the only two things that can end a call, and both are exact. There
// is deliberately no third rule about the output looking finished or looking
// like a question: nothing on the wire says whether a program is waiting for a
// person, and a rule that pretended otherwise was wrong in both directions.
func (s *interactive) collect(wait time.Duration) (string, bool) {
	deadline := time.Now().Add(wait)
	var collected strings.Builder
	var dropped bool

	for {
		if chunk, cut := s.take(); chunk != "" {
			collected.WriteString(chunk)
			dropped = dropped || cut
		}
		// Finished is checked after taking, and taken again: the reader marks the
		// session done only once it has appended everything, so nothing printed on
		// the way out is left behind.
		if done, _ := s.finished(); done {
			rest, cut := s.take()
			collected.WriteString(rest)
			return capped(collected.String(), dropped || cut)
		}
		if time.Now().After(deadline) {
			return capped(collected.String(), dropped)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// state is whether the program is still running, and what it ended with if not.
func (s *interactive) state() (bool, *int) {
	done, err := s.finished()
	if !done {
		return true, nil
	}
	code := 0
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitStatus()
	}
	return false, &code
}

// capped bounds what one call hands back.
//
// Each take() is capped on its own, and that is not the same thing: collect
// makes SEVERAL takes and joins them, so a program printing faster than the
// poll interval returned a multiple of the limit. The limit exists to bound
// what reaches a model's context, and a bound that holds per read and not per
// answer does not bound anything.
//
// The END is kept, the same choice append makes: the last thing a program said
// before it stopped is almost always the part that matters.
func capped(out string, dropped bool) (string, bool) {
	if len(out) <= sessionOutputLimit {
		return out, dropped
	}
	return out[len(out)-sessionOutputLimit:], true
}
