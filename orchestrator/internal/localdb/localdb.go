// Package localdb runs the database the desktop application owns (KB/36).
//
// The server deployment is given a DSN and connects to a database somebody else
// runs. A desktop has nobody else, so it carries one: a stripped MariaDB in the
// application bundle, its data under the user's own application-support
// directory, started and stopped as a supervised child (internal/supervise).
//
// # It is invisible to the machine
//
// A bundled database that behaves like an installed one is a bug: somebody
// developing against their own MariaDB must never find ours in the way. So it
// has its own datadir, its own socket, its own generated password, it is never
// registered as a service and it is never on 3306.
//
// The sharpest edge is that a packaged build carries compiled-in defaults
// pointing wherever the packager's server lived. Getting this wrong once, in
// testing, had our server spend thirty seconds trying to take an exclusive lock
// on the SYSTEM MariaDB's live data directory, and it failed only because that
// server was running and holding it. Every path is therefore passed explicitly
// on every invocation, assembled in exactly one place (baseArgs), and there is
// no other way to start the server.
package localdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"flexie.io/sag/internal/supervise"
)

// Name is what the database, its user and its socket are called. Product
// vocabulary, and nothing here is named after the engine.
const Name = "sag"

// Config says where the pieces are. Everything else is decided here.
type Config struct {
	// BundleDir holds the server: bin/mariadbd and share/{english,charsets,
	// bootstrap}, as produced by desktop/mariadb/bundle-*.sh. Read-only, and on
	// macOS genuinely so, because it sits inside a signed application bundle.
	BundleDir string

	// StateDir is where this installation's data lives. Never inside the
	// bundle: writing there breaks the signature.
	StateDir string

	// Port is used only where there is no unix socket. Zero picks a free one.
	Port int
}

// DB is a bundled database: somewhere to put it, and a child to supervise.
type DB struct {
	cfg      Config
	password string
	port     int
	overTCP  bool
}

// maxSocketPath is the length a unix socket path must stay under.
//
// The kernel's sun_path is 104 bytes on macOS and 108 on Linux, NUL included,
// and going over does not fail at bind with something legible: it surfaces later
// as "connect: invalid argument" from a client, which reads like a bug in the
// client. Found exactly that way, from a test whose temporary directory was 110
// characters, and it is not only a test's problem: a long account name or a
// relocated home directory reaches the same limit on a real machine.
//
// Conservative, because the margin costs nothing and the failure is obscure.
const maxSocketPath = 100

func New(cfg Config) (*DB, error) {
	if cfg.BundleDir == "" || cfg.StateDir == "" {
		return nil, errors.New("localdb: BundleDir and StateDir are required")
	}
	return &DB{cfg: cfg, port: cfg.Port}, nil
}

func (d *DB) dataDir() string    { return filepath.Join(d.cfg.StateDir, "database") }
func (d *DB) runDir() string     { return filepath.Join(d.cfg.StateDir, "run") }
func (d *DB) socketPath() string { return filepath.Join(d.runDir(), Name+".sock") }
func (d *DB) secretPath() string { return filepath.Join(d.cfg.StateDir, "database.key") }
func (d *DB) serverBin() string {
	name := "mariadbd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(d.cfg.BundleDir, "bin", name)
}
func (d *DB) shareDir() string { return filepath.Join(d.cfg.BundleDir, "share") }

// chooseTransport prefers a unix socket and falls back to loopback TCP.
//
// A socket is better in every way that matters here: no port to pick, no port to
// collide with, and nothing can reach it that cannot already read our directory.
// Two things take it away. Windows has none for this server, and a state
// directory deep enough to push the socket path past the kernel's limit makes
// one unusable, so the fallback is real rather than theoretical.
func (d *DB) chooseTransport() {
	d.overTCP = runtime.GOOS == "windows" || len(d.socketPath()) > maxSocketPath
}

