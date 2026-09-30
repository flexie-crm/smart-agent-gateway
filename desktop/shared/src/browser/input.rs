//! Clicking and typing.
//!
//! This is the half Playwright's injected script deliberately does not do.
//! Theirs decides WHAT (which element) and WHETHER (is it visible, enabled,
//! still, and would a click land on it); the protocol does the doing. Their
//! bundle has no `click` in it at all, and filling a text field returns
//! `needsinput` with the field selected and nothing typed, which is the seam
//! stated in their own code.
//!
//! **Why not dispatch events from JavaScript.** A `new MouseEvent(...)` is
//! `isTrusted: false`. Real sites tell the difference, some native behaviour
//! does not fire at all, and the failures are the worst kind: a form that
//! looks clicked and was not. Going through the protocol produces the same
//! events a person's mouse does, which was measured rather than assumed:
//! `isTrusted` came back true.

use serde_json::{json, Value};

use super::cdp::Connection;
use super::keys;

/// Click at a point in the page.
///
/// Pressed and released as two messages, which is what the protocol has and
/// what a real click is. `clickCount` matters: at zero, a page listening for
/// `click` never hears one, because the browser only synthesises that from a
/// press and release that belong to the same click.
pub async fn click(
    browser: &Connection,
    session: &str,
    x: f64,
    y: f64,
    how: Click<'_>,
) -> Result<(), String> {
    // Moved first. A page that shows a menu on hover, or a button that only
    // arms itself once the pointer is over it, needs the move to have happened;
    // a press with no preceding move is a click from nowhere.
    browser
        .call_on(
            session,
            "Input.dispatchMouseEvent",
            json!({
                "type": "mouseMoved", "x": x, "y": y, "button": "none",
                "modifiers": modifiers(how.modifiers),
            }),
        )
        .await?;
    for kind in ["mousePressed", "mouseReleased"] {
        browser
            .call_on(
                session,
                "Input.dispatchMouseEvent",
                json!({
                    "type": kind, "x": x, "y": y,
                    "button": how.button,
                    // Two for a double click, which is what the browser turns
                    // into a dblclick event. Sending two single clicks does
                    // not: a page listening for dblclick hears nothing.
                    "clickCount": if how.double { 2 } else { 1 },
                    "modifiers": modifiers(how.modifiers),
                }),
            )
            .await?;
    }
    Ok(())
}

/// How to click: the parameters Playwright's own schema declares.
#[derive(Debug, Clone, Copy)]
pub struct Click<'a> {
    /// "left", "right" or "middle". Their enum, passed through.
    pub button: &'a str,
    pub double: bool,
    pub modifiers: &'a [String],
}

impl Default for Click<'_> {
    fn default() -> Self {
        Self {
            button: "left",
            double: false,
            modifiers: &[],
        }
    }
}

/// The modifier keys as one number, which is how the protocol takes them.
///
/// Through `keys`, because a mouse event and a key event take the same mask and
/// two of it would be two things to get wrong. `ControlOrMeta` is resolved
/// there too: it means "the key people use for shortcuts on this computer",
/// which is Command on a Mac and Control everywhere else, and sending the wrong
/// one is a shortcut a page ignores rather than an error anybody sees.
fn modifiers(named: &[String]) -> u32 {
    let resolved: Vec<String> = named.iter().map(|name| keys::resolve(name)).collect();
    keys::mask(&resolved)
}

/// Move the pointer over a point, without pressing anything.
///
/// Its own function rather than a flag on `click`, because it is a different
/// thing: a great many pages only reveal a menu while the pointer is over
/// something, and the whole point is NOT clicking.
pub async fn hover(browser: &Connection, session: &str, x: f64, y: f64) -> Result<(), String> {
    browser
        .call_on(
            session,
            "Input.dispatchMouseEvent",
            json!({ "type": "mouseMoved", "x": x, "y": y, "button": "none" }),
        )
        .await?;
    Ok(())
}

