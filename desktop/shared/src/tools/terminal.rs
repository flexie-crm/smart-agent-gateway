//! A shell on this computer, with a memory.
//!
//! It was one command at a time in a fresh shell, which is a shell in the way a
//! photograph is a conversation: `cd` did not carry, an export was gone by the
//! next call, nothing could answer a prompt, and anything slower than ninety
//! seconds was killed for taking as long as it takes. An install came within
//! three seconds of that ceiling and the next one was cut in half.
//!
//! Now: what a command changes about its shell is kept, and what a command is
//! doing can be watched, answered and stopped. `cd` carries between calls, and
//! so does anything exported. A command still going comes back with what it has
//! printed SO FAR, and asking again gets the rest. One that stops to ask a
//! question can be answered. That is the same contract the server tool already
//! has, on purpose: an assistant that has learnt one has learnt both.
//!
//! WHY THE STATE PERSISTS AND NOT THE PROCESS. The obvious build is one shell
//! held open, with commands typed into it and a marker line after each to say
//! it finished. It does not work, and the way it fails is instructive: the
//! command stream and the program's input are the same stdin, so the first
//! command that READS anything swallows the marker meant for the shell. A test
//! with `read who` in it ate the line that reports the exit code.
//!
//! So each command is its own child, with its own stdin that only it can
//! consume, and the SHELL STATE travels between them: the folder and the
//! exported variables are dumped when a command ends and restored before the
//! next one starts. What does not survive is what only a live shell could hold
//! (a function, an alias, an unexported variable), which is the honest cost of
//! not needing a terminal emulator to run a command.
//!
//! Its life has nothing to do with any socket's. A link that drops and comes
//! back finds the same folder and the same variables.
//!
//! What may run was decided before the call arrived: the gateway parsed the
//! command against the rules an administrator wrote. This half owns WHERE, and
//! now also WHEN: it is the only side that knows a command is still going.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;

use serde::Deserialize;
use serde_json::{json, Value};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::Mutex;

use super::Response;
use crate::workspace;

pub const NAME: &str = "terminal";

/// The version of this tool's arguments.
///
/// THREE: TWO added the memory (`input`, `wait`, `stop`) that a one-shot never
/// had, and THREE added more than one terminal per conversation (`session`,
/// `status`). An application still speaking an older shape is simply not
/// offered the tool, rather than being offered it and failing on arguments it
/// has never heard of.
///
/// It sat at TWO for a while after this half had grown `session` and `status`,
/// which is the failure this number exists to prevent, pointed the wrong way:
/// the gateway spoke THREE, no application answered THREE, and the terminal was
/// quietly offered to nobody. Nothing errored, because nothing was wrong except
/// a number. That is why the contract is now asserted from both sides
/// (desktop/link-tools.json).
pub const VERSION: i64 = 3;

/// How long a call waits before coming back with what there is so far. A
/// ceiling, not a delay: something that finishes sooner answers sooner.
const DEFAULT_WAIT: u64 = 30;
const MAX_WAIT: u64 = 600;

/// How much is held while nobody is asking.
///
/// Not the same as what one answer carries. A command left running prints into
/// this buffer whether or not anybody is reading, so `yes` or a verbose build
/// would grow it until the person's machine noticed. What is kept is the END,
/// for the reason an answer keeps the end: a command says what happened last.
const MAX_HELD: usize = 2 * 1024 * 1024;

/// How much of what it printed comes back in one answer. What arrives goes into
/// a model's context, so a build that prints a hundred megabytes is trimmed
/// rather than allowed to end the turn on tokens.
const MAX_OUTPUT: usize = 96 * 1024;

/// How often the running command is looked at. A fifth of a second between it
/// finishing and the answer going out is not something anybody notices.
const LOOK: std::time::Duration = std::time::Duration::from_millis(200);

#[derive(Debug, Deserialize)]
pub struct Args {
    /// What to run. Omitted when reading more of something already running, or
    /// answering it, or stopping it.
    #[serde(default)]
    pub command: String,
    /// A folder to run in, for this command. The shell stays wherever it ends
    /// up afterwards, exactly as a terminal does.
    #[serde(default)]
    pub directory: String,
    /// What to type into the command that is still running.
    #[serde(default)]
    pub input: String,
    /// How many seconds to wait for it to finish.
    #[serde(default)]
    pub wait: u64,
    /// End what is running in this terminal.
    #[serde(default)]
    pub stop: bool,
    /// Report every terminal in this conversation and what it is doing, rather
    /// than running anything. It is the answer to "is that install still
    /// going, and what about the tests?" when several are.
    #[serde(default)]
    pub status: bool,
    /// Which conversation is asking. Added by the gateway, never by a model: it
    /// is what makes the shell theirs rather than everybody's.
    #[serde(default)]
    pub conversation: i64,
    /// WHICH terminal in that conversation.
    ///
    /// One terminal is one thing at a time, which is right: a shell is. But a
    /// person working on something has several things going at once (a build,
    /// a test run, a search), and making them queue behind each other is a
    /// limit of the tool rather than of the work. So a conversation may have
    /// several terminals, each named by whoever is using it.
    ///
    /// Empty is the terminal called "main", which is what a caller that has
    /// never thought about this gets, and it behaves exactly as before.
    #[serde(default)]
    pub session: String,
}

