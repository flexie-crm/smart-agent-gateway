// SAG Enterprise (KB/36).
//
// The chat, against a server somebody else runs. It starts nothing: no gateway,
// no database, no inference node, and it does not carry the console, which
// lives on the web where an administrator already works.
//
// Then why is it an application at all, rather than the page it points at? So
// that the assistant can reach the computer it is installed on. A web page
// cannot read a file on your disk or run a command for you; this half is Rust,
// on the machine, and that is where those tools belong. It carries the link the
// gateway reaches this computer through, the terminal it runs commands in, and
// the one folder a person chooses for it to work in.
//
// The session is the server's, held in the webview as cookies exactly as a
// browser holds them. There is no second scheme, no token custody in Rust and
// nothing extra to keep secure: signing in here is signing in, and it is the
// same session the console has on the web.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod server;

use std::sync::Arc;

use sag_desktop::link::Link;
use sag_desktop::screens;
use sag_desktop::{open_outside, show_failure};
use tauri::webview::{NewWindowResponse, WebviewWindowBuilder};
use tauri::{Emitter, Manager, WebviewUrl, WebviewWindow};

/// Where a server's chat lives, appended to the address somebody gave us.
const OPENS_AT: &str = "/chat/";

/// What the served page is allowed to ask this half of the application to do.
///
/// A page loaded from a server is REMOTE, and Tauri gives a remote page none of
/// an application's own commands unless a capability names them. Without one
/// the page calls, gets "not allowed", and neither half can say so: the link
/// simply never starts.
///
/// Granted at run time, for THIS installation's own server and nothing else.
/// A capability in a file would have to be a wildcard, because the address is
/// typed by the person on first run and there is no list to write, and a
/// wildcard would mean any page the window ever loaded could start the link.
/// The address is known here, so the grant can name it.
///
/// What it grants is named one command at a time (permissions/link.toml): hand
/// down a credential, stop, ask whether the route is up, say which installation
/// this is, and read or change the folder the assistant may work in. None of
/// them reads a file or runs anything by itself; what does that is a tool the
/// gateway calls, over the link, after its own rules have been applied.
fn allow_the_server_page(app: &tauri::AppHandle, address: &str) {
    let capability = format!(
        r#"{{
          "identifier": "the-server-page",
          "description": "The chat, served by the gateway this installation is pointed at.",
          "windows": ["main", "chat"],
          "remote": {{ "urls": ["{address}/*"] }},
          "permissions": ["core:default", "core:event:default", "allow-machine", "allow-screens", "store:default"]
        }}"#
    );
    if let Err(err) = app.add_capability(capability) {
        // Worth a line rather than a silence: everything else will work, and
        // the one thing that will not is a route into this computer's network,
        // which is exactly the failure that is hard to see from the outside.
        eprintln!("link: the served page was not granted access to this computer: {err}");
    }
}

/// The page shown when there is no server yet, and after one is forgotten.
const ASKS_FOR_A_SERVER: &str = "index.html";

/// Told to the waiting screen when the chat has not arrived, with the address.
const CANNOT_REACH: &str = "server-unreachable";

/// How long the waiting screen stays before the chat replaces it.
///
/// It is always shown, even against a server that answers at once, because it is
/// what says the application has started and which server it is going to. What
/// this stops is the other thing: a screen appearing and being taken away inside
/// a fifth of a second, which reads as a glitch rather than as progress.
const WAITING_STAYS: std::time::Duration = std::time::Duration::from_millis(1500);

/// How long a server may take before we stop calling it slow.
///
/// Not a timeout: nothing is cancelled and the chat window is still loading, so
/// a server that answers in the fortieth second still swaps in. It is the point
/// at which the waiting screen stops counting and offers the way out, because
/// an address that answers nothing leaves a webview showing ITS error page,
/// which has no Change server button on it and no way back to one.
const LONG_ENOUGH: std::time::Duration = std::time::Duration::from_secs(25);

