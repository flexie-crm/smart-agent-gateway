//! The browser: one ability, with Playwright's own actions inside it.
//!
//! ONE tool named `browser`, taking an `action`, which is the shape the
//! terminal already has here. The schema is the gateway's
//! (internal/tools/machine/browser.go, holding Playwright's own wording); this
//! half is the doing.
//!
//! **What an element is named by.** Their `target` is documented as "Exact
//! target element reference from the page snapshot, or a unique element
//! selector", so it takes either. A reference (`e12`) becomes `aria-ref=e12`,
//! which their own engine resolves against the last snapshot; anything else is
//! passed to their selector parser as written. A reference is the reliable one
//! and is what the guide steers towards, because it names exactly one element
//! and so cannot be ambiguous.
//!
//! **Every action answers with a fresh snapshot**, which is what their server
//! does (`response.setIncludeSnapshot()`). It costs one more round trip inside
//! a call that is already several, and it saves the model a whole turn: after
//! clicking something it can see what changed without asking.
//!
//! **Finding and deciding is Playwright's. Doing is the protocol's.** Their
//! script has no click, and filling a text field returns `needsinput` with the
//! field selected and nothing typed. See `browser::input` and `browser::keys`.

use std::collections::HashMap;

use serde::Deserialize;
use serde_json::{json, Value};

use super::Response;
use crate::browser::page::Page;
use crate::browser::record::Modal;
use crate::browser::{input, live, session};

/// The one name.
pub const NAME: &str = "browser";

/// The version of its arguments.
///
/// Two, because the arguments grew: an application on version one is offered a
/// browser it would refuse half the calls to.
pub const VERSION: i64 = 3;

/// Their actions, which are their own tool names with the `browser_` taken off.
///
/// Listed rather than left to the match below, because this is the set the
/// refusal quotes: a model that asked for something else is told what there is
/// and can correct itself in one turn.
const ACTIONS: [&str; 23] = [
    "navigate",
    "navigate_back",
    "snapshot",
    "click",
    "type",
    "fill_form",
    "press_key",
    "select_option",
    "hover",
    "drag",
    "drop",
    "file_upload",
    "wait_for",
    "resize",
    "tabs",
    "handle_dialog",
    "console_messages",
    "network_requests",
    "network_request",
    "exec_js",
    "save_session",
    "forget_session",
    "close",
];

/// How much of a snapshot one answer may carry.
///
/// The same ceiling the terminal and the skill runner use. An outline is small
/// (a few hundred bytes to a few kilobytes) where the markup of the same page
/// is hundreds, which is most of the reason the outline is what these answer
/// with.
const MOST_TEXT: usize = 96 * 1024;

/// Which page this call belongs to. Added by the gateway, never by a model, and
/// per RUNNING AGENT rather than per conversation (KB/39).
#[derive(Debug, Deserialize)]
struct Whose {
    #[serde(default)]
    conversation: i64,
}

fn whose(args: &Value) -> i64 {
    serde_json::from_value::<Whose>(args.clone())
        .map(|w| w.conversation)
        .unwrap_or_default()
}

/// Which action this call is.
#[derive(Debug, Deserialize)]
struct Which {
    #[serde(default)]
    action: String,
}

/// run does the one action the call names.
///
/// The page is opened HERE rather than in each handler, and the modal-state
/// gate sits between the two. That is structural on purpose: a gate each
/// handler had to remember to call is a gate that is missing from the next
/// action somebody adds, and the failure would be a tool call that hangs for a
/// minute rather than one that says why.
pub async fn run(args: Value) -> Response {
    let owner = whose(&args);
    let action = serde_json::from_value::<Which>(args.clone())
        .map(|which| which.action)
        .unwrap_or_default();
    let action = action.trim().to_string();

    // The two that are not about a page. `close` is also the way out of a page
    // that is stuck: it ends the page rather than asking it anything, so it
    // works when a dialog has the page blocked, and something has to.
    match action.as_str() {
        "close" => return close(args).await,
        "tabs" => return tabs(args).await,
        "" => {
            return Response::bad_arguments(format!(
                "no action, so there is nothing to do. The actions are {}.",
                ACTIONS.join(", ")
            ))
        }
        other if !ACTIONS.contains(&other) => {
            return Response::bad_arguments(format!(
                "\"{other}\" is not one of this browser's actions. They are {}.",
                ACTIONS.join(", ")
            ))
        }
        _ => {}
    }

    // Leased for the length of this call, so the pool cannot take it away
    // mid-action. It is given back when `page` goes out of scope, on the way
    // out of a panic as well as a return.
    let page = match live::page(owner).await {
        Ok(page) => page,
        Err(why) => return Response::failed(why),
    };
    if let Some(refused) = held_up(&page, &action) {
        return refused;
    }
    // This agent's tab was closed to make room while it was away, so what it
    // is holding is a blank page rather than where it was. Said once, to the
    // first action that would have read the old page, because acting on a
    // blank page and reporting success is the outcome worth avoiding.
    //
    // navigate is exempt: it is about to replace the page anyway, and telling
    // it to navigate again would be telling it to do what it is doing.
    if page.replaced() && !matches!(action.as_str(), "navigate" | "navigate_back") {
        return Response::bad_arguments(
            "this page was closed to make room while nothing was happening on it, so there is \
             nothing here now. Navigate again to carry on.",
        );
    }

    // An action that makes the page stop and ask a question does not come back
    // on its own, so it is raced against the page becoming blocked.
    //
    // This is not a precaution, and it is the half that does the work.
    // `confirm()` stops the renderer dead, and the protocol call that DELIVERED
    // the click is not answered until the handler that called it finishes,
    // which it never does. Measured by taking it out: clicking a button that
    // confirms gives "Input.dispatchMouseEvent was not answered in 60s" and the
    // test takes 92 seconds instead of 32, with the click having landed
    // perfectly all along.
    //
    // Blocked wins the race when both are ready, because the work's answer in
    // that case is a timeout sixty seconds later.
    //
    // Not for the two that ANSWER a question: they are the ones that run while
    // the page is waiting, so racing them against it would cut them off before
    // they had done anything. Which is not a hypothetical, it is what the first
    // cut of this did: `handle_dialog` reported the dialog it was in the middle
    // of answering.
    if answers_a_question(&action) {
        return dispatch(&page, owner, &action, args).await;
    }
    let acting = dispatch(&page, owner, &action, args);
    tokio::pin!(acting);
    tokio::select! {
        biased;
        waiting = until_blocked(&page) => stopped_to_ask(waiting),
        done = &mut acting => done,
    }
}

/// Whether this action is one of the two that answer what the page is asking.
///
/// The same two `held_up` lets through, and deliberately one function rather
/// than two lists: a third one added to one and not the other would be an
/// action that is allowed to run and is then cut off before it does.
fn answers_a_question(action: &str) -> bool {
    matches!(action, "handle_dialog" | "file_upload")
}

/// Wait until the page has stopped to ask something.
///
/// Polled off the recorder rather than raced against the event, and that is the
/// difference between correct and nearly correct: the recorder is the one
/// writer of this, so by the time this returns the state is already written and
/// the NEXT call is gated. Racing the raw event would return first sometimes,
/// leaving the next action to hang on exactly the dialog this exists to catch.
///
/// A file chooser is deliberately not one of these. It does not block the page,
/// so an action that opens one carries on and answers normally, and cutting it
/// short would throw away a perfectly good snapshot.
async fn until_blocked(page: &Page) -> Modal {
    loop {
        if let Some(modal @ Modal::Dialog { .. }) = page.records().modal() {
            return modal;
        }
        tokio::time::sleep(std::time::Duration::from_millis(5)).await;
    }
}

/// What to say when an action set the page asking instead of finishing.
///
/// Not a failure: the click landed, which is why the page is asking. It is
/// reported as what happened, with the way out, because the alternative is a
/// model that reads "failed" and tries the same click again.
fn stopped_to_ask(waiting: Modal) -> Response {
    Response::ok(json!({
        "opened": waiting.describe(),
        "note": format!(
            "that worked, and the page has stopped to ask something. Nothing else can \
             happen until it is answered: use action {}.",
            waiting.answered_by()
        ),
    }))
}

