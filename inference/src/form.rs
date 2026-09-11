//! Settings we do not have to understand.
//!
//! Every model has knobs, they differ per model and per architecture, and the
//! orchestrator does not know what they are. So it never encodes them: **the
//! node declares the form and the console draws it blind**, storing whatever
//! values come back and handing them to us again when the model is loaded.
//!
//! This is the third instance of one idea in this product (a custom tool's
//! driver declares its connection form, a datasource driver declares its own,
//! KB/31), and the shapes below are deliberately the SAME JSON as those, field
//! for field, so the console renders all three with one component. Changing a
//! name here without changing `internal/tools/template` there breaks the
//! renderer for a form nobody was editing, which is why the wire names are
//! spelled out rather than derived.
//!
//! **What is a fact is not a setting.** A model's architecture, parameter count,
//! context length and licence are read once when it is pulled and shown
//! read-only. They are properties of the weights, and an input invites somebody
//! to change something that cannot be changed.

use serde::{Deserialize, Serialize};

/// How the console renders one input. The same five the tool templates use.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum FieldType {
    Text,
    Number,
    Password,
    Select,
    Textarea,
}

/// One option of a select: the value stored, and what it says to a person.
///
/// A bare string was sent here, and the console renders `{value, label}`, so
/// every option came out blank: a dropdown of empty rows with one of them
/// ticked. The other producer of this same form (the orchestrator's tool
/// settings, internal/tool/form.go) has always sent the pair, so this side was
/// the one breaking a contract rather than the console being strict.
///
/// Teaching the console to accept both shapes would have been the smaller
/// change and the wrong one: one form, one shape, and the reader of a select
/// should not have to know which half of the product declared it.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Choice {
    pub value: String,
    pub label: String,
}

/// One settings input.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Field {
    pub key: String,
    pub label: String,
    #[serde(rename = "type")]
    pub kind: FieldType,
    pub required: bool,
    #[serde(default, skip_serializing_if = "is_false")]
    pub secret: bool,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub options: Vec<Choice>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub default: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub help: String,
    /// Width in a six-column row, so the declaring side decides the layout.
    /// Zero means a sensible default, and a textarea always fills the row.
    #[serde(default, skip_serializing_if = "is_zero")]
    pub span: u8,
}

/// A heading and the inputs under it.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Section {
    pub title: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub hint: String,
    pub fields: Vec<Field>,
}

fn is_false(b: &bool) -> bool {
    !*b
}

fn is_zero(n: &u8) -> bool {
    *n == 0
}

impl Field {
    fn new(key: &str, label: &str, kind: FieldType, span: u8) -> Self {
        Self {
            key: key.into(),
            label: label.into(),
            kind,
            required: false,
            secret: false,
            options: Vec::new(),
            default: String::new(),
            help: String::new(),
            span,
        }
    }

    fn help(mut self, help: &str) -> Self {
        self.help = help.into();
        self
    }

    fn default(mut self, value: &str) -> Self {
        self.default = value.into();
        self
    }

    /// The choices for a select, each as the value stored and what it says.
    fn options(mut self, options: &[(&str, &str)]) -> Self {
        self.options = options
            .iter()
            .map(|(value, label)| Choice {
                value: (*value).to_string(),
                label: (*label).to_string(),
            })
            .collect();
        self
    }
}

/// The keys this node accepts. A value the form never offered is refused rather
/// than stored, because a setting nothing reads is a setting somebody believes
/// is in effect.
pub fn keys() -> Vec<String> {
    settings_form()
        .into_iter()
        .flat_map(|s| s.fields)
        .map(|f| f.key)
        .collect()
}

