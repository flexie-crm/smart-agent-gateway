//! The browser, as a supervised child of this application.
//!
//! It is started when it is first needed and it goes when the application goes.
//! Nothing here knows what the browser is FOR: it starts one, says where its
//! protocol is listening, and makes sure that when this process ends there is
//! nothing left behind.
//!
//! Three rules, each of which was a bug somewhere else in this tree first.
//!
//! **Readiness is a probe, never a log line.** The browser prints "DevTools
//! listening on ws://..." on its way up, and reading that would work until the
//! day it does not print it. What proves a browser is ready is asking it
//! something and being answered, which is the rule the personal edition's
//! database already follows (KB/36: readiness is `SELECT 1`).
//!
//! **Ending it is addressed to its GROUP, as a backstop and not as the
//! mechanism.** A browser is several processes: one parent and a helper per
//! renderer and service. What actually ends them was measured rather than
//! assumed, because the assumption was wrong: killing ONLY the parent with
//! SIGKILL leaves an empty process group four seconds later, every time. The
//! helpers watch the channel to the browser process and go when it closes.
//!
//! So this is not the MariaDB story from `gateway.rs`, where a database really
//! was left parented to init holding its data directory, and it would have been
//! written up as if it were if nobody had run the control. The group signal
//! stays because it is one call, it costs nothing, and it is the honest
//! handling of a helper that is wedged and not reading its channel. It is a
//! belt with braces, not the thing holding the trousers up.
//!
//! **A browser that will not start is refused with a reason, not retried for
//! ever.** Restarting on a loop turns one broken install into a machine that
//! spawns a process every second until somebody opens Activity Monitor.

use std::path::{Path, PathBuf};
use std::process::{Child, Stdio};
use std::time::{Duration, Instant};

use tokio::sync::Mutex;

/// Where the protocol is listening, for whoever is about to speak it.
///
/// The websocket path carries a UUID the browser minted, and that is not
/// decoration: the debugging port serves anything that can reach it, so the
/// unguessable path is what stands between this browser and any other program
/// on the machine. It is read from the browser's own file rather than found by
/// asking `/json`, because asking is a route somebody else could take too.
#[derive(Debug, Clone)]
pub struct Endpoint {
    pub port: u16,
    pub websocket: String,
}

/// How long a browser gets to come up before we conclude it will not.
///
/// A cold start measured 326, 382 and 328 ms on a developer's machine. Ten
/// seconds is not that number with margin, it is a different question: a machine
/// under real load, a cold disk, an antivirus reading two hundred megabytes of a
/// binary it has never seen.
const START_DEADLINE: Duration = Duration::from_secs(10);

/// How long a stopping browser is given before it is insisted upon.
///
/// It is asked first, because it has a profile directory open and a clean exit
/// is what keeps the next start from being a repair. The kill is for one that is
/// wedged, never the first move.
const STOP_GRACE: Duration = Duration::from_secs(5);

/// How many failed starts in a row before this stops trying.
///
/// Three, and then the reason is repeated rather than the attempt. What makes it
/// worth having is the shape of the failure it is for: a browser that cannot
/// start does not usually start on the fourth go, and a supervisor that believes
/// otherwise spawns processes until somebody notices.
const GIVE_UP_AFTER: u32 = 3;

/// How long before a run of failures is forgotten.
///
/// The count is about a browser that is broken NOW. One that failed at nine in
/// the morning because the disk was full says nothing about one at four in the
/// afternoon, and holding it against them would mean an application somebody has
/// to restart to recover from a problem that fixed itself.
const FORGET_FAILURES_AFTER: Duration = Duration::from_secs(300);

/// One running browser: the child we spawned, and where its protocol is.
///
/// The CHILD is held, not just its number, and that is the whole of how this
/// knows whether the browser is still there. Asking the operating system
/// whether a process id exists cannot tell a running process from one that has
/// exited and not yet been collected: the id is still there either way, and a
/// supervisor that asks it waits out its whole deadline for a browser that died
/// in the first millisecond. Asking the child reaps it and answers truly.
///
/// It also removes the question that has already been got wrong once in this
/// tree, where signal 0 reported a RUNNING process as gone on Windows and every
/// later start failed on a locked data directory (KB/36, windows).
#[derive(Debug)]
struct Running {
    child: std::process::Child,
    pid: u32,
    endpoint: Endpoint,
    /// Where its profile is, so stopping can take the record away with it. A
    /// record left behind names a process that has gone, and the next start
    /// would ask the system about a pid that may since belong to somebody
    /// else.
    profile: PathBuf,
}

