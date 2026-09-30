//! Every combination the browser tools offer, against a real page.
//!
//! Driven through `tools::run`, which is the dispatcher the link calls, so what
//! is exercised is the whole path a tool call takes: the name, the arguments as
//! they arrive, the handler, Playwright's script, the protocol, and the shape of
//! the answer. Nothing is stubbed at any layer.
//!
//! The REFUSALS get as much attention as the successes, and deliberately: a
//! browser tool that quietly clicks the wrong thing, or reports a fill that did
//! not happen, is worse than one that says no. Each refusal here is one a model
//! will meet, and each is asserted to name what is wrong rather than to merely
//! fail.

use std::path::PathBuf;

use sag_desktop::browser;
use sag_desktop::tools::{self, Request, Response};
use serde_json::json;

/// One page holding one of everything the tools do.
const PAGE: &str = r##"<!doctype html><meta charset="utf-8"><title>Everything</title>
<style>#menu { display: none } #hoverme:hover + #menu { display: block }</style>
<h1>Everything</h1>

<label for="who">Your name</label>
<input id="who" placeholder="Type it here" value="old value">

<label for="when">When</label>
<input id="when" type="date">

<label for="agree">I agree</label>
<input id="agree" type="checkbox">

<label for="pick">Pick one</label>
<select id="pick">
  <option value="one">One</option>
  <option value="two">Two</option>
  <option value="three">Three</option>
</select>

<button id="save" disabled>Save</button>
<button id="cancel" onclick="document.getElementById('trusted').textContent = String(event.isTrusted)">Cancel</button>
<p id="trusted">no click yet</p>
<div id="hidden" style="display:none"><button>Invisible</button></div>

<button id="hoverme">Hover me</button>
<p id="menu">the menu</p>

<form id="form" onsubmit="document.getElementById('submitted').textContent = 'submitted'; return false;">
  <input id="term" placeholder="Search">
</form>
<p id="submitted">not submitted</p>

<p id="prose">Some words a person would read.</p>

<input id="chord" value="abc">
<p id="chorded">no chord yet</p>
<script>
document.getElementById('chord').addEventListener('keydown', function (e) {
  document.getElementById('chorded').textContent =
    'key=' + e.key + ' code=' + e.code +
    ' ctrl=' + e.ctrlKey + ' meta=' + e.metaKey + ' shift=' + e.shiftKey +
    ' alt=' + e.altKey;
});
</script>
"##;

/// A second page, to prove a navigation actually happened.
const SECOND: &str = "<!doctype html><title>Second</title><h1>Second page</h1>";

fn scratch() -> PathBuf {
    std::env::temp_dir().join(format!("sag-btools-{}", std::process::id()))
}

