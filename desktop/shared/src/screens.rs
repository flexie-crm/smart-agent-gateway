//! Which window is on the screen, and the exact moment it goes there.
//!
//! Both applications open on something that is not the chat: one waits for a
//! gateway it is starting, the other for a server across a network. Both then
//! replace it. How that is done is the same in both, it is not obvious, and
//! getting it wrong is the white screen people report. So it is written once.
//!
//! There are two rules and they are both load-bearing.
//!
//! **The window is the right colour from the first frame it exists, and stays
//! that colour until the page inside it has painted.** Nothing fades, nothing is
//! timed, and there is no moment when the ground is a different colour: the
//! window carries the colour, and the WEBVIEW is what is transparent until it
//! has something to show.
//!
//! Every part of that is counter-intuitive, so all of it was measured frame by
//! frame off a screen recording rather than reasoned about:
//!
//! - A window that is never hidden shows **517ms** of white.
//! - One hidden, and shown when its page has LOADED, shows **217ms**.
//! - One hidden for three seconds shows none.
//!
//! So the webview needs real time to composite, and it only composites while its
//! window is on the screen: hiding a window to avoid the white is exactly what
//! prevents the paint that would end it. There is no asking, either. A page that
//! is not being drawn cannot report having been drawn, and the browser's own
//! first-paint entry is never recorded (that was tried; the window never
//! reported anything and only the backstop below ever moved it).
//!
//! Moving it off the display was tried next, and taught the last piece: macOS
//! renders the tiles that are ON the screen. Clamped to a forty pixel sliver at
//! the edge, that sliver painted, the page reported its first paint quite
//! truthfully, and the other eleven hundred pixels arrived white when it moved
//! in: **100ms**, down from 517, and still a white window.
//!
//! Hiding the WINDOW was the mistake in all of that. What has to be invisible is
//! the webview, and only until it has drawn: a transparent view is still laid
//! out, still rendered and still composited (unlike a hidden one), so the page
//! paints, reports it, and is revealed over a ground that has been the same
//! colour the whole time. `alphaValue` is ordinary AppKit on both, not the
//! private call this is all written to avoid.
//!
//! The second window is the one exception, and for a reason that is not about
//! white: the chat must not cover the waiting screen while it loads. So its
//! window is transparent too, and both go opaque together, over the same ground.
//!
//! Why none of this is simply a colour: a webview paints opaque white from the
//! moment it is on the screen until its document paints, and no colour we can
//! set reaches that frame. The window's own background is BEHIND the webview,
//! and the one public call that colours the webview itself
//! (`setUnderPageBackgroundColor`) governs the overscroll area. WebKit stops
//! drawing that white when `drawsBackground` is turned off, which is a private
//! API: wry gates it behind its `transparent` feature, which is what Tauri's
//! `macOSPrivateApi` turns on, and an application in the App Store must not ship
//! it. So the white frame is not coloured. It is never shown.
//!
//! **The page says when that is, and nothing else is trusted to.** A page having
//! LOADED is not a page having been drawn: `PageLoadEvent::Finished` is the
//! navigation finishing, and showing a window then, or a guessed number of
//! milliseconds after then, is showing it before the first frame exists. What is
//! reported here is one composited frame, by the only half that can see one.
//!
//! It is reported by INVOKING a command rather than by emitting an event, and
//! that is not a preference. A page reaches this half of the application through
//! one door, and a command named in a permission set is the door the served chat
//! already comes through (the machine link, the remembered appearance). An event
//! looked equivalent and was not: nothing arrived, nothing was refused, and both
//! halves looked innocent, which is the exact failure the permission files here
//! were written to warn about.
//!
//! **A navigation inside a window needs none of this, and hiding the webview
//! across one is a mistake that was made and undone.** WebKit keeps the page it
//! has on the screen until the next one has something to paint, so moving
//! between the chat and the console does not blank anything. Making the webview
//! transparent for the crossing REPLACED a page that was already there with the
//! window's bare ground, which reads as two windows swapping each other. The
//! white people reported on that crossing was never a blank: it was the page
//! rendering in the wrong appearance for a moment, because the value the shell
//! injects is decided when the window is built and went stale the moment
//! somebody changed it (chat-ui/index.html).
//!
//! **And two windows at startup, for a reason that is not about white at all.**
//! The waiting screen has to stay on the screen WHILE the chat loads, and one
//! webview shows one document: sending it to the chat is giving up the screen
//! that says what is happening. So the waiting screen keeps its own window, the
//! chat loads in a second one nobody can see, and they swap when it is ready.

