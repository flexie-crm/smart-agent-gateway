//! The supervisor, against a real browser.
//!
//! Everything in the unit tests is about the supervisor's own reasoning: a
//! half-written port file, a program that is not a browser, a run of failures.
//! None of it starts Chromium, and none of it can tell you that the thing we
//! ship actually comes up, answers, and leaves nothing behind when it goes.
//!
//! This does. It is separate for the reason the link's gate is separate: it
//! needs something on the machine that a plain `cargo test` cannot assume, so
//! it is driven by `make browser-e2e`, which points it at a browser.
//!
//! **The orphan assertion is the point of the file.** A browser is several
//! processes, and a `stop` that returns without ending them is not a failure
//! anything reports: the call returns, the test passes, the application quits,
//! and the processes are still there with nothing left that knows about them.
//! So the count is taken before and after, and it has to come back to zero.
//!
//! What that assertion does NOT prove, said here because the first version of
//! this comment claimed it did: it is not evidence that signalling the process
//! group beats signalling the process. That control was run and it passed,
//! because Chromium's helpers watch the channel to the browser process and exit
//! on their own within four seconds of it dying. The control that does fail is
//! a `stop` that signals nothing, which leaves three processes behind.

use std::path::PathBuf;
use std::time::{Duration, Instant};

use sag_desktop::browser::supervise;

/// Where the browser is, for a run that has one.
///
/// A gate that quietly passes because it skipped is worse than one that fails,
/// and this repository has been bitten by exactly that: three MCP tests
/// asserted a name that had been renamed and stayed green for weeks, because
/// the whole package skipped without a database. So the skip says what is
/// missing and how to give it, and `make browser-e2e` always sets it.
fn browser() -> Option<PathBuf> {
    let path = std::env::var("SAG_BROWSER_PATH").ok()?;
    let path = PathBuf::from(path);
    path.is_file().then_some(path)
}

/// How many processes of the browser we started are running right now.
///
/// Counted by PROCESS GROUP, and the first version of this counted by the
/// profile path instead, which was wrong in a way that made the whole file
/// useless: only the parent carries `--user-data-dir` on its command line. The
/// helpers carry `--type=renderer` and friends and no profile at all, so the
/// count was always one, and the control (signal the process instead of the
/// group) passed happily.
///
/// The group is the right unit because it is the one the supervisor creates:
/// `process_group(0)` puts the browser in a group of its own, so every process
/// of this browser is in it and nothing else on the machine is, however many
/// other Chromiums a developer has open.
///
/// The parent is found by the profile path, which only it carries, and its
/// group is read from there.
#[cfg(unix)]
fn processes_holding(profile: &std::path::Path) -> usize {
    let out = std::process::Command::new("ps")
        .args(["-eo", "pid=,pgid=,command="])
        .output()
        .expect("ask the system what is running");
    let listing = String::from_utf8_lossy(&out.stdout);
    let needle = profile.to_string_lossy().to_string();

    // The group, from whichever process carries the profile path.
    let group = listing
        .lines()
        .find(|line| line.contains(&needle) && line.contains("chrome-headless-shell"))
        .and_then(|line| line.split_whitespace().nth(1))
        .and_then(|pgid| pgid.parse::<i64>().ok());
    let Some(group) = group else {
        return 0; // no parent, so no browser, so nothing in its group either
    };

    listing
        .lines()
        .filter(|line| line.contains("chrome-headless-shell"))
        .filter(|line| {
            line.split_whitespace()
                .nth(1)
                .and_then(|pgid| pgid.parse::<i64>().ok())
                == Some(group)
        })
        .count()
}

