//! The folder the assistant may work in.
//!
//! Chosen on this computer, by the person using it, and nowhere else. A server
//! cannot know what is on somebody's disk, and a folder that arrived from one
//! would be a folder they never agreed to.
//!
//! One folder, not a list. It is the shape people already understand from every
//! editor and every terminal — you are working in a project — and it is the
//! whole of the scope: a command runs there, a path is relative to it, and
//! anything that resolves outside it is refused.

use std::path::PathBuf;
use std::sync::Mutex;

use tauri::WebviewWindow;
use tauri_plugin_dialog::DialogExt;

/// Where it is remembered, beside the server address and the device id.
const FILE: &str = "workspace.json";

/// The folder, read once and kept: a tool call must not wait on a disk read,
/// and it changes only when somebody chooses another.
static CHOSEN: Mutex<Option<Option<PathBuf>>> = Mutex::new(None);

/// The state directory, which the shell knows and this module does not.
static STATE: Mutex<Option<PathBuf>> = Mutex::new(None);

/// remember where this installation keeps its files. Called once at startup by
/// the shell, which is the half that knows.
pub fn use_state_dir(dir: PathBuf) {
    *held(&STATE) = Some(dir);
}

/// state_dir is where this installation keeps its files, as the shell said.
///
/// Nothing here can work it out: one edition keeps it beside a server address
/// the person typed, the other beside a database it started. So the shell tells
/// us once, and everything on this side reads it from here.
pub fn state_dir() -> Option<PathBuf> {
    held(&STATE).clone()
}

/// folder is what was chosen, or nothing if nobody has chosen yet.
pub fn folder() -> Option<PathBuf> {
    if let Some(known) = held(&CHOSEN).clone() {
        return known;
    }
    let read = read();
    *held(&CHOSEN) = Some(read.clone());
    read
}

/// choose remembers a folder, after checking it is one.
pub fn choose(path: PathBuf) -> Result<(), String> {
    if !path.is_dir() {
        return Err(format!("{} is not a folder", path.to_string_lossy()));
    }
    write(&path)?;
    *held(&CHOSEN) = Some(Some(path));
    Ok(())
}

/// forget takes the folder away, which is how somebody stops the assistant
/// reaching their disk without signing out of anything.
pub fn forget() -> Result<(), String> {
    let Some(dir) = held(&STATE).clone() else {
        return Ok(());
    };
    match std::fs::remove_file(dir.join(FILE)) {
        Ok(()) => {}
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => {}
        Err(err) => return Err(format!("{err}")),
    }
    *held(&CHOSEN) = Some(None);
    Ok(())
}

fn read() -> Option<PathBuf> {
    let dir = held(&STATE).clone()?;
    let raw = std::fs::read_to_string(dir.join(FILE)).ok()?;
    let value: serde_json::Value = serde_json::from_str(&raw).ok()?;
    let path = value.get("folder")?.as_str()?.trim();
    if path.is_empty() {
        return None;
    }
    let path = PathBuf::from(path);
    // A folder that has been deleted or unmounted since is not a folder. Saying
    // nothing was chosen is the honest answer, and the tool then says so.
    path.is_dir().then_some(path)
}

fn write(path: &std::path::Path) -> Result<(), String> {
    let dir = held(&STATE).clone().ok_or("this installation has no state directory")?;
    std::fs::create_dir_all(&dir).map_err(|e| format!("{e}"))?;
    let body = serde_json::json!({ "folder": path.to_string_lossy() }).to_string();
    std::fs::write(dir.join(FILE), body).map_err(|e| format!("{e}"))
}

fn held<T>(lock: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    lock.lock().unwrap_or_else(|poisoned| poisoned.into_inner())
}

/// Asks for the folder, with the system's own picker.
///
/// Here rather than in a shell because it is the same on both editions: what
/// the assistant may reach is one folder, chosen by the person in a dialog
/// their own computer drew, and everything outside it is refused on this side.
///
/// The CALLBACK form, not the blocking one. A file dialog on macOS has to be
/// opened from the main thread, and a Tauri command marked async does not run
/// there. `blocking_pick_folder` from one does not fail, it does NOTHING: no
/// window, no error, no log line, and a person clicking a button that appears
/// to be dead.
pub async fn pick(window: WebviewWindow) -> Result<Option<String>, String> {
    let (answer, wait) = tokio::sync::oneshot::channel();
    window
        .dialog()
        .file()
        .set_title("Choose the folder the assistant may work in")
        .pick_folder(move |picked| {
            let _ = answer.send(picked);
        });

    let Some(picked) = wait
        .await
        .map_err(|_| "the folder dialog was interrupted".to_string())?
    else {
        return Ok(None); // they closed it, which is an answer rather than an error
    };
    let path = picked
        .into_path()
        .map_err(|e| format!("that folder could not be read: {e}"))?;
    choose(path.clone())?;
    Ok(Some(path.to_string_lossy().into_owned()))
}
