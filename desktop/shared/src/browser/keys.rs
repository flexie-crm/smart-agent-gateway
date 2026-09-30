//! The keyboard: what a key name means, and what a chord does.
//!
//! Playwright's own documentation for pressing a key names `F1` - `F12`,
//! `Digit0` - `Digit9`, `KeyA` - `KeyZ`, `Backquote`, `Minus`, `Equal`,
//! `Backslash`, `PageDown` and the rest, and says that `Control+o` and
//! `Control+Shift+T` work. That text ships to the model as the tool's
//! documentation, so it has to be TRUE, and the only way for it to be true is
//! to use the table it is documenting.
//!
//! So both tables are theirs, shipped beside their injected script:
//! `usKeyboardLayout.json` is every key and its numbers, and
//! `macEditingCommands.json` is what a key combination MEANS to a Mac. This
//! module is their `buildLayoutClosure`, `_keyDescriptionForString`, `press`
//! and `_commandsForCode`, in Rust, keeping the behaviour those four have:
//!
//!   - a key is reachable by its code (`KeyA`), by the character it types
//!     (`a`), by the character it types with Shift held (`A`), and by the
//!     aliases they define (`Shift` for `ShiftLeft`, `\n` for `Enter`);
//!   - with Shift held, a key resolves to its shifted self, so `Shift+1` types
//!     `!` and not `1`;
//!   - with any other modifier held, the key carries no text, so the event is
//!     a `rawKeyDown` rather than a `keyDown` (see `describe` for what that
//!     was measured to do, and not do);
//!   - a chord presses its modifiers, presses and releases the key, and
//!     releases the modifiers in reverse.
//!
//! One thing is deliberately not theirs: `ControlOrMeta` is resolved with
//! `cfg!(target_os)` rather than by asking a runtime for a platform string.

use std::collections::HashMap;
use std::sync::OnceLock;

use serde::Deserialize;
use serde_json::{json, Value};

use super::assets;

/// The keypad, which is location 3 in the protocol.
const KEYPAD: i64 = 3;

/// One key as Playwright's layout file writes it.
#[derive(Debug, Deserialize)]
struct Definition {
    #[serde(default)]
    key: String,
    #[serde(default)]
    #[serde(rename = "keyCode")]
    key_code: i64,
    #[serde(default)]
    #[serde(rename = "keyCodeWithoutLocation")]
    key_code_without_location: Option<i64>,
    #[serde(default)]
    #[serde(rename = "shiftKey")]
    shift_key: Option<String>,
    #[serde(default)]
    #[serde(rename = "shiftKeyCode")]
    shift_key_code: Option<i64>,
    #[serde(default)]
    text: Option<String>,
    #[serde(default)]
    location: i64,
}

/// One key, resolved: what to send for it.
#[derive(Debug, Clone, PartialEq)]
pub struct Key {
    pub key: String,
    pub code: String,
    pub key_code: i64,
    pub key_code_without_location: i64,
    pub text: String,
    pub location: i64,
    /// The same key with Shift held, where that is a different character.
    shifted: Option<Box<Key>>,
}

/// The four modifiers a key event carries.
///
/// Their `kModifiers`, in their order, which is also the order
/// `_commandsForCode` builds a shortcut name in.
const MODIFIERS: [&str; 4] = ["Shift", "Control", "Alt", "Meta"];

/// Their aliases: another name for a key that is already in the table.
const ALIASES: [(&str, &[&str]); 5] = [
    ("ShiftLeft", &["Shift"]),
    ("ControlLeft", &["Control"]),
    ("AltLeft", &["Alt"]),
    ("MetaLeft", &["Meta"]),
    ("Enter", &["\n", "\r"]),
];

/// The layout, read once.
fn layout() -> &'static HashMap<String, Key> {
    static LAYOUT: OnceLock<HashMap<String, Key>> = OnceLock::new();
    LAYOUT.get_or_init(|| closure(&assets::keyboard_layout()))
}

/// The editing commands, read once.
fn commands() -> &'static HashMap<String, Vec<String>> {
    static COMMANDS: OnceLock<HashMap<String, Vec<String>>> = OnceLock::new();
    COMMANDS
        .get_or_init(|| serde_json::from_str(&assets::mac_editing_commands()).unwrap_or_default())
}

