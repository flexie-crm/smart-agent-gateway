package sshtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"net"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
	"flexie.io/sag/internal/tools/toolkit"
)

// handler is one bound tool: the server it works on, the agent it belongs to,
// and the shared pool that holds the connection. A new one is built for every
// turn, which is why the connection cannot live here.
type handler struct {
	cfg  Config
	pool *pool
	// owner is the agent this handler was bound for. Anything that outlives a
	// call (a running program, a sign-in waiting on a code) is filed under it, so
	// two agents working on the same machine never reach each other's, even when
	// they share a conversation as a Gateway and its background agents do.
	owner    tool.Owner
	sessions *sessions
	// machines carries the connection through the caller's own computer when the
	// tool is configured that way. Resolved per call, because which computer
	// depends on who is asking.
	machines template.Machines
}

// forCaller returns this handler's configuration as it applies to one call:
// unchanged for an ordinary server, and carrying the caller's own computer when
// the tool is reached through the chat application.
func (h *handler) forCaller(call tool.Call) Config {
	cfg := h.cfg
	if !truthy(cfg.ThroughChat) || h.machines == nil {
		return cfg
	}
	cfg.reachKey = fmt.Sprintf("|chat:%d:%d:%s", call.WorkspaceID, call.UserID, call.DeviceID)
	cfg.reach = func(ctx context.Context) (net.Conn, error) {
		return h.machines.Dial(ctx, call.WorkspaceID, call.UserID, call.DeviceID, cfg.Host, cfg.Port, "a connection to "+cfg.Host)
	}
	return cfg
}

// handle resolves the call to a handler for the person making it, then
// dispatches on that.
//
// A copy, not a field written in place: a bound tool is shared for the whole
// turn and two calls can be in flight at once, and what this resolves (which
// computer carries the connection) belongs to the caller, not to the tool.
// Everything below therefore reads one config that cannot change under it.
func (h *handler) handle(ctx context.Context, call tool.Call) (tool.Result, error) {
	live := *h
	live.cfg = h.forCaller(call)
	return live.dispatch(ctx, call)
}

// dispatch is the tool's real work. Which of the four things a call means
// follows from what it set, so there is no mode to choose and none to get wrong.
func (h *handler) dispatch(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args CallArgs
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return toolkit.BadArguments("the arguments could not be read")
	}
	if err := ValidateCall(args); err != nil {
		return toolkit.BadArguments(err.Error())
	}
	command := strings.TrimSpace(args.Command)

	switch {
	case args.Stop:
		return h.stop()
	case args.Input != "":
		return h.answer(ctx, command, args.Input, args.waitFor())
	case command != "":
		return h.run(ctx, command, args.waitFor())
	default:
		return h.look(args.waitFor())
	}
}

// result is what every call comes back with. One shape for all of them: what it
// printed, whether it is still going, and how it ended if it is not.
type result struct {
	Success bool   `json:"success"`
	Output  string `json:"output"`
	// Running says the command is still there and can be answered, read again, or
	// stopped. False means it finished and took its session with it.
	Running bool `json:"running"`
	// RunningCommand is what is still going, named in every answer while it is.
	// It is here so that nothing has to be REMEMBERED between calls: an assistant
	// that has to recall what it left running will eventually not, and send a new
	// command into a machine that is still busy with the last one. Told every
	// time, it cannot get it wrong.
	RunningCommand string `json:"running_command,omitempty"`
	ExitCode       *int   `json:"exit_code,omitempty"`
	Dropped        bool   `json:"output_dropped,omitempty"`
	Note           string `json:"note,omitempty"`
}

// run starts a command. If it finishes inside the window that is the whole call
// and the session is gone; if it does not, what it printed comes back and it
// stays open to be answered or read.
func (h *handler) run(ctx context.Context, command string, wait time.Duration) (tool.Result, error) {
	// What may run is decided before a connection is touched, so a refused
	// command never reaches the server and never holds a slot.
	if reason := h.cfg.Policy.Check(command); reason != "" {
		return toolkit.Blocked(reason)
	}
	if live, ok := h.sessions.get(h.owner, h.server()); ok {
		return toolkit.BadArguments(fmt.Sprintf("%q has not finished on this server, and only one thing runs at a "+
			"time. Answer it with input, call again with nothing to read more of it, or end it with stop. Then run "+
			"this.", live.command))
	}

	live, err := h.open(ctx, command)
	if err != nil {
		return h.connectionProblem(err)
	}
	h.sessions.add(live)
	return h.report(live, wait)
}