/// One action, once the page is open and nothing is in the way.
async fn dispatch(page: &Page, owner: i64, action: &str, args: Value) -> Response {
    match action {
        "navigate" => navigate(page, owner, args).await,
        "navigate_back" => navigate_back(page).await,
        "snapshot" => snapshot(page, args).await,
        "click" => click(page, args).await,
        "type" => type_text(page, args).await,
        "fill_form" => fill_form(page, args).await,
        "press_key" => press_key(page, args).await,
        "select_option" => select_option(page, args).await,
        "hover" => hover(page, args).await,
        "drag" => drag(page, args).await,
        "drop" => drop_onto(page, args).await,
        "file_upload" => file_upload(page, args).await,
        "wait_for" => wait_for(page, args).await,
        "resize" => resize(page, args).await,
        "handle_dialog" => handle_dialog(page, args).await,
        "console_messages" => console_messages(page, args).await,
        "network_requests" => network_requests(page, args).await,
        "network_request" => network_request(page, args).await,
        "exec_js" => exec_js(page, args).await,
        "save_session" => save_session(page).await,
        "forget_session" => forget_session(page, args).await,
        // Unreachable: the list is checked before the page is opened.
        other => Response::failed(format!("{other} is not a browser action")),
    }
}

/// Whether something the page is waiting on stops this action, or is the point
/// of it.
///
/// Playwright's rule, copied: a tool that clears a modal state may only run
/// while that state is present, and every other tool refuses while one is.
///
/// What this does and does not do, measured with each half removed on its own.
/// It is NOT what stops a call hanging on a blocked page: the race below is,
/// and with the race in place and this gate gone, a snapshot of a page showing
/// a dialog comes back promptly saying the page is asking something. What this
/// adds is that it comes back REFUSED, naming the dialog and the action that
/// answers it, rather than as a success a model would read as "the snapshot
/// worked" before going on to use a page outline it never got.
fn held_up(page: &Page, action: &str) -> Option<Response> {
    let waiting = page.records().modal();
    let clears = answers_a_question(action).then_some(action);

    match (waiting, clears) {
        // The page is stuck on exactly what this action answers.
        (Some(modal), Some(answers)) if modal.answered_by() == answers => None,
        // It is stuck on something else this action cannot answer.
        (Some(modal), _) => Some(Response::failed(format!(
            "the page is waiting on {} and nothing else can happen until it is answered. \
             Use action {} to answer it.",
            modal.describe(),
            modal.answered_by()
        ))),
        // This action answers something and there is nothing to answer.
        (None, Some(answers)) => Some(Response::bad_arguments(format!(
            "{answers} answers something the page is waiting on, and this page is not \
             waiting on anything."
        ))),
        (None, None) => None,
    }
}

// ------------------------------------------------------------------ navigating

#[derive(Debug, Deserialize)]
struct Navigate {
    #[serde(default)]
    url: String,
}

async fn navigate(page: &Page, owner: i64, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<Navigate>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    if args.url.trim().is_empty() {
        return Response::bad_arguments("no URL to navigate to");
    }
    let url = args.url.trim();

    // A signed-in session for this site, put back BEFORE the page loads, so
    // the site reads it on load and knows who this is. Once per site per run:
    // after that the live browser holds the truth, and restoring an older jar
    // over it would put the session backwards.
    let restored = restore_session(page, url).await;

    let went = page.goto(url).await;
    // Taken away whether the page loaded or not: a seeding script left behind
    // would run on every later document this tab holds.
    if restored {
        page.stop_seeding().await;
    }
    if let Err(why) = went {
        return Response::failed(why);
    }
    // One tab per address for this agent. Landing on a page it already had
    // open somewhere else leaves two tabs showing one thing, which is twice
    // the memory for one view and two sets of refs for the same controls.
    let closed = live::one_tab_per_address(owner, page, url).await;

    // Kept fresh. A site that rotates its token would otherwise go stale in
    // the folder and the next start would land signed out, which looks exactly
    // like the feature not working.
    if session::state_dir()
        .is_some_and(|state| session::site_of(url).is_some_and(|site| session::have(&state, &site)))
    {
        // Best effort: a refresh that fails leaves the previous jar in place,
        // which is a sign-in that may be stale rather than one that is gone.
        // Failing the navigation over it would be the worse outcome.
        let _ = keep_session(page, url).await;
    }

    let mut answer = landed(page).await;
    if restored {
        if let Some(said) = answer.content.as_mut().and_then(Value::as_object_mut) {
            said.insert(
                "signed_in".into(),
                json!(
                    "a saved sign-in for this site was put back, so you should already be \
                       signed in. If the page says otherwise the session has expired: sign in \
                       again and call save_session."
                ),
            );
        }
    }
    if closed > 0 {
        if let Some(said) = answer.content.as_mut().and_then(Value::as_object_mut) {
            said.insert(
                "note".into(),
                json!(format!(
                    "{closed} other tab{} on this address {} closed: one page is one tab.",
                    if closed == 1 { "" } else { "s" },
                    if closed == 1 { "was" } else { "were" }
                )),
            );
        }
    }
    answer
}

async fn navigate_back(page: &Page) -> Response {
    // A page with nowhere to go back to is not an error: it is a page at the
    // start of its history, and saying so is better than failing.
    match page.back().await {
        Ok(false) => Response::ok(json!({
            "moved": false,
            "note": "there is nothing to go back to on this page",
        })),
        Ok(true) => landed(page).await,
        Err(why) => Response::failed(why),
    }
}

async fn close(args: Value) -> Response {
    match live::close(whose(&args)).await {
        Ok(closed) => Response::ok(json!({ "closed": closed })),
        Err(why) => Response::failed(why),
    }
}

// ------------------------------------------------------------------- reading

#[derive(Debug, Deserialize)]
struct Snapshot {
    #[serde(default)]
    target: String,
    #[serde(default)]
    depth: Option<u32>,
    #[serde(default)]
    boxes: Option<bool>,
}

async fn snapshot(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<Snapshot>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    match outline(page, &args.target, args.depth, args.boxes).await {
        Ok(Value::Null) => Response::failed(nothing_matches(&args.target)),
        Ok(text) => Response::ok(with_snapshot(json!({}), text)),
        Err(why) => Response::failed(why),
    }
}

// -------------------------------------------------------------------- acting

/// The two every element tool takes.
#[derive(Debug, Deserialize, Default)]
struct Element {
    /// Their human-readable description of what is being touched.
    ///
    /// Read by the GATEWAY, not here: it is what a person is shown on the
    /// approval card, and it is in the tool's `Shown` display. This half
    /// accepts it and does nothing with it, which is the honest arrangement,
    /// because a handler that rejected an argument the schema declares would
    /// refuse a correctly formed call.
    #[allow(dead_code)]
    #[serde(default)]
    element: String,
    #[serde(default)]
    target: String,
}

#[derive(Debug, Deserialize)]
struct ClickArgs {
    #[serde(flatten)]
    at: Element,
    /// Their name on the wire, ours in the code. The schema is theirs and
    /// cannot be renamed; the field is ours and should read like Rust.
    #[serde(default, rename = "doubleClick")]
    double_click: Option<bool>,
    #[serde(default)]
    button: Option<String>,
    #[serde(default)]
    modifiers: Option<Vec<String>>,
}

async fn click(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<ClickArgs>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    if args.at.target.trim().is_empty() {
        return Response::bad_arguments("no target, so there is nothing to click");
    }
    let found = locator(&args.at.target);

    let point = match where_it_is(page, &found).await {
        Ok(point) => point,
        Err(why) => return why,
    };
    let browser = page.browser().clone();
    let modifiers = args.modifiers.unwrap_or_default();
    let how = input::Click {
        button: args.button.as_deref().unwrap_or("left"),
        double: args.double_click.unwrap_or(false),
        modifiers: &modifiers,
    };
    if let Err(why) = input::click(&browser, page.session(), point.0, point.1, how).await {
        return Response::failed(why);
    }
    answered(page, json!({ "clicked": true })).await
}

#[derive(Debug, Deserialize)]
struct TypeArgs {
    #[serde(flatten)]
    at: Element,
    /// An OPTION rather than a defaulted String, because absent and empty are
    /// different things here: their schema marks text required, and an empty
    /// one is a legitimate value that clears the field. Defaulted, a call with
    /// no text at all reached their script and came back as "not an input",
    /// which is a failure a model cannot learn anything from.
    #[serde(default)]
    text: Option<String>,
    #[serde(default)]
    submit: Option<bool>,
    #[serde(default)]
    slowly: Option<bool>,
}