/// The same count on Windows, where the unit is the process TREE rather than a
/// process group.
///
/// Windows HAS process groups, and `CREATE_NEW_PROCESS_GROUP` is what the
/// supervisor sets, but a group there is a target for console control events
/// rather than something a process can be listed by: there is no `pgid` column
/// to read back. Parentage can be read back, and it answers the same question,
/// because the helpers Chromium starts are children of the browser process.
///
/// The ROOT is found exactly as it is on Unix and for the same recorded reason:
/// only the parent carries `--user-data-dir`, so the profile identifies it, and
/// the rest are counted by descending from it rather than by looking for a
/// profile they do not carry.
///
/// Without this the whole file did not COMPILE on Windows: the Unix version was
/// gated and every call site was not, so five unresolved names took the browser
/// gate down on this platform entirely (`make browser-e2e` builds all targets,
/// so it never reached a test).
#[cfg(windows)]
fn processes_holding(profile: &std::path::Path) -> usize {
    // Tab separated rather than CSV: a command line is full of commas and
    // quotes and empty of tabs, so there is nothing to escape and nothing to
    // parse wrongly.
    const ASK: &str = "Get-CimInstance Win32_Process -Filter \"Name='chrome-headless-shell.exe'\" \
         | ForEach-Object { \"$($_.ProcessId)`t$($_.ParentProcessId)`t$($_.CommandLine)\" }";
    let out = std::process::Command::new("powershell")
        .args(["-NoProfile", "-NonInteractive", "-Command", ASK])
        .output()
        .expect("ask the system what is running");
    let listing = String::from_utf8_lossy(&out.stdout);
    let needle = profile.to_string_lossy().to_string();

    let mut parentage: Vec<(u32, u32)> = Vec::new();
    let mut root: Option<u32> = None;
    for line in listing.lines() {
        let mut parts = line.splitn(3, '\t');
        let Some(pid) = parts.next() else { continue };
        let Some(parent) = parts.next() else { continue };
        let command = parts.next().unwrap_or("");
        let (Ok(pid), Ok(parent)) = (pid.trim().parse::<u32>(), parent.trim().parse::<u32>())
        else {
            continue;
        };
        parentage.push((pid, parent));
        if command.contains(&needle) {
            root = Some(pid);
        }
    }
    let Some(root) = root else {
        return 0; // no parent, so no browser, so no tree under it either
    };

    // The root and everything descended from it. Repeated until it stops
    // growing rather than recursing, because the listing arrives in no
    // particular order and a child can be named before its parent.
    let mut held = vec![root];
    loop {
        let before = held.len();
        for (pid, parent) in &parentage {
            if held.contains(parent) && !held.contains(pid) {
                held.push(*pid);
            }
        }
        if held.len() == before {
            break;
        }
    }
    held.len()
}

