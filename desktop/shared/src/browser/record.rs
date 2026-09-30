//! What a page did while nobody was asking.
//!
//! Three of Playwright's tools report things that have ALREADY happened: what
//! the console said, what the page fetched, and whether a dialog is waiting.
//! None of them can be answered by asking the page at the moment the question
//! arrives, because by then the console line has been printed, the request has
//! finished, and a dialog that opened has been blocking the page for a while.
//! So a page records as it goes, and these answer from the record.
//!
//! **One task per page, subscribed to the whole connection.** Events arrive on
//! one socket for every page (sessions are flattened, see `cdp`), so each
//! recorder filters by its own session. It keeps no lock across an await and
//! never speaks back to the browser, so it cannot deadlock with a tool call
//! going the other way.
//!
//! **The buffers are bounded and say when they dropped something.** A page left
//! open on a dashboard that polls every second would otherwise grow until the
//! application ran out of memory. What is dropped is the OLDEST, and the count
//! is kept, because an answer that quietly omits the first hour is worse than
//! one that says how much it is missing.
//!
//! **A request's body is not kept.** It is fetched from the browser on demand
//! (`Network.getResponseBody`), because bodies are the large part and most are
//! never asked for. The cost is that a body can be gone by the time it is
//! wanted, which is reported as what it is rather than as an empty body.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use serde_json::Value;

use super::cdp::{Connection, Event};

/// How many console lines a page keeps.
const MOST_CONSOLE: usize = 1_000;

/// How many network exchanges a page keeps.
const MOST_REQUESTS: usize = 500;

/// The four levels Playwright orders console messages by, most severe first.
///
/// Their `consoleMessageLevels`. The order is the whole of the filter: asking
/// for "warning" returns warnings AND errors, because their rule is
/// `indexOf(level) <= indexOf(threshold)`.
pub const LEVELS: [&str; 4] = ["error", "warning", "info", "debug"];

/// Whether a message at this level is included when asking for that one.
///
/// Their `shouldIncludeMessage`. An unknown threshold behaves as their default
/// does, which is "info".
pub fn included(threshold: &str, level: &str) -> bool {
    let rank = |name: &str| LEVELS.iter().position(|l| *l == name);
    let wanted = rank(threshold).unwrap_or(2);
    rank(level).unwrap_or(2) <= wanted
}

/// Playwright's `consoleLevelForMessageType`, for the console API's own names.
fn level_of(kind: &str) -> &'static str {
    match kind {
        "assert" | "error" => "error",
        "warning" => "warning",
        "count" | "dir" | "dirxml" | "info" | "log" | "table" | "time" | "timeEnd" => "info",
        "clear"
        | "debug"
        | "endGroup"
        | "profile"
        | "profileEnd"
        | "startGroup"
        | "startGroupCollapsed"
        | "trace" => "debug",
        _ => "info",
    }
}

/// The same, for the browser's own log, whose levels are named differently.
fn level_of_entry(level: &str) -> &'static str {
    match level {
        "error" => "error",
        "warning" => "warning",
        "verbose" => "debug",
        _ => "info",
    }
}

/// One line the console printed.
#[derive(Debug, Clone)]
pub struct Line {
    pub level: &'static str,
    /// The console method that produced it (`log`, `error`, `warn`), or the
    /// part of the browser that did.
    pub kind: String,
    pub text: String,
    /// Where it came from, as `url:line`, when the browser said.
    pub at: String,
    /// Which page load it belongs to, for their `all` option.
    pub generation: u64,
}

/// One request the page made, and what came back.
#[derive(Debug, Clone)]
pub struct Exchange {
    /// 1-based and monotonic, which is what `browser_network_request`'s index
    /// means: "as printed by browser_network_requests". Theirs is a position in
    /// the list; ours is minted when the request starts, so the number stays
    /// the same after older entries are dropped instead of sliding onto a
    /// different request.
    pub index: u64,
    pub generation: u64,
    pub method: String,
    pub url: String,
    /// Playwright's resourceType: Document, Stylesheet, Image, Script, XHR,
    /// Fetch, Font and so on. Their static filter is written in terms of it.
    pub resource: String,
    pub request_headers: Value,
    pub post_data: Option<String>,
    pub status: Option<i64>,
    pub status_text: String,
    pub response_headers: Value,
    pub mime: String,
    pub failure: Option<String>,
    /// What the browser calls this request, for fetching its body later.
    pub id: String,
}

