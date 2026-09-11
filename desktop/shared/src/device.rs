//! Which installation this is.
//!
//! A person may be signed in on a laptop and a desktop at once. A tool that
//! reaches their own network has to reach the computer they are TYPING on, and
//! nothing on the server can work that out: the request is the only thing that
//! knows where it came from. So this installation gives itself a name, the page
//! sends it with every request, and the gateway routes by it.
//!
//! Kept on disk beside the address, so the same computer is the same computer
//! after a restart. When it cannot be (a read-only home directory, a first run
//! that races itself) a fresh one is minted for this run instead: the identity
//! only has to be stable while the application is open, because it is registered
//! when the link opens and forgotten when it closes.
//!
//! Shared by both editions, because an installation is an installation: the
//! personal one is a desktop application on somebody's computer exactly as the
//! enterprise one is, and a tool that runs on that computer needs to know which
//! computer it is either way.

use std::fs;

use crate::workspace;

/// The file that remembers it, beside the server address.
const FILE: &str = "device.json";

/// This installation's id: the one on disk, or a new one written there, or a
/// fresh one for this run if the disk will not have it.
pub fn id() -> String {
    if let Some(saved) = read() {
        return saved;
    }
    let minted = mint();
    let _ = write(&minted);
    minted
}

fn read() -> Option<String> {
    let path = workspace::state_dir()?.join(FILE);
    let raw = fs::read_to_string(path).ok()?;
    let value: serde_json::Value = serde_json::from_str(&raw).ok()?;
    let id = value.get("device_id")?.as_str()?.trim().to_string();
    if id.is_empty() {
        return None;
    }
    Some(id)
}

fn write(id: &str) -> Result<(), String> {
    let dir = workspace::state_dir().ok_or("this installation has no state directory")?;
    fs::create_dir_all(&dir).map_err(|e| format!("{e}"))?;
    let body = serde_json::json!({ "device_id": id }).to_string();
    fs::write(dir.join(FILE), body).map_err(|e| format!("{e}"))
}

/// mint makes an identifier that is this installation's and nobody else's.
///
/// Random, not derived from anything about the machine. A name, a serial number
/// or a hardware address would identify the COMPUTER, which is more than is
/// needed here and is somebody's information; what this has to do is tell two
/// installations apart.
///
/// And it must never return the same thing twice, which is why the failure of
/// the random source is handled rather than ignored. Written as
/// `getrandom::fill(&mut bytes).ok()`, a failure leaves the buffer as it was
/// declared: every installation that hit it would call itself
/// `00000000000000000000000000000000`, two of a person's computers would be one
/// machine in the registry, and each would quietly replace the other. A silent
/// collision is the worst possible answer here, because the symptom is a tool
/// reaching the wrong computer and nothing at all in the logs.
fn mint() -> String {
    let mut bytes = [0u8; 16];
    if getrandom::fill(&mut bytes).is_ok() {
        return hex(&bytes);
    }
    // No entropy. Anything still unique is better than a constant: this run, on
    // this machine, at this moment. Two of them would have to be the same
    // process started in the same nanosecond.
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or_default();
    format!("{:032x}", now ^ (u128::from(std::process::id()) << 96))
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    // Two installations must never call themselves the same thing. The registry
    // keys on this, so a repeat means one person's two computers are one
    // machine and each replaces the other, with a tool reaching whichever
    // connected last: the failure this whole identifier exists to remove.
    #[test]
    fn an_identifier_is_never_the_same_twice() {
        let mut seen = std::collections::HashSet::new();
        for _ in 0..1000 {
            assert!(seen.insert(mint()), "an identifier was minted twice");
        }
    }

    // And never the empty answer a zeroed buffer would give.
    #[test]
    fn an_identifier_is_never_nothing() {
        for _ in 0..100 {
            let id = mint();
            assert_eq!(id.len(), 32, "an identifier is 32 hex characters: {id}");
            assert!(id.chars().all(|c| c.is_ascii_hexdigit()), "not hex: {id}");
            assert_ne!(id, "0".repeat(32), "a zeroed buffer became an identifier");
        }
    }
}