/// Their `buildLayoutClosure`: one map holding every name a key answers to.
fn closure(source: &str) -> HashMap<String, Key> {
    // An ORDERED read, because the map is built by writing later entries over
    // earlier ones and their file's order is what decides which key owns a
    // character. `Digit1` comes before `Numpad1`, so `1` is the top-row one.
    let Ok(defined) =
        serde_json::from_str::<Vec<(String, Definition)>>(&ordered(source).unwrap_or_default())
    else {
        return HashMap::new();
    };

    let mut resolved = HashMap::new();
    for (code, definition) in defined {
        let mut key = Key {
            key: definition.key.clone(),
            code: code.clone(),
            key_code: definition.key_code,
            key_code_without_location: definition
                .key_code_without_location
                .unwrap_or(definition.key_code),
            text: definition.text.clone().unwrap_or_default(),
            location: definition.location,
            shifted: None,
        };
        // A key whose name is one character types that character.
        if definition.key.chars().count() == 1 {
            key.text = definition.key.clone();
        }
        let shifted = definition.shift_key.as_ref().map(|shift| {
            let mut other = key.clone();
            other.key = shift.clone();
            other.text = shift.clone();
            if let Some(code) = definition.shift_key_code {
                other.key_code = code;
            }
            other
        });

        let mut whole = key.clone();
        whole.shifted = shifted.clone().map(Box::new);
        resolved.insert(code.clone(), whole);

        for (of, names) in ALIASES {
            if of == code {
                for name in names {
                    resolved.insert((*name).to_string(), key.clone());
                }
            }
        }

        // A key that is one of a pair (left Shift and right Shift, the keypad)
        // is reachable by its code only. Registering it by character too would
        // have the keypad's `1` answer for the `1` somebody meant.
        if definition.location != 0 {
            continue;
        }
        if key.key.chars().count() == 1 {
            resolved.insert(key.key.clone(), key.clone());
        }
        if let Some(shifted) = shifted {
            resolved.insert(shifted.key.clone(), shifted);
        }
    }
    resolved
}

/// Read a JSON object as a list of pairs, keeping its order.
///
/// `serde_json` gives an object as a map, and a map has no order. The order is
/// load-bearing here (see `closure`), so the text is re-read as an array of
/// pairs through the streaming reader, which does preserve it.
fn ordered(source: &str) -> Option<String> {
    let value: Value = serde_json::from_str(source).ok()?;
    let object = value.as_object()?;
    let pairs: Vec<Value> = object
        .iter()
        .map(|(name, definition)| json!([name, definition]))
        .collect();
    serde_json::to_string(&pairs).ok()
}

/// `ControlOrMeta`, resolved: the key people use for shortcuts on THIS
/// computer.
///
/// Their `resolveSmartModifierString`. Sending Control on a Mac is sending the
/// wrong key, and a page ignores a shortcut it was not sent rather than
/// failing, so the mistake would be silent.
pub fn resolve_smart(name: &str) -> &str {
    if name == "ControlOrMeta" {
        if cfg!(target_os = "macos") {
            return "Meta";
        }
        return "Control";
    }
    name
}

/// Their `press` split: the last piece is the key and the rest are modifiers.
///
/// Character by character rather than `split('+')`, which is their own
/// implementation and is what makes `Control++` mean Control and the plus key:
/// a `+` only ends a piece when there is something in it.
fn split(chord: &str) -> Vec<String> {
    let mut pieces = Vec::new();
    let mut building = String::new();
    for character in chord.chars() {
        if character == '+' && !building.is_empty() {
            pieces.push(std::mem::take(&mut building));
        } else {
            building.push(character);
        }
    }
    pieces.push(building);
    pieces
}

