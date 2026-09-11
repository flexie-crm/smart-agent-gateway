package link

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// A credential running out under a call that is in flight, through the REAL
// client.
//
// This is the failure as it happened, reproduced end to end. A link credential
// lasts an hour. The application only ever asked for another after being
// refused, which is after the gateway had already closed the socket at expiry,
// so every hour there was a gap of seconds wherever the clock happened to land.
// One of them landed on a `git push`: twenty seconds later the turn was told
// "your computer did not answer in time" about a computer that was back within
// eight.
//
// Two halves have to hold for this to pass, and each was broken on its own:
// the gateway asks for a fresh credential BEFORE the old one runs out, and a
// call in flight survives the reconnection that follows.
func TestACallSurvivesItsCredentialRunningOut(t *testing.T) {
	if os.Getenv("SAG_LINK_E2E") == "" {
		t.Skip("SAG_LINK_E2E not set; skipping the cross-language link gate (make link-e2e)")
	}

	// Seconds rather than an hour, which is the only reason this is a test
	// somebody will run rather than a thing nobody ever checks.
	wasRecheck, wasRenew := recheck, renewBefore
	recheck, renewBefore = 100*time.Millisecond, 2*time.Second
	t.Cleanup(func() { recheck, renewBefore = wasRecheck, wasRenew })

	// The first credential runs out three seconds in; the one the application
	// is handed when it asks does not.
	runsOut := time.Now().Add(3 * time.Second)
	r := NewRegistry(zerolog.Nop(), func(token string) (int64, int64, string, time.Time, bool) {
		switch token {
		case "first":
			if time.Now().After(runsOut) {
				return 0, 0, "", time.Time{}, false // expired, exactly as the real one stops parsing
			}
			return 11, 5, "the-laptop", runsOut, true
		case "second":
			return 11, 5, "the-laptop", time.Now().Add(time.Hour), true
		}
		return 0, 0, "", time.Time{}, false
	}, []string{"*"})

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/link", r.ServeControl)
	mux.HandleFunc("/v1/link/stream", r.ServeStream)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, heard := startRustClientRenewing(t, server.URL, "first", "second")
	t.Cleanup(client.stop)
	waitUntil(t, func() bool { return r.Online(5, 11, "the-laptop") })

	if runs := r.Runs(5, 11, "the-laptop"); runs["terminal"] == 0 {
		t.Skip("this build of the application has no terminal")
	}

	// A command that outlives the credential it started under.
	started := time.Now()
	answer, err := r.Call(context.Background(), 5, 11, "the-laptop", "terminal",
		json.RawMessage(`{"command":"sleep 6; echo survived","wait":30,"conversation":404}`),
		"a test")
	if err != nil {
		t.Fatalf("a call did not survive its credential running out: %v", err)
	}
	if !answer.OK {
		t.Fatalf("the terminal refused: %s", answer.Message)
	}
	var content struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(answer.Content, &content); err != nil {
		t.Fatalf("the answer is unreadable: %v (%s)", err, answer.Content)
	}
	if !strings.Contains(content.Output, "survived") {
		t.Fatalf("the command did not finish: %q", content.Output)
	}
	if took := time.Since(started); took < 5*time.Second {
		t.Fatalf("the command cannot have run: it took %s", took)
	}

	// And the link is up on the SECOND credential.
	if !r.Online(5, 11, "the-laptop") {
		t.Fatal("the link is not up after the renewal")
	}
	if time.Now().Before(runsOut) {
		t.Fatalf("the test finished before the first credential could even expire")
	}

	// The renewal happened, and it happened WITHOUT the link going down.
	//
	// Asserted separately from the call surviving, because the two fixes cover
	// for each other: a call that waits for a machine to come back would carry
	// this test even if the credential were still being renewed only after a
	// refusal, and the gap would still be there in front of everybody else's
	// work. What is being proved here is that there is no gap.
	askedAt, wasAsked := heard.firstAt("needs-token")
	if !wasAsked {
		t.Fatalf("the application was never asked to renew:\n%s", heard.all())
	}
	// The whole of the fix, in one comparison. Asked BEFORE the credential ran
	// out is a renewal at a moment of our choosing; asked after is the gap that
	// used to land in the middle of somebody's work, every hour, because the
	// only thing that ever prompted an application to renew was being refused.
	if !askedAt.Before(runsOut) {
		t.Fatalf("the renewal was asked for %s AFTER the credential ran out, which is the gap:\n%s",
			askedAt.Sub(runsOut), heard.all())
	}
	if !heard.contains("reconnecting on it") {
		t.Fatalf("the application was given a fresh credential and did not reconnect on it:\n%s", heard.all())
	}
	// And it SAID the link went down while it reconnected.
	//
	// A renewal is a reconnection, so a moment of down is honest and expected.
	// What was not honest is what happened before: the connection ended with an
	// early return that skipped the announcement entirely, so the page was never
	// told, and the indicator sat there green through an outage while calls
	// failed underneath it. Somebody watched exactly that and asked why.
	if !heard.contains("link down") {
		t.Fatalf("the link reconnected without telling anybody it had gone, which is how "+
			"the indicator stayed green through an outage:\n%s", heard.all())
	}
	if !heard.contains("link up") {
		t.Fatalf("the link never came back up:\n%s", heard.all())
	}
}
