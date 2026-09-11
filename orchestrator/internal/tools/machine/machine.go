// Package machine is the tools that run on the person's own computer.
//
// The schema is here and the doing is there, and that split is the whole
// design. A tool's name, its arguments and everything the model is told about
// it are prompt text, and prompt text has to be versioned with the gateway: if
// the application owned it, two people asking the same assistant the same
// question would be talking to different products depending on which build they
// had installed.
//
// So this package is schemas plus one dispatcher. Adding a tool is a file here
// and a handler in the application, which is not double work: it is one
// contract in the two places it has to hold.
//
// Everything else a tool has comes free, because these are ordinary built-in
// tools: the catalogue, the grants, the agent's confirm set, approval,
// narration, and a failure the loop can learn from.
package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"flexie.io/sag/internal/cmdpolicy"
	"flexie.io/sag/internal/link"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolkit"
)

// Machines is what this package needs of the link: run a tool on one
// installation, and say what that installation can run.
//
// An interface, so the tools depend on the capability rather than on the
// package that implements it, and a test can answer without a socket.
type Machines interface {
	Call(ctx context.Context, workspaceID, userID int64, deviceID, name string, args json.RawMessage, reason string) (link.Result, error)
	Runs(workspaceID, userID int64, deviceID string) map[string]int
}

// Tools is every machine tool this build ships, ready to register.
//
// Native tools, fixed: there is one terminal on a person's computer and one
// answer to what computer it is, so neither is something an administrator
// creates. What the terminal may RUN is theirs, and arrives per turn.
func Tools(machines Machines) []tool.Tool {
	tools := []tool.Tool{
		{Schema: infoSchema(), Handle: Dispatch(machines, infoSchema(), infoSchema().Name)},
		// The terminal ships with an EMPTY policy, which refuses nothing and
		// allows nothing beyond the defaults. The turn rebinds it over what the
		// administrator wrote (BindTerminal), exactly as the brain tools are
		// rebound over the agent's own brains: a handler built at boot cannot
		// know a setting somebody changes at four in the afternoon.
		{Schema: terminalSchema(), Handle: terminalHandler(machines, cmdpolicy.Policy{})},
	}
	// The files on that same computer, each its own tool because each is its
	// own grant (files.go).
	return append(tools, fileTools(machines)...)
}

// Offers reports which machine tools this person's computer can actually run,
// so a turn is built from what is there rather than from what this build ships.
//
// A gateway is often newer than somebody's application. Filtering here means
// the tool is simply absent from that turn: the model never learns of an
// ability it cannot use, nothing fails in the middle of a conversation, and
// nobody is asked to update before using the things that do work.
func Offers(machines Machines, workspaceID, userID int64, deviceID string) map[string]bool {
	if machines == nil || deviceID == "" {
		return nil
	}
	runs := machines.Runs(workspaceID, userID, deviceID)
	if len(runs) == 0 {
		return nil
	}
	offered := make(map[string]bool, len(runs))
	for _, t := range Tools(machines) {
		version, known := runs[t.Schema.Name]
		offered[t.Schema.Name] = known && version == versionOf(t.Schema.Name)
	}
	return offered
}

// Runs reports whether a tool of this name runs on the person's own computer.
//
// The loadout asks, so it can leave such a tool out when there is no computer
// to run it on. It is a question about the NAME rather than a list to keep in
// step: adding a tool to this package answers it, and forgetting to add it
// somewhere else is not a thing that can happen.
func Runs(name string) bool {
	for _, t := range Tools(nil) {
		if t.Schema.Name == name {
			return true
		}
	}
	return false
}

