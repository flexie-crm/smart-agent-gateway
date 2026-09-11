//! Finding, starting and letting go of the one gateway both applications share.
//!
//! The rule: the first window to open starts it, every later window finds it,
//! and the last window to close stops it. Two processes on one data directory
//! would fight over it, so there is exactly one, and no application owns it more
//! than any other.

use std::fs;
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};

/// How long the gateway gets to answer. A first run creates a database and
/// applies every migration, so this is generous by necessity rather than by
/// pessimism.
const READY_TIMEOUT: Duration = Duration::from_secs(180);
const POLL: Duration = Duration::from_millis(300);

#[derive(Serialize, Deserialize)]
struct Running {
    port: u16,
    pid: u32,
}

/// Where this installation keeps everything: the database, its keys, its logs.
/// The same directory the gateway itself resolves, so both agree without being
/// told.
/// What this installation's folder is called, and what it used to be.
const DATA_DIR: &str = "SAG Personal";
const EARLIER_DATA_DIR: &str = "Flexie SAG";

/// Where this installation keeps everything: the database, its keys, its logs.
///
/// This side does the move from the earlier name, and it has to be this side:
/// the shell resolves the directory and then TELLS the gateway where it is
/// (`SAG_PERSONAL_STATE`), which means the gateway never reaches its own
/// default and never gets the chance. The gateway keeps an adoption of its own
/// for `sag personal` run from a terminal with nothing passed; both are the
/// same idempotent move and they cannot race, because this one finishes before
/// the gateway is spawned.
///
/// A folder is not cosmetic here. It holds the database, the keys everything
/// sealed was sealed with, and the models. Renaming the constant alone would
/// leave a working installation looking brand new, with its conversations, its
/// agents and its connections still on disk under a name nothing reads.
pub fn state_dir() -> Result<PathBuf, String> {
    let base = dirs_config().ok_or("this system has no application data directory")?;
    let dir = base.join(DATA_DIR);
    if dir.exists() {
        return Ok(dir);
    }
    let earlier = base.join(EARLIER_DATA_DIR);
    if earlier.exists() {
        // Only ever when there is nothing to overwrite. Two data directories
        // must never be merged: the keys in one cannot open what was sealed
        // with the other.
        fs::rename(&earlier, &dir).map_err(|e| {
            format!(
                "cannot move this installation's data to {}: {e}",
                dir.display()
            )
        })?;
    }
    Ok(dir)
}

#[cfg(target_os = "macos")]
fn dirs_config() -> Option<PathBuf> {
    std::env::var_os("HOME").map(|h| PathBuf::from(h).join("Library/Application Support"))
}

#[cfg(target_os = "windows")]
fn dirs_config() -> Option<PathBuf> {
    std::env::var_os("APPDATA").map(PathBuf::from)
}

#[cfg(all(unix, not(target_os = "macos")))]
fn dirs_config() -> Option<PathBuf> {
    std::env::var_os("XDG_CONFIG_HOME")
        .map(PathBuf::from)
        .or_else(|| std::env::var_os("HOME").map(|h| PathBuf::from(h).join(".config")))
}

fn record_path(state: &Path) -> PathBuf {
    state.join("gateway.json")
}

/// Stops the gateway this window started.
///
/// One application, so there is no counting to do. The pair of claim/release
/// functions that used to live here existed only to work out which of TWO
/// applications was the last one out, and that question no longer exists.
pub fn stop_running() {
    let Ok(state) = state_dir() else { return };
    if let Some(running) = read_record(&state) {
        stop(&state, running.pid);
        let _ = fs::remove_file(record_path(&state));
    }
}