pub async fn run(args: Value) -> Response {
    let args: Args = match serde_json::from_value(args) {
        Ok(args) => args,
        Err(err) => return Response::bad_arguments(format!("the arguments could not be read: {err}")),
    };
    let which = named(args.conversation, &args.session);

    // What is going on across this conversation's terminals, which is a
    // question worth being able to ask when several are working at once.
    if args.status {
        return status(args.conversation).await;
    }
    if args.stop {
        return stop(&which).await;
    }

    let Some(root) = workspace::folder() else {
        return Response::refused(
            "no folder has been chosen on this computer yet. \
             The person can choose one in the chat application, and everything runs inside it.",
        );
    };

    {
        let mut sessions = open().await;
        let session = sessions
            .entry(which.clone())
            .or_insert_with(|| Session::new(&root));

        if !args.input.is_empty() {
            if session.running.is_none() {
                return Response::bad_arguments(
                    "nothing is running to type into. Run a command first, and answer it if it asks.",
                );
            }
            if let Err(reason) = session.type_in(&args.input).await {
                return Response::failed(reason);
            }
        } else if !args.command.trim().is_empty() {
            if session.running.is_some() {
                // NOT a bare refusal. The model asked to run something and
                // needs to decide what to do instead, and "something else is
                // running" is not enough to decide with: it needs to know what,
                // for how long, and what it has said. So this answers with the
                // state of the terminal and says plainly that the new command
                // was not started.
                //
                // It was a refusal with nothing in it, which cost a whole turn
                // to learn something the tool already knew.
                return session.busy(&args.command).await;
            }
            let directory = match resolve(&session.directory, &root, &args.directory) {
                Ok(directory) => directory,
                Err(reason) => return Response::bad_arguments(reason),
            };
            if let Err(reason) = session.send(&args.command, &directory).await {
                return Response::failed(reason);
            }
        } else if session.running.is_none() {
            // Nothing to run, nothing to type, nothing going: this is a call
            // with nothing in it rather than somebody asking for more.
            return Response::bad_arguments("give a command to run, or input for what is running");
        }
    }

    look(&which, wait_for(args.wait)).await
}

/// named is which terminal a call is about: the one it asked for, or "main".
fn named(conversation: i64, session: &str) -> (i64, String) {
    let name = session.trim();
    (conversation, if name.is_empty() { "main".to_string() } else { name.to_string() })
}

/// wait_for settles how long this call may wait.
fn wait_for(asked: u64) -> std::time::Duration {
    let seconds = if asked == 0 { DEFAULT_WAIT } else { asked.min(MAX_WAIT) };
    std::time::Duration::from_secs(seconds)
}

/// look waits for the running command to finish, and answers with what it
/// printed either way.
async fn look(which: &(i64, String), deadline: std::time::Duration) -> Response {
    let until = std::time::Instant::now() + deadline;
    loop {
        {
            let mut sessions = open().await;
            let Some(session) = sessions.get_mut(which) else {
                return Response::bad_arguments(format!(
                    "there is no terminal called {:?} in this conversation. Run a command to start one.",
                    which.1
                ));
            };
            session.harvest().await;
            if session.running.is_none() || std::time::Instant::now() >= until {
                return session.answer().await;
            }
        }
        tokio::time::sleep(LOOK).await;
    }
}

/// status is what every terminal in this conversation is doing.
///
/// A person with three things going wants one answer about all of them, and so
/// does a model deciding what to wait for. Reading them one at a time means
/// three calls and a picture assembled by hand.
async fn status(conversation: i64) -> Response {
    let mut sessions = open().await;
    let mut terminals = Vec::new();
    for ((of, name), session) in sessions.iter_mut() {
        if *of != conversation {
            continue;
        }
        session.harvest().await;
        terminals.push(json!({
            "session": name,
            "running": session.running.is_some(),
            "running_command": session.running.clone(),
            "running_for_seconds": session.started.map(|at| at.elapsed().as_secs()),
            "directory": session.directory.to_string_lossy(),
            "last_exit_code": session.exit_code,
        }));
    }
    terminals.sort_by(|a, b| a["session"].as_str().cmp(&b["session"].as_str()));
    Response::ok(json!({
        "terminals": terminals,
        "count": terminals.len(),
    }))
}

/// stop ends what is running. The folder and the variables are kept, because
/// stopping a command is not starting again from nothing.
async fn stop(which: &(i64, String)) -> Response {
    let mut sessions = open().await;
    let Some(session) = sessions.get_mut(which) else {
        return Response::ok(json!({ "running": false, "note": "nothing was running" }));
    };
    let was = session.running.clone();
    session.end().await;
    let printed = session.take_printed().await;
    Response::ok(json!({
        "stopped": was.clone().unwrap_or_else(|| "nothing".into()),
        "running": false,
        "output": trimmed(&printed),
        "directory": session.directory.to_string_lossy(),
    }))
}

