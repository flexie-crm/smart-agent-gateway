//! What can go wrong, said once.
//!
//! Every fallible path in this crate returns [`Error`], and [`Error`] knows how
//! to become an HTTP response. A handler therefore never decides a status code:
//! it returns the failure it actually had, and the mapping below is the only
//! place that says what that means on the wire.
//!
//! The messages are read by an administrator through the console, so they carry
//! product vocabulary and never name the engine, the toolchain or a crate.

use axum::{
    Json,
    http::StatusCode,
    response::{IntoResponse, Response},
};
use serde::Serialize;

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// The caller asked for something this node does not have.
    #[error("{0} not found")]
    NotFound(String),

    /// The request was understood and is wrong.
    #[error("{0}")]
    Invalid(String),

    /// No key, or the wrong one.
    #[error("not authorised")]
    Unauthorised,

    /// The request is fine but the node's state refuses it: pulling a model it
    /// already has, unloading one that is already released.
    #[error("{0}")]
    Conflict(String),

    /// The library did not answer: a name that would not resolve, a connection
    /// nothing accepted, a certificate that would not verify.
    #[error("the model library is unreachable: {0}")]
    Hub(String),

    /// The library answered, and the answer is not one this node can use.
    ///
    /// Apart from [`Self::Hub`] because "unreachable" is a false sentence about
    /// a library that replied, and the two send somebody to look in completely
    /// different places: one is this machine's network, the other is the
    /// library's answer.
    #[error("the model library gave an answer this node cannot use: {0}")]
    HubAnswer(String),

    /// The engine refused. Its own words are worth keeping: they are the only
    /// account of why a model would not load.
    #[error("the engine refused: {0}")]
    Engine(String),

    /// The disk.
    #[error("storage: {0}")]
    Io(#[from] std::io::Error),

    /// A catalogue entry that will not parse, which means a file we wrote.
    #[error("catalogue: {0}")]
    Catalogue(#[from] serde_json::Error),
}

impl Error {
    pub fn invalid(msg: impl Into<String>) -> Self {
        Self::Invalid(msg.into())
    }

    pub fn not_found(what: impl Into<String>) -> Self {
        Self::NotFound(what.into())
    }

    pub fn conflict(msg: impl Into<String>) -> Self {
        Self::Conflict(msg.into())
    }

    /// The engine's own account of a failure, which is the only one there is:
    /// nothing above it knows why a set of weights would not fit in memory.
    ///
    /// It is passed through REDACTED. The text comes from a library and can name
    /// it, and an error message is a user-facing string (CLAUDE.md), so the one
    /// place that text crosses into ours is the one place that can be sure. The
    /// unredacted line is logged first, because the operator reading the node's
    /// log is the person who needs the whole of it.
    pub fn engine(msg: impl std::fmt::Display) -> Self {
        let raw = msg.to_string();
        tracing::error!(detail = %raw, "engine failure");
        Self::Engine(redact(&raw))
    }

    /// A failure reaching the library, with the whole of why.
    ///
    /// Takes an error and not a message, deliberately: the reason is never in
    /// the error's own `Display`, it is in what that error wraps, and a
    /// constructor that accepted a string would let a caller pass the top line
    /// alone without noticing they had thrown the answer away. That is exactly
    /// what used to happen here. See [`chain`].
    pub fn hub(err: impl std::error::Error) -> Self {
        Self::Hub(redact(&chain(&err)))
    }

    /// The library replied and the reply is unusable. A sentence of ours,
    /// because there is no underlying error to unwrap: nothing failed, the
    /// answer was simply not what it has to be.
    pub fn hub_answer(msg: impl Into<String>) -> Self {
        Self::HubAnswer(msg.into())
    }

    /// The status this failure is on the wire.
    pub fn status(&self) -> StatusCode {
        match self {
            Self::NotFound(_) => StatusCode::NOT_FOUND,
            Self::Invalid(_) => StatusCode::BAD_REQUEST,
            Self::Unauthorised => StatusCode::UNAUTHORIZED,
            Self::Conflict(_) => StatusCode::CONFLICT,
            Self::Hub(_) | Self::HubAnswer(_) => StatusCode::BAD_GATEWAY,
            Self::Engine(_) | Self::Io(_) | Self::Catalogue(_) => StatusCode::INTERNAL_SERVER_ERROR,
        }
    }

    /// A stable machine-readable name for the failure, so the caller can branch
    /// on the kind without reading prose that may be reworded.
    pub fn code(&self) -> &'static str {
        match self {
            Self::NotFound(_) => "not_found",
            Self::Invalid(_) => "invalid",
            Self::Unauthorised => "unauthorised",
            Self::Conflict(_) => "conflict",
            Self::Hub(_) => "hub_unreachable",
            Self::HubAnswer(_) => "hub_bad_answer",
            Self::Engine(_) => "engine_refused",
            Self::Io(_) => "storage",
            Self::Catalogue(_) => "catalogue",
        }
    }
}

