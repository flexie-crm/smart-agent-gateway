//! Where the JavaScript this browser runs comes from.
//!
//! One place, because there will be more than one file: Playwright's injected
//! script today, and whatever we write on top of it as the tools grow. A path
//! spelled out at each use is a path that is wrong in N places the day anything
//! moves.
//!
//! **Compiled in by default.** Every script ships inside the binary, so a build
//! with no network produces the same application, there is no file to be missing
//! from a bundle, and an installation cannot end up running a script that does
//! not match the browser it was pinned against.
//!
//! **Readable from a directory when asked.** `SAG_BROWSER_JS_DIR` points at a
//! folder of the same file names, and anything found there is used instead.
//! That is for one job: changing a script and seeing the effect without waiting
//! for a Rust build. It is exactly what `SAG_CONSOLE_DIR` and `SAG_CHAT_DIR`
//! already do for the two front ends, which is what makes `make dev-personal` a
//! reload rather than a ten minute rebuild.
//!
//! The override is not a security boundary and is not meant to be one. Anybody
//! who can set an environment variable on this process can already replace the
//! binary. It is a development convenience, and it says so where it is read.

use std::borrow::Cow;
use std::path::{Path, PathBuf};

/// The environment variable that redirects every script here.
const FROM_DIRECTORY: &str = "SAG_BROWSER_JS_DIR";

/// Playwright's injected script, held in `desktop/lib/playwright` (KB/43).
///
/// Anchored at the CRATE rather than at this file. `include_str!` resolves
/// relative to the source file it appears in, so `../../../lib/...` is a path
/// that silently breaks the day this module moves one level in or out, and the
/// failure is a compile error pointing at a file that is exactly where it
/// always was. `CARGO_MANIFEST_DIR` does not move.
const INJECTED_SCRIPT: &str = include_str!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../lib/playwright/injectedScript.js"
));

/// Playwright's US keyboard layout: what every key on it does.
///
/// Their file, their numbers. It is what makes the key names in their own
/// `press` documentation (`F1`, `Digit0`, `KeyA`, `Backquote`, `PageDown`)
/// mean what that documentation says they mean, rather than only the handful
/// somebody would think to write out.
const KEYBOARD_LAYOUT: &str = include_str!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../lib/playwright/usKeyboardLayout.json"
));

/// Playwright's macOS editing commands.
///
/// Also their file. On macOS a key press does not edit anything by itself:
/// Chromium expects the editing operation to be named alongside it, because on
/// a real Mac the operating system is what turns Command+Left into "go to the
/// start of the line". Sending the key without the command is a key that
/// registers and does nothing.
const MAC_EDITING_COMMANDS: &str = include_str!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../lib/playwright/macEditingCommands.json"
));

/// The file name each script is known by, used both for the compiled-in copy
/// and for the override directory, so the two cannot disagree about what a
/// script is called.
const INJECTED_SCRIPT_NAME: &str = "injectedScript.js";
const KEYBOARD_LAYOUT_NAME: &str = "usKeyboardLayout.json";
const MAC_EDITING_COMMANDS_NAME: &str = "macEditingCommands.json";

/// Playwright's injected script: what finds things on a page.
pub fn injected_script() -> Cow<'static, str> {
    read(
        overriding().as_deref(),
        INJECTED_SCRIPT_NAME,
        INJECTED_SCRIPT,
    )
}

/// Playwright's keyboard layout, as JSON.
pub fn keyboard_layout() -> Cow<'static, str> {
    read(
        overriding().as_deref(),
        KEYBOARD_LAYOUT_NAME,
        KEYBOARD_LAYOUT,
    )
}

/// Playwright's macOS editing commands, as JSON.
pub fn mac_editing_commands() -> Cow<'static, str> {
    read(
        overriding().as_deref(),
        MAC_EDITING_COMMANDS_NAME,
        MAC_EDITING_COMMANDS,
    )
}

/// Where the override directory is, if there is one.
pub fn overriding() -> Option<PathBuf> {
    let dir = std::env::var(FROM_DIRECTORY).ok()?;
    if dir.trim().is_empty() {
        return None;
    }
    Some(PathBuf::from(dir))
}

/// read hands back what is on disk when a directory was given, and what was
/// compiled in otherwise.
///
/// A file that is named but cannot be read falls back to the compiled-in copy
/// and SAYS so. The alternative is a browser that silently reverts to the
/// shipped script while somebody is trying to change it, which is half an hour
/// of wondering why an edit does nothing.
fn read(dir: Option<&Path>, name: &str, built_in: &'static str) -> Cow<'static, str> {
    let Some(dir) = dir else {
        return Cow::Borrowed(built_in);
    };
    let path = dir.join(name);
    match std::fs::read_to_string(&path) {
        Ok(script) => {
            eprintln!("browser: using {} from {}", name, path.display());
            Cow::Owned(script)
        }
        Err(err) => {
            eprintln!(
                "browser: {} could not be read from {} ({err}); using the one built in",
                name,
                path.display()
            );
            Cow::Borrowed(built_in)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // These take the directory as an ARGUMENT and never touch the environment,
    // which is not a style choice. The first version set SAG_BROWSER_JS_DIR to
    // a folder holding a two word file, and every test in this binary that
    // reads a script reads the same variable: the inject tests, running in
    // parallel in the same process, got `// mine` instead of three hundred
    // kilobytes of Playwright and failed five runs out of ten. An environment
    // variable is process-wide, so a test that writes one is a test that writes
    // everybody else's input.

    /// With no directory given, the compiled-in script is what runs.
    #[test]
    fn the_script_is_compiled_in_by_default() {
        let script = read(None, INJECTED_SCRIPT_NAME, INJECTED_SCRIPT);
        assert!(matches!(script, Cow::Borrowed(_)), "it was read from disk");
        assert!(
            script.len() > 250_000,
            "the compiled-in script is too small"
        );
    }

    /// A directory that has the file wins.
    #[test]
    fn a_directory_replaces_the_script() {
        let dir = std::env::temp_dir().join(format!("sag-js-has-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("make the directory");
        std::fs::write(dir.join(INJECTED_SCRIPT_NAME), "// mine").expect("write");

        assert_eq!(
            read(Some(&dir), INJECTED_SCRIPT_NAME, INJECTED_SCRIPT).as_ref(),
            "// mine",
            "the file on disk should have been used"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// And one that does not falls back rather than failing.
    ///
    /// Somebody pointing this at the wrong folder gets a working browser and a
    /// line saying which script it is actually running. The alternative is an
    /// application that will not start because a development convenience was
    /// misspelled.
    #[test]
    fn a_directory_without_the_file_falls_back() {
        let dir = std::env::temp_dir().join(format!("sag-js-empty-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("make the directory");

        let script = read(Some(&dir), INJECTED_SCRIPT_NAME, INJECTED_SCRIPT);
        assert!(
            script.len() > 250_000,
            "an unreadable override should fall back to the built-in script"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }
}
