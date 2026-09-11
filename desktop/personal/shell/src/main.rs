// SAG Personal (KB/36).
//
// One application, one person, one machine. It starts the gateway, waits for it
// to answer, and points a webview at it; everything a person sees is served by
// the gateway, exactly as it is on a server. There is no product logic here.
//
// The other edition, SAG Enterprise, is the same window with none of this: it
// starts nothing, because the server belongs to somebody else. What the two
// genuinely share lives in the `sag-desktop` crate beside them.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod gateway;

use std::sync::{Arc, Mutex};

use sag_desktop::link::Link;
use sag_desktop::screens;
use sag_desktop::{open_outside, report_progress, show_failure};
use tauri::webview::{NewWindowResponse, WebviewWindowBuilder};
use tauri::{Emitter, Manager, RunEvent, WebviewUrl, WebviewWindow};

/// Where the window opens.
///
/// The chat, because that is what the application IS. With nothing set up yet
/// the chat says so and offers the console, which is one click and the only
/// moment anybody needs it; opening on the console every time would be opening
/// an assistant on its settings.
///
/// The console is the same origin, so moving between them is an ordinary link.
/// They remain two applications sharing a gateway and no logic, which is what
/// lets the console live on the web in the full edition while this one ships as
/// an application.
const OPENS_AT: &str = "/chat/";

/// Where this installation's own gateway answered.
///
/// Known only once it has started, because the port is chosen then, and needed
/// later by the half that opens the link. One value, written once.
static ADDRESS: Mutex<Option<String>> = Mutex::new(None);

fn address() -> Option<String> {
    ADDRESS.lock().ok().and_then(|held| held.clone())
}

/// What the served page may ask this half of the application to do.
///
/// The page is served by the gateway over loopback, which makes it REMOTE as
/// far as the webview is concerned, and Tauri gives a remote page none of an
/// application's own commands unless a capability names them. Without one the
/// page calls, gets "not allowed", and neither half can say so: the link simply
/// never starts, which is exactly what this edition shipped doing.
///
/// Granted at run time for THIS installation's own gateway, because the port is
/// chosen when it starts and a capability in a file would have to be a wildcard
/// over loopback.
fn allow_the_page(app: &tauri::AppHandle, address: &str) {
    let capability = format!(
        r#"{{
          "identifier": "the-gateway-page",
          "description": "The chat and console, served by this installation's own gateway.",
          "windows": ["main", "chat"],
          "remote": {{ "urls": ["{address}/*"] }},
          "permissions": ["core:default", "core:event:default", "allow-machine", "allow-screens", "store:default"]
        }}"#
    );
    if let Err(err) = app.add_capability(capability) {
        // Worth a line rather than a silence: everything else will work, and
        // the one thing that will not is the assistant reaching this computer,
        // which is the failure that is hardest to see from the outside.
        eprintln!("link: the served page was not granted access to this computer: {err}");
    }
}

/// Starts the link, or hands a fresh credential to one already running.
///
/// The gateway is on this same computer, and the link is needed all the same:
/// it carries the CALLS whose far end is this application (the terminal, the
/// files), not only a connection to something on a network (KB/39). Leaving it
/// out is what left the edition that runs on somebody's own computer as the one
/// that could not touch their terminal or their files.
#[tauri::command]
fn link_start(link: tauri::State<'_, Arc<Link>>, token: String) -> Result<(), String> {
    let address = address().ok_or_else(|| "the gateway has not started yet".to_string())?;
    link.start(address, token);
    Ok(())
}

/// Stops it, and forgets the credential. Signing out has to reach this half.
#[tauri::command]
fn link_stop(link: tauri::State<'_, Arc<Link>>) {
    link.stop();
}

/// Whether the link is up right now, for a window that has just opened and
/// missed whatever was announced before it existed.
#[tauri::command]
fn link_up(link: tauri::State<'_, Arc<Link>>) -> bool {
    link.is_up()
}

/// Which installation this is, sent with every message so a tool that runs on
/// this computer runs on THIS one.
#[tauri::command]
fn link_device() -> String {
    sag_desktop::device::id()
}