async fn type_text(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<TypeArgs>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    if args.at.target.trim().is_empty() {
        return Response::bad_arguments("no target, so there is nothing to type into");
    }
    let Some(text) = args.text.clone() else {
        return Response::bad_arguments(
            "no text to type. Pass an empty string to clear the field.",
        );
    };
    let found = locator(&args.at.target);
    let browser = page.browser().clone();

    // Slowly means one key event per character, into a field that has been
    // cleared and focused. Their `fill` sets the whole value at once, so it is
    // the wrong tool when a page is listening per keystroke.
    if args.slowly.unwrap_or(false) {
        let ready = page
            .ask(&format!(
                "const el = s.querySelector(s.parseSelector({found}), document, true);
                 if (!el) return null;
                 const missing = await s.checkElementStates(el, ['visible','enabled','editable']);
                 if (missing) return {{blocked: missing.missingState}};
                 s.fill(el, '');
                 s.focusNode(el, true);
                 return true;"
            ))
            .await;
        if let Err(why) = blocked_or(ready) {
            return why;
        }
        if let Err(why) = input::type_slowly(&browser, page.session(), &text).await {
            return Response::failed(why);
        }
    } else {
        let outcome = page
            .ask(&format!(
                "const el = s.querySelector(s.parseSelector({found}), document, true);
                 if (!el) return null;
                 const missing = await s.checkElementStates(el, ['visible','enabled','editable']);
                 if (missing) return {{blocked: missing.missingState}};
                 return {{fill: s.fill(el, {})}};",
                quoted(&text)
            ))
            .await;
        let outcome = match blocked_or(outcome) {
            Ok(outcome) => outcome,
            Err(why) => return why,
        };
        match outcome.get("fill").and_then(Value::as_str) {
            // Their script did it: an input that is SET rather than typed
            // into (a date, a colour, a range). Nothing left to do.
            Some("done") => {}
            // The seam. The field is selected and empty; the protocol types.
            Some("needsinput") => {
                if let Err(why) = input::insert_text(&browser, page.session(), &text).await {
                    return Response::failed(why);
                }
            }
            Some(other) => return Response::failed(format!("it could not be filled: {other}")),
            None => return Response::failed("the page did not say whether it was filled"),
        }
    }

    if args.submit.unwrap_or(false) {
        if let Err(why) = input::press(&browser, page.session(), "Enter").await {
            return Response::failed(why);
        }
    }
    answered(
        page,
        json!({ "typed": true, "submitted": args.submit.unwrap_or(false) }),
    )
    .await
}

#[derive(Debug, Deserialize)]
struct PressKey {
    #[serde(default)]
    key: String,
}

async fn press_key(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<PressKey>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    if args.key.trim().is_empty() {
        return Response::bad_arguments("no key to press");
    }
    let browser = page.browser().clone();
    // Into whatever has focus, which is what a keyboard does. Their schema
    // takes no target for this one.
    if let Err(why) = input::press(&browser, page.session(), args.key.trim()).await {
        // A key this does not know is correctable, so it is said as an
        // argument mistake rather than a failure.
        return Response::bad_arguments(why);
    }
    answered(page, json!({ "pressed": args.key.trim() })).await
}

#[derive(Debug, Deserialize)]
struct SelectOption {
    #[serde(flatten)]
    at: Element,
    #[serde(default)]
    values: Vec<String>,
}

async fn select_option(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<SelectOption>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    if args.at.target.trim().is_empty() {
        return Response::bad_arguments("no target, so there is nothing to select in");
    }
    if args.values.is_empty() {
        return Response::bad_arguments("no values to select");
    }
    let found = locator(&args.at.target);
    // Entirely their script: selecting an option is setting a value and firing
    // the events a page listens for, all of which is in the page. Nothing
    // about it needs the protocol.
    let outcome = page
        .ask(&format!(
            "const el = s.querySelector(s.parseSelector({found}), document, true);
             if (!el) return null;
             const missing = await s.checkElementStates(el, ['visible','enabled']);
             if (missing) return {{blocked: missing.missingState}};
             return {{selected: s.selectOptions(el, {})}};",
            quoted_values(&args.values)
        ))
        .await;
    let outcome = match blocked_or(outcome) {
        Ok(outcome) => outcome,
        Err(why) => return why,
    };
    // Their selectOptions answers with the values it actually took, or an
    // error string. A dropdown that has none of the asked-for options is a
    // real answer and worth passing on rather than reporting success.
    match outcome.get("selected") {
        Some(Value::Array(taken)) => answered(page, json!({ "selected": taken })).await,
        Some(Value::String(why)) => Response::failed(format!("it could not be selected: {why}")),
        _ => Response::failed("the page did not say what was selected"),
    }
}

async fn hover(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<Element>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    if args.target.trim().is_empty() {
        return Response::bad_arguments("no target, so there is nothing to hover over");
    }
    let found = locator(&args.target);
    let point = match where_it_is(page, &found).await {
        Ok(point) => point,
        Err(why) => return why,
    };
    let browser = page.browser().clone();
    if let Err(why) = input::hover(&browser, page.session(), point.0, point.1).await {
        return Response::failed(why);
    }
    answered(page, json!({ "hovered": true })).await
}

// ------------------------------------------------------------------- forms

/// One field of a form, as their `fill_form` takes them.
#[derive(Debug, Deserialize)]
struct Field {
    #[serde(default)]
    target: String,
    /// Their "Human-readable field name". Their own handler uses it as the
    /// element description on the approval card, which is why it is here and
    /// why it is not used to find anything.
    #[serde(default)]
    name: String,
    #[serde(default, rename = "type")]
    kind: String,
    #[serde(default)]
    value: String,
}

#[derive(Debug, Deserialize)]
struct FillForm {
    #[serde(default)]
    fields: Vec<Field>,
}

/// Fill several fields in one call.
///
/// The saving is round trips: a form of eight fields is one call rather than
/// eight, each of which would carry a whole page outline back. The snapshot
/// comes once, at the end.
///
/// A field that fails stops the rest, and the answer says which one. Carrying
/// on would leave a form half filled with no way to tell which half.
async fn fill_form(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<FillForm>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    if args.fields.is_empty() {
        return Response::bad_arguments("no fields to fill");
    }
    let browser = page.browser().clone();

    let mut filled = Vec::new();
    for field in &args.fields {
        if field.target.trim().is_empty() {
            return Response::bad_arguments(format!(
                "the field {:?} has no target, so there is nothing to fill",
                field.name
            ));
        }
        if let Err(why) = fill_one(page, &browser, field).await {
            return Response::failed(format!(
                "{} of {} filled; {:?} could not be: {}",
                filled.len(),
                args.fields.len(),
                field.name,
                what_went_wrong(why)
            ));
        }
        filled.push(field.name.clone());
    }
    answered(page, json!({ "filled": filled })).await
}

/// Fill one field, by what kind of field it is.
///
/// Their five kinds, handled their way: a textbox and a slider are both
/// filled, a checkbox and a radio are SET rather than clicked (clicking one
/// that is already ticked turns it off, so a form filled twice would come out
/// wrong), and a combobox selects by the label a person reads.
async fn fill_one(
    page: &Page,
    browser: &crate::browser::cdp::Connection,
    field: &Field,
) -> Result<(), Response> {
    let found = locator(&field.target);
    match field.kind.as_str() {
        "checkbox" | "radio" => {
            let wanted = field.value.trim().eq_ignore_ascii_case("true");
            let state = page
                .ask(&format!(
                    "const el = s.querySelector(s.parseSelector({found}), document, true);
                     if (!el) return null;
                     const missing = await s.checkElementStates(el, ['visible','enabled']);
                     if (missing) return {{blocked: missing.missingState}};
                     return {{checked: !!el.checked}};"
                ))
                .await;
            let state = blocked_or(state)?;
            let already = state
                .get("checked")
                .and_then(Value::as_bool)
                .unwrap_or(false);
            if already == wanted {
                return Ok(());
            }
            // Clicked rather than assigned, because assigning `checked` fires
            // no events and a page listening for the change never hears it.
            let point = where_it_is(page, &found).await?;
            input::click(
                browser,
                page.session(),
                point.0,
                point.1,
                input::Click::default(),
            )
            .await
            .map_err(Response::failed)?;
            Ok(())
        }
        "combobox" => {
            // By LABEL, which is what their own fill_form asks for
            // (`selectOption({label: value})`) and is different from what their
            // select_option does. A form is filled from what a person reads.
            let outcome = page
                .ask(&format!(
                    "const el = s.querySelector(s.parseSelector({found}), document, true);
                     if (!el) return null;
                     const missing = await s.checkElementStates(el, ['visible','enabled']);
                     if (missing) return {{blocked: missing.missingState}};
                     return {{selected: s.selectOptions(el, [{{label: {}}}])}};",
                    quoted(&field.value)
                ))
                .await;
            let outcome = blocked_or(outcome)?;
            match outcome.get("selected") {
                Some(Value::Array(_)) => Ok(()),
                Some(Value::String(why)) => {
                    Err(Response::failed(format!("it could not be selected: {why}")))
                }
                _ => Err(Response::failed("the page did not say what was selected")),
            }
        }
        // Their textbox and slider, both filled. An unnamed kind is filled too
        // rather than refused: the value is text and a text field is the
        // overwhelmingly likely thing, and refusing would fail a form over a
        // word in an argument nothing else depends on.
        _ => {
            let outcome = page
                .ask(&format!(
                    "const el = s.querySelector(s.parseSelector({found}), document, true);
                     if (!el) return null;
                     const missing = await s.checkElementStates(el, ['visible','enabled','editable']);
                     if (missing) return {{blocked: missing.missingState}};
                     return {{fill: s.fill(el, {})}};",
                    quoted(&field.value)
                ))
                .await;
            let outcome = blocked_or(outcome)?;
            match outcome.get("fill").and_then(Value::as_str) {
                Some("done") => Ok(()),
                Some("needsinput") => input::insert_text(browser, page.session(), &field.value)
                    .await
                    .map_err(Response::failed),
                Some(other) => Err(Response::failed(format!("it could not be filled: {other}"))),
                None => Err(Response::failed(
                    "the page did not say whether it was filled",
                )),
            }
        }
    }
}