use std::sync::atomic::{AtomicBool, Ordering};
// Every lock here recovers from poisoning rather than expecting its way past it:
// this runs in a command path, where a panic would take the window with it, and
// the guarded values are a flag and two lists that any thread can read as they
// stand.
use std::sync::Mutex;
use std::time::{Duration, Instant};

use tauri::{AppHandle, Manager, Window};

/// The window somebody waits in front of: a form, or a message about starting.
pub const WAITING: &str = "main";

/// And the one the chat loads in, whoever is serving it.
pub const CHAT: &str = "chat";

/// The windows already moved into view, by label.
///
/// Kept rather than read back off the window, because macOS does not honour the
/// position it was given: it clamps a window to keep part of it reachable, so
/// where a window IS says nothing about whether it has been placed.
static IN_VIEW: Mutex<Vec<String>> = Mutex::new(Vec::new());

/// How long the chat waits for the bar to finish being drawn. Reaching this is
/// a page that did not answer, not a page that was slow: it is a frame or two of
/// work.
const AT_MOST: Duration = Duration::from_millis(400);

/// How long a window is given to report a frame before it is shown regardless.
///
/// A window that is never shown is worse than one shown a moment early: it is an
/// application that does not start. This is the backstop for a page that throws
/// before our script runs, or a subresource that never arrives, and reaching it
/// is a bug rather than a mode.
const AT_THE_LATEST: Duration = Duration::from_secs(3);

/// The least time a waiting screen that is up gets to be read. Set once, at
/// startup, because it differs between the two applications: the personal
/// edition's screen carries a real account of a gateway starting, and the
/// enterprise edition's says which server it is going to.
static STAYS: Mutex<Duration> = Mutex::new(Duration::from_millis(700));

/// When the waiting screen went up. None while it has not.
static WAITING_SINCE: Mutex<Option<Instant>> = Mutex::new(None);

/// Set by the waiting screen when it has DRAWN a full bar, which is what the
/// chat waits for before it takes the screen.
static BAR_IS_FULL: AtomicBool = AtomicBool::new(false);

/// Set the moment the chat is shown, and never unset. Two threads race for the
/// screen, and this is what they agree on: the waiting screen must not appear
/// after the chat has.
static CHAT_IS_UP: AtomicBool = AtomicBool::new(false);

/// Says how long a waiting screen stays up. Called once, before any window.
pub fn holds_the_waiting_screen_for(stays: Duration) {
    *STAYS
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner) = stays;
}

/// Whether the chat is the thing on the screen.
fn chat_is_up() -> bool {
    CHAT_IS_UP.load(Ordering::SeqCst)
}

/// Forgets that a chat was ever up, so a waiting screen may be shown again.
///
/// One case: somebody walks away from the server they were on, and the form that
/// asks for another has to be able to appear.
pub fn chat_is_gone() {
    CHAT_IS_UP.store(false, Ordering::SeqCst);
    *WAITING_SINCE
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner) = None;
    IN_VIEW
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .clear();
}

/// The script that reports a window's first composited frame.
///
/// Handed to every window this application builds, and it has to be an
/// initialization script rather than anything in a page: the chat is served by a
/// gateway and this half cannot edit it, and even the pages we do ship would
/// then each carry a copy of something that must not drift.
///
/// `load` and then two frames. `load` is every subresource in, so the stylesheet
/// that decides the ground has arrived; the first `requestAnimationFrame` is the
/// callback before the next frame is composited, and the second is after it. So
/// when this fires, one frame of this document exists on the compositor, which
/// is the thing a window must not be shown without.
///
/// Which window it is does not travel: the command is told by Tauri which
/// webview called it, so there is nothing here to keep in step with a label.
pub fn reports_when_painted() -> String {
    r#"(function () {
         // Whether this document has drawn any of its own content yet.
         //
         // The browser records that itself, at the moment it happens, under a
         // name it has always used for it. Asking is not a heuristic and not a
         // guess at how long a page ought to take: either the entry is there and
         // a frame with content in it exists, or it is not.
         //
         // Asked on a timer rather than on an animation frame, because a window
         // nobody can see is not composited and its animation frames are
         // SUSPENDED: waiting on one is waiting for a paint that cannot happen
         // until the window is shown, which is the thing being waited for.
         // Timers keep running, so this keeps asking, and the moment the answer
         // is yes the window can go up with nothing white in it.
         var look = function () {
           var drawn = performance.getEntriesByType('paint').some(function (e) {
             return e.name === 'first-contentful-paint'
           })
           if (!drawn) return setTimeout(look, 16)
           // Painted, but not yet ON the screen: the entry is recorded when the
           // paint is issued and the composited frame goes out after it, which
           // measured as two frames of white. Animation frames run here, because
           // this window IS on the screen and merely transparent, so waiting for
           // one waits for exactly that.
           requestAnimationFrame(function () {
             requestAnimationFrame(function () {
               window.__TAURI__.core.invoke('painted')
             })
           })
         }
         look()
       })()"#
        .to_string()
}