/// Their `_keyDescriptionForString`: what to send for a key name, given what
/// is already held down.
///
/// Two rules, both theirs, and both about text rather than about which key it
/// is. With Shift held, a key becomes its shifted self. With any other
/// modifier held, the key carries no text at all, which makes the event a
/// `rawKeyDown` instead of a `keyDown`.
///
/// What the second rule does NOT do, measured rather than assumed: it is not
/// what stops `Control+o` from typing an `o`. Removing the blanking and
/// pressing `Control+o` into a real field over the real protocol left the
/// field unchanged, because the modifier MASK is what tells Chromium the key
/// is a shortcut; the same key with an empty mask and the same text inserts
/// the character. So this is kept because it is theirs and because the event
/// kind is part of what a page can read, not because it is load-bearing.
fn describe(name: &str, held: &[String]) -> Option<Key> {
    let mut key = layout().get(resolve_smart(name))?.clone();
    let shift = held.iter().any(|m| m == "Shift");
    if shift {
        if let Some(shifted) = key.shifted.clone() {
            key = *shifted;
        }
    }
    if held.len() > 1 || (held.len() == 1 && !shift) {
        key.text.clear();
    }
    key.shifted = None;
    Some(key)
}

/// `ControlOrMeta` resolved, as an owned name.
///
/// For a caller holding a list of modifier names to turn into a mask: the mask
/// only knows the four real ones.
pub fn resolve(name: &str) -> String {
    resolve_smart(name).to_string()
}

/// Whether a name is one of the four modifiers.
fn is_modifier(name: &str) -> bool {
    MODIFIERS.contains(&resolve_smart(name))
}

/// The modifiers held down, as the one number the protocol takes.
///
/// Their `toModifiersMask`.
pub fn mask(held: &[String]) -> u32 {
    let mut mask = 0;
    for name in held {
        mask |= match name.as_str() {
            "Alt" => 1,
            "Control" => 2,
            "Meta" => 4,
            "Shift" => 8,
            _ => 0,
        };
    }
    mask
}

/// What a key combination MEANS on a Mac, for Chromium to carry out.
///
/// Their `_commandsForCode`. On macOS the editing operation is named alongside
/// the key, because on a real Mac the operating system is what decides that
/// Command+Left goes to the start of the line: send the key alone and it
/// registers and edits nothing. The two edits are theirs as well: a command
/// that inserts is dropped (Chromium does the inserting from the key's own
/// text) and the trailing colon of the Objective-C selector name is cut.
fn editing_commands(code: &str, held: &[String]) -> Vec<String> {
    if !cfg!(target_os = "macos") {
        return Vec::new();
    }
    let mut parts: Vec<&str> = MODIFIERS
        .iter()
        .filter(|modifier| held.iter().any(|name| name == *modifier))
        .copied()
        .collect();
    parts.push(code);
    let shortcut = parts.join("+");
    commands()
        .get(&shortcut)
        .map(|named| {
            named
                .iter()
                .filter(|command| !command.starts_with("insert"))
                .map(|command| command.trim_end_matches(':').to_string())
                .collect()
        })
        .unwrap_or_default()
}

/// One key event, as the protocol takes it.
///
/// Their `RawKeyboardImpl.keydown` and `.keyup`, field for field. The type is
/// the one that matters: a key carrying text is a `keyDown` (Chromium inserts
/// the character) and one carrying none is a `rawKeyDown` (it does not).
pub fn event(down: bool, key: &Key, held: &[String]) -> Value {
    if !down {
        return json!({
            "type": "keyUp",
            "modifiers": mask(held),
            "key": key.key,
            "windowsVirtualKeyCode": key.key_code_without_location,
            "code": key.code,
            "location": key.location,
        });
    }
    json!({
        "type": if key.text.is_empty() { "rawKeyDown" } else { "keyDown" },
        "modifiers": mask(held),
        "windowsVirtualKeyCode": key.key_code_without_location,
        "code": key.code,
        "commands": editing_commands(&key.code, held),
        "key": key.key,
        "text": key.text,
        "unmodifiedText": key.text,
        "autoRepeat": false,
        "location": key.location,
        "isKeypad": key.location == KEYPAD,
    })
}

/// One step of pressing something: an event, and what is held after it.
#[derive(Debug, Clone)]
pub struct Step {
    pub event: Value,
    /// Only for saying what went wrong, which needs the name rather than the
    /// event.
    pub name: String,
}

