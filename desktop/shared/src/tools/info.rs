//! What computer this is.
//!
//! The first machine tool, and deliberately the least interesting one: it reads
//! nothing, changes nothing, and needs no permission from anybody. What it is
//! for is proving the whole path — a model asks, the gateway routes to the
//! right installation, this half answers, the answer reaches the conversation —
//! with nothing at stake if any of it is wrong.

use serde_json::{json, Value};

use super::Response;

pub const NAME: &str = "machine_info";

/// The version of this tool's arguments, not of the application. It changes
/// when the shape does, and a gateway that speaks a different one does not
/// offer the tool.
pub const VERSION: i64 = 1;

pub async fn run(_args: Value) -> Response {
    Response::ok(json!({
        "operating_system": std::env::consts::OS,
        "architecture": std::env::consts::ARCH,
        // The name a person would recognise, not an identifier: this is what
        // the assistant says back when somebody asks which computer it can see.
        "name": hostname(),
    }))
}

/// hostname is what this machine calls itself, or an honest blank.
///
/// Read from the environment rather than a system call, because the answer is
/// for a sentence in a conversation and not for identifying anything: nothing
/// depends on it being unique or even present.
fn hostname() -> String {
    for key in ["HOSTNAME", "COMPUTERNAME", "NAME"] {
        if let Ok(name) = std::env::var(key) {
            let name = name.trim();
            if !name.is_empty() {
                return name.to_string();
            }
        }
    }
    String::new()
}