/// The folder the assistant may work in, or nothing if none was chosen.
#[tauri::command]
fn chosen_folder() -> Option<String> {
    sag_desktop::workspace::folder().map(|p| p.to_string_lossy().into_owned())
}

/// Asks for one, with the system's own folder picker.
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

/// How long the waiting screen stays before the chat replaces it.
///
/// Longer than the enterprise edition's, and for a reason it does not have: this
/// screen carries a real account of what is happening (creating a database,
/// applying migrations), and on a machine where the gateway is already warm all
/// of that is over almost at once. A screen that appears and vanishes inside
/// half a second reads as a glitch rather than as an application starting.
const WAITING_STAYS: std::time::Duration = std::time::Duration::from_millis(1500);

/// A window of this application: the waiting screen and the chat are the same
/// window twice, differing only in what they load. Written once, and never
/// without `visible(false)`, which is the whole of how the white frame is
/// avoided (sag_desktop::screens).
fn a_window(
    app: &tauri::AppHandle,
    label: &str,
    url: WebviewUrl,
) -> tauri::Result<tauri::WebviewWindow> {
    let window = WebviewWindowBuilder::new(app, label, url)
        .title("SAG Personal")
        .inner_size(1180.0, 820.0)
        .min_inner_size(720.0, 560.0)
        // Built unseen, and shown by draws_unseen once the webview inside it is
        // transparent. The window's own colour is then the only thing on the
        // screen until the page has drawn, so the ground never changes.
        .visible(false)
        .background_color(screens::ground())
        // The appearance, handed to every page a window loads, BEFORE any of
        // them parses a line.
        //
        // Without it the chat renders light and then turns dark, because it
        // works the appearance out after it has started. It reads what it
        // remembered last time, and here there is nothing to read: the gateway
        // binds to a free port, so the address changes on every launch, a
        // different port is a different origin, and a different origin is an
        // empty storage. An initialization script runs after the global object
        // exists and before the document is parsed, which is the one moment
        // early enough. The page reads this ahead of its own memory.
        .initialization_script(format!(
            "window.__SAG_APPEARANCE__ = {};{}",
            serde_json::to_string(&sag_desktop::appearance::chosen())
                .unwrap_or_else(|_| "\"system\"".into()),
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

fn main() {
    let app = tauri::Builder::default()
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
            chosen_folder,
            choose_folder,
            forget_folder
        ])
        .on_menu_event(sag_desktop::menu::handle)
        .plugin(tauri_plugin_updater::Builder::new().build())
        .plugin(tauri_plugin_store::Builder::default().build())
        .plugin(tauri_plugin_dialog::init())
        .setup(move |app| {
            // Before any window is built: what shows one is its page saying it
            // has drawn a frame, and this is how long the first of them stays.
            screens::holds_the_waiting_screen_for(WAITING_STAYS);
            // The next version, fetched quietly and announced once. It waits a
            // minute and a half before the first look, because the seconds after
            // launch belong to the gateway starting.
            sag_desktop::update::watch(app.handle().clone());
            // Where this installation keeps its files, told once to the halves
            // that remember things: which installation this is, and which
            // folder the assistant may work in.
            if let Ok(dir) = gateway::state_dir() {
                sag_desktop::workspace::use_state_dir(dir);
            }
            // And where the SETTINGS file is, which is somewhere else: the store
            // plugin writes under the identifier, everything else here is under
            // the product name. Said before the window is built, because the
            // window's colour is read from it.
            if let Ok(dir) = app.path().app_config_dir() {
                sag_desktop::appearance::use_settings_dir(dir);
            }

            // The link into this computer, idle until the page signs in and
            // hands it a credential. The gateway it dials is this application's
            // own, over loopback, which is the same mechanism the other edition
            // uses over a network and deliberately not a second one.
            let asking = app.handle().clone();
            let announcing = app.handle().clone();
            app.manage(Link::new(
                move || {
                    let _ = asking.emit(sag_desktop::link::NEEDS_TOKEN, ());
                },
                move |up| {
                    let _ = announcing.emit(sag_desktop::link::STATE_CHANGED, up);
                },
            ));

            // Built here rather than declared in the configuration, for one
            // reason: a window can only be told what a second window means
            // while it is being built.
            // The window somebody sees first: the screen that says what is
            // being started. Opened knowing which appearance this person chose,
            // because it cannot find out for itself (it belongs to the
            // application and the chat belongs to the gateway, which are
            // different origins with different storage).
            let window = a_window(
                &app.handle().clone(),
                screens::WAITING,
                WebviewUrl::App(
                    format!(
                        "index.html?appearance={}",
                        sag_desktop::appearance::chosen()
                    )
                    .into(),
                ),
            )?;

            let resources = app
                .path()
                .resource_dir()
                .map_err(|e| format!("the application is missing its resources: {e}"))?;

            // Starting the gateway can mean creating a database and applying
            // every migration, which is half a minute somebody is looking at.
            // The window therefore opens at once on a page that says so, and is
            // sent to the real one when there is a real one.
            let progress_window = window.clone();
            let report =
                move |pct: u8, message: &str| report_progress(&progress_window, pct, message);

            let opening = app.handle().clone();
            std::thread::spawn(move || match gateway::ensure_running(&resources, &report) {
                Ok(port) => {
                    // The page is granted its commands BEFORE it is loaded: a
                    // capability added after the page is running does not reach
                    // the page that is already there.
                    let origin = format!("http://127.0.0.1:{port}");
                    if let Ok(mut held) = ADDRESS.lock() {
                        *held = Some(origin.clone());
                    }
                    allow_the_page(&opening, &origin);
                    let url = format!("http://127.0.0.1:{port}{OPENS_AT}");
                    match url.parse() {
                        Ok(parsed) => {
                            // A second window rather than sending this one
                            // there: navigating the window somebody is looking
                            // at means watching it go white on the way. This one
                            // loads unseen and the two swap, which is also what
                            // holds the waiting screen on the screen long enough
                            // to have been read.
                            if let Err(err) =
                                a_window(&opening, screens::CHAT, WebviewUrl::External(parsed))
                            {
                                show_failure(
                                    &window,
                                    &format!("This window could not be opened: {err}"),
                                );
                            }
                        }
                        Err(err) => {
                            show_failure(&window, &format!("A bad address was built: {err}"))
                        }
                    }
                }
                Err(err) => show_failure(&window, &err),
            });
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("the shell could not start");

    app.run(|_handle, event| {
        // One application, so there is nothing to count: when this window goes,
        // the gateway goes with it.
        if let RunEvent::Exit = event {
            gateway::stop_running();
        }
    });
}

#[cfg(test)]
mod tests {
    /// The page is served by this installation's own gateway, which makes it a
    /// REMOTE page as far as the webview is concerned. Without `withGlobalTauri`
    /// the bridge is never injected: `window.__TAURI__` is undefined, the page
    /// asks this half of the application for nothing, and every symptom is an
    /// absence. No error, no log line, no failed call. The link never starts,
    /// the assistant has no terminal and no files on the machine it is running
    /// on, and the only visible sign is an indicator that stays grey.
    ///
    /// This edition shipped without it while carrying all the rest of the
    /// wiring, so the setting is asserted rather than trusted.
    #[test]
    fn the_page_is_given_the_bridge() {
        let config = include_str!("../tauri.conf.json");
        let parsed: serde_json::Value =
            serde_json::from_str(config).expect("the configuration is not valid JSON");
        assert_eq!(
            parsed["app"]["withGlobalTauri"], true,
            "the served page would not be able to reach this half of the application"
        );
    }

    /// And what it may ask for. A capability names commands one at a time, so a
    /// command added to the handler and forgotten here is a command the page
    /// calls and is refused, which looks exactly like the command not existing.
    #[test]
    fn the_page_may_ask_for_what_this_shell_offers() {
        let permission = include_str!("../permissions/machine.toml");
        for command in [
            "link_start",
            "link_stop",
            "link_up",
            "link_device",
            "chosen_folder",
            "choose_folder",
            "forget_folder",
        ] {
            assert!(
                permission.contains(command),
                "the page is never allowed to call {command}"
            );
        }
    }
}