/// Every event a key or a chord sends, in order.
///
/// Their `press`: each modifier down, the key down, the key up, each modifier
/// up in reverse. The modifiers accumulate as they go, so the key's own event
/// carries all of them and its text is blanked by the rule in `describe`.
///
/// Nothing is sent from here. The caller sends, which keeps this pure and
/// testable: what a chord DOES is a list of events, and that list is the thing
/// worth asserting.
pub fn pressing(chord: &str) -> Result<Vec<Step>, String> {
    let pieces = split(chord);
    let Some((key, modifiers)) = pieces.split_last() else {
        return Err("no key to press".to_string());
    };
    for name in modifiers {
        if !is_modifier(name) {
            return Err(format!(
                "\"{name}\" is not a modifier key. The modifiers are {}.",
                MODIFIERS.join(", ")
            ));
        }
    }

    let mut held: Vec<String> = Vec::new();
    let mut steps = Vec::new();
    for name in modifiers {
        let described = describe(name, &held).ok_or_else(|| unknown(name))?;
        held.push(resolve_smart(name).to_string());
        steps.push(Step {
            event: event(true, &described, &held),
            name: name.clone(),
        });
    }

    let described = describe(key, &held).ok_or_else(|| unknown(key))?;
    steps.push(Step {
        event: event(true, &described, &held),
        name: key.clone(),
    });
    steps.push(Step {
        event: event(false, &described, &held),
        name: key.clone(),
    });

    for name in modifiers.iter().rev() {
        let described = describe(name, &held).ok_or_else(|| unknown(name))?;
        held.retain(|down| down != resolve_smart(name));
        steps.push(Step {
            event: event(false, &described, &held),
            name: name.clone(),
        });
    }
    Ok(steps)
}

