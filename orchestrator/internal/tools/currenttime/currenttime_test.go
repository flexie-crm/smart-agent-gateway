package currenttime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"flexie.io/sag/internal/link"
	"flexie.io/sag/internal/tool"
)

// The clock that answers is the one in front of the PERSON, when there is one.
func TestItReadsTheClockOnTheirOwnComputer(t *testing.T) {
	theirs := &computer{
		runs:   map[string]int{Name: version()},
		answer: `{"local":"2026-09-04T11:15:16+02:00","zone":"Europe/Tirane","weekday":"Friday","utc":"2026-09-04T09:15:16Z"}`,
	}
	answered := call(t, New(theirs, nowhere), tool.Call{DeviceID: "the-laptop", WorkspaceID: 5, UserID: 11})

	if answered["zone"] != "Europe/Tirane" || answered["local"] != "2026-09-04T11:15:16+02:00" {
		t.Fatalf("the person was given a clock that is not theirs: %v", answered)
	}
	if theirs.asked != Name {
		t.Fatalf("their computer was not asked: %q", theirs.asked)
	}
}

// A browser has no computer to ask, so our own clock answers, read in the
// person's own place: their OS told us the zone when the turn arrived, and the
// instant is the same instant wherever it is read.
func TestABrowserIsAnsweredWhereThePersonIs(t *testing.T) {
	theirs := &computer{runs: map[string]int{Name: version()}}
	answered := call(t, New(theirs, inTirana), tool.Call{WorkspaceID: 5, UserID: 11}) // no device

	if answered["zone"] != "Europe/Tirane" {
		t.Fatalf("the browser was answered in the rack's zone: %v", answered)
	}
	if theirs.asked != "" {
		t.Fatalf("something was asked of a computer that is not there: %q", theirs.asked)
	}
}

// And when nothing is known about where they are, it is UTC and says UTC,
// rather than a server reading passed off as somebody's local time.
func TestNothingKnownIsUTCAndSaysSo(t *testing.T) {
	answered := call(t, New(nil, nowhere), tool.Call{UserID: 11})
	if answered["zone"] != "UTC" {
		t.Fatalf("a server reading was passed off as a local one: %v", answered)
	}
}

// An application a release behind does not break the question. It declares
// which version of each tool it speaks, and one that does not speak this is not
// asked: the server clock answers, exactly as it does for a browser.
func TestAnOlderApplicationIsNotAsked(t *testing.T) {
	theirs := &computer{runs: map[string]int{Name: version() + 1}, answer: `{"zone":"Europe/Tirane"}`}
	answered := call(t, New(theirs, nowhere), tool.Call{DeviceID: "the-laptop", WorkspaceID: 5, UserID: 11})

	if answered["zone"] != "UTC" {
		t.Fatalf("a version we do not speak was asked anyway: %v", answered)
	}
}

// And a computer that fails to answer is not a failed tool call. The question
// is what time it is, and we know a true answer to it.
func TestAComputerThatWillNotAnswerFallsBack(t *testing.T) {
	theirs := &computer{runs: map[string]int{Name: version()}, refuse: true}
	answered := call(t, New(theirs, nowhere), tool.Call{DeviceID: "the-laptop", WorkspaceID: 5, UserID: 11})

	if answered["zone"] != "UTC" {
		t.Fatalf("a computer that would not answer left the assistant with nothing: %v", answered)
	}
}

// A deployment with no chat application at all still tells the time.
func TestNoLinkAtAllStillAnswers(t *testing.T) {
	answered := call(t, New(nil, nowhere), tool.Call{DeviceID: "the-laptop"})
	if answered["zone"] != "UTC" || answered["weekday"] == "" {
		t.Fatalf("a gateway with no link could not say what time it is: %v", answered)
	}
}

// The server's own reading says the same things the application's does, because
// the model reads one shape whichever clock was read.
func TestBothClocksAnswerTheSameShape(t *testing.T) {
	ours := ourClock(time.Date(2026, 9, 4, 9, 15, 16, 0, time.UTC), time.UTC)
	for _, field := range []string{"local", "zone", "weekday", "utc"} {
		if _, there := ours[field]; !there {
			t.Fatalf("the server clock answers without %q: %v", field, ours)
		}
	}
	if ours["utc"] != "2026-09-04T09:15:16Z" || ours["weekday"] != "Friday" {
		t.Fatalf("the server clock misread its own instant: %v", ours)
	}
}

// nowhere knows nothing about anybody; inTirana knows where this person is.
func nowhere(context.Context, int64) string  { return "" }
func inTirana(context.Context, int64) string { return "Europe/Tirane" }

func call(t *testing.T, tl tool.Tool, c tool.Call) map[string]any {
	t.Helper()
	result, err := tl.Handle(context.Background(), c)
	if err != nil {
		t.Fatalf("the tool failed: %v", err)
	}
	var answered map[string]any
	if err := json.Unmarshal(result.Content, &answered); err != nil {
		t.Fatalf("the answer is not readable: %v (%s)", err, result.Content)
	}
	return answered
}

// computer stands in for the person's own, and records what it was asked.
type computer struct {
	runs   map[string]int
	answer string
	refuse bool
	asked  string
}

func (c *computer) Call(
	_ context.Context, _, _ int64, _, name string, _ json.RawMessage, _ string,
) (link.Result, error) {
	c.asked = name
	if c.refuse {
		return link.Result{OK: false, Message: "not now"}, nil
	}
	return link.Result{OK: true, Content: json.RawMessage(c.answer)}, nil
}

func (c *computer) Runs(_, _ int64, deviceID string) map[string]int {
	if deviceID == "" {
		return nil
	}
	return c.runs
}