/// The words out of a refusal, for putting inside another one.
fn what_went_wrong(refusal: Response) -> String {
    refusal.message
}

// ------------------------------------------------------------- dragging

#[derive(Debug, Deserialize)]
struct Drag {
    #[serde(default, rename = "startTarget")]
    start_target: String,
    #[serde(default, rename = "endTarget")]
    end_target: String,
}

/// Drag one element onto another.
async fn drag(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<Drag>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    if args.start_target.trim().is_empty() || args.end_target.trim().is_empty() {
        return Response::bad_arguments(
            "dragging needs both startTarget and endTarget, so there is somewhere to drag from \
             and somewhere to drag to",
        );
    }
    // Both ends are resolved BEFORE either is touched. Resolving the second
    // after the drag has begun would read a page that is mid-drag, where a
    // target may have moved or may not exist until something is over it.
    let from = match where_it_is(page, &locator(&args.start_target)).await {
        Ok(point) => point,
        Err(why) => return why,
    };
    let to = match where_it_is(page, &locator(&args.end_target)).await {
        Ok(point) => point,
        Err(why) => return why,
    };
    if let Err(why) = input::drag(page.browser(), page.session(), from, to).await {
        return Response::failed(why);
    }
    answered(page, json!({ "dragged": true })).await
}

#[derive(Debug, Deserialize)]
struct Drop {
    #[serde(flatten)]
    at: Element,
    #[serde(default)]
    paths: Option<Vec<String>>,
    #[serde(default)]
    data: Option<HashMap<String, String>>,
}

/// Drop files or data onto an element, as if dragged in from outside.
async fn drop_onto(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<Drop>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    if args.at.target.trim().is_empty() {
        return Response::bad_arguments("no target, so there is nothing to drop onto");
    }
    let paths = args.paths.unwrap_or_default();
    let data = args.data.unwrap_or_default();
    // Their own handler's first line, and their own words for it.
    if paths.is_empty() && data.is_empty() {
        return Response::bad_arguments("At least one of \"paths\" or \"data\" must be provided.");
    }
    if let Err(why) = readable(&paths) {
        return Response::bad_arguments(why);
    }

    // The protocol's drag payload: `files` are paths the browser reads itself,
    // and `items` are the MIME-typed values a page reads off the DataTransfer.
    let mut payload = json!({
        "items": data.iter().map(|(mime, value)| json!({
            "mimeType": mime, "data": value
        })).collect::<Vec<_>>(),
        // Copy, move and link. A page asks what is allowed and refuses the drop
        // if the answer is none, so saying "any of them" is what lets an
        // ordinary drop target accept it.
        "dragOperationsMask": 1 | 2 | 4,
    });
    if !paths.is_empty() {
        payload["files"] = json!(paths);
    }

    let point = match where_it_is(page, &locator(&args.at.target)).await {
        Ok(point) => point,
        Err(why) => return why,
    };
    if let Err(why) =
        input::drop_onto(page.browser(), page.session(), point.0, point.1, payload).await
    {
        return Response::failed(why);
    }
    answered(
        page,
        json!({ "dropped": true, "files": paths.len(), "kinds": data.keys().collect::<Vec<_>>() }),
    )
    .await
}

// ----------------------------------------------------------------- files

#[derive(Debug, Deserialize)]
struct Upload {
    #[serde(default)]
    paths: Option<Vec<String>>,
}

/// Answer a file chooser the page opened.
///
/// This action only runs while the page is waiting on one, which is the gate in
/// `held_up`. The order is theirs and it is the only order that works: click
/// the thing that asks for a file FIRST, which leaves the page waiting, then
/// call this.
async fn file_upload(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<Upload>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    let paths = args.paths.unwrap_or_default();
    if let Err(why) = readable(&paths) {
        return Response::bad_arguments(why);
    }

    // Taken, not read: nobody may answer the same chooser twice, and leaving it
    // would block every later action on this page.
    let Some(Modal::FileChooser { node, multiple }) = page.records().take_modal() else {
        return Response::failed("the page is not waiting for a file");
    };
    if paths.len() > 1 && !multiple {
        return Response::bad_arguments(format!(
            "this field takes one file and {} were given",
            paths.len()
        ));
    }

    // With no paths the input is cleared, which is their "file chooser is
    // cancelled": the page stops waiting and nothing is attached.
    if let Err(why) = page
        .browser()
        .call_on(
            page.session(),
            "DOM.setFileInputFiles",
            json!({ "files": paths, "backendNodeId": node }),
        )
        .await
    {
        return Response::failed(why);
    }
    answered(page, json!({ "uploaded": paths })).await
}

/// Every path exists and is a file, checked before the browser is asked.
///
/// The browser's own refusal names neither the path nor the reason, so a model
/// given one would have to guess which of four files was wrong. This is also
/// the approval rule: a call that is going to fail should fail before anything
/// happens rather than after.
fn readable(paths: &[String]) -> Result<(), String> {
    for path in paths {
        let at = std::path::Path::new(path);
        if !at.is_absolute() {
            return Err(format!(
                "{path} is not an absolute path, and a browser is given absolute ones"
            ));
        }
        match std::fs::metadata(at) {
            Ok(found) if found.is_file() => {}
            Ok(_) => return Err(format!("{path} is a folder, not a file")),
            Err(err) => return Err(format!("{path} cannot be read: {err}")),
        }
    }
    Ok(())
}

// ----------------------------------------------------------------- waiting

#[derive(Debug, Deserialize)]
struct WaitFor {
    #[serde(default)]
    time: Option<f64>,
    #[serde(default)]
    text: Option<String>,
    #[serde(default, rename = "textGone")]
    text_gone: Option<String>,
}

/// The longest a wait may take, whichever way it was asked for.
///
/// Thirty seconds, which is theirs: `Math.min(30000, time * 1000)`. A model
/// that asks to wait an hour gets thirty seconds and is told so, rather than
/// holding a tool call open until the protocol's own timeout kills it.
const WAIT_CEILING: f64 = 30.0;

/// How often the page is asked again while waiting for text.
const LOOK_EVERY: std::time::Duration = std::time::Duration::from_millis(100);

/// Wait for text to appear, to go, or for a while.
async fn wait_for(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<WaitFor>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    let some_text = args.text.as_deref().filter(|t| !t.trim().is_empty());
    let gone_text = args.text_gone.as_deref().filter(|t| !t.trim().is_empty());
    if some_text.is_none() && gone_text.is_none() && args.time.is_none() {
        // Their own message.
        return Response::bad_arguments("Either time, text or textGone must be provided");
    }

    let mut capped = false;
    if let Some(seconds) = args.time {
        if seconds > 0.0 {
            let waiting = seconds.min(WAIT_CEILING);
            capped = waiting < seconds;
            tokio::time::sleep(std::time::Duration::from_secs_f64(waiting)).await;
        }
    }

    // Gone first, then present, which is their order. It matters on a page that
    // swaps one message for another: waiting for the new one first would return
    // the moment it appeared, with the old one still on screen.
    if let Some(text) = gone_text {
        if let Err(why) = until(page, text, false).await {
            return Response::failed(why);
        }
    }
    if let Some(text) = some_text {
        if let Err(why) = until(page, text, true).await {
            return Response::failed(why);
        }
    }

    let waited = some_text
        .or(gone_text)
        .map(str::to_string)
        .unwrap_or_else(|| format!("{}s", args.time.unwrap_or_default()));
    let mut said = json!({ "waited": waited });
    if capped {
        said["capped"] = json!(format!(
            "asked to wait {}s, which is longer than the {WAIT_CEILING}s ceiling",
            args.time.unwrap_or_default()
        ));
    }
    answered(page, said).await
}