// Prepare makes the database exist. It is safe to call on every start and does
// its work only once, when the data directory is empty.
func (d *DB) Prepare(ctx context.Context) error {
	// 0700 throughout: on a socket deployment the directory permissions are the
	// access control.
	for _, dir := range []string{d.cfg.StateDir, d.dataDir(), d.runDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("localdb: create %s: %w", dir, err)
		}
	}
	if err := d.loadOrMakePassword(); err != nil {
		return err
	}
	d.chooseTransport()

	// A server of ours left running by a process that died still holds this
	// directory, and nothing new can open it. Killing the app mid-start does
	// exactly that: mariadbd is reparented to init and keeps its locks, and
	// every later start fails on "Can't lock aria control file".
	//
	// BEFORE the port is chosen, and that order is load-bearing on a loopback
	// deployment. An orphan is still listening on the port this installation
	// recorded, so picking a port first would find that one taken, choose a
	// different one, and leave the only way of reaching the orphan behind.
	if err := d.reapOrphan(); err != nil {
		return err
	}
	if d.overTCP && d.port == 0 {
		port, err := d.loopbackPort(ctx)
		if err != nil {
			return err
		}
		d.port = port
	}

	// "Already initialised" is a marker written LAST, not an empty directory.
	//
	// Judging by emptiness was wrong for the case that actually happens: quit
	// the application while it is first starting, or lose power, and the
	// directory is full of half-written InnoDB files with no mysql database in
	// it. Every later start then skips bootstrap and fails against a directory
	// that will never work, which looks permanent and is not obviously undoable.
	if d.initialised() {
		return nil
	}
	// Whatever is there was interrupted before it finished. Nothing in it can be
	// wanted, because the marker is written only when a bootstrap completes.
	if err := os.RemoveAll(d.dataDir()); err != nil {
		return fmt.Errorf("localdb: clear an unfinished database: %w", err)
	}
	if err := os.MkdirAll(d.dataDir(), 0o700); err != nil {
		return fmt.Errorf("localdb: create %s: %w", d.dataDir(), err)
	}
	return d.bootstrap(ctx)
}

// How long a server left behind gets to go when asked, and then how long it
// gets once we have stopped asking. The second is short because a process that
// has been ended does not linger; it is a bound, not a wait.
const (
	orphanGrace = 20 * time.Second
	endGrace    = 5 * time.Second
)

// reapOrphan stops a server left behind by a previous run.
//
// The pid file is ours and names only our own server, so this can never reach
// somebody else's database: it is the one written by the arguments in baseArgs,
// under our own state directory.
//
// It asks, then it insists. Asking is what keeps the next start from being a
// recovery; insisting is what keeps a server that will not go from being a data
// directory nothing can ever open again, which used to be the outcome and had no
// way out but Task Manager.
func (d *DB) reapOrphan() error {
	path := filepath.Join(d.runDir(), Name+".pid")
	raw, err := os.ReadFile(path) //nolint:gosec // a path this process built
	if err != nil {
		return nil // no previous run to clean up after
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		_ = os.Remove(path)
		return nil
	}
	if !supervise.Running(pid) {
		_ = os.Remove(path) // gone already; the file is a leftover
		return nil
	}

	_ = d.askToStop(pid, d.storedPort())
	if gone(pid, orphanGrace) {
		_ = os.Remove(path)
		return nil
	}
	if err := supervise.EndByID(pid); err != nil {
		return fmt.Errorf("localdb: a database from a previous run (pid %d) will not stop: %w", pid, err)
	}
	if !gone(pid, endGrace) {
		return fmt.Errorf("localdb: a database from a previous run (pid %d) will not stop", pid)
	}
	_ = os.Remove(path)
	return nil
}