/// Every terminal there is, by the conversation it belongs to and its name.
///
/// Two people, or one person in two chats, never share a folder or a variable.
/// Within a conversation, a name is a separate terminal: its own folder, its
/// own variables, its own one-thing-at-a-time. That is what lets a build and a
/// test run happen at once instead of queueing.
static SESSIONS: Mutex<Option<HashMap<(i64, String), Session>>> = Mutex::const_new(None);

/// open is the map, made on first use. A const Mutex cannot hold a HashMap
/// directly, and one lock is better than the lazy-static machinery.
async fn open() -> tokio::sync::MappedMutexGuard<'static, HashMap<(i64, String), Session>> {
    tokio::sync::MutexGuard::map(SESSIONS.lock().await, |held| held.get_or_insert_with(HashMap::new))
}

/// A conversation's shell: where it is, what it has exported, and what it is
/// doing at the moment.
struct Session {
    /// Where the next command starts. Changed by a command that cds.
    directory: PathBuf,
    /// Where the exported variables are kept between commands, written by the
    /// shell itself in a form it can read back.
    exports: PathBuf,
    /// The command in flight, and the child running it.
    running: Option<String>,
    /// When it started, so that "still running" can say for how long. A person
    /// reading "it is still going" wants to know whether that is four seconds
    /// or four minutes, and so does a model deciding whether to wait again.
    started: Option<std::time::Instant>,
    child: Option<tokio::process::Child>,
    stdin: Option<tokio::process::ChildStdin>,
    /// The two tasks reading what it prints. Waited for when it ends, because a
    /// process exits before its output has necessarily been read and taking the
    /// answer then loses the last of it.
    readers: Vec<tokio::task::JoinHandle<()>>,
    printed: Arc<Mutex<String>>,
    exit_code: Option<i32>,
}

impl Session {
    fn new(root: &Path) -> Session {
        let exports = std::env::temp_dir().join(format!("sag-shell-{}-{}.env", std::process::id(), nonce()));
        Session {
            directory: root.to_path_buf(),
            exports,
            running: None,
            started: None,
            child: None,
            stdin: None,
            readers: Vec::new(),
            printed: Arc::new(Mutex::new(String::new())),
            exit_code: None,
        }
    }

    /// send runs a command as its own child, with the shell state around it.
    ///
    /// The wrapper is three things either side of what was asked for: read back
    /// what was exported, go where the shell was, and afterwards write both out
    /// again. The command's own exit code is the child's, so nothing has to be
    /// parsed out of what it printed and nothing it prints can be mistaken for
    /// a marker.
    async fn send(&mut self, command: &str, directory: &Path) -> Result<(), String> {
        let script = wrapped(command, directory, &self.exports);
        let mut started = tokio::process::Command::new(shell());
        let started = started
            .arg(shell_flag())
            .arg(&script)
            .current_dir(directory);
        // The person's PATH, not this process's.
        //
        // An application opened from the Finder is started by launchd with
        // `/usr/bin:/bin:/usr/sbin:/sbin`, so everything they installed
        // themselves is invisible to it: `node -v` answered "command not found"
        // inside the application and worked in their own terminal. A tool
        // started FROM a terminal inherits this and never notices; this one has
        // to ask for it.
        let started = match crate::environment::run_path() {
            Some(path) => started.env("PATH", path),
            None => started,
        };
        let started = started
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        // Its own process group, so stopping stops the WHOLE command.
        //
        // Killing the shell alone leaves whatever it started: `a | b` leaves b,
        // and a build script leaves the compiler it spawned, still running,
        // still holding the files, with nothing left that knows about them. A
        // group can be ended in one go, which is what a terminal does when
        // somebody presses control-C.
        #[cfg(unix)]
        let started = started.process_group(0);
        let mut child = started
            .spawn()
            .map_err(|err| format!("the command could not be started: {err}"))?;

        self.stdin = child.stdin.take();
        // Both channels into one buffer, in the order they arrive, because that
        // is what a person sees in a terminal: a compiler's warnings belong
        // between the lines they interrupted.
        let printed = self.printed.clone();
        self.readers.clear();
        if let Some(out) = child.stdout.take() {
            self.readers.push(gather(Box::pin(out), printed.clone()));
        }
        if let Some(err) = child.stderr.take() {
            self.readers.push(gather(Box::pin(err), printed));
        }
        self.child = Some(child);
        self.running = Some(command.trim().to_string());
        self.started = Some(std::time::Instant::now());
        self.exit_code = None;
        Ok(())
    }

    /// type_in answers a program that is asking something. It goes to the
    /// command's OWN input, which nothing else can consume.
    async fn type_in(&mut self, input: &str) -> Result<(), String> {
        let Some(stdin) = self.stdin.as_mut() else {
            return Err("what is running is not listening for anything".into());
        };
        let mut line = input.to_string();
        if !line.ends_with('\n') {
            line.push('\n');
        }
        stdin
            .write_all(line.as_bytes())
            .await
            .map_err(|err| format!("that could not be typed in: {err}"))?;
        stdin.flush().await.map_err(|err| format!("that could not be typed in: {err}"))
    }

