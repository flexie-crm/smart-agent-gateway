// Package supervise runs child processes the way Erlang runs them: a child is
// declared rather than started by hand, a restart is the answer to a crash, and
// the interesting decision is not when to restart but when to STOP restarting.
//
// It exists because the desktop application owns its own database (KB/36), and a
// bundled server is a process that can die at three in the morning with nobody
// watching. Everything here is one of Erlang's answers, kept under its own name
// so the reasoning stays findable:
//
//   - A child is ready when it says so, not when it has been spawned. Readiness
//     is a probe the child defines, and start does not return until it passes.
//   - Restart intensity: so many restarts within so long, and after that the
//     supervisor gives up and escalates rather than looping forever. Five
//     failures in a minute is not a flaky process, it is a broken datadir or a
//     port already taken, and a restart loop turns a diagnosable fault into a
//     spinner.
//   - Let it crash. A dead child is restarted from durable state, not repaired.
//   - Shutdown runs in reverse start order, so whatever depends on a child stops
//     before the child does.
package supervise

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// ErrGaveUp is what Run returns when a child failed more often than its
// intensity allows. It is the escalation: this supervisor cannot fix the
// problem, and says so to whatever supervises IT rather than hiding the fault
// behind another attempt.
var ErrGaveUp = errors.New("supervise: restarted too often, giving up")

// ErrNoSuchChild is what Restart returns when asked for something this
// supervisor does not run.
var ErrNoSuchChild = errors.New("supervise: no such child")

// ErrNotRunning is what Restart returns for a child that is declared but has no
// process right now, which is the window between one exiting and its
// replacement being ready.
var ErrNotRunning = errors.New("supervise: child is not running")

// Restart says when a child that has exited should be started again.
type Restart int

const (
	// Permanent is always restarted, however it exited. A database that exits
	// cleanly on its own is still a database that is no longer there.
	Permanent Restart = iota
	// Transient is restarted only when it exited abnormally.
	Transient
	// Temporary is never restarted.
	Temporary
)

// Process is something running that can be asked to stop, made to stop, and
// waited on.
//
// An interface rather than *os.Process so that the decisions in this package
// (when to restart, when to give up, what order to stop in) are testable without
// spawning anything, which is the same seam KB/28 uses to test the Gateway
// without a vendor.
type Process interface {
	// Stop asks it to exit, and is the polite half of shutdown.
	Stop() error
	// Kill ends it, and is what happens when the grace period runs out.
	Kill() error
	// Wait blocks until it has exited, reporting a non-nil error for an
	// abnormal exit.
	Wait() error
}

// Child is one supervised process, declared rather than started.
type Child struct {
	// Name appears in logs and in the error when this child is what gave up.
	Name string

	// Start launches it. It is called again on every restart, so it must be
	// able to run from whatever state the previous one left behind: that is the
	// let-it-crash assumption, and for a database it is InnoDB's own recovery.
	Start func(ctx context.Context) (Process, error)

	// Ready reports whether the process is USABLE, which is not the same as
	// running and is the part that is usually skipped. For a database it is a
	// connection and a query; it is never "the pid exists" and never a line in
	// a log, which is a log line and not a contract.
	//
	// It is polled until it passes or ReadyTimeout expires. Nil means starting
	// is enough.
	Ready func(ctx context.Context) error

	// ReadyTimeout bounds the wait for Ready. A first start that has to create
	// a database from nothing is slower than every start after it, so this is
	// generous by nature.
	ReadyTimeout time.Duration

	// Shutdown is how long Stop is given before Kill. Erlang's shutdown value.
	Shutdown time.Duration

	// Restart is the policy. The zero value is Permanent, which is the right
	// default for anything worth supervising.
	Restart Restart
}

// Intensity is the give-up rule: more than MaxRestarts within Period and the
// supervisor terminates instead of trying again.
type Intensity struct {
	MaxRestarts int
	Period      time.Duration
}

// DefaultIntensity is deliberately small. The failures this is meant to survive
// are one-off; the failures it must not paper over repeat.
var DefaultIntensity = Intensity{MaxRestarts: 5, Period: time.Minute}

// Supervisor starts children in order and keeps them running.
//
// The strategy is one-for-one: a child that dies is restarted, and its siblings
// are left alone. That is correct while the children are independent. The moment
// one child depends on another (an inference node that needs the database), the
// restart decision in exited() is where rest-for-one belongs: restart the failed
// child and everything started after it.
type Supervisor struct {
	log       zerolog.Logger
	intensity Intensity

	mu       sync.Mutex
	children []*supervised
}