/// A real browser starts, answers, and goes without leaving anything behind.
#[tokio::test]
async fn a_real_browser_starts_answers_and_leaves_nothing_behind() {
    let Some(exe) = browser() else {
        panic!(
            "SAG_BROWSER_PATH is not set to a browser, so this gate would prove nothing. \
             Run it through `make browser-e2e`, which points it at one."
        );
    };
    let profile = std::env::temp_dir().join(format!("sag-browser-live-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&profile);

    assert_eq!(
        processes_holding(&profile),
        0,
        "something is already running against this profile before the test started"
    );

    // It starts, and what proves it is ready is that it answered.
    let began = Instant::now();
    let endpoint = supervise::ensure(&exe, &profile)
        .await
        .expect("the browser should start");
    let took = began.elapsed();
    assert!(endpoint.port > 0, "no port");
    assert!(
        endpoint.websocket.starts_with("ws://127.0.0.1:"),
        "the protocol should be on loopback and nowhere else: {}",
        endpoint.websocket
    );
    // The token in the path is what keeps anything else on this machine from
    // driving the browser. An endpoint without one is a browser with an open
    // door, so its absence is a failure rather than a detail.
    assert!(
        endpoint.websocket.contains("/devtools/browser/"),
        "the endpoint carries no token: {}",
        endpoint.websocket
    );
    assert!(
        took < Duration::from_secs(10),
        "it took {took:?} to become ready"
    );

    let running = processes_holding(&profile);
    assert!(
        running > 0,
        "the browser reported ready and nothing is running against its profile"
    );

    // Asking twice does not start a second one. This is what stops every tool
    // call spawning its own browser.
    let again = supervise::ensure(&exe, &profile)
        .await
        .expect("the second ask should get the same browser");
    assert_eq!(
        again.port, endpoint.port,
        "the second ask started a different browser"
    );
    assert_eq!(
        processes_holding(&profile),
        running,
        "asking twice changed how many processes are running"
    );

    // And it goes, with everything it started.
    supervise::stop().await;

    // The processes do not vanish the instant the signal is sent. A few hundred
    // milliseconds of grace, and then the count has to be zero: this is the
    // assertion the whole file exists for.
    let waited = Instant::now();
    while waited.elapsed() < Duration::from_secs(5) {
        if processes_holding(&profile) == 0 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    let left = processes_holding(&profile);
    assert_eq!(
        left, 0,
        "{left} of the browser's processes are still running after it was stopped. The \
         application would quit and leave them behind, holding its profile directory, with \
         nothing left that knows about them."
    );

    let _ = std::fs::remove_dir_all(&profile);
}

/// After it has been stopped, asking again starts a fresh one.
///
/// The supervisor keeps what it started in a global, and a `stop` that cleared
/// the handle but left the browser, or cleared the browser but left the handle,
/// would both show up here: the first as a second browser nobody can stop, the
/// second as an `ensure` that hands back a port with nothing behind it.
#[tokio::test]
async fn it_can_be_started_again_after_being_stopped() {
    let Some(exe) = browser() else {
        panic!("SAG_BROWSER_PATH is not set to a browser; run this through `make browser-e2e`");
    };
    let profile = std::env::temp_dir().join(format!("sag-browser-again-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&profile);

    let first = supervise::ensure(&exe, &profile)
        .await
        .expect("first start");
    supervise::stop().await;
    let second = supervise::ensure(&exe, &profile)
        .await
        .expect("it should start again after being stopped");

    assert_ne!(
        first.port, second.port,
        "the second start handed back the first browser's port, which is now dead"
    );

    supervise::stop().await;
    let _ = std::fs::remove_dir_all(&profile);
}

/// A browser left behind by a crashed run is ended, not joined.
///
/// This is the one that matters for owning the process, and the behaviour it
/// prevents was measured rather than feared: a second browser started against
/// a profile another one is holding does NOT fail. It starts, listens on a new
/// port, OVERWRITES the port file, and two browsers write to one profile while
/// logging lock errors. The one that was already there becomes unreachable,
/// because the only record of its port has just been replaced, so nothing can
/// stop it ever again. Every force quit would add one more.
///
/// The orphan here is real: a browser started outside the supervisor, on the
/// profile the supervisor is about to use, exactly as a killed application
/// leaves one.
#[tokio::test]
async fn a_browser_from_a_crashed_run_is_ended_before_another_starts() {
    let Some(exe) = browser() else {
        panic!("SAG_BROWSER_PATH is not set to a browser; run this through `make browser-e2e`");
    };
    let profile = std::env::temp_dir().join(format!("sag-browser-orphan-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&profile);

    // A browser nothing owns, on the profile the supervisor will want. Started
    // the way the supervisor starts one, so the port file it writes is the one
    // the supervisor would read.
    let mut orphan = std::process::Command::new(&exe)
        .arg("--headless")
        .arg("--remote-debugging-port=0")
        .arg("--remote-debugging-address=127.0.0.1")
        .arg(format!("--user-data-dir={}", profile.display()))
        .arg("--no-first-run")
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn()
        .expect("start an orphan browser");
    let orphaned = orphan.id();

    // Everything is GATHERED first and asserted at the end, with the orphan
    // ended in between. This test deliberately makes a process nothing owns,
    // so an assertion firing early would leave it running: the same leak this
    // file's header is about, in the test written to catch it. Clippy said so
    // before it bit.
    let port_file = profile.join("DevToolsActivePort");
    let waited = Instant::now();
    while waited.elapsed() < Duration::from_secs(15) && !port_file.is_file() {
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    let came_up = port_file.is_file();
    let was = std::fs::read_to_string(&port_file).unwrap_or_default();

    // Now the supervisor starts, as a fresh run of the application does.
    let started = supervise::ensure(&exe, &profile).await;

    // The orphan is GONE. Asked of the process itself rather than of the
    // operating system: a zombie's id still answers a signal, which is the
    // trap this file's header is about.
    let ended = Instant::now();
    let mut gone = false;
    while ended.elapsed() < Duration::from_secs(10) {
        if matches!(orphan.try_wait(), Ok(Some(_))) {
            gone = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    let now = std::fs::read_to_string(&port_file).unwrap_or_default();

    // Cleared up before anything is asserted, on every path. Killed only if
    // it is still there, and waited on either way: `try_wait` above has
    // already reaped it when it went on its own, and waiting again on a child
    // that is gone is an error nobody needs to hear about.
    if !gone {
        let _ = orphan.kill();
    }
    let _ = orphan.wait();
    supervise::stop().await;
    let _ = std::fs::remove_dir_all(&profile);

    assert!(came_up, "the orphan never came up, so this proved nothing");
    let endpoint = started.expect("the supervisor starts");
    assert!(
        gone,
        "the orphan (process {orphaned}) is still running, so two browsers now share one profile"
    );
    assert_ne!(
        now.lines().next(),
        was.lines().next(),
        "the port file still names the orphan's port"
    );
    assert!(
        endpoint.websocket.contains(&format!("{}", endpoint.port)),
        "the supervisor's endpoint does not match its own port"
    );
}

/// A browser that is alive but will not answer is ended too.
///
/// The hole this closes: "it does not answer" was being read as "it is not
/// there". A browser can be alive and unreachable, and every one of those
/// states is ordinary: a hung renderer, a process stopped by a debugger, one
/// still coming up. It is still holding the profile, so starting another
/// against it is the exact situation the reaping exists to prevent.
///
/// Stopped with SIGSTOP here, which is the cleanest way to make a real browser
/// unreachable without killing it: the process is there, its port is bound,
/// and nothing it is asked ever comes back.
#[cfg(unix)]
#[tokio::test]
async fn a_browser_that_will_not_answer_is_ended_too() {
    let Some(exe) = browser() else {
        panic!("SAG_BROWSER_PATH is not set to a browser; run this through `make browser-e2e`");
    };
    let profile = std::env::temp_dir().join(format!("sag-browser-hung-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&profile);

    // A browser this supervisor started, so the pid is on record exactly as a
    // real run leaves it.
    let first = supervise::ensure(&exe, &profile)
        .await
        .expect("the first browser starts");
    let hung = std::fs::read_to_string(profile.join("supervised.pid"))
        .expect("the supervisor wrote its pid down")
        .trim()
        .parse::<i32>()
        .expect("a pid");

    // Make it unreachable WITHOUT killing it, and forget it the way a crashed
    // application does: the record on disk is all the next run will have.
    nix::sys::signal::kill(
        nix::unistd::Pid::from_raw(hung),
        nix::sys::signal::Signal::SIGSTOP,
    )
    .expect("stop the browser");
    supervise::forget_for_test().await;

    // It really is unreachable now, or this proves nothing.
    let unreachable = !supervise::answers_for_test(first.port).await;

    // A fresh run of the application.
    let second = supervise::ensure(&exe, &profile).await;

    // Gathered, cleaned up, then asserted: this test makes a process nothing
    // owns, so an assertion firing early would leave a STOPPED browser behind
    // that nothing would ever reap.
    let still_there = alive(hung);
    if still_there {
        let _ = nix::sys::signal::kill(
            nix::unistd::Pid::from_raw(hung),
            nix::sys::signal::Signal::SIGCONT,
        );
        let _ = nix::sys::signal::killpg(
            nix::unistd::Pid::from_raw(hung),
            nix::sys::signal::Signal::SIGKILL,
        );
    }
    supervise::stop().await;
    let _ = std::fs::remove_dir_all(&profile);

    assert!(
        unreachable,
        "the browser still answered after being stopped, so this tested nothing"
    );
    assert!(
        second.is_ok(),
        "the supervisor could not start after a hung predecessor: {second:?}"
    );
    assert!(
        !still_there,
        "a browser that would not answer ({hung}) was left running, holding the profile"
    );
}

/// Whether a process id is still a RUNNING process.
///
/// Asked of `ps`, not of a signal, and the first version of this asked with a
/// signal and was wrong: the browser had been killed perfectly and its state
/// was "Z", a zombie waiting to be reaped by the test process that started it.
/// A zombie still accepts signal zero, so the test reported a dead browser as
/// still holding the profile. That is the trap this file's own header is
/// about, walked into while writing the test for it.
///
/// It does not apply to the supervisor's own check: a zombie only exists while
/// its parent is alive, and the case there is a pid recorded by an application
/// that has since died, whose children are reparented and reaped by the
/// system.
#[cfg(unix)]
fn alive(pid: i32) -> bool {
    let out = std::process::Command::new("ps")
        .args(["-o", "state=", "-p", &pid.to_string()])
        .output();
    let Ok(out) = out else { return false };
    let state = String::from_utf8_lossy(&out.stdout).trim().to_string();
    !state.is_empty() && !state.starts_with('Z')
}