/// Wait until some text is visible, or until it is not.
///
/// Polled rather than waited on, because there is no event for "this text
/// appeared": it can arrive by a fetch, a re-render or a class change, and the
/// only thing they have in common is that the page looks different afterwards.
///
/// Playwright's own text engine finds it (`internal:text=`, which is what
/// `getByText` compiles to), so "some words" matches the same elements here as
/// it would there: a substring, case-insensitive, whitespace normalised.
async fn until(page: &Page, text: &str, present: bool) -> Result<(), String> {
    let selector = quoted(&format!("internal:text={}i", quoted(text)));
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs_f64(WAIT_CEILING);
    loop {
        let seen = page
            .ask(&format!(
                "const all = s.querySelectorAll(s.parseSelector({selector}), document);
                 for (const el of all) {{
                   if (!(await s.checkElementStates(el, ['visible']))) return true;
                 }}
                 return false;"
            ))
            .await?;
        if seen.as_bool().unwrap_or(false) == present {
            return Ok(());
        }
        if std::time::Instant::now() >= deadline {
            return Err(if present {
                format!("{text:?} did not appear within {WAIT_CEILING}s")
            } else {
                format!("{text:?} was still on the page after {WAIT_CEILING}s")
            });
        }
        tokio::time::sleep(LOOK_EVERY).await;
    }
}

// ------------------------------------------------------------------ window

#[derive(Debug, Deserialize)]
struct Resize {
    #[serde(default)]
    width: Option<f64>,
    #[serde(default)]
    height: Option<f64>,
}

/// Change the size of the page.
///
/// There is no window to resize, so what changes is the size the page believes
/// it has: the viewport every media query, every `vh` and every responsive
/// layout is measured against. That is the whole of what resizing a window does
/// to a page, which is why this is the same tool and not an approximation of
/// one.
async fn resize(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<Resize>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    let (Some(width), Some(height)) = (args.width, args.height) else {
        return Response::bad_arguments("resizing needs both a width and a height");
    };
    if width < 1.0 || height < 1.0 {
        return Response::bad_arguments("a width and a height are both at least 1");
    }
    if let Err(why) = page
        .browser()
        .call_on(
            page.session(),
            "Emulation.setDeviceMetricsOverride",
            json!({
                "width": width as i64,
                "height": height as i64,
                // Zero means "whatever this screen uses", which is what a
                // window on this computer would have had.
                "deviceScaleFactor": 0,
                "mobile": false,
            }),
        )
        .await
    {
        return Response::failed(why);
    }

    // Waited for, and what is waited for was measured twice because the first
    // answer was wrong.
    //
    // The protocol call comes back at once and `window.innerWidth` is ALREADY
    // the new value: measured, stale 0 times in 30. So polling that, which is
    // what this did first, waits for something that is already true and is no
    // wait at all. What lags is the PAGE: its own resize handler had not run
    // in 26 of those same 30, so a caller that resized and looked straight
    // after was reading a layout the page had not redrawn.
    //
    // Two animation frames is what settles it, and that is not a guess either:
    // 28 of 30 stale without, 0 of 30 with. The first frame carries the style
    // and layout for this change, the second lands after it is committed.
    let _ = tokio::time::timeout(
        SETTLE_WITHIN,
        page.ask(
            "await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
             return true;",
        ),
    )
    .await;

    // What the page ACTUALLY has, rather than what was asked for. They agree
    // unless something like a scrollbar takes a few pixels, and answering with
    // the request would be answering with a number nobody checked.
    let has = page
        .ask("return {width: window.innerWidth, height: window.innerHeight};")
        .await
        .unwrap_or_else(|_| json!({ "width": width as i64, "height": height as i64 }));
    answered(page, has).await
}

/// The longest resizing waits for the page to redraw.
///
/// A ceiling rather than a delay: the frames normally land in milliseconds.
/// It exists because `requestAnimationFrame` is a promise the page makes, and
/// a page that never paints would otherwise hold the call until the protocol
/// gave up on it a minute later.
const SETTLE_WITHIN: std::time::Duration = std::time::Duration::from_secs(2);

// -------------------------------------------------------------------- tabs

#[derive(Debug, Deserialize)]
struct TabsArgs {
    /// Their `action`, under another name.
    ///
    /// One tool with an action argument leaves nowhere for a second one to go:
    /// this is the only tool of the twenty whose own schema declares a
    /// parameter called `action`, and ours already means which of their tools
    /// to run. Their description of it is kept word for word.
    #[serde(default)]
    operation: String,
    /// Their `index`, under another name, for the same reason: their
    /// `network_request` also has an `index` and the two count differently
    /// (tabs from 0, requests from 1), so one name could not carry both.
    #[serde(default)]
    tab: Option<usize>,
    #[serde(default)]
    url: Option<String>,
}

/// List, open, close or switch tabs.
///
/// Outside the modal-state gate, deliberately, and it is safe to be: nothing
/// here runs JavaScript in a page. Listing asks the BROWSER for each tab's
/// title and address (`Page::where_and_what`), so a tab stuck on a dialog is
/// listed rather than hung on, and being unable to walk away from a stuck page
/// would be a trap with no way out.
async fn tabs(args: Value) -> Response {
    let owner = whose(&args);
    let Ok(args) = serde_json::from_value::<TabsArgs>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    match args.operation.trim() {
        "list" => {}
        "new" => {
            let wanted = args.url.as_deref().map(str::trim).filter(|u| !u.is_empty());
            // A tab on that address already? Then that IS the new tab. Asking
            // for a second one is a caller that has lost track of what it has
            // open, and two tabs showing one page has no use: the same
            // controls, twice the memory, two sets of refs for one thing.
            if let Some(url) = wanted {
                if live::tab_showing(owner, url).await.is_some() {
                    return listing(
                        owner,
                        Some(
                            "that address was already open, so that tab is now the current one \
                             rather than a second copy of it."
                                .to_string(),
                        ),
                    )
                    .await;
                }
            }
            let fresh = match live::new_tab(owner).await {
                Ok(page) => page,
                Err(why) => return Response::failed(why),
            };
            if let Some(url) = wanted {
                if let Err(why) = fresh.goto(url).await {
                    return Response::failed(why);
                }
                live::one_tab_per_address(owner, &fresh, url).await;
            }
        }
        "close" => {
            if let Err(why) = live::close_tab(owner, args.tab).await {
                return Response::failed(why);
            }
        }
        "select" => {
            let Some(which) = args.tab else {
                // Their own message for it.
                return Response::bad_arguments("Tab index is required");
            };
            if let Err(why) = live::select_tab(owner, which).await {
                return Response::failed(why);
            }
        }
        "" => {
            return Response::bad_arguments(
                "no operation, so there is nothing to do. They are list, new, close, select.",
            )
        }
        other => {
            return Response::bad_arguments(format!(
                "\"{other}\" is not one of the tab operations. They are list, new, close, select."
            ))
        }
    }

    listing(owner, None).await
}

/// Every tab this agent has, as the tabs action answers.
///
/// Its own function because two paths answer with it: doing what was asked,
/// and declining to open a duplicate. A caller that asked for a new tab and
/// got the one it already had needs to see the same list either way.
async fn listing(owner: i64, note: Option<String>) -> Response {
    let (open, current) = live::tabs(owner).await;
    let mut listed = Vec::new();
    for (at, page) in open.iter().enumerate() {
        let (url, title) = page.where_and_what().await.unwrap_or_default();
        listed.push(json!({
            "index": at,
            "current": at == current,
            "title": title,
            "url": url,
        }));
    }
    let mut said = json!({ "tabs": listed });
    if let (Some(note), Some(object)) = (note, said.as_object_mut()) {
        object.insert("note".into(), json!(note));
    }
    Response::ok(said)
}

// ----------------------------------------------------------------- dialogs

#[derive(Debug, Deserialize)]
struct HandleDialog {
    #[serde(default)]
    accept: Option<bool>,
    #[serde(default, rename = "promptText")]
    prompt_text: Option<String>,
}

