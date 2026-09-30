//! Getting the next version, without asking anybody to stop what they are doing.
//!
//! The promise is the same on every platform and it is the whole design: check
//! quietly, download quietly, say ONE thing once, and let the next launch be the
//! newer version. Nothing is lost by ignoring that for a week, and nothing is
//! ever interrupted. What DIFFERS between the platforms is the moment the bytes
//! go into place, because the two systems do not allow the same moment.
//!
//! **macOS puts it in place at once.** The process already running keeps its own
//! copy of the bundle alive, so swapping the application under it disturbs
//! nothing at all. There is no reason to wait and no reason to ask.
//!
//! **Windows puts it in place when the person quits**, and that is not a
//! preference. There is no way to replace an installed application there without
//! running its installer, and `tauri_plugin_updater` runs it and then ends this
//! process: `Update::download_and_install` is documented as "exits the app after
//! launching the updater installer successfully" and does it with
//! `std::process::exit(0)`. Called the moment a download finishes, that is an
//! application vanishing mid-conversation, and the announcement below would be
//! unreachable code. So the download is kept (`keep_or_install`) and put in
//! place from the exit path (`install_pending`), when the person has already
//! decided to stop.
//!
//! Two further things follow from that, and both are load-bearing:
//!
//! * **The gateway must be stopped first.** It runs as a detached child holding
//!   the database, so it survives this process; a running `sag.exe` cannot be
//!   overwritten (Windows refuses to write a running image), and the installer
//!   would fail on the one file that matters. Each edition therefore calls
//!   `install_pending` AFTER whatever it has to quiesce.
//! * **Whoever calls `watch` must call `install_pending`.** A shell that only
//!   did the first would download every new version and install none of them,
//!   silently, for ever. `both_shells_put_a_held_version_in_place` is what keeps
//!   that from being possible to forget.
//!
//! **What it will not do, anywhere, is restart.** The gateway is holding a
//! database and possibly a conversation being written; ending that to save
//! somebody a double-click is a bad trade.
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
use tauri_plugin_updater::{Update, UpdaterExt};

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
            // also keeps no record of what it fetched. So a version already
            // dealt with is offered again on the next tick, and the round below
            // would fetch it again: sixty-six megabytes every six hours for as
            // long as the application stays open, all of it after the work was
            // already done. On Windows it would also throw away the copy being
            // held and hold an identical one.
            //
            // There is nothing left to do anyway. The new version is on disk or
            // in hand and the chip has said so; what remains is a person
            // reopening, and no amount of asking again brings that closer.
            if fetch(&app).await == Outcome::Ready {
                return;
            }
            tokio::time::sleep(THEN_EVERY).await;
        }
    });
}

/// What one round of asking came to.
#[derive(PartialEq, Eq, Debug)]
enum Outcome {
    /// A new version is dealt with: in place on macOS, in hand on Windows.
    /// Either way there is no reason to ask again.
    Ready,
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
    if let Err(err) = obtain(update).await {
        eprintln!("update: version {version} could not be installed: {err}");
        return Outcome::NothingToDo;
    }
    eprintln!("update: version {version} is ready and starts on the next launch");

    // Every window, because whichever one somebody is looking at is the one
    // that should say so, and there is no way to know which that is.
    if let Err(err) = app.emit(READY, Ready { version }) {
        eprintln!("update: nothing was listening for the new version: {err}");
    }
    Outcome::Ready
}

/// Ask now, rather than waiting for the next round.
///
/// For a person who has just been told there is a new version and wants it, and
/// for the tests. It answers whether one was fetched rather than doing it
/// silently, because somebody who asked is somebody waiting for an answer.
#[tauri::command]
pub async fn check_for_update<R: Runtime>(app: AppHandle<R>) -> Result<Option<String>, String> {
    let updater = app.updater().map_err(|err| err.to_string())?;
    let update = updater.check().await.map_err(|err| err.to_string())?;
    let Some(update) = update else { return Ok(None) };
    let version = update.version.clone();
    obtain(update).await?;
    let _ = app.emit(READY, Ready { version: version.clone() });
    Ok(Some(version))
}

/// Gets the next version and puts it where this platform allows.
///
/// The two halves are the same promise and different timing; the split is at the
/// top of this file.
///
/// This one is UNCHANGED, and the call is written out rather than assembled from
/// its two halves on purpose: it is what every platform but Windows has been
/// doing since the updater shipped, it is tested, and it is not being rewritten
/// to share code with a platform that needed something else.
#[cfg(not(windows))]
async fn obtain(update: Update) -> Result<(), String> {
    update
        .download_and_install(|_, _| {}, || {})
        .await
        .map_err(|err| err.to_string())
}

/// And the Windows one, which is the same pair of calls with the person's own
/// decision to quit in between. `download_and_install` cannot be used here: its
/// second half ends this process, so calling it when a download finishes is an
/// application disappearing mid-conversation.
#[cfg(windows)]
async fn obtain(update: Update) -> Result<(), String> {
    let bytes = update
        .download(|_, _| {}, || {})
        .await
        .map_err(|err| err.to_string())?;
    // Held, because putting it in place means running an installer that ends
    // this process. `install_pending` does it on the way out.
    let mut held = WAITING
        .lock()
        .map_err(|_| "the held version could not be recorded".to_string())?;
    *held = Some(Waiting { update, bytes });
    Ok(())
}