impl Exchange {
    /// Their `isFetch`: what the page asked for itself, rather than what the
    /// browser fetched to render it.
    pub fn is_fetch(&self) -> bool {
        self.resource.eq_ignore_ascii_case("fetch") || self.resource.eq_ignore_ascii_case("xhr")
    }

    /// Their `isSuccessfulResponse`.
    pub fn succeeded(&self) -> bool {
        self.failure.is_none() && self.status.is_some_and(|status| status < 400)
    }
}

/// Something the page is waiting on, which blocks everything else.
///
/// Playwright's "modal state", and their rule is copied exactly: a tool that
/// clears one may only run while it is present, and every other tool refuses
/// while it is. That is not strictness for its own sake. A page showing an
/// `alert()` runs no JavaScript until it is answered, so a snapshot taken then
/// does not come back slowly, it does not come back at all.
#[derive(Debug, Clone)]
pub enum Modal {
    /// `alert`, `confirm`, `prompt` or `beforeunload`.
    Dialog {
        kind: String,
        message: String,
        default_prompt: String,
    },
    /// A file input was clicked. The browser is holding it open for us.
    FileChooser { node: i64, multiple: bool },
}

impl Modal {
    /// What to call it when telling somebody one is in the way.
    pub fn describe(&self) -> String {
        match self {
            Modal::Dialog { kind, message, .. } => {
                format!("a {kind} dialog saying {message:?}")
            }
            Modal::FileChooser { .. } => "a file chooser".to_string(),
        }
    }

    /// Which action answers it.
    pub fn answered_by(&self) -> &'static str {
        match self {
            Modal::Dialog { .. } => "handle_dialog",
            Modal::FileChooser { .. } => "file_upload",
        }
    }
}

/// Everything one page has been seen to do.
#[derive(Clone)]
pub struct Records {
    inner: Arc<Inner>,
}

impl std::fmt::Debug for Records {
    /// Deliberately says nothing. What this holds is the contents of somebody's
    /// console and the addresses their page fetched, which is not something a
    /// log line should be able to print by accident.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Records(page)")
    }
}

#[derive(Default)]
struct Kept {
    console: Vec<Line>,
    /// How many console lines were dropped to stay under the ceiling.
    console_dropped: u64,
    requests: Vec<Exchange>,
    requests_dropped: u64,
    /// Requests that have started and not yet been matched to a response, by
    /// the browser's own id.
    open: HashMap<String, usize>,
    modal: Option<Modal>,
    generation: u64,
    next_index: u64,
    /// Events the recorder fell behind by, which is the one way it can be
    /// incomplete without knowing what it missed.
    missed: u64,
}

struct Inner {
    kept: Mutex<Kept>,
    held: Mutex<Option<tauri::async_runtime::JoinHandle<()>>>,
}

impl Drop for Inner {
    /// The page has gone, so the recorder goes with it. Without this the task
    /// outlives every page ever opened and keeps a copy of its console.
    fn drop(&mut self) {
        if let Some(task) = self.held.lock().ok().and_then(|mut h| h.take()) {
            task.abort();
        }
    }
}

impl Records {
    /// Start recording one page.
    ///
    /// The caller has already enabled the domains this listens to. Starting the
    /// task here rather than there is what makes it impossible to have a page
    /// whose events nobody is keeping.
    pub fn start(browser: &Connection, session: &str) -> Self {
        let inner = Arc::new(Inner {
            kept: Mutex::new(Kept {
                next_index: 1,
                ..Kept::default()
            }),
            held: Mutex::new(None),
        });

        let mut listening = browser.listen();
        let session = session.to_string();
        let writing = Arc::downgrade(&inner);
        let task = tauri::async_runtime::spawn(async move {
            loop {
                match listening.recv().await {
                    Ok(event) => {
                        let Some(inner) = writing.upgrade() else {
                            return;
                        };
                        if event.session.as_deref() != Some(session.as_str()) {
                            continue;
                        }
                        inner.take(&event);
                    }
                    Err(tokio::sync::broadcast::error::RecvError::Lagged(by)) => {
                        let Some(inner) = writing.upgrade() else {
                            return;
                        };
                        inner.lagged(by);
                    }
                    Err(_) => return,
                }
            }
        });
        *inner.held.lock().unwrap_or_else(|e| e.into_inner()) = Some(task);

        Self { inner }
    }

