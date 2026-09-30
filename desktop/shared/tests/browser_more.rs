//! The eleven actions beyond the first nine, against a real page.
//!
//! Its own file rather than more of `browser_tools.rs`, because these need
//! things that one does not: a real HTTP server (a `file://` page makes no
//! network requests worth listing and cannot fetch anything), a real file on
//! disk to upload, and a page that takes its time.
//!
//! Driven through `tools::run`, which is the dispatcher the link calls, so what
//! is exercised is the whole path: the arguments as they arrive, the handler,
//! Playwright's script, the protocol, and the shape of the answer.

use std::io::{Read, Write};
use std::path::PathBuf;

use sag_desktop::browser;
use sag_desktop::tools::{self, Request, Response};
use serde_json::{json, Value};

/// The conversation these calls belong to. Its own, so the pages this opens are
/// not the ones `browser_tools.rs` is working in.
const WHOSE: i64 = 7171;

/// One page with everything the eleven need.
const PAGE: &str = r##"<!doctype html><meta charset="utf-8"><title>More</title>
<style>
  #zone { width: 200px; height: 80px; border: 2px dashed #999 }
  #box { width: 60px; height: 40px; background: #ccc }
  #later { display: none }
</style>
<h1>More</h1>

<form id="form">
  <label for="who">Your name</label><input id="who" value="old">
  <label for="agree">I agree</label><input id="agree" type="checkbox">
  <label for="post">By post</label><input id="post" type="radio" name="how">
  <label for="pick">Pick one</label>
  <select id="pick">
    <!-- A decoy whose VALUE is another option's label. It is what tells the two
         matching rules apart: their fill_form picks by the label a person
         reads, so "Three" must reach the last option and not this one. -->
    <option value="Three">Decoy</option>
    <option value="1">One</option>
    <option value="2">Two</option>
    <option value="3">Three</option>
  </select>
  <label for="level">Level</label><input id="level" type="range" min="0" max="10" value="0">
</form>

<div id="box" draggable="true">drag me</div>
<div id="zone">drop here</div>
<p id="dropped">no drop yet</p>

<input id="file" type="file">
<p id="chosen">no file</p>

<p id="soon">not yet</p>
<p id="later">it arrived</p>
<p id="size">?</p>

<p id="alerts"><button id="ask">Ask</button></p>

<script>
// Drag between two elements on the page.
const box = document.getElementById('box'), zone = document.getElementById('zone');
box.addEventListener('dragstart', e => e.dataTransfer.setData('text/plain', 'the box'));
zone.addEventListener('dragover', e => e.preventDefault());
zone.addEventListener('drop', e => {
  e.preventDefault();
  const files = e.dataTransfer.files.length;
  const text = e.dataTransfer.getData('text/plain');
  document.getElementById('dropped').textContent =
    'dropped files=' + files + ' text=' + text;
});
// A mouse drag with no HTML5 drag events still moves things on plenty of
// pages, so record that too.
let down = false;
box.addEventListener('mousedown', () => { down = true; });
zone.addEventListener('mouseup', () => {
  if (down) document.getElementById('dropped').textContent = 'dropped by mouse';
  down = false;
});

document.getElementById('file').addEventListener('change', e => {
  document.getElementById('chosen').textContent =
    'chose ' + [...e.target.files].map(f => f.name).join(', ');
});

// Something that takes a moment, and something that goes away.
setTimeout(() => {
  document.getElementById('soon').style.display = 'none';
  document.getElementById('later').style.display = 'block';
}, 700);

function showSize() { document.getElementById('size').textContent = window.innerWidth + 'x' + window.innerHeight; }
showSize();
window.addEventListener('resize', showSize);

document.getElementById('ask').addEventListener('click', () => {
  const said = confirm('Really?');
  document.getElementById('alerts').textContent = 'answered ' + said;
});

// What the console has to say, one of each level.
console.log('a plain line');
console.info('an informative line');
console.warn('a warning line');
console.error('an error line');
console.debug('a quiet line');
setTimeout(() => { undefinedFunctionSomewhere(); }, 50);

// And some traffic: one the page asked for, one that fails, one picture.
fetch('/api/thing').then(r => r.json()).then(() => {});
fetch('/api/missing').then(() => {});
const img = new Image(); img.src = '/picture.png';
</script>
"##;