/// A window's page reporting its first drawn frame. This is what shows a window.
///
/// Registered by both applications, and named in a permission set, or the page
/// calling it is refused and the window is never shown by anything but the
/// backstop below.
#[tauri::command]
pub fn painted(window: tauri::WebviewWindow) {
    let app = window.app_handle().clone();
    let is_chat = window.label() == CHAT;
    appears(&app, window.as_ref().window().clone(), is_chat);
}

/// Shows a window whose page has reported a frame.
///
/// Returns at once. The chat waits for the waiting screen to have had its time,
/// which is a wait, and the thread that carries the interface must not do it.
fn appears(app: &AppHandle, window: Window, is_chat: bool) {
    if already_in_view(window.label()) {
        return;
    }
    let app = app.clone();
    let stays = *STAYS
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    std::thread::spawn(move || {
        if is_chat {
            let up = *WAITING_SINCE
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            if let Some(since) = up {
                let shown = since.elapsed();
                if shown < stays {
                    std::thread::sleep(stays - shown);
                }
            }
            // The bar reaching the end and the chat arriving are one event, and
            // in that order. The waiting screen holds itself short of the end
            // until told, so that nobody is left in front of a full bar; told
            // now, it draws the last of it and says when that frame is on the
            // screen. Waited for rather than timed, and bounded, because a
            // screen with no bar on it (the form somebody just pressed Connect
            // on) answers at once and must not cost anything.
            if let Some(waiting) = app.get_webview_window(WAITING) {
                BAR_IS_FULL.store(false, Ordering::SeqCst);
                let _ = waiting.eval(
                    "if (window.__sagDone) window.__sagDone(); \
                     else window.__TAURI__.core.invoke('waiting_finished')",
                );
                let until = Instant::now() + AT_MOST;
                while !BAR_IS_FULL.load(Ordering::SeqCst) && Instant::now() < until {
                    std::thread::sleep(Duration::from_millis(8));
                }
            }
            CHAT_IS_UP.store(true, Ordering::SeqCst);
            into_view(&window);
            // The screen that was waiting for it has nothing left to say.
            if let Some(waiting) = app.get_webview_window(WAITING) {
                let _ = waiting.close();
            }
            return;
        }
        let mut since = WAITING_SINCE
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        // Under the lock, because the chat may have arrived while this was being
        // decided, and a waiting screen shown after it is never taken away.
        if chat_is_up() {
            return;
        }
        into_view(&window);
        *since = Some(Instant::now());
    });
}

/// Whether a window has been moved to where it can be seen, and a note that it
/// has. Asked and answered once, because two threads race for the screen here.
fn already_in_view(label: &str) -> bool {
    let mut placed = IN_VIEW
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    if placed.iter().any(|seen| seen == label) {
        return true;
    }
    placed.push(label.to_string());
    false
}

/// Puts a window on the screen with its own colour showing and nothing else.
///
/// Called for every window as it is built, and it is the reason a window is
/// built with `visible(false)`: not to keep it hidden, but so that the webview
/// inside it is already transparent by the time anybody sees the window. A
/// window shown first and made transparent afterwards is a window that shows one
/// white frame, which is the whole of what this file is about.
pub fn draws_unseen(window: &tauri::WebviewWindow) {
    let frame = window.as_ref().window().clone();
    let waiting = window.label() != CHAT;
    let _ = window.with_webview(move |webview| {
        // The page has nothing to show yet, so it shows nothing, and what is
        // seen is the window's own colour underneath it.
        view_alpha(webview.inner(), 0.0);
        // The waiting screen's window IS the application appearing, so it does.
        // The chat's would cover the waiting screen while it loaded, so it does
        // not: on the screen, drawing, and wholly transparent.
        window_alpha(webview.ns_window(), if waiting { 1.0 } else { 0.0 });
        // Shown here and not before. This runs on the thread that owns the
        // interface, some moments after it was asked for, and a window shown
        // ahead of it is a window that shows one white frame.
        let _ = frame.show();
    });
}