struct State {
    running: Option<Running>,
    failures: u32,
    last_failure: Option<Instant>,
    last_reason: String,
}

static STATE: Mutex<State> = Mutex::const_new(State {
    running: None,
    failures: 0,
    last_failure: None,
    last_reason: String::new(),
});

/// say records what the browser is doing, where somebody can see it.
///
/// One line per event on stderr, the same as the link's. This half runs with no
/// window of its own, and a browser that never starts otherwise looks exactly
/// like a browser nobody asked for.
fn say(what: &str) {
    eprintln!("browser: {what}");
}

/// still_running asks the child whether it is still there, and collects it if
/// it is not.
///
/// The child is asked rather than the operating system, and `try_wait` is the
/// whole reason: it REAPS. A process id outlives its process, as a zombie,
/// until somebody collects it, so asking the system whether the id exists
/// answers "alive" about a browser that died in its first millisecond. The
/// first version of this file did exactly that, and a test that ran a program
/// which exits immediately took the full ten second deadline to report it.
fn still_running(child: &mut Child) -> bool {
    matches!(child.try_wait(), Ok(None))
}

/// collect reaps a child we have just ended, and gives up rather than waiting
/// for ever.
///
/// The obvious line here is `child.wait()`, and it is a trap: it blocks until
/// the process exits, with no bound, so anything that fails to end the browser
/// turns this into a hang. That is not theoretical. A control that removed the
/// kill from `stop` did not fail the test it was aiming at, it hung the test
/// run until it was killed two minutes later, because `wait` sat there for a
/// browser nobody had signalled.
///
/// This path runs when the application is quitting, so a hang here is a
/// window that will not close. A second is long after a SIGKILL, and if the
/// process somehow outlives that, a zombie left behind by a process which is
/// itself about to exit costs nothing: init collects it.
async fn collect(child: &mut Child) {
    let waited = Instant::now();
    while waited.elapsed() < Duration::from_secs(1) {
        if !still_running(child) {
            return;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    say("it could not be collected; leaving it to the system");
}

/// ensure there is a browser running, and say where to talk to it.
///
/// Starting twice does not start twice. One that died since the last call is
/// replaced rather than reported, because from the caller's side "it is not there
/// any more" and "it was never there" want the same thing to happen.
pub async fn ensure(exe: &Path, profile: &Path) -> Result<Endpoint, String> {
    let mut state = STATE.lock().await;

    if let Some(running) = &mut state.running {
        if still_running(&mut running.child) && answers(running.endpoint.port).await {
            return Ok(running.endpoint.clone());
        }
        // It went while nobody was looking. Its group goes before another is
        // started, or the profile stays held by whatever is left of it.
        say("the browser is gone; clearing what is left of it");
        let pid = running.pid;
        off_the_runtime(move || end_group(pid)).await;
        state.running = None;
    }

    // NOTHING of ours is running. Before starting one, deal with a browser
    // left behind by a PREVIOUS run of the application.
    //
    // This is not housekeeping. Measured: a second browser started against a
    // profile another one is holding does NOT fail. It starts, listens on a
    // new port, OVERWRITES the port file, and logs lock errors while two
    // browsers write to one profile directory. The one that was there becomes
    // unreachable, because the only record of its port has just been replaced,
    // so nothing can ever stop it again. Every force quit would add another.
    //
    // It is ENDED rather than adopted, which is what total ownership means.
    // An adopted browser is not our child, so it cannot be waited on, and its
    // tabs are not in our registry, so the ten-tab cap would be counting the
    // wrong thing and its pages would never be evicted.
    end_any_orphan(profile).await;

    if let Some(reason) = giving_up(&state) {
        return Err(reason);
    }

    match start(exe, profile).await {
        Ok(running) => {
            say(&format!("ready on port {}", running.endpoint.port));
            // Written down for the NEXT run of the application, which knows
            // nothing about this one except what this one leaves behind.
            write_record(profile, running.pid);
            let endpoint = running.endpoint.clone();
            state.running = Some(running);
            state.failures = 0;
            state.last_failure = None;
            state.last_reason.clear();
            Ok(endpoint)
        }
        Err(why) => {
            state.failures += 1;
            state.last_failure = Some(Instant::now());
            state.last_reason = why.clone();
            say(&format!("did not start ({}): {why}", state.failures));
            Err(why)
        }
    }
}

/// End a browser a previous run of the application left behind.
///
/// Found through its own port file rather than by a process id, and asked to
/// close over the protocol rather than signalled. Both halves matter:
///
///   - a pid we wrote down could have been reused by something else by now,
///     and killing a stranger is worse than leaving a browser running;
///   - `Browser.close` is the browser shutting itself down, so it releases the
///     profile properly instead of being cut off mid-write, and it needs no
///     platform code at all.
///
/// Measured: the browser stops answering and its process goes. It does not
/// reply to the request first, because it closes the socket on its way out,
/// which is why nothing here waits for an answer.
async fn end_any_orphan(profile: &Path) {
    // Asked nicely first, if it is in a state to answer.
    if let Some(endpoint) = read_endpoint(&profile.join("DevToolsActivePort")).await {
        if answers(endpoint.port).await {
            say("a browser from an earlier run is still going; ending it");
            ask_over_protocol(&endpoint.websocket).await;
            if gone_within(endpoint.port).await {
                forget_record(profile);
                return;
            }
            say("it would not close when asked");
        }
    }

    // Not answering is NOT the same as not there, and reading it that way was
    // the hole this closes. A browser can be alive and unreachable: a hung
    // renderer, a stopped process, one still starting. It is still holding the
    // profile, so starting another against it is the situation being avoided.
    //
    // So the pid is written down when one starts, the way the gateway records
    // its own (personal/shell/src/gateway.rs), and what cannot be asked is
    // ENDED. By group, because a browser is several processes.
    let Some(pid) = read_record(profile) else {
        return;
    };
    // Unanswerable counts as alive: ending a process that has already gone
    // does nothing, and believing a live one gone starts a second browser on
    // a held profile (see `alive`).
    if !off_the_runtime(move || alive(pid)).await.unwrap_or(true) {
        // A record left by a browser that has already gone.
        forget_record(profile);
        return;
    }
    say(&format!(
        "a browser from an earlier run ({pid}) will not answer; ending it"
    ));
    off_the_runtime(move || end_group(pid)).await;
    forget_record(profile);
}

/// Forget what is running, WITHOUT stopping it.
///
/// For one caller: the gate that has to look like a crashed application, where
/// the process survives and the supervisor's memory of it does not. There is no
/// other way to reach that state from inside one process.
#[doc(hidden)]
pub async fn forget_for_test() {
    STATE.lock().await.running = None;
}

/// Whether a browser is answering, for a gate that needs to prove one is not.
#[doc(hidden)]
pub async fn answers_for_test(port: u16) -> bool {
    answers(port).await
}

/// Whether a browser stopped answering within the grace period.
async fn gone_within(port: u16) -> bool {
    let waited = Instant::now();
    while waited.elapsed() < STOP_GRACE {
        if !answers(port).await {
            say("the earlier browser is gone");
            return true;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    false
}

/// Where the running browser's process id is written down.
///
/// Beside the profile, because that is what a browser holds and what a second
/// one must not be started against. The same idea as the gateway's own record
/// (personal/shell/src/gateway.rs): a supervisor that has been restarted knows
/// nothing about its predecessor except what the predecessor wrote down.
fn record_path(profile: &Path) -> PathBuf {
    profile.join("supervised.pid")
}

fn write_record(profile: &Path, pid: u32) {
    let _ = std::fs::create_dir_all(profile);
    let _ = std::fs::write(record_path(profile), pid.to_string());
}

fn read_record(profile: &Path) -> Option<u32> {
    std::fs::read_to_string(record_path(profile))
        .ok()?
        .trim()
        .parse()
        .ok()
}

fn forget_record(profile: &Path) {
    let _ = std::fs::remove_file(record_path(profile));
}

/// Process control, on the threads kept for work that blocks.
///
/// On Windows each of these starts `tasklist` or `taskkill` and waits for it to
/// finish, measured at 280 to 380 milliseconds apiece, and that wait was spent
/// holding a thread the link answers the gateway on (KB/29). Elsewhere each is
/// one system call and moving it costs nothing that matters. `None` means the
/// work itself panicked.
async fn off_the_runtime<T: Send + 'static>(
    work: impl FnOnce() -> T + Send + 'static,
) -> Option<T> {
    tokio::task::spawn_blocking(work).await.ok()
}

/// Whether a process id belongs to something still running.
///
/// Only ever asked about a pid WE wrote down for a browser we started, and
/// only when it is not answering. It is the weaker question this file warns
/// about (a zombie's id still answers), and that is acceptable here for the
/// reason it is not acceptable for our own child: ending a process that has
/// already gone does nothing, where believing a live one is gone is what
/// starts a second browser on a held profile.
#[cfg(unix)]
fn alive(pid: u32) -> bool {
    // No signal at all: the one that asks whether it could be delivered.
    nix::sys::signal::kill(nix::unistd::Pid::from_raw(pid as i32), None).is_ok()
}

#[cfg(windows)]
fn alive(pid: u32) -> bool {
    let out = std::process::Command::new("tasklist")
        .args(["/FI", &format!("PID eq {pid}"), "/NH"])
        .output();
    match out {
        Ok(out) => String::from_utf8_lossy(&out.stdout).contains(&pid.to_string()),
        Err(_) => false,
    }
}

/// Ask a browser to shut itself down.
///
/// One message and no answer expected. Its own function so the reason for not
/// waiting is in one place.
async fn ask_over_protocol(websocket: &str) {
    let Ok((mut socket, _)) = tokio_tungstenite::connect_async(websocket).await else {
        return;
    };
    use futures_util::SinkExt;
    let _ = socket
        .send(tokio_tungstenite::tungstenite::Message::Text(
            r#"{"id":1,"method":"Browser.close","params":{}}"#.into(),
        ))
        .await;
    let _ = socket.close(None).await;
}

/// giving_up says whether this has failed often enough lately to stop trying,
/// and what to tell whoever asked.
fn giving_up(state: &State) -> Option<String> {
    let last = state.last_failure?;
    if last.elapsed() >= FORGET_FAILURES_AFTER || state.failures < GIVE_UP_AFTER {
        return None;
    }
    Some(format!(
        "the browser on this computer would not start, {} times running, and is not being \
         tried again for now. The last reason was: {}",
        state.failures, state.last_reason
    ))
}

/// stop ends the browser and everything it started.
///
/// Called when the application is closing. Safe to call when nothing is running,
/// which matters: the shell calls it on a path that also runs when the browser
/// was never used at all.
pub async fn stop() {
    let mut state = STATE.lock().await;
    let Some(mut running) = state.running.take() else {
        return;
    };
    forget_record(&running.profile);
    say("stopping");
    let pid = running.pid;
    off_the_runtime(move || ask_to_stop(pid)).await;

    // Asked first, insisted on only if it is still there. A browser holds a
    // profile directory open, and cutting it off mid-write is what turns the
    // next start into a repair.
    let waited = Instant::now();
    while waited.elapsed() < STOP_GRACE {
        if !still_running(&mut running.child) {
            say("stopped");
            return;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    say("it did not stop when asked; ending it");
    off_the_runtime(move || end_group(pid)).await;
    collect(&mut running.child).await;
}

/// start launches one and waits until it answers.
async fn start(exe: &Path, profile: &Path) -> Result<Running, String> {
    if !exe.is_file() {
        return Err(format!(
            "the browser is not installed on this computer ({})",
            exe.display()
        ));
    }
    tokio::fs::create_dir_all(profile)
        .await
        .map_err(|e| format!("the browser's folder could not be made: {e}"))?;

    // Whatever a previous run left. The browser writes this file when it is
    // listening and removes it when it stops cleanly, so one lying here is from
    // a run that crashed, and reading it would hand back a port that is either
    // dead or, worse, somebody else's by now.
    let port_file = profile.join("DevToolsActivePort");
    let _ = tokio::fs::remove_file(&port_file).await;

    let log = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(profile.join("browser.log"))
        .map_err(|e| format!("the browser's log could not be opened: {e}"))?;
    let errors = log
        .try_clone()
        .map_err(|e| format!("the browser's log could not be opened: {e}"))?;

    let mut command = std::process::Command::new(exe);
    command
        .arg("--headless")
        // Zero, so the operating system picks one that is free and the browser
        // writes down which. Choosing one here and hoping means racing
        // everything else on the machine, and the window between looking and
        // binding is exactly where that goes wrong.
        .arg("--remote-debugging-port=0")
        // Loopback, said out loud. It is the default, and a default is a thing
        // that can change in a release of somebody else's software.
        .arg("--remote-debugging-address=127.0.0.1")
        .arg(format!("--user-data-dir={}", profile.display()))
        .arg("--no-first-run")
        .arg("--no-default-browser-check")
        .stdin(Stdio::null())
        .stdout(Stdio::from(log))
        .stderr(Stdio::from(errors));
    own_group(&mut command);

    let mut child = command
        .spawn()
        .map_err(|e| format!("the browser could not be started: {e}"))?;
    let pid = child.id();

    match wait_until_ready(&mut child, &port_file).await {
        Ok(endpoint) => Ok(Running {
            child,
            pid,
            profile: profile.to_path_buf(),
            endpoint,
        }),
        Err(why) => {
            // Half-started: it is running but will not answer, so it is ended
            // rather than left, and collected rather than left as a zombie.
            // Bounded, for the reason `collect` gives: an unbounded wait here
            // would turn a browser that cannot be killed into a caller that
            // never returns.
            off_the_runtime(move || end_group(pid)).await;
            collect(&mut child).await;
            Err(why)
        }
    }
}

/// wait_until_ready waits for the browser to say where it is listening, and then
/// for it to actually answer there.
///
/// Both halves matter. The file appears when the socket is bound, which is not
/// the moment the browser can serve a request, and a caller that connects in
/// between gets a refusal that reads like a broken browser.
async fn wait_until_ready(child: &mut Child, port_file: &Path) -> Result<Endpoint, String> {
    let waited = Instant::now();
    while waited.elapsed() < START_DEADLINE {
        if !still_running(child) {
            return Err("the browser stopped while it was starting".into());
        }
        if let Some(endpoint) = read_endpoint(port_file).await {
            if answers(endpoint.port).await {
                return Ok(endpoint);
            }
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    Err(format!(
        "the browser did not answer within {} seconds of starting",
        START_DEADLINE.as_secs()
    ))
}

/// read_endpoint reads what the browser wrote down: the port on the first line,
/// the path to talk to it on the second.
async fn read_endpoint(port_file: &Path) -> Option<Endpoint> {
    let raw = tokio::fs::read_to_string(port_file).await.ok()?;
    let mut lines = raw.lines();
    let port: u16 = lines.next()?.trim().parse().ok()?;
    let path = lines.next()?.trim();
    if port == 0 || path.is_empty() {
        // Written but not finished: the browser appends the second line after
        // the first, and a read that lands between them sees half of it.
        return None;
    }
    Some(Endpoint {
        port,
        websocket: format!("ws://127.0.0.1:{port}{path}"),
    })
}

/// What a healthy answer contains. Not "200 OK", which a proxy or a captive
/// portal would also say: this is a field only the browser's own version
/// document has, so seeing it means we are talking to the browser.
const WANTED: &[u8] = b"webSocketDebuggerUrl";

/// answers asks the browser something and reports whether it was answered.
///
/// This is the readiness test, and it is deliberately the cheapest question the
/// protocol has. What it proves is not that the browser is healthy, which no
/// probe can, but that it is listening and serving, which is what the caller is
/// about to need.
async fn answers(port: u16) -> bool {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let Ok(Ok(mut socket)) = tokio::time::timeout(
        Duration::from_secs(2),
        tokio::net::TcpStream::connect(("127.0.0.1", port)),
    )
    .await
    else {
        return false;
    };
    let request = format!(
        "GET /json/version HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\nConnection: close\r\n\r\n"
    );
    if socket.write_all(request.as_bytes()).await.is_err() {
        return false;
    }
    // Read until the answer says what we asked, and NOT until the connection
    // closes, because it does not close.
    //
    // This was `read_to_end`, which waits for an end. The browser sends a
    // perfectly good 200 with the whole body in it and then holds the socket
    // open regardless of `Connection: close`, so the read waited out its
    // timeout every single time and the probe reported a healthy browser as
    // one that never came up. Measured against the real thing: 574 bytes
    // arrive, and then nothing ever does.
    //
    // So what ends the read is having seen enough, with time and size as the
    // backstops rather than the mechanism.
    let mut answer = Vec::new();
    let mut chunk = [0u8; 1024];
    let deadline = tokio::time::Instant::now() + Duration::from_secs(2);
    loop {
        match tokio::time::timeout_at(deadline, socket.read(&mut chunk)).await {
            Ok(Ok(0)) => break, // it did close after all
            Ok(Ok(read)) => answer.extend_from_slice(&chunk[..read]),
            Ok(Err(_)) => return false, // the socket broke
            Err(_) => break,            // it stopped sending; judge what arrived
        }
        if answer.windows(WANTED.len()).any(|w| w == WANTED) {
            return true;
        }
        if answer.len() > 8192 {
            break; // far more than this answer is, so it is not coming
        }
    }
    answer.windows(WANTED.len()).any(|w| w == WANTED)
}

/// own_group puts the browser in a process group of its own, so the whole of it
/// can be ended together.
#[cfg(unix)]
fn own_group(command: &mut std::process::Command) {
    use std::os::unix::process::CommandExt;
    command.process_group(0);
}

#[cfg(windows)]
fn own_group(command: &mut std::process::Command) {
    use std::os::windows::process::CommandExt;
    // Its own group, and no console window: this is the child of an application
    // somebody launched from an icon, and a black window flashing past is
    // something people report as a virus.
    const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    command.creation_flags(CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW);
}

/// ask_to_stop asks the browser to close, without insisting.
#[cfg(unix)]
fn ask_to_stop(pid: u32) {
    // To the group, so the renderer and its siblings hear it too.
    let _ = nix::sys::signal::killpg(
        nix::unistd::Pid::from_raw(pid as i32),
        nix::sys::signal::Signal::SIGTERM,
    );
}

#[cfg(windows)]
fn ask_to_stop(pid: u32) {
    // Without /F, which asks rather than insists. It reaches a process with no
    // window of its own only because of the process group above.
    let _ = std::process::Command::new("taskkill")
        .args(["/PID", &pid.to_string(), "/T"])
        .output();
}

/// end_group ends the browser and every process it started.
///
/// Addressed to the group rather than the process, which is a backstop and not
/// the mechanism: measured, a parent killed on its own takes its helpers with
/// it within four seconds, because they watch the channel to it. The group
/// signal is here for the helper that is wedged and not watching anything, and
/// it costs one call.
#[cfg(unix)]
fn end_group(pid: u32) {
    let _ = nix::sys::signal::killpg(
        nix::unistd::Pid::from_raw(pid as i32),
        nix::sys::signal::Signal::SIGKILL,
    );
}

#[cfg(windows)]
fn end_group(pid: u32) {
    let _ = std::process::Command::new("taskkill")
        .args(["/PID", &pid.to_string(), "/T", "/F"])
        .output();
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Both applications end the browser when they exit, and start it lazily.
    ///
    /// Read from the shells' own source, which is the honest thing a unit test
    /// can do about a Tauri event loop it cannot run. What it catches is the
    /// wiring going away, which is the failure that has already happened once
    /// in this product: the terminal was offered to nobody for a day because
    /// one constant was not bumped, and nothing errored, because withholding
    /// is what the mechanism is for.
    ///
    /// Three things are asserted, and each of them is a thing somebody could
    /// remove without any test noticing: that the exit handler ends the
    /// browser, that it does so BEFORE the gateway goes (a browser holding a
    /// profile outlives a gateway that has drained), and that the browser is
    /// only fetched at startup rather than started.
    #[test]
    fn both_applications_end_the_browser_when_they_exit() {
        for (which, source) in [
            (
                "personal",
                include_str!("../../../personal/shell/src/main.rs"),
            ),
            (
                "enterprise",
                include_str!("../../../enterprise/shell/src/main.rs"),
            ),
        ] {
            assert!(
                source.contains("browser::supervise::stop()"),
                "the {which} application does not end the browser when it exits, so closing it \
                 leaves one running until the next start reaps it"
            );
            assert!(
                source.contains("RunEvent::Exit"),
                "the {which} application has no exit handler to end it in"
            );
            let ends_browser = source
                .find("browser::supervise::stop()")
                .expect("just asserted");
            let ends_gateway = source.find("gateway::stop_running()").unwrap_or(usize::MAX);
            assert!(
                ends_browser < ends_gateway,
                "the {which} application ends the gateway before the browser. The browser is \
                 holding a profile and a handful of processes and has no reason to outlive us; \
                 the gateway is draining work."
            );
            assert!(
                source.contains("browser::bring_up("),
                "the {which} application never fetches the browser, so the first tool call \
                 would wait for a hundred and eighty megabyte download"
            );
        }
    }

    fn scratch(name: &str) -> std::path::PathBuf {
        let dir = std::env::temp_dir().join(format!("sag-browser-{}-{}", name, std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("make the test directory");
        dir
    }

    /// A half-written port file is not read as a port.
    ///
    /// The browser writes the port and then the path, and a reader that lands
    /// between the two writes sees a file with one line in it. Parsing that
    /// would hand back an endpoint with no path, which reaches the debugging
    /// port with no token and is refused in a way that reads like a broken
    /// browser rather than a read that was too early.
    #[tokio::test]
    async fn a_half_written_port_file_is_not_an_endpoint() {
        let dir = scratch("half");
        let file = dir.join("DevToolsActivePort");

        std::fs::write(&file, "57502").expect("write");
        assert!(
            read_endpoint(&file).await.is_none(),
            "a file holding only the port was read as a complete endpoint"
        );

        std::fs::write(&file, "57502\n/devtools/browser/abc-123").expect("write");
        let endpoint = read_endpoint(&file)
            .await
            .expect("the whole file is an endpoint");
        assert_eq!(endpoint.port, 57502);
        assert_eq!(
            endpoint.websocket,
            "ws://127.0.0.1:57502/devtools/browser/abc-123"
        );

        let _ = std::fs::remove_dir_all(&dir);
    }

    /// A port of zero is not a port, however well formed the file is.
    #[tokio::test]
    async fn a_zero_port_is_refused() {
        let dir = scratch("zero");
        let file = dir.join("DevToolsActivePort");
        std::fs::write(&file, "0\n/devtools/browser/abc").expect("write");
        assert!(read_endpoint(&file).await.is_none());
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Asking for a browser that is not installed says so, rather than failing
    /// somewhere further in with something about a socket.
    #[tokio::test]
    async fn a_browser_that_is_not_there_is_said_plainly() {
        let dir = scratch("missing");
        let why = start(&dir.join("no-such-browser"), &dir.join("profile"))
            .await
            .expect_err("it is not installed");
        assert!(
            why.contains("not installed"),
            "the reason should name the problem: {why}"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Something that is not a browser does not become one by being executable.
    ///
    /// It starts, it exits, and nothing ever writes a port file. What this pins
    /// is that the wait ends by NOTICING THE PROCESS IS GONE rather than by
    /// running out the ten second deadline: a supervisor that waits the full
    /// deadline for every bad start is one that takes half a minute to report
    /// three failures.
    #[tokio::test]
    async fn a_program_that_is_not_a_browser_fails_fast() {
        let dir = scratch("notabrowser");
        let fake = dir.join("fake");
        std::fs::write(&fake, "#!/bin/sh\nexit 1\n").expect("write the fake");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&fake, std::fs::Permissions::from_mode(0o755))
                .expect("make it executable");
        }

        let began = Instant::now();
        let why = start(&fake, &dir.join("profile"))
            .await
            .expect_err("it is not a browser");
        let took = began.elapsed();

        // Two different refusals for one behaviour, which is why this is not
        // one string.
        //
        // The fixture is a shell script, so Unix RUNS it and it exits, and the
        // supervisor notices the process went. Windows will not start it at all
        // ("%1 is not a valid Win32 application", os error 193), because a file
        // beginning `#!` is not an executable there and never was. Both are the
        // property this test is about: a program that is not a browser is done
        // with FAST. Asserting only the Unix wording made this fail on Windows
        // for a difference that is not a defect.
        //
        // What must not be relaxed is the bound below: that is the assertion
        // the bug lived behind.
        assert!(
            why.contains("stopped while it was starting")
                || (cfg!(windows) && why.contains("could not be started")),
            "it should say the process went or would not start, not time out: {why}"
        );
        // Two seconds, not START_DEADLINE. Against the deadline this assertion
        // would pass at 9.9 seconds, which IS the bug it exists to catch: the
        // first version of this file asked the operating system whether the
        // process id existed, which is true of one that has exited and not been
        // collected, so it waited the whole deadline out every time. A bound
        // the bug satisfies is not a bound.
        assert!(
            took < Duration::from_secs(2),
            "it waited {took:?} for a process that had already exited, so the deadline is \
             doing the work instead of the child being asked"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Three failures in a row stop the attempts, and a fourth ask is told why
    /// rather than spawning another process.
    #[test]
    fn a_browser_that_will_not_start_is_not_tried_for_ever() {
        let mut state = State {
            running: None,
            failures: 1,
            last_failure: Some(Instant::now()),
            last_reason: "the disk is full".into(),
        };
        assert!(giving_up(&state).is_none(), "one failure is not a pattern");

        state.failures = GIVE_UP_AFTER;
        let reason = giving_up(&state).expect("three failures stop it");
        assert!(
            reason.contains("the disk is full"),
            "it must carry the real reason, not only a count: {reason}"
        );

        // And it forgets. A browser that failed this morning says nothing about
        // one this afternoon, and holding it would mean restarting the whole
        // application to recover from something that fixed itself.
        state.last_failure = Some(Instant::now() - FORGET_FAILURES_AFTER - Duration::from_secs(1));
        assert!(
            giving_up(&state).is_none(),
            "an old run of failures is still being held against it"
        );
    }

    /// Checking on a browser an earlier run left behind must not hold the
    /// thread the link answers the gateway on (KB/29).
    ///
    /// On Windows the check starts `tasklist` and waits for it, about a third
    /// of a second measured. What this measures is the longest the runtime went
    /// without getting to do anything else: a check done on its own thread makes
    /// that the whole wait, and one on the blocking pool leaves it at almost
    /// nothing. Windows only, because elsewhere the check is one system call
    /// and there is no wait to move.
    ///
    /// The process asked about cannot exist: Windows numbers processes in
    /// multiples of four, so this can never find, let alone end, a real one.
    #[cfg(windows)]
    #[tokio::test]
    async fn checking_on_a_left_browser_leaves_the_link_free() {
        use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
        use std::sync::Arc;

        let profile = scratch("left-behind");
        write_record(&profile, 999_999_999);

        let longest = Arc::new(AtomicU64::new(0)); // microseconds
        let going = Arc::new(AtomicBool::new(true));
        let (seen, still) = (longest.clone(), going.clone());
        let ticker = tokio::spawn(async move {
            let mut last = Instant::now();
            while still.load(Ordering::Relaxed) {
                tokio::task::yield_now().await;
                let now = Instant::now();
                seen.fetch_max((now - last).as_micros() as u64, Ordering::Relaxed);
                last = now;
            }
        });
        tokio::task::yield_now().await;
        end_any_orphan(&profile).await;
        going.store(false, Ordering::Relaxed);
        let _ = ticker.await;

        assert!(
            read_record(&profile).is_none(),
            "the record of a browser that has gone was kept, so the check never ran"
        );
        let longest = Duration::from_micros(longest.load(Ordering::Relaxed));
        assert!(
            longest < Duration::from_millis(100),
            "the runtime could do nothing for {longest:?} while the check ran, so the link could not have answered the gateway meanwhile"
        );
        let _ = std::fs::remove_dir_all(&profile);
    }
}