// gone waits for a process to disappear, and says whether it did.
func gone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if !supervise.Running(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// askToStop asks a server of ours to shut down.
//
// This is the difference between a next start that opens a consistent directory
// and one that runs InnoDB recovery over a directory cut off mid-write. Two
// platforms, two mechanisms, one meaning:
//
// A signal is how it is done where there is one. mariadbd reads SIGTERM as
// "shut down", finishes what it is doing, flushes, and logs "Shutdown complete".
//
// Windows has no signal it reads, and the usual answer is to bundle
// mariadb-admin and run `shutdown` through it, which ships a 4.6MB client
// program to send a request we are already connected to send. SHUTDOWN is a
// STATEMENT, so it goes down the same connection the readiness probe just
// proved: if the server can answer SELECT 1 it can be asked to stop, and if it
// cannot answer, there is nothing to ask politely anyway.
func (d *DB) askToStop(pid, port int) error {
	if runtime.GOOS == "windows" {
		return d.shutdownStatement(port)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("localdb: find the server to stop it: %w", err)
	}
	return supervise.Terminate(proc)
}

// shutdownAsk bounds the request. It is not how long shutting down may take,
// which the supervisor's own grace period covers: it is how long we wait to be
// heard.
const shutdownAsk = 10 * time.Second

func (d *DB) shutdownStatement(port int) error {
	if port <= 0 {
		return errors.New("localdb: no recorded port to reach the server on")
	}
	db, err := sql.Open("mysql", d.dsn(port))
	if err != nil {
		return fmt.Errorf("localdb: reach the server to stop it: %w", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), shutdownAsk)
	defer cancel()
	if _, err := db.ExecContext(ctx, "SHUTDOWN"); !shutdownAccepted(err) {
		return fmt.Errorf("localdb: ask the server to stop: %w", err)
	}
	return nil
}

// shutdownAccepted reads what came back from asking.
//
// The awkward part is that succeeding looks like failing: a server executing
// SHUTDOWN stops answering as part of executing it, so the driver can see the
// connection end before it sees a reply. That is the statement working.
//
// What must NOT be read that way is the server ANSWERING and refusing, which is
// error 1227 and means the account was made without the privilege. Swallowing
// that would leave every shutdown silently falling through to the supervisor's
// kill, which is the current behaviour dressed up as the fixed one.
func shutdownAccepted(err error) bool {
	if err == nil {
		return true
	}
	var fromServer *mysql.MySQLError
	if errors.As(err, &fromServer) {
		return false // it answered, and what it said was no
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false // we stopped waiting, which is not the same as it going
	}
	return true // the connection ended under us, which is what SHUTDOWN does to it
}

// readyFile is written once a bootstrap has finished, and is the only thing that
// says a data directory is usable.
const readyFile = ".initialised"

func (d *DB) initialised() bool {
	_, err := os.Stat(filepath.Join(d.dataDir(), readyFile))
	return err == nil
}

// bootstrap builds the system tables, the application's database and its user,
// in ONE run of the server against an empty directory.
//
// No install script and no perl, which is what makes the same procedure work on
// Windows: the server reads SQL on stdin and the files it needs ship beside it.
func (d *DB) bootstrap(ctx context.Context) error {
	scripts, err := bootstrapScripts(d.shareDir())
	if err != nil {
		return err
	}

	readers := []io.Reader{
		// Bootstrap reads ONE STATEMENT PER LINE. Putting these on one line is a
		// syntax error, which is why they are written out separately.
		strings.NewReader("CREATE DATABASE IF NOT EXISTS mysql;\nUSE mysql;\n"),
	}
	for _, path := range scripts {
		f, err := os.Open(path) //nolint:gosec // paths come from our own bundle
		if err != nil {
			return fmt.Errorf("localdb: open %s: %w", path, err)
		}
		defer func() { _ = f.Close() }()
		readers = append(readers, f, strings.NewReader("\n"))
	}

	// FLUSH PRIVILEGES is what makes this one pass instead of two. Bootstrap
	// runs with --skip-grant-tables, so CREATE USER fails with error 1290; the
	// flush lifts that mid-run, and the account can be made here rather than by
	// starting a throwaway server afterwards just to grant on it.
	// The account is made for BOTH hosts, and deliberately so. Which transport
	// this installation uses is decided from the length of a path, and with
	// --skip-name-resolve a TCP client arrives as 127.0.0.1 while a socket client
	// arrives as localhost: granting one and connecting over the other is an
	// access-denied that would look like a wrong password. Neither is reachable
	// from off the machine.
	grants := "FLUSH PRIVILEGES;\n" +
		fmt.Sprintf("CREATE DATABASE %s CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;\n", Name)
	// SHUTDOWN is granted globally, and it is the only global privilege here.
	// It is not a widening: it carries no access to any data, and this account
	// already owns every table it can see. It is what lets the server be ASKED
	// to stop on a platform with no signal for it (askToStop), which is the
	// difference between a clean directory and a recovery on every start. The
	// alternative is bundling a client program to send one statement.
	for _, host := range []string{"localhost", "127.0.0.1"} {
		grants += fmt.Sprintf(
			"CREATE USER '%[1]s'@'%[2]s' IDENTIFIED BY '%[3]s';\n"+
				"GRANT ALL PRIVILEGES ON %[1]s.* TO '%[1]s'@'%[2]s';\n"+
				"GRANT SHUTDOWN ON *.* TO '%[1]s'@'%[2]s';\n",
			Name, host, d.password)
	}
	readers = append(readers, strings.NewReader(grants))

	cmd := exec.CommandContext(ctx, d.serverBin(), append(d.baseArgs(), "--bootstrap")...) //nolint:gosec
	supervise.HideConsole(cmd)
	cmd.Stdin = io.MultiReader(readers...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Leave nothing half-made: a partly bootstrapped directory would be
		// treated as ready on the next start.
		_ = os.RemoveAll(d.dataDir())
		return fmt.Errorf("localdb: bootstrap: %w: %s", err, lastErrorLine(out))
	}
	// Last, and only on success. Everything before this point is a directory
	// that must be thrown away rather than started.
	if err := os.WriteFile(filepath.Join(d.dataDir(), readyFile), []byte("1\n"), 0o600); err != nil {
		return fmt.Errorf("localdb: record that the database is ready: %w", err)
	}
	return nil
}

// bootstrapScripts returns the shipped SQL in the order it must be fed.
//
// Sorted, which is safe only because the bundler numbers them: under their own
// names mariadb_performance_tables sorts FIRST and the bootstrap aborts.
func bootstrapScripts(shareDir string) ([]string, error) {
	dir := filepath.Join(shareDir, "bootstrap")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("localdb: read %s: %w", dir, err)
	}
	var scripts []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			scripts = append(scripts, filepath.Join(dir, e.Name()))
		}
	}
	if len(scripts) == 0 {
		return nil, fmt.Errorf("localdb: no bootstrap SQL in %s", dir)
	}
	sort.Strings(scripts)
	return scripts, nil
}