/// How far along a start is, and what it is doing.
///
/// Read from the gateway's own log rather than reported by it, because the
/// slowest part by far is applying migrations and goose already says which one
/// it is on. The total comes from a file the build writes beside the payload, so
/// the fraction is exact rather than a guess that creeps towards a number.
pub fn progress(state: &Path, resources: &Path) -> (u8, String) {
    let log = state.join("run/gateway.log");
    let Ok(text) = fs::read_to_string(&log) else {
        return (5, "Starting the database".into());
    };
    if text.contains("http server listening") {
        return (100, "Ready".into());
    }

    let done = text.matches("OK   ").count();
    if done == 0 {
        if text.contains("goose:") || text.contains("migrat") {
            return (12, "Preparing the database".into());
        }
        return (8, "Starting the database".into());
    }

    let total = fs::read_to_string(resources.join("migrations.count"))
        .ok()
        .and_then(|raw| raw.trim().parse::<usize>().ok())
        .filter(|t| *t > 0)
        .unwrap_or(done.max(1));
    // Migrations occupy the middle of the bar: there is work before them
    // (creating the database) and after them (seeding, tools, listening).
    let share = (done.min(total) as f32) / (total as f32);
    let pct = 15.0 + share * 70.0;
    (
        pct as u8,
        format!("Preparing the database ({done} of {total})"),
    )
}

/// Returns the port of a gateway that is answering, starting one if none is.
///
/// `report` is called while waiting so a window can show what is happening. A
/// first run is half a minute of nothing visible otherwise, which reads as a
/// hang rather than as work.
pub fn ensure_running(resources: &Path, report: &dyn Fn(u8, &str)) -> Result<u16, String> {
    let state = state_dir()?;
    fs::create_dir_all(&state).map_err(|e| format!("cannot create {}: {e}", state.display()))?;

    // Somebody else's, already up. Answering is the whole test: a record left
    // behind by a process that died says a port, and nothing is on it.
    //
    // Reported even though there is nothing to wait for, because the waiting
    // screen's bar is what this number drives, and a gateway that was already
    // running is the ONE path that reports nothing at all. Left out, the bar sat
    // at nothing for the whole of a warm start, which is every start after the
    // first.
    if let Some(running) = read_record(&state) {
        if healthy(running.port) {
            report(100, "Ready");
            return Ok(running.port);
        }
    }

    // Two applications opened at once would otherwise both start one. The lock
    // is a directory because creating one is atomic on every system we run on.
    let lock = state.join("starting.lock");
    let mut held = fs::create_dir(&lock).is_ok();
    if !held && stale(&lock) {
        // Left behind by a process that is gone. Quitting the application while
        // it was starting does exactly this: the guard below lives on a thread,
        // and process exit does not run its destructor. Without this the next
        // launch waits the full timeout and then reports that another window is
        // starting the gateway, for an application that is not running at all.
        let _ = fs::remove_dir_all(&lock);
        held = fs::create_dir(&lock).is_ok();
    }
    if !held {
        // Somebody is mid-start. Wait for THEIR gateway rather than starting a
        // second one on the same data directory.
        let deadline = Instant::now() + READY_TIMEOUT;
        while Instant::now() < deadline {
            if let Some(running) = read_record(&state) {
                if healthy(running.port) {
                    report(100, "Ready");
                    return Ok(running.port);
                }
            }
            let (pct, message) = progress(&state, resources);
            report(pct, &message);
            std::thread::sleep(POLL);
        }
        return Err("another window is still starting the gateway".into());
    }
    // Whose lock this is, so a later launch can tell "somebody is starting it"
    // from "somebody was starting it and died".
    let _ = fs::write(lock.join("pid"), std::process::id().to_string());
    let _guard = LockGuard(lock);

    // Checked again inside the lock: the process we waited behind may have
    // finished between our first look and our taking it.
    if let Some(running) = read_record(&state) {
        if healthy(running.port) {
            report(100, "Ready");
            return Ok(running.port);
        }
    }

    let port = free_port()?;
    let pid = spawn(resources, &state, port)?;
    write_record(&state, &Running { port, pid })?;

    let deadline = Instant::now() + READY_TIMEOUT;
    while Instant::now() < deadline {
        if healthy(port) {
            report(100, "Ready");
            return Ok(port);
        }
        let (pct, message) = progress(&state, resources);
        report(pct, &message);
        std::thread::sleep(POLL);
    }
    stop(&state, pid);
    let _ = fs::remove_file(record_path(&state));
    Err("the gateway did not start. Its log is in the application data folder.".into())
}