/// What to say about a key name the layout does not have.
///
/// Their message is `Unknown key: "x"`. This one adds where the names come
/// from, because the caller is a model that can correct itself from a sentence
/// and cannot read the layout file.
fn unknown(name: &str) -> String {
    format!(
        "\"{name}\" is not a key. A key is a single character (`a`), a key name \
         (`Enter`, `ArrowLeft`, `F5`, `PageDown`), or a code (`KeyA`, `Digit1`), \
         optionally with modifiers (`Control+o`, `Control+Shift+T`)."
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Every key name Playwright's own documentation lists is a key.
    ///
    /// This is the test the copied documentation needs. That text goes to the
    /// model as the tool's guide, and every name in it is a promise: a model
    /// that presses `PageDown` because the guide said it could and is told
    /// `PageDown` is not a key has been lied to. The list is transcribed from
    /// their doc comment for `press`.
    #[test]
    fn every_key_their_documentation_names_exists() {
        let mut named: Vec<String> = vec![
            "Backquote",
            "Minus",
            "Equal",
            "Backslash",
            "Backspace",
            "Tab",
            "Delete",
            "Escape",
            "ArrowDown",
            "End",
            "Enter",
            "Home",
            "Insert",
            "PageDown",
            "PageUp",
            "ArrowRight",
            "ArrowUp",
            "ArrowLeft",
        ]
        .into_iter()
        .map(String::from)
        .collect();
        for n in 1..=12 {
            named.push(format!("F{n}"));
        }
        for n in 0..=9 {
            named.push(format!("Digit{n}"));
        }
        for letter in 'A'..='Z' {
            named.push(format!("Key{letter}"));
        }
        // And the modifiers, which the same paragraph lists.
        for modifier in [
            "Shift",
            "Control",
            "Alt",
            "Meta",
            "ShiftLeft",
            "ControlOrMeta",
        ] {
            named.push(modifier.to_string());
        }

        for name in named {
            assert!(
                describe(&name, &[]).is_some(),
                "{name} is in Playwright's documentation and not in the layout"
            );
        }
    }

    /// A chord is split their way, so `Control++` is Control and plus.
    #[test]
    fn a_chord_splits_the_way_theirs_does() {
        assert_eq!(split("a"), vec!["a"]);
        assert_eq!(split("Control+o"), vec!["Control", "o"]);
        assert_eq!(split("Control+Shift+T"), vec!["Control", "Shift", "T"]);
        // Their own documented example, and the reason this is not `split('+')`:
        // that would give ["Control", "", ""] and a key of nothing.
        assert_eq!(split("Control++"), vec!["Control", "+"]);
    }

    /// Pressing a chord holds the modifier around the key and lets go after.
    #[test]
    fn a_chord_holds_its_modifier_around_the_key() {
        let steps = pressing("Control+o").expect("Control+o is a chord");
        let names: Vec<&str> = steps.iter().map(|s| s.name.as_str()).collect();
        assert_eq!(names, vec!["Control", "o", "o", "Control"]);
        let kinds: Vec<&str> = steps
            .iter()
            .map(|s| s.event["type"].as_str().unwrap_or_default())
            .collect();
        assert_eq!(
            kinds,
            vec!["rawKeyDown", "rawKeyDown", "keyUp", "keyUp"],
            "a modifier and a shortcut key both type nothing"
        );

        // The key's own event carries the modifier, which is what a page reads
        // to know the shortcut was pressed rather than the letter.
        assert_eq!(steps[1].event["modifiers"], 2, "Control is 2");
        assert_eq!(steps[1].event["code"], "KeyO");
        // And it types NOTHING. This is the rule that matters: with text, the
        // browser inserts an `o` into whatever has focus as well as firing the
        // shortcut.
        assert_eq!(steps[1].event["text"], "");
        // Let go at the end, or the next press is silently modified.
        assert_eq!(steps[3].event["modifiers"], 0);
    }

    /// Shift makes a key its shifted self, and still types.
    ///
    /// Shift is the one modifier that does not blank the text, because
    /// shifting is how a character is typed rather than a shortcut fired.
    ///
    /// Which character comes back is their behaviour and is worth knowing,
    /// because it is not the obvious one: a key named by its CODE has a
    /// shifted self and a key named by its CHARACTER does not. Their
    /// `buildLayoutClosure` puts the shifted variant only on the entry keyed
    /// by code, so `Shift+Digit1` is `!` and `Shift+1` is `1`. That is why
    /// their documentation's own examples are codes (`Digit0` - `Digit9`,
    /// `KeyA` - `KeyZ`) where it talks about Shift. We match them rather than
    /// improving on them: a caller that knows Playwright would be surprised by
    /// anything else, and `Shift+1` typing `!` here and `1` there is a
    /// difference nobody could debug.
    #[test]
    fn shift_types_the_shifted_character() {
        let steps = pressing("Shift+Digit1").expect("Shift+Digit1 is a chord");
        let key = &steps[1].event;
        assert_eq!(
            key["key"], "!",
            "Shift+Digit1 should be an exclamation mark"
        );
        assert_eq!(key["text"], "!");
        assert_eq!(
            key["type"], "keyDown",
            "it types, so it is not a rawKeyDown"
        );
        assert_eq!(
            key["code"], "Digit1",
            "the code is the key that was pressed"
        );
        assert_eq!(key["modifiers"], 8, "Shift is 8");

        // A letter too, which is what their documentation says: "Holding down
        // `Shift` will type the text that corresponds to the `key` in the
        // upper case."
        let steps = pressing("Shift+KeyA").expect("Shift+KeyA is a chord");
        assert_eq!(steps[1].event["text"], "A");

        // And the half of their arrangement that surprises: a key named by
        // the character it types has no shifted self, so this is a `1`.
        let steps = pressing("Shift+1").expect("Shift+1 is a chord");
        assert_eq!(
            steps[1].event["text"], "1",
            "a by-character key has no shifted variant in their layout"
        );

        // Two modifiers blank it again, even with Shift among them.
        let steps = pressing("Control+Shift+T").expect("their own example");
        let key = &steps[2].event;
        assert_eq!(key["code"], "KeyT");
        assert_eq!(key["text"], "", "two modifiers means no text");
        assert_eq!(key["modifiers"], 2 | 8);
    }

    /// A single key is one down and one up, and Enter types a return.
    ///
    /// The return is what submits a form. A keyDown with no text is a key that
    /// was pressed and typed nothing, which a form ignores.
    #[test]
    fn one_key_is_a_press_and_a_release() {
        let steps = pressing("Enter").expect("Enter is a key");
        assert_eq!(steps.len(), 2);
        assert_eq!(steps[0].event["text"], "\r");
        assert_eq!(steps[0].event["type"], "keyDown");
        assert_eq!(steps[0].event["windowsVirtualKeyCode"], 13);
        assert_eq!(steps[1].event["type"], "keyUp");

        // By code, by character, and by their alias, all the same key.
        assert_eq!(describe("KeyA", &[]).unwrap().code, "KeyA");
        assert_eq!(describe("a", &[]).unwrap().code, "KeyA");
        assert_eq!(describe("\n", &[]).unwrap().code, "Enter");
    }

    /// A key that is not a key is refused by name, and so is a bad modifier.
    #[test]
    fn what_is_not_a_key_is_refused_and_named() {
        let refused = pressing("Ctrl+A").expect_err("Ctrl is not a modifier");
        assert!(refused.contains("Ctrl"), "it should name what was wrong");
        assert!(refused.contains("Shift"), "and what the modifiers are");

        let refused = pressing("Nonsense").expect_err("not a key");
        assert!(refused.contains("Nonsense"));
        assert!(refused.contains("Control+o"), "it should show a chord");

        assert!(pressing("hello").is_err(), "a word is not a key");
        assert!(pressing("").is_err(), "nothing is not a key");
    }

    /// The keypad's keys answer to their code only.
    ///
    /// `1` has to be the number row's, because that is the one somebody means.
    /// Registering the keypad by character too would make it a coin toss which
    /// key `1` was, decided by the order of a JSON file.
    #[test]
    fn the_number_row_owns_the_digits() {
        assert_eq!(describe("1", &[]).unwrap().code, "Digit1");
        assert_eq!(describe("Numpad1", &[]).unwrap().code, "Numpad1");
        assert_eq!(
            describe("Numpad1", &[]).unwrap().location,
            KEYPAD,
            "a keypad key should say it is on the keypad"
        );
        assert_eq!(describe("1", &[]).unwrap().location, 0);
    }

    /// ControlOrMeta is this computer's own shortcut key.
    #[test]
    fn control_or_meta_is_the_key_this_computer_uses() {
        let steps = pressing("ControlOrMeta+a").expect("a chord");
        let wanted = if cfg!(target_os = "macos") { 4 } else { 2 };
        assert_eq!(
            steps[1].event["modifiers"], wanted,
            "on this platform the shortcut key should be {wanted}"
        );
    }

    /// On a Mac, an editing key carries the operation it stands for.
    ///
    /// Without it the key arrives and edits nothing, which is the silent half
    /// of this: the press succeeds, the page sees the key, and the text does
    /// not move. Their table is what says Command+A means select all.
    #[test]
    fn a_mac_gets_the_editing_command_with_the_key() {
        let steps = pressing("Meta+a").expect("a chord");
        let commands = steps[1].event["commands"]
            .as_array()
            .expect("commands is a list")
            .iter()
            .map(|c| c.as_str().unwrap_or_default().to_string())
            .collect::<Vec<_>>();
        if cfg!(target_os = "macos") {
            assert_eq!(
                commands,
                vec!["selectAll"],
                "Command+A on a Mac is select all, and the colon is cut"
            );
        } else {
            assert!(
                commands.is_empty(),
                "only a Mac needs the editing command named"
            );
        }

        // And a command that inserts is dropped, because the key's own text is
        // what does the inserting. Enter is their clearest case
        // (insertNewline:).
        let steps = pressing("Enter").expect("Enter is a key");
        assert!(
            steps[0].event["commands"]
                .as_array()
                .expect("a list")
                .is_empty(),
            "an insert command should have been filtered out"
        );
    }

    /// The layout loaded, and is theirs.
    #[test]
    fn the_layout_is_playwrights_own() {
        let layout = layout();
        assert!(
            layout.len() > 150,
            "only {} keys: the layout did not load",
            layout.len()
        );
        // Their numbers, spot-checked against the file.
        assert_eq!(layout["Escape"].key_code, 27);
        assert_eq!(layout["KeyA"].key, "a");
        assert_eq!(layout["KeyA"].shifted.as_ref().unwrap().key, "A");
        assert!(!commands().is_empty(), "the editing commands did not load");
    }
}