/// A web server, because a `file://` page cannot fetch anything.
///
/// Thirty lines rather than a dependency. What the network actions need is a
/// real request with a real status and a real body, and three fixed answers is
/// the whole of that: one that works and returns JSON, one that is missing, and
/// one picture, which is what the static filter is about.
fn serve() -> (String, std::thread::JoinHandle<()>) {
    let listening =
        std::net::TcpListener::bind("127.0.0.1:0").expect("a port for the fixture server");
    let at = format!("http://{}", listening.local_addr().expect("its address"));

    let held = std::thread::spawn(move || {
        for stream in listening.incoming() {
            let Ok(mut stream) = stream else { continue };
            let mut head = [0u8; 2048];
            let read = stream.read(&mut head).unwrap_or(0);
            let asked = String::from_utf8_lossy(&head[..read]).to_string();
            let path = asked.split_whitespace().nth(1).unwrap_or("/").to_string();

            let (status, kind, body): (&str, &str, Vec<u8>) = match path.as_str() {
                "/api/thing" => ("200 OK", "application/json", br#"{"answer":42}"#.to_vec()),
                "/api/missing" => ("404 Not Found", "text/plain", b"nope".to_vec()),
                // The smallest real PNG there is, so the browser calls it an
                // image rather than something that failed.
                "/picture.png" => (
                    "200 OK",
                    "image/png",
                    vec![
                        0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
                        0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
                        0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00,
                        0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
                        0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49,
                        0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
                    ],
                ),
                // A site that decides on LOAD whether you are signed in,
                // which is the shape that makes the restore ordering matter.
                "/app" => (
                    "200 OK",
                    "text/html; charset=utf-8",
                    br#"<!doctype html><title>App</title><p id=who>?</p><script>
                    var jar = document.cookie.match(/sid=([^;]*)/);
                    var tok = localStorage.getItem('jwt');
                    document.getElementById('who').textContent =
                      (jar || tok) ? ('signed in ' + (jar ? jar[1] : '') + ' ' + (tok || '')) : 'SIGNED OUT';
                    </script>"#.to_vec(),
                ),
                "/quit" => return,
                _ => (
                    "200 OK",
                    "text/html; charset=utf-8",
                    PAGE.as_bytes().to_vec(),
                ),
            };
            let head = format!(
                "HTTP/1.1 {status}\r\nContent-Type: {kind}\r\nContent-Length: {}\r\n\
                 Connection: close\r\n\r\n",
                body.len()
            );
            let _ = stream.write_all(head.as_bytes());
            let _ = stream.write_all(&body);
            let _ = stream.flush();
        }
    });
    (at, held)
}

fn scratch() -> PathBuf {
    std::env::temp_dir().join(format!("sag-bmore-{}", std::process::id()))
}

/// The browser, linked into a scratch state directory. Same reasoning as
/// `browser_tools.rs`: the gate is given one rather than downloading a hundred
/// megabytes on every run.
fn set_up() -> PathBuf {
    let given = std::env::var("SAG_BROWSER_PATH").expect(
        "SAG_BROWSER_PATH is not set, so this gate would prove nothing. \
         Run it through `make browser-e2e`.",
    );
    let state = scratch();
    let _ = std::fs::remove_dir_all(&state);
    std::fs::create_dir_all(&state).expect("make the state directory");

    let home = browser::binary::home(&state);
    std::fs::create_dir_all(home.parent().expect("the browser has a parent folder"))
        .expect("make the browser's folder");
    let real = PathBuf::from(&given)
        .parent()
        .expect("the browser sits in a folder")
        .to_path_buf();
    #[cfg(unix)]
    std::os::unix::fs::symlink(&real, &home).expect("link the browser into place");
    // A JUNCTION, not a symlink.
    //
    // `symlink_dir` needs SeCreateSymbolicLink, which an ordinary Windows
    // account does not hold unless Developer Mode is on. Measured on a normal
    // account it answers "Administrator privilege required", so this whole gate
    // failed in its first setup line before asserting anything. A junction is
    // the same thing for a directory on one volume and needs no privilege.
    //
    // Through `raw_arg` because mklink is a cmd BUILTIN: `arg` escapes for the
    // C runtime's parser, which cmd does not use, and a quoted path reaches it
    // mangled (terminal.rs was caught by exactly this and says so).
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        let made = std::process::Command::new("cmd")
            .arg("/C")
            .raw_arg(format!(
                "mklink /J \"{}\" \"{}\"",
                home.display(),
                real.display()
            ))
            .output()
            .expect("ask for the browser to be linked into place");
        assert!(
            made.status.success(),
            "could not link the browser into place: {}",
            String::from_utf8_lossy(&made.stderr)
        );
    }

    sag_desktop::workspace::use_state_dir(state.clone());
    state
}

async fn call(action: &str, mut args: Value) -> Response {
    args["action"] = json!(action);
    args["conversation"] = json!(WHOSE);
    tools::run(Request {
        tool: sag_desktop::tools::browser::NAME.to_string(),
        args,
        call: String::new(),
        resume: false,
    })
    .await
}

fn content(answer: Response, what: &str) -> Value {
    assert!(answer.ok, "{what} failed: {}", answer.message);
    answer.content.unwrap_or(Value::Null)
}

/// The outline of the whole page, read through the tool like everything else.
async fn outline() -> String {
    content(call("snapshot", json!({})).await, "snapshot")["snapshot"]
        .as_str()
        .unwrap_or_default()
        .to_string()
}

/// The reference the outline gave something.
fn reference_for(outline: &str, naming: &str) -> String {
    let line = outline
        .lines()
        .find(|l| l.contains(naming))
        .unwrap_or_else(|| panic!("{naming} is not in the outline:\n{outline}"));
    line.split("[ref=")
        .nth(1)
        .and_then(|rest| rest.split(']').next())
        .unwrap_or_else(|| panic!("{naming} carries no reference: {line}"))
        .to_string()
}

/// Read one element back, to see what an action actually did.
async fn value_of(target: &str) -> String {
    content(
        call("snapshot", json!({ "target": target })).await,
        "reading back",
    )["snapshot"]
        .as_str()
        .unwrap_or_default()
        .to_string()
}

/// Everything the eleven do, in one test.
///
/// One test because they share a browser, a page and a server, and because the
/// ORDER is part of what is checked: fill a form then read it back, open a
/// dialog then find everything else refuses, click a file input then answer the
/// chooser it opened.
#[test]
fn every_new_action_against_a_real_page() {
    // On a stack of its own, not the test harness's. See `on_a_big_enough_stack`.
    on_a_big_enough_stack(|| async {
        let state = set_up();
        stopping_afterwards(
            &state.clone(),
            every_new_action_against_a_real_page_body(state),
        )
        .await;
    });
}