// Dispatch is every machine tool's handler: send the call to the computer the
// request came from, and shape the answer the way the loop reads one.
//
// remote is the name the APPLICATION knows the tool by, which is not always the
// name the model sees: a tool an administrator created carries their alias, and
// the far side has one handler for the kind of thing it is.
func Dispatch(machines Machines, schema tool.Schema, remote string) tool.Handler {
	return func(ctx context.Context, call tool.Call) (tool.Result, error) {
		if machines == nil {
			return toolkit.Failed("this installation cannot reach a personal computer")
		}
		if call.DeviceID == "" {
			// A browser, or anything with no person in front of it. Said plainly,
			// because "it did not work" would send somebody looking at the tool.
			return toolkit.Failed("this ability works in the chat application, on your own computer, and this conversation is not in one")
		}

		answer, err := machines.Call(ctx, call.WorkspaceID, call.UserID, call.DeviceID,
			remote, inThisConversation(call), schema.FriendlyName)
		switch {
		case errors.Is(err, link.ErrNoMachine):
			return toolkit.Failed("the chat application is not connected on this computer")
		case errors.Is(err, link.ErrTimeout):
			return toolkit.Failed("your computer did not answer in time")
		case err != nil:
			return toolkit.Failed(fmt.Sprintf("your computer could not do that: %s", err))
		}
		if answer.OK {
			return toolkit.Success(answer.Content)
		}
		// The computer's own reason, classified as it classified it, so a tool
		// called wrongly is something the assistant corrects rather than a
		// failure it merely reports.
		switch answer.Kind {
		case string(tool.ErrorBadArguments):
			return toolkit.BadArguments(answer.Message)
		case string(tool.ErrorDenied):
			// The computer's own words, not the generic refusal: "outside the
			// folder this may touch" tells the assistant something it can act
			// on, where "you do not have permission" does not.
			return toolkit.Blocked(answer.Message)
		default:
			return toolkit.Failed(answer.Message)
		}
	}
}

// BindTerminal points the terminal at what this workspace decided it may run.
//
// Called once per turn with the tool's stored settings, because a policy is
// read at boot and edited at four in the afternoon: a handler built with the
// old one would go on enforcing it until the process restarted. The brain tools
// are rebound the same way and for the same reason.
func BindTerminal(loadout tool.Loadout, machines Machines, policy cmdpolicy.Policy) {
	name := terminalSchema().Name
	if _, ok := loadout.Handlers[name]; !ok {
		return
	}
	loadout.Handlers[name] = terminalHandler(machines, policy)
}

// RefuseTerminal binds a terminal that runs nothing and says WHY.
//
// For the one state a policy cannot describe: settings that could not be read.
// The gate refuses those already, because an unreadable policy resolves to an
// allowlist of nothing, but it would refuse them with the words for an empty
// list, and an administrator whose list is full would go looking at the list.
func RefuseTerminal(loadout tool.Loadout, reason string) {
	name := terminalSchema().Name
	if _, ok := loadout.Handlers[name]; !ok {
		return
	}
	loadout.Handlers[name] = func(context.Context, tool.Call) (tool.Result, error) {
		return toolkit.Blocked(reason)
	}
}

// inThisConversation adds which conversation a call belongs to.
//
// The computer needs it for one rule: a file may not be overwritten unless it
// was READ first, and "first" means in this conversation rather than at some
// point last week. The two halves of that rule live apart on purpose. Whether
// the file exists, and which path is which once symlinks and `..` are settled,
// is a question only the machine can answer; which conversation is asking is a
// question only the gateway can. So the conversation travels and the machine
// remembers.
//
// It is added to the arguments rather than declared in the schema, so no model
// ever sees it, and it is written LAST, so a model that invented one of its own
// cannot pass it off. A tool that has no use for it ignores it, which is what
// the reader on the other side does with a field it does not know.
func inThisConversation(call tool.Call) json.RawMessage {
	if call.SessionID == 0 {
		return call.Args
	}
	args := map[string]json.RawMessage{}
	if len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &args); err != nil {
			// Arguments that are not an object are the model's mistake to hear
			// about from the tool, not something to rewrite on the way past.
			return call.Args
		}
	}
	args["conversation"] = json.RawMessage(strconv.FormatInt(call.SessionID, 10))
	carried, err := json.Marshal(args)
	if err != nil {
		return call.Args
	}
	return carried
}
