//! What this computer can do, when the assistant asks.
//!
//! The gateway holds every tool's SCHEMA: its name, what it takes, and what the
//! model is told about it. This half holds the doing. The split is not
//! tidiness: the model's instructions have to be versioned with the gateway
//! rather than with whichever build of this application somebody happens to
//! have installed, or two people asking the same assistant the same question
//! would be talking to different products.
//!
//! So nothing here describes a tool. It receives a name and arguments, does the
//! work, and answers.

use serde::{Deserialize, Serialize};
use serde_json::Value;

pub mod clock;
pub mod files;
pub mod info;
pub mod terminal;

/// What the gateway sends: which tool, and the arguments the model gave.
#[derive(Debug, Deserialize)]
pub struct Request {
    pub tool: String,
    #[serde(default)]
    pub args: Value,
    /// What this CALL is, as opposed to what it does.
    ///
    /// The gateway mints it and repeats it if the socket carrying the call
    /// dies. That is what makes a dropped link a pause rather than a loss: the
    /// work is already going, and a second arrival of the same id waits for the
    /// answer instead of running the command again.
    #[serde(default)]
    pub call: String,
    /// This is the gateway COMING BACK for a call it already sent, not asking
    /// for something new.
    ///
    /// It matters when this application has restarted in between. The work died
    /// with the process and the memory of it went too, so an id that is not
    /// known here means the call was interrupted. Running it now would be
    /// running it a SECOND time, which for a command is not a retry: it is a
    /// deploy that happens twice, a directory removed after it was recreated.
    /// So a resume that finds nothing says what happened instead of doing
    /// something.
    #[serde(default)]
    pub resume: bool,
}

/// What this half answers.
///
/// `kind` carries a failure's KIND and not only its words, because the loop
/// treats them differently: a tool called wrongly is something the assistant
/// can learn from and correct, while a refusal is not.
#[derive(Debug, Serialize, Clone)]
pub struct Response {
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub content: Option<Value>,
    #[serde(skip_serializing_if = "str::is_empty")]
    pub kind: &'static str,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub message: String,
}

impl Response {
    pub fn ok(content: Value) -> Self {
        Self { ok: true, content: Some(content), kind: "", message: String::new() }
    }

    /// The assistant called this wrongly: wrong arguments, a missing one, a
    /// path that is not a path. Worth telling it, because it can correct.
    pub fn bad_arguments(message: impl Into<String>) -> Self {
        Self { ok: false, content: None, kind: "bad_arguments", message: message.into() }
    }

    /// This computer would not: outside what it may touch, or turned off here.
    pub fn refused(message: impl Into<String>) -> Self {
        Self { ok: false, content: None, kind: "denied", message: message.into() }
    }

    /// It tried and could not. The file was locked, the disk was full.
    pub fn failed(message: impl Into<String>) -> Self {
        Self { ok: false, content: None, kind: "failed", message: message.into() }
    }
}

/// What this build can do, and which version of each tool's arguments it
/// speaks. Sent when the link opens; the gateway offers only what is in it.
///
/// A tool's arguments never change shape under the same version. A different
/// shape is a different version, and an application that speaks only the old
/// one is simply not offered the tool, rather than being offered it and failing
/// when somebody tries to use it.
pub fn runs() -> serde_json::Map<String, Value> {
    let mut runs = serde_json::Map::new();
    runs.insert(info::NAME.to_string(), Value::from(info::VERSION));
    runs.insert(clock::NAME.to_string(), Value::from(clock::VERSION));
    runs.insert(terminal::NAME.to_string(), Value::from(terminal::VERSION));
    runs.insert(files::read::NAME.to_string(), Value::from(files::read::VERSION));
    runs.insert(files::write::NAME.to_string(), Value::from(files::write::VERSION));
    runs.insert(files::edit::NAME.to_string(), Value::from(files::edit::VERSION));
    runs.insert(files::find::NAME.to_string(), Value::from(files::find::VERSION));
    runs.insert(files::search::NAME.to_string(), Value::from(files::search::VERSION));
    runs
}