// answer sends what was typed to whatever is waiting for it. A running program
// takes precedence and cannot be confused with a sign-in: a program can only be
// running over a connection, and a sign-in is only pending when there is none.
func (h *handler) answer(ctx context.Context, command, input string, wait time.Duration) (tool.Result, error) {
	if live, ok := h.sessions.get(h.owner, h.server()); ok {
		// A command repeated alongside the answer is the caller saying which
		// program it is answering, which is the shape the verification-code
		// instruction asks for ("the same call again, with the code in input").
		// Refusing it here taught two different rules for one field. Only a
		// DIFFERENT command is ambiguous, and only that is refused.
		if command != "" && command != live.command {
			return toolkit.BadArguments(fmt.Sprintf("%q is still running. Answer it with input on its own, or end it "+
				"with stop, before running something else.", live.command))
		}
		// A line typed into a shell is a command, and is checked as one. Into
		// anything else it is that program's own business, bar a denied program
		// named in it.
		if reason := h.cfg.Policy.CheckInput(input, live.shell); reason != "" {
			return toolkit.Blocked(reason)
		}
		if running, _ := live.state(); !running {
			return h.report(live, 0)
		}
		if err := live.write(input); err != nil {
			h.sessions.remove(live.key)
			live.close()
			return toolkit.Failed("the program could not be typed into: " + shortReason(err))
		}
		return h.report(live, wait)
	}

	// Nothing is running, so this can only be the code the server asked for. It
	// completes the sign-in and the same call then does the work that could not
	// be done the first time.
	if err := h.pool.submitCode(h.cfg, h.owner, input); err != nil {
		return h.verificationProblem(err)
	}
	if command == "" {
		return toolkit.BadArguments("the sign-in was completed, but this call asked for nothing. Send the command you " +
			"were trying to run.")
	}
	return h.run(ctx, command, wait)
}

// look reads what the running program has printed since it was last read. It is
// how a log being followed is followed, and how a slow command is checked on.
func (h *handler) look(wait time.Duration) (tool.Result, error) {
	live, ok := h.sessions.get(h.owner, h.server())
	if !ok {
		return toolkit.BadArguments("nothing is running on this server, so there is nothing to read. Send a command " +
			"to run something.")
	}
	return h.report(live, wait)
}

// stop ends the running program and gives back the channel it was holding.
func (h *handler) stop() (tool.Result, error) {
	live, ok := h.sessions.get(h.owner, h.server())
	if !ok {
		return toolkit.BadArguments("nothing is running on this server, so there is nothing to stop. Send a command " +
			"to run something.")
	}
	h.sessions.remove(live.key)
	output, dropped := live.take()
	live.close()
	return toolkit.Success(result{Success: true, Output: output, Running: false, Dropped: dropped})
}

// report waits for the program and shapes the answer. One that has ended takes
// its session with it, so the next call knows there is nothing running rather
// than typing into something that is gone.
func (h *handler) report(live *interactive, wait time.Duration) (tool.Result, error) {
	output, dropped := live.collect(wait)
	running, exit := live.state()
	if !running {
		h.sessions.remove(live.key)
		live.close()
	}
	answer := result{Success: true, Output: output, Running: running, ExitCode: exit, Dropped: dropped}
	if running {
		answer.RunningCommand = live.command
		answer.Note = stillRunningNote()
	}
	return toolkit.Success(answer)
}

// stillRunningNote is the one thing the result cannot say by itself: what to do
// next. Three choices, each named with the exact call that makes it, so the
// decision left to the assistant is only WHICH, never HOW.
func stillRunningNote() string {
	return "Still running after the wait. If it is asking something, send input with the answer. If it just needs " +
		"longer, call again with a bigger wait and nothing else, which collects what it prints in the meantime. " +
		"If you are done with it, send stop. Do not call repeatedly to check: wait for it instead."
}

