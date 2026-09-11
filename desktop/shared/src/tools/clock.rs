//! What time it is where the PERSON is.
//!
//! The gateway has a clock and it is the wrong one. It runs in UTC on a machine
//! in a rack, and an assistant grounded in that tells somebody in Tirana that it
//! is nine in the morning when their own screen says eleven, then cannot convert
//! it because a server does not know where they are sitting.
//!
//! The computer the person is using does know. It knows its zone by name, which
//! is the thing that survives a daylight-saving change, and it knows its offset
//! right now. So the clock is read here, in the application, and the answer says
//! where it was read.

use serde_json::{json, Value};

use super::Response;

pub const NAME: &str = "current_time";

/// The version of this tool's arguments, not of the application. It changes
/// when the shape does, and a gateway that speaks a different one does not
/// offer the tool.
pub const VERSION: i64 = 1;

pub async fn run(_args: Value) -> Response {
    Response::ok(now(chrono::Local::now(), iana_time_zone::get_timezone().ok()))
}

/// now describes one instant as the person's own computer sees it.
///
/// Split from `run` so it can be tested against a fixed instant: a test that
/// reads the real clock and asserts anything about it is asserting the machine
/// it happens to run on.
fn now(local: chrono::DateTime<chrono::Local>, zone: Option<String>) -> Value {
    let mut answer = json!({
        // Local FIRST, because it is the answer to the question. The offset is
        // part of the timestamp rather than a separate field to be recombined.
        "local": local.format("%Y-%m-%dT%H:%M:%S%:z").to_string(),
        "weekday": local.format("%A").to_string(),
        // UTC alongside it, since anything the assistant computes across
        // machines wants the instant rather than the reading.
        "utc": local.with_timezone(&chrono::Utc).format("%Y-%m-%dT%H:%M:%SZ").to_string(),
    });
    // The zone BY NAME when the system will say ("Europe/Tirane"), which is what
    // makes "the same time next Tuesday" survive a daylight-saving change. A
    // machine that will not say keeps its offset, which is still true.
    if let Some(zone) = zone.filter(|z| !z.trim().is_empty()) {
        answer["zone"] = Value::from(zone);
    }
    answer
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;

    /// The local reading, the instant, and the zone are three different things,
    /// and the bug this tool exists for was answering only the third.
    #[test]
    fn it_answers_where_the_person_is() {
        let local = chrono::Local.timestamp_opt(1_788_512_116, 0).unwrap();
        let answer = now(local, Some("Europe/Tirane".to_string()));

        assert_eq!(answer["zone"], "Europe/Tirane");
        // Whatever this machine's zone is, the local reading carries an offset
        // and the UTC one carries Z: the two are never confusable.
        let reading = answer["local"].as_str().unwrap();
        assert!(
            reading.contains('+') || reading.contains("-0") || reading.ends_with("+00:00"),
            "the local reading carries no offset: {reading}"
        );
        assert!(answer["utc"].as_str().unwrap().ends_with('Z'));
        assert_eq!(answer["utc"], "2026-09-04T08:55:16Z");
    }

    /// A machine that will not name its zone still answers, because an offset
    /// is a true thing to say and refusing would leave the person with nothing.
    #[test]
    fn a_machine_that_names_no_zone_still_answers() {
        let answer = now(chrono::Local.timestamp_opt(1_788_512_116, 0).unwrap(), None);
        assert!(answer.get("zone").is_none());
        assert!(answer["local"].as_str().is_some());
    }
}