    /// The console lines at or above this level, optionally from before the
    /// last page load.
    pub fn console(&self, threshold: &str, all: bool) -> (Vec<Line>, u64) {
        let kept = self.inner.kept.lock().unwrap_or_else(|e| e.into_inner());
        let lines = kept
            .console
            .iter()
            .filter(|line| all || line.generation == kept.generation)
            .filter(|line| included(threshold, line.level))
            .cloned()
            .collect();
        (lines, kept.console_dropped)
    }

    /// Every request since the page loaded, oldest first.
    pub fn requests(&self, all: bool) -> (Vec<Exchange>, u64) {
        let kept = self.inner.kept.lock().unwrap_or_else(|e| e.into_inner());
        let requests = kept
            .requests
            .iter()
            .filter(|exchange| all || exchange.generation == kept.generation)
            .cloned()
            .collect();
        (requests, kept.requests_dropped)
    }

    /// One request by the number `requests` printed.
    pub fn request(&self, index: u64) -> Option<Exchange> {
        let kept = self.inner.kept.lock().unwrap_or_else(|e| e.into_inner());
        kept.requests
            .iter()
            .find(|exchange| exchange.index == index)
            .cloned()
    }

    /// What the page is waiting on, if anything.
    pub fn modal(&self) -> Option<Modal> {
        self.inner
            .kept
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .modal
            .clone()
    }

    /// Forget the modal state, having dealt with it.
    ///
    /// Taken rather than cleared: the caller needs what was there (a file
    /// chooser carries the element to put the files on) and nobody else may
    /// answer the same dialog twice.
    pub fn take_modal(&self) -> Option<Modal> {
        self.inner
            .kept
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .modal
            .take()
    }

    /// How many events the recorder fell behind by, which is the one gap it
    /// cannot describe.
    pub fn missed(&self) -> u64 {
        self.inner
            .kept
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .missed
    }
}

impl Inner {
    fn lagged(&self, by: u64) {
        self.kept.lock().unwrap_or_else(|e| e.into_inner()).missed += by;
    }

