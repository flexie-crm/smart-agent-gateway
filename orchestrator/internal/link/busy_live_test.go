package link

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/linktest"
)

// A tool that takes a while on the person's computer must not silence the link
// carrying it (KB/29).
//
// Found in use: a search over a large folder froze the chat application's link
// for as long as the search ran, the gateway's heartbeat went unanswered, and
// the gateway dropped a computer that was sitting right there. Every call made
// meanwhile failed with "not connected", and the application's own indicator
// stayed green, because the part of it that would have turned it grey was the
// part that was frozen.
//
// The REAL client runs a REAL search, and while it works the same computer is
// asked the time, over and over. A computer that answers is not frozen; one
// whose answers wait as long as the search does is.
//
// The client runs with ONE worker thread. On the application's own runtime the
// same fault froze the link in some runs and not in others (which thread
// happens to be minding the network decides it), and a gate that catches a
// regression only some of the time passes it on a lucky day. With one thread
// there is no luck in it: work that holds the thread holds all of it.
func TestALongToolDoesNotSilenceTheLink(t *testing.T) {
	linktest.Skip(t)
	t.Setenv("TOKIO_WORKER_THREADS", "1")

	// Something to search: enough that searching it takes seconds even on a
	// fast disk, and nothing in it that matches, so every byte is read.
	tree := t.TempDir()
	line := strings.Repeat("nothing to find here, only something to read past. ", 20) + "\n"
	page := []byte(strings.Repeat(line, 40))
	for d := 0; d < 40; d++ {
		dir := filepath.Join(tree, fmt.Sprintf("d%02d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("make the tree: %v", err)
		}
		for f := 0; f < 100; f++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", f)), page, 0o644); err != nil {
				t.Fatalf("make the tree: %v", err)
			}
		}
	}

	r := NewRegistry(zerolog.Nop(), func(token string) (int64, int64, string, time.Time, bool) {
		if token == "a-real-looking-token" {
			return 11, 5, "the-laptop", time.Now().Add(time.Hour), true
		}
		return 0, 0, "", time.Now().Add(time.Hour), false
	}, []string{"*"})
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/link", r.ServeControl)
	mux.HandleFunc("/v1/link/stream", r.ServeStream)
	gateway := newRestartableServer(t, mux)
	defer gateway.stop()
	client := startRustClient(t, gateway.url, "a-real-looking-token")
	defer client.Stop()
	waitFor(t, "the real client to link", func() bool { return r.Online(5, 11, "the-laptop") })

	// Whether the gateway ever lost the computer, watched on its own so that a
	// probe waiting out a frozen link cannot hide it.
	var lost atomic.Bool
	watching := make(chan struct{})
	defer close(watching)
	go func() {
		for {
			select {
			case <-watching:
				return
			case <-time.After(100 * time.Millisecond):
				if !r.Online(5, 11, "the-laptop") {
					lost.Store(true)
				}
			}
		}
	}()

	args, err := json.Marshal(map[string]string{"pattern": "zqxjv", "folder": tree})
	if err != nil {
		t.Fatalf("the arguments: %v", err)
	}
	type searched struct {
		took   time.Duration
		answer Result
		err    error
	}
	done := make(chan searched, 1)
	began := time.Now()
	go func() {
		answer, err := r.Call(context.Background(), 5, 11, "the-laptop", "search_files", args, "a test")
		done <- searched{time.Since(began), answer, err}
	}()

	var worst time.Duration
	answered := 0
	for {
		select {
		case got := <-done:
			if got.err != nil || !got.answer.OK {
				t.Fatalf("the search itself failed, so this proves nothing: %v %s", got.err, got.answer.Message)
			}
			// Too quick to tell a frozen link from a free one: the probe
			// below could not have waited long whatever the code did.
			if got.took < 2*time.Second {
				t.Fatalf("the search took %v, too little to show anything; give it more to read", got.took)
			}
			if lost.Load() {
				t.Errorf("the gateway lost the computer while it searched")
			}
			if worst > time.Second {
				t.Errorf("while the search ran for %v, asking the same computer the time took up to %v: "+
					"the search held the thread the link answers on", got.took, worst)
			}
			// About ten a second when nothing is in the way.
			if want := int(got.took.Seconds() * 3); answered < want {
				t.Errorf("only %d questions were answered while the search ran for %v, where %d would show "+
					"a link that was free", answered, got.took, want)
			}
			t.Logf("the search took %v; %d questions answered meanwhile, the slowest in %v", got.took, answered, worst)
			return
		default:
		}
		asked := time.Now()
		if _, err := r.Call(context.Background(), 5, 11, "the-laptop", "current_time", []byte(`{}`), "a test"); err == nil {
			answered++
		}
		if took := time.Since(asked); took > worst {
			worst = took
		}
		time.Sleep(100 * time.Millisecond)
	}
}
