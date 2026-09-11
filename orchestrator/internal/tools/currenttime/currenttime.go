// Package currenttime is the tool that tells the assistant what day and time it
// is. Models do not know, and asking them to guess produces confident nonsense.
//
// It reads the clock on the PERSON'S OWN COMPUTER when there is one to read.
// The gateway's clock is the wrong clock: it runs in UTC in a rack, so an
// assistant grounded in it told somebody in Tirana it was nine in the morning
// when their screen said eleven, and then could not convert it, because a
// server does not know where anybody is sitting. The computer in front of them
// knows its zone by name and its offset right now.
//
// One tool all the same, not a machine tool and a server one. The name, the
// grant and everything the model reads stay exactly as they were, and a
// conversation held in a browser still gets an answer: the handler asks the
// computer when the request came from one, and reads its own clock when it did
// not. What changes is not WHETHER there is an answer but whose clock it came
// from, which is why the answer always names the zone it was read in.
package currenttime

import (
	"context"
	"encoding/json"
	"time"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/machine"
	"flexie.io/sag/internal/tools/toolkit"
)

// Name is the tool's canonical name.
const Name = "current_time"

// version is which shape of this tool's answer the application must speak for
// us to ask it. The application declares its own when the link opens (KB/39),
// and one that speaks a different number is simply not asked: the server clock
// answers instead, which is the same fallback a browser gets.
//
// Read from the one table every machine tool's version lives in, rather than
// written again here. Two numbers for one contract is how the terminal came to
// be offered to nobody: the application moved and the gateway moved, and a
// third copy stayed where it was.
func version() int { return machine.VersionOf(Name) }

// Zones answers where somebody is, by IANA name, or nothing. It is how the
// server's own clock is read in the right place for a person with no computer
// to ask: a browser has no machine tools, but its OS still told us the zone
// when the turn arrived.
type Zones func(ctx context.Context, userID int64) string

// New builds the current-time tool.
//
// Both arguments may be nil, which is a deployment with no chat application and
// nothing remembered about anybody. Then this is what it always was: the
// server's clock, in UTC, saying so.
func New(machines machine.Machines, zones Zones) tool.Tool {
	return tool.Tool{
		Schema: Schema(),
		Handle: func(ctx context.Context, call tool.Call) (tool.Result, error) {
			// Their own computer first: it reads its OS clock and names its own
			// zone, which is the truest answer available and the only one that
			// is right when the two disagree.
			if answer, ok := theirClock(ctx, machines, call); ok {
				return toolkit.Success(answer)
			}
			// No computer to ask (a browser). Our clock, read in THEIR place
			// when we know it: the instant is ours either way, and where it is
			// read is what the question was about.
			return toolkit.Success(ourClock(time.Now(), placeOf(ctx, zones, call.UserID)))
		},
	}
}

// placeOf is where to read our own clock: the person's own zone when we have
// been told it, and UTC when we have not.
func placeOf(ctx context.Context, zones Zones, userID int64) *time.Location {
	if zones == nil || userID == 0 {
		return time.UTC
	}
	name := zones(ctx, userID)
	if name == "" {
		return time.UTC
	}
	place, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return place
}

// Schema is what the model is told, which does not change with whose clock
// answers: the assistant asks what time it is, and is told, in the zone of
// whoever is asking when that can be known.
func Schema() tool.Schema {
	return tool.Schema{
		Name:         Name,
		FriendlyName: "Current time",
		About: "Tells the agent today's date and the time right now, in the time zone of the person it is " +
			"talking to when they are using the chat application, and in UTC otherwise. Without it the agent " +
			"is working from whenever it was trained, so anything about today, this week, or a deadline is a guess.",
		FriendlyNarration: "Checking the time",
		Description: "Get the current date and time. In the chat application it is read from the " +
			"person's own computer, so it comes back as their local time with their zone named; " +
			"elsewhere it is UTC and says so. Answer in the zone it gives you.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Kind:        tool.KindBuiltin,
		Risk:        tool.RiskReadOnly,
		// What a person sees when they open one of these calls: the reading
		// where they are, and the zone it was read in. UTC is in the answer for
		// the model to compute across machines with, and is not worth a row in
		// front of somebody who asked what time it is.
		Shown: tool.Display{
			Answered: []tool.Shown{
				tool.Value("local"), tool.Value("zone"), tool.Value("weekday"),
			},
		},
	}
}

// theirClock reads the clock on the computer the request came from.
//
// Three things have to be true, and none of them is an error when it is not:
// there is a link at all, the request came from an installation of the chat
// application rather than a browser, and that installation speaks this tool.
// Any of them missing means our own clock answers, so the assistant is never
// left unable to say what day it is because somebody's laptop is a release
// behind.
func theirClock(ctx context.Context, machines machine.Machines, call tool.Call) (json.RawMessage, bool) {
	if machines == nil || call.DeviceID == "" {
		return nil, false
	}
	if machines.Runs(call.WorkspaceID, call.UserID, call.DeviceID)[Name] != version() {
		return nil, false
	}
	answer, err := machines.Call(ctx, call.WorkspaceID, call.UserID, call.DeviceID,
		Name, json.RawMessage(`{}`), "Current time")
	if err != nil || !answer.OK || len(answer.Content) == 0 {
		// A computer that did not answer is not a reason to fail: the question
		// is what time it is, and we know a true answer to it.
		return nil, false
	}
	return answer.Content, true
}

// ourClock is the gateway's own instant, read in the person's place.
//
// The same field names the application answers with, because the model reads
// one shape whichever clock was read, and the zone is always NAMED: a reading
// labelled UTC is never mistaken for somebody's local time.
func ourClock(now time.Time, place *time.Location) map[string]any {
	t := now.In(place)
	return map[string]any{
		"local":   t.Format("2006-01-02T15:04:05Z07:00"),
		"zone":    place.String(),
		"weekday": t.Weekday().String(),
		"utc":     now.UTC().Format("2006-01-02T15:04:05Z"),
	}
}
