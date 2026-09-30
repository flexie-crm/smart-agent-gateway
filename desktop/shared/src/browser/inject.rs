//! Putting Playwright's script into a page, and asking it things.
//!
//! The script is theirs, held in `desktop/lib/playwright` and compiled in. What
//! is here is the small amount a host has to provide: the eight options it is
//! constructed with, a page to put it in, and the calls that reach it.
//!
//! **It is installed on every document, before the page's own scripts run.**
//! `Page.addScriptToEvaluateOnNewDocument` is what makes that true for
//! navigations we did not cause: a redirect, a form post, a frame. Evaluating it
//! after a load instead would work until the first page that navigates itself,
//! and then the next call would find nothing there.
//!
//! **And it is re-checked before every use.** A page can replace its own
//! document, and any number of ordinary things (a `document.write`, a bfcache
//! restore) leave a window where the script is not yet there. Asking whether it
//! exists costs one round trip against a tool call that is about to cost
//! several, and the alternative is an error the model cannot act on.

use serde_json::{json, Value};

use super::assets;
use super::cdp::Connection;

// Playwright's injected script comes from `assets`, which owns the question of
// where any of this browser's JavaScript lives: compiled in by default, read
// from a directory when somebody is changing it. Spelling an include path out
// here would put that decision in as many places as there are scripts.

/// What we call the thing once it is in the page.
///
/// Deliberately not a name anybody would pick by accident, because it shares a
/// global object with whatever the page itself defines. A page that happened to
/// use the same name would break this, and the failure would look like the
/// script having vanished.
const HANDLE: &str = "__sagInjected";

/// The options the script is constructed with, and why each is what it is.
///
/// Eight, read at `injectedScript.js:6668`. Playwright builds the same object
/// at `coreBundle.js:19210`; this is that object for a host that is not their
/// driver.
fn options() -> Value {
    json!({
        // Their own test mode, which among other things hangs the script off a
        // second global. We are not their test suite.
        "isUnderTest": false,
        // Which language errors are phrased for. Their selector errors quote
        // example code, and "javascript" is the only one whose phrasing is not
        // actively wrong somewhere else in this product.
        "sdkLanguage": "javascript",
        // The attribute getByTestId looks at. Their default, because a product
        // choosing its own here would find nothing on anybody else's site.
        "testIdAttributeName": "data-testid",
        // How many animation frames an element must hold still for before it
        // counts as stable. ONE, which is what every engine but Firefox on
        // Windows uses (rafCountForStablePosition, four implementations in
        // coreBundle.js return 1). It still costs two frames in practice: the
        // first records a position and the second compares it.
        "stableRafCount": 1,
        // Which engine, for the handful of workarounds they keep per browser.
        // Ours is Chromium and saying anything else would turn on corrections
        // for bugs this browser does not have.
        "browserName": "chromium",
        // Whether to put their own prefix on error messages. We read the
        // messages ourselves and pass them on in our own words.
        "shouldPrependErrorPrefix": false,
        // Whether this is running in an isolated world. It is NOT: the protocol
        // installs it in the page's own world, which is where a page's own
        // scripts live. Saying otherwise would be a lie the script acts on.
        "isUtilityWorld": false,
        // Selector engines somebody registered. We register none.
        "customEngines": []
    })
}

/// The whole bootstrap, exactly as Playwright assembles it.
///
/// `coreBundle.js:19223`: a bare `module` object, their script, and the class it
/// exports constructed with the page's globals and the options. The only thing
/// added is the name it is kept under.
fn bootstrap() -> String {
    format!(
        "window.{HANDLE} = (() => {{\n  const module = {{}};\n{}\n  return new (module.exports.InjectedScript())(globalThis, {});\n}})();",
        assets::injected_script(),
        options()
    )
}

/// Install the script on a page, for this document and every one after it.
pub async fn install(browser: &Connection, session: &str) -> Result<(), String> {
    browser
        .call_on(
            session,
            "Page.addScriptToEvaluateOnNewDocument",
            json!({ "source": bootstrap() }),
        )
        .await?;
    Ok(())
}

/// Make sure the script is in the document that is loaded RIGHT NOW.
///
/// `addScriptToEvaluateOnNewDocument` applies to the next document, not the one
/// already open, so a page that was loaded before the script was installed has
/// nothing in it. Rather than ordering the two carefully and hoping, this asks
/// and puts it there if it is missing, which is also the answer to a page that
/// replaced its own document afterwards.
pub async fn installed(browser: &Connection, session: &str) -> Result<bool, String> {
    let answer = browser
        .call_on(
            session,
            "Runtime.evaluate",
            json!({ "expression": format!("typeof window.{HANDLE}"), "returnByValue": true }),
        )
        .await?;
    Ok(answer.pointer("/result/value").and_then(Value::as_str) == Some("object"))
}

