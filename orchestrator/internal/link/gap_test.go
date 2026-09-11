package link

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/rs/zerolog"
)

// What happens to work in flight when the application is away for a moment.
//
// It goes away routinely and briefly: its credential lasts an hour and is
// renewed, a laptop's wifi hands over, a VPN reconnects. The product's promise
// is that a call SURVIVES that (KB/39): the work is going on over there, and
// what bounds the wait is whether the machine comes back, not a stopwatch.
//
// It did not hold, and it cost somebody a `git push` mid-turn: the call was
// dispatched, the application went away before it dialled back, and twenty
// seconds later the turn was told "your computer did not answer in time" about
// a computer that was back within eight.
//
// These run in about a second each, because every wait in this package is a
// variable a test can shorten. The bug is in what happens when a machine goes
// away, and a test that takes three minutes to make it go away is a test that
// is never run while looking for it.

// quickly shortens the waits for one test and puts them back afterwards.
func quickly(t *testing.T) {
	t.Helper()
	dial, back, look := dialDeadline, comesBackWithin, lookForItEvery
	dialDeadline, comesBackWithin, lookForItEvery = 200*time.Millisecond, 3*time.Second, 20*time.Millisecond
	t.Cleanup(func() { dialDeadline, comesBackWithin, lookForItEvery = dial, back, look })
}

// A call dispatched just as the application goes away waits for it to come back
// rather than giving up on the dial deadline.
func TestACallWaitsForAnApplicationThatIsComingBack(t *testing.T) {
	quickly(t)
	r, server := newTestRegistry(t)
	app := linkApp(t, server.URL, "good")
	waitOnline(t, r)

	// It goes away with the call in the air, and returns a moment later, which
	// is exactly what a credential renewal looks like from here.
	app.ignore = make(chan openRequest, 4)
	go func() {
		<-app.ignore               // the request the gateway sent while it was still there
		_ = app.control.CloseNow() // away
		time.Sleep(400 * time.Millisecond)
		back := linkApp(t, server.URL, "good") // and back, answering the call
		back.answerAfter = 10 * time.Millisecond
	}()

	started := time.Now()
	answer, err := r.Call(context.Background(), 3, 7, "the-laptop", "terminal",
		json.RawMessage(`{"command":"git push"}`), "a test")
	t.Logf("the call ended after %s: err=%v", time.Since(started), err)
	if err != nil {
		t.Fatalf("the call gave up on a computer that came back: %v", err)
	}
	if !answer.OK {
		t.Fatalf("the call was refused: %s", answer.Message)
	}
}

// And it does not wait for ever: a machine that stays away ends the call with
// the answer that says so, rather than hanging the turn.
func TestACallGivesUpOnAnApplicationThatStaysAway(t *testing.T) {
	quickly(t)
	r, server := newTestRegistry(t)
	app := linkApp(t, server.URL, "good")
	waitOnline(t, r)

	app.ignore = make(chan openRequest, 4)
	go func() {
		<-app.ignore
		_ = app.control.CloseNow()
	}()

	started := time.Now()
	_, err := r.Call(context.Background(), 3, 7, "the-laptop", "terminal",
		json.RawMessage(`{"command":"sleep 1"}`), "a test")
	if !errors.Is(err, ErrNoMachine) {
		t.Fatalf("a machine that never came back should end as absent, got %v", err)
	}
	if took := time.Since(started); took > 2*comesBackWithin {
		t.Fatalf("it waited %s, which is longer than it promised", took)
	}
}

// The application is asked for a new credential BEFORE the old one runs out.
//
// It used to be asked only after being refused, which is after this side had
// already closed the socket at expiry. That is a gap of seconds every hour, in
// the middle of whatever was running: the credential is renewed on a clock, so
// the gap landed wherever the clock happened to land, and one of them landed on
// a `git push`.
func TestTheApplicationIsAskedToRenewBeforeItsCredentialExpires(t *testing.T) {
	// A credential a second from running out, checked ten times a second, asked
	// for half a second before the end.
	was, wasBefore := recheck, renewBefore
	recheck, renewBefore = 100*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { recheck, renewBefore = was, wasBefore })

	expiry := time.Now().Add(time.Second)
	r := NewRegistry(zerolog.Nop(), func(token string) (int64, int64, string, time.Time, bool) {
		if token != "good" {
			return 0, 0, "", time.Time{}, false
		}
		return 7, 3, "the-laptop", expiry, true
	}, []string{"*"})
	mux := http.NewServeMux()
	mux.HandleFunc("/link", r.ServeControl)
	mux.HandleFunc("/link/stream", r.ServeStream)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	sock, res, err := websocket.Dial(context.Background(), wsURL(server.URL, "/link"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	closeHandshake(res)
	t.Cleanup(func() { _ = sock.CloseNow() })
	if err := wsjson.Write(context.Background(), sock, hello{Type: "authenticate", Token: "good"}); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	// The acknowledgement, then whatever the gateway says next.
	deadline, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var asked time.Time
	for {
		var message openRequest
		if err := wsjson.Read(deadline, sock, &message); err != nil {
			t.Fatalf("nothing asked for a renewal before the credential ran out: %v", err)
		}
		if message.Type == "renew" {
			asked = time.Now()
			break
		}
	}
	if !asked.Before(expiry) {
		t.Fatalf("the renewal was asked for at %s, after the credential ran out at %s", asked, expiry)
	}
	if left := expiry.Sub(asked); left < 300*time.Millisecond {
		t.Fatalf("only %s of the credential was left when the renewal was asked for", left)
	}
}