/// A version that has been fetched and is waiting for the person to quit.
///
/// The bytes are held rather than written to a file of our own: the plugin takes
/// them as bytes, writes its own temporary copy and cleans that up, and an
/// installer we left on somebody's disk after a crash would be ours to tidy. It
/// is one installer, about twenty megabytes, for as long as the application is
/// open.
#[cfg(windows)]
struct Waiting {
    update: Update,
    bytes: Vec<u8>,
}

#[cfg(windows)]
static WAITING: std::sync::Mutex<Option<Waiting>> = std::sync::Mutex::new(None);

/// Puts a held version in place, if one is waiting. Nothing at all otherwise.
///
/// Called from the exit path of each edition, AFTER that edition has stopped
/// whatever it owns. On Windows this does not return: the plugin launches the
/// installer and ends the process, which is what we want at exactly this moment
/// and nowhere else.
#[cfg(windows)]
pub fn install_pending() {
    let taken = match WAITING.lock() {
        Ok(mut held) => held.take(),
        // Poisoned by a panic somewhere else. There is nothing to recover and
        // the application is on its way out; the version is offered again next
        // time, which is the same outcome as never having downloaded it.
        Err(_) => None,
    };
    let Some(waiting) = taken else { return };
    let version = waiting.update.version.clone();
    if let Err(err) = waiting.update.install(waiting.bytes) {
        // Reached only when the installer could not be STARTED. Once it starts,
        // this process is gone.
        eprintln!("update: version {version} could not be put in place: {err}");
    }
}

/// The same door on a platform that never holds anything back.
#[cfg(not(windows))]
pub fn install_pending() {}

#[cfg(test)]
mod tests {
    /// A shell that looks for a new version has to put one in place.
    ///
    /// On Windows the two are separated by however long somebody leaves the
    /// application open, which makes forgetting the second half a defect with no
    /// symptom: every release is downloaded, none is ever installed, and the
    /// only visible sign is a version that never changes. Asserted from here
    /// rather than trusted, because the call lives in a file this crate does not
    /// otherwise read.
    ///
    /// Required of an edition that BUILDS FOR WINDOWS, which is what having a
    /// `tauri.windows.conf.json` means, rather than of every edition. The
    /// enterprise shell ships on macOS only today, where nothing is ever held
    /// and the call would do nothing; the day somebody gives it a Windows
    /// configuration, this starts asking for the other half, which is the day it
    /// starts to matter.
    /// The updater configuration is one the plugin will actually accept.
    ///
    /// Nothing else checks this. `tauri build` passes `plugins` through without
    /// reading it, and the plugin deserializes it at RUN time, so a misspelled
    /// key or an install mode that is not one of the three makes
    /// `app.updater()` fail on a machine somebody has already installed. The
    /// only visible sign is "no updater on this build" in a log nobody is
    /// reading, and an application that never updates again: the same silent,
    /// permanent freeze as the Apple Silicon gap, arrived at from the config.
    ///
    /// So it is parsed here with the plugin\'s OWN type, which is the only thing
    /// that can answer the question.
    #[test]
    fn the_updater_configuration_is_one_the_plugin_accepts() {
        let config: serde_json::Value =
            serde_json::from_str(include_str!("../../personal/shell/tauri.conf.json"))
                .expect("the personal shell config is not valid JSON");
        let updater = config
            .get("plugins")
            .and_then(|p| p.get("updater"))
            .expect("the personal shell no longer configures an updater");

        let parsed: tauri_plugin_updater::Config = serde_json::from_value(updater.clone())
            .expect("the plugin would refuse this configuration at startup");

        assert!(
            !parsed.endpoints.is_empty(),
            "an updater with no endpoint asks nobody anything"
        );

        // And the Windows half, which decides what a person SEES when a held
        // version goes in. The plugin defaults to `passive`, documented as
        // showing a progress bar; the install happens from the exit path, so
        // passive is a progress bar appearing after somebody closed the
        // application. Read through Debug because the mode\'s type is not
        // exported, which is also why nothing else could assert it.
        let windows = format!("{:?}", parsed.windows);
        assert!(
            windows.contains("Quiet"),
            "the Windows install mode is {windows}, so an update would put a \
             window on somebody\'s screen after they quit"
        );
    }

    #[test]
    fn a_shell_that_builds_for_windows_puts_a_held_version_in_place() {
        for (edition, windows_config, source) in [
            (
                "personal",
                include_str!("../../personal/shell/tauri.windows.conf.json"),
                include_str!("../../personal/shell/src/main.rs"),
            ),
            (
                "enterprise",
                // No Windows configuration of its own: the empty string stands
                // for "this edition does not build for that platform".
                "",
                include_str!("../../enterprise/shell/src/main.rs"),
            ),
        ] {
            assert!(
                source.contains("update::watch"),
                "the {edition} shell no longer looks for a new version"
            );
            if windows_config.is_empty() {
                continue;
            }
            assert!(
                source.contains("update::install_pending"),
                "the {edition} shell builds for Windows, downloads a new version there,                  and never puts it in place"
            );
        }
    }
}
