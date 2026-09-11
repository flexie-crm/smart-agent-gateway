package supervise

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// lingerEnv turns this test binary into a process that exists and does nothing,
// which is what these tests need to ask questions about.
//
// A real process rather than a fake one, because the thing under test IS the
// operating system call: everything else in this package is tested through
// fakeProc precisely so the decisions can be checked without spawning anything,
// and these two functions are the exception where spawning is the point. There
// is also no portable command to reach for. `sleep 120` is not a program on
// Windows and `timeout` is not one on macOS.
const lingerEnv = "SAG_SUPERVISE_LINGER"

func TestMain(m *testing.M) {
	if os.Getenv(lingerEnv) == "1" {
		// Bounded, so a test that fails before its cleanup runs does not leave a
		// process sitting on this machine until somebody notices.
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func linger(t *testing.T) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self) //nolint:gosec // this very process
	cmd.Env = append(os.Environ(), lingerEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// TestRunningSeesALiveProcessAndThenSeesItGo is a small test guarding a bug that
// was not small.
//
// The unix way to ask whether a process is alive is to send it signal 0. Ported
// to Windows unchanged it does not merely fail to work, it answers CONFIDENTLY
// AND WRONGLY: os.Process.Signal refuses every signal but Kill, so a process
// running perfectly well comes back as gone. The caller is localdb, deciding
// whether a database from a previous run is still holding the data directory,
// and being told "no" when the answer is yes means walking past it, deleting its
// pid file, and then failing to start for as long as the machine stays up.
func TestRunningSeesALiveProcessAndThenSeesItGo(t *testing.T) {
	cmd := linger(t)
	pid := cmd.Process.Pid

	if !Running(pid) {
		t.Fatal("a process that is running was reported as gone")
	}

	if err := EndByID(pid); err != nil {
		t.Fatalf("ending it: %v", err)
	}
	// Waited on before asking again, and that matters on unix: a killed child is
	// a zombie until its parent reaps it, and a zombie still answers signal 0.
	_ = cmd.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for Running(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if Running(pid) {
		t.Fatal("a process that has ended is still reported as running")
	}
}

// TestEndingWorksOnAProcessWeDidNotStart guards the second half of the same
// port, which is about access rights rather than about signals.
//
// The reflex is os.FindProcess followed by Kill, and on Windows that fails: the
// handle os.FindProcess opens carries read and query rights only, and
// TerminateProcess is refused through it whoever owns the process, because the
// right is checked against the handle and not against the account. The caller
// here is the last resort for a database that will not go, so a Kill that
// cannot kill leaves an installation permanently unable to start.
func TestEndingWorksOnAProcessWeDidNotStart(t *testing.T) {
	cmd := linger(t)
	pid := cmd.Process.Pid

	// By id, with no reference to the exec.Cmd that started it: this is what the
	// caller has, which is a number it read out of a file.
	if err := EndByID(pid); err != nil {
		t.Fatalf("ending a process by its id: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("it exited normally, so it was not the ending that stopped it")
	}
}