// baseArgs is the one place a path is decided. Nothing is left to a default,
// because the defaults compiled into a packaged build point at the packager's
// server and not at ours.
func (d *DB) baseArgs() []string {
	args := []string{
		"--no-defaults", // no my.cnf anywhere on this machine gets a say
		"--basedir=" + d.cfg.BundleDir,
		"--datadir=" + d.dataDir(),
		"--lc-messages-dir=" + d.shareDir(),
		"--character-sets-dir=" + filepath.Join(d.shareDir(), "charsets"),
		"--aria-log-dir-path=" + d.dataDir(),
		"--tmpdir=" + d.runDir(),
		"--pid-file=" + filepath.Join(d.runDir(), Name+".pid"),
		"--general-log-file=" + filepath.Join(d.runDir(), Name+"-general.log"),
		"--slow-query-log-file=" + filepath.Join(d.runDir(), Name+"-slow.log"),
		"--skip-name-resolve",

		// --- sized for one person on a laptop -------------------------------
		//
		// Everything below overrides a default chosen for a machine that exists
		// to be a database. This one shares a computer with a browser, an editor
		// and whatever else, and its whole workload is one person's chat
		// history. Left alone it takes 140MB of disk before a single row exists
		// and a few hundred MB of memory to serve almost nothing.

		// 96MB of redo log by default, written out in full at creation. It is
		// sized for sustained write throughput nobody here will ever produce,
		// and the writing is time spent watching a splash screen.
		"--innodb-log-file-size=16M",
		// Three 10MB undo tablespaces, for long transactions this does not have.
		"--innodb-undo-tablespaces=0",
		// The cache that matters. Small enough to be polite on an 8GB machine,
		// large enough that a personal history is effectively all resident.
		"--innodb-buffer-pool-size=192M",
		// One buffer pool instance: several exist to reduce contention between
		// many concurrent writers, and here there is one process.
		"--innodb-buffer-pool-instances=1",
		"--innodb-purge-threads=1",
		"--innodb-read-io-threads=2",
		"--innodb-write-io-threads=2",
		// 151 connections is a server's answer. One process with one connection
		// pool and a worker needs a fraction of it, and every slot is memory
		// reserved against a person who does not exist.
		"--max-connections=32",
		"--thread-cache-size=8",
		// Instrumentation for somebody tuning a production database, costing
		// tens of megabytes to collect statistics nobody on a laptop reads.
		"--performance-schema=OFF",
		// MyISAM's index cache. Nothing we create uses the engine; the system
		// tables that do are tiny.
		"--key-buffer-size=8M",
		// The scheduler runs nothing here, and neither does replication.
		"--skip-log-bin",
		// Durability is left at its strictest. It is the one default worth the
		// milliseconds: relaxing it trades a person's last few seconds of
		// conversation for speed they would not notice.
	}
	if runtime.GOOS == "windows" {
		// Keep the server's own voice on stderr, where we are already reading it.
		//
		// Without this it takes stderr over and writes to an error log of its own
		// choosing, named after the machine, inside the data directory: exactly
		// the kind of compiled-in path this function exists to prevent, and the
		// reason the log we DO capture was completely empty on this platform.
		// Everything the server had to say about starting, refusing and shutting
		// down went into a file nothing reads.
		//
		// The flag is named for a console and does not need one. All it does is
		// stop the redirection, so the handles the supervisor passed stay the
		// handles the server writes to, which is what it already does on unix.
		// Naming an error log file instead would work, but it would put two
		// writers on one file for no gain.
		args = append(args, "--console")
	}
	if d.overTCP {
		args = append(args, "--bind-address=127.0.0.1", fmt.Sprintf("--port=%d", d.port))
	} else {
		args = append(args, "--socket="+d.socketPath(), "--skip-networking")
	}
	return args
}