    /// harvest notices a command that has finished, and takes the shell state
    /// it left behind.
    async fn harvest(&mut self) {
        let Some(child) = self.child.as_mut() else { return };
        // Still going, or gone in a way wait cannot explain: either way there
        // is nothing to harvest yet.
        let Ok(Some(status)) = child.try_wait() else { return };
        // The process is gone; its output may not all have been read yet.
        // Waiting for the readers is the difference between an answer and an
        // answer missing its last lines, which is the kind of wrong that reads
        // as a flaky tool.
        for reader in std::mem::take(&mut self.readers) {
            let _ = reader.await;
        }
        self.exit_code = status.code();
        self.running = None;
        self.started = None;
        self.child = None;
        self.stdin = None;
        // Where the command left the shell, if it moved it.
        if let Ok(moved) = tokio::fs::read_to_string(cwd_file(&self.exports)).await {
            let moved = moved.trim();
            if !moved.is_empty() && Path::new(moved).is_dir() {
                self.directory = PathBuf::from(moved);
            }
        }
    }

    async fn take_printed(&mut self) -> String {
        let mut held = self.printed.lock().await;
        std::mem::take(&mut *held)
    }

    /// busy is what a call gets when something else is already running: what is
    /// going on, and the fact that this command was not started.
    async fn busy(&mut self, refused: &str) -> Response {
        let printed = self.take_printed().await;
        let going = self.running.clone().unwrap_or_default();
        let seconds = self.started.map(|at| at.elapsed().as_secs()).unwrap_or(0);
        // What it has printed SINCE THE LAST CALL, which is often nothing: the
        // previous call already took it. Saying "its output is below" over an
        // empty field is the kind of small lie that makes a tool untrustworthy.
        let since = if printed.trim().is_empty() {
            "It has printed nothing new since the last call.".to_string()
        } else {
            "What it has printed since the last call is below.".to_string()
        };
        Response::ok(json!({
            "started": false,
            "note": format!(
                "{refused:?} was NOT started: this conversation's terminal is busy with {going:?}, \
                 running {seconds}s so far. {since} Wait for it (call again with no command and a \
                 wait), answer it with input if it is asking something, or stop it."
            ),
            "output": trimmed(&printed),
            "running": true,
            "running_command": going,
            "running_for_seconds": seconds,
            "directory": self.directory.to_string_lossy(),
        }))
    }

    /// answer is what this call hands back: everything printed since the last
    /// one, and whether there is more coming.
    async fn answer(&mut self) -> Response {
        let printed = self.take_printed().await;
        let running = self.running.clone();
        let seconds = self.started.map(|at| at.elapsed().as_secs());
        Response::ok(json!({
            "output": trimmed(&printed),
            "running": running.is_some(),
            "running_command": running.clone(),
            // How long it has been going, while it is: a model deciding whether
            // to wait again is deciding with this.
            "running_for_seconds": running.as_ref().map(|_| seconds.unwrap_or(0)),
            "exit_code": self.exit_code,
            "directory": self.directory.to_string_lossy(),
        }))
    }

    /// end stops what is running, and everything it started, and leaves the
    /// shell state alone: stopping a command is not starting again from nothing.
    async fn end(&mut self) {
        if let Some(child) = self.child.as_mut() {
            #[cfg(unix)]
            if let Some(id) = child.id() {
                // The whole group, which is what control-C does. The shell's
                // own kill below is still there for anything this misses.
                let _ = nix::sys::signal::killpg(
                    nix::unistd::Pid::from_raw(id as i32),
                    nix::sys::signal::Signal::SIGKILL,
                );
            }
            let _ = child.kill().await;
        }
        // Whatever it managed to print before it was stopped is still worth
        // having: an error before a hang is usually the reason for the hang.
        for reader in std::mem::take(&mut self.readers) {
            let _ = reader.await;
        }
        self.child = None;
        self.stdin = None;
        self.running = None;
        self.started = None;
    }
}

/// gather reads one of a command's channels into the shared buffer.
///
/// BYTES, not lines, and both halves of that matter. A program that asks a
/// question prints a prompt with no newline after it ("Password: "), and a line
/// reader holds that back until something else prints one: the person would be
/// told nothing is happening while the thing sat waiting for them. And a single
/// enormous line, which is what a binary or a minified file is, would buffer
/// inside the line reader where the cap cannot reach it.
///
/// The handle is kept so that a finished command can be waited for: the process
/// exits before its output has necessarily been read, and taking the answer
/// then would lose the last of it.
fn gather(
    mut stream: std::pin::Pin<Box<dyn tokio::io::AsyncRead + Send>>,
    into: Arc<Mutex<String>>,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        let mut buffer = [0u8; 8192];
        loop {
            let read = match stream.read(&mut buffer).await {
                Ok(0) | Err(_) => return, // the end of it, or a channel that broke
                Ok(read) => read,
            };
            let mut held = into.lock().await;
            // Lossy on purpose: a command that prints bytes which are not text
            // should show as something rather than end the reading.
            held.push_str(&String::from_utf8_lossy(&buffer[..read]));
            keep_the_end(&mut held);
        }
    })
}

/// keep_the_end drops the front of the buffer once it is past what is held, so
/// a command nobody is reading cannot grow it for ever.
fn keep_the_end(held: &mut String) {
    if held.len() <= MAX_HELD {
        return;
    }
    let mut from = held.len() - MAX_HELD;
    while from < held.len() && !held.is_char_boundary(from) {
        from += 1;
    }
    held.replace_range(..from, "");
}