/// How many levels of a failure are worth reading, and a stop for a chain that
/// refers back to itself. Nothing real here is deeper than four.
const CHAIN_DEPTH: usize = 8;

/// Every level of a failure, outermost first, as one sentence.
///
/// An HTTP client's own `Display` is the request it was making, not the reason
/// it failed: "error sending request for url (...)" are the same words whether
/// the name did not resolve, the connection was refused, or the certificate was
/// rejected. Which of those it was lives one to three levels down `source()`:
///
/// ```text
/// [0] error sending request for url (https://.../api/models)
/// [1] client error (Connect)
/// [2] tcp connect error          <- the stage
/// [3] deadline has elapsed       <- and what it did
/// ```
///
/// Keeping only level 0 made all four failures one indistinguishable sentence,
/// which is how a day went on arguing whether a node's problem was DNS, a proxy
/// or a certificate, from a log that could not settle it. A diagnostic that
/// cannot name the stage that stalled is not a diagnostic.
fn chain(err: &dyn std::error::Error) -> String {
    let mut parts: Vec<String> = Vec::new();
    let mut cur = Some(err);
    // Counted in LEVELS WALKED and not in levels kept: a chain whose every
    // level reads the same is deduplicated down to one part, so a bound on what
    // was kept would never be reached and this would spin forever.
    for _ in 0..CHAIN_DEPTH {
        let Some(err) = cur else { break };
        let text = err.to_string();
        // Levels that merely restate the one above read as emphasis and carry
        // nothing: some wrappers Display exactly what they wrap.
        if !parts.iter().any(|seen| seen == &text) {
            parts.push(text);
        }
        cur = err.source();
    }
    parts.join(": ")
}

/// Names of the things we are built on, which must not appear in anything a
/// person reads (CLAUDE.md). Matched case-insensitively on whole words so that
/// a model called `mistral-7b`, which is a legitimate thing an administrator
/// downloaded, is not mangled into nonsense: only the bare product name goes.
const STACK_NAMES: &[&str] = &[
    "mistralrs",
    "mistral.rs",
    "candle",
    "tokio",
    "axum",
    "cargo",
    "rustc",
];

/// What a redacted name is replaced by.
const REDACTED: &str = "the engine";

/// Replace any name of the stack with the product word for the thing it is.
///
/// The search runs against `to_ascii_lowercase`, deliberately, and not
/// `to_lowercase`: every needle is ASCII, and only the ASCII form is guaranteed
/// to preserve byte length. A Unicode lowercase can change it (`İ` is two bytes
/// and lowercases to three), and then an index found in the lowered copy is the
/// wrong index in the original, which is a panic on a character boundary in the
/// one code path whose whole job is to report a failure calmly.
fn redact(raw: &str) -> String {
    let mut out = raw.to_string();
    for name in STACK_NAMES {
        loop {
            let hay = out.to_ascii_lowercase();
            let Some(at) = hay.find(name) else { break };
            out.replace_range(at..at + name.len(), REDACTED);
        }
    }
    out
}

#[derive(Serialize)]
struct Envelope<'a> {
    error: Detail<'a>,
}

#[derive(Serialize)]
struct Detail<'a> {
    code: &'a str,
    message: String,
}