type supervised struct {
	spec Child
	proc Process
	// done is closed by the one goroutine that waits on proc. Exactly one thing
	// may call Wait on a process (a second call to *os.Process.Wait fails with
	// "no child processes"), so shutdown waits on this rather than waiting
	// again itself.
	done    chan struct{}
	history []time.Time // when it was restarted, oldest first
	stopped bool        // true once shutdown has taken it down on purpose
	asked   bool        // true while a restart somebody asked for is in flight
}

func New(log zerolog.Logger, intensity Intensity) *Supervisor {
	if intensity.MaxRestarts <= 0 || intensity.Period <= 0 {
		intensity = DefaultIntensity
	}
	return &Supervisor{log: log, intensity: intensity}
}

// Add declares a child. Order matters: children start in the order they were
// added and stop in the reverse.
func (s *Supervisor) Add(c Child) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.children = append(s.children, &supervised{spec: c})
}

// Run starts every child, keeps them running, and blocks until ctx ends or a
// child exceeds its restart intensity.
//
// Whatever ends it, every child is stopped in reverse start order before it
// returns, so there is no path out of here that leaves a process behind.
func (s *Supervisor) Run(ctx context.Context) error {
	// A child's death is reported here. Buffered by the number of children so a
	// reporting goroutine never blocks on a supervisor that is already shutting
	// down and no longer reading.
	s.mu.Lock()
	children := append([]*supervised(nil), s.children...)
	s.mu.Unlock()
	exits := make(chan *supervised, len(children))

	for _, c := range children {
		if err := s.launch(ctx, c, exits); err != nil {
			// Nothing is running that we did not just start, and it must not be
			// left behind because the NEXT one failed.
			s.shutdown(children)
			return fmt.Errorf("supervise: start %s: %w", c.spec.Name, err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			s.shutdown(children)
			return nil
		case c := <-exits:
			if err := s.exited(ctx, c, exits); err != nil {
				s.shutdown(children)
				return err
			}
		}
	}
}

// Restart takes one child down and lets it come straight back.
//
// The recovery path for a process that is running but no longer useful. A node
// whose engine has wedged still holds its port and still accepts a connection,
// so nothing the supervisor watches for will ever fire: it is not dead, it has
// simply stopped answering. Before this, the only way back was killing it from a
// task manager or restarting the whole application, and a person who cannot see
// a terminal has neither.
//
// It ASKS and then INSISTS, the same escalation shutdown uses, because the case
// this exists for is exactly the one where a polite stop is ignored.
//
// A restart somebody asked for is NOT evidence of a fault, so it does not count
// against the give-up rule. Five deliberate restarts in a minute are five
// deliberate restarts; five crashes in a minute mean something is wrong and the
// supervisor is right to stop trying. Counting the two together would let a
// person put the application down by pressing a button we gave them.
//
// It returns once the old process is gone. Bringing the replacement up is the
// ordinary restart path, and waiting for it here would hold a request open for
// as long as a model takes to load.
func (s *Supervisor) Restart(name string) error {
	s.mu.Lock()
	var target *supervised
	for _, c := range s.children {
		if c.spec.Name == name {
			target = c
			break
		}
	}
	if target == nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNoSuchChild, name)
	}
	// Shutting down is not a state to restart out of: the process is going away
	// on purpose and exited() will decline to bring it back.
	if target.stopped || target.proc == nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotRunning, name)
	}
	proc, done, grace := target.proc, target.done, target.spec.Shutdown
	target.asked = true
	s.mu.Unlock()

	s.log.Info().Str("child", name).Msg("restarting supervised process on request")
	askThenInsist(proc, done, grace)
	return nil
}

// launch starts one child and does not return until it is ready.
func (s *Supervisor) launch(ctx context.Context, c *supervised, exits chan<- *supervised) error {
	proc, err := c.spec.Start(ctx)
	if err != nil {
		return err
	}

	if err := s.awaitReady(ctx, c, proc); err != nil {
		// A child that never became usable is not left running as if it had.
		// Nothing is monitoring it yet, so this call owns the wait.
		stopUnmonitored(proc, c.spec.Shutdown)
		return err
	}

	done := make(chan struct{})
	s.mu.Lock()
	c.proc = proc
	c.done = done
	c.stopped = false
	s.mu.Unlock()

	// One goroutine per running child, owned by this supervisor, ending when the
	// process does. It is the only caller of Wait for this process.
	go func() {
		err := proc.Wait()
		close(done)
		s.log.Debug().Str("child", c.spec.Name).Err(err).Msg("supervised process exited")
		exits <- c
	}()

	s.log.Info().Str("child", c.spec.Name).Msg("supervised process ready")
	return nil
}

