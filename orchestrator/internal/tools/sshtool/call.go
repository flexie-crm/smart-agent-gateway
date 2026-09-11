package sshtool

import (
	"fmt"
	"strings"
	"time"
)

// What a call says.
//
// There is no operation to choose. A call runs a command, answers the one that
// is running, or ends it, and which of those it means follows from which field
// it set.
//
// A call ends on one of two things, and both are exact: the command EXITED,
// which the protocol reports, or the wait ran out. Nothing is inferred from what
// the output looks like. It cannot be: SSH carries bytes, and whether a program
// is blocked reading from its terminal is a fact on the far side that nothing
// reports. Every rule that claimed to know it was a guess, and guesses here cost
// either a wrong answer or a wasted turn.
//
// So the waiting is the caller's to say, because the caller is the only one that
// knows what it ran. A build gets two minutes. A program that will stop and ask
// gets five seconds. And waiting is what to do instead of calling again and
// again to see: an extra call is a whole turn through the model, which is far
// more expensive than sitting on an open channel.

const (
	// defaultWait is how long a call waits when it does not say. Enough for
	// ordinary work to finish inside one call.
	defaultWait = 30 * time.Second
	// maxWait is the ceiling. A call that sits longer than this is holding a turn
	// open on something that should be looked at rather than waited on.
	maxWait = 300 * time.Second
)

// CallArgs is one call's arguments.
type CallArgs struct {
	// Command is the work: run this. Nothing else is needed for the ordinary
	// case, which is a command that finishes.
	Command string `json:"command,omitempty"`
	// Input answers whatever is waiting: the program left running by an earlier
	// call, or, when the server stopped the sign-in to ask for one, the
	// verification code the person read out. The two cannot both be waiting, so
	// the one field is never ambiguous: a sign-in is only pending when there is
	// no connection, and there is no running program without one.
	Input string `json:"input,omitempty"`
	// Stop ends the running program. It is for something that will not end on its
	// own, a log being followed, a program part-way through asking questions.
	Stop bool `json:"stop,omitempty"`
	// Wait is how many seconds to wait for the command to finish before coming
	// back with what it has printed so far. It is a ceiling, not a delay: a
	// command that finishes sooner returns sooner.
	Wait int `json:"wait,omitempty"`
}

// waitFor is how long this call is prepared to wait.
func (a CallArgs) waitFor() time.Duration {
	if a.Wait <= 0 {
		return defaultWait
	}
	return time.Duration(a.Wait) * time.Second
}

// ValidateCall rejects the one shape that cannot mean anything. Everything else
// is answered by the handler, which knows what is actually running and can say
// so precisely, rather than being guessed at from the arguments alone.
func ValidateCall(args CallArgs) error {
	if args.Stop && (strings.TrimSpace(args.Command) != "" || args.Input != "") {
		return fmt.Errorf("stop ends what is running and takes nothing with it; send it on its own")
	}
	if args.Wait < 0 || time.Duration(args.Wait)*time.Second > maxWait {
		return fmt.Errorf("wait must be between 1 and %d seconds", int(maxWait.Seconds()))
	}
	return nil
}