// verificationRequest is what the assistant is told when the server stopped to
// ask for a code. It is not a failure: nothing was called wrong and nothing
// broke, so it comes back as an ordinary result the assistant reads and acts on.
type verificationRequest struct {
	Success              bool   `json:"success"`
	VerificationRequired bool   `json:"verification_required"`
	Question             string `json:"question,omitempty"`
	Instruction          string `json:"instruction,omitempty"`
	ExpiresInSeconds     int    `json:"expires_in_seconds"`
	Message              string `json:"message"`
}

// askForCode turns the server's question into the answer the assistant reads. It
// says plainly whose question it is and what to do with it, because the one
// thing that must not happen is the assistant inventing a code.
func askForCode(question *needsCode) (tool.Result, error) {
	remaining := int(time.Until(question.Expires).Seconds())
	if remaining < 0 {
		remaining = 0
	}
	prompt := question.Question()
	return toolkit.Success(verificationRequest{
		VerificationRequired: true,
		Question:             prompt,
		Instruction:          question.Instruction,
		ExpiresInSeconds:     remaining,
		Message: "The server will not accept the connection until it is given a verification code, and it is holding " +
			"the connection open waiting for one. Ask the person for the code (" + prompt + "), then make this same " +
			"call again with the same command, adding input set to what they tell you. Never guess a code or make one " +
			"up: only the person has it. If nobody answers within " + strconv.Itoa(remaining) +
			" seconds the server gives up, and making the call again simply asks afresh.",
	})
}

// verificationProblem is a code that arrived too late, or for a question that is
// no longer being asked. Making the call again raises a fresh question, so the
// assistant is told to do that rather than to give up.
func (h *handler) verificationProblem(err error) (tool.Result, error) {
	switch {
	case errors.Is(err, errNoPendingSignIn):
		// Nothing was waiting for it at all: no program running and no sign-in
		// asking. That is the call being wrong, which is worth correcting.
		return toolkit.BadArguments("nothing on this server is waiting for input: no command is running, and the " +
			"server is not asking for a verification code. Send the command you want to run.")
	case errors.Is(err, errNotYourSignIn), errors.Is(err, errCodeWindowClosed):
		return toolkit.Transient("that answer came too late, or was for a question somebody else was asked, so the " +
			"server stopped waiting for it. Send the command again on its own: if the server asks for a code, the " +
			"person can read out a fresh one.")
	default:
		return toolkit.Failed("the sign-in could not be completed: " + shortReason(err))
	}
}

// connectionProblem turns a failure to get onto the server into something the
// assistant can act on: waiting its turn is a retry, everything else is reported.
func (h *handler) connectionProblem(err error) (tool.Result, error) {
	var question *needsCode
	if errors.As(err, &question) {
		return askForCode(question)
	}
	switch {
	case errors.Is(err, errSigningIn):
		return toolkit.Transient("somebody else is signing in to this server right now. Wait a few seconds and make " +
			"the same call again.")
	case errors.Is(err, errBusy):
		return toolkit.Transient(fmt.Sprintf("this server runs at most %d operations at once and all of them are busy. "+
			"Wait a few seconds and make the same call again.", h.cfg.Limits.MaxChannels))
	case errors.Is(err, errPoolClosed):
		return toolkit.Transient("the gateway is restarting, so nothing was run. Make the same call again shortly.")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return toolkit.Transient("the server did not answer in time, so nothing was run.")
	default:
		return toolkit.Failed("the server could not be reached: " + shortReason(err))
	}
}

// shortReason keeps our own wording and drops what it wrapped. The detail
// underneath is about sockets and handshakes: an administrator wants it in a
// log, the assistant only needs to know which part failed.
func shortReason(err error) string {
	msg := err.Error()
	if cut := strings.Index(msg, ": "); cut > 0 {
		return msg[:cut]
	}
	return msg
}
