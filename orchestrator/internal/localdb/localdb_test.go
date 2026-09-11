package localdb

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/rs/zerolog"

	"flexie.io/sag/internal/supervise"
)

// bundle is a real MariaDB bundle: desktop/personal/mariadb/bundle-macos.sh on a
// Mac, desktop/personal/windows/mariadb.ps1 on Windows. Without one the
// integration tests here have nothing to run, so they skip; the unit tests below
// do not need it.
func bundle(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("SAG_DB_BUNDLE")
	if dir == "" {
		t.Skip("SAG_DB_BUNDLE not set; build one with desktop/personal/mariadb/bundle-macos.sh" +
			" or desktop/personal/windows/mariadb.ps1")
	}
	return dir
}

// fakeServerEnv turns this test binary into the server a test needs.
const fakeServerEnv = "SAG_LOCALDB_FAKE_SERVER"

// TestMain lets this binary stand in for mariadbd.
//
// One test needs a program at bin/mariadbd that fails the way a server fails: a
// complaint on stderr and a non-zero exit. A two-line shell script does that and
// is not a program on Windows, where an executable has to be a real one, so the
// test binary is copied into place and told what to be by an environment
// variable. It is the standard library's own answer to the same problem
// (os/exec's tests do this), and it is one implementation rather than two.
func TestMain(m *testing.M) {
	if os.Getenv(fakeServerEnv) == "1" {
		fmt.Fprintln(os.Stderr, "[ERROR] nope")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// installFakeServer puts that program where the bundle keeps its server, under
// whatever name this platform runs.
func installFakeServer(t *testing.T, bundleDir string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(self) //nolint:gosec // this very process
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundleDir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	name := "mariadbd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "bin", name), body, 0o700); err != nil {
		t.Fatal(err)
	}
	// Set on this process, so the child inherits it. Undone when the test ends.
	t.Setenv(fakeServerEnv, "1")
}

func TestBootstrapScriptsComeBackInFeedingOrder(t *testing.T) {
	share := t.TempDir()
	dir := filepath.Join(share, "bootstrap")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Written in the wrong order on purpose, and named the way the bundler
	// names them. Under their ORIGINAL names the performance tables sort first
	// and the bootstrap aborts, which is the mistake the numbering prevents.
	for _, name := range []string{
		"03-mariadb_performance_tables.sql",
		"01-mariadb_system_tables.sql",
		"02-mariadb_system_tables_data.sql",
		"notes.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("--\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := bootstrapScripts(share)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"01-mariadb_system_tables.sql",
		"02-mariadb_system_tables_data.sql",
		"03-mariadb_performance_tables.sql",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if filepath.Base(got[i]) != want[i] {
			t.Fatalf("position %d was %s, want %s", i, filepath.Base(got[i]), want[i])
		}
	}
}

func TestBootstrapScriptsRefusesAnEmptyBundle(t *testing.T) {
	share := t.TempDir()
	if err := os.MkdirAll(filepath.Join(share, "bootstrap"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrapScripts(share); err == nil {
		t.Fatal("a bundle with no bootstrap SQL must be reported, not started")
	}
}

func TestEveryPathIsPassedExplicitly(t *testing.T) {
	// Built with filepath rather than written as literals, because the arguments
	// are built with filepath too: on Windows "/state" comes back out as
	// "\state" and a test comparing against the slash version fails for a reason
	// that has nothing to do with what it is checking.
	state := filepath.FromSlash("/state")
	d, err := New(Config{BundleDir: filepath.FromSlash("/opt/app/mariadb"), StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(d.baseArgs(), " ")

	// The failure this guards against is not cosmetic: a path left to a
	// compiled-in default sent our server at the SYSTEM database's data
	// directory and it tried for thirty seconds to lock it.
	for _, flag := range []string{
		"--no-defaults", "--basedir=", "--datadir=", "--lc-messages-dir=",
		"--character-sets-dir=", "--aria-log-dir-path=", "--tmpdir=",
		"--pid-file=", "--general-log-file=", "--slow-query-log-file=",
	} {
		if !strings.Contains(args, flag) {
			t.Errorf("%s is not passed, so it falls back to a build-time default: %s", flag, args)
		}
	}
	// Everything it writes belongs under the state directory, never the bundle.
	for _, arg := range d.baseArgs() {
		for _, flag := range []string{"--datadir=", "--tmpdir=", "--aria-log-dir-path=", "--pid-file="} {
			if strings.HasPrefix(arg, flag) {
				if path := strings.TrimPrefix(arg, flag); !strings.HasPrefix(path, state) {
					t.Errorf("%s writes to %s, outside the state directory", flag, path)
				}
			}
		}
	}

	// And on Windows the server must be told to leave stderr alone. Without it,
	// it opens an error log named after the machine inside the data directory
	// and everything it has to say goes there instead of to the log the
	// supervisor is holding, which is how a completely empty log first turned up.
	if runtime.GOOS == "windows" && !strings.Contains(args, "--console") {
		t.Errorf("the server will take stderr over and log where nothing reads: %s", args)
	}
}

func TestThePasswordIsGeneratedOnceAndKept(t *testing.T) {
	state := t.TempDir()
	d, err := New(Config{BundleDir: t.TempDir(), StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.loadOrMakePassword(); err != nil {
		t.Fatal(err)
	}
	first := d.password
	if len(first) < 32 {
		t.Fatalf("password is only %d characters", len(first))
	}
	if strings.ContainsAny(first, ":@/?&") {
		t.Fatalf("password %q contains something a DSN would have to escape", first)
	}

	// A second run is the same installation and must reach the same database.
	again, err := New(Config{BundleDir: t.TempDir(), StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := again.loadOrMakePassword(); err != nil {
		t.Fatal(err)
	}
	if again.password != first {
		t.Fatal("the password changed between runs; the existing database would be unreachable")
	}

	info, err := os.Stat(filepath.Join(state, "database.key"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// There are no permission bits to assert here: Windows has no POSIX
		// mode, the one Go passes only decides the read-only attribute, and
		// every file comes back 0666. What keeps this file to one person is the
		// ACL it inherits from the directory it is in, and that directory is
		// under %APPDATA%, which is inside the user's own profile. That is the
		// same protection 0600 under $HOME gives on macOS, arrived at by the
		// platform's own mechanism rather than by ours.
		//
		// Said out loud rather than skipped quietly, because a permission
		// assertion that silently does not run reads afterwards as one that
		// passed.
		return
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("password file is %o, want 600", perm)
	}
}

func TestATooLongStateDirectoryFallsBackToLoopback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix socket to fall back from")
	}
	// A state directory deep enough that the socket path passes the kernel's
	// sun_path limit. Left as a socket this does not fail at bind: it surfaces
	// later as "connect: invalid argument" from the client, which reads like a
	// bug somewhere else entirely.
	deep := filepath.Join(t.TempDir(), strings.Repeat("a-long-directory-name/", 6))
	d, err := New(Config{BundleDir: "/b", StateDir: deep})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.socketPath()) <= maxSocketPath {
		t.Fatalf("the test's own path is only %d characters; it proves nothing", len(d.socketPath()))
	}
	d.chooseTransport()
	if !d.overTCP {
		t.Fatal("a socket path over the limit must fall back to loopback")
	}
	if !strings.Contains(strings.Join(d.baseArgs(), " "), "--bind-address=127.0.0.1") {
		t.Fatal("the fallback did not reach the server's arguments")
	}

	// And a short one still gets the better transport.
	short, err := New(Config{BundleDir: "/b", StateDir: "/s"})
	if err != nil {
		t.Fatal(err)
	}
	short.chooseTransport()
	if short.overTCP {
		t.Fatal("a short path should use a unix socket")
	}
}

func TestDSNCarriesParseTime(t *testing.T) {
	d, err := New(Config{BundleDir: "/b", StateDir: "/s"})
	if err != nil {
		t.Fatal(err)
	}
	d.password = "abc"
	dsn := d.DSN()
	// Without this the schema's datetime columns do not scan into time.Time and
	// the failure surfaces far from here.
	if !strings.Contains(dsn, "parseTime=true") {
		t.Fatalf("DSN has no parseTime: %s", dsn)
	}
	if !strings.Contains(dsn, "/"+Name+"?") {
		t.Fatalf("DSN does not name the application database: %s", dsn)
	}
}

// TestARealDatabaseComesUpAndAnswers is the whole point: a bundle, an empty
// directory, and a database the gateway could use, brought up and taken down by
// the supervisor.
func TestARealDatabaseComesUpAndAnswers(t *testing.T) {
	dir := bundle(t)
	state := t.TempDir()

	d, err := New(Config{BundleDir: dir, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := d.Prepare(ctx); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// The supervisor's OWN readiness decision, observed rather than guessed at.
	//
	// This used to poll Ping from here and cancel as soon as one succeeded, on
	// the reasoning that the supervisor's probe is the same query. It is, but the
	// two run on different clocks: the supervisor sleeps 100ms between polls, so
	// a cancel landing in that gap makes a launch that was about to succeed
	// return "context canceled" instead. That is a real race and it lost often
	// enough on Windows to fail the suite, while passing alone every time.
	child := d.Child()
	probe := child.Ready
	ready := make(chan struct{})
	var once sync.Once
	child.Ready = func(ctx context.Context) error {
		err := probe(ctx)
		if err == nil {
			once.Do(func() { close(ready) })
		}
		return err
	}

	s := supervise.New(zerolog.Nop(), supervise.DefaultIntensity)
	s.Add(child)

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Run(runCtx) }()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("the supervisor gave up before the database answered: %v", err)
	case <-time.After(90 * time.Second):
		t.Fatalf("the database never answered: %v", d.Ping(ctx))
	}

	// And the gateway could use it, which is a connection this test opens for
	// itself rather than the one the supervisor just made.
	if err := d.Ping(ctx); err != nil {
		t.Fatalf("ready, but not usable: %v", err)
	}

	stop()
	if err := <-done; err != nil {
		t.Fatalf("supervisor: %v", err)
	}
	// A clean stop, not a kill: the log says so and the next start depends on it.
	log, err := os.ReadFile(filepath.Join(state, "run", Name+"-database.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "Shutdown complete") {
		// With the server's own last words, because without them this failure
		// says only that something is wrong and the way to find out what is to
		// reproduce the whole thing by hand outside the test.
		t.Fatalf("the server did not shut down cleanly. Its log ends:\n%s", tail(string(log), 15))
	}
}

// TestShutdownRefusedIsNotMistakenForShutdownDone is the assertion the whole
// Windows stop path rests on.
//
// Asking a server to stop looks, from the driver, almost exactly like being
// refused: both come back as an error from one Exec. Read the wrong way in
// either direction and something is silently broken. Swallow a refusal and every
// shutdown falls through to the supervisor's kill after a 30 second wait, which
// is the old behaviour wearing the new one's clothes. Treat a dropped connection
// as a refusal and a shutdown that worked is reported as a failure.
func TestShutdownRefusedIsNotMistakenForShutdownDone(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		accepted bool
	}{
		{"it said yes", nil, true},
		{
			// What the server sends when the account was bootstrapped without
			// the privilege. It ANSWERED, and the answer was no.
			"it refused",
			&mysql.MySQLError{Number: 1227, Message: "Access denied; you need (at least one of) the SHUTDOWN privilege(s)"},
			false,
		},
		{"we stopped waiting", context.DeadlineExceeded, false},
		{"we gave up", context.Canceled, false},
		{
			// Executing SHUTDOWN is what ends the connection, so this is the
			// statement working, not failing.
			"the connection ended under us",
			mysql.ErrInvalidConn,
			true,
		},
		{"wrapped, and still a refusal", fmt.Errorf("asking: %w",
			&mysql.MySQLError{Number: 1227, Message: "Access denied"}), false},
		{"wrapped, and still a deadline", fmt.Errorf("asking: %w", context.DeadlineExceeded), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shutdownAccepted(c.err); got != c.accepted {
				t.Fatalf("shutdownAccepted(%v) = %v, want %v", c.err, got, c.accepted)
			}
		})
	}
}

func TestAServerWithNoRecordedPortIsNotAskedIntoTheVoid(t *testing.T) {
	d, err := New(Config{BundleDir: "/b", StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// Zero is what storedPort returns when there is nothing to read. Building a
	// DSN out of it would connect to port 0, which is not an address, and the
	// failure would read as the server refusing rather than as us not knowing
	// where it is.
	if err := d.shutdownStatement(0); err == nil {
		t.Fatal("asking a server we cannot locate must be an error, not an attempt")
	}
}

// TestThePortIsRememberedSoAnOrphanCanBeReached is why the port is not simply
// picked fresh every start.
//
// A server left behind by a run that was killed is still listening on the port
// that run chose. Where there is no signal to send it, connecting to it is the
// only way to ask it to stop, so a port nobody wrote down is a server nobody can
// reach and a data directory nothing can open again.
func TestThePortIsRememberedSoAnOrphanCanBeReached(t *testing.T) {
	state := t.TempDir()
	ctx := t.Context()

	first, err := New(Config{BundleDir: "/b", StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	port, err := first.loopbackPort(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if port <= 0 {
		t.Fatalf("no port was chosen: %d", port)
	}

	// A second run of the same installation, which is what a restart is.
	again, err := New(Config{BundleDir: "/b", StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	if got := again.storedPort(); got != port {
		t.Fatalf("the next run reads %d, so it would look for the previous server in the wrong place (it is on %d)", got, port)
	}
	second, err := again.loopbackPort(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second != port {
		t.Fatalf("the port moved between runs: %d then %d", port, second)
	}
}

func TestAPortSomethingElseTookIsGivenUp(t *testing.T) {
	state := t.TempDir()
	ctx := t.Context()

	d, err := New(Config{BundleDir: "/b", StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	port, err := d.loopbackPort(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Somebody else is on it now, which does happen: the port was free when we
	// wrote it down and nothing reserves it while we are not running.
	var lc net.ListenConfig
	held, err := lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	next, err := d.loopbackPort(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next == port {
		t.Fatal("the taken port was handed back; the server would fail to bind")
	}
	if got := d.storedPort(); got != next {
		t.Fatalf("the new port was not recorded: file says %d, we are using %d", got, next)
	}
}

// TestAServerLeftBehindIsStoppedBeforeTheNextOneStarts is the failure this whole
// path exists for, run against a real server.
//
// Kill the application rather than closing it and mariadbd survives it, still
// holding the data directory. Every later start then fails on "Can't lock aria
// control file" and stays broken until somebody finds the process by hand. On
// Windows it was worse than unhandled: the liveness check reported the running
// server as gone, deleted its pid file, and walked straight past it.
func TestAServerLeftBehindIsStoppedBeforeTheNextOneStarts(t *testing.T) {
	dir := bundle(t)
	state := t.TempDir()
	ctx := t.Context()

	abandoned, err := New(Config{BundleDir: dir, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := abandoned.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	// Started outside any supervisor, and then simply let go of: this is the
	// process a hard kill of the application leaves behind.
	proc, err := abandoned.Child().Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = proc.Wait() }()

	ready := time.Now().Add(90 * time.Second)
	for time.Now().Before(ready) {
		if abandoned.Ping(ctx) == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := abandoned.Ping(ctx); err != nil {
		t.Fatalf("the server under test never came up: %v", err)
	}
	pid := readPID(t, state)
	if !supervise.Running(pid) {
		t.Fatalf("pid %d was written but is not seen as running; the liveness check is wrong", pid)
	}

	// A new run of the same installation, which is what the next launch is.
	next, err := New(Config{BundleDir: dir, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Prepare(ctx); err != nil {
		t.Fatalf("the next start could not clear the previous server: %v", err)
	}
	if supervise.Running(pid) {
		t.Fatalf("the server from the previous run (pid %d) is still holding the data directory", pid)
	}

	// And it was ASKED, not killed. That is the difference between the next
	// start opening a consistent directory and running recovery over one.
	log, err := os.ReadFile(filepath.Join(state, "run", Name+"-database.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "Shutdown complete") {
		t.Fatalf("the abandoned server was cut off rather than asked. Its log ends:\n%s", tail(string(log), 15))
	}

	// And the new run can actually have the directory, which is the whole point.
	after, err := next.Child().Start(ctx)
	if err != nil {
		t.Fatalf("the data directory is still not free: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = after.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = after.Stop()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = after.Kill()
			<-done
		}
	})
	started := time.Now().Add(90 * time.Second)
	for time.Now().Before(started) {
		if next.Ping(ctx) == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the replacement server never answered: %v", next.Ping(ctx))
}

func readPID(t *testing.T, state string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(state, "run", Name+".pid"))
	if err != nil {
		t.Fatalf("the server wrote no pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("the pid file says %q: %v", raw, err)
	}
	return pid
}

// tail keeps the end of a log, which is the part that says how something ended.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func TestPreparingTwiceKeepsTheExistingDatabase(t *testing.T) {
	dir := bundle(t)
	state := t.TempDir()
	ctx := t.Context()

	d, err := New(Config{BundleDir: dir, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(state, "database", "ibdata1")
	before, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}

	// Second run of the same installation: it must find its database, not make
	// a new one over the top of it.
	again, err := New(Config{BundleDir: dir, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("the second Prepare rewrote the existing database")
	}
	if again.password != d.password {
		t.Fatal("the second Prepare could not reach the first one's database")
	}
}

func TestAFailedBootstrapLeavesNothingBehind(t *testing.T) {
	// A bundle with the right shape and a server that cannot run: bootstrap must
	// fail and must not leave a directory that the next start reads as ready.
	fake := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fake, "share", "bootstrap"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake, "share", "bootstrap", "01-x.sql"), []byte("--\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	installFakeServer(t, fake)

	state := t.TempDir()
	d, err := New(Config{BundleDir: fake, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	err = d.Prepare(t.Context())
	if err == nil {
		t.Fatal("a bootstrap that failed must be reported")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("the server's own complaint is missing from %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(state, "database")); !os.IsNotExist(statErr) {
		t.Fatal("a half-made data directory was left behind; the next start would treat it as ready")
	}
}