/// nonce keeps one conversation's state file from being another's.
fn nonce() -> String {
    use std::time::{SystemTime, UNIX_EPOCH};
    let now = SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_nanos()).unwrap_or(0);
    format!("{now:x}")
}

/// Where the folder is remembered, beside the exports.
fn cwd_file(exports: &Path) -> PathBuf {
    exports.with_extension("cwd")
}

/// resolve works out where a command should start.
///
/// Nothing given: wherever the shell already is, which is the point of a shell
/// that remembers. A relative path: from the folder the person chose. An
/// absolute path: itself, because they could type that themselves.
fn resolve(current: &Path, root: &Path, given: &str) -> Result<PathBuf, String> {
    let given = given.trim();
    if given.is_empty() {
        return Ok(if current.is_dir() { current.to_path_buf() } else { root.to_path_buf() });
    }
    let candidate = Path::new(given);
    let joined = if candidate.is_absolute() { candidate.to_path_buf() } else { root.join(candidate) };
    if !joined.is_dir() {
        return Err(format!("{} is not a folder on this computer", joined.to_string_lossy()));
    }
    Ok(joined.canonicalize().unwrap_or(joined))
}

/// quoted puts a path into a shell line safely, whatever is in its name.
fn quoted(path: &Path) -> String {
    format!("'{}'", path.to_string_lossy().replace('\'', r"'\''"))
}

/// trimmed is what was printed, cut to what can be carried, and said so.
fn trimmed(text: &str) -> String {
    if text.len() <= MAX_OUTPUT {
        return text.to_string();
    }
    // The END is kept: a command that prints a lot says what happened last,
    // and an error is at the bottom.
    let cut = text.len() - MAX_OUTPUT;
    let mut boundary = cut;
    while boundary < text.len() && !text.is_char_boundary(boundary) {
        boundary += 1;
    }
    format!("[the first {cut} characters are not shown]\n{}", &text[boundary..])
}

/// wrapped is the command with the shell's memory either side of it.
///
/// `export -p` writes the exported variables in a form the shell can read back,
/// which is what makes a variable set in one call visible in the next. It is
/// POSIX, so it is the same in every shell somebody might have as theirs.
#[cfg(not(target_os = "windows"))]
fn wrapped(command: &str, directory: &Path, exports: &Path) -> String {
    format!(
        "[ -f {exports} ] && . {exports} 2>/dev/null\ncd {directory} 2>/dev/null\n{command}\n         __sag_code=$?\nexport -p > {exports} 2>/dev/null\npwd > {cwd} 2>/dev/null\nexit $__sag_code\n",
        exports = quoted(exports),
        directory = quoted(directory),
        command = command.trim_end(),
        cwd = quoted(&cwd_file(exports)),
    )
}

/// The same on Windows, where the folder is kept and the environment is not:
/// cmd has no portable way to write its variables in a form it can read back,
/// and a half-working memory would be worse than an honest one.
#[cfg(target_os = "windows")]
fn wrapped(command: &str, directory: &Path, exports: &Path) -> String {
    format!(
        "cd /d \"{directory}\" & {command} & set __sag_code=%errorlevel% & cd > \"{cwd}\" & exit /b %__sag_code%",
        directory = directory.to_string_lossy(),
        command = command.trim_end(),
        cwd = cwd_file(exports).to_string_lossy(),
    )
}

#[cfg(target_os = "windows")]
pub(crate) fn shell() -> String {
    "cmd.exe".to_string()
}

#[cfg(target_os = "windows")]
fn shell_flag() -> &'static str {
    "/C"
}

#[cfg(not(target_os = "windows"))]
pub(crate) fn shell() -> String {
    std::env::var("SHELL").unwrap_or_else(|_| "/bin/sh".to_string())
}

#[cfg(not(target_os = "windows"))]
fn shell_flag() -> &'static str {
    "-c"
}

#[cfg(test)]
pub(super) mod tests {
    use super::*;