impl IntoResponse for Error {
    fn into_response(self) -> Response {
        let status = self.status();
        // A 5xx is our fault and is logged here, once, where every one of them
        // passes. A 4xx is the caller's and is already in their hands.
        if status.is_server_error() {
            tracing::error!(error = %self, code = self.code(), "request failed");
        }
        let body = Envelope {
            error: Detail {
                code: self.code(),
                message: self.to_string(),
            },
        };
        (status, Json(body)).into_response()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// One level of a wrapped failure, so a chain can be built in a test the
    /// way a client builds one at runtime.
    #[derive(Debug)]
    struct Layer(&'static str, Option<Box<Layer>>);

    impl std::fmt::Display for Layer {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            f.write_str(self.0)
        }
    }

    impl std::error::Error for Layer {
        fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
            self.1.as_deref().map(|next| next as &dyn std::error::Error)
        }
    }

    /// The four levels reqwest 0.13 actually produces, measured against a real
    /// server: a connection that was never accepted.
    fn tcp_timeout() -> Layer {
        Layer(
            "error sending request for url (https://huggingface.co/api/models)",
            Some(Box::new(Layer(
                "client error (Connect)",
                Some(Box::new(Layer(
                    "tcp connect error",
                    Some(Box::new(Layer("deadline has elapsed", None))),
                ))),
            ))),
        )
    }

    #[test]
    fn a_failure_carries_every_level_of_why_it_failed() {
        // The defect this replaces: only level 0 survived, and level 0 is the
        // same sentence for DNS, for a refused connection and for a rejected
        // certificate. Whoever reads the log has to be able to tell them apart.
        let err = Error::hub(tcp_timeout());
        let text = err.to_string();

        assert!(
            text.contains("tcp connect error") && text.contains("deadline has elapsed"),
            "the stage that stalled is missing: {text}"
        );
        assert_eq!(
            text,
            "the model library is unreachable: error sending request for url \
             (https://huggingface.co/api/models): client error (Connect): tcp connect error: \
             deadline has elapsed"
        );
    }

    #[test]
    fn the_three_stages_do_not_read_alike() {
        // The property that matters, stated as itself: three failures whose top
        // line is identical must produce three different messages.
        let dns = Error::hub(Layer(
            "error sending request for url (https://huggingface.co/api/models)",
            Some(Box::new(Layer(
                "client error (Connect)",
                Some(Box::new(Layer(
                    "dns error",
                    Some(Box::new(Layer(
                        "No such host is known. (os error 11001)",
                        None,
                    ))),
                ))),
            ))),
        ));
        let tls = Error::hub(Layer(
            "error sending request for url (https://huggingface.co/api/models)",
            Some(Box::new(Layer(
                "client error (Connect)",
                Some(Box::new(Layer("invalid peer certificate: Expired", None))),
            ))),
        ));
        let tcp = Error::hub(tcp_timeout());

        let messages = [dns.to_string(), tls.to_string(), tcp.to_string()];
        let unique: std::collections::BTreeSet<_> = messages.iter().collect();
        assert_eq!(
            unique.len(),
            3,
            "two different stages produced one message: {messages:#?}"
        );
    }

    #[test]
    fn a_level_that_repeats_the_one_above_is_not_said_twice() {
        // Some wrappers Display exactly what they wrap, and a message that says
        // the same thing three times reads as three faults.
        let err = Error::hub(Layer(
            "connection closed",
            Some(Box::new(Layer(
                "connection closed",
                Some(Box::new(Layer("connection closed", None))),
            ))),
        ));
        assert_eq!(
            err.to_string(),
            "the model library is unreachable: connection closed"
        );
    }