/// Run an async test body on a thread with a stack big enough to hold it.
///
/// A test thread gets 2 MB, and an async function is ONE state machine holding
/// every local it ever has at the same time. This file's longest body grew past
/// that and aborted the whole binary with "has overflowed its stack", which
/// reads as a crash rather than as a test that got too big, and takes the other
/// tests in the binary down with it.
///
/// Boxing the future was tried and cannot work: it is built on the stack before
/// there is anything to move it into. Setting RUST_MIN_STACK was the other
/// option and was rejected, because a test that only passes when it is run a
/// particular way is a test somebody will run the other way.
///
/// So the body gets its own thread and its own runtime. The panic comes back
/// through the join and is re-raised here, so a failing test still fails with
/// its own message and its own place.
fn on_a_big_enough_stack<F, Fut>(body: F)
where
    F: FnOnce() -> Fut + Send + 'static,
    Fut: std::future::Future<Output = ()>,
{
    let ran = std::thread::Builder::new()
        .stack_size(32 * 1024 * 1024)
        .spawn(move || {
            tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .expect("a runtime for the test")
                .block_on(body());
        })
        .expect("a thread for the test")
        .join();
    if let Err(panic) = ran {
        std::panic::resume_unwind(panic);
    }
}

async fn every_new_action_against_a_real_page_body(state: PathBuf) {
    let (at, _server) = serve();
    let page = format!("{at}/page");

    content(call("navigate", json!({ "url": page })).await, "navigate");

    // ----------------------------------------------------------- fill_form
    let seen = outline().await;
    let filled = content(
        call(
            "fill_form",
            json!({ "fields": [
                {"target": "#who", "name": "Your name", "type": "textbox", "value": "Ada"},
                {"target": "#agree", "name": "I agree", "type": "checkbox", "value": "true"},
                {"target": "#post", "name": "By post", "type": "radio", "value": "true"},
                {"target": "#pick", "name": "Pick one", "type": "combobox", "value": "Three"},
                {"target": "#level", "name": "Level", "type": "slider", "value": "7"}
            ]}),
        )
        .await,
        "fill_form",
    );
    assert_eq!(
        filled["filled"].as_array().map(Vec::len),
        Some(5),
        "not every field was filled: {filled:?}"
    );
    let _ = seen;

    let after = outline().await;
    assert!(
        after.contains("Ada"),
        "the textbox was not filled:\n{after}"
    );
    assert!(
        after.contains("checkbox \"I agree\" [checked]") || after.contains("[checked]"),
        "the checkbox was not ticked:\n{after}"
    );
    // A combobox picks by the LABEL a person reads, which is their rule for
    // fill_form and is different from select_option's. "Three" is the label of
    // the option whose value is "3", so picking by value would have missed.
    // The decoy in the fixture is what makes this discriminate: its VALUE is
    // "Three" and its label is "Decoy". Matching on value or label would take
    // the decoy; matching on the label, which is their rule for fill_form and
    // is different from select_option's, takes the option a person would have
    // clicked.
    // The decoy in the fixture is what makes this discriminate: its VALUE is
    // "Three" and its label is "Decoy". Matching on value or label takes the
    // decoy; matching on the label, which is their rule for fill_form and is
    // different from select_option's, takes the option a person would have
    // clicked. The snapshot lists every option and marks the chosen one, so
    // the assertion is on which one carries the mark rather than on which
    // words are present.
    let chosen = value_of("#pick").await;
    assert!(
        chosen.contains(r#"option "Three" [selected]"#),
        "the dropdown matched on the value rather than the label a person reads:\n{chosen}"
    );

    // Filling the SAME form again must leave it in the same state. This is
    // what "set, not clicked" buys: a checkbox that is clicked twice ends up
    // unticked, and a form filled twice would quietly come out wrong.
    content(
        call(
            "fill_form",
            json!({ "fields": [
                {"target": "#agree", "name": "I agree", "type": "checkbox", "value": "true"}
            ]}),
        )
        .await,
        "fill_form again",
    );
    let twice = value_of("#agree").await;
    assert!(
        twice.contains("[checked]"),
        "filling a checkbox twice unticked it:\n{twice}"
    );

    // ---------------------------------------------------------------- drag
    let seen = outline().await;
    let from = reference_for(&seen, "drag me");
    let to = reference_for(&seen, "drop here");
    content(
        call("drag", json!({ "startTarget": from, "endTarget": to })).await,
        "drag",
    );
    // Asserted on what the drop handler WRITES, not on a word the page
    // already said. The first version looked for "dropped" while the page
    // started out saying "nothing dropped", so it passed with drag doing
    // nothing at all: proved by making drag return immediately, and the test
    // still passed. The end to end gate is what disagreed with it.
    let landed = value_of("#dropped").await;
    assert!(
        landed.contains("text=the box"),
        "the drag did not carry the dragged data to the drop zone:\n{landed}"
    );

    // ---------------------------------------------------------------- drop
    let file = state.join("note.txt");
    std::fs::write(&file, "hello from a file").expect("write the file to drop");
    let dropped = content(
        call(
            "drop",
            json!({
                "target": "#zone",
                "paths": [file.to_string_lossy()],
                "data": {"text/plain": "from outside"}
            }),
        )
        .await,
        "drop",
    );
    assert_eq!(dropped["files"], 1);
    let landed = value_of("#dropped").await;
    assert!(
        landed.contains("files=1"),
        "the file did not arrive on the drop zone:\n{landed}"
    );

    // Their own rule, and their own words for it.
    let neither = call("drop", json!({ "target": "#zone" })).await;
    assert_eq!(neither.kind, "bad_arguments", "{}", neither.message);
    assert!(
        neither.message.contains("At least one of"),
        "the refusal should be Playwright's own: {}",
        neither.message
    );

    // --------------------------------------------------------- file_upload
    // Answering a chooser that is not open is refused, because there is
    // nothing to answer.
    let early = call("file_upload", json!({ "paths": [file.to_string_lossy()] })).await;
    assert_eq!(early.kind, "bad_arguments", "{}", early.message);
    assert!(
        early.message.contains("not waiting"),
        "the refusal should say the page is not waiting: {}",
        early.message
    );

    // The order that works: click the thing that asks for a file first.
    content(
        call("click", json!({ "target": "#file" })).await,
        "click the file input",
    );
    let uploaded = content(
        call("file_upload", json!({ "paths": [file.to_string_lossy()] })).await,
        "file_upload",
    );
    assert_eq!(uploaded["uploaded"].as_array().map(Vec::len), Some(1));
    let chosen = value_of("#chosen").await;
    assert!(
        chosen.contains("note.txt"),
        "the page was not given the file:\n{chosen}"
    );

    // A path that is not there is refused BEFORE the browser is asked, which
    // is what makes the reason usable.
    content(
        call("click", json!({ "target": "#file" })).await,
        "click again",
    );
    let missing = call("file_upload", json!({ "paths": ["/no/such/file.txt"] })).await;
    assert_eq!(missing.kind, "bad_arguments", "{}", missing.message);
    assert!(
        missing.message.contains("/no/such/file.txt"),
        "the refusal should name the path: {}",
        missing.message
    );
    // And the chooser it left open is answered, so the rest of this test can
    // carry on.
    content(call("file_upload", json!({})).await, "cancel the chooser");

    // ------------------------------------------------------------ wait_for
    content(
        call("navigate", json!({ "url": page })).await,
        "reload for the wait",
    );
    let waited = content(
        call(
            "wait_for",
            json!({ "text": "it arrived", "textGone": "not yet" }),
        )
        .await,
        "wait_for",
    );
    assert!(waited["waited"].is_string());
    let arrived = outline().await;
    assert!(
        arrived.contains("it arrived") && !arrived.contains("not yet"),
        "the wait returned before the page changed:\n{arrived}"
    );

    // Their own refusal when nothing was asked for.
    let nothing = call("wait_for", json!({})).await;
    assert_eq!(nothing.kind, "bad_arguments");
    assert!(
        nothing.message.contains("Either time, text or textGone"),
        "the refusal should be Playwright's own: {}",
        nothing.message
    );

    // Text that never appears runs out and says so rather than hanging.
    let never = call("wait_for", json!({ "text": "this is nowhere on the page" })).await;
    assert!(!never.ok, "waiting for nothing succeeded");
    assert!(
        never.message.contains("did not appear"),
        "the timeout should say what it was waiting for: {}",
        never.message
    );

    // -------------------------------------------------------------- resize
    content(
        call("resize", json!({ "width": 400, "height": 600 })).await,
        "resize",
    );
    let reported = value_of("#size").await;
    assert!(
        reported.contains("400x600"),
        "the page did not see the new size:\n{reported}"
    );

    // ---------------------------------------------------- console_messages
    let errors = content(
        call("console_messages", json!({ "level": "error" })).await,
        "console at error",
    );
    let at_error = errors["messages"].as_array().cloned().unwrap_or_default();
    assert!(
        at_error
            .iter()
            .any(|m| m["text"].as_str().unwrap_or("").contains("an error line")),
        "the error line is missing: {at_error:?}"
    );
    assert!(
        at_error.iter().all(|m| m["level"] == "error"),
        "asking for errors returned something else: {at_error:?}"
    );
    // The uncaught error, which the page never printed for itself.
    assert!(
        at_error.iter().any(|m| m["text"]
            .as_str()
            .unwrap_or("")
            .contains("undefinedFunctionSomewhere")),
        "the uncaught error is not in the console: {at_error:?}"
    );

    let info = content(
        call("console_messages", json!({ "level": "info" })).await,
        "console at info",
    );
    let at_info = info["messages"].as_array().cloned().unwrap_or_default();
    let said = |lines: &[Value], words: &str| {
        lines
            .iter()
            .any(|m| m["text"].as_str().unwrap_or("").contains(words))
    };
    assert!(said(&at_info, "a plain line"), "info should include log");
    assert!(
        said(&at_info, "a warning line"),
        "info should include warnings"
    );
    assert!(
        said(&at_info, "an error line"),
        "info should include errors"
    );
    assert!(
        !said(&at_info, "a quiet line"),
        "info should NOT include debug: {at_info:?}"
    );

    let all_of_it = content(
        call("console_messages", json!({ "level": "debug" })).await,
        "console at debug",
    );
    let at_debug = all_of_it["messages"]
        .as_array()
        .cloned()
        .unwrap_or_default();
    assert!(
        said(&at_debug, "a quiet line"),
        "debug should include debug"
    );
    assert!(
        at_debug.len() > at_info.len(),
        "debug should be a superset of info"
    );

    // A line carries where it came from, which is half of what it is for.
    let placed = at_error
        .iter()
        .find(|m| m["text"].as_str().unwrap_or("").contains("an error line"))
        .cloned()
        .expect("the error line");
    assert!(
        placed["at"].as_str().unwrap_or("").contains("/page"),
        "a console line should say where it came from: {placed:?}"
    );

    // --------------------------------------------------- network_requests
    let listed = content(
        call("network_requests", json!({})).await,
        "network_requests",
    );
    let requests = listed["requests"].as_array().cloned().unwrap_or_default();
    let url_of = |r: &Value| r["url"].as_str().unwrap_or("").to_string();
    assert!(
        requests.iter().any(|r| url_of(r).contains("/api/thing")),
        "the fetch the page made is missing: {requests:?}"
    );
    assert!(
        requests
            .iter()
            .any(|r| url_of(r).contains("/api/missing") && r["status"] == 404),
        "the 404 is missing or lost its status: {requests:?}"
    );
    // Their static rule: a picture that loaded is noise, and the answer says
    // how much noise it kept back rather than silently dropping it.
    assert!(
        !requests.iter().any(|r| url_of(r).contains("picture.png")),
        "a successful image should be hidden by default: {requests:?}"
    );
    assert!(
        listed["hidden_static"].as_u64().unwrap_or(0) > 0,
        "the answer should say how many were hidden: {listed}"
    );

    let with_static = content(
        call("network_requests", json!({ "static": true })).await,
        "network_requests with static",
    );
    assert!(
        with_static["requests"]
            .as_array()
            .map(|all| all.iter().any(|r| url_of(r).contains("picture.png")))
            .unwrap_or(false),
        "asking for static did not bring the image back: {with_static}"
    );

    let filtered = content(
        call("network_requests", json!({ "filter": "/api/th.*g" })).await,
        "network_requests filtered",
    );
    let only = filtered["requests"].as_array().cloned().unwrap_or_default();
    assert!(
        !only.is_empty() && only.iter().all(|r| url_of(r).contains("/api/thing")),
        "the filter is not a regular expression over the URL: {only:?}"
    );

    let broken = call("network_requests", json!({ "filter": "[unclosed" })).await;
    assert_eq!(broken.kind, "bad_arguments", "{}", broken.message);

    // ---------------------------------------------------- network_request
    let wanted = requests
        .iter()
        .find(|r| url_of(r).contains("/api/thing"))
        .expect("the fetch");
    let number = wanted["index"].as_u64().expect("it is numbered");
    let whole = content(
        call("network_request", json!({ "index": number })).await,
        "network_request",
    );
    assert!(url_of(&whole).contains("/api/thing"));
    assert_eq!(whole["status"], 200);
    assert!(
        whole["responseBody"].as_str().unwrap_or("").contains("42"),
        "the body did not come back: {whole}"
    );
    assert!(
        whole["responseHeaders"]
            .to_string()
            .contains("application/json"),
        "the headers did not come back: {whole}"
    );

    let part = content(
        call(
            "network_request",
            json!({ "index": number, "part": "response-headers" }),
        )
        .await,
        "one part",
    );
    assert!(part.get("headers").is_some() && part.get("responseBody").is_none());

    let no_such = call("network_request", json!({ "index": 99999 })).await;
    assert_eq!(no_such.kind, "bad_arguments", "{}", no_such.message);
    let bad_part = call(
        "network_request",
        json!({ "index": number, "part": "everything" }),
    )
    .await;
    assert_eq!(bad_part.kind, "bad_arguments", "{}", bad_part.message);

    // ---------------------------------------------------------------- tabs
    let listed = content(
        call("tabs", json!({ "operation": "list" })).await,
        "tabs list",
    );
    assert_eq!(listed["tabs"].as_array().map(Vec::len), Some(1));
    assert_eq!(listed["tabs"][0]["current"], true);

    let opened = content(
        call(
            "tabs",
            // A DIFFERENT address, which is what a second tab is for. Asking
            // for one on the address already open gives back the tab already
            // open, which `one_agent_never_holds_an_address_twice` covers.
            json!({ "operation": "new", "url": format!("{at}/page?other") }),
        )
        .await,
        "tabs new",
    );
    let two = opened["tabs"].as_array().cloned().unwrap_or_default();
    assert_eq!(two.len(), 2, "a new tab did not appear: {opened}");
    assert_eq!(two[1]["current"], true, "the new tab should be current");
    assert!(
        two[1]["title"].as_str().unwrap_or("").contains("More"),
        "the new tab did not load its address: {opened}"
    );

    let back = content(
        call("tabs", json!({ "operation": "select", "tab": 0 })).await,
        "tabs select",
    );
    assert_eq!(back["tabs"][0]["current"], true);

    let shut = content(
        call("tabs", json!({ "operation": "close", "tab": 1 })).await,
        "tabs close",
    );
    assert_eq!(shut["tabs"].as_array().map(Vec::len), Some(1));

    let nonsense = call("tabs", json!({ "operation": "reopen" })).await;
    assert_eq!(nonsense.kind, "bad_arguments", "{}", nonsense.message);

    // ------------------------------------------------------- handle_dialog
    // Answering nothing is refused.
    let early = call("handle_dialog", json!({ "accept": true })).await;
    assert_eq!(early.kind, "bad_arguments", "{}", early.message);

    content(
        call("navigate", json!({ "url": page })).await,
        "reload for the dialog",
    );
    let seen = outline().await;
    let ask = reference_for(&seen, "\"Ask\"");
    // The click that OPENS a dialog does not come back on its own: `confirm()`
    // stops the renderer, and the protocol call carrying the click is not
    // answered until the handler finishes, which it never does. So it answers
    // as what happened instead, and it is not a failure: the click landed,
    // which is why the page is asking.
    let opening = content(
        call("click", json!({ "target": ask })).await,
        "open the dialog",
    );
    assert!(
        opening["opened"].as_str().unwrap_or("").contains("confirm"),
        "opening a dialog should say what the page is asking: {opening}"
    );
    assert!(
        opening["note"]
            .as_str()
            .unwrap_or("")
            .contains("handle_dialog"),
        "and how to answer it: {opening}"
    );

    // THE GATE. Every other action refuses while the page is waiting, and this
    // is what stops a snapshot hanging until the protocol gives up: a page
    // showing a dialog runs no JavaScript at all.
    let blocked = call("snapshot", json!({})).await;
    assert!(!blocked.ok, "a snapshot ran while a dialog was open");
    assert!(
        blocked.message.contains("handle_dialog") && blocked.message.contains("confirm"),
        "the refusal should name the dialog and what answers it: {}",
        blocked.message
    );

    let answered = content(
        call("handle_dialog", json!({ "accept": true })).await,
        "handle_dialog",
    );
    assert_eq!(answered["accepted"], true);
    assert_eq!(answered["handled"], "confirm");
    let said = value_of("#alerts").await;
    assert!(
        said.contains("answered true"),
        "the page was not told the dialog was accepted:\n{said}"
    );

    // And the page works again afterwards.
    content(
        call("snapshot", json!({})).await,
        "snapshot after the dialog",
    );

    content(call("close", json!({})).await, "close");
}

/// Run a test body and stop the browser whichever way it ends.
///
/// Not tidiness. `supervise::stop()` is the ONLY thing that ends the browser
/// (a dropped `std::process::Child` does not kill what it holds), so a test
/// that panics before reaching it leaves a running browser behind, and the
/// panic is exactly when a test is most likely to be run again. Measured the
/// expensive way: a session of failing runs left 176 chrome-headless-shell
/// processes alive and took the machine down with them.
///
/// The panic is caught, the browser is stopped, and then the panic goes on its
/// way, so a failing test still fails with its own message and its own place.
async fn stopping_afterwards<F>(state: &std::path::Path, body: F)
where
    F: std::future::Future<Output = ()>,
{
    let outcome = futures_util::FutureExt::catch_unwind(std::panic::AssertUnwindSafe(body)).await;
    browser::supervise::stop().await;
    let _ = std::fs::remove_dir_all(state);
    if let Err(panic) = outcome {
        std::panic::resume_unwind(panic);
    }
}

/// A hundred agents cannot leave a hundred tabs open.
///
/// This is the case that made the cap: an agent starts a fleet, each member
/// opens a tab, they all finish, and nothing tells the browser. Every other
/// mechanism for cleaning up is a signal that might not arrive, so the cap is
/// checked when a tab is OPENED and the build-up cannot happen rather than
/// being tidied up afterwards.
///
/// Driven through the tool, with a different owner per agent, which is what
/// the gateway sends for a fleet.
#[tokio::test]
async fn a_fleet_cannot_open_more_tabs_than_the_cap() {
    let state = set_up();
    stopping_afterwards(
        &state.clone(),
        a_fleet_cannot_open_more_tabs_than_the_cap_body(),
    )
    .await;
}

async fn a_fleet_cannot_open_more_tabs_than_the_cap_body() {
    let (at, _server) = serve();
    let page = format!("{at}/page");

    // Fifty agents, each doing a little work and then stopping, which is a
    // fleet finishing without anybody saying so.
    for agent in 1..=50 {
        let answer = tools::run(Request {
            tool: sag_desktop::tools::browser::NAME.to_string(),
            args: json!({ "action": "navigate", "url": page, "conversation": -agent }),
            call: String::new(),
            resume: false,
        })
        .await;
        assert!(
            answer.ok,
            "agent {agent} could not open a page: {}",
            answer.message
        );
    }

    // Ask the browser itself, rather than our own bookkeeping: what is being
    // checked is the number of real pages, which is the number of processes.
    let open = open_pages().await;
    assert!(
        open <= 10,
        "fifty agents left {open} tabs open, and the cap is 10"
    );
    // And it did not overshoot the other way: the cap is a ceiling, not a
    // target of one.
    assert!(
        open >= 2,
        "only {open} tabs open, so something is closing them that should not be"
    );

    // The agent whose tab survived is still usable, which is the half that
    // matters: a cap that works by breaking everything is not a cap.
    let last = tools::run(Request {
        tool: sag_desktop::tools::browser::NAME.to_string(),
        args: json!({ "action": "navigate", "url": page, "conversation": -50 }),
        call: String::new(),
        resume: false,
    })
    .await;
    assert!(
        last.ok,
        "the most recent agent lost its page: {}",
        last.message
    );

    // And an agent whose tab WAS taken is told so rather than handed a blank
    // page. Agent 1 is the oldest, so it is long gone.
    let evicted = tools::run(Request {
        tool: sag_desktop::tools::browser::NAME.to_string(),
        args: json!({ "action": "snapshot", "conversation": -1 }),
        call: String::new(),
        resume: false,
    })
    .await;
    assert!(!evicted.ok, "an agent acted on a page that had been closed");
    assert!(
        evicted.message.contains("closed to make room"),
        "the refusal should say what happened and what to do: {}",
        evicted.message
    );
}

/// How many pages the BROWSER says it has, which is what costs memory.
async fn open_pages() -> usize {
    let browser = sag_desktop::browser::live::connection()
        .await
        .expect("a connection");
    let targets = browser
        .call("Target.getTargets", json!({}))
        .await
        .expect("ask the browser what it has");
    targets["targetInfos"]
        .as_array()
        .map(|all| all.iter().filter(|t| t["type"] == "page").count())
        .unwrap_or_default()
}

/// Navigating never opens a tab, however many times it is called.
///
/// An agent navigates constantly: it is how it gets anywhere, and over one
/// task it will do it dozens of times, often back to a page it has already
/// been on. If each one opened a tab, a single agent would exhaust the pool by
/// itself and the cap would be hiding the problem rather than solving it.
///
/// So navigating REPLACES what is in the current tab, which is what a browser
/// does when you type in the address bar. The tab is the agent's workspace and
/// the address is what happens to be in it.
///
/// Two agents on the same address still get a tab each, and that is not a miss:
/// sharing would mean one navigating the other away mid-read, which is the
/// exact bug the terminal was fixed for (KB/39).
#[tokio::test]
async fn navigating_reuses_the_tab_it_already_has() {
    let state = set_up();
    stopping_afterwards(
        &state.clone(),
        navigating_reuses_the_tab_it_already_has_body(),
    )
    .await;
}

async fn navigating_reuses_the_tab_it_already_has_body() {
    let (at, _server) = serve();
    let go = |owner: i64, url: String| async move {
        let answer = tools::run(Request {
            tool: sag_desktop::tools::browser::NAME.to_string(),
            args: json!({ "action": "navigate", "url": url, "conversation": owner }),
            call: String::new(),
            resume: false,
        })
        .await;
        assert!(answer.ok, "navigate failed: {}", answer.message);
    };

    // The same address, over and over, which is the case that worried us.
    for _ in 0..20 {
        go(-900, format!("{at}/page")).await;
    }
    assert_eq!(
        open_pages().await,
        1,
        "twenty navigations to one address should be one tab"
    );

    // And twenty DIFFERENT addresses, which is the ordinary shape of a task.
    for n in 0..20 {
        go(-900, format!("{at}/page?step={n}")).await;
    }
    assert_eq!(
        open_pages().await,
        1,
        "an agent walking through twenty pages should still be one tab"
    );

    // A second agent gets its own, deliberately: sharing would mean one
    // navigating the other away mid-read.
    go(-901, format!("{at}/page")).await;
    assert_eq!(
        open_pages().await,
        2,
        "a second agent should have a tab of its own"
    );
}

/// One agent never holds the same address in two tabs.
///
/// Two tabs showing one page has no use anybody can name: the same controls,
/// twice the memory, and two sets of refs for one thing with no way to tell
/// which snapshot a ref came from. So asking for a new tab on a page that is
/// already open gives back the tab that is already open, and navigating onto
/// an address another tab is holding closes that one.
///
/// Per agent and not browser-wide: two agents on one address keep a tab each,
/// because sharing would let one navigate the other away between its snapshot
/// and its click.
#[tokio::test]
async fn one_agent_never_holds_an_address_twice() {
    let state = set_up();
    stopping_afterwards(
        &state.clone(),
        one_agent_never_holds_an_address_twice_body(),
    )
    .await;
}

async fn one_agent_never_holds_an_address_twice_body() {
    let (at, _server) = serve();
    let act = |args: Value| async move {
        tools::run(Request {
            tool: sag_desktop::tools::browser::NAME.to_string(),
            args,
            call: String::new(),
            resume: false,
        })
        .await
    };
    let me = -800;

    let first = format!("{at}/page");
    let second = format!("{at}/page?two");
    content(
        act(json!({"action": "navigate", "url": first, "conversation": me})).await,
        "navigate",
    );

    // A second tab on a DIFFERENT address is legitimate: two pages at once.
    let two = content(
        act(json!({"action": "tabs", "operation": "new", "url": second, "conversation": me})).await,
        "a second tab",
    );
    assert_eq!(two["tabs"].as_array().map(Vec::len), Some(2));

    // A tab on an address already open is NOT. It gives back the one that is
    // open, makes it current, and says so.
    let again = content(
        act(json!({"action": "tabs", "operation": "new", "url": first, "conversation": me})).await,
        "a duplicate tab",
    );
    assert_eq!(
        again["tabs"].as_array().map(Vec::len),
        Some(2),
        "asking for a tab on a page already open opened another one: {again}"
    );
    assert!(
        again["note"]
            .as_str()
            .unwrap_or("")
            .contains("already open"),
        "it should say why there is no new tab: {again}"
    );
    assert_eq!(
        again["tabs"][0]["current"], true,
        "the tab already showing that address should be the current one: {again}"
    );

    // And a trailing slash is the same page, not a second one.
    let slashed = content(
        act(json!({"action": "tabs", "operation": "new", "url": format!("{first}/"), "conversation": me})).await,
        "a trailing slash",
    );
    assert_eq!(
        slashed["tabs"].as_array().map(Vec::len),
        Some(2),
        "a trailing slash opened a second tab on the same page: {slashed}"
    );

    // Navigating the second tab ONTO the first's address closes the duplicate.
    content(
        act(json!({"action": "tabs", "operation": "select", "tab": 1, "conversation": me})).await,
        "select the second tab",
    );
    let landed = content(
        act(json!({"action": "navigate", "url": first, "conversation": me})).await,
        "navigate onto an address already open",
    );
    assert!(
        landed["note"]
            .as_str()
            .unwrap_or("")
            .contains("one page is one tab"),
        "it should say a duplicate was closed: {landed}"
    );
    let left = content(
        act(json!({"action": "tabs", "operation": "list", "conversation": me})).await,
        "list",
    );
    assert_eq!(
        left["tabs"].as_array().map(Vec::len),
        Some(1),
        "navigating onto an open address left two tabs on it: {left}"
    );

    // A second AGENT keeps its own tab on the same address, on purpose.
    content(
        act(json!({"action": "navigate", "url": first, "conversation": -801})).await,
        "another agent",
    );
    assert_eq!(
        open_pages().await,
        2,
        "two agents on one address should have a tab each"
    );
}

/// A sign-in survives the application being closed.
///
/// The whole point of keeping a folder. The test closes the browser outright
/// and wipes its profile, which is what closing the application does, then
/// opens a new one and asks the site who it thinks you are.
///
/// The CONTROL is a second site signed in the same way and never saved: it must
/// come back signed out, or the restore is not what is doing this.
#[tokio::test]
async fn a_saved_sign_in_survives_the_application_closing() {
    let state = set_up();
    stopping_afterwards(
        &state.clone(),
        a_saved_sign_in_survives_the_application_closing_body(state),
    )
    .await;
}

async fn a_saved_sign_in_survives_the_application_closing_body(state: PathBuf) {
    let (at, _server) = serve();
    let app = format!("{at}/app");
    let me = -700;
    let act = |args: Value| async move {
        tools::run(Request {
            tool: sag_desktop::tools::browser::NAME.to_string(),
            args,
            call: String::new(),
            resume: false,
        })
        .await
    };

    // Signed out to begin with, which is what makes the rest mean anything.
    let first = content(
        act(json!({"action": "navigate", "url": app, "conversation": me})).await,
        "navigate",
    );
    assert!(
        first["snapshot"]
            .as_str()
            .unwrap_or("")
            .contains("SIGNED OUT"),
        "the site should start signed out:\n{}",
        first["snapshot"]
    );

    // Sign in, the way a site does: a cookie and a token in local storage.
    let ran = content(
        act(json!({
            "action": "exec_js", "conversation": me,
            "script": "document.cookie = 'sid=LOGGED-IN; path=/'; \
                       localStorage.setItem('jwt', 'TOKEN-42'); \
                       return { ok: true };"
        }))
        .await,
        "sign in",
    );
    assert_eq!(
        ran["js_result"]["ok"], true,
        "the script did not return: {ran}"
    );

    // The CONTROL site: signed in exactly the same way and never saved.
    //
    // A different HOST on the same server, not a different path. The first
    // attempt used `?other`, which is the same origin, so both "sites" shared
    // one jar and one localStorage and the control was not a control at all.
    // `localhost` and `127.0.0.1` are different origins and different cookie
    // domains while being the same fixture.
    let other = app.replace("127.0.0.1", "localhost");
    content(
        act(json!({"action": "navigate", "url": other, "conversation": -701})).await,
        "navigate to the control",
    );
    content(
        act(json!({
            "action": "exec_js", "conversation": -701,
            "script": "localStorage.setItem('jwt', 'CONTROL-TOKEN'); return true;"
        }))
        .await,
        "sign in to the control",
    );

    // Keep only the first.
    let kept = content(
        act(json!({"action": "save_session", "conversation": me})).await,
        "save_session",
    );
    assert!(
        kept["saved"].as_str().unwrap_or("").contains("127.0.0.1"),
        "it should say which site was kept: {kept}"
    );
    let folder = sag_desktop::browser::session::home(&state);
    assert_eq!(
        std::fs::read_dir(&folder).map(|d| d.count()).unwrap_or(0),
        1,
        "exactly one site should have a folder: only one was saved"
    );

    // CLOSE THE APPLICATION. The browser dies and its profile goes with it,
    // which is what makes the folder the only thing left.
    browser::supervise::stop().await;
    let _ = std::fs::remove_dir_all(state.join("browser-profile"));

    // Open it again. Everything in memory is gone, including which sites had
    // been restored, so this is genuinely a new run.
    let back = content(
        act(json!({"action": "navigate", "url": app, "conversation": -702})).await,
        "navigate after reopening",
    );
    let seen = back["snapshot"].as_str().unwrap_or("");
    assert!(
        seen.contains("signed in") && seen.contains("LOGGED-IN") && seen.contains("TOKEN-42"),
        "the saved sign-in did not come back:\n{seen}"
    );
    assert!(
        back["signed_in"].is_string(),
        "the answer should say a sign-in was restored: {back}"
    );

    // And the control, signed in the same way and never saved, is signed out.
    // Without this the test would pass on a browser that had simply kept its
    // profile, which is the thing being ruled out.
    let control = content(
        act(json!({"action": "navigate", "url": other, "conversation": -703})).await,
        "navigate to the control after reopening",
    );
    assert!(
        control["snapshot"]
            .as_str()
            .unwrap_or("")
            .contains("SIGNED OUT"),
        "a site that was never saved came back signed in, so this is not the folder doing it:\n{}",
        control["snapshot"]
    );

    // Forgetting is signing out: the folder goes.
    let gone = content(
        act(json!({"action": "forget_session", "url": app, "conversation": -702})).await,
        "forget_session",
    );
    assert_eq!(gone["forgotten"], true, "{gone}");
    assert_eq!(
        std::fs::read_dir(&folder).map(|d| d.count()).unwrap_or(0),
        0,
        "the folder should be gone after forgetting"
    );
}