/// Put the browser where the tools look for it, and point the installation at
/// this scratch directory.
///
/// The tools ask `binary::installed`, which looks under the state directory for
/// the pinned version, because that is what the product does. The gate is given
/// a browser rather than downloading one, so it is linked into place.
fn set_up() -> PathBuf {
    let given = std::env::var("SAG_BROWSER_PATH").expect(
        "SAG_BROWSER_PATH is not set, so this gate would prove nothing. \
         Run it through `make browser-e2e`.",
    );
    let state = scratch();
    let _ = std::fs::remove_dir_all(&state);
    std::fs::create_dir_all(&state).expect("make the state directory");

    // The WHOLE directory, not just the program. Chromium resolves its
    // resources beside its own executable, so a lone link gives it a folder
    // holding one symlink and it exits on "icudtl.dat not found in bundle".
    // Linking the directory is also the more honest fixture: it is the shape a
    // real download leaves behind.
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

/// Write the fixture pages and answer their addresses.
fn pages(state: &std::path::Path) -> (String, String) {
    let first = state.join("page.html");
    let second = state.join("second.html");
    std::fs::write(&first, PAGE).expect("write the page");
    std::fs::write(&second, SECOND).expect("write the second page");
    (
        format!("file://{}", first.display()),
        format!("file://{}", second.display()),
    )
}

/// Call one of the browser's actions the way the link does.
///
/// The tool is always `browser`; what varies is the action, which is an
/// argument. So the action is put into the arguments here rather than being
/// typed out at each of the fifty call sites, and everything below this line is
/// exactly what the link sends.
async fn call(action: &str, mut args: serde_json::Value) -> Response {
    args["action"] = serde_json::json!(action);
    tools::run(Request {
        tool: sag_desktop::tools::browser::NAME.to_string(),
        args,
        call: String::new(),
        resume: false,
    })
    .await
}

/// The conversation every call in this file belongs to. One page, shared, which
/// is what a single agent working through a task actually has.
const WHOSE: i64 = 4242;

/// Their arguments, plus the identity the gateway adds.
fn with(mut args: serde_json::Value) -> serde_json::Value {
    args["conversation"] = serde_json::json!(WHOSE);
    args
}

/// What a successful answer carries, or a panic naming the failure.
fn content(answer: Response, what: &str) -> serde_json::Value {
    assert!(answer.ok, "{what} failed: {}", answer.message);
    answer.content.unwrap_or(serde_json::Value::Null)
}

/// Read something back out of the page, to check what a tool actually did.
///
/// Through the snapshot action rather than through a second mechanism, so this file
/// has no privileged access the tools do not have: if an effect cannot be seen
/// through the tool surface, a model cannot see it either.
async fn value_of(target: &str) -> String {
    let answer = content(
        call("snapshot", with(serde_json::json!({"target": target}))).await,
        "reading back",
    );
    answer["snapshot"].as_str().unwrap_or_default().to_string()
}

/// The outline of the whole page.
async fn page_outline() -> String {
    let answer = content(
        call("snapshot", with(serde_json::json!({}))).await,
        "snapshot",
    );
    answer["snapshot"].as_str().unwrap_or_default().to_string()
}

/// The reference the outline gave something, found the way a model would.
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

/// Everything, in one test, addressed the way a model would: by reference.
///
/// One test rather than twenty because they share a browser and a page, and the
/// order is part of what is checked: type then read it back, check then check
/// again, act then navigate then act on the new page. Splitting them would mean
/// twenty browser starts and would lose the sequence.
#[tokio::test]
async fn every_combination_against_a_real_page() {
    let state = set_up();
    stopping_afterwards(
        &state.clone(),
        every_combination_against_a_real_page_body(state),
    )
    .await;
}

async fn every_combination_against_a_real_page_body(state: PathBuf) {
    let (first, second) = pages(&state);

    // ---------------------------------------------------------- navigate
    let landed = content(
        call("navigate", with(json!({"url": first}))).await,
        "navigate",
    );
    assert_eq!(landed["title"], "Everything", "the page did not load");
    assert!(landed["url"]
        .as_str()
        .unwrap_or_default()
        .ends_with("page.html"));
    // Their server answers every action with a snapshot, and so do we: it saves
    // the model a turn.
    assert!(
        landed["snapshot"]
            .as_str()
            .unwrap_or_default()
            .contains("[ref=e"),
        "navigate did not answer with an outline carrying references"
    );

    // ---------------------------------------------------------- snapshot
    let outline = page_outline().await;
    for named in [
        "textbox \"Your name\"",
        "button \"Save\"",
        "button \"Cancel\"",
        "checkbox \"I agree\"",
    ] {
        assert!(
            outline.contains(named),
            "the outline has no {named}:\n{outline}"
        );
    }
    assert!(
        outline.contains("[disabled]"),
        "the outline does not say Save is disabled"
    );
    assert!(
        !outline.contains("Invisible"),
        "a hidden control is in the outline, which would send a model at something it cannot use"
    );
    // Every control carries a reference, which is what target takes.
    assert!(
        outline.contains("[ref=e"),
        "the outline carries no references:\n{outline}"
    );

    // Their depth and boxes parameters, which we declare and therefore honour.
    let shallow = content(
        call("snapshot", with(json!({"depth": 1}))).await,
        "a shallow snapshot",
    );
    assert!(
        shallow["snapshot"]
            .as_str()
            .unwrap_or_default()
            .lines()
            .count()
            < outline.lines().count(),
        "depth did nothing"
    );
    let boxed = content(
        call("snapshot", with(json!({"boxes": true}))).await,
        "a snapshot with boxes",
    );
    assert!(
        boxed["snapshot"]
            .as_str()
            .unwrap_or_default()
            .contains("[box="),
        "boxes did nothing"
    );

    // ---------------------------------------------------------- type
    let field = reference_for(&outline, "textbox \"Your name\"");
    let typed = content(
        call(
            "type",
            with(json!({
                "element": "the name field", "target": field, "text": "Maren"
            })),
        )
        .await,
        "type",
    );
    assert_eq!(typed["typed"], true);
    let after = value_of(&field).await;
    assert!(
        after.contains("Maren"),
        "it was reported typed and the field does not hold the value:\n{after}"
    );
    assert!(
        !after.contains("old value"),
        "typing appended instead of replacing what was there:\n{after}"
    );

    // A field that is SET rather than typed into takes their own path and never
    // reaches the protocol. It must still report success.
    let dated = reference_for(&page_outline().await, "\"When\"");
    let set = content(
        call("type", with(json!({"target": dated, "text": "2026-09-20"}))).await,
        "type into a date",
    );
    assert_eq!(set["typed"], true, "a date field was not filled");

    // ---------------------------------------------------------- click
    let cancel = reference_for(&page_outline().await, "button \"Cancel\"");
    let clicked = content(
        call(
            "click",
            with(json!({"element": "the Cancel button", "target": cancel})),
        )
        .await,
        "click",
    );
    assert_eq!(clicked["clicked"], true);
    // The page recorded whether the click was TRUSTED, which is the whole
    // reason input goes through the protocol. A synthetic event reads false
    // here, and real sites ignore those.
    assert!(
        clicked["snapshot"]
            .as_str()
            .unwrap_or_default()
            .contains("true"),
        "the click reached the page as an untrusted event:\n{}",
        clicked["snapshot"]
    );

    // ---------------------------------------------------------- press_key
    let outline = page_outline().await;
    let term = reference_for(&outline, "\"Search\"");
    content(
        call("type", with(json!({"target": term, "text": "hello"}))).await,
        "type before pressing",
    );
    let pressed = content(
        call("press_key", with(json!({"key": "Enter"}))).await,
        "press Enter",
    );
    assert_eq!(pressed["pressed"], "Enter");
    assert!(
        pressed["snapshot"]
            .as_str()
            .unwrap_or_default()
            .contains("submitted"),
        "Enter was reported pressed and the form never submitted:\n{}",
        pressed["snapshot"]
    );

    // A CHORD, which their own documentation for pressing a key promises:
    // "Shortcuts such as `key: \"Control+o\"` ... are supported as well."
    // Copying that sentence into the tool's guide is a promise, so it is
    // driven here against a page that records what arrived.
    let outline = page_outline().await;
    let chord = reference_for(&outline, "textbox");
    content(
        call("click", with(json!({"target": "#chord"}))).await,
        "focus the field that records chords",
    );
    let _ = chord;
    let shortcut = content(
        call("press_key", with(json!({"key": "Control+o"}))).await,
        "press Control+o",
    );
    let recorded = shortcut["snapshot"].as_str().unwrap_or_default();
    assert!(
        recorded.contains("key=o") && recorded.contains("ctrl=true"),
        "Control+o did not arrive as a shortcut:\n{recorded}"
    );
    // And it typed NOTHING, which is what a caller reaching for a shortcut
    // needs. Measured to be the modifier mask's doing rather than the blanked
    // text's: putting the text back leaves this passing (see
    // `browser::keys::describe`).
    let still = value_of("#chord").await;
    assert!(
        still.contains("abc"),
        "Control+o typed an o into the field:\n{still}"
    );

    // Two modifiers, which is their other documented example.
    let two = content(
        call("press_key", with(json!({"key": "Control+Shift+T"}))).await,
        "press Control+Shift+T",
    );
    let recorded = two["snapshot"].as_str().unwrap_or_default();
    assert!(
        recorded.contains("code=KeyT")
            && recorded.contains("ctrl=true")
            && recorded.contains("shift=true"),
        "Control+Shift+T did not arrive with both modifiers:\n{recorded}"
    );

    // A key named by its code, which is what their example list is made of.
    let by_code = content(
        call("press_key", with(json!({"key": "Shift+Digit1"}))).await,
        "press Shift+Digit1",
    );
    let recorded = by_code["snapshot"].as_str().unwrap_or_default();
    assert!(
        recorded.contains("key=!"),
        "Shift+Digit1 should be an exclamation mark:\n{recorded}"
    );

    // The EDITING half, which is the one that fails silently. On macOS a key
    // press does not edit anything by itself: Chromium expects the editing
    // operation named alongside it, because on a real Mac the operating system
    // is what turns Command+A into "select all". So select everything, type one
    // character over it, and the field should hold only that character. With
    // the command not sent the key arrives, the page sees it, and nothing is
    // selected, so the field would read `abcx`.
    content(
        call("type", with(json!({"target": "#chord", "text": "abc"}))).await,
        "fill the field before selecting",
    );
    content(
        call("press_key", with(json!({"key": "ControlOrMeta+a"}))).await,
        "select all",
    );
    content(
        call("press_key", with(json!({"key": "x"}))).await,
        "type over the selection",
    );
    let after = value_of("#chord").await;
    assert!(
        after.trim_end().ends_with(": x") && !after.contains("abc"),
        "select-all did not select, so the typed character was appended:\n{after}"
    );

    // And their `submit`, which does the same in one call.
    let outline = page_outline().await;
    let term = reference_for(&outline, "\"Search\"");
    let submitted = content(
        call(
            "type",
            with(json!({"target": term, "text": "again", "submit": true})),
        )
        .await,
        "type and submit",
    );
    assert_eq!(submitted["submitted"], true);

    // And their `slowly`, which types a key at a time.
    let outline = page_outline().await;
    let term = reference_for(&outline, "\"Search\"");
    let slowly = content(
        call(
            "type",
            with(json!({"target": term, "text": "abc", "slowly": true})),
        )
        .await,
        "type slowly",
    );
    assert_eq!(slowly["typed"], true);
    assert!(
        value_of(&term).await.contains("abc"),
        "typing slowly did not put the text in"
    );

    // ---------------------------------------------------------- select_option
    let outline = page_outline().await;
    let picker = reference_for(&outline, "combobox");
    let chose = content(
        call(
            "select_option",
            with(json!({"target": picker, "values": ["two"]})),
        )
        .await,
        "select an option",
    );
    assert_eq!(
        chose["selected"],
        json!(["two"]),
        "the wrong option was taken"
    );

    // ---------------------------------------------------------- hover
    let outline = page_outline().await;
    let over = reference_for(&outline, "\"Hover me\"");
    let hovered = content(call("hover", with(json!({"target": over}))).await, "hover");
    assert_eq!(hovered["hovered"], true);
    assert!(
        hovered["snapshot"]
            .as_str()
            .unwrap_or_default()
            .contains("the menu"),
        "hovering did not reveal what only appears on hover:\n{}",
        hovered["snapshot"]
    );

    // ---------------------------------------------------------- checkbox
    let outline = page_outline().await;
    let agree = reference_for(&outline, "checkbox \"I agree\"");
    content(
        call("click", with(json!({"target": agree}))).await,
        "tick a checkbox",
    );
    assert!(
        value_of(&agree).await.contains("[checked]"),
        "clicking a checkbox did not tick it"
    );

    // ---------------------------------------------------------- navigate again
    let moved = content(
        call("navigate", with(json!({"url": second}))).await,
        "navigate to the second page",
    );
    assert_eq!(moved["title"], "Second", "the second page did not load");

    // ---------------------------------------------------------- navigate_back
    let back = content(call("navigate_back", with(json!({}))).await, "go back");
    assert_eq!(
        back["title"], "Everything",
        "going back did not return to the first page"
    );

    // ---------------------------------------------------------- close
    let closed = content(call("close", with(json!({}))).await, "close");
    assert_eq!(closed["closed"], true);
    let twice = content(call("close", with(json!({}))).await, "close again");
    assert_eq!(
        twice["closed"], false,
        "closing nothing reported closing something"
    );
}

/// Every refusal a model will meet, each saying what is wrong.
#[tokio::test]
async fn every_refusal_names_what_is_wrong() {
    let state = set_up();
    stopping_afterwards(
        &state.clone(),
        every_refusal_names_what_is_wrong_body(state),
    )
    .await;
}

async fn every_refusal_names_what_is_wrong_body(state: PathBuf) {
    let (first, _) = pages(&state);
    content(
        call("navigate", with(json!({"url": first}))).await,
        "navigate",
    );

    // A selector matching two things. Acting on the first would be a guess, and
    // the guess is what this exists to prevent. A REFERENCE cannot do this,
    // which is the argument for using them.
    let ambiguous = call("click", with(json!({"target": "internal:role=button"}))).await;
    assert!(!ambiguous.ok, "an ambiguous selector was acted on");
    assert!(
        ambiguous.message.contains("strict mode violation"),
        "the refusal should say it matched several: {}",
        ambiguous.message
    );

    // Nothing matches, whether by selector or by a reference the page never
    // handed out.
    for target in ["#nothing-like-this", "e9999"] {
        let missing = call("click", with(json!({"target": target}))).await;
        assert!(!missing.ok, "{target} was acted on");
        assert!(
            missing.message.contains("nothing on this page matches"),
            "{target}: {}",
            missing.message
        );
    }

    // Disabled and hidden, with Playwright's own word for which state is wrong.
    let outline = page_outline().await;
    let save = reference_for(&outline, "button \"Save\"");
    let disabled = call("click", with(json!({"target": save}))).await;
    assert!(!disabled.ok);
    assert!(
        disabled.message.contains("not enabled"),
        "the refusal should name the state: {}",
        disabled.message
    );

    let hidden = call("click", with(json!({"target": "#hidden button"}))).await;
    assert!(!hidden.ok);
    assert!(
        hidden.message.contains("not visible"),
        "the refusal should name the state: {}",
        hidden.message
    );

    // Something that cannot be typed into, which is their error rather than
    // ours.
    let unfillable = call("type", with(json!({"target": "#prose", "text": "x"}))).await;
    assert!(!unfillable.ok, "a paragraph was typed into");

    // Arguments a model can correct. These are bad_arguments and not failures,
    // because the loop treats the two differently: one is something to learn
    // from, the other is something to report.
    for (tool, args) in [
        ("navigate", json!({})),
        ("click", json!({})),
        // A target that exists, with no text: their schema marks text
        // required, so this is the model's mistake to hear about.
        ("type", json!({"target": "#who"})),
        ("press_key", json!({})),
        ("select_option", json!({"target": "e1"})),
        ("hover", json!({})),
    ] {
        let answer = call(tool, with(args.clone())).await;
        assert_eq!(
            answer.kind, "bad_arguments",
            "{tool} with {args} answered {:?}: {}",
            answer.kind, answer.message
        );
    }

    // A modifier that is not one. "Ctrl" is the mistake a caller will actually
    // make, and the answer has to name it: pressing `A` instead would be worse
    // than saying no.
    let bad_modifier = call("press_key", with(json!({"key": "Ctrl+A"}))).await;
    assert_eq!(
        bad_modifier.kind, "bad_arguments",
        "{}",
        bad_modifier.message
    );
    assert!(
        bad_modifier.message.contains("Ctrl") && bad_modifier.message.contains("Control"),
        "the refusal should name what was wrong and what the modifiers are: {}",
        bad_modifier.message
    );

    // And a key that is not a key, whose refusal shows the shapes that are.
    let bad_key = call("press_key", with(json!({"key": "Nonsense"}))).await;
    assert_eq!(bad_key.kind, "bad_arguments", "{}", bad_key.message);
    assert!(
        bad_key.message.contains("Nonsense") && bad_key.message.contains("Control+o"),
        "the refusal should name it and show a chord: {}",
        bad_key.message
    );

    // An action that is not one of theirs, which one tool makes possible in a
    // way nine tools did not: the action is a string the model chose.
    let bad_action = call("screenshot", with(json!({}))).await;
    assert_eq!(bad_action.kind, "bad_arguments", "{}", bad_action.message);
    assert!(
        bad_action.message.contains("screenshot") && bad_action.message.contains("snapshot"),
        "the refusal should name what was asked and list what there is: {}",
        bad_action.message
    );
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
