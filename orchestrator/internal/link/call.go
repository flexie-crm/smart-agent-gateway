package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Running a tool on the person's own computer.
//
// The gateway holds the schema: what the tool is called, what it takes, what
// the model is told about it. The machine holds the doing. That split is not
// tidiness, it is the only way the model's instructions can be versioned with
// the gateway rather than with whichever build of the application somebody
// happens to have installed.
//
// A call is a STREAM, exactly as a connection is, and for the same reasons: it
// may take a while, it may say a lot, and it must not hold up anything else. It
// inherits the ticket, the identity check, the per-machine limit and the
// cleanup when a machine goes away, none of which had to be built again.
//
// The arguments travel on the stream rather than in the request that asks for
// it, so the control socket stays small whatever somebody is writing to a file.

// A call is bounded by LIVENESS, not by a stopwatch.
//
// Two minutes was an arbitrary answer to a question nobody can answer in
// advance: an install, a build, a search across a disk take what they take, and
// cutting one off in the middle loses the work and tells the person their
// computer did not answer, which is not what happened. What a call must never
// do is wait for ever on a machine that has gone away, and that is a different
// question with a real answer.
//
// The answer is the machine's PRESENCE, which the registry already keeps: a
// machine is here while its control socket is, and that socket is pinged by the
// gateway on its own loop (socket.go) and closed the moment it stops answering.
//
// It is NOT a ping on the call's own stream, which is what this was first built
// as, and the way that failed is worth keeping. The application does not poll a
// call's socket while it is running the tool: it reads the request, awaits the
// work, and writes the answer. A ping arrives in the middle of that and nothing
// is listening for it, so the watchdog concluded the machine was gone and
// closed the call. Every command over about forty seconds died, which is every
// command anybody would have wanted this for. Pinging a socket only proves the
// far end is polling it, and here the far end is deliberately busy.
//
// What still bounds the WORK is where it belongs: the tool's own deadline on
// the machine, and the turn, which ends when the person cancels it.
var stillThereEvery = 5 * time.Second

// maxResult is the most a machine may answer with. A tool that reads a file
// caps itself, but the cap must also exist HERE: what arrives is going into a
// model's context, and an application that answered with a gigabyte would be a
// turn that fails on tokens rather than a tool that refused.
const maxResult = 4 << 20 // 4 MiB

// callRequest is what goes down the stream first: which tool, and its
// arguments as the model gave them.
type callRequest struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
	// Call is what this call IS, as opposed to what it does. It is repeated if
	// the socket carrying it dies, and the machine answers the same call rather
	// than running it again: that is what makes a link that drops a pause
	// instead of losing four minutes of an install.
	Call string `json:"call"`
	// Resume says this is the gateway coming back for a call it already sent,
	// rather than asking for something new. It is set only when the request
	// was known to have ARRIVED: a call whose request never got out is safe to
	// start, and one that did must never be started twice.
	Resume bool `json:"resume,omitempty"`
}

// callResponse is what comes back.
//
// Kind carries a failure's KIND rather than only its words, so the loop can
// treat a tool called wrongly as something to learn from, exactly as it does
// for a tool that runs here (tool.ErrorKind).
type callResponse struct {
	OK      bool            `json:"ok"`
	Content json.RawMessage `json:"content,omitempty"`
	Kind    string          `json:"kind,omitempty"`
	Message string          `json:"message,omitempty"`
}

// Result is one finished call, as the tool layer reads it.
type Result struct {
	OK      bool
	Content json.RawMessage
	Kind    string
	Message string
}

// Call runs a tool on one machine and waits for its answer.
// A call SURVIVES its socket.
//
// A websocket dies for reasons that have nothing to do with the work: a network
// moves, a laptop sleeps, a credential is renewed. The command it was carrying
// is still running on that computer, and failing the call because the socket
// went is losing work nobody can get back, and telling somebody their computer
// is not connected when it is sitting in front of them.
//
// So a call has an id, and asking again with the same id does not run anything
// again: the machine waits for the work already going and answers with it. A
// dropped socket is then a pause of however long the application takes to come
// back, rather than a failure.
//
// What it does NOT do is retry a machine that stays away, or a call the person
// cancelled: waiting is bounded by the machine coming back, and by the turn.
var (
	// How long to wait for a machine to come back before giving up on a call it
	// was carrying.
	//
	// THREE MINUTES, and the number is a judgement about what is on the other
	// side of it. The work is still running over there and its answer is being
	// held for whoever comes back for it; giving up early throws away something
	// that exists. A laptop that sleeps while a build runs, a VPN that
	// reconnects, a wifi handover in a lift: all of those are a minute, not
	// thirty seconds, and all of them used to end with "your computer is not
	// connected" about a computer that was fine.
	//
	// It is not unbounded, because a turn cannot hang on a laptop that has gone
	// home. What makes three minutes tolerable is that the person can stop the
	// turn, which cancels this the moment they do.
	comesBackWithinDefault = 3 * time.Minute
	// How often to look for it while waiting.
	lookForItEveryDefault = time.Second
)

