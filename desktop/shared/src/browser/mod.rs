//! The browser this computer drives.
//!
//! Not the window anybody looks at. This is a headless build of Chromium, kept
//! as a separate program and spoken to over a socket, which the assistant uses
//! to read a page, act on one, and look at a user interface it is working on.
//!
//! **Why it is not the application's own webview.** The window a person is
//! looking at is the wrong place to automate: a page that crashes would take
//! the chat with it, the cookies would share a jar with their session, and only
//! one page can be open at a time. It is also, on a Mac, impossible: the
//! embedded webview is WKWebView, which has no debugging protocol reachable
//! from this process, so the actions a page needs (a real click, a real
//! keystroke) have nowhere to come from.
//!
//! **What we own and what we borrow.** The infrastructure is ours: fetching the
//! browser, running it, supervising it, speaking its protocol, and the tools the
//! assistant is offered. The part that knows how to find a button by its role
//! and decide whether it can be clicked is Playwright's, injected into the page
//! as their own script, and updated by taking their next one. Writing a selector
//! engine is the debt this avoids.
//!
//! The split between the two is exact and measured: their script finds an
//! element, checks it is visible, stable and enabled, and verifies a click would
//! land on it, then hands back. Filling a text field returns `needsinput` with
//! the field selected and nothing typed, and the protocol types it. There is no
//! `click` anywhere in their bundle.

// Where the JavaScript this browser runs comes from: compiled in by default,
// readable from a directory for anybody iterating on it.
pub mod assets;
pub mod binary;
pub mod cdp;
pub mod inject;
// Clicking and typing, which Playwright's script deliberately does not do:
// theirs decides WHAT and WHETHER, the protocol does the doing.
pub mod input;
// What a key name means and what a chord does, from Playwright's own two
// tables: the US keyboard layout and the macOS editing commands.
pub mod keys;
// What a page did while nobody was asking: its console, its requests, and
// whether it is waiting on a dialog. Three of Playwright's tools report things
// that have already happened, so a page records as it goes.
pub mod record;
// A browser that is running, a connection to it, and a page per agent. What the
// tools reach for; everything below it is machinery.
pub mod live;
// One page in the browser: a target plus the session that reaches it. Opening,
// navigating and asking the injected script about it. Phase 2's gate needs a
// real page to query, and this is what opens one.
pub mod page;
// Signed-in sessions kept on disk, so closing the application does not undo a
// login. Only for sites somebody actually signed into, and the assistant never
// sees a cookie.
pub mod session;
pub mod supervise;

use std::path::{Path, PathBuf};

/// Where the browser is published, unless this build is told otherwise.
///
/// Overridable by the environment for one reason: a developer pointing at a
/// mirror of their own, and the gate pointing at a server it made up. It is not
/// a setting anybody is meant to find.
const REPOSITORY: &str = "https://sag-repo.flexie.io";

// There is NO delay before fetching, and there was one. This is its grave.
//
// It waited twenty seconds, on the reasoning that the first seconds after a
// launch belong to the gateway starting and a window waiting to paint, and that
// a hundred megabyte download competing with them makes for a slower start.
// That was borrowed from the update check, which waits ninety, and not one part
// of it was measured.
//
// Measured, it is wrong. Fetching the archive is 28.95 seconds of wall clock
// and 4.0 seconds of processor (1.69 user, 2.31 system): it is a network wait,
// not work. Unpacking two hundred megabytes is 2.04 seconds. Six seconds of one
// core spread over half a minute, on a machine with at least eight, is not
// something a window paint can notice.
//
// So it bought nothing, and it cost twenty seconds on the one run where the
// browser is most likely to be reached for: the first one, while somebody is
// still finding out what the product does.

/// Where the browser keeps its cookies and its cache.
///
/// Beside the browser rather than inside it: the program lives under its
/// version and a new version replaces that directory, which would throw away
/// every session somebody had signed into. The profile outlives the browser
/// that reads it.
pub fn profile(state: &Path) -> PathBuf {
    state.join("browser-profile")
}