    /// The chosen folder, once, for every test here.
    pub(super) fn workspace() -> &'static PathBuf {
        crate::tools::test_workspace()
    }

    pub(super) async fn shell_call(args: Value) -> Value {
        let answer = run(args).await;
        assert!(answer.ok, "{} ({})", answer.message, answer.kind);
        answer.content.expect("a successful call carries content")
    }

    /// The whole point of a shell that stays open: what one command does, the
    /// next one sees. A one-shot could do neither of these.
    #[tokio::test]
    async fn the_shell_remembers_between_calls() {
        workspace();
        let conversation = 9001;

        // A variable EXPORTED in one call... (a plain assignment is a live
        // shell's own memory, which is the thing this deliberately does not
        // keep; the tool's description says so.)
        shell_call(json!({ "command": "export SAG_TEST_VALUE=kept", "conversation": conversation })).await;
        let answer = shell_call(json!({ "command": "echo $SAG_TEST_VALUE", "conversation": conversation })).await;
        assert!(
            answer["output"].as_str().unwrap().contains("kept"),
            "the shell forgot a variable between calls: {answer:?}"
        );

        // ...and a folder moved into stays moved into.
        std::fs::create_dir_all(workspace().join("inner")).expect("a folder to move into");
        shell_call(json!({ "command": "cd inner", "conversation": conversation })).await;
        let answer = shell_call(json!({ "command": "pwd", "conversation": conversation })).await;
        assert!(
            answer["output"].as_str().unwrap().contains("inner"),
            "the shell forgot where it was: {answer:?}"
        );
        assert!(
            answer["directory"].as_str().unwrap().ends_with("inner"),
            "it did not report where it is: {answer:?}"
        );

        shell_call(json!({ "stop": true, "conversation": conversation })).await;
    }

    /// Two conversations are two shells. One person in two chats must not be
    /// typing into one terminal.
    #[tokio::test]
    async fn two_conversations_are_two_shells() {
        workspace();
        shell_call(json!({ "command": "export SAG_WHOSE=first", "conversation": 9002 })).await;
        shell_call(json!({ "command": "export SAG_WHOSE=second", "conversation": 9003 })).await;

        // Quoted: an unquoted [..] is a glob in zsh, which is the shell this
        // machine actually has.
        let first = shell_call(json!({ "command": "echo \"[$SAG_WHOSE]\"", "conversation": 9002 })).await;
        assert!(
            first["output"].as_str().unwrap().contains("[first]"),
            "one conversation's shell saw another's: {first:?}"
        );

        shell_call(json!({ "stop": true, "conversation": 9002 })).await;
        shell_call(json!({ "stop": true, "conversation": 9003 })).await;
    }

    /// A command slower than the wait comes back with what it has so far and
    /// says it is still going. This is what a ninety second ceiling could not
    /// do: an install used to be killed for taking as long as it takes.
    #[tokio::test]
    async fn something_slow_comes_back_and_keeps_going() {
        workspace();
        let conversation = 9004;

        let answer = shell_call(json!({
            "command": "echo starting; sleep 2; echo finished",
            "wait": 1,
            "conversation": conversation,
        }))
        .await;
        assert_eq!(answer["running"], true, "it should still be going: {answer:?}");
        assert!(
            answer["output"].as_str().unwrap().contains("starting"),
            "what it printed so far did not come back: {answer:?}"
        );
        assert!(
            !answer["output"].as_str().unwrap().contains("finished"),
            "it cannot have printed that yet: {answer:?}"
        );

        // Asking again gets the rest, and the exit code that ends it.
        let rest = shell_call(json!({ "wait": 10, "conversation": conversation })).await;
        assert_eq!(rest["running"], false, "it should be done: {rest:?}");
        assert!(
            rest["output"].as_str().unwrap().contains("finished"),
            "the rest of it did not come back: {rest:?}"
        );
        assert_eq!(rest["exit_code"], 0);

        shell_call(json!({ "stop": true, "conversation": conversation })).await;
    }

    /// A program that stops to ask can be answered, which a one-shot with no
    /// input could never do.
    #[tokio::test]
    async fn a_program_that_asks_can_be_answered() {
        workspace();
        let conversation = 9005;

        let asked = shell_call(json!({
            // Portable on purpose: the shell is the PERSON's ($SHELL), and
            // `read -p` is a bashism that means something else in zsh.
            "command": "printf 'name? '; read who; echo hello $who",
            "wait": 1,
            "conversation": conversation,
        }))
        .await;
        assert_eq!(asked["running"], true, "it should be waiting for an answer: {asked:?}");

        let answered = shell_call(json!({ "input": "Sam", "wait": 10, "conversation": conversation })).await;
        assert!(
            answered["output"].as_str().unwrap().contains("hello Sam"),
            "the answer did not reach it: {answered:?}"
        );

        shell_call(json!({ "stop": true, "conversation": conversation })).await;
    }

    /// A command sent while one is running gets the STATE, not a refusal.
    ///
    /// It used to come back as a bad-arguments with a sentence in it, which
    /// told a model that something was running and nothing it could decide
    /// with: not what, not for how long, not what it had printed. A whole turn
    /// spent learning something the tool already knew.
    #[tokio::test]
    async fn one_thing_at_a_time_and_it_says_what() {
        workspace();
        let conversation = 9006;
        shell_call(json!({
            "command": "echo working; sleep 3",
            "wait": 1,
            "conversation": conversation,
        }))
        .await;

        let busy = shell_call(json!({ "command": "echo second", "conversation": conversation })).await;
        assert_eq!(busy["started"], false, "it must say the new command did not run");
        assert_eq!(busy["running"], true);
        assert!(
            busy["running_command"].as_str().unwrap().contains("sleep 3"),
            "it must say WHAT is running: {busy:?}"
        );
        assert!(
            busy["running_for_seconds"].as_u64().is_some(),
            "it must say for how long: {busy:?}"
        );
        // Output is handed over ONCE: the call before this one already took
        // "working", so there is nothing new, and the note says so rather than
        // pointing at an empty field.
        assert!(
            busy["note"].as_str().unwrap().contains("nothing new"),
            "it should say there is nothing new rather than point at an empty output: {busy:?}"
        );
        // And the second command really did not run.
        let rest = shell_call(json!({ "wait": 10, "conversation": conversation })).await;
        assert!(
            !rest["output"].as_str().unwrap().contains("second"),
            "the refused command ran anyway: {rest:?}"
        );

        shell_call(json!({ "stop": true, "conversation": conversation })).await;
    }

    /// And stopping ends it, so the next command is not refused for ever.
    #[tokio::test]
    async fn stopping_frees_the_shell() {
        workspace();
        let conversation = 9007;
        shell_call(json!({ "command": "sleep 30", "wait": 1, "conversation": conversation })).await;
        shell_call(json!({ "stop": true, "conversation": conversation })).await;

        let after = shell_call(json!({ "command": "echo free", "wait": 10, "conversation": conversation })).await;
        assert!(
            after["output"].as_str().unwrap().contains("free"),
            "the shell was not free after stopping: {after:?}"
        );
        shell_call(json!({ "stop": true, "conversation": conversation })).await;
    }

    /// A command nobody is reading cannot grow the buffer for ever.
    ///
    /// The trim on the way OUT is not enough: output arrives whether or not
    /// anybody has asked for it, so something like `yes` left running would
    /// grow this until the machine noticed.
    #[test]
    fn what_is_held_is_bounded() {
        let mut held = String::new();
        for _ in 0..(MAX_HELD / 8 + 1000) {
            held.push_str("chatter\n");
            keep_the_end(&mut held);
        }
        assert!(held.len() <= MAX_HELD, "the buffer grew to {}", held.len());
        // And what is kept is the END, which is where a command says what
        // happened last.
        assert!(held.ends_with("chatter\n"), "it kept the wrong end");
    }

    /// The same, through the tool: a command that prints a great deal answers
    /// with what can be carried rather than with everything.
    #[tokio::test]
    async fn a_noisy_command_answers_with_what_can_be_carried() {
        workspace();
        let conversation = 9008;
        let answer = shell_call(json!({
            "command": "i=0; while [ $i -lt 60000 ]; do echo 'a line of output that is not short'; i=$((i+1)); done",
            "wait": 60,
            "conversation": conversation,
        }))
        .await;
        let output = answer["output"].as_str().unwrap();
        assert!(output.len() <= MAX_OUTPUT + 200, "one answer carried {} bytes", output.len());
        assert!(output.contains("a line of output"), "it carried nothing useful");
        shell_call(json!({ "stop": true, "conversation": conversation })).await;
    }

    /// The last of what a command printed must not be lost.
    ///
    /// A process exits before its output has necessarily been read, so taking
    /// the answer the moment it exits loses whatever was still in flight. It
    /// reads as a flaky tool: the same command, sometimes missing its last
    /// line. This runs a command whose whole point is that it prints and exits
    /// at once, many times, because a race that happens sometimes must be
    /// looked for many times.
    #[tokio::test]
    async fn nothing_printed_is_lost_when_a_command_ends() {
        workspace();
        for round in 0..25 {
            let conversation = 9100 + round;
            let answer = shell_call(json!({
                "command": "printf 'first\nsecond\nlast\n'",
                "wait": 10,
                "conversation": conversation,
            }))
            .await;
            let output = answer["output"].as_str().unwrap();
            assert!(
                output.contains("first") && output.contains("second") && output.contains("last"),
                "round {round} lost part of what it printed: {output:?}"
            );
            assert_eq!(answer["running"], false);
            shell_call(json!({ "stop": true, "conversation": conversation })).await;
        }
    }

    /// A question with no newline after it must arrive anyway.
    ///
    /// Every prompt worth answering ends without one ("Password: "), and a
    /// reader that waits for a line holds it back until something else prints
    /// one. The person is then told nothing is happening while the thing sits
    /// waiting for them.
    #[tokio::test]
    async fn a_prompt_with_no_newline_still_arrives() {
        workspace();
        let conversation = 9200;
        let asked = shell_call(json!({
            "command": "printf 'Password: '; read secret; echo \"[$secret]\"",
            "wait": 2,
            "conversation": conversation,
        }))
        .await;
        assert_eq!(asked["running"], true, "it should be waiting: {asked:?}");
        assert!(
            asked["output"].as_str().unwrap().contains("Password:"),
            "the question never arrived, so nobody could know to answer it: {asked:?}"
        );

        let answered = shell_call(json!({ "input": "hunter2", "wait": 10, "conversation": conversation })).await;
        assert!(
            answered["output"].as_str().unwrap().contains("[hunter2]"),
            "the answer did not reach it: {answered:?}"
        );
        shell_call(json!({ "stop": true, "conversation": conversation })).await;
    }

    /// Stopping ends what the command started, not just the shell.
    ///
    /// `sleep` inside a pipeline is a grandchild: killing the shell leaves it
    /// running, holding whatever it holds, with nothing left that knows about
    /// it. A group can be ended in one go, which is what control-C does.
    #[cfg(unix)]
    #[tokio::test]
    async fn stopping_ends_what_the_command_started() {
        workspace();
        let conversation = 9300;
        // A pipeline, so the sleeping process is not the shell itself, and a
        // duration nothing else would use, so this counts ITS OWN processes.
        // Counting every `sleep 45` on the machine means a leftover from
        // another run makes this fail for something it did not do, which is
        // how it failed once and then passed thirteen times.
        let tag = format!("45.{:03}", std::process::id() % 1000);
        shell_call(json!({
            "command": format!("sleep {tag} | cat"),
            "wait": 1,
            "conversation": conversation,
        }))
        .await;
        shell_call(json!({ "stop": true, "conversation": conversation })).await;

        // Waited FOR rather than waited a bit and hoped: signalling a group and
        // the system reaping it are two moments, and how far apart they are
        // depends on what else the machine is doing. A fixed pause passed alone
        // and failed in a full run, which is a flaky test rather than a fact.
        let mut orphans = 1;
        for _ in 0..60 {
            let still = std::process::Command::new("sh")
                .arg("-c")
                .arg(format!("ps -ax -o command | grep -c '[s]leep {tag}' || true"))
                .output()
                .expect("ask what is running");
            orphans = String::from_utf8_lossy(&still.stdout).trim().parse().unwrap_or(0);
            if orphans == 0 {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        }
        assert_eq!(orphans, 0, "stopping left {orphans} orphaned processes behind");
    }
}

