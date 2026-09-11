//! What both desktop applications share.
//!
//! Deliberately small. The two shells have different jobs (one supervises a
//! gateway, the other points at somebody else's), and the temptation with a
//! shared crate is to put the differences in it behind a flag. What belongs
//! here is what is the same because of the WINDOW, not because of the product:
//! how an outside address is opened, and how a shell that cannot start says so.

pub mod appearance;
pub mod device;
pub mod link;
pub mod menu;
pub mod update;
pub mod screens;
pub mod tools;
pub mod workspace;

use std::sync::Mutex;

use tauri::{Url, WebviewWindow};

/// The last thing said about the gateway starting, so it is not said again.
static SAID: Mutex<Option<String>> = Mutex::new(None);

/// What a page means when it asks for a second window.
///
/// It means "send this person somewhere else", and there is nowhere else in
/// either application: each is one window onto one gateway. So the address goes
/// to the browser the person already uses, and the window they asked for is
/// refused.
///
/// Without this the request is not refused, it is IGNORED. The webview only
/// creates a window when it has been told how, so a link with a target and
/// every `window.open` did nothing whatsoever, said nothing, and left no trace.
/// That is how a Connect button can register a client on a remote server and
/// still look, from the chair, like a button that is not wired up (KB/29).
///
/// It is also where a sign-in belongs. A page shown inside an application's own
/// webview has no address bar, so nobody can see whose page they are typing a
/// password into; the standard for native applications says to use the system
/// browser for exactly that reason (RFC 8252), and several large providers
/// refuse to render their sign-in anywhere else. The person's own browser is
/// also where they are already signed in.
pub fn open_outside(url: &Url) {
    if !opens_outside(url.scheme()) {
        return;
    }
    // Detached: the browser outlives this call, and waiting on it would hold
    // the thread that runs the interface.
    let _ = open::that_detached(url.as_str());
}

/// Whether an address is one to hand to the operating system.
///
/// Only the two a page can mean by "open this". Everything else is either not a
/// destination at all (`about:blank`) or something that would be asking the
/// system to run a file chosen by a page, which is not a thing a page gets to
/// decide.
pub fn opens_outside(scheme: &str) -> bool {
    matches!(scheme, "http" | "https")
}

/// Replaces the waiting page with what went wrong.
///
/// A shell that cannot reach its gateway has to say so in the window. Exiting
/// silently is the failure people report as "I clicked it and nothing
/// happened", and it is the one thing a launcher must never do.
pub fn show_failure(window: &WebviewWindow, message: &str) {
    let _ = window.eval(format!(
        r#"(function(){{
             var el = document.getElementById('status');
             if (el) {{ el.textContent = `{}`; el.className = 'failed'; }}
           }})()"#,
        escape(message)
    ));
}

/// Says how far along the work on the waiting page is.
///
/// The number only. What the page shows is "Loading" and a percentage, the same
/// in both editions, and it decides where the bar actually sits: it holds this
/// number under a floor of its own so a gateway ready at once still reads as an
/// application starting, and it never reaches a hundred until the chat is
/// opening. Here we only report the truth.
///
/// The message is not shown. It was, and it said "Ready" while the chat was
/// still loading, which is the one thing a waiting screen must not claim. It is
/// still taken, and written where somebody supporting a slow first run can read
/// it: half a minute of migrations should leave a trace somewhere.
pub fn report_progress(window: &WebviewWindow, percent: u8, message: &str) {
    // Written once per thing that happens, not once per poll: this is called
    // three times a second while a gateway starts, and a first run is half a
    // minute of it.
    if let Ok(mut last) = SAID.lock() {
        if last.as_deref() != Some(message) {
            eprintln!("gateway: {percent}% {message}");
            *last = Some(message.to_string());
        }
    }
    let _ = window.eval(format!(
        "if (window.__sagProgress) window.__sagProgress({percent})"
    ));
}

/// Makes a message safe to put inside a JavaScript template literal.
///
/// The messages carry paths and errors from the operating system, which is to
/// say text nobody here wrote. A backslash or a backtick in one of those would
/// otherwise end the literal and take the rest of the script with it.
fn escape(message: &str) -> String {
    message
        .replace('\\', "\\\\")
        .replace('`', "\\`")
        .replace("${", "\\${")
}

#[cfg(test)]
mod tests {
    use super::{escape, opens_outside};

    // The filter is the whole security boundary of handing an address to the
    // operating system, so it is stated as a test rather than as care.
    #[test]
    fn only_a_web_address_leaves_the_application() {
        assert!(opens_outside("http"));
        assert!(opens_outside("https"));
        for scheme in [
            "about",
            "file",
            "javascript",
            "data",
            "tauri",
            "ftp",
            "mailto",
        ] {
            assert!(!opens_outside(scheme), "{scheme} was handed to the system");
        }
    }

    // These messages hold paths and errors from outside, and they are pasted
    // into a script. A backtick that ended the literal early would break the
    // page that exists to explain why nothing started.
    #[test]
    fn a_message_cannot_escape_the_script_it_is_put_in() {
        assert_eq!(escape(r"C:\Users"), r"C:\\Users");
        assert_eq!(escape("a `backtick`"), "a \\`backtick\\`");
        assert_eq!(escape("${alert(1)}"), "\\${alert(1)}");
    }
}
