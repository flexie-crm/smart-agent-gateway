// Package linktest starts the REAL chat application's link client, so that a
// test in any package can drive the far end of the machine link rather than a
// stand-in written from the same assumptions as the code under test.
//
// It is a package rather than a helper in one suite's _test.go because two
// suites need it and they are proving different things. internal/link's own
// gate proves the TRANSPORT: that bytes, half-closes, big answers and
// reconnections survive the whole route. The skills gate proves something the
// transport cannot see, that a fleet of agents reaching for one uninstalled
// skill puts it on the computer ONCE, and that is a property of the gateway
// which only a real client and a real disk can show: the client's staging
// directory is named for the version alone, so a gate that does not hold makes
// the real application fail on a real file rather than making a counter wrong.
//
// Nothing here fakes anything. It finds the binary the build produced, runs it
// with the environment the application runs with, and hands back the folders it
// was told to use so a test can look at what really landed.
package linktest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Routes is the pair of endpoints the application dials. Taking them as an
// interface keeps this package independent of internal/link, which matters
// because the packages that import link are the ones that need this.
type Routes interface {
	ServeControl(w http.ResponseWriter, r *http.Request)
	ServeStream(w http.ResponseWriter, r *http.Request)
}

// Skip leaves a test alone unless it was asked for by name.
//
// The client is a separate build in another language: a suite that ran it by
// default would fail on a machine with no Rust toolchain, for a reason that has
// nothing to do with the change being tested. `make link-e2e` sets this.
func Skip(t *testing.T) {
	t.Helper()
	if os.Getenv("SAG_LINK_E2E") == "" {
		t.Skip("SAG_LINK_E2E not set; skipping the cross-language link gate (make link-e2e)")
	}
}

// Serve carries the two link routes on a real HTTP server and answers with its
// address.
func Serve(t *testing.T, routes Routes) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/link", routes.ServeControl)
	mux.HandleFunc("/v1/link/stream", routes.ServeStream)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

// Client is the application, running.
type Client struct {
	// State is where it keeps what it remembers between runs, which is where
	// an installed skill lands: <State>/.sag/skill/<handle>/<version>/.
	State string
	// Folder is the working folder a person would have chosen with their own
	// folder dialog. A test cannot open a dialog, so it is given one.
	Folder string

	cmd *exec.Cmd
	// end kills the running client. It is a cancelled context rather than a
	// signal because that is what exec offers as the supported way to end a
	// child, and it means the process cannot outlive the test that started it
	// even if a Wait is missed.
	end   context.CancelFunc
	again func() (*exec.Cmd, context.CancelFunc)
}

// Start runs the built client against a gateway, linking as token.
func Start(t *testing.T, base, token string) *Client {
	return StartSaying(t, base, token, nil)
}

// StartSaying is Start with somewhere to collect what the client printed, for
// the tests that assert on WHEN it said something.
func StartSaying(t *testing.T, base, token string, out io.Writer) *Client {
	t.Helper()
	root, err := repository()
	if err != nil {
		t.Fatalf("find the repository: %v", err)
	}
	binary := filepath.Join(root, "desktop", "target", "debug", "examples", "link_client")
	if runtime.GOOS == "windows" {
		// What cargo names a program there. Without it every Windows machine
		// was told the client had not been built, and this gate never ran.
		binary += ".exe"
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("the client has not been built (%v); make link-e2e builds it", err)
	}

	c := &Client{State: t.TempDir(), Folder: t.TempDir()}
	if out == nil {
		out = os.Stderr // its own words, where a failing test shows them
	}
	env := append(os.Environ(),
		"SAG_LINK_URL="+base, "SAG_LINK_TOKEN="+token,
		"SAG_LINK_STATE="+c.State, "SAG_LINK_FOLDER="+c.Folder)
	c.again = func() (*exec.Cmd, context.CancelFunc) {
		ctx, end := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, binary)
		cmd.Env = env
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Start(); err != nil {
			end()
			t.Fatalf("start the client: %v", err)
		}
		return cmd, end
	}
	c.cmd, c.end = c.again()
	t.Cleanup(c.Stop)
	return c
}

// Quit ends the application the way quitting it does, and Reopen starts it
// again. Together they are the most common interruption there is: somebody
// closing the chat application, or a new version being installed.
func (c *Client) Quit() {
	if c.end != nil {
		c.end()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_, _ = c.cmd.Process.Wait()
	}
}

// Reopen starts it again, on the same folders, which is what makes it the same
// installation rather than a new one.
func (c *Client) Reopen() { c.cmd, c.end = c.again() }

// Stop ends it for good. Called by Start's cleanup, and safe to call twice.
func (c *Client) Stop() {
	if c.end != nil {
		c.end()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Wait()
	}
}

// Await polls until what is being waited for is true, and fails saying what it
// was rather than timing the whole test out.
func Await(t *testing.T, what string, until func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if until() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waited 20s for %s", what)
}

// repository is the root of the checkout, found by walking up from the test's
// own directory until the desktop workspace is there.
//
// Walked rather than a fixed number of ".." so this works from any package's
// directory: the two callers are three and four levels deep, and a constant
// would be right for one of them.
func repository() (string, error) {
	at, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(at, "desktop", "Cargo.toml")); err == nil {
			return at, nil
		}
		up := filepath.Dir(at)
		if up == at {
			return "", os.ErrNotExist
		}
		at = up
	}
}