/// Answer the dialog the page is waiting on.
async fn handle_dialog(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<HandleDialog>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    let Some(accept) = args.accept else {
        return Response::bad_arguments(
            "say whether to accept the dialog: accept is true or false",
        );
    };
    // Taken before the browser is asked, so a second call cannot answer a
    // dialog that has already been answered.
    let Some(Modal::Dialog { kind, message, .. }) = page.records().take_modal() else {
        return Response::failed("the page is not waiting on a dialog");
    };

    let mut asked = json!({ "accept": accept });
    if let Some(text) = args.prompt_text.as_deref() {
        asked["promptText"] = json!(text);
    }
    if let Err(why) = page
        .browser()
        .call_on(page.session(), "Page.handleJavaScriptDialog", asked)
        .await
    {
        return Response::failed(why);
    }
    answered(
        page,
        json!({ "handled": kind, "message": message, "accepted": accept }),
    )
    .await
}

// --------------------------------------------------------------- scripting

/// The longest a script may run.
///
/// Thirty seconds, which is what Flexie's own documentation tells a caller to
/// stay under, so being stopped at thirty is the number they were given rather
/// than a surprise. It is also `wait_for`'s ceiling, so the two read as one
/// tool with one idea of how long is too long.
const SCRIPT_WITHIN: std::time::Duration = std::time::Duration::from_secs(30);

#[derive(Debug, Deserialize)]
struct ExecJs {
    #[serde(default)]
    script: Option<String>,
}

/// Run the assistant's own JavaScript in the page.
async fn exec_js(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<ExecJs>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    let Some(script) = args.script.filter(|s| !s.trim().is_empty()) else {
        return Response::bad_arguments("no script to run");
    };
    match page.run_script(&script, SCRIPT_WITHIN).await {
        // A throw inside the snippet is NOT a failure: it comes back as
        // __error and the page is still there, which is the whole reason the
        // try/catch is inside the wrapper. The snapshot below is what makes
        // that useful, because a script that got half way leaves a page worth
        // looking at.
        Ok(result) => answered(page, json!({ "js_result": result })).await,
        Err(why) => Response::failed(why),
    }
}

// -------------------------------------------------------------- signed in

/// Put a site's stored session back, if there is one and it has not been put
/// back already this run.
///
/// Once per site per run, which is not an optimisation. The browser is the
/// live truth once a site has been visited: a token it refreshed five minutes
/// ago is in the browser and not in the file, so restoring the file over it
/// would sign the session backwards.
async fn restore_session(page: &Page, url: &str) -> bool {
    let Some(state) = session::state_dir() else {
        return false;
    };
    let Some(site) = session::site_of(url) else {
        return false;
    };
    // Kept by `live`, beside the pages, because the window this is once-per is
    // one BROWSER: a new one holds nothing and everything is restorable again.
    if !live::first_visit_to(&site) {
        return false;
    }
    let Some(jar) = session::load(&state, &site) else {
        return false;
    };
    page.restore(&jar).await.is_ok()
}

/// Write this site's session to its folder.
async fn keep_session(page: &Page, url: &str) -> Result<String, String> {
    let state =
        session::state_dir().ok_or("this installation does not know where it keeps its files")?;
    let jar = page.signed_in_state(url).await?;
    if jar.cookies.is_empty() && jar.local_storage.is_empty() {
        return Err(
            "this site has nothing to keep: no cookies and no stored data, which means nothing \
             signed in. Sign in first, then save."
                .to_string(),
        );
    }
    let site = jar.site.clone();
    session::save(&state, &jar)?;
    Ok(site)
}

/// Keep this site signed in, past the end of the application.
async fn save_session(page: &Page) -> Response {
    let (url, _) = match page.where_and_what().await {
        Ok(at) => at,
        Err(why) => return Response::failed(why),
    };
    if url.trim().is_empty() || url.starts_with("about:") {
        return Response::bad_arguments(
            "there is no site open to save. Navigate to the site and sign in first.",
        );
    }
    match keep_session(page, &url).await {
        Ok(site) => Response::ok(json!({
            "saved": site,
            "note": "this sign-in is kept on this computer and put back the next time the site \
                     is opened, including after the application is closed. Use forget_session to \
                     sign out.",
        })),
        Err(why) => Response::bad_arguments(why),
    }
}

#[derive(Debug, Deserialize)]
struct ForgetArgs {
    #[serde(default)]
    url: Option<String>,
}

/// Forget a site's stored sign-in. This is what signing out is.
async fn forget_session(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<ForgetArgs>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    let Some(state) = session::state_dir() else {
        return Response::failed("this installation does not know where it keeps its files");
    };
    // The site named, or the one this page is on.
    let url = match args.url.as_deref().map(str::trim).filter(|u| !u.is_empty()) {
        Some(url) => url.to_string(),
        None => match page.where_and_what().await {
            Ok((url, _)) => url,
            Err(why) => return Response::failed(why),
        },
    };
    let Some(site) = session::site_of(&url) else {
        return Response::bad_arguments(
            "no site to forget. Give a url, or do this on the site's own page.",
        );
    };
    let had = session::forget(&state, &site);
    Response::ok(json!({
        "forgotten": had,
        "site": site,
        "note": if had {
            "the saved sign-in is gone from this computer. The page open right now is still \
             signed in until it is closed or navigated away from."
        } else {
            "there was no saved sign-in for that site."
        },
    }))
}

// ---------------------------------------------------------------- the record

#[derive(Debug, Deserialize)]
struct ConsoleArgs {
    #[serde(default)]
    level: Option<String>,
    #[serde(default)]
    all: Option<bool>,
}

/// What the console has said.
///
/// No snapshot on this one, nor on the two network actions. They report what
/// has already happened and change nothing, so a page outline with each would
/// be several kilobytes answering a question nobody asked.
async fn console_messages(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<ConsoleArgs>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    let level = args.level.unwrap_or_else(|| "info".to_string());
    if !crate::browser::record::LEVELS.contains(&level.as_str()) {
        return Response::bad_arguments(format!(
            "\"{level}\" is not a level. They are {}.",
            crate::browser::record::LEVELS.join(", ")
        ));
    }

    let (lines, dropped) = page.records().console(&level, args.all.unwrap_or(false));
    let messages: Vec<Value> = lines
        .iter()
        .map(|line| {
            json!({
                "level": line.level,
                "type": line.kind,
                "text": line.text,
                "at": line.at,
            })
        })
        .collect();
    let mut said = json!({ "messages": messages, "level": level });
    note_gaps(&mut said, page, dropped, "console lines");
    Response::ok(said)
}

#[derive(Debug, Deserialize)]
struct RequestsArgs {
    #[serde(default, rename = "static")]
    include_static: Option<bool>,
    #[serde(default)]
    filter: Option<String>,
}

/// What the page has fetched.
async fn network_requests(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<RequestsArgs>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    let matching = match args.filter.as_deref().filter(|f| !f.is_empty()) {
        Some(pattern) => match regex::Regex::new(pattern) {
            Ok(built) => Some(built),
            // Their schema refuses an unreadable expression before the call
            // runs. Ours says which part of it the engine could not read, which
            // is the correctable half.
            Err(err) => {
                return Response::bad_arguments(format!(
                    "{pattern:?} is not a usable regular expression: {err}"
                ))
            }
        },
        None => None,
    };

    let (exchanges, dropped) = page.records().requests(false);
    let want_static = args.include_static.unwrap_or(false);
    let mut listed = Vec::new();
    let mut hidden = 0usize;
    for exchange in &exchanges {
        // Their rule: a successful thing the page did not ask for itself (an
        // image, a font, a stylesheet) is noise unless it is wanted.
        if !want_static && !exchange.is_fetch() && exchange.succeeded() {
            hidden += 1;
            continue;
        }
        if let Some(matching) = &matching {
            if !matching.is_match(&exchange.url) {
                continue;
            }
        }
        listed.push(json!({
            "index": exchange.index,
            "method": exchange.method,
            "url": exchange.url,
            "type": exchange.resource,
            "status": exchange.status,
            "statusText": exchange.status_text,
            "failed": exchange.failure,
        }));
    }

    let mut said = json!({ "requests": listed });
    if hidden > 0 {
        said["hidden_static"] = json!(hidden);
        said["note"] = json!(format!(
            "{hidden} static request{} not shown; pass static: true to see {}.",
            if hidden == 1 { "" } else { "s" },
            if hidden == 1 { "it" } else { "them" }
        ));
    }
    note_gaps(&mut said, page, dropped, "requests");
    Response::ok(said)
}

