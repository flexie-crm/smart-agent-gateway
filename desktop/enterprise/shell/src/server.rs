//! Which server this installation talks to.
//!
//! The whole of what SAG Enterprise remembers. There is no database here, no
//! keys and no models: the server holds all of that, and this is a window onto
//! it. What must survive a restart is one address.
//!
//! The SESSION is deliberately not here. Signing in is the server's business
//! and the cookies are the webview's, exactly as they are in a browser, which
//! is what makes this the same logged-in session the console has on the web
//! rather than a second scheme to keep secure.

use std::fs;
use std::path::PathBuf;
use std::time::Duration;

use serde::{Deserialize, Serialize};

/// This edition's folder, beside SAG Personal's rather than inside it: both may
/// be installed, and one must never read the other's settings.
const DATA_DIR: &str = "SAG Enterprise";

/// How long a server gets to answer before we call it unreachable. Short: a
/// person typed an address and is waiting to find out whether it was right.
const PROBE_TIMEOUT: Duration = Duration::from_secs(10);

#[derive(Serialize, Deserialize)]
pub struct Settings {
    pub address: String,
}

pub fn state_dir() -> Result<PathBuf, String> {
    let base = dirs_config().ok_or("this system has no application data directory")?;
    Ok(base.join(DATA_DIR))
}

#[cfg(target_os = "macos")]
fn dirs_config() -> Option<PathBuf> {
    std::env::var_os("HOME").map(|h| PathBuf::from(h).join("Library/Application Support"))
}

#[cfg(target_os = "windows")]
fn dirs_config() -> Option<PathBuf> {
    std::env::var_os("APPDATA").map(PathBuf::from)
}

#[cfg(all(unix, not(target_os = "macos")))]
fn dirs_config() -> Option<PathBuf> {
    std::env::var_os("XDG_CONFIG_HOME")
        .map(PathBuf::from)
        .or_else(|| std::env::var_os("HOME").map(|h| PathBuf::from(h).join(".config")))
}

fn settings_path() -> Result<PathBuf, String> {
    Ok(state_dir()?.join("server.json"))
}

/// The address this installation was pointed at, if it has been.
pub fn saved() -> Option<String> {
    let raw = fs::read_to_string(settings_path().ok()?).ok()?;
    let settings: Settings = serde_json::from_str(&raw).ok()?;
    let address = settings.address.trim().to_string();
    if address.is_empty() {
        return None;
    }
    Some(address)
}

pub fn save(address: &str) -> Result<(), String> {
    let dir = state_dir()?;
    fs::create_dir_all(&dir).map_err(|e| format!("cannot create {}: {e}", dir.display()))?;
    let body = serde_json::to_string_pretty(&Settings { address: address.to_string() })
        .map_err(|e| format!("cannot write the settings: {e}"))?;
    fs::write(settings_path()?, body).map_err(|e| format!("cannot save the address: {e}"))
}

/// Forgets it, which is what signing out of one server and into another means.
pub fn forget() -> Result<(), String> {
    match fs::remove_file(settings_path()?) {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(format!("cannot forget the address: {e}")),
    }
}

/// Reads what somebody typed as an address, or says why it is not one.
///
/// People type `sag.company.com`, and they are not wrong to: it is an address.
/// So a missing scheme is filled in rather than refused, and only then is the
/// result held to the rule. A trailing slash goes, because every path is built
/// by appending to this and `//chat/` is a different path on some servers.
pub fn normalise(typed: &str) -> Result<String, String> {
    let typed = typed.trim().trim_end_matches('/');
    if typed.is_empty() {
        return Err("Enter the address of your SAG server.".into());
    }
    let with_scheme = if typed.contains("://") {
        typed.to_string()
    } else {
        format!("https://{typed}")
    };
    let parsed = tauri::Url::parse(&with_scheme)
        .map_err(|_| format!("{typed} is not an address SAG can open."))?;
    if !matches!(parsed.scheme(), "http" | "https") {
        return Err("The address must start with https:// or http://.".into());
    }
    if parsed.host_str().is_none() {
        return Err("That address is missing a server name.".into());
    }
    Ok(with_scheme)
}

/// What a SAG server says it is.
///
/// Only the field this side acts on. A server describes itself in capabilities
/// rather than by a name, so this asks a question ("is one person all there is
/// here?") instead of matching a label.
#[derive(Deserialize)]
struct Posture {
    single_user: bool,
}

/// Whether a SAG server is answering at this address, and whether it is one
/// this application should be pointed at.
///
/// Checked BEFORE the address is saved, so an address that is accepted is one
/// that works. The alternative is the failure this codebase keeps refusing:
/// something is accepted, the window navigates, and the person is left looking
/// at a browser error with no way back to the field they typed it in.
///
/// It asks for the posture rather than a health check, and that is the point:
/// plenty of things answer 200 on a URL, and "the address is reachable" is not
/// the question. A reply we can read as a posture is a SAG server.
pub fn reachable(address: &str) -> Result<(), String> {
    let agent = ureq::AgentBuilder::new()
        .timeout_connect(PROBE_TIMEOUT)
        .timeout_read(PROBE_TIMEOUT)
        .build();
    let response = match agent.get(&format!("{address}/v1/meta")).call() {
        Ok(response) => response,
        // Something is there and it is not answering the way SAG does, or it is
        // SAG and it is unwell. Saying which is more use than "cannot connect".
        Err(ureq::Error::Status(code, _)) => {
            return Err(format!(
                "The server at {address} answered {code}. Check the address, or ask whoever runs it."
            ))
        }
        Err(_) => {
            return Err(format!(
                "Nothing answered at {address}. Check the address, and that you can reach it from here."
            ))
        }
    };
    let posture: Posture = response
        .into_json()
        .map_err(|_| format!("{address} answered, but it is not a SAG server."))?;
    if posture.single_user {
        // A personal installation is one person on their own machine, signing
        // in without a password because only that machine can reach it. Whoever
        // typed this meant their company's server, and the mistake is worth
        // catching here rather than at a sign-in screen that behaves oddly.
        return Err(format!(
            "{address} is a personal installation, not a company server. \
             Ask your administrator for the address of your SAG server."
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::normalise;

    #[test]
    fn an_address_typed_the_way_people_type_them_is_understood() {
        assert_eq!(normalise("sag.example.com").unwrap(), "https://sag.example.com");
        assert_eq!(normalise("  sag.example.com/  ").unwrap(), "https://sag.example.com");
        assert_eq!(normalise("https://sag.example.com/").unwrap(), "https://sag.example.com");
        // Not everybody's server has a certificate yet, and an internal one on
        // plain HTTP is a real deployment rather than a mistake to refuse.
        assert_eq!(normalise("http://10.0.0.4:8080").unwrap(), "http://10.0.0.4:8080");
    }

    #[test]
    fn what_is_not_an_address_is_refused_with_a_reason() {
        for typed in ["", "   ", "ftp://files.example.com", "javascript:alert(1)"] {
            assert!(normalise(typed).is_err(), "{typed:?} was accepted");
        }
        // The reason is for a person to act on, not a parser error.
        let why = normalise("ftp://files.example.com").unwrap_err();
        assert!(why.contains("https://"), "unhelpful reason: {why}");
    }
}