/// Make sure the script is in the document that is loaded RIGHT NOW, putting it
/// there if it is not.
///
/// Split from `installed` so that the repair cannot hide a broken install. It
/// did: a test that navigated a page to a second document and then asked a
/// question still passed with `install` replaced by a one-off evaluate, because
/// this function quietly put the script back before the question was asked.
/// Asking and repairing are two facts, and a test that wants the first must be
/// able to get it without the second.
pub async fn present(browser: &Connection, session: &str) -> Result<(), String> {
    if installed(browser, session).await? {
        return Ok(());
    }
    let put = browser
        .call_on(
            session,
            "Runtime.evaluate",
            json!({ "expression": bootstrap(), "returnByValue": false }),
        )
        .await?;
    if let Some(thrown) = described(&put) {
        return Err(format!(
            "the page would not take the automation script: {thrown}"
        ));
    }
    Ok(())
}

/// Ask the injected script something, and bring back a plain value.
///
/// `expression` is JavaScript with the script available as `s`, which keeps the
/// callers readable: `s.querySelector(s.parseSelector(sel), document, true)`.
///
/// Awaited, always. Several of their methods are async (anything that waits for
/// an element to settle), and without this the answer would be a promise the
/// protocol serialises as an empty object, which reads as "it worked and found
/// nothing".
pub async fn ask(browser: &Connection, session: &str, expression: &str) -> Result<Value, String> {
    present(browser, session).await?;
    let answer = browser
        .call_on(
            session,
            "Runtime.evaluate",
            json!({
                "expression": format!("(async (s) => {{ {expression} }})(window.{HANDLE})"),
                "awaitPromise": true,
                "returnByValue": true,
            }),
        )
        .await?;
    if let Some(thrown) = described(&answer) {
        // Their own words. A selector that names an engine nobody registered,
        // or a strict-mode violation listing what it matched, is something the
        // model can act on; "the call failed" is not.
        return Err(thrown);
    }
    Ok(answer
        .pointer("/result/value")
        .cloned()
        .unwrap_or(Value::Null))
}

/// Run the assistant's own JavaScript in the page.
///
/// A different wrapper from `ask` on purpose, and the difference is the
/// contract. `ask` is ours: it is handed the injected script and its caller is
/// this codebase. This is somebody else's code, written by a model, and the
/// shape it is promised is Flexie's, which has been in use long enough to be
/// worth copying rather than inventing:
///
///   - the snippet is the BODY of an async function, so plain statements work
///     and a bare `return` sends a value back;
///   - `await` works at the top level, because the body is already inside an
///     async function;
///   - a throw comes back as `__error` and does NOT abort the call, so what is
///     left on the page can still be read afterwards.
///
/// The last one is why the try/catch is here and not left to the protocol. A
/// thrown exception reported as a failure loses the page state the snippet
/// created before it threw, which on a script that clicked something is the
/// difference between "it went through" and nobody knowing.
pub async fn run_script(
    browser: &Connection,
    session: &str,
    body: &str,
    within: std::time::Duration,
) -> Result<Value, String> {
    let wrapped = format!(
        "(async () => {{ try {{ {body} \n }} catch (e) {{          return {{ __error: String((e && e.stack) || e) }}; }} }})()"
    );
    let asked = browser.call_on(
        session,
        "Runtime.evaluate",
        json!({
            "expression": wrapped,
            "awaitPromise": true,
            "returnByValue": true,
        }),
    );
    // Bounded here rather than left to the protocol's own minute. A snippet
    // that waits forever is the common mistake, and their own documentation
    // tells a caller to keep it under thirty seconds, so being told at thirty
    // is the answer that matches what the caller was told.
    let answer = match tokio::time::timeout(within, asked).await {
        Ok(answer) => answer?,
        Err(_) => {
            return Err(format!(
                "the script was still running after {}s and was given up on. Scripts that wait \
                 should poll on a bounded timer rather than blocking.",
                within.as_secs()
            ))
        }
    };
    if let Some(thrown) = described(&answer) {
        // A syntax error rather than a throw: the catch above never ran,
        // because the code never became a function.
        return Err(format!(
            "that script could not run: {thrown}. The snippet is the body of an async function, \
             so write plain statements and use a bare return."
        ));
    }
    Ok(answer
        .pointer("/result/value")
        .cloned()
        .unwrap_or(Value::Null))
}