/// Drag from one point to another.
///
/// What Playwright's `dragTo` does, in the order it does it: put the pointer on
/// the source, press, move onto the target, and release. The move happens
/// TWICE, which is not belt and braces: many drag implementations only decide
/// a drop is possible on the second `dragover` at a position, and a single move
/// leaves the target un-armed so the release does nothing.
pub async fn drag(
    browser: &Connection,
    session: &str,
    from: (f64, f64),
    to: (f64, f64),
) -> Result<(), String> {
    let at = |kind: &str, point: (f64, f64), button: &str| json!({ "type": kind, "x": point.0, "y": point.1, "button": button, "clickCount": 1 });
    browser
        .call_on(
            session,
            "Input.dispatchMouseEvent",
            at("mouseMoved", from, "none"),
        )
        .await?;
    browser
        .call_on(
            session,
            "Input.dispatchMouseEvent",
            at("mousePressed", from, "left"),
        )
        .await?;
    for _ in 0..2 {
        browser
            .call_on(
                session,
                "Input.dispatchMouseEvent",
                at("mouseMoved", to, "left"),
            )
            .await?;
    }
    browser
        .call_on(
            session,
            "Input.dispatchMouseEvent",
            at("mouseReleased", to, "left"),
        )
        .await?;
    Ok(())
}

/// Drop files or data onto a point, as if dragged in from outside the page.
///
/// A different mechanism from `drag`, and it has to be: there is no source
/// element and no mouse press, because the drag began somewhere the browser is
/// not (a file manager, another application). The protocol has a drag event of
/// its own that carries the payload, which is the only way to give a page a
/// file it did not ask for through an input.
///
/// Three events rather than one: a page decides whether it will accept a drop
/// during `dragEnter` and `dragOver`, and one that is sent only `drop` refuses
/// it, because as far as it knows nothing was ever dragged over it.
pub async fn drop_onto(
    browser: &Connection,
    session: &str,
    x: f64,
    y: f64,
    data: Value,
) -> Result<(), String> {
    for kind in ["dragEnter", "dragOver", "drop"] {
        browser
            .call_on(
                session,
                "Input.dispatchDragEvent",
                json!({ "type": kind, "x": x, "y": y, "data": data }),
            )
            .await?;
    }
    Ok(())
}

/// Type text into whatever has focus.
///
/// `Input.insertText` rather than a key event per character. It is what
/// Playwright's own fill does after its script hands back `needsinput`, and it
/// is right for text: a key event per character would be a key event per
/// character, which for an accented letter or an emoji is not one key.
///
/// What it does NOT do is fire per-key handlers, which is why pressing a key is
/// a separate thing below.
pub async fn insert_text(browser: &Connection, session: &str, text: &str) -> Result<(), String> {
    browser
        .call_on(session, "Input.insertText", json!({ "text": text }))
        .await?;
    Ok(())
}

/// Type text one character at a time, as their `slowly` asks for.
///
/// What it is FOR is the page that listens per keystroke: a search box that
/// filters as you type, a field that validates on each character, an editor
/// with a shortcut handler. `insert_text` puts the whole value in at once and
/// such a page sees one event, or none.
pub async fn type_slowly(browser: &Connection, session: &str, text: &str) -> Result<(), String> {
    for character in text.chars() {
        press(browser, session, &character.to_string()).await?;
    }
    Ok(())
}

/// Press one key, or a chord.
///
/// Every event comes from `keys::pressing`, which is Playwright's own `press`:
/// the modifiers down, the key down and up, the modifiers up in reverse, with
/// each event carrying what is held at the time. All this does is send them in
/// order.
pub async fn press(browser: &Connection, session: &str, key: &str) -> Result<(), String> {
    for step in keys::pressing(key)? {
        browser
            .call_on(session, "Input.dispatchKeyEvent", step.event)
            .await?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The modifiers a click is given become the number a click carries.
    ///
    /// Their five names against the protocol's four bits, which is the whole of
    /// what this file decides about keys: `keys` owns the rest. An unknown name
    /// contributes nothing rather than being guessed at, which matters because
    /// their schema enumerates five and this is only reachable if that list
    /// grows.
    #[test]
    fn the_modifiers_a_click_holds_become_one_number() {
        assert_eq!(modifiers(&[]), 0);
        assert_eq!(modifiers(&["Alt".into()]), 1);
        assert_eq!(modifiers(&["Control".into()]), 2);
        assert_eq!(modifiers(&["Meta".into()]), 4);
        assert_eq!(modifiers(&["Shift".into()]), 8);
        assert_eq!(modifiers(&["Control".into(), "Shift".into()]), 10);
        assert_eq!(modifiers(&["Nonsense".into()]), 0);

        // And this computer's own shortcut key, which is the one name that is
        // not a bit of its own.
        let wanted = if cfg!(target_os = "macos") { 4 } else { 2 };
        assert_eq!(modifiers(&["ControlOrMeta".into()]), wanted);
    }
}