/// Have the browser HERE, without anybody noticing. Not running.
///
/// Called once when the application starts. If the browser is not on this
/// computer it is fetched, quietly, in the background. It is deliberately NOT
/// started: the first browser tool call starts it, and every path to a tab
/// already goes through `live::connection`, which does that.
///
/// The split is measured on both sides. Starting one costs 83 MB and three
/// processes, and most sessions never ask the assistant to open a web page, so
/// starting it at launch spends that on everybody to save 421 milliseconds for
/// the few who need it. Four hundred milliseconds on the first call of a
/// conversation is nothing; eighty megabytes for a feature nobody used is not.
///
/// The FETCH stays eager, and that is the other half of the same measurement:
/// it is a hundred and eighty megabytes, so a caller that had to wait for it
/// would wait minutes rather than milliseconds. Downloading in the background
/// and starting on demand is the only arrangement where neither cost lands on
/// somebody.
///
/// **A failure is logged and nothing else.** Nobody asked for this and nobody
/// is waiting for it, so a repository that cannot be reached must not produce a
/// dialog, a banner, or a retry loop. It is tried again the next time the
/// application opens, which is the cheapest possible recovery and needs no
/// state anywhere: the browser is either in its folder or it is not.
///
/// There is deliberately **no retry within a run**. A download that failed
/// because the network is down will fail again in thirty seconds, and a loop
/// that keeps discovering this is a laptop with its fan on. The next launch is
/// soon enough.
pub fn bring_up(state: PathBuf) {
    tauri::async_runtime::spawn(async move {
        if let Err(why) = arrive(&state).await {
            eprintln!("browser: {why}");
        }
    });
}

/// The work `bring_up` does, as something that can be awaited and therefore
/// tested.
///
/// The spawn above is three lines on purpose. A behaviour that only exists
/// inside a detached task is a behaviour no test can see, and the three things
/// worth pinning here are that a browser already on this computer is not
/// fetched again, that a fetch which fails stops rather than looping, and that
/// nothing is STARTED by it.
async fn arrive(state: &Path) -> Result<(), String> {
    // Looked for, and fetched at once if it is not there. See the note above
    // AFTER_START's grave: the wait this used to do was measured and found to
    // be protecting nothing.
    if binary::installed(state).is_none() {
        let base =
            std::env::var("SAG_BROWSER_REPOSITORY").unwrap_or_else(|_| REPOSITORY.to_string());
        eprintln!("browser: not here yet; fetching {}", binary::version());
        // The one place this gives up, and it gives up for the whole run. Said
        // plainly, because a browser that never arrived is a set of abilities
        // the assistant will report as unavailable, and somebody supporting
        // that needs a line to find.
        binary::fetch(state, &base)
            .await
            .map_err(|why| format!("not fetched, and will be tried again next time: {why}"))?;
        eprintln!("browser: fetched");
    }

    // Checked, not started. A fetch that lands somewhere unexpected is worth
    // saying now rather than at the first tool call, which is the one thing
    // this still does beyond fetching.
    binary::installed(state)
        .ok_or_else(|| "fetched, but its program is not where it should be".to_string())?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// `arrive` fetches and does not start.
    ///
    /// The property worth pinning, because it is invisible: a browser started
    /// at launch works perfectly and costs 83 MB on every session that never
    /// opens a web page. Nothing would ever fail to tell us it had come back.
    ///
    /// Asserted by READING, which is the honest thing a unit test can do here:
    /// starting a browser needs one on the machine, which is the live gate's
    /// job (`browser_live.rs`). What this catches is somebody putting the
    /// `ensure` call back.
    #[test]
    fn nothing_is_started_by_arriving() {
        let source = include_str!("mod.rs");
        let body = source
            .split("async fn arrive(")
            .nth(1)
            .expect("arrive is in this file");
        let body = body.split("\n}").next().unwrap_or_default();
        assert!(
            !body.contains("supervise::ensure"),
            "arrive starts the browser again. It fetches; the first tool call starts it, \
             through live::connection. See the note on bring_up for the measurement."
        );
        assert!(
            body.contains("binary::fetch"),
            "arrive no longer fetches, so the first tool call would wait for the download"
        );
    }

    /// The profile lives BESIDE the browser, not inside its version.
    ///
    /// The program lives under its version so that publishing a new pin makes a
    /// new directory, which is what makes staleness impossible. The cookies
    /// must not: a profile under the version would be thrown away by every
    /// browser update, signing somebody out of everything they had signed into
    /// for no reason they could see.
    #[test]
    fn a_browser_update_does_not_throw_away_the_profile() {
        let state = Path::new("/tmp/state");
        let kept = profile(state);
        assert!(
            !kept.starts_with(binary::home(state)),
            "the profile is inside the versioned folder ({}), so an update would delete it",
            kept.display()
        );
        assert!(
            kept.starts_with(state),
            "the profile escaped the state directory"
        );
    }
}