// Child is the supervised process. Start only starts: everything that has to be
// true before it can work has already been done by Prepare.
func (d *DB) Child() supervise.Child {
	return supervise.Child{
		Name:    "database",
		Restart: supervise.Permanent,
		// Generous, because the first start after a bootstrap has tens of
		// megabytes to lay down before it answers.
		ReadyTimeout: 90 * time.Second,
		// The database goes down last and is worth waiting for: an InnoDB that
		// is killed recovers, but recovering is work it should not have to do.
		Shutdown: 30 * time.Second,
		Start: func(ctx context.Context) (supervise.Process, error) {
			// Deliberately detached from ctx: cancelling it must NOT kill the
			// server. Shutdown belongs to the supervisor, which asks with
			// SIGTERM and waits before insisting, and a database killed instead
			// of asked pays for it with recovery on the next start.
			cmd := exec.CommandContext(context.WithoutCancel(ctx), d.serverBin(), d.baseArgs()...) //nolint:gosec
			supervise.HideConsole(cmd)
			log, err := os.OpenFile(filepath.Join(d.runDir(), Name+"-database.log"),
				os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return nil, fmt.Errorf("localdb: open log: %w", err)
			}
			cmd.Stdout, cmd.Stderr = log, log
			if err := cmd.Start(); err != nil {
				_ = log.Close()
				return nil, fmt.Errorf("localdb: start server: %w", err)
			}
			return &process{db: d, cmd: cmd, log: log}, nil
		},
		Ready: d.Ping,
	}
}

// Ping is the readiness probe, and it is a real query on a real connection.
//
// Never the pid, and never the "ready for connections" line in the log: that is
// a log line and not a contract, and a server can print it and still refuse the
// account we are going to use. This asks the question the gateway will ask.
func (d *DB) Ping(ctx context.Context) error {
	db, err := sql.Open("mysql", d.DSN())
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	var one int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return err
	}
	if one != 1 {
		return fmt.Errorf("localdb: database answered %d", one)
	}
	return nil
}

