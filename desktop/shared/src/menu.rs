//! The menu a right-click gives, which is ours rather than the browser's.
//!
//! A webview's own menu gives away that this is a web page: on macOS it offers
//! "Search with Google", "Translate", "Speech" and "Services" over the inside of
//! an application that does none of those. So the page suppresses it
//! (`desktop-chrome.ts`) everywhere it is not useful, and that left nothing at
//! all over most of the window: no Copy over an answer somebody wanted, and no
//! way to reload a page that had gone wrong short of quitting.
//!
//! This is the other half. The page asks for a menu and says what is under the
//! pointer; we build the one an application would have and pop it where the
//! click was.
//!
//! **The editing items are PREDEFINED, not ours.** `PredefinedMenuItem::copy`
//! is the system's own, so it carries the platform accelerator, the platform
//! label in the person's language, and it acts on the webview's real selection.
//! Sending a "copy" event back to JavaScript instead would mean reimplementing
//! the clipboard against a selection we cannot see, and getting it wrong inside
//! a text field, which is the one place the browser already had it right.
//!
//! Reload is ours, because there is no predefined one: a webview reload is a
//! thing only the host can do.

use tauri::menu::{Menu, MenuEvent, MenuItem, PredefinedMenuItem};
use tauri::{AppHandle, Manager, Runtime, WebviewWindow};

/// What the page says is under the pointer, so the menu offers what applies.
///
/// It is the page's to decide because only the page knows: whether the click
/// landed in a text field, and whether anything is selected. Asking the webview
/// from here would mean evaluating JavaScript to find out and then acting on the
/// answer, which is the same question asked the long way round.
#[derive(Debug, Clone, Copy, serde::Deserialize)]
pub struct Where {
    /// A field somebody can type into: paste and select-all apply.
    #[serde(default)]
    pub editable: bool,
    /// Something is selected, so copy and cut mean something.
    #[serde(default)]
    pub selection: bool,
}

/// The id of our one non-predefined item. Compared on the event, so it is
/// written once rather than spelled out at both ends.
const RELOAD: &str = "reload";

/// Build the menu for one right-click and show it at the pointer.
///
/// Nothing that cannot act is offered: Copy with no selection would be a dead
/// row, and Paste outside a field would paste into nothing. A menu whose items
/// are mostly greyed out is a menu that has not been thought about.
pub fn popup<R: Runtime>(window: &WebviewWindow<R>, at: Where) -> tauri::Result<()> {
    let app = window.app_handle();
    let mut items: Vec<Box<dyn tauri::menu::IsMenuItem<R>>> = Vec::new();

    if at.selection {
        items.push(Box::new(PredefinedMenuItem::copy(app, None)?));
        if at.editable {
            items.push(Box::new(PredefinedMenuItem::cut(app, None)?));
        }
    }
    if at.editable {
        items.push(Box::new(PredefinedMenuItem::paste(app, None)?));
        items.push(Box::new(PredefinedMenuItem::select_all(app, None)?));
    }
    if !items.is_empty() {
        items.push(Box::new(PredefinedMenuItem::separator(app)?));
    }
    items.push(Box::new(MenuItem::with_id(
        app,
        RELOAD,
        "Reload",
        true,
        // The accelerator people already have in their fingers, and it works
        // whether or not this menu is open.
        Some("CmdOrCtrl+R"),
    )?));

    let refs: Vec<&dyn tauri::menu::IsMenuItem<R>> = items.iter().map(|i| i.as_ref()).collect();
    let menu = Menu::with_items(app, &refs)?;
    window.popup_menu(&menu)
}

/// Act on the one item that is ours. The predefined ones act by themselves.
pub fn handle<R: Runtime>(app: &AppHandle<R>, event: MenuEvent) {
    if event.id() != RELOAD {
        return;
    }
    // Every window, because the reload was asked for by the one in front and
    // there is only ever one carrying content. Naming a label here would be a
    // second place that has to agree with the shell that made it.
    for (_, window) in app.webview_windows() {
        let _ = window.eval("window.location.reload()");
    }
}

/// The page asking for a menu, because only it knows what is under the pointer.
#[tauri::command]
pub fn show_context_menu<R: Runtime>(window: WebviewWindow<R>, at: Where) -> Result<(), String> {
    popup(&window, at).map_err(|err| err.to_string())
}