#[cfg(test)]
mod sessions {
    use super::tests::{shell_call, workspace};
    use super::*;

    /// Two named terminals in one conversation run at the same time.
    ///
    /// Reported from use: parallel checks collided because one long command
    /// blocked everything else. One terminal is one thing at a time, which is
    /// what a shell is; the answer is not to make it two things at once but to
    /// let there be two terminals, which is what a person does.
    #[tokio::test]
    async fn two_named_terminals_do_not_wait_for_each_other() {
        workspace();
        let conversation = 9500;

        // A slow one, left going.
        let slow = shell_call(json!({
            "command": "sleep 5; echo slow done",
            "session": "build",
            "wait": 1,
            "conversation": conversation,
        }))
        .await;
        assert_eq!(slow["running"], true, "{slow:?}");

        // Another terminal answers immediately, rather than being told to wait.
        let quick = shell_call(json!({
            "command": "echo quick done",
            "session": "tests",
            "wait": 10,
            "conversation": conversation,
        }))
        .await;
        assert_eq!(quick["running"], false, "the second terminal was blocked: {quick:?}");
        assert!(quick["output"].as_str().unwrap().contains("quick done"));

        // And they are separate shells: what one sets, the other does not see.
        shell_call(json!({ "command": "export SAG_WHICH=build", "session": "build2", "conversation": conversation })).await;
        let other = shell_call(json!({ "command": "echo \"[$SAG_WHICH]\"", "session": "tests", "conversation": conversation })).await;
        assert!(
            other["output"].as_str().unwrap().contains("[]"),
            "one terminal saw another's variables: {other:?}"
        );

        shell_call(json!({ "stop": true, "session": "build", "conversation": conversation })).await;
        shell_call(json!({ "stop": true, "session": "tests", "conversation": conversation })).await;
        shell_call(json!({ "stop": true, "session": "build2", "conversation": conversation })).await;
    }