/// Calls that are running or have just finished, by the id the gateway gave
/// them.
///
/// It is what makes the link recoverable. A websocket dies for reasons that
/// have nothing to do with the work: a network moves, a laptop sleeps, a
/// credential is renewed. The command it was carrying is still running here,
/// and losing its answer because a socket went is losing work nobody can get
/// back: an install that ran for four minutes, gone, and the assistant told the
/// computer was not connected.
///
/// So a call is remembered by its id. The same id arriving again does not run
/// anything: it waits for the one already going, or answers from what it
/// already produced. That is the difference between at-most-once and
/// exactly-once from the person's side, and it is the whole reason the id
/// travels.
mod calls {
    use std::collections::HashMap;
    use std::sync::Arc;
    use std::time::{Duration, Instant};

    use tokio::sync::{Mutex, OnceCell};

    use super::Response;

    /// How long a finished call is kept for somebody to come back for.
    ///
    /// An hour, which is not about how long a reconnection takes: it is about
    /// what is being thrown away. An answer nobody collected is the whole
    /// result of work that has already been done, and keeping a few hundred of
    /// them costs nothing next to running an install again.
    const KEEP: Duration = Duration::from_secs(3600);

    /// How many are kept, so a long day cannot grow this without end. What is
    /// dropped first is always something already collected or long finished; a
    /// call still RUNNING is never dropped, because its answer is what somebody
    /// is coming back for.
    const MOST: usize = 512;

    /// One call: the work, and what it produced.
    ///
    /// The slot is shared, so a second arrival waits on the SAME work rather
    /// than starting its own. Whoever gets there first does it; everybody waits
    /// on the one answer.
    #[derive(Clone)]
    pub struct Call {
        pub answer: Arc<OnceCell<Response>>,
        at: Instant,
    }

    static CALLS: Mutex<Option<HashMap<String, Call>>> = Mutex::const_new(None);

    /// slot is this call's place to put its answer, and whether somebody else
    /// is already filling it.
    pub async fn slot(id: &str) -> (Call, bool) {
        let mut held = CALLS.lock().await;
        let all = held.get_or_insert_with(HashMap::new);
        forget_the_old(all);
        if let Some(known) = all.get(id) {
            return (known.clone(), true);
        }
        let fresh = Call { answer: Arc::new(OnceCell::new()), at: Instant::now() };
        all.insert(id.to_string(), fresh.clone());
        (fresh, false)
    }

    /// forget_the_old drops what nobody can be waiting for any more.
    fn forget_the_old(all: &mut HashMap<String, Call>) {
        all.retain(|_, call| call.at.elapsed() < KEEP || call.answer.get().is_none());
        if all.len() <= MOST {
            return;
        }
        // Oldest first, and never one that has not finished: something is still
        // working on that, and its answer is what somebody will come back for.
        let mut done: Vec<(String, Instant)> = all
            .iter()
            .filter(|(_, call)| call.answer.get().is_some())
            .map(|(id, call)| (id.clone(), call.at))
            .collect();
        done.sort_by_key(|(_, at)| *at);
        for (id, _) in done.into_iter().take(all.len() - MOST) {
            all.remove(&id);
        }
    }

    /// forget takes a call out, for one that turned out not to be ours.
    pub async fn forget(id: &str) {
        let mut held = CALLS.lock().await;
        if let Some(all) = held.as_mut() {
            all.remove(id);
        }
    }

    #[cfg(test)]
    pub async fn forget_everything() {
        let mut held = CALLS.lock().await;
        if let Some(all) = held.as_mut() {
            all.clear();
        }
    }
}