// The same two, as a test may shorten them. Every wait in this package is a
// variable for one reason: a bug in what happens when a machine goes away is
// found by making it go away, and a test that has to wait three minutes to ask
// the question is a test nobody runs.
var (
	comesBackWithin = comesBackWithinDefault
	lookForItEvery  = lookForItEveryDefault
)

func (r *Registry) Call(ctx context.Context, workspaceID, userID int64, deviceID, name string, args json.RawMessage, reason string) (Result, error) {
	id, err := newTicket() // the same minting a stream ticket uses: unguessable, unique
	if err != nil {
		return Result{}, fmt.Errorf("the call could not be identified: %w", err)
	}
	k := key{workspaceID, userID, deviceID}
	started := time.Now()
	// Whether the request is known to have reached the machine. Until it has,
	// a retry may safely run the tool; once it has, a retry may only collect
	// the answer, because the work may be going on over there and running a
	// command twice is worse than not knowing how the first one ended.
	arrived := false
	// Whether the machine was there when this call began. It decides what an
	// absence MEANS: nobody running the application is an answer, and an
	// application that was here a moment ago is a wait.
	wasThere := r.Online(workspaceID, userID, deviceID)
	for attempt := 1; ; attempt++ {
		result, sent, err := r.callOnce(ctx, k, id, name, args, reason, arrived)
		arrived = arrived || sent
		switch {
		case err == nil:
			r.log.Debug().Str("tool", name).Str("device_id", deviceID).Int("attempt", attempt).
				Dur("took", time.Since(started)).Bool("ok", result.OK).
				Msg("a call to a computer finished")
			return result, nil

		case errors.Is(err, ErrTimeout) && attempt < dialAttempts && r.Online(workspaceID, userID, deviceID):
			// The application is there but did not dial back in time. That is
			// a moment, not a state: it was busy, the machine was asleep for a
			// second, something was slow. One more try before an answer that
			// reads as "your computer is not connected" about a computer that
			// plainly is.
			r.log.Info().Str("tool", name).Str("device_id", deviceID).Int("attempt", attempt).
				Msg("a computer did not dial back in time; asking again")

		case errors.Is(err, errLinkDied), wentAway(err) && wasThere:
			// The machine is not there, and it WAS. Three shapes of the same
			// moment: the socket went while the work was going, the dial-back
			// never came because the application had gone, or it had already
			// gone when this attempt was made.
			//
			// All three are a wait, not an answer. The application goes away
			// routinely and briefly (its credential is renewed every hour, a
			// laptop hands over wifi, a VPN reconnects), and the work is going
			// on over there the whole time. Giving up on the dial deadline is
			// how a `git push` mid-turn came back as "your computer did not
			// answer in time" about a computer that was back within eight
			// seconds.
			//
			// The same id is used when it returns, so what arrived is resumed
			// rather than run a second time.
			// It may already be back: this is a machine that goes away and
			// returns, and between the failure and this line it can have done
			// both. Waiting is for when it is still gone; when it is here, ask
			// it again now.
			if r.Online(workspaceID, userID, deviceID) {
				r.log.Info().Str("tool", name).Str("device_id", deviceID).Int("attempt", attempt).
					Msg("the computer is back; asking it again")
				continue
			}
			r.log.Info().Str("tool", name).Str("device_id", deviceID).Int("attempt", attempt).
				Dur("took", time.Since(started)).Msg("the computer carrying a call went away; waiting for it")
			if !r.comesBack(ctx, k) {
				r.log.Info().Str("tool", name).Str("device_id", deviceID).
					Dur("took", time.Since(started)).Msg("the computer did not come back; the call is given up")
				return Result{}, ErrNoMachine
			}

		default:
			r.log.Info().Err(err).Str("tool", name).Str("device_id", deviceID).Int("attempt", attempt).
				Dur("took", time.Since(started)).Msg("a call to a computer failed")
			return result, err
		}
	}
}

// How many times a call asks a machine that is THERE to dial back. Two, which
// covers a moment of slowness without turning a real problem into a long wait.
const dialAttempts = 2

// What bounds this is TIME, not a count of returns: each wait is capped
// (comesBackWithin) and a machine that does not come back inside one ends the
// call. Counting them instead broke the case this exists for, a link cut every
// two seconds for a minute while a build runs, which is a laptop on bad wifi
// and is meant to survive.