/// The parts of one request that can be asked for on their own.
const PARTS: [&str; 4] = [
    "request-headers",
    "request-body",
    "response-headers",
    "response-body",
];

#[derive(Debug, Deserialize)]
struct RequestArgs {
    #[serde(default)]
    index: Option<u64>,
    #[serde(default)]
    part: Option<String>,
}

/// One request in full, or one part of it.
async fn network_request(page: &Page, args: Value) -> Response {
    let Ok(args) = serde_json::from_value::<RequestArgs>(args) else {
        return Response::bad_arguments("the arguments are not the shape this tool takes");
    };
    let Some(index) = args.index else {
        return Response::bad_arguments(
            "no index. Use the number from network_requests to say which one.",
        );
    };
    let part = args.part.unwrap_or_default();
    if !part.is_empty() && !PARTS.contains(&part.as_str()) {
        return Response::bad_arguments(format!(
            "\"{part}\" is not a part of a request. They are {}.",
            PARTS.join(", ")
        ));
    }
    let Some(exchange) = page.records().request(index) else {
        let (all, dropped) = page.records().requests(true);
        return Response::bad_arguments(match (all.first(), dropped) {
            (Some(first), dropped) if dropped > 0 && index < first.index => format!(
                "request {index} is no longer kept: the oldest {dropped} were dropped and this \
                 page now remembers from {} onwards",
                first.index
            ),
            _ => format!(
                "there is no request {index}. Use network_requests to see which numbers there are."
            ),
        });
    };

    // The body is fetched now rather than kept, which is why it can be gone:
    // the browser keeps a response body only while it has room for it. Reported
    // as what it is, because an empty body and a body nobody kept are different
    // answers.
    let body = |want: bool| async move {
        if !want {
            return Value::Null;
        }
        match page
            .browser()
            .call_on(
                page.session(),
                "Network.getResponseBody",
                json!({ "requestId": exchange.id }),
            )
            .await
        {
            Ok(got) => {
                let encoded = got
                    .get("base64Encoded")
                    .and_then(Value::as_bool)
                    .unwrap_or(false);
                let text = got.get("body").and_then(Value::as_str).unwrap_or_default();
                if encoded {
                    json!({ "base64": text })
                } else {
                    json!(text)
                }
            }
            Err(why) => json!({ "unavailable": why }),
        }
    };

    match part.as_str() {
        "request-headers" => Response::ok(json!({ "headers": exchange.request_headers })),
        "request-body" => Response::ok(json!({ "body": exchange.post_data })),
        "response-headers" => Response::ok(json!({ "headers": exchange.response_headers })),
        "response-body" => Response::ok(json!({ "body": body(true).await })),
        _ => Response::ok(json!({
            "index": exchange.index,
            "method": exchange.method,
            "url": exchange.url,
            "type": exchange.resource,
            "status": exchange.status,
            "statusText": exchange.status_text,
            "mimeType": exchange.mime,
            "failed": exchange.failure,
            "requestHeaders": exchange.request_headers,
            "requestBody": exchange.post_data,
            "responseHeaders": exchange.response_headers,
            "responseBody": body(true).await,
        })),
    }
}

/// Say what is missing from an answer, when anything is.
///
/// Two different gaps, and both are worth admitting. `dropped` is the ceiling
/// doing its job, which is known exactly. `missed` is the recorder having
/// fallen behind the browser, which is the one gap it cannot describe: those
/// events are gone and it does not know what they were.
fn note_gaps(said: &mut Value, page: &Page, dropped: u64, what: &str) {
    let Some(object) = said.as_object_mut() else {
        return;
    };
    if dropped > 0 {
        object.insert(
            "dropped".into(),
            json!(format!(
                "{dropped} older {what} were dropped to stay within what a page keeps"
            )),
        );
    }
    let missed = page.records().missed();
    if missed > 0 {
        object.insert(
            "incomplete".into(),
            json!(format!(
                "the recorder fell {missed} events behind the browser, so something may be missing"
            )),
        );
    }
}

// -------------------------------------------------------------------- shared

/// What `target` means: a reference from the last snapshot, or a selector.
///
/// Their own wording, and the order matters. A reference is theirs to resolve
/// (`aria-ref=` against the snapshot they remember), and it is the reliable
/// one because it names exactly one element. Anything else goes to their
/// selector parser as written, which is what makes `#email` work.
fn locator(target: &str) -> String {
    let target = target.trim();
    if is_reference(target) {
        return quoted(&format!("aria-ref={target}"));
    }
    quoted(target)
}

/// Whether this looks like a reference the snapshot handed out.
///
/// `e` and then digits, which is the shape their snapshot writes
/// (`[ref=e12]`). Anything else is treated as a selector, which is the safe
/// way round: a selector that happens to look like a reference would be a
/// strange selector, where a reference treated as a selector finds nothing and
/// the refusal says so.
fn is_reference(target: &str) -> bool {
    let Some(digits) = target.strip_prefix('e') else {
        return false;
    };
    !digits.is_empty() && digits.chars().all(|c| c.is_ascii_digit())
}

/// Where an element is, having checked it can be acted on.
///
/// Playwright's script does all the deciding: it finds the element, checks it
/// is visible, stable and enabled, scrolls it into view, and reports the point
/// to aim at. The error half comes back as a ready-made Response, because the
/// three outcomes (not there, not actionable, page broke) each read differently
/// to a model.
async fn where_it_is(page: &Page, found: &str) -> Result<(f64, f64), Response> {
    let ready = page
        .ask(&format!(
            "const el = s.querySelector(s.parseSelector({found}), document, true);
             if (!el) return null;
             const missing = await s.checkElementStates(el, ['visible','stable','enabled']);
             if (missing) return {{blocked: missing.missingState}};
             el.scrollIntoViewIfNeeded ? el.scrollIntoViewIfNeeded() : el.scrollIntoView({{block:'center'}});
             const r = el.getBoundingClientRect();
             return {{x: r.x + r.width / 2, y: r.y + r.height / 2}};"
        ))
        .await;
    let point = blocked_or(ready)?;
    let (Some(x), Some(y)) = (
        point.get("x").and_then(Value::as_f64),
        point.get("y").and_then(Value::as_f64),
    ) else {
        return Err(Response::failed("the page did not say where that is"));
    };
    Ok((x, y))
}

/// Turn what an element expression answered into either a value or a refusal.
///
/// Three cases, and they are genuinely different things to tell a model: the
/// selector matched nothing, the element is there but cannot be acted on (with
/// Playwright's own word for which state is missing), or the page threw.
fn blocked_or(answer: Result<Value, String>) -> Result<Value, Response> {
    match answer {
        Ok(Value::Null) => Err(Response::failed(
            "nothing on this page matches that. Take another snapshot: the page may have \
             navigated, or the control may only appear after something else.",
        )),
        Ok(value) => {
            if let Some(state) = value.get("blocked").and_then(Value::as_str) {
                return Err(Response::failed(format!(
                    "it cannot be used yet: it is not {state}"
                )));
            }
            Ok(value)
        }
        Err(why) => Err(Response::failed(why)),
    }
}

/// The outline of a page, or of one element of it.
///
/// `mode: 'ai'` is not decoration: it is what makes the snapshot carry a `ref`
/// for each control, and those references are what `target` takes. Without it
/// the outline names things a model then has no way to point at.
async fn outline(
    page: &Page,
    target: &str,
    depth: Option<u32>,
    boxes: Option<bool>,
) -> Result<Value, String> {
    let root = if target.trim().is_empty() {
        "document.body".to_string()
    } else {
        format!(
            "s.querySelector(s.parseSelector({}), document, true)",
            locator(target)
        )
    };
    let mut options = json!({ "mode": "ai" });
    if let Some(depth) = depth {
        options["depth"] = json!(depth);
    }
    if let Some(true) = boxes {
        options["boxes"] = json!(true);
    }
    page.ask(&format!(
        "const el = {root}; return el ? s.ariaSnapshot(el, {options}) : null;"
    ))
    .await
}

/// Finish an action by answering with what it did AND a fresh outline.
///
/// What their own server does. The extra round trip buys the model a whole
/// turn: after clicking something it can see the result without asking.
async fn answered(page: &Page, mut did: Value) -> Response {
    match outline(page, "", None, None).await {
        Ok(text) => {
            did = with_snapshot(did, text);
            Response::ok(did)
        }
        // The action happened. A page that cannot be read afterwards (it
        // navigated somewhere that is still loading, it closed itself) must not
        // turn a successful click into a failure.
        Err(_) => Response::ok(did),
    }
}

