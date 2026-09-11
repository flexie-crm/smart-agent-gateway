//! Whether this person reads in daylight or at night.
//!
//! The application knows: they chose it in the chat, and it is kept in that
//! page's own storage. The WINDOW does not, and cannot. A shell's first screen
//! is served from the application itself and the chat from a server, which are
//! different origins with different storage, and no amount of asking will cross
//! that line.
//!
//! So the page tells this side once, when the choice is made, and the shell puts
//! it back into the first screen it opens. Without it that screen can only
//! follow the computer, which is right for somebody who never chose and wrong
//! for everybody who did: they set the application to night, and it opened white
//! every time.
//!
//! One word in a file beside the server address and the device id. It is a
//! preference, not a secret, and losing it costs one flash of the wrong colour.

use std::fs;
use std::path::PathBuf;
use std::sync::Mutex;

use crate::workspace::state_dir;

/// Where it is remembered: the settings file the page writes through the store
/// plugin, so both halves read and write ONE thing rather than two that drift.
pub const FILE: &str = "settings.json";

/// The key inside it, spelled the way the page spells it, because the page is
/// what writes it.
pub const KEY: &str = "fx_theme";

/// Where the settings file is, which is NOT where this application keeps its own
/// files.
///
/// The store plugin writes under the identifier (io.flexie.sag.enterprise), and
/// everything else here is under the product name (SAG Enterprise). Two
/// directories, and the page writing to one while this side read the other is
/// two records that can never meet. The shell knows both and says which.
static SETTINGS: Mutex<Option<PathBuf>> = Mutex::new(None);

/// Told once at startup, before the window is built.
pub fn use_settings_dir(dir: PathBuf) {
    if let Ok(mut held) = SETTINGS.lock() {
        *held = Some(dir);
    }
}

fn path() -> Option<PathBuf> {
    if let Ok(held) = SETTINGS.lock() {
        if let Some(dir) = held.clone() {
            return Some(dir.join(FILE));
        }
    }
    state_dir().map(|dir| dir.join(FILE))
}

/// What this used to be kept in, before the page and this side shared one file.
///
/// Read when the settings file has nothing to say, so that somebody who already
/// chose does not have to choose again. Nothing writes it any more.
fn the_old_way() -> Option<String> {
    let raw = fs::read_to_string(state_dir()?.join("appearance.json")).ok()?;
    let kept: serde_json::Value = serde_json::from_str(&raw).ok()?;
    match kept.get("theme").and_then(|v| v.as_str()) {
        Some(word @ ("system" | "light" | "dark")) => Some(word.to_string()),
        _ => None,
    }
}

/// remember what was chosen. Anything not one of the three words is ignored
/// rather than stored, so a future choice this build does not know cannot make
/// the first screen unreadable.
pub fn remember(theme: &str) {
    if !matches!(theme, "system" | "light" | "dark") {
        return;
    }
    let Some(file) = path() else { return };
    if let Some(dir) = file.parent() {
        let _ = fs::create_dir_all(dir);
    }
    // Read, change one key, write. The file belongs to the store plugin and the
    // page writes other things into it; replacing it wholesale would throw those
    // away.
    let mut kept: serde_json::Map<String, serde_json::Value> = fs::read_to_string(&file)
        .ok()
        .and_then(|raw| serde_json::from_str(&raw).ok())
        .unwrap_or_default();
    kept.insert(KEY.into(), serde_json::Value::String(theme.into()));
    if let Ok(body) = serde_json::to_string(&kept) {
        let _ = fs::write(file, body);
    }
}

/// chosen is what was chosen, or "system" when nobody has: follow the computer,
/// which is what somebody who has never touched it expects.
pub fn chosen() -> String {
    let stored = path()
        .and_then(|file| fs::read_to_string(file).ok())
        .and_then(|raw| serde_json::from_str::<serde_json::Value>(&raw).ok())
        .and_then(|kept| {
            kept.get(KEY)
                .and_then(|v| v.as_str())
                .filter(|w| matches!(*w, "system" | "light" | "dark"))
                .map(|w| w.to_string())
        });
    stored.or_else(the_old_way).unwrap_or_else(|| "system".into())
}

/// What is actually wanted right now: the choice, or the computer's own setting
/// when the choice is to follow it.
///
/// Asked of the operating system rather than guessed. Guessing is what the
/// window's colour used to do (it assumed night), which is right for somebody
/// working at night and wrong in every lit room.
pub fn wanted() -> Wanted {
    match chosen().as_str() {
        "light" => Wanted::Light,
        "dark" => Wanted::Dark,
        _ => match dark_light::detect() {
            Ok(dark_light::Mode::Light) => Wanted::Light,
            // Unknown counts as dark: a dark blink in a lit room is a blink, a
            // white one in a dark room is what all of this is about.
            _ => Wanted::Dark,
        },
    }
}

/// Light or night, with nothing left to decide.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Wanted {
    Light,
    Dark,
}

/// The command the page calls when somebody picks light, night, or the
/// computer's own setting.
#[tauri::command]
pub fn remember_appearance(app: tauri::AppHandle, theme: String) {
    remember(&theme);
    // The windows' own ground is what shows whenever a page has nothing drawn,
    // so it follows the choice rather than staying at whatever it was when they
    // were built.
    crate::screens::the_ground_changed(&app);
}

/// And the command it calls to find out what was chosen last time.
///
/// Which it cannot do for itself, and this is the sharpest reason why: the
/// personal edition serves the chat from a gateway that binds to a free port,
/// so the address changes on every launch. A different port is a different
/// origin, a different origin is a different storage, and everything the page
/// remembered is gone. The choice therefore lives on this side, where it
/// survives the port.
#[tauri::command]
pub fn chosen_appearance() -> String {
    chosen()
}
