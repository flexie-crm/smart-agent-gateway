package link

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// A turn stopped while a command runs on the person's computer ends with the
// turn's own error and no retry; a computer that really goes away is still
// reported as the computer.

// syncBuffer is a log the registry writes from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// loggedRegistry is newTestRegistry with a log that can be read afterwards, so
// a retry is seen rather than inferred.
func loggedRegistry(t *testing.T) (*Registry, *httptest.Server, *syncBuffer) {
	t.Helper()
	logged := &syncBuffer{}
	r := NewRegistry(zerolog.New(logged), func(token string) (int64, int64, string, time.Time, bool) {
		if token == "good" {
			return 7, 3, "the-laptop", time.Now().Add(time.Hour), true
		}
		return 0, 0, "", time.Now().Add(time.Hour), false
	}, []string{"*"})
	mux := http.NewServeMux()
	mux.HandleFunc("/link", r.ServeControl)
	mux.HandleFunc("/link/stream", r.ServeStream)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return r, server, logged
}

func TestAStoppedTurnIsNotTheComputerFailingToAnswer(t *testing.T) {
	quickly(t)
	r, server, logged := loggedRegistry(t)
	app := linkApp(t, server.URL, "good")
	app.answerAfter = time.Second // busy with a command
	waitOnline(t, r)

	ctx, stop := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, stop) // the person presses stop

	_, err := r.Call(ctx, 3, 7, "the-laptop", "terminal",
		json.RawMessage(`{"command":"make release"}`), "a test")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a stopped turn came back as %v, not as the turn being stopped", err)
	}
	if errors.Is(err, ErrTimeout) || errors.Is(err, ErrNoMachine) {
		t.Fatalf("a stopped turn was blamed on the computer: %v", err)
	}
	if !r.Online(3, 7, "the-laptop") {
		t.Fatal("the computer went away during the test, so it proves nothing")
	}
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "asking again") || strings.Contains(line, "asking it again") ||
			strings.Contains(line, "waiting for it") {
			t.Fatalf("a stopped turn's call was tried again: %s", line)
		}
	}
}

func TestAComputerThatGoesAwayIsStillTheComputer(t *testing.T) {
	quickly(t)
	back := comesBackWithin
	comesBackWithin = 500 * time.Millisecond
	t.Cleanup(func() { comesBackWithin = back })
	r, server, _ := loggedRegistry(t)
	app := linkApp(t, server.URL, "good")
	app.answerAfter = time.Second
	waitOnline(t, r)

	time.AfterFunc(300*time.Millisecond, func() { _ = app.control.CloseNow() })

	_, err := r.Call(context.Background(), 3, 7, "the-laptop", "terminal",
		json.RawMessage(`{"command":"make release"}`), "a test")

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a computer that went away was blamed on the turn: %v", err)
	}
	if !errors.Is(err, ErrNoMachine) {
		t.Fatalf("a computer that went away and did not come back was reported as %v", err)
	}
}