    /// take records one event, if it is one of the ones worth keeping.
    fn take(&self, event: &Event) {
        let mut kept = self.kept.lock().unwrap_or_else(|e| e.into_inner());
        let p = &event.params;
        match event.method.as_str() {
            // A new page load. Everything before it is still kept, and their
            // `all` option is what reaches it.
            "Page.frameNavigated" => {
                let sub_frame = p.get("frame").and_then(|f| f.get("parentId")).is_some();
                if !sub_frame {
                    kept.generation += 1;
                }
            }

            "Runtime.consoleAPICalled" => {
                let kind = text_at(p, "type").unwrap_or("log").to_string();
                let line = Line {
                    level: level_of(&kind),
                    kind,
                    text: console_text(p),
                    at: source_of(p.get("stackTrace")),
                    generation: kept.generation,
                };
                push_console(&mut kept, line);
            }

            // An uncaught error. Always an error, and the one console entry a
            // page never prints for itself.
            "Runtime.exceptionThrown" => {
                let details = p.get("exceptionDetails");
                let text = details
                    .and_then(|d| d.get("exception"))
                    .and_then(|e| e.get("description"))
                    .and_then(Value::as_str)
                    .or_else(|| details.and_then(|d| d.get("text")).and_then(Value::as_str))
                    .unwrap_or("an error with no message")
                    .to_string();
                let at = match (
                    details.and_then(|d| d.get("url")).and_then(Value::as_str),
                    details
                        .and_then(|d| d.get("lineNumber"))
                        .and_then(Value::as_i64),
                ) {
                    (Some(url), Some(line)) => format!("{url}:{}", line + 1),
                    (Some(url), None) => url.to_string(),
                    _ => String::new(),
                };
                let line = Line {
                    level: "error",
                    kind: "pageerror".into(),
                    text,
                    at,
                    generation: kept.generation,
                };
                push_console(&mut kept, line);
            }

            // The browser's own log, which is where a blocked request, a
            // certificate problem or a content-policy refusal appears. A page
            // never prints these and they are usually the answer.
            "Log.entryAdded" => {
                let entry = p.get("entry");
                let level = entry
                    .and_then(|e| e.get("level"))
                    .and_then(Value::as_str)
                    .unwrap_or("info");
                let line = Line {
                    level: level_of_entry(level),
                    kind: entry
                        .and_then(|e| e.get("source"))
                        .and_then(Value::as_str)
                        .unwrap_or("browser")
                        .to_string(),
                    text: entry
                        .and_then(|e| e.get("text"))
                        .and_then(Value::as_str)
                        .unwrap_or_default()
                        .to_string(),
                    at: match (
                        entry.and_then(|e| e.get("url")).and_then(Value::as_str),
                        entry
                            .and_then(|e| e.get("lineNumber"))
                            .and_then(Value::as_i64),
                    ) {
                        (Some(url), Some(line)) => format!("{url}:{}", line + 1),
                        (Some(url), None) => url.to_string(),
                        _ => String::new(),
                    },
                    generation: kept.generation,
                };
                push_console(&mut kept, line);
            }

            "Network.requestWillBeSent" => {
                let Some(id) = text_at(p, "requestId").map(str::to_string) else {
                    return;
                };
                let request = p.get("request");
                let index = kept.next_index;
                kept.next_index += 1;
                let exchange = Exchange {
                    index,
                    generation: kept.generation,
                    method: request
                        .and_then(|r| r.get("method"))
                        .and_then(Value::as_str)
                        .unwrap_or("GET")
                        .to_string(),
                    url: request
                        .and_then(|r| r.get("url"))
                        .and_then(Value::as_str)
                        .unwrap_or_default()
                        .to_string(),
                    resource: text_at(p, "type").unwrap_or("Other").to_string(),
                    request_headers: request
                        .and_then(|r| r.get("headers"))
                        .cloned()
                        .unwrap_or(Value::Null),
                    post_data: request
                        .and_then(|r| r.get("postData"))
                        .and_then(Value::as_str)
                        .map(str::to_string),
                    status: None,
                    status_text: String::new(),
                    response_headers: Value::Null,
                    mime: String::new(),
                    failure: None,
                    id: id.clone(),
                };
                push_request(&mut kept, id, exchange);
            }

            "Network.responseReceived" => {
                let Some(at) = kept
                    .open
                    .get(text_at(p, "requestId").unwrap_or(""))
                    .copied()
                else {
                    return;
                };
                let response = p.get("response");
                let Some(exchange) = kept.requests.get_mut(at) else {
                    return;
                };
                exchange.status = response
                    .and_then(|r| r.get("status"))
                    .and_then(Value::as_i64);
                exchange.status_text = response
                    .and_then(|r| r.get("statusText"))
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_string();
                exchange.response_headers = response
                    .and_then(|r| r.get("headers"))
                    .cloned()
                    .unwrap_or(Value::Null);
                exchange.mime = response
                    .and_then(|r| r.get("mimeType"))
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_string();
                // The resource type is more accurate here than on the request:
                // the browser often does not know what it is fetching until it
                // sees what came back.
                if let Some(kind) = text_at(p, "type") {
                    exchange.resource = kind.to_string();
                }
            }

            "Network.loadingFailed" => {
                let Some(at) = kept
                    .open
                    .get(text_at(p, "requestId").unwrap_or(""))
                    .copied()
                else {
                    return;
                };
                let why = text_at(p, "errorText")
                    .unwrap_or("Unknown error")
                    .to_string();
                let cancelled = p.get("canceled").and_then(Value::as_bool).unwrap_or(false);
                if let Some(exchange) = kept.requests.get_mut(at) {
                    exchange.failure = Some(if cancelled {
                        format!("{why} (cancelled)")
                    } else {
                        why
                    });
                }
            }

            // A dialog. The page is now blocked, which is why this is recorded
            // rather than merely noticed.
            "Page.javascriptDialogOpening" => {
                kept.modal = Some(Modal::Dialog {
                    kind: text_at(p, "type").unwrap_or("alert").to_string(),
                    message: text_at(p, "message").unwrap_or_default().to_string(),
                    default_prompt: text_at(p, "defaultPrompt").unwrap_or_default().to_string(),
                });
            }
            // Answered, by us or by the page itself.
            "Page.javascriptDialogClosed" => {
                if matches!(kept.modal, Some(Modal::Dialog { .. })) {
                    kept.modal = None;
                }
            }

            // A file input was clicked and the browser is holding the chooser
            // open, because we asked it to intercept them.
            "Page.fileChooserOpened" => {
                kept.modal = Some(Modal::FileChooser {
                    node: p
                        .get("backendNodeId")
                        .and_then(Value::as_i64)
                        .unwrap_or_default(),
                    multiple: text_at(p, "mode") == Some("selectMultiple"),
                });
            }

            _ => {}
        }
    }
}