/// run does what was asked, or says why it did not.
///
/// A call with an id is done ONCE. The same id arriving again (the gateway
/// asking after a socket died) waits for the answer the first one is producing,
/// or repeats the answer it produced. Without an id it is an ordinary call and
/// runs as it is asked.
pub async fn run(request: Request) -> Response {
    if request.call.is_empty() {
        return dispatch(request).await;
    }
    let (call, already) = calls::slot(&request.call).await;
    if request.resume && !already {
        // The gateway came back for a call this application has never heard
        // of, which means it is not the application that took it: it has been
        // restarted since. Whatever was running died with the process.
        calls::forget(&request.call).await;
        return Response::failed(
            "the chat application was restarted while this was running, so it was interrupted \
             and did not finish. Nothing was run again: start it over if it should still happen.",
        );
    }
    // Whether this is the first arrival or the second, the shape is the same:
    // whoever gets there first does the work, and everybody waits on the one
    // answer. THIS is what makes a dropped link a pause rather than a loss: the
    // install kept installing, and the call that comes back for it waits.
    let _ = already;
    call.answer
        .get_or_init(|| async { dispatch(request).await })
        .await
        .clone()
}

/// dispatch is the doing, without the remembering.
async fn dispatch(request: Request) -> Response {
    match request.tool.as_str() {
        info::NAME => info::run(request.args).await,
        clock::NAME => clock::run(request.args).await,
        terminal::NAME => terminal::run(request.args).await,
        files::read::NAME => files::read::run(request.args).await,
        files::write::NAME => files::write::run(request.args).await,
        files::edit::NAME => files::edit::run(request.args).await,
        files::find::NAME => files::find::run(request.args).await,
        files::search::NAME => files::search::run(request.args).await,
        // A tool this build has never heard of. It should not be reachable (the
        // gateway offers only what was declared), so it is worth saying plainly
        // rather than failing vaguely.
        other => Response::failed(format!(
            "this version of the chat application does not know the ability \"{other}\""
        )),
    }
}

