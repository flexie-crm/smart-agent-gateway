package run

import (
	"context"
	"testing"
	"time"

	"flexie.io/sag/internal/chat"
)

// The run's log and its fan-out, tested without a database: these are the rules
// that make "close the tab and come back" work, and they are worth reading on
// their own.

func testRun() *Run {
	return newRun(1, "run_test", 1, 1, 1, func() {})
}

func delta(text string) chat.Frame {
	return chat.Frame{Type: chat.FrameDelta, Message: text}
}

// A reader that arrives late gets everything it missed, and then the rest as it
// arrives. It cannot tell where one ends and the other begins, which is the
// point: a reconnect should look like nothing happened.
func TestALateReaderGetsTheWholeAnswer(t *testing.T) {
	r := testRun()

	// The turn starts talking before anyone is listening.
	_ = r.Write(delta("Hello"))
	_ = r.Write(delta(", "))

	frames, detach := r.Subscribe(-1)
	defer detach()

	// And carries on afterwards.
	_ = r.Write(delta("world"))
	_ = r.Write(chat.Frame{Type: chat.FrameResult, Final: true, Message: "Hello, world"})

	var text string
	for frame := range frames {
		if frame.Type == chat.FrameDelta {
			text += frame.Message.(string)
		}
	}
	if text != "Hello, world" {
		t.Fatalf("the late reader missed part of the answer: %q", text)
	}
}

// A reader that was in the middle of the answer picks up exactly where it left
// off. It says the last index it saw, and it gets what came after it: no gap,
// and no word twice.
func TestAReaderResumesFromWhereItStopped(t *testing.T) {
	r := testRun()
	_ = r.Write(delta("one "))
	_ = r.Write(delta("two "))
	_ = r.Write(delta("three"))

	// It saw the first two frames (indexes 0 and 1) and then the connection
	// dropped.
	frames, detach := r.Subscribe(1)
	defer detach()

	select {
	case frame := <-frames:
		if frame.Index != 2 || frame.Message.(string) != "three" {
			t.Fatalf("the reader was given the wrong frame back: %+v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("the reader was given nothing")
	}

	select {
	case frame := <-frames:
		t.Fatalf("the reader was sent a frame it had already seen: %+v", frame)
	default:
	}
}

// Frames are numbered as they are logged, and the number is what a client holds
// on to. Without it there is no way to say "I got this far".
func TestFramesAreNumbered(t *testing.T) {
	r := testRun()
	for i := 0; i < 3; i++ {
		_ = r.Write(delta("x"))
	}

	for i, frame := range r.Frames() {
		if frame.Index != i {
			t.Fatalf("frame %d is numbered %d", i, frame.Index)
		}
	}
}

// Two readers of one turn both hear all of it. A second tab is not a second
// turn.
func TestTwoReadersBothHearTheAnswer(t *testing.T) {
	r := testRun()

	first, detachFirst := r.Subscribe(-1)
	defer detachFirst()
	second, detachSecond := r.Subscribe(-1)
	defer detachSecond()

	_ = r.Write(delta("shared"))
	_ = r.Write(chat.Frame{Type: chat.FrameResult, Final: true})

	for name, frames := range map[string]<-chan chat.Frame{"first": first, "second": second} {
		var seen int
		for range frames {
			seen++
		}
		if seen != 2 {
			t.Fatalf("the %s reader saw %d frames, not both", name, seen)
		}
	}
}

// A reader leaving does not end the turn. This is the whole reason the package
// exists: the browser tab does not own the vendor call.
func TestAReaderLeavingDoesNotEndTheTurn(t *testing.T) {
	r := testRun()

	_, detach := r.Subscribe(-1)
	detach()

	if r.Done() {
		t.Fatal("the turn ended because its only reader left")
	}
	if err := r.Write(delta("still talking")); err != nil {
		t.Fatalf("the turn could not carry on: %v", err)
	}
	if len(r.Frames()) != 1 {
		t.Fatal("the frame written after the reader left was dropped")
	}

	// And a reader coming back finds it.
	frames, _ := r.Subscribe(-1)
	frame := <-frames
	if frame.Message.(string) != "still talking" {
		t.Fatalf("the returning reader missed what was said: %+v", frame)
	}
}

// A reader that cannot keep up is dropped rather than allowed to stall the
// turn. Backpressure from a slow browser must never reach the model, and
// dropping is safe: the log kept everything, so the reader reattaches and
// replays.
func TestASlowReaderIsDroppedRatherThanStallingTheTurn(t *testing.T) {
	r := testRun()

	frames, detach := r.Subscribe(-1)
	defer detach()

	// Fill its buffer and then some, without ever reading a single frame.
	const written = subscriberBuffer + 10
	for i := 0; i < written; i++ {
		if err := r.Write(delta("x")); err != nil {
			t.Fatalf("the turn was stalled by a reader that stopped reading: %v", err)
		}
	}

	// Every frame is still in the log. Nothing was lost, only the slow reader.
	if len(r.Frames()) != written {
		t.Fatalf("frames were dropped to keep up with a slow reader: %d", len(r.Frames()))
	}

	// The slow reader was let go: its channel is closed, so draining it ends.
	var received int
	for range frames {
		received++
	}
	if received >= written {
		t.Fatal("the slow reader was not dropped, so the turn was at its mercy")
	}

	// And it can come back for everything, including what it never received.
	_ = r.Write(chat.Frame{Type: chat.FrameResult, Final: true})

	again, _ := r.Subscribe(-1)
	var replayed int
	for range again {
		replayed++
	}
	if replayed != written+1 {
		t.Fatalf("the dropped reader could not replay the turn: got %d of %d", replayed, written+1)
	}
}

// A run that has finished is still readable, so a page reloaded a moment after
// the answer landed renders it without waiting for anything.
func TestAFinishedRunStillReplays(t *testing.T) {
	r := testRun()
	_ = r.Write(delta("done"))
	_ = r.Write(chat.Frame{Type: chat.FrameResult, Final: true, Message: "done"})

	if !r.Done() {
		t.Fatal("a final frame did not end the run")
	}

	frames, _ := r.Subscribe(-1)
	var seen int
	for range frames {
		seen++
	}
	if seen != 2 {
		t.Fatalf("the finished run did not replay: %d frames", seen)
	}
}

// A frame written after the turn ended is dropped. A late goroutine must not
// append to an answer the user has already been told is complete.
func TestNothingIsAppendedToAFinishedTurn(t *testing.T) {
	r := testRun()
	_ = r.Write(chat.Frame{Type: chat.FrameResult, Final: true})
	_ = r.Write(delta("too late"))

	if len(r.Frames()) != 1 {
		t.Fatalf("a frame was appended to a finished turn: %+v", r.Frames())
	}
}

// Cancel is what a person pressing stop means. It is the only thing that means
// it, now that a reader disconnecting does not.
func TestCancelStopsTheTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := newRun(1, "run_test", 1, 1, 1, cancel)

	r.Cancel()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancelling the run did not cancel the work")
	}
}
