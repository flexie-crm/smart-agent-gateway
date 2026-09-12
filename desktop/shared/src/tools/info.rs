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
    // Read from `environment`, which is also what the application sends with
    // every message for the system prompt. One source, so the paragraph the
    // assistant was given and the answer it gets from asking cannot disagree
    // about which computer this is. The SHAPE here is the tool's contract and
    // is unchanged: a different shape would be a new VERSION, and a gateway
    // speaking the old one would stop offering the tool.
    Response::ok(json!({
        "operating_system": crate::environment::os(),
        "architecture": crate::environment::arch(),
        // The name a person would recognise, not an identifier: this is what
        // the assistant says back when somebody asks which computer it can see.
        "name": crate::environment::hostname(),
    }))
}