/// The one workspace every test in this module tree shares.
///
/// There is ONE chosen folder on a computer, and choosing it is global, which
/// is right for the product and a trap for tests: two test modules each
/// choosing their own left whichever ran last holding it, and the other one
/// looking for its files somewhere they had never been. They share this
/// instead, and take a folder of their own underneath it.
#[cfg(test)]
pub(crate) fn test_workspace() -> &'static std::path::PathBuf {
    use std::path::PathBuf;
    use std::sync::OnceLock;
    static ROOT: OnceLock<PathBuf> = OnceLock::new();
    ROOT.get_or_init(|| {
        let root = std::env::temp_dir().join(format!("sag-tools-{}", std::process::id()));
        std::fs::create_dir_all(root.join(".state")).expect("make the test workspace");
        crate::workspace::use_state_dir(root.join(".state"));
        crate::workspace::choose(root.clone()).expect("choose the test workspace");
        root
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The same call, asked for twice, is done ONCE.
    ///
    /// This is what makes a dropped link a pause rather than a loss. The
    /// gateway repeats a call's id when the socket carrying it dies, and the
    /// wrong answer to that is to run the command again: an install that half
    /// happened, twice, is worse than one that was lost. So the second arrival
    /// waits on the work already going and gets the same answer.
    #[tokio::test]
    async fn the_same_call_is_done_once() {
        calls::forget_everything().await;
        let marks = std::env::temp_dir().join(format!("sag-once-{}.txt", std::process::id()));
        let _ = std::fs::remove_file(&marks);
        crate::tools::test_workspace();

        let ask = |id: &str| {
            let command = format!(
                "sleep 1; echo ran >> {}; echo done",
                marks.to_string_lossy()
            );
            let id = id.to_string();
            async move {
                run(Request {
                    tool: terminal::NAME.to_string(),
                    args: serde_json::json!({ "command": command, "wait": 30, "conversation": 4242 }),
                    call: id,
                    resume: false,
                })
                .await
            }
        };

        // Both at once, with the same id, which is what a resumed call looks
        // like from here: the second arrives while the first is still working.
        let (first, second) = tokio::join!(ask("call-1"), ask("call-1"));
        assert!(first.ok, "{}", first.message);
        assert!(second.ok, "{}", second.message);

        let ran = std::fs::read_to_string(&marks).unwrap_or_default();
        assert_eq!(
            ran.lines().count(),
            1,
            "the command ran {} times; a repeated call id must not repeat the work",
            ran.lines().count()
        );
        // And both callers got the same answer, not one answer and one refusal
        // about the terminal being busy.
        assert_eq!(
            first.content.as_ref().map(|c| c["output"].clone()),
            second.content.as_ref().map(|c| c["output"].clone()),
            "the two arrivals were given different answers"
        );
        let _ = std::fs::remove_file(&marks);
    }

    /// A call with NO id is an ordinary call: nothing is remembered, and two of
    /// them are two calls. The id is the gateway's, and a tool called without
    /// one (a test, a future caller) must not be quietly de-duplicated.
    #[tokio::test]
    async fn a_call_with_no_id_is_not_remembered() {
        calls::forget_everything().await;
        let answer = run(Request {
            tool: info::NAME.to_string(),
            args: serde_json::json!({}),
            call: String::new(),
            resume: false,
        })
        .await;
        assert!(answer.ok, "{}", answer.message);
    }

    /// Coming back for a call this application has never heard of does NOT run
    /// it.
    ///
    /// That is the application having restarted in between: the work died with
    /// the process and the memory of it went too. Running the command now
    /// would be running it a second time, which for a terminal is a deploy
    /// that happens twice or a directory removed after somebody recreated it.
    /// The first version of the resume did exactly that, and an end-to-end
    /// test that quit the application caught it.
    #[tokio::test]
    async fn a_resume_that_finds_nothing_does_not_run_it() {
        calls::forget_everything().await;
        crate::tools::test_workspace();
        let marks = std::env::temp_dir().join(format!("sag-resume-{}.txt", std::process::id()));
        let _ = std::fs::remove_file(&marks);

        let answer = run(Request {
            tool: terminal::NAME.to_string(),
            args: serde_json::json!({
                "command": format!("echo ran >> {}", marks.to_string_lossy()),
                "wait": 30,
                "conversation": 4243,
            }),
            call: "a-call-this-process-never-saw".to_string(),
            resume: true,
        })
        .await;

        assert!(!answer.ok, "a resume that found nothing ran the command");
        assert!(
            answer.message.contains("interrupted"),
            "it must say what happened: {}",
            answer.message
        );
        assert!(
            !marks.exists(),
            "the command was run by a resume that should only have collected an answer"
        );
    }
}

#[cfg(test)]
mod contract {
    //! What this half says it speaks, held against what the gateway speaks.
    //!
    //! The two ship separately and are versioned per tool on purpose, so an
    //! application a release behind is not offered a tool it has never heard of
    //! (KB/39). What that mechanism cannot see is the two halves of ONE release
    //! disagreeing: the terminal grew `session` and `status` here, the gateway
    //! moved to version three for them, and this number stayed at two. Every
    //! test passed, the link opened, and the tool was offered to nobody. There
    //! is no error for that, by design.
    //!
    //! So the numbers live in a file both sides read, and both sides assert it.

    #[test]
    fn this_half_speaks_what_the_contract_says() {
        let agreed: serde_json::Map<String, serde_json::Value> =
            serde_json::from_str(include_str!("../../../link-tools.json"))
                .expect("link-tools.json is not valid JSON");
        let speaks = super::runs();

        for (tool, version) in &agreed {
            assert_eq!(
                speaks.get(tool),
                Some(version),
                "the gateway speaks {tool} at {version} and this half does not: the tool is offered to nobody"
            );
        }
        for tool in speaks.keys() {
            assert!(
                agreed.contains_key(tool),
                "this half offers {tool}, which the contract has never heard of"
            );
        }
    }
}