/// Where a page ended up, plus its outline.
async fn landed(page: &Page) -> Response {
    let at = page
        .ask("return {url: location.href, title: document.title};")
        .await;
    let at = match at {
        Ok(at) => at,
        Err(why) => return Response::failed(why),
    };
    answered(page, at).await
}

/// Add an outline to an answer, trimmed to what will fit.
fn with_snapshot(mut answer: Value, text: Value) -> Value {
    let text = text.as_str().unwrap_or_default();
    let (kept, complete) = trimmed(text);
    if let Some(object) = answer.as_object_mut() {
        object.insert("snapshot".into(), json!(kept));
        if !complete {
            object.insert("snapshot_complete".into(), json!(false));
        }
    }
    answer
}

fn nothing_matches(target: &str) -> String {
    if target.trim().is_empty() {
        return "this page could not be read".to_string();
    }
    format!("nothing on this page matches {}", target.trim())
}

/// A string, as JavaScript would write it.
///
/// Through the JSON encoder rather than by wrapping it in quotes: a target is a
/// model's text and can hold a quote, a backslash or a newline, each of which
/// ends the expression early and turns a bad target into a syntax error
/// somewhere inside a three hundred kilobyte script.
fn quoted(value: &str) -> String {
    serde_json::to_string(value).unwrap_or_else(|_| "\"\"".to_string())
}

/// The values for a dropdown, in the shape their script matches on.
///
/// NOT a list of strings, and the difference is silent. Their `selectOptions`
/// matches each option against fields on the thing it is given
/// (`valueOrLabel`, `value`, `label`, `index`), and a bare string has none of
/// them: every field check is skipped, `matches` stays true, and the FIRST
/// option in the dropdown is taken. Asking for "two" quietly selected "one",
/// reported success, and answered with the value it had actually taken, which
/// is the only reason the test caught it.
///
/// `valueOrLabel` is what their own driver wraps a string in
/// (`convertSelectOptionValues`), and it means what the schema's "values"
/// loosely means: match the option's value OR the label a person reads.
fn quoted_values(values: &[String]) -> String {
    let wrapped: Vec<Value> = values
        .iter()
        .map(|value| json!({ "valueOrLabel": value }))
        .collect();
    serde_json::to_string(&wrapped).unwrap_or_else(|_| "[]".to_string())
}

/// Cut an answer to what will fit, and say whether anything was cut.
fn trimmed(text: &str) -> (String, bool) {
    if text.len() <= MOST_TEXT {
        return (text.to_string(), true);
    }
    // On a character boundary: a page is UTF-8, and cutting mid-character
    // produces bytes that are not text.
    let mut end = MOST_TEXT;
    while end > 0 && !text.is_char_boundary(end) {
        end -= 1;
    }
    (text[..end].to_string(), false)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A reference is resolved through their engine; anything else is a
    /// selector.
    ///
    /// Both directions matter. A reference sent as a raw selector finds
    /// nothing, and a selector mangled into `aria-ref=` finds nothing either,
    /// and in both cases the model is told the page does not have something it
    /// can see in the snapshot.
    #[test]
    fn a_reference_and_a_selector_are_told_apart() {
        assert_eq!(locator("e12"), "\"aria-ref=e12\"");
        assert_eq!(locator("  e3  "), "\"aria-ref=e3\"", "it should be trimmed");
        assert_eq!(locator("#email"), "\"#email\"");
        assert_eq!(locator("text=Save"), "\"text=Save\"");
        // Things that merely start with e are not references.
        assert_eq!(locator("email"), "\"email\"");
        assert_eq!(locator("e"), "\"e\"");
        assert_eq!(locator("e12a"), "\"e12a\"");
        assert!(is_reference("e1") && is_reference("e999"));
        assert!(!is_reference("") && !is_reference("12") && !is_reference("#e1"));
    }

    /// A target with a quote in it cannot end the expression it is put in.
    ///
    /// Targets are model text and routinely contain quotes: every
    /// `internal:role=button[name="Save"i]` has two. Concatenated naively the
    /// first closes the string and the rest becomes JavaScript, failing inside
    /// Playwright's bundle with a message about a token nobody wrote.
    #[test]
    fn a_target_cannot_break_out_of_the_expression() {
        assert_eq!(
            quoted(r#"internal:role=button[name="Save"i]"#),
            "\"internal:role=button[name=\\\"Save\\\"i]\""
        );
        assert_eq!(quoted("back\\slash"), "\"back\\\\slash\"");
        let hostile = quoted(r#"a"); alert(1); ("#);
        assert!(
            !hostile.contains("alert(1);\""),
            "the payload escaped its string"
        );
    }

    /// Dropdown values are wrapped the way their script matches on.
    ///
    /// A bare list of strings is the silent failure this prevents: their
    /// matcher checks fields that a string does not have, concludes every
    /// option matches, and takes the first one. There is no error, and the
    /// answer even names the value it took, so only comparing that value
    /// against what was asked for reveals it.
    #[test]
    fn dropdown_values_are_wrapped_so_their_matcher_sees_them() {
        assert_eq!(
            quoted_values(&["two".to_string()]),
            r#"[{"valueOrLabel":"two"}]"#
        );
        assert_eq!(
            quoted_values(&["a".to_string(), "b".to_string()]),
            r#"[{"valueOrLabel":"a"},{"valueOrLabel":"b"}]"#
        );
        // And a value with a quote in it still survives the wrapping.
        assert_eq!(
            quoted_values(&["a\"b".to_string()]),
            r#"[{"valueOrLabel":"a\"b"}]"#
        );
    }

    /// A long outline is cut on a character boundary and says it was cut.
    #[test]
    fn a_long_outline_is_cut_honestly() {
        let (kept, all) = trimmed("short");
        assert_eq!(kept, "short");
        assert!(all);

        let long = "é".repeat(MOST_TEXT);
        let (kept, all) = trimmed(&long);
        assert!(!all, "an outline over the ceiling was reported as complete");
        assert!(kept.len() <= MOST_TEXT);
        assert!(
            kept.chars().all(|c| c == 'é'),
            "the cut landed inside a character"
        );

        // And the flag reaches the answer, which is the part a model reads.
        let answer = with_snapshot(json!({"clicked": true}), json!(long));
        assert_eq!(answer["snapshot_complete"], false);
        assert_eq!(answer["clicked"], true, "the action's own answer was lost");
    }

    /// Every action is dispatched, and anything else is refused by name.
    ///
    /// The refusal is the half worth asserting: one tool with an action
    /// argument means a wrong action is a string the model chose, and the
    /// answer has to be correctable in one turn. A bare "no" is not.
    #[tokio::test]
    async fn an_action_that_is_not_one_is_refused_by_name() {
        let refused = run(json!({ "action": "screenshot" })).await;
        let said = format!("{refused:?}");
        assert!(said.contains("screenshot"), "it should name what was asked");
        assert!(said.contains("snapshot"), "and list what there is");

        let refused = run(json!({})).await;
        assert!(
            format!("{refused:?}").contains("no action"),
            "a call with no action should say so"
        );
    }

    /// The twenty actions are Playwright's twenty tool names.
    ///
    /// Theirs with `browser_` taken off, which is the compatibility claim at
    /// this level: the gateway's schema enumerates the same twenty, and a test
    /// there holds them as literals against their record.
    #[test]
    fn the_actions_are_playwrights_tools_without_the_prefix() {
        assert_eq!(ACTIONS.len(), 23);
        for action in ACTIONS {
            assert!(!action.starts_with("browser_"), "{action} keeps the prefix");
            assert!(!action.is_empty());
        }
        assert!(ACTIONS.contains(&"navigate_back"));
        assert!(ACTIONS.contains(&"select_option"));
        assert!(ACTIONS.contains(&"network_request"));
        assert!(ACTIONS.contains(&"handle_dialog"));
    }

    /// The two actions that answer a waiting page are the same two everywhere.
    ///
    /// They are used in two places that must agree: the gate that lets them
    /// through while the page is blocked, and the race that must NOT cut them
    /// off. One function feeds both, and this is what says so, because the
    /// first cut had them as two separate lists and `handle_dialog` reported
    /// the dialog it was in the middle of answering.
    #[test]
    fn the_actions_that_answer_a_waiting_page_are_one_list() {
        assert!(answers_a_question("handle_dialog"));
        assert!(answers_a_question("file_upload"));
        for action in ACTIONS {
            if action == "handle_dialog" || action == "file_upload" {
                continue;
            }
            assert!(
                !answers_a_question(action),
                "{action} would be let through the gate and then cut off by the race"
            );
        }
    }
}
