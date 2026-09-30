//! Playwright's script, in our browser, answering about a real page.
//!
//! This is what phase 2 is for. The unit tests check that the script we compile
//! in is the one we think it is; none of them loads it, and a bundle can be
//! entirely present and still fail to construct, because the contract is not
//! only "these strings appear" but "this class takes these options in this
//! runtime".
//!
//! So this starts the real browser, opens a real page, puts their script into
//! it, and asks it the questions the tools above will ask. If Playwright
//! reorganise their bundle, this is what fails.

use std::path::PathBuf;

use sag_desktop::browser::{cdp::Connection, page::Page, supervise};

/// A page with one of everything the selector engines are for.
const PAGE: &str = r#"<!doctype html><meta charset="utf-8"><title>Proof</title>
<label for="who">Your name</label><input id="who" placeholder="Type it here">
<button disabled>Save</button>
<button>Cancel</button>
<div data-testid="note">a note</div>
<div id="hidden" style="display:none">not visible</div>
"#;

fn browser() -> PathBuf {
    let path = std::env::var("SAG_BROWSER_PATH").expect(
        "SAG_BROWSER_PATH is not set, so this gate would prove nothing. \
         Run it through `make browser-e2e`.",
    );
    let path = PathBuf::from(path);
    assert!(
        path.is_file(),
        "SAG_BROWSER_PATH is not a file: {}",
        path.display()
    );
    path
}