    #[test]
    fn a_chain_that_refers_to_itself_still_ends() {
        // A self-referential source() would spin forever in the one code path
        // whose whole job is to report a failure calmly. The bound is asserted
        // rather than trusted, because nothing else here could catch it.
        struct Loop;

        impl std::fmt::Debug for Loop {
            fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                f.write_str("Loop")
            }
        }
        impl std::fmt::Display for Loop {
            fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                f.write_str("round and round")
            }
        }
        static FOREVER: Loop = Loop;

        impl std::error::Error for Loop {
            fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
                Some(&FOREVER)
            }
        }

        // If this test hangs rather than fails, the walk is unbounded: that is
        // the failure, and it is why the bound counts levels walked rather than
        // levels kept (every level here reads the same, so dedup keeps one).
        assert_eq!(
            Error::hub(Loop).to_string(),
            "the model library is unreachable: round and round"
        );
    }

    #[test]
    fn a_library_that_answered_is_not_called_unreachable() {
        // Two different problems: one is this machine's network, the other is
        // the library's reply. Saying "unreachable" about a library that
        // replied sends somebody to look at a firewall for an hour.
        let answered = Error::hub_answer("meta-llama/Llama-3.2-1B did not say which commit it is");
        assert!(
            !answered.to_string().contains("unreachable"),
            "a library that answered was called unreachable: {answered}"
        );
        assert_eq!(answered.code(), "hub_bad_answer");
        assert_eq!(Error::hub(tcp_timeout()).code(), "hub_unreachable");
    }

    #[test]
    fn statuses_match_the_kind_of_failure() {
        assert_eq!(Error::not_found("model").status(), StatusCode::NOT_FOUND);
        assert_eq!(Error::invalid("bad repo").status(), StatusCode::BAD_REQUEST);
        assert_eq!(Error::Unauthorised.status(), StatusCode::UNAUTHORIZED);
        assert_eq!(
            Error::conflict("already here").status(),
            StatusCode::CONFLICT
        );
        assert_eq!(Error::hub(tcp_timeout()).status(), StatusCode::BAD_GATEWAY);
        assert_eq!(
            Error::hub_answer("no commit").status(),
            StatusCode::BAD_GATEWAY
        );
        assert_eq!(
            Error::engine("out of memory").status(),
            StatusCode::INTERNAL_SERVER_ERROR
        );
    }

    #[test]
    fn codes_are_distinct_so_a_caller_can_branch_on_them() {
        let codes = [
            Error::not_found("m").code(),
            Error::invalid("m").code(),
            Error::Unauthorised.code(),
            Error::conflict("m").code(),
            Error::hub(tcp_timeout()).code(),
            Error::hub_answer("m").code(),
            Error::engine("m").code(),
        ];
        let unique: std::collections::BTreeSet<_> = codes.iter().collect();
        assert_eq!(unique.len(), codes.len(), "two failures share one code");
    }

    #[test]
    fn the_replacement_contains_no_needle_so_redaction_terminates() {
        // redact() loops until a name is gone. If what it substitutes contained
        // one, it would substitute forever. That is an invariant of the pair of
        // constants, so it is asserted rather than reasoned about.
        let lowered = REDACTED.to_ascii_lowercase();
        for name in STACK_NAMES {
            assert!(
                !lowered.contains(name),
                "{REDACTED:?} contains {name:?}: redaction would not terminate"
            );
        }
    }

    #[test]
    fn redaction_replaces_every_occurrence_whatever_the_case() {
        assert_eq!(redact("MistralRs said no"), "the engine said no");
        assert_eq!(
            redact("mistralrs and mistralrs"),
            "the engine and the engine"
        );
        assert_eq!(redact("nothing to hide"), "nothing to hide");
    }

    #[test]
    fn redaction_survives_multibyte_text() {
        // The reason to_ascii_lowercase is used. A Unicode lowercase would move
        // the byte indices under us and split a character.
        let raw = "İstanbul: MISTRALRS out of memory (ß)";
        assert_eq!(redact(raw), "İstanbul: the engine out of memory (ß)");
    }

    #[test]
    fn an_engine_failure_reaches_the_caller_redacted() {
        let err = Error::engine("mistralrs-core: no CUDA device");
        assert_eq!(
            err.to_string(),
            "the engine refused: the engine-core: no CUDA device"
        );
    }

    #[test]
    fn messages_never_name_the_stack() {
        // The console shows these. A person reading one must not learn what we
        // build on (CLAUDE.md), so the forbidden words are asserted rather than
        // left to review.
        let messages = [
            Error::engine("cuda oom").to_string(),
            // The real four-level chain, because this rule is about what a
            // person actually reads and what they read is now the whole chain.
            Error::hub(tcp_timeout()).to_string(),
            Error::hub_answer("no commit").to_string(),
            Error::not_found("model").to_string(),
        ];
        for m in messages {
            let lowered = m.to_lowercase();
            for banned in ["mistral", "rust", "cargo", "tokio", "axum", "candle"] {
                assert!(
                    !lowered.contains(banned),
                    "{m:?} names the stack ({banned})"
                );
            }
        }
    }
}
