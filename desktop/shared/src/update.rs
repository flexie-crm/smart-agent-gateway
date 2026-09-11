//! Getting the next version, without asking anybody to stop what they are doing.
//!
//! The shape is decided by what macOS actually does when a bundle is replaced:
//! the process already running keeps its own copy alive, so swapping the
//! application under it disturbs nothing. There is therefore no reason to
//! interrupt somebody mid-conversation to ask permission for a download, and no
//! reason to restart on their behalf either.
//!
//! So: check quietly, download quietly, install quietly, and then say ONE thing,
//! once, which is that the next version is already on the disk and starts the
//! next time they open the application. Nothing is lost by ignoring that for a
//! week.
//!
//! **What it will not do is restart.** The gateway is holding a database and
//! possibly a conversation being written; ending that to save somebody a
//! double-click is a bad trade. Quitting normally runs the ordered shutdown
//! (`RunEvent::Exit`), and the new version is what opens next.
//!
//! # What makes an update trustworthy
//!
//! A minisign keypair that has nothing to do with Apple's certificates: the
//! public half is compiled into the application, the private half signs each
//! release. An update whose signature does not verify is not installed, and that
//! check happens before anything is written, so a compromised download host can
//! serve nothing worse than a refusal.
//!
//! The bundle it downloads is signed and notarised by the same path a fresh
//! install takes, because a replaced bundle that is not properly signed fails
//! Gatekeeper on the next launch: the update channel is not a looser one.

use std::time::Duration;

use serde::Serialize;
use tauri::{AppHandle, Emitter, Runtime};
use tauri_plugin_updater::UpdaterExt;

/// How long after starting before the first check.
///
/// Not immediately: the first seconds after launch are the gateway starting, a
/// database opening and a window waiting to paint, and a download competing with
/// that is a slower start for no gain. Nothing here is urgent.
const FIRST_CHECK: Duration = Duration::from_secs(90);

/// And how often after that, for an application somebody leaves open for days.
const THEN_EVERY: Duration = Duration::from_secs(6 * 60 * 60);

/// What the page is told when a version is waiting. One event, sent once.
#[derive(Debug, Clone, Serialize)]
pub struct Ready {
    /// The version that will run next time, so the page can name it.
    pub version: String,
}

/// The event name. Spelled out here rather than at both ends, because a
/// listener that mishears one is a banner that never appears and an error
/// nobody sees.
pub const READY: &str = "sag://update-ready";

/// Watch for a new version, fetch it, and say when it is there.
///
/// Failures are logged and nothing else. An application that cannot reach its
/// update host is an application that goes on working, and somebody who is
/// offline, or behind a proxy that refuses, must not be told about it every six
/// hours in a dialog they cannot act on.
pub fn watch<R: Runtime>(app: AppHandle<R>) {
    tauri::async_runtime::spawn(async move {
        tokio::time::sleep(FIRST_CHECK).await;
        loop {
            // Stop once something is installed, and this is not tidiness.
            //
            // The plugin compares against the version compiled into the RUNNING
            // process (its `current_version`, taken from `package_info()` at
            // startup), which replacing the bundle on disk cannot change. It
            // also keeps no record of what it installed. So an installed update
            // is offered again on the next tick, and `download_and_install`
            // happily re-extracts it over itself: sixty-six megabytes fetched
            // and written every six hours, for as long as the application stays
            // open, all of it after the work was already done.
            //
            // There is nothing left to do anyway. The new version is on disk and
            // the chip has said so; what remains is a person reopening, and no
            // amount of asking again brings that closer.
            if fetch(&app).await == Outcome::Installed {
                return;
            }
            tokio::time::sleep(THEN_EVERY).await;
        }
    });
}

/// What one round of asking came to.
#[derive(PartialEq, Eq, Debug)]
enum Outcome {
    /// A new version is on disk. There is no reason to ask again.
    Installed,
    /// Current, unreachable, or it failed. Worth asking again later.
    NothingToDo,
}

async fn fetch<R: Runtime>(app: &AppHandle<R>) -> Outcome {
    let updater = match app.updater() {
        Ok(updater) => updater,
        Err(err) => {
            // No endpoint configured, or no public key: a build that cannot
            // update rather than a failure to update.
            eprintln!("update: no updater on this build: {err}");
            return Outcome::NothingToDo;
        }
    };
    let update = match updater.check().await {
        Ok(Some(update)) => update,
        // Already current. The common answer, and not worth a line in a log
        // somebody reads when something is wrong.
        Ok(None) => return Outcome::NothingToDo,
        Err(err) => {
            eprintln!("update: could not ask about a new version: {err}");
            return Outcome::NothingToDo;
        }
    };

    let version = update.version.clone();
    // Downloaded and written in one call, which is what the plugin offers. On
    // macOS this replaces the bundle beside the running process and leaves it
    // untouched; the next launch is the new one.
    if let Err(err) = update.download_and_install(|_, _| {}, || {}).await {
        eprintln!("update: version {version} could not be installed: {err}");
        return Outcome::NothingToDo;
    }
    eprintln!("update: version {version} is installed and starts on the next launch");

    // Every window, because whichever one somebody is looking at is the one
    // that should say so, and there is no way to know which that is.
    if let Err(err) = app.emit(READY, Ready { version }) {
        eprintln!("update: nothing was listening for the new version: {err}");
    }
    Outcome::Installed
}

/// Ask now, rather than waiting for the next round.
///
/// For a person who has just been told there is a new version and wants it, and
/// for the tests. It answers whether one was installed rather than doing it
/// silently, because somebody who asked is somebody waiting for an answer.
#[tauri::command]
pub async fn check_for_update<R: Runtime>(app: AppHandle<R>) -> Result<Option<String>, String> {
    let updater = app.updater().map_err(|err| err.to_string())?;
    let update = updater.check().await.map_err(|err| err.to_string())?;
    let Some(update) = update else { return Ok(None) };
    let version = update.version.clone();
    update
        .download_and_install(|_, _| {}, || {})
        .await
        .map_err(|err| err.to_string())?;
    let _ = app.emit(READY, Ready { version: version.clone() });
    Ok(Some(version))
}