// DSN is what the store connects with. parseTime is not optional: the schema is
// full of datetime columns and the rest of the codebase expects time.Time.
func (d *DB) DSN() string { return d.dsn(d.port) }

// dsn takes the port as an argument because there is one caller that needs a
// port other than this run's: the one reaching a server left behind by the
// previous run, to ask it to stop.
func (d *DB) dsn(port int) string {
	if d.overTCP {
		return fmt.Sprintf("%s:%s@tcp(127.0.0.1:%d)/%s?parseTime=true&loc=UTC",
			Name, d.password, port, Name)
	}
	return fmt.Sprintf("%s:%s@unix(%s)/%s?parseTime=true&loc=UTC",
		Name, d.password, d.socketPath(), Name)
}

// portPath is where this installation remembers the port its database answers
// on.
func (d *DB) portPath() string { return filepath.Join(d.cfg.StateDir, "database.port") }

// storedPort is the port the previous run used, or zero if there was none.
func (d *DB) storedPort() int {
	raw, err := os.ReadFile(d.portPath()) //nolint:gosec // a path this process built
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || port <= 0 {
		return 0
	}
	return port
}

// loopbackPort picks the port once and then keeps it.
//
// A fresh port every start would be fine for connecting, because the process
// that picks it is the process that connects. It is not fine for the one case
// that matters: a server left behind by a run that was killed. That process is
// holding the data directory, it has to be asked to stop, and where there is no
// signal that means connecting to it (askToStop). A port nobody wrote down is a
// server nobody can reach.
//
// So it is kept beside the rest of this installation's state, exactly as the
// inference node keeps its own (cmd/sag/personal_node.go, nodeAddress). If
// something else has taken it since, a new one is picked and recorded, which is
// the one case where the stored port is wrong for a single start.
func (d *DB) loopbackPort(ctx context.Context) (int, error) {
	if port := d.storedPort(); port > 0 && portFree(ctx, port) {
		return port, nil
	}
	port, err := freePort(ctx)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(d.portPath(), []byte(strconv.Itoa(port)), 0o600); err != nil {
		return 0, fmt.Errorf("localdb: record the database port: %w", err)
	}
	return port, nil
}

// portFree reports whether we can still have the port we had last time.
func portFree(ctx context.Context, port int) bool {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// loadOrMakePassword keeps the account's password beside the data it protects.
//
// It is generated once and never shown: nobody types it, so it has no reason to
// be memorable, and on a socket deployment the directory permissions are the
// real guard anyway.
func (d *DB) loadOrMakePassword() error {
	existing, err := os.ReadFile(d.secretPath())
	if err == nil {
		d.password = strings.TrimSpace(string(existing))
		if d.password != "" {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("localdb: read password: %w", err)
	}

	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("localdb: generate password: %w", err)
	}
	// Hex, so it can go into a DSN with nothing to quote and nothing to escape.
	d.password = hex.EncodeToString(buf)
	if err := os.WriteFile(d.secretPath(), []byte(d.password), 0o600); err != nil {
		return fmt.Errorf("localdb: store password: %w", err)
	}
	return nil
}

// process adapts a running server to the supervisor's view of one.
type process struct {
	db  *DB
	cmd *exec.Cmd
	log *os.File
}

func (p *process) Stop() error { return p.db.askToStop(p.cmd.Process.Pid, p.db.port) }
func (p *process) Kill() error { return p.cmd.Process.Kill() }
func (p *process) Wait() error {
	err := p.cmd.Wait()
	_ = p.log.Close()
	return err
}

// freePort asks the operating system for one and hands it back. There is a gap
// between letting go and the server binding, and nothing closes it; a start that
// loses that race is a start that failed, and the supervisor is what retries.
func freePort(ctx context.Context) (int, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("localdb: find a free port: %w", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// lastErrorLine pulls the line worth showing out of a wall of startup notes.
func lastErrorLine(out []byte) string {
	var found string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "[ERROR]") || strings.HasPrefix(line, "ERROR") {
			found = strings.TrimSpace(line)
		}
	}
	if found == "" {
		return strings.TrimSpace(lastLines(string(out), 3))
	}
	return found
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