// wentAway is an error that means the machine is not answering: it did not dial
// back in time, or it was already gone. Whether that is a wait or an answer
// depends on whether it was there when the call started, which is the caller's
// question and not this one's.
func wentAway(err error) bool {
	return errors.Is(err, ErrTimeout) || errors.Is(err, ErrNoMachine)
}

// errLinkDied is the one failure worth trying again: the socket carrying a call
// ended before the answer did. Everything else is an answer.
var errLinkDied = errors.New("the link carrying this call ended")

// comesBack waits for a machine to link again, and says whether it did.
func (r *Registry) comesBack(ctx context.Context, k key) bool {
	waited, cancel := context.WithTimeout(ctx, comesBackWithin)
	defer cancel()
	ticker := time.NewTicker(lookForItEvery)
	defer ticker.Stop()
	for {
		select {
		case <-waited.Done():
			return false
		case <-ticker.C:
			if r.Online(k.workspaceID, k.userID, k.deviceID) {
				return true
			}
		}
	}
}

func (r *Registry) callOnce(
	ctx context.Context,
	k key,
	id, name string,
	args json.RawMessage,
	reason string,
	resume bool,
) (result Result, sent bool, err error) {
	conn, err := r.stream(ctx, k, func(ticket string) openRequest {
		// The tool's NAME is not here. It travels on the stream with its
		// arguments, so there is one place a call is described rather than two
		// that can disagree.
		return openRequest{Type: "open", Kind: kindCall, Ticket: ticket, Reason: reason}
	})
	if err != nil {
		return Result{}, false, err
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Ending is enforced by closing the connection, because a carried
	// connection has no deadlines of its own (conn.go): what bounds it is the
	// context it was opened with, and whether the machine is still there.
	done := make(chan struct{})
	defer close(done)
	go r.keepWatch(ctx, conn, done, k)

	request, err := json.Marshal(callRequest{Tool: name, Args: args, Call: id, Resume: resume})
	if err != nil {
		return Result{}, false, fmt.Errorf("the call could not be written: %w", err)
	}
	if _, err := conn.Write(request); err != nil {
		return Result{}, false, fmt.Errorf("%w: %w", ErrNoMachine, err)
	}
	// Nothing more is coming from this end, and saying so is what lets the
	// machine know the request is complete without a length prefix.
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		if err := half.CloseWrite(); err != nil {
			return Result{}, false, fmt.Errorf("the call could not be sent: %w", err)
		}
	}
	// From here the machine has it. A retry may only collect the answer.
	sent = true

	raw, err := io.ReadAll(io.LimitReader(conn, maxResult+1))
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, sent, ErrTimeout
		}
		// The socket ended before the answer did. The work is still going on
		// that computer, so this is worth asking again for rather than
		// reporting: the caller waits for the machine and repeats the id.
		return Result{}, sent, fmt.Errorf("%w: %w", errLinkDied, err)
	}
	// Nothing at all, on a socket that closed cleanly, is the same event.
	if len(raw) == 0 {
		return Result{}, sent, errLinkDied
	}
	if len(raw) > maxResult {
		return Result{}, sent, errors.New("the chat application answered with more than can be read")
	}
	var answer callResponse
	if err := json.Unmarshal(raw, &answer); err != nil {
		// An answer we cannot read is the application's fault and not the
		// model's, so it is an error rather than a tool result.
		return Result{}, sent, fmt.Errorf("the chat application's answer could not be read: %w", err)
	}
	return Result(answer), sent, nil
}

// Runs is what this machine says it can do.
//
// A gateway is often newer than somebody's application, so a tool it ships may
// be one that installation has never heard of. Asking here means the tool is
// simply not offered that turn, rather than being offered and failing when
// somebody tries to use it.
func (r *Registry) Runs(workspaceID, userID int64, deviceID string) map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.machines[key{workspaceID, userID, deviceID}]
	if !ok {
		return nil
	}
	return m.runs
}

// keepWatch closes the call when the turn ends, or when the machine goes away.
// It is the whole of what bounds a call, and neither half is a clock: one is
// somebody cancelling, the other is a computer that is no longer there.
//
// Presence is READ rather than asked for. It costs nothing on the wire, it
// cannot be confused by an application that is busy, and it is already true:
// the control socket is pinged on its own loop and a machine whose socket stops
// answering is taken out of the registry.
func (r *Registry) keepWatch(ctx context.Context, conn net.Conn, done <-chan struct{}, k key) {
	ticker := time.NewTicker(stillThereEvery)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			_ = conn.Close()
			return
		case <-ticker.C:
			if !r.Online(k.workspaceID, k.userID, k.deviceID) {
				// Closing the connection is what turns this into an answer
				// instead of a wait.
				_ = conn.Close()
				return
			}
		}
	}
}