// awaitReady polls the child's own readiness probe. A probe that never passes is
// a failed start, not a slow one.
func (s *Supervisor) awaitReady(ctx context.Context, c *supervised, proc Process) error {
	if c.spec.Ready == nil {
		return nil
	}
	timeout := c.spec.ReadyTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)

	var last error
	for {
		probe, cancel := context.WithTimeout(ctx, timeout)
		last = c.spec.Ready(probe)
		cancel()
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not ready after %s: %w", timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// exited decides what a child's death means. It is where the give-up rule lives.
func (s *Supervisor) exited(ctx context.Context, c *supervised, exits chan<- *supervised) error {
	s.mu.Lock()
	deliberate := c.stopped
	asked := c.asked
	c.asked = false
	s.mu.Unlock()
	if deliberate || ctx.Err() != nil {
		return nil // we took it down, or we are going down
	}

	// Asked for, so it comes back without being counted or questioned. Neither
	// the give-up rule nor the Temporary policy applies: both exist to decide
	// what a process DYING means, and this one did not die, it was replaced.
	if asked {
		s.log.Info().Str("child", c.spec.Name).Msg("starting again after a requested restart")
		return s.relaunch(ctx, c, exits)
	}

	if c.spec.Restart == Temporary {
		s.log.Warn().Str("child", c.spec.Name).Msg("temporary child exited, not restarting")
		return nil
	}

	now := time.Now()
	c.history = append(c.history, now)
	// Only failures inside the window count, so a process that misbehaves once a
	// week is not eventually condemned for it.
	kept := c.history[:0]
	for _, at := range c.history {
		if now.Sub(at) <= s.intensity.Period {
			kept = append(kept, at)
		}
	}
	c.history = kept

	if len(c.history) > s.intensity.MaxRestarts {
		s.log.Error().Str("child", c.spec.Name).
			Int("restarts", len(c.history)).Dur("within", s.intensity.Period).
			Msg("restarted too often, giving up")
		return fmt.Errorf("%w: %s failed %d times in %s",
			ErrGaveUp, c.spec.Name, len(c.history), s.intensity.Period)
	}

	s.log.Warn().Str("child", c.spec.Name).Int("restart", len(c.history)).Msg("restarting")
	return s.relaunch(ctx, c, exits)
}

// relaunch starts a child again, and turns a start that fails into another exit
// rather than an error here.
//
// A restart that cannot even start counts as one, and is reported back through
// the same channel so the give-up rule applies to it: without this a child that
// fails to spawn would be retried forever, which is precisely the loop the
// intensity exists to stop. A restart somebody ASKED for reaches this too, and
// that is deliberate: the request is not counted, but a replacement that will
// not start is a fault like any other and must not spin.
//
// The pause keeps that from being a hot loop, and gives whatever is wrong (a
// port still held by the previous one, a disk still busy) the chance to clear
// that an immediate retry would not.
func (s *Supervisor) relaunch(ctx context.Context, c *supervised, exits chan<- *supervised) error {
	if err := s.launch(ctx, c, exits); err != nil {
		s.log.Error().Str("child", c.spec.Name).Err(err).Msg("restart failed")
		go func() {
			select {
			case <-ctx.Done():
			case <-time.After(retryPause):
				exits <- c
			}
		}()
	}
	return nil
}

// retryPause separates a failed restart from the next attempt. Short, because
// the give-up rule is what bounds this, not patience.
const retryPause = 250 * time.Millisecond

// shutdown stops every child in reverse start order.
//
// Reverse, because a child started later may depend on one started earlier: the
// gateway must finish draining before the database it is writing to goes away.
func (s *Supervisor) shutdown(children []*supervised) {
	for i := len(children) - 1; i >= 0; i-- {
		c := children[i]
		s.mu.Lock()
		proc, done := c.proc, c.done
		c.stopped = true
		c.proc = nil
		s.mu.Unlock()
		if proc == nil {
			continue
		}
		s.log.Info().Str("child", c.spec.Name).Msg("stopping supervised process")
		askThenInsist(proc, done, c.spec.Shutdown)
	}
}

// askThenInsist asks a process to go, waits for the grace period, and then ends
// it. The grace is a ceiling and not a delay: one that goes when asked is not
// waited for.
//
// It waits on done rather than calling Wait, because the child's monitor
// goroutine is already the one waiting and a process may only be waited on once.
func askThenInsist(proc Process, done <-chan struct{}, grace time.Duration) {
	if grace <= 0 {
		grace = 10 * time.Second
	}
	_ = proc.Stop()
	select {
	case <-done:
	case <-time.After(grace):
		_ = proc.Kill()
		<-done
	}
}

// stopUnmonitored is the same for a process nothing is waiting on yet, which is
// only ever one that failed its readiness probe before a monitor was started.
func stopUnmonitored(proc Process, grace time.Duration) {
	done := make(chan struct{})
	go func() {
		_ = proc.Wait()
		close(done)
	}()
	askThenInsist(proc, done, grace)
}