/// What went wrong inside the page, if anything did.
///
/// The protocol reports a thrown exception as a SUCCESSFUL call carrying
/// `exceptionDetails`, which is the trap: a caller that checks only for a
/// protocol error treats every thrown error as a result, and the result it then
/// reads is null.
fn described(answer: &Value) -> Option<String> {
    let details = answer.get("exceptionDetails")?;
    let said = details
        .pointer("/exception/description")
        .and_then(Value::as_str)
        .or_else(|| details.get("text").and_then(Value::as_str))
        .unwrap_or("the page threw something with no description");
    // The first line. Their errors carry a stack, and a stack through a
    // three hundred kilobyte injected script helps nobody reading a tool result.
    Some(said.lines().next().unwrap_or(said).to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The script we compile in is the one we think it is.
    ///
    /// It is somebody else's generated file, updated by re-running an
    /// extraction against a new release, and nothing about their bundle
    /// promises to keep any of this. If they rename the export or drop a
    /// method, the honest outcome is a failure here rather than an application
    /// whose browser tools quietly stop finding anything.
    #[test]
    fn the_injected_script_is_what_we_build_against() {
        let injected = assets::injected_script();
        assert!(
            injected.len() > 250_000,
            "the injected script is {} bytes, which is too small to be it",
            injected.len()
        );
        assert!(
            injected.contains("InjectedScript: () => InjectedScript"),
            "the export we construct is not in the script"
        );
        // A sample from each thing we are relying on it for. Not every method:
        // this is a tripwire for a bundle that has moved, not a specification.
        for method in [
            "parseSelector",
            "querySelector",
            "querySelectorAll",
            "elementState",
            "checkElementStates",
            "expectHitTarget",
            "fill",
            "ariaSnapshot",
        ] {
            assert!(
                injected.contains(&format!("{method}(")),
                "the injected script has no {method}, which we call"
            );
        }
    }

    /// Every option the script reads is one we pass.
    ///
    /// The constructor reads eight, and an option it does not find is
    /// `undefined` rather than an error: `stableRafCount` missing makes the
    /// stability check compare against nothing, which is a hang, and
    /// `testIdAttributeName` missing breaks getByTestId with no message. So the
    /// list is asserted against the script's own reads.
    #[test]
    fn every_option_the_script_reads_is_one_we_pass() {
        let injected = assets::injected_script();
        let passed = options();
        let passed = passed.as_object().expect("the options are an object");
        for option in [
            "isUnderTest",
            "sdkLanguage",
            "testIdAttributeName",
            "stableRafCount",
            "browserName",
            "shouldPrependErrorPrefix",
            "isUtilityWorld",
            "customEngines",
        ] {
            assert!(
                injected.contains(&format!("options.{option}")),
                "we pass {option} and the script never reads it"
            );
            assert!(
                passed.contains_key(option),
                "the script reads options.{option} and we do not pass it"
            );
        }
        assert_eq!(passed.len(), 8, "the option list has changed size");
    }

    /// The bootstrap is assembled the way their driver assembles it.
    #[test]
    fn the_bootstrap_has_the_shape_the_script_expects() {
        let boot = bootstrap();
        assert!(boot.contains("const module = {}"), "no module object");
        assert!(
            boot.contains("new (module.exports.InjectedScript())(globalThis,"),
            "the script is not constructed the way it expects"
        );
        assert!(boot.contains(HANDLE), "the script is not kept anywhere");
        assert!(
            boot.len() > assets::injected_script().len(),
            "the script is not in the bootstrap"
        );
    }

    /// A thrown error is not read as a result.
    ///
    /// The protocol answers a throw with a SUCCESSFUL call carrying
    /// exceptionDetails, so a caller looking only at `result.value` reads null
    /// and reports that the page has no such element. This is the check that
    /// tells the two apart.
    #[test]
    fn something_thrown_in_the_page_is_not_a_null_answer() {
        let thrown = json!({
            "result": {"type": "object", "subtype": "error"},
            "exceptionDetails": {
                "text": "Uncaught",
                "exception": {"description": "Error: strict mode violation\n  at <anonymous>:1:1"}
            }
        });
        let said = described(&thrown).expect("a throw should be described");
        assert_eq!(
            said, "Error: strict mode violation",
            "the stack should be cut"
        );

        let fine = json!({ "result": {"type": "string", "value": "DIV"} });
        assert!(
            described(&fine).is_none(),
            "an ordinary answer was read as a throw"
        );
    }
}