fn text_at<'a>(params: &'a Value, key: &str) -> Option<&'a str> {
    params.get(key).and_then(Value::as_str)
}

/// The words a console call printed.
///
/// The browser sends each argument as a described object rather than as text,
/// so `console.log('a', 1, {b: 2})` arrives as three of them. `value` covers
/// the plain ones, `description` covers objects and errors, and
/// `unserializableValue` covers the handful that are neither (`NaN`,
/// `Infinity`).
fn console_text(params: &Value) -> String {
    let Some(args) = params.get("args").and_then(Value::as_array) else {
        return String::new();
    };
    args.iter()
        .map(|arg| {
            if let Some(value) = arg.get("value") {
                return match value {
                    Value::String(text) => text.clone(),
                    other => other.to_string(),
                };
            }
            arg.get("description")
                .or_else(|| arg.get("unserializableValue"))
                .and_then(Value::as_str)
                .unwrap_or_else(|| arg.get("type").and_then(Value::as_str).unwrap_or(""))
                .to_string()
        })
        .collect::<Vec<_>>()
        .join(" ")
}

/// Where a console call was made, as `url:line`.
fn source_of(stack: Option<&Value>) -> String {
    let Some(frame) = stack
        .and_then(|s| s.get("callFrames"))
        .and_then(Value::as_array)
        .and_then(|frames| frames.first())
    else {
        return String::new();
    };
    let url = frame.get("url").and_then(Value::as_str).unwrap_or_default();
    match frame.get("lineNumber").and_then(Value::as_i64) {
        // The protocol counts from zero and every editor counts from one.
        Some(line) => format!("{url}:{}", line + 1),
        None => url.to_string(),
    }
}

fn push_console(kept: &mut Kept, line: Line) {
    if kept.console.len() >= MOST_CONSOLE {
        kept.console.remove(0);
        kept.console_dropped += 1;
    }
    kept.console.push(line);
}