    /// A call with no session is the one called "main", so everything written
    /// before this existed behaves exactly as it did.
    #[tokio::test]
    async fn no_session_is_the_main_one() {
        workspace();
        let conversation = 9501;
        shell_call(json!({ "command": "export SAG_MAIN=yes", "conversation": conversation })).await;
        let named = shell_call(json!({
            "command": "echo \"[$SAG_MAIN]\"", "session": "main", "conversation": conversation,
        }))
        .await;
        assert!(
            named["output"].as_str().unwrap().contains("[yes]"),
            "a call with no session is not the terminal called main: {named:?}"
        );
        shell_call(json!({ "stop": true, "conversation": conversation })).await;
    }

    /// One answer about everything going on, which is what somebody with three
    /// things running wants to ask.
    #[tokio::test]
    async fn status_says_what_every_terminal_is_doing() {
        workspace();
        let conversation = 9502;
        shell_call(json!({ "command": "sleep 5", "session": "one", "wait": 1, "conversation": conversation })).await;
        shell_call(json!({ "command": "echo done", "session": "two", "wait": 10, "conversation": conversation })).await;

        let status = shell_call(json!({ "status": true, "conversation": conversation })).await;
        let terminals = status["terminals"].as_array().unwrap();
        assert_eq!(terminals.len(), 2, "{status:?}");

        let one = terminals.iter().find(|t| t["session"] == "one").expect("the slow one");
        assert_eq!(one["running"], true);
        assert!(one["running_for_seconds"].as_u64().is_some(), "it must say for how long");
        let two = terminals.iter().find(|t| t["session"] == "two").expect("the quick one");
        assert_eq!(two["running"], false);
        assert_eq!(two["last_exit_code"], 0);

        // And another conversation's terminals are not in it.
        let elsewhere = shell_call(json!({ "status": true, "conversation": 9503 })).await;
        assert_eq!(elsewhere["count"], 0, "one conversation saw another's terminals");

        shell_call(json!({ "stop": true, "session": "one", "conversation": conversation })).await;
        shell_call(json!({ "stop": true, "session": "two", "conversation": conversation })).await;
    }
}