struct LockGuard(PathBuf);
impl Drop for LockGuard {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

/// Whether a lock was left behind rather than being held.
///
/// Two tests, and the age one matters most: a lock whose owner is gone is stale,
/// and so is one older than any start could plausibly take. The second covers the
/// case the first cannot, which is a process id that has since been reused.
fn stale(lock: &Path) -> bool {
    if let Ok(raw) = fs::read_to_string(lock.join("pid")) {
        if let Ok(pid) = raw.trim().parse::<u32>() {
            if !running(pid) {
                return true;
            }
        }
    }
    // No pid inside means it was written by a version that did not record one,
    // or the writer died between creating the directory and filling it in.
    match fs::metadata(lock).and_then(|m| m.modified()) {
        Ok(at) => at.elapsed().map(|age| age > READY_TIMEOUT).unwrap_or(true),
        Err(_) => true,
    }
}

#[cfg(unix)]
fn running(pid: u32) -> bool {
    // Signal 0 asks the question without sending anything.
    Command::new("kill")
        .args(["-0", &pid.to_string()])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .map(|s| s.success())
        .unwrap_or(false)
}

/// Whether a process is still there.
///
/// Asked with a console program, which is the reason for the flag. This runs in
/// a POLL LOOP while the gateway drains, so a window per call is not one window
/// somebody might miss: closing the application threw up dozens of black
/// terminals that opened and closed on their own, which reads as something
/// having gone badly wrong at the exact moment a person is leaving.
///
/// Every process this shell STARTS was given this flag already. These two were
/// missed because they are not started, they are asked, and nothing about
/// shutdown had been watched from an icon rather than a terminal (KB/29: the
/// platform supplies a console when nothing else does).
/// Windows gives a console program with no console to inherit A NEW ONE, and a
/// windowed application's children have none to inherit. So every process this
/// shell starts or asks about carries this, or a black terminal appears beside
/// the application (KB/29).
#[cfg(windows)]
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

#[cfg(windows)]
fn running(pid: u32) -> bool {
    use std::os::windows::process::CommandExt;
    Command::new("tasklist")
        .args(["/FI", &format!("PID eq {pid}"), "/NH"])
        .creation_flags(CREATE_NO_WINDOW)
        .output()
        .map(|o| String::from_utf8_lossy(&o.stdout).contains(&pid.to_string()))
        .unwrap_or(false)
}

fn spawn(resources: &Path, state: &Path, port: u16) -> Result<u32, String> {
    let sag = resources.join(exe("sag"));
    make_executable(&sag);
    make_executable(&resources.join("mariadb/bin/mariadbd"));
    make_executable(&resources.join(exe("sag-inference")));
    make_executable(&resources.join(exe("sag-inference-cpu")));

    // The gateway's own output goes to a file, and it MUST: launched from an
    // icon rather than a terminal, an inherited stdout is not a pipe anything is
    // reading, and the gateway died partway through its first start writing to
    // it. That failure had no log by definition, which is what made it a puzzle
    // rather than a message.
    let run = state.join("run");
    fs::create_dir_all(&run).map_err(|e| format!("cannot create {}: {e}", run.display()))?;
    let log = fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(run.join("gateway.log"))
        .map_err(|e| format!("cannot open the gateway log: {e}"))?;
    let errors = log
        .try_clone()
        .map_err(|e| format!("cannot open the gateway log: {e}"))?;

    let mut command = Command::new(&sag);
    // Its own process group, so the gateway and everything it starts (the
    // database, the inference node) can be stopped together. Without this,
    // killing the application leaves the DATABASE running with init as its
    // parent, still holding the data directory, and every later start fails on
    // "Can't lock aria control file" with no way out but a terminal.
    detached(&mut command);
    let child = command
        .arg("personal")
        .env("SAG_HTTP_ADDR", format!("127.0.0.1:{port}"))
        .env("SAG_PERSONAL_BUNDLE", resources.join("mariadb"))
        .env("SAG_PERSONAL_STATE", state)
        .env("SAG_CONSOLE_DIR", resources.join("console"))
        .env("SAG_CHAT_DIR", resources.join("chat"))
        .env("SAG_LOG_FORMAT", "json")
        // Told, not left to the server default, because the side that decides
        // when to stop waiting is this one. See DRAIN.
        .env("SAG_SHUTDOWN_GRACE", drain())
        .stdin(Stdio::null())
        .stdout(Stdio::from(log))
        .stderr(Stdio::from(errors))
        .spawn()
        .map_err(|e| format!("the gateway could not be started: {e}"))?;
    Ok(child.id())
}

/// A bundler does not always keep the executable bit on a resource, and a
/// gateway that cannot be run is a window that never opens. Setting it costs
/// nothing and removes a whole class of packaging accident.
#[cfg(unix)]
fn make_executable(path: &Path) {
    use std::os::unix::fs::PermissionsExt;
    if let Ok(meta) = fs::metadata(path) {
        let mut perms = meta.permissions();
        perms.set_mode(perms.mode() | 0o755);
        let _ = fs::set_permissions(path, perms);
    }
}

#[cfg(not(unix))]
fn make_executable(_path: &Path) {}

/// The drain the gateway is told to use, as a duration it parses.
///
/// macOS keeps the server's own default: there the shell sends SIGTERM and then
/// exits without waiting, so nothing force-kills a gateway that is still working
/// and a longer drain costs nobody anything.
#[cfg(windows)]
fn drain() -> String {
    format!("{}s", DRAIN.as_secs())
}

#[cfg(not(windows))]
fn drain() -> String {
    // Unset would be better than a value, but Command has no "leave this alone"
    // for a key it is being asked about. The server's own default, said out
    // loud, is the same thing without a special case in the caller.
    "60s".to_string()
}

fn exe(name: &str) -> String {
    if cfg!(windows) {
        format!("{name}.exe")
    } else {
        name.to_string()
    }
}

/// Puts the child in a process group of its own, named after itself.
#[cfg(unix)]
fn detached(command: &mut Command) {
    use std::os::unix::process::CommandExt;
    command.process_group(0);
}

/// Starts the gateway without giving it a console.
///
/// This application is windowed and therefore has no console of its own. The
/// gateway is a console program, and Windows gives a console program with no
/// console to inherit A NEW ONE, which appears as a black terminal window next
/// to the application, titled with the path to sag.exe. It stays for as long as
/// the gateway runs, and closing it kills the gateway.
///
/// It is not only ugly. A console window is in quick-edit mode by default, so
/// clicking anywhere inside it SUSPENDS the process that owns it: an idle click
/// on that window freezes the whole application until somebody presses a key,
/// with no indication anywhere that that is what happened.
///
/// Nothing is lost by taking it away. The gateway's output already goes to a
/// file (see spawn), because an inherited stdout is not a pipe anything is
/// reading when an application is launched from an icon.
#[cfg(windows)]
fn detached(command: &mut Command) {
    use std::os::windows::process::CommandExt;
    // CREATE_NO_WINDOW. Not HideWindow, which still allocates the console and
    // merely asks for it not to be shown, and which flashes on the way past.
    command.creation_flags(CREATE_NO_WINDOW);
}

#[cfg(unix)]
fn stop(_state: &Path, pid: u32) {
    // Asked, not killed: the gateway drains its work and shuts its database
    // down cleanly, which is what keeps the next start from being a recovery.
    //
    // Sent to the GROUP (the negative pid), so a gateway that is already gone
    // does not leave its database behind. Both get it, which is harmless: the
    // gateway is asking its database to stop at the same moment.
    let _ = Command::new("kill")
        .arg("-TERM")
        .arg(format!("-{pid}"))
        .status();
}

/// How long the gateway is given to drain what is in flight when the window
/// closes, and how long this side waits before insisting.
///
/// The two are ONE decision and are written down together, because the first
/// version got it wrong in a way that only shows up under load: this side waited
/// 15 seconds while the gateway's own default drain is a MINUTE (config.go,
/// defaultShutdownGrace), so a quit during real work force-killed a gateway that
/// was draining exactly as designed, cutting the database off mid-write and
/// making the next start a recovery. The kill is meant for a gateway that is
/// wedged, not for one that is doing its job.
///
/// A desktop's drain is deliberately shorter than a server's. Closing a window
/// means "I am done", and a minute of an application refusing to disappear is
/// not something a person reads as care. Whatever is still running when the
/// drain ends is cancelled and RECORDED rather than lost (KB/27, settle), so
/// the cost of the shorter window is bounded and recoverable.
///
/// The wait is longer than the drain, so on every path the gateway finishes
/// first and the kill below stays what it is supposed to be: a last resort.
#[cfg(windows)]
const DRAIN: Duration = Duration::from_secs(15);
#[cfg(windows)]
const STOP_GRACE: Duration = Duration::from_secs(30);

#[cfg(windows)]
fn stop(state: &Path, pid: u32) {
    // Asked through the state directory, because there is no signal on this
    // platform that reaches a gateway.
    //
    // `taskkill` without /F was what used to be here and it did not work at
    // all: it asks a window to close, the gateway has none, and it failed on
    // BOTH processes. Closing the application left the gateway and its database
    // running, which is why the shutdown had to move to something the gateway
    // itself is watching for (cmd/sag/personal_stop.go).
    //
    // With /F it would be a hard kill, which is the wrong first move and the
    // right last one: it skips the drain and cuts the database off mid-write.
    // So it is what happens after the asking, not instead of it.
    if fs::write(state.join("stop"), b"").is_err() {
        // Nothing to ask with, so go straight to insisting.
        force(pid);
        return;
    }

    let deadline = Instant::now() + STOP_GRACE;
    while Instant::now() < deadline {
        if !running(pid) {
            return;
        }
        std::thread::sleep(POLL);
    }
    force(pid);
}

/// Ends the gateway and everything under it, which on this platform means the
/// database too. It is the last resort: the next start finds a data directory
/// that was not closed and a server still holding it, and clears both
/// (internal/localdb, reapOrphan).
#[cfg(windows)]
fn force(pid: u32) {
    use std::os::windows::process::CommandExt;
    let _ = Command::new("taskkill")
        .args(["/F", "/T", "/PID", &pid.to_string()])
        .creation_flags(CREATE_NO_WINDOW)
        .status();
}

fn read_record(state: &Path) -> Option<Running> {
    let raw = fs::read(record_path(state)).ok()?;
    serde_json::from_slice(&raw).ok()
}

fn write_record(state: &Path, running: &Running) -> Result<(), String> {
    let raw = serde_json::to_vec(running).map_err(|e| e.to_string())?;
    fs::write(record_path(state), raw).map_err(|e| format!("cannot record the gateway: {e}"))
}

fn free_port() -> Result<u16, String> {
    let listener = TcpListener::bind("127.0.0.1:0")
        .map_err(|e| format!("no free port for the gateway: {e}"))?;
    listener
        .local_addr()
        .map(|a| a.port())
        .map_err(|e| e.to_string())
}

/// Asks the gateway whether it is well, rather than whether something is
/// listening. A port that accepts a connection and then fails every request is
/// the state a first run passes through.
fn healthy(port: u16) -> bool {
    let Ok(mut stream) =
        TcpStream::connect_timeout(&([127, 0, 0, 1], port).into(), Duration::from_millis(500))
    else {
        return false;
    };
    let _ = stream.set_read_timeout(Some(Duration::from_secs(2)));
    if stream
        .write_all(b"GET /healthz HTTP/1.0\r\nHost: 127.0.0.1\r\n\r\n")
        .is_err()
    {
        return false;
    }
    let mut response = String::new();
    if stream.read_to_string(&mut response).is_err() {
        return false;
    }
    response.starts_with("HTTP/1.") && response.contains(" 200") && response.contains("\"status\"")
}