fn push_request(kept: &mut Kept, id: String, exchange: Exchange) {
    if kept.requests.len() >= MOST_REQUESTS {
        let gone = kept.requests.remove(0);
        kept.requests_dropped += 1;
        kept.open.retain(|_, at| {
            if *at == 0 {
                return false;
            }
            *at -= 1;
            true
        });
        let _ = gone;
    }
    kept.open.insert(id, kept.requests.len());
    kept.requests.push(exchange);
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Their level ordering: asking for one level includes the severer ones.
    #[test]
    fn a_level_includes_everything_more_severe() {
        assert!(included("info", "error"));
        assert!(included("info", "warning"));
        assert!(included("info", "info"));
        assert!(!included("info", "debug"), "info should not include debug");

        assert!(included("error", "error"));
        assert!(!included("error", "warning"), "error is the narrowest");

        assert!(included("debug", "debug"), "debug includes everything");
        assert!(included("debug", "error"));

        // Their default when the threshold is not one of the four.
        assert!(included("nonsense", "info") && !included("nonsense", "debug"));
    }

    /// The console API's many names map onto four levels, their way.
    #[test]
    fn every_console_method_lands_on_one_of_the_four_levels() {
        assert_eq!(level_of("error"), "error");
        assert_eq!(level_of("assert"), "error");
        assert_eq!(level_of("warning"), "warning");
        assert_eq!(level_of("log"), "info");
        assert_eq!(level_of("table"), "info");
        assert_eq!(level_of("trace"), "debug");
        assert_eq!(level_of("startGroup"), "debug");
        // Anything they do not list is info, which is their default arm.
        assert_eq!(level_of("somethingNew"), "info");

        // And the browser's own log, whose names are not the same words.
        assert_eq!(level_of_entry("verbose"), "debug");
        assert_eq!(level_of_entry("error"), "error");
        assert_eq!(level_of_entry("info"), "info");
    }

    /// A console call's arguments become the words it printed.
    #[test]
    fn the_arguments_of_a_console_call_become_its_text() {
        let params = serde_json::json!({
            "type": "log",
            "args": [
                {"type": "string", "value": "hello"},
                {"type": "number", "value": 42},
                {"type": "object", "description": "Object"},
                {"type": "number", "unserializableValue": "NaN"}
            ]
        });
        assert_eq!(console_text(&params), "hello 42 Object NaN");

        // A string keeps its own characters rather than being re-quoted, which
        // is what would happen if the value were printed as JSON.
        let quoted = serde_json::json!({"args": [{"value": "a \"b\" c"}]});
        assert_eq!(console_text(&quoted), "a \"b\" c");

        assert_eq!(console_text(&serde_json::json!({})), "");
    }

    /// A line says where it came from, counting from one.
    #[test]
    fn a_console_line_carries_its_place_in_the_source() {
        let stack = serde_json::json!({
            "callFrames": [{"url": "https://example.com/app.js", "lineNumber": 11}]
        });
        assert_eq!(source_of(Some(&stack)), "https://example.com/app.js:12");
        assert_eq!(source_of(None), "");
        assert_eq!(source_of(Some(&serde_json::json!({}))), "");
    }

    /// Their static filter: what the page asked for, against what the browser
    /// fetched to draw it.
    #[test]
    fn a_fetch_is_told_from_a_picture() {
        let of = |resource: &str, status: Option<i64>, failure: Option<&str>| Exchange {
            index: 1,
            generation: 0,
            method: "GET".into(),
            url: "https://example.com".into(),
            resource: resource.into(),
            request_headers: Value::Null,
            post_data: None,
            status,
            status_text: String::new(),
            response_headers: Value::Null,
            mime: String::new(),
            failure: failure.map(str::to_string),
            id: "1".into(),
        };
        assert!(of("Fetch", Some(200), None).is_fetch());
        assert!(of("XHR", Some(200), None).is_fetch());
        assert!(!of("Image", Some(200), None).is_fetch());
        assert!(!of("Script", Some(200), None).is_fetch());

        assert!(of("Image", Some(200), None).succeeded());
        assert!(of("Image", Some(304), None).succeeded());
        assert!(!of("Image", Some(404), None).succeeded());
        assert!(!of("Image", Some(500), None).succeeded());
        assert!(
            !of("Image", None, None).succeeded(),
            "unfinished is not success"
        );
        assert!(!of("Image", Some(200), Some("net::ERR_ABORTED")).succeeded());
    }

    /// A modal state says what it is and what answers it.
    #[test]
    fn a_modal_state_names_the_action_that_clears_it() {
        let dialog = Modal::Dialog {
            kind: "confirm".into(),
            message: "Delete everything?".into(),
            default_prompt: String::new(),
        };
        assert!(dialog.describe().contains("confirm"));
        assert!(dialog.describe().contains("Delete everything?"));
        assert_eq!(dialog.answered_by(), "handle_dialog");

        let chooser = Modal::FileChooser {
            node: 7,
            multiple: true,
        };
        assert_eq!(chooser.answered_by(), "file_upload");
        assert!(chooser.describe().contains("file chooser"));
    }
}