/// And reveals one, once its page has something to show.
fn into_view(window: &Window) {
    if let Some(webview_window) = window.app_handle().get_webview_window(window.label()) {
        let _ = webview_window.with_webview(|webview| {
            window_alpha(webview.ns_window(), 1.0);
            view_alpha(webview.inner(), 1.0);
        });
    }
    let _ = window.set_focus();
}

/// How much of a thing is drawn: none of it, or all of it.
///
/// One AppKit property, on a window or on a view, both of which have it. Public
/// API, deliberately: the private one that would do this job by other means is
/// exactly what this arrangement exists to avoid shipping.
///
/// Two functions rather than one, and typed, because this was `msg_send!` with
/// the selector written out as a string and the receiver as an untyped pointer.
/// Nothing checked either: the same function was handed an NSWindow at one call
/// and a WKWebView at the next, `setAlphaValue` was spelled correctly by hand,
/// and its signature was asserted rather than known. A typo compiles and is
/// undefined behaviour when it runs. Through objc2-app-kit the method and its
/// argument are the compiler's business, and what is left unsafe is the one
/// thing that genuinely cannot be checked: Tauri hands out a raw pointer, and
/// somebody has to say what is on the end of it.
#[cfg(target_os = "macos")]
fn window_alpha(handle: *mut std::ffi::c_void, alpha: f64) {
    if handle.is_null() {
        return;
    }
    // SAFETY: the pointer is the window's own NSWindow, from Tauri's
    // `ns_window()`, and it lives as long as the window does.
    let window: &objc2_app_kit::NSWindow = unsafe { &*handle.cast() };
    window.setAlphaValue(alpha);
}

/// The same, for the view the page is drawn into.
#[cfg(target_os = "macos")]
fn view_alpha(handle: *mut std::ffi::c_void, alpha: f64) {
    if handle.is_null() {
        return;
    }
    // SAFETY: the pointer is the window's own WKWebView, from Tauri's
    // `inner()`. A WKWebView is an NSView, which is where alphaValue lives.
    let view: &objc2_app_kit::NSView = unsafe { &*handle.cast() };
    view.setAlphaValue(alpha);
}

/// The waiting screen saying it has drawn a full bar. See the chat's wait above.
#[tauri::command]
pub fn waiting_finished() {
    BAR_IS_FULL.store(true, Ordering::SeqCst);
}

/// The colour behind everything, and what shows whenever a page has not drawn.
///
/// Not guessed. It is what the person chose, read from the same settings file
/// the page writes, and when they have chosen to follow the computer, from the
/// computer. Assuming night was wrong in every lit room and assuming light was
/// wrong in every dark one.
///
/// The two values are the pages' own grounds (#1c1e22 and #fafbfc, in
/// chat-ui/src/index.css) and have to be changed in both places: a colour in a
/// stylesheet cannot reach a window frame.
pub fn ground() -> tauri::window::Color {
    match crate::appearance::wanted() {
        crate::appearance::Wanted::Light => tauri::window::Color(250, 251, 252, 255),
        crate::appearance::Wanted::Dark => tauri::window::Color(28, 30, 34, 255),
    }
}

/// Repaints every window's ground, because somebody changed which one they want.
///
/// It is not decoration. This colour is what is on the screen for as long as a
/// page has nothing to show: at startup, and across every navigation. Left at
/// what it was when the window was built, a person who switched to night would
/// cross from the chat to the console through a flash of the daylight ground.
pub fn the_ground_changed(app: &AppHandle) {
    let now = ground();
    for window in app.webview_windows().values() {
        let _ = window.set_background_color(Some(now));
    }
}

/// The backstop: moves in a window that never reported a frame.
///
/// Wired to the page-load event, which fires when the navigation finished rather
/// than when anything was drawn. That is too early to place a window and exactly
/// right to start counting from.
pub fn or_shortly_after_loading(app: &AppHandle, window: Window) {
    let app = app.clone();
    std::thread::spawn(move || {
        std::thread::sleep(AT_THE_LATEST);
        let label = window.label().to_string();
        if already_in_view(&label) {
            return;
        }
        IN_VIEW
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .retain(|seen| seen != &label);
        eprintln!("screens: {label} never reported a frame; moving it in anyway");
        appears(&app, window, label == CHAT);
    });
}