/// What this node can be told about how to run a model.
///
/// Everything here is a real choice with a real cost, and none of it is
/// expressible from the orchestrator's side without the orchestrator learning
/// what an accelerator is. That is the whole argument for declaring it.
pub fn settings_form() -> Vec<Section> {
    vec![
        Section {
            title: "Loading".into(),
            hint: "How the weights are placed on this machine. Changing any of these takes effect the next time the model is loaded.".into(),
            fields: vec![
                Field::new("quantization", "Quantization", FieldType::Select, 2)
                    .options(&[
                        ("", "As published (no compression)"),
                        ("FP8", "FP8: half the size, near-identical quality"),
                        ("Q8_0", "Q8_0: half the size"),
                        ("Q6K", "Q6K: about 40% of the size"),
                        ("Q5K", "Q5K: about a third of the size"),
                        ("Q4K", "Q4K: about a quarter, the smallest sensible"),
                        ("HQQ8", "HQQ8: half the size, quantised on load"),
                        ("HQQ4", "HQQ4: a quarter, quantised on load"),
                    ])
                    .help("Compresses the weights as they are loaded, trading a little quality for a lot of memory. Blank keeps the weights as they were published."),
                Field::new("device_layers", "Layers on the accelerator", FieldType::Number, 2)
                    .help("How many layers to keep on the accelerator, the rest on the processor. Blank fits as many as will go."),
                Field::new("max_sequences", "Concurrent requests", FieldType::Number, 2)
                    .default("16")
                    .help("How many requests this model answers at once. Higher needs more memory and answers more people."),
            ],
        },
        Section {
            title: "Context and cache".into(),
            hint: "How much conversation the model holds, and what is kept between turns.".into(),
            fields: vec![
                Field::new("context_length", "Context length", FieldType::Number, 2)
                    .help("The longest conversation this model will accept, in tokens. Blank uses what the weights declare."),
                Field::new("cache_memory_mb", "Cache memory", FieldType::Number, 2)
                    .help("Memory reserved for the attention cache, in megabytes. Blank lets the node choose from what is free."),
                Field::new("prefix_cache", "Reuse repeated openings", FieldType::Select, 2)
                    .options(&[("on", "On"), ("off", "Off")])
                    .default("on")
                    .help("Keeps the work done on the start of a conversation so the next turn does not repeat it. Worth having on unless memory is tight."),
            ],
        },
        Section {
            title: "Conversation template".into(),
            hint: "Only needed when a model ships without one, or ships a wrong one.".into(),
            fields: vec![
                Field::new("chat_template", "Template", FieldType::Textarea, 6)
                    .help("Overrides how turns are laid out for this model. Leave blank to use the template published with the weights."),
            ],
        },
    ]
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_field_serialises_with_the_names_the_console_reads() {
        let field = Field::new("quantization", "Quantization", FieldType::Select, 2)
            .options(&[("Q4K", "Q4K")])
            .default("Q4K")
            .help("why");
        let json = serde_json::to_value(&field).expect("field would not serialise");

        // These names are a contract with the console's shared renderer and with
        // `internal/tools/template` on the Go side. Asserting them is what stops
        // a rename here from quietly emptying a form there.
        assert_eq!(json["key"], "quantization");
        assert_eq!(json["label"], "Quantization");
        assert_eq!(json["type"], "select");
        assert_eq!(json["required"], false);
        // An option is a PAIR. This line asserted a bare string, which is what
        // let the wrong shape ship: the test named the console's renderer as
        // the contract it was protecting and then encoded the opposite of what
        // that renderer reads.
        assert_eq!(json["options"][0]["value"], "Q4K");
        assert_eq!(json["options"][0]["label"], "Q4K");
        assert_eq!(json["default"], "Q4K");
        assert_eq!(json["help"], "why");
        assert_eq!(json["span"], 2);
    }

    #[test]
    fn empty_optional_parts_are_left_out_rather_than_sent_blank() {
        let field = Field::new("context_length", "Context length", FieldType::Number, 0);
        let json = serde_json::to_value(&field).expect("field would not serialise");
        let object = json.as_object().expect("not an object");

        for absent in ["secret", "options", "default", "help", "span"] {
            assert!(
                !object.contains_key(absent),
                "{absent} was sent even though it is empty"
            );
        }
        // The four that are always meaningful stay.
        for present in ["key", "label", "type", "required"] {
            assert!(object.contains_key(present), "{present} was dropped");
        }
    }

    #[test]
    fn every_field_type_has_the_wire_name_the_renderer_expects() {
        let pairs = [
            (FieldType::Text, "text"),
            (FieldType::Number, "number"),
            (FieldType::Password, "password"),
            (FieldType::Select, "select"),
            (FieldType::Textarea, "textarea"),
        ];
        for (kind, name) in pairs {
            assert_eq!(serde_json::to_value(kind).unwrap(), name);
        }
    }

    #[test]
    fn the_form_has_no_duplicate_keys() {
        // Two fields with one key means one of them silently does nothing, and
        // which one depends on iteration order.
        let keys = keys();
        let unique: std::collections::BTreeSet<_> = keys.iter().collect();
        assert_eq!(unique.len(), keys.len(), "the settings form repeats a key");
    }

    #[test]
    fn every_section_has_a_title_and_at_least_one_field() {
        for section in settings_form() {
            assert!(!section.title.is_empty(), "a section has no title");
            assert!(
                !section.fields.is_empty(),
                "section {:?} would draw an empty heading",
                section.title
            );
        }
    }

    #[test]
    fn a_select_offers_its_own_default() {
        // A default the options do not contain draws a form whose first render
        // is already invalid.
        for section in settings_form() {
            for field in section.fields {
                if field.kind == FieldType::Select && !field.default.is_empty() {
                    assert!(
                        field.options.iter().any(|o| o.value == field.default),
                        "{} defaults to {:?}, which it does not offer",
                        field.key,
                        field.default
                    );
                }
            }
        }
    }

    #[test]
    fn every_option_says_what_it_is() {
        // The console renders `{value, label}` and so does the orchestrator's
        // half of this same form (internal/tool/form.go). Sending bare strings
        // drew a dropdown of empty rows with one of them ticked, and nothing
        // failed: the shape was wrong on the wire and no test looked at it.
        for section in settings_form() {
            for field in section.fields {
                for option in &field.options {
                    assert!(
                        !option.label.is_empty(),
                        "{}: an option with value {:?} has no label, so it draws blank",
                        field.key,
                        option.value
                    );
                }
            }
        }
    }

    #[test]
    fn an_option_is_serialised_as_a_value_and_a_label() {
        // The wire shape, asserted on the JSON rather than on the struct: the
        // console reads o.value and o.label, and a rename here is a blank
        // dropdown there.
        let field = Field::new("q", "Q", FieldType::Select, 2).options(&[("on", "On")]);
        let json = serde_json::to_value(&field).unwrap();
        assert_eq!(json["options"][0]["value"], "on");
        assert_eq!(json["options"][0]["label"], "On");
    }

    #[test]
    fn no_help_text_names_the_stack() {
        // An administrator reads all of this (CLAUDE.md).
        for section in settings_form() {
            let mut prose = vec![section.title.clone(), section.hint.clone()];
            for field in &section.fields {
                prose.push(field.label.clone());
                prose.push(field.help.clone());
            }
            for line in prose {
                let lowered = line.to_lowercase();
                for banned in ["mistral", "rust", "cuda", "metal", "gpu", "vram", "candle"] {
                    assert!(
                        !lowered.contains(banned),
                        "{line:?} names the stack ({banned})"
                    );
                }
            }
        }
    }
}