/// Start a browser, connect, and open a page on the fixture.
async fn a_page_on_the_fixture(name: &str) -> (Connection, Page, PathBuf) {
    let exe = browser();
    let dir = std::env::temp_dir().join(format!("sag-inject-{}-{}", name, std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).expect("make the test directory");
    let fixture = dir.join("page.html");
    std::fs::write(&fixture, PAGE).expect("write the fixture");

    let endpoint = supervise::ensure(&exe, &dir.join("profile"))
        .await
        .expect("the browser should start");
    let browser = Connection::open(&endpoint.websocket)
        .await
        .expect("the browser should accept a connection");
    let page = Page::open(&browser).await.expect("a page should open");
    page.goto(&format!("file://{}", fixture.display()))
        .await
        .expect("the fixture should load");
    (browser, page, dir)
}

/// Find one element and read a property off it.
///
/// Taken as functions rather than closures because a closure capturing `page`
/// moves it into the first async block it makes, and every assertion after that
/// is a borrow of something already gone.
async fn query(page: &Page, selector: &str, read: &str) -> Result<serde_json::Value, String> {
    page.ask(&format!(
        "const el = s.querySelector(s.parseSelector({}), document, true); return el ? el.{read} : null;",
        serde_json::to_string(selector).unwrap()
    ))
    .await
}

/// Ask their actionability check about one element.
async fn states(page: &Page, selector: &str, want: &str) -> Result<serde_json::Value, String> {
    page.ask(&format!(
        "const el = s.querySelector(s.parseSelector({}), document, true); \
         const r = await s.checkElementStates(el, {want}); \
         return r === undefined ? 'all passed' : JSON.stringify(r);",
        serde_json::to_string(selector).unwrap()
    ))
    .await
}

/// Their selector engines find things in our browser.
///
/// One assertion per engine the tools will be built on, because they fail
/// independently: a bundle that dropped the role engine would still answer for
/// text, and a single "it works" assertion would pass on half a script.
#[tokio::test]
async fn playwrights_selector_engines_answer_in_our_browser() {
    let (_browser, page, dir) = a_page_on_the_fixture("selectors").await;

    // getByRole
    assert_eq!(
        query(
            &page,
            r#"internal:role=button[name="Cancel"i]"#,
            "outerHTML"
        )
        .await
        .unwrap(),
        "<button>Cancel</button>"
    );
    // getByLabel
    assert_eq!(
        query(&page, r#"internal:label="Your name"i"#, "id")
            .await
            .unwrap(),
        "who"
    );
    // getByPlaceholder
    assert_eq!(
        query(
            &page,
            r#"internal:attr=[placeholder="Type it here"i]"#,
            "id"
        )
        .await
        .unwrap(),
        "who"
    );
    // getByTestId
    assert_eq!(
        query(
            &page,
            r#"internal:testid=[data-testid="note"s]"#,
            "textContent"
        )
        .await
        .unwrap(),
        "a note"
    );
    // getByText
    assert_eq!(query(&page, "text=a note", "tagName").await.unwrap(), "DIV");
    // and plain CSS, which everything else is layered on
    assert_eq!(query(&page, "#who", "id").await.unwrap(), "who");

    let _ = page.close().await;
    supervise::stop().await;
    let _ = std::fs::remove_dir_all(&dir);
}

/// Their actionability checks answer, and answer DIFFERENTLY for different
/// elements.
///
/// The second half is the point. `checkElementStates` returns undefined when
/// every state passes, which serialises as null, and a test that only asserted
/// null would pass just as well against a script that had stopped working
/// entirely. So a disabled button is asked the same question and has to come
/// back with the state it is missing.
#[tokio::test]
async fn playwrights_actionability_answers_and_can_say_no() {
    let (_browser, page, dir) = a_page_on_the_fixture("states").await;

    assert_eq!(
        states(
            &page,
            r#"internal:role=button[name="Cancel"i]"#,
            r#"["visible","stable","enabled"]"#
        )
        .await
        .unwrap(),
        "all passed",
        "an ordinary button failed one of the checks"
    );
    assert_eq!(
        states(
            &page,
            r#"internal:role=button[name="Save"i]"#,
            r#"["visible","enabled"]"#
        )
        .await
        .unwrap(),
        r#"{"missingState":"enabled"}"#,
        "a disabled button passed the enabled check, so the check is not running"
    );
    assert_eq!(
        states(&page, "#hidden", r#"["visible"]"#).await.unwrap(),
        r#"{"missingState":"visible"}"#,
        "a display:none element passed the visible check"
    );

    let _ = page.close().await;
    supervise::stop().await;
    let _ = std::fs::remove_dir_all(&dir);
}

/// Strict mode reaches us as an error rather than as a quiet first match.
///
/// This is the behaviour a tool surface depends on: a selector matching two
/// things must not silently act on one of them. It also proves the error path,
/// which the protocol reports as a SUCCESSFUL call carrying exceptionDetails.
#[tokio::test]
async fn an_ambiguous_selector_is_refused_with_their_own_words() {
    let (_browser, page, dir) = a_page_on_the_fixture("strict").await;

    let why = page
        .ask("return s.querySelector(s.parseSelector('internal:role=button'), document, true).tagName;")
        .await
        .expect_err("two buttons match, so strict mode should refuse");
    assert!(
        why.contains("strict mode violation"),
        "the refusal should carry their words: {why}"
    );
    assert!(
        !why.contains('\n'),
        "the stack should have been cut off: {why}"
    );

    let _ = page.close().await;
    supervise::stop().await;
    let _ = std::fs::remove_dir_all(&dir);
}

/// The script is in a document the page navigated ITSELF to, without anything
/// having to put it back.
///
/// `addScriptToEvaluateOnNewDocument` is what makes that true, and it is why
/// the script is installed rather than evaluated once: a page that redirects,
/// submits a form, or is sent somewhere by its own JavaScript gets a fresh
/// document, and a one-off evaluate would leave every document after the first
/// with nothing in it.
///
/// It asks `inject::installed`, which only looks, and NOT `page.ask`, which
/// repairs. The first version of this used `ask` and passed with the install
/// replaced by a one-off evaluate, because the repair put the script back
/// before the question was asked. It was testing the safety net.
#[tokio::test]
async fn the_script_is_installed_into_a_document_the_page_navigated_to_itself() {
    let (browser, page, dir) = a_page_on_the_fixture("navigate").await;
    let second = dir.join("second.html");
    std::fs::write(
        &second,
        "<!doctype html><title>Second</title><button>Onwards</button>",
    )
    .expect("write the second page");

    page.goto(&format!("file://{}", second.display()))
        .await
        .expect("the second page should load");

    assert!(
        sag_desktop::browser::inject::installed(&browser, page.session())
            .await
            .expect("the page should answer"),
        "the script is not in a document the page navigated to on its own, so it was put \
         there once rather than installed for every document"
    );

    // And it works there, which is the other half: present is not the same as
    // constructed, and a script that threw on the way up would also be absent.
    let found = page
        .ask(
            "const el = s.querySelector(s.parseSelector('internal:role=button[name=\"Onwards\"i]'), document, true); \
             return el ? el.textContent : null;",
        )
        .await
        .expect("the script should answer in the new document");
    assert_eq!(found, "Onwards");

    let _ = page.close().await;
    supervise::stop().await;
    let _ = std::fs::remove_dir_all(&dir);
}

/// The outline can hand out REFERENCES, and they can be acted on.
///
/// This is Playwright's own answer to the thing that makes a browser tool hard
/// to use: their selector syntax. `ariaSnapshot` with refs marks every
/// actionable element with a short id, and `aria-ref=` resolves that id back to
/// the element. Read the page, get a list of what is on it with an id each, act
/// on an id. No selector syntax anywhere.
///
/// Checked here before anything is built on it.
#[tokio::test]
async fn the_outline_can_hand_out_references_that_can_be_acted_on() {
    let (_browser, page, dir) = a_page_on_the_fixture("refs").await;

    let outline = page
        .ask("return s.ariaSnapshot(document.body, {mode: 'ai'});")
        .await
        .expect("an outline with refs")
        .as_str()
        .unwrap_or_default()
        .to_string();
    eprintln!("--- outline with refs ---\n{outline}\n---");

    assert!(
        outline.contains("[ref=e"),
        "the outline carries no references:\n{outline}"
    );

    // Find the reference the outline gave the Cancel button, the way a model
    // reading this would.
    let line = outline
        .lines()
        .find(|l| l.contains("\"Cancel\""))
        .expect("Cancel is in the outline");
    let reference = line
        .split("[ref=")
        .nth(1)
        .and_then(|rest| rest.split(']').next())
        .expect("Cancel carries a reference");
    eprintln!("Cancel is {reference}");

    // And it resolves back to the element, with no selector syntax involved.
    let found = page
        .ask(&format!(
            "const el = s.querySelector(s.parseSelector('aria-ref={reference}'), document, true); \
             return el ? el.outerHTML : null;"
        ))
        .await
        .expect("the reference should resolve");
    assert_eq!(
        found, "<button>Cancel</button>",
        "the reference did not resolve to the element it named"
    );

    let _ = page.close().await;
    supervise::stop().await;
    let _ = std::fs::remove_dir_all(&dir);
}