fn main() {
    tauri::Builder::default()
        // Two windows, and this is the whole of how either one appears.
        //
        // NOTHING is shown before the page inside it has finished loading. A
        // webview paints opaque white from the moment it is on the screen until
        // its document paints, and no colour we can set reaches that frame: the
        // window's own background is behind the webview, and the one public call
        // that colours the webview (setUnderPageBackgroundColor) governs only
        // the overscroll area. WebKit stops drawing that white when its
        // `drawsBackground` is turned off, which is a private API, which is
        // exactly what this application must not ship.
        //
        // So the white frame is not coloured, it is never on the screen. Each
        // window is built hidden and shown here, when what is in it is ready.
        //
        // Two windows and not one, because one cannot do both jobs: showing the
        // waiting screen means showing its webview, and that same webview then
        // goes white again while it navigates to the server. The waiting screen
        // is its own window; the chat loads unseen in a second; they swap.
        // The backstop only. What actually shows a window is the page in it
        // reporting a painted frame; this fires when the navigation finished,
        // which is too early to show anything and exactly right to start
        // counting from. Both rules are in sag_desktop::screens.
        .on_page_load(|webview, payload| {
            if !matches!(payload.event(), tauri::webview::PageLoadEvent::Finished) {
                return;
            }
            screens::or_shortly_after_loading(&webview.app_handle().clone(), webview.window());
        })
        .invoke_handler(tauri::generate_handler![
            connect,
            forget_server,
            link_start,
            link_stop,
            link_up,
            link_device,
            sag_desktop::screens::painted,
            sag_desktop::screens::waiting_finished,
            sag_desktop::appearance::remember_appearance,
            sag_desktop::appearance::chosen_appearance,
            sag_desktop::menu::show_context_menu,
            sag_desktop::update::check_for_update,
            sag_desktop::environment::machine_environment,
            chosen_folder,
            choose_folder,
            forget_folder
        ])
        .on_menu_event(sag_desktop::menu::handle)
        .plugin(tauri_plugin_updater::Builder::new().build())
        .plugin(tauri_plugin_store::Builder::default().build())
        .plugin(tauri_plugin_dialog::init())
        .setup(|app| {
            // What kind of computer this is, worked out now rather than when
            // the first message is sent: it runs the person's shell once, and
            // that is a thing to do while a window is opening, not while
            // somebody is waiting for an answer.
            sag_desktop::environment::warm();
            // Before any window is built: what shows one is its page saying it
            // has drawn a frame, and this is how long the first of them stays.
            screens::holds_the_waiting_screen_for(WAITING_STAYS);
            // The next version, fetched quietly and announced once. The same
            // mechanism the personal edition uses, on its own channel and its
            // own signing key: an enterprise installation is on somebody else's
            // machine, and a release cadence of never is not a choice we get to
            // make on their behalf.
            sag_desktop::update::watch(app.handle().clone());
            // Where this installation keeps its files, told once to the half
            // that remembers the chosen folder.
            if let Ok(dir) = server::state_dir() {
                sag_desktop::workspace::use_state_dir(dir);
            }
            // And where the SETTINGS file is, which is somewhere else: the store
            // plugin writes under the identifier, everything else here is under
            // the product name. Said before the window is built, because the
            // window's colour is read from it.
            if let Ok(dir) = app.path().app_config_dir() {
                sag_desktop::appearance::use_settings_dir(dir);
            }

            // The machine link, idle until the page signs in and hands it a
            // credential. It is why this is an application rather than a page:
            // a route from the gateway to what only this computer can see.
            let asking = app.handle().clone();
            let announcing = app.handle().clone();
            app.manage(Link::new(
                move || {
                    // The page is the only half that can sign in, so when the
                    // link needs a credential it asks the window for one.
                    let _ = asking.emit(sag_desktop::link::NEEDS_TOKEN, ());
                },
                move |up| {
                    // And when the route comes up or goes down, the window is
                    // told, so a person can see whether their assistant can
                    // reach their own network.
                    let _ = announcing.emit(sag_desktop::link::STATE_CHANGED, up);
                },
            ));
            // The first screen is opened knowing two things it cannot find out
            // for itself: which appearance this person chose (the chat's own
            // storage is a different origin) and whether this window is about
            // to leave for a server it already has. Both change what it draws,
            // and both have to be known BEFORE it draws.
            let opening = format!(
                "{ASKS_FOR_A_SERVER}?appearance={}{}",
                sag_desktop::appearance::chosen(),
                if server::saved().is_some() {
                    "&going=1"
                } else {
                    ""
                }
            );
            // The window somebody sees first: the form that asks for a server,
            // or, when there is one saved, the screen that says it is on its way.
            // Hidden until its page has painted, like every window here.
            a_window(
                &app.handle().clone(),
                screens::WAITING,
                WebviewUrl::App(opening.into()),
            )?;

            // And the chat behind it, when this installation already has a
            // server. It is not probed first: a server that is briefly down
            // should not send somebody back to a form to retype an address that
            // was right. If it never answers, what stays on the screen is the
            // waiting window saying so, which is why that window is the one
            // shown and this one is not.
            if let Some(address) = server::saved() {
                open_the_chat(&app.handle().clone(), &address);
            }

            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("the shell could not start");
}

/// The appearance, handed to every page a window loads, BEFORE any of them
/// parses a line.
///
/// An initialization script runs after the global object exists and before the
/// document is parsed, which is the one moment early enough to decide what
/// colour to draw. Working it out after the page has started is what made the
/// chat render light and then turn dark.
fn appearance_script() -> String {
    format!(
        "window.__SAG_APPEARANCE__ = {};",
        serde_json::to_string(&sag_desktop::appearance::chosen())
            .unwrap_or_else(|_| "\"system\"".into())
    )
}

/// A window of this application: the same one twice, differing only in what it
/// loads. Written once because three places need it, and because a window built
/// without `visible(false)` is a white frame nobody meant to ship.
fn a_window(
    app: &tauri::AppHandle,
    label: &str,
    url: WebviewUrl,
) -> tauri::Result<tauri::WebviewWindow> {
    let window = WebviewWindowBuilder::new(app, label, url)
        .title("SAG Enterprise")
        .inner_size(1180.0, 820.0)
        .min_inner_size(720.0, 560.0)
        // Built unseen, and shown by draws_unseen once the webview inside it is
        // transparent. The window's own colour is then the only thing on the
        // screen until the page has drawn, so the ground never changes.
        .visible(false)
        .background_color(screens::ground())
        .initialization_script(format!(
            "{}{}",
            appearance_script(),
            screens::reports_when_painted()
        ))
        .on_new_window(|url, _features| {
            open_outside(&url);
            NewWindowResponse::Deny
        })
        .build()?;
    // On the screen at full size and drawing itself, with none of it visible:
    // the one arrangement in which a window paints before anybody sees it.
    screens::draws_unseen(&window);
    Ok(window)
}

/// Opens the chat on a server, in a window of its own that nobody sees yet.
///
/// A window rather than a navigation, and this is the point of the whole
/// arrangement: navigating the window somebody is looking at means watching it
/// go white on the way. This one is built hidden, and the page-load hook swaps
/// it for the waiting window once there is something in it to look at.
fn open_the_chat(app: &tauri::AppHandle, address: &str) {
    allow_the_server_page(app, address);
    let waiting = app
        .get_webview_window(screens::WAITING)
        .expect("the first window is built before anything asks for a chat");
    let url = match format!("{address}{OPENS_AT}").parse() {
        Ok(url) => url,
        Err(err) => return show_failure(&waiting, &format!("A bad address was built: {err}")),
    };
    let built = a_window(app, screens::CHAT, WebviewUrl::External(url));
    if let Err(err) = built {
        return show_failure(&waiting, &format!("This window could not be opened: {err}"));
    }
    // And, if it never arrives, somebody has to be told rather than left with a
    // bar that has stopped.
    let telling = app.clone();
    let said = address.to_string();
    std::thread::spawn(move || {
        std::thread::sleep(LONG_ENOUGH);
        let arrived = telling
            .get_webview_window(screens::CHAT)
            .and_then(|w| w.is_visible().ok())
            .unwrap_or(true);
        if !arrived {
            let _ = telling.emit_to(screens::WAITING, CANNOT_REACH, said);
        }
    });
}

/// Takes the address somebody typed, and only accepts one that works.
///
/// Read it, then REACH it, then save it, then go. In that order, because an
/// address accepted and saved without being reached is a window that navigates
/// to a browser error with no way back to the field. Approval must equal
/// success, and here the approval is somebody pressing Connect.
#[tauri::command]
async fn connect(window: WebviewWindow, address: String) -> Result<(), String> {
    let address = server::normalise(&address)?;
    // The probe blocks, and the interface must not. This runs on a worker
    // thread and the window keeps painting the "Connecting" state on the
    // button, which is the only reason a ten second timeout is tolerable.
    let checked = address.clone();
    tauri::async_runtime::spawn_blocking(move || server::reachable(&checked))
        .await
        .map_err(|_| "The check could not be completed.".to_string())??;
    server::save(&address)?;
    // The form stays up, saying nothing new, until the chat is ready to replace
    // it. open_the_chat grants the page its access before building the window,
    // because the page may ask for the link as soon as it loads and a grant
    // that arrives second arrives too late.
    open_the_chat(&window.app_handle().clone(), &address);
    Ok(())
}

/// Forgets the server and asks for another.
///
/// The way back out, which the window would otherwise not have: once it is on
/// somebody's server, every pixel belongs to that server and a wrong address
/// would be unrecoverable without deleting a file by hand.
#[tauri::command]
fn forget_server(window: WebviewWindow) -> Result<(), String> {
    server::forget()?;
    // A waiting screen is allowed to be the thing on the screen again.
    screens::chat_is_gone();
    let app = window.app_handle().clone();
    // The form goes back into its own window, hidden until it has painted. The
    // window the chat was in is not navigated there, for the reason the two
    // windows exist at all: navigating what somebody is looking at turns it
    // white on the way.
    match app.get_webview_window(screens::WAITING) {
        Some(asking) => asking
            .navigate(
                format!("tauri://localhost/{ASKS_FOR_A_SERVER}")
                    .parse()
                    .map_err(|e| format!("{e}"))?,
            )
            .map_err(|e| format!("{e}"))?,
        None => {
            a_window(
                &app,
                screens::WAITING,
                WebviewUrl::App(ASKS_FOR_A_SERVER.into()),
            )
            .map_err(|e| format!("{e}"))?;
        }
    }
    // And the chat's window goes, once the form is up: closing it here would
    // leave nothing on the screen for as long as the form takes to draw.
    let leaving = app.clone();
    std::thread::spawn(move || {
        let until = std::time::Instant::now() + LONG_ENOUGH;
        while std::time::Instant::now() < until {
            let up = leaving
                .get_webview_window(screens::WAITING)
                .and_then(|w| w.is_visible().ok())
                .unwrap_or(false);
            if up {
                break;
            }
            std::thread::sleep(std::time::Duration::from_millis(40));
        }
        if let Some(chat) = leaving.get_webview_window(screens::CHAT) {
            let _ = chat.close();
        }
    });
    Ok(())
}

/// Starts the machine link, or hands it a fresh credential.
///
/// Called by the chat page: once when somebody signs in, and again whenever the
/// link says it needs another token. Rust cannot sign in and does not try: the
/// page has the session, mints a token that opens one socket, and passes it
/// down. There is no second way in and nothing to keep in step.
#[tauri::command]
fn link_start(link: tauri::State<'_, Arc<Link>>, token: String) -> Result<(), String> {
    let address = server::saved().ok_or_else(|| "There is no server yet.".to_string())?;
    link.start(address, token);
    Ok(())
}

/// Stops it, and forgets the credential.
///
/// Signing out has to reach this half too. A link left open would be a route
/// into this network held by a session that has ended.
#[tauri::command]
fn link_stop(link: tauri::State<'_, Arc<Link>>) {
    link.stop();
}

/// Which installation this is, for the page to send with its requests.
///
/// The page asks once and repeats it: when it mints a credential, and on every
/// message, so a tool that reaches this computer reaches THIS one rather than
/// whichever of the person's machines connected most recently.
#[tauri::command]
fn link_device() -> String {
    sag_desktop::device::id()
}

/// Whether the route into this computer's network is up.
///
/// Asked once by a window that has just opened: it missed whatever was
/// announced before it existed, and the state is not something to guess at.
#[tauri::command]
fn link_up(link: tauri::State<'_, Arc<Link>>) -> bool {
    link.is_up()
}

/// The folder the assistant may work in, or nothing if none was chosen.
#[tauri::command]
fn chosen_folder() -> Option<String> {
    sag_desktop::workspace::folder().map(|p| p.to_string_lossy().into_owned())
}

/// Asks for one, with the system's own folder picker.
///
/// The dialog is the operating system's, so what is chosen is chosen by the
/// person in a window their computer drew, not by anything a page or a server
/// said. The doing is shared with the other edition, because it is the same
/// dialog under the same rule.
#[tauri::command]
async fn choose_folder(window: WebviewWindow) -> Result<Option<String>, String> {
    sag_desktop::workspace::pick(window).await
}

/// Takes it away again, which is how somebody stops the assistant reaching
/// their disk without signing out of anything.
#[tauri::command]
fn forget_folder() -> Result<(), String> {
    sag_desktop::workspace::forget()
}

/// The permission files, read as text so a test can compare them with the
/// commands this binary registers.
#[cfg(test)]
const PERMISSION_FILES: &[&str] = &[
    include_str!("../permissions/machine.toml"),
    include_str!("../permissions/screens.toml"),
    include_str!("../permissions/setup.toml"),
];

#[cfg(test)]
mod permission_contract {
    /// Every command the page can call has to be NAMED in a permission, or the
    /// call is refused and nobody hears about it.
    ///
    /// A page served by the gateway is REMOTE to this application, and Tauri
    /// allows a remote page none of its commands unless a permission names them
    /// and a capability grants it. Registering one in `generate_handler!` is
    /// only half; the other half is a TOML file, and forgetting it fails the way
    /// this whole area fails, which is silently: the page asks, gets "not
    /// allowed", and the code that asked treats a refusal like an absence.
    ///
    /// It has now happened three times, twice before this test existed (the
    /// right-click menu, then the machine environment) and the permission file's
    /// own comment is about the first of them. A comment did not stop the
    /// second, so this is a test.
    ///
    /// Read from the source text rather than from the macro, because the macro
    /// expands to code and there is nothing left to compare by then. It is the
    /// two lists that have to agree, and both are text.
    #[test]
    fn commands_are_all_permitted() {
        let source = include_str!("main.rs");
        let start = source
            .find("generate_handler![")
            .expect("the handler list moved; this test reads it by name");
        let body = &source[start..];
        let end = body.find("])").expect("the handler list is not closed");
        let registered: Vec<String> = body[..end]
            .lines()
            .skip(1)
            .filter_map(|line| {
                let name = line.trim().trim_end_matches(',').trim();
                let name = name.rsplit("::").next().unwrap_or(name);
                (!name.is_empty() && !name.starts_with("//")).then(|| name.to_string())
            })
            .collect();
        assert!(!registered.is_empty(), "no commands were read from the handler list");

        let permissions = super::PERMISSION_FILES.concat();
        let unpermitted: Vec<&String> = registered
            .iter()
            .filter(|name| !permissions.contains(name.as_str()))
            .collect();
        assert!(
            unpermitted.is_empty(),
            "these commands are registered but no permission names them, so the \
             page calls them and is refused with nothing written down anywhere: {unpermitted:?}"
        );
    }
}
