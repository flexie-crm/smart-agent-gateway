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

/// How long the readers get, once the command itself has gone, before the
/// answer goes out without them (Windows).
///
/// WHY THIS EXISTS, and it is not tidiness. A reader ends at end-of-file, and
/// end-of-file comes when the LAST holder of the pipe lets go. A command's
/// grandchild inherits that pipe, so a command that leaves something running
/// (a server, a watcher) keeps it open after the command itself is gone. On
/// Unix that cannot happen for long, because ending a command ends its process
/// GROUP and the whole tree goes at once. On Windows it happens constantly, and
/// the wait is forever: measured, a terminal that had started an HTTP server
/// sat in `running` with no `stop` anywhere near it, because `harvest` was
/// waiting on a pipe the server still held.
///
/// Two seconds is chosen against what the wait is FOR: the readers are draining
/// what a process already wrote and has now exited, which takes microseconds.
/// Anything approaching this bound means somebody else is holding the pipe, and
/// then the last few lines are worth less than the call coming back at all.
#[cfg(windows)]
const READERS_PATIENCE: std::time::Duration = std::time::Duration::from_secs(2);

/// waited_for_readers drains what a command printed, giving up after
/// `READERS_PATIENCE` (Windows only).
///
/// Its counterpart on every other platform is the plain loop at each call site,
/// left exactly where it was: a pipe there closes when the process group ends,
/// so there is nothing to give up on and nothing to change.
#[cfg(windows)]
async fn waited_for_readers(readers: Vec<tokio::task::JoinHandle<()>>) {
    for reader in readers {
        if tokio::time::timeout(READERS_PATIENCE, &mut { reader })
            .await
            .is_err()
        {
            return;
        }
    }
}

/// Start a console program without giving it a console (Windows).
///
/// CREATE_NO_WINDOW, and not `HideWindow`: that one still allocates the console
/// and merely asks for it not to be shown, which flashes on the way past. The
/// gateway reached the same conclusion for the same reason (`gateway.rs`).
#[cfg(windows)]
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

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
        Err(err) => {
            return Response::bad_arguments(format!("the arguments could not be read: {err}"))
        }
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
    (
        conversation,
        if name.is_empty() {
            "main".to_string()
        } else {
            name.to_string()
        },
    )
}

/// wait_for settles how long this call may wait.
fn wait_for(asked: u64) -> std::time::Duration {
    let seconds = if asked == 0 {
        DEFAULT_WAIT
    } else {
        asked.min(MAX_WAIT)
    };
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
    tokio::sync::MutexGuard::map(SESSIONS.lock().await, |held| {
        held.get_or_insert_with(HashMap::new)
    })
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
    /// What holds the command and everything it started, on Windows.
    ///
    /// The other platform has this already, in the shape of a process GROUP
    /// that `end` signals. There is no group to signal here, so the tree is an
    /// object the child is put into at birth, and ending it ends all of them.
    ///
    /// Held per COMMAND, not per session: `send` replaces it, and the handle
    /// going means anything the previous command left behind goes with it.
    #[cfg(windows)]
    job: Option<super::windows_job::Job>,
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
        let exports =
            std::env::temp_dir().join(format!("sag-shell-{}-{}.env", std::process::id(), nonce()));
        Session {
            directory: root.to_path_buf(),
            exports,
            running: None,
            started: None,
            child: None,
            #[cfg(windows)]
            job: None,
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
        // UNCHANGED everywhere but Windows: the same three calls in the same
        // order, written out in full rather than shared with the branch below,
        // so what a Mac runs is visible on the page rather than assembled.
        #[cfg(not(windows))]
        let started = started
            .arg(shell_flag())
            .arg(&script)
            .current_dir(directory);
        // The script goes to cmd UNESCAPED, and on a command LINE rather than
        // in a file, because a file makes cmd expand per cent signs in the
        // person's own command (see `wrapped`).
        //
        // `arg` quotes for the C runtime's parser, which is what almost every
        // program on Windows uses and what cmd.exe emphatically does not: it
        // turns a quote into \" , and cmd reads the backslash as part of the
        // name. So `cd /d "C:\somewhere"` arrived as `cd /d \"C:\somewhere\"`
        // and every single call answered "The filename, directory name, or
        // volume label syntax is incorrect" twice, once for the cd in and once
        // for the cd that records where the shell ended up, with those two lines
        // wrapped around the real output where the model then read them as part
        // of the answer. A command of the person's own carrying quotes was
        // broken the same way: --format="%h %s" reached git as --format=\"%h
        // and %s\".
        #[cfg(windows)]
        let started = started
            .arg(shell_flag())
            .raw_arg(&script)
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
        // And no console window, which is the same kind of line for the other
        // platform.
        //
        // A GUI application has no console of its own, so Windows allocates one
        // for any console program it starts and shows it. Every call to this
        // tool therefore put a cmd window on the screen, in front of whatever
        // the person was reading, for as long as the command ran: a build or a
        // test run sat there until it finished. Nothing failed, which is why it
        // reached somebody rather than a test. macOS has no console to allocate
        // and so had nothing to show, which is why it is only seen here.
        //
        // `creation_flags` here is tokio's own, not the std extension trait:
        // this is a tokio Command, and importing `CommandExt` for it compiles
        // to an unused import.
        #[cfg(windows)]
        let started = started.creation_flags(CREATE_NO_WINDOW);
        let mut child = started
            .spawn()
            .map_err(|err| format!("the command could not be started: {err}"))?;

        // The tree, on the platform that has no process group to signal.
        //
        // Taken before anything is read from the child, so there is no moment
        // where a command is running outside the only thing that can end it.
        // Replacing what was here ends whatever the PREVIOUS command left
        // behind, which is what a terminal that does one thing at a time means.
        #[cfg(windows)]
        {
            self.job = super::windows_job::Job::hold(&child);
            debug_assert!(
                self.job.is_some(),
                "no job object was created, so nothing can end this command's tree"
            );
        }

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
        stdin
            .flush()
            .await
            .map_err(|err| format!("that could not be typed in: {err}"))
    }

    /// harvest notices a command that has finished, and takes the shell state
    /// it left behind.
    async fn harvest(&mut self) {
        let Some(child) = self.child.as_mut() else {
            return;
        };
        // Still going, or gone in a way wait cannot explain: either way there
        // is nothing to harvest yet.
        let Ok(Some(status)) = child.try_wait() else {
            return;
        };
        // The process is gone; its output may not all have been read yet.
        // Waiting for the readers is the difference between an answer and an
        // answer missing its last lines, which is the kind of wrong that reads
        // as a flaky tool.
        //
        #[cfg(not(windows))]
        for reader in std::mem::take(&mut self.readers) {
            let _ = reader.await;
        }
        // BOUNDED on Windows, because "the process is gone" is not "the pipe is
        // closed" there: whatever it started still holds the pipe, and this is
        // where a call with no `stop` in it hangs for ever (see
        // `READERS_PATIENCE`).
        #[cfg(windows)]
        waited_for_readers(std::mem::take(&mut self.readers)).await;
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
        // The whole TREE, which is what the group signal below is on the other
        // platform. It is here rather than beside that signal because
        // `self.child.as_mut()` borrows all of self, and reading `self.job`
        // inside that block would not compile.
        //
        // This is the call that reaches a server the command started. Killing
        // the shell alone leaves that server running AND holding the pipes this
        // reads from, so the reads at the end of this function never finish and
        // the call never returns: measured, a `stop` on a terminal that had
        // started an HTTP server sat in `running` for ever.
        #[cfg(windows)]
        if let Some(job) = &self.job {
            job.end();
        }
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
        //
        #[cfg(not(windows))]
        for reader in std::mem::take(&mut self.readers) {
            let _ = reader.await;
        }
        // Bounded on Windows for the same reason as `harvest`. The job above
        // should already have closed every pipe by ending the tree, so reaching
        // the bound here means the job was refused and this is the backstop.
        #[cfg(windows)]
        waited_for_readers(std::mem::take(&mut self.readers)).await;
        self.child = None;
        #[cfg(windows)]
        {
            self.job = None;
        }
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
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
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
        return Ok(if current.is_dir() {
            current.to_path_buf()
        } else {
            root.to_path_buf()
        });
    }
    let candidate = Path::new(given);
    let joined = if candidate.is_absolute() {
        candidate.to_path_buf()
    } else {
        root.join(candidate)
    };
    if !joined.is_dir() {
        return Err(format!(
            "{} is not a folder on this computer",
            joined.to_string_lossy()
        ));
    }
    Ok(joined.canonicalize().unwrap_or(joined))
}

/// quoted puts a path into a shell line safely, whatever is in its name.
///
/// Only the POSIX `wrapped` below uses it, so on Windows it is compiled and
/// never called, which is a dead_code warning on every build of this crate
/// there. The cfg is on the function rather than on the warning: macOS goes on
/// compiling and calling exactly what it compiles and calls today.
#[cfg(not(target_os = "windows"))]
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
    format!(
        "[the first {cut} characters are not shown]\n{}",
        &text[boundary..]
    )
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

/// The same on Windows, where the folder is kept and the environment is not.
///
/// THIS IS A DEFECT, NOT A DECISION, and it is recorded as work still to do.
/// The comment that used to sit here said cmd had no portable way to write its
/// variables back. That is false: `set` prints all of them and each line
/// restores as `set "NAME=value"`, measured, including values holding `&`, `%`
/// and `^`.
///
/// What is NOT solved is where to put that round trip. Both attempts failed on
/// the person's own command, and each failed silently, which is worse than not
/// working:
///
/// - As one line, cmd has no separator that survives a block. A false
///   `if exist x (...)` discards every clause after it, and everything after a
///   `for`'s `do` is absorbed into the loop body and runs once per iteration.
/// - As a batch file, the bookkeeping is correct but the person's command is no
///   longer on a command line, so cmd expands per cent signs in it:
///   `git log --format="%h %s"` arrives as `--format=s`. Doubling them is not a
///   fix either, measured: `%%REAL%%` stops a real variable expanding and
///   `%%%%` corrupts a literal `%%`.
///
/// The shape that should work is the person's command left on the command line
/// where per cents pass through untouched, with only the restore in a file the
/// line `call`s, `call` being the one form that neither absorbs nor discards
/// what follows it. Unmeasured, which is why it is not here.
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

    /// A per cent sign reaches the command intact.    /// A command answers with its own output and nothing else.
    ///
    /// This is the one that was broken, and it was broken for EVERY command.
    /// `arg` escapes for the C runtime's parser, which is what nearly every
    /// program on Windows uses and what cmd.exe does not: it writes a quote as
    /// `\"`, and cmd reads the backslash as part of the name. Both of the
    /// wrapper's own quoted paths therefore failed, so every answer arrived as
    ///
    ///     The filename, directory name, or volume label syntax is incorrect.
    ///     <what the command actually printed>
    ///     The filename, directory name, or volume label syntax is incorrect.
    ///
    /// and the model read those two lines as part of the result. Worse, the
    /// first of them is the `cd` INTO the working folder and the second is the
    /// `cd` that records where the shell ended up, so the terminal's memory of
    /// its own folder never worked here at all.
    ///
    /// Asserted as equality rather than `contains`, because `contains` is what
    /// would have passed throughout: the real output was always in there,
    /// sandwiched between two errors.
    #[cfg(windows)]
    #[tokio::test]
    async fn a_command_answers_with_its_own_output_and_nothing_else() {
        workspace();
        let answer = shell_call(json!({
            "command": "echo hello",
            "wait": 20,
            "conversation": 9401,
        }))
        .await;
        let printed = answer["output"].as_str().unwrap_or_default().trim().to_string();
        assert_eq!(printed, "hello", "something other than the command spoke");
    }

    /// And a quote the person wrote reaches the program they wrote it for.
    ///
    /// Reported from use as per cent signs being eaten, which they are not:
    /// `--format=%h %s` survives untouched. What broke `git log
    /// --format="%h %s"` was the quoting, which reached git as `--format=\"%h`
    /// and `%s\"`. Both halves are asserted here so the diagnosis cannot drift
    /// back to the per cent sign.
    #[cfg(windows)]
    #[tokio::test]
    async fn a_quoted_argument_reaches_the_command() {
        workspace();
        let answer = shell_call(json!({
            "command": "echo \"--format=%h %s\"",
            "wait": 20,
            "conversation": 9402,
        }))
        .await;
        let printed = answer["output"].as_str().unwrap_or_default().trim().to_string();
        assert_eq!(
            printed, "\"--format=%h %s\"",
            "the quotes or the per cent signs did not survive"
        );
    }

    /// And the folder the shell is left in is the folder the next call starts
    /// in, which is what the `cd` that failed above was for.
    #[cfg(windows)]
    #[tokio::test]
    async fn the_shell_remembers_which_folder_it_is_in() {
        workspace();
        let conversation = 9403;
        shell_call(json!({
            "command": "mkdir sag_cd_probe 2>nul & cd sag_cd_probe",
            "wait": 20,
            "conversation": conversation,
        }))
        .await;
        let answer = shell_call(json!({
            "command": "echo still here",
            "wait": 20,
            "conversation": conversation,
        }))
        .await;
        let directory = answer["directory"].as_str().unwrap_or_default().to_string();
        assert!(
            directory.ends_with("sag_cd_probe"),
            "the shell forgot where it was: {directory}"
        );
    }

    /// The whole point of a shell that stays open: what one command does, the
    /// next one sees. A one-shot could do neither of these.
    ///
    /// NOT ON WINDOWS, and that gate is a KNOWN DEFECT rather than a decision:
    /// the terminal there keeps the folder and loses the variables. See
    /// `wrapped` for what was measured and what the fix has to do. This
    /// test passes on Windows the moment that lands, and turning it back on is
    /// how the fix should be proved.
    #[cfg(not(windows))]
    #[tokio::test]
    async fn the_shell_remembers_between_calls() {
        workspace();
        let conversation = 9001;

        // A variable EXPORTED in one call... (a plain assignment is a live
        // shell's own memory, which is the thing this deliberately does not
        // keep; the tool's description says so.)
        shell_call(
            json!({ "command": "export SAG_TEST_VALUE=kept", "conversation": conversation }),
        )
        .await;
        let answer =
            shell_call(json!({ "command": "echo $SAG_TEST_VALUE", "conversation": conversation }))
                .await;
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
    ///
    /// NOT ON WINDOWS: it shows the separation with a variable, and variables
    /// are the known defect (see `the_shell_remembers_between_calls`). The
    /// property itself is real on both platforms.
    #[cfg(not(windows))]
    #[tokio::test]
    async fn two_conversations_are_two_shells() {
        workspace();
        shell_call(json!({ "command": "export SAG_WHOSE=first", "conversation": 9002 })).await;
        shell_call(json!({ "command": "export SAG_WHOSE=second", "conversation": 9003 })).await;

        // Quoted: an unquoted [..] is a glob in zsh, which is the shell this
        // machine actually has.
        let first =
            shell_call(json!({ "command": "echo \"[$SAG_WHOSE]\"", "conversation": 9002 })).await;
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

        // The same shape in either shell: print, take longer than the wait,
        // print again. `;` sequences in a POSIX shell and `&` in cmd, and cmd
        // waits with `ping` because `timeout` refuses a redirected stdin,
        // which is exactly what this tool hands it (measured: it exits at once
        // saying "Input redirection is not supported").
        #[cfg(not(windows))]
        let slowly = "echo starting; sleep 2; echo finished";
        // `sleep` is not a Windows program, so cmd waits with `ping`, and the
        // count is the stand-in for the seconds.
        //
        // Three, not more. This test flakes when the whole module runs in
        // parallel, and a wider margin is NOT the fix: it was measured at
        // eight and came out WORSE than at three. Written down here so it is not
        // tried again.
        #[cfg(windows)]
        let slowly = "echo starting & ping -n 3 127.0.0.1 >nul & echo finished";

        let answer = shell_call(json!({
            "command": slowly,
            "wait": 1,
            "conversation": conversation,
        }))
        .await;
        assert_eq!(
            answer["running"], true,
            "it should still be going: {answer:?}"
        );
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

        // Portable on purpose: the shell is the PERSON's ($SHELL), and
        // `read -p` is a bashism that means something else in zsh.
        #[cfg(not(windows))]
        let asks = "printf 'name? '; read who; echo hello $who";
        // cmd's own `set /p` cannot stand in here, and both reasons were
        // measured: it does not wait on a redirected stdin, and `%who%` is
        // expanded when the LINE is parsed, so the echo carries the literal
        // text whatever was typed. PowerShell ships with every Windows and
        // does exactly what the POSIX line does.
        #[cfg(windows)]
        let asks = "powershell -NoProfile -Command \"Write-Host -NoNewline 'name? '; \
                    $n = [Console]::In.ReadLine(); Write-Host ('hello ' + $n)\"";

        let asked = shell_call(json!({
            "command": asks,
            "wait": 1,
            "conversation": conversation,
        }))
        .await;
        assert_eq!(
            asked["running"], true,
            "it should be waiting for an answer: {asked:?}"
        );

        let answered =
            shell_call(json!({ "input": "Sam", "wait": 10, "conversation": conversation })).await;
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
        // Print, then stay busy for longer than the wait. `marker` is what the
        // answer must name as the thing still running, so it moves with the
        // command rather than being spelled twice.
        #[cfg(not(windows))]
        let (working, marker) = ("echo working; sleep 3", "sleep 3");
        // The cmd stand-in for `sleep 3`. Deliberately not widened: see
        // `something_slow_comes_back_and_keeps_going`.
        #[cfg(windows)]
        let (working, marker) = ("echo working & ping -n 4 127.0.0.1 >nul", "ping -n 4");

        shell_call(json!({
            "command": working,
            "wait": 1,
            "conversation": conversation,
        }))
        .await;

        let busy =
            shell_call(json!({ "command": "echo second", "conversation": conversation })).await;
        assert_eq!(
            busy["started"], false,
            "it must say the new command did not run"
        );
        assert_eq!(busy["running"], true);
        assert!(
            busy["running_command"].as_str().unwrap().contains(marker),
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

        let after =
            shell_call(json!({ "command": "echo free", "wait": 10, "conversation": conversation }))
                .await;
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
        // Far more output than one answer carries, however the shell says it.
        // cmd's `for /L` is its loop; measured at 3000 lines in 0.28s, so
        // 60000 is the same order of work the POSIX loop does.
        //
        // THE BRACKETS ARE LOAD-BEARING. Everything after `do` is absorbed into
        // the loop body, and this command is spliced into a line that continues
        // `& set __sag_code=%errorlevel% & cd > ... & exit /b ...` (see
        // `wrapped`). Without them the shell's own bookkeeping runs once per
        // iteration and the `exit /b` ends the batch on the FIRST one, so the
        // test would fail for a reason that has nothing to do with output being
        // carried. Measured both ways: unbracketed, the trailing clause ran
        // three times out of three iterations; bracketed, once.
        #[cfg(not(windows))]
        let noisy = "i=0; while [ $i -lt 60000 ]; do echo 'a line of output that is not short'; i=$((i+1)); done";
        #[cfg(windows)]
        let noisy = "(for /L %i in (1,1,60000) do @echo a line of output that is not short)";

        let answer = shell_call(json!({
            "command": noisy,
            "wait": 60,
            "conversation": conversation,
        }))
        .await;
        let output = answer["output"].as_str().unwrap();
        assert!(
            output.len() <= MAX_OUTPUT + 200,
            "one answer carried {} bytes",
            output.len()
        );
        assert!(
            output.contains("a line of output"),
            "it carried nothing useful"
        );
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
            // Three lines in one command, however the shell spells it. cmd's
            // `echo` is one line at a time and `&` joins them; measured, all
            // three arrive.
            #[cfg(not(windows))]
            let says = "printf 'first\nsecond\nlast\n'";
            #[cfg(windows)]
            let says = "echo first& echo second& echo last";

            let answer = shell_call(json!({
                "command": says,
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
        // The prompt carries no newline, which is the whole point: it has to
        // reach the reader before anybody can know to answer it.
        #[cfg(not(windows))]
        let asks = "printf 'Password: '; read secret; echo \"[$secret]\"";
        #[cfg(windows)]
        let asks = "powershell -NoProfile -Command \"Write-Host -NoNewline 'Password: '; \
                    $s = [Console]::In.ReadLine(); Write-Host ('[' + $s + ']')\"";

        let asked = shell_call(json!({
            "command": asks,
            "wait": 2,
            "conversation": conversation,
        }))
        .await;
        assert_eq!(asked["running"], true, "it should be waiting: {asked:?}");
        assert!(
            asked["output"].as_str().unwrap().contains("Password:"),
            "the question never arrived, so nobody could know to answer it: {asked:?}"
        );

        let answered =
            shell_call(json!({ "input": "hunter2", "wait": 10, "conversation": conversation }))
                .await;
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
                .arg(format!(
                    "ps -ax -o command | grep -c '[s]leep {tag}' || true"
                ))
                .output()
                .expect("ask what is running");
            orphans = String::from_utf8_lossy(&still.stdout)
                .trim()
                .parse()
                .unwrap_or(0);
            if orphans == 0 {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        }
        assert_eq!(
            orphans, 0,
            "stopping left {orphans} orphaned processes behind"
        );
    }

    /// The same property on Windows, where there is no group to signal.
    ///
    /// REPORTED FROM USE, and this is the shape of it: a terminal started an
    /// HTTP server, `stop` killed the `cmd` that started it, the server lived
    /// on, and because it had inherited `cmd`'s output channel that channel
    /// never closed. `end` waits for the readers, a reader ends at end-of-file,
    /// and end-of-file never came. The call sat in `running` for ever and the
    /// chat showed a spinner that could not finish.
    ///
    /// So this asserts TWO things, and the first is the one that bit:
    ///
    ///   1. the call comes BACK, which it could not do before
    ///   2. nothing it started is still running
    ///
    /// Asserting only the second would pass a build where `stop` returns
    /// promptly and leaks, and would HANG rather than fail on the real defect,
    /// which reads as a slow test rather than a broken product.
    ///
    /// `start /b` is what makes the grandchild: it launches `ping` and returns,
    /// so the process outlives the `cmd` that started it and keeps the pipe.
    /// The count is tagged with this process's id, because counting every
    /// `ping` on the machine lets a leftover from another run fail this for
    /// something it did not do.
    #[cfg(windows)]
    #[tokio::test]
    async fn stopping_ends_what_the_command_started() {
        workspace();
        let conversation = 9300;
        // -w is the wait between pings in milliseconds, and it is what carries
        // the tag: -n is a count and a hundred of those is long enough that
        // this can never pass because the thing simply finished.
        let tag = 900 + (std::process::id() % 90);

        // THE CALL ITSELF MUST COME BACK, and this is the half that actually
        // bit: `cmd` exits in about a quarter of a second here (measured), so
        // `harvest` sees a finished command and waits for the readers, which
        // the ping still holds. That wait was unbounded, so this call took the
        // ping's whole lifetime, about ninety-five seconds, with `wait: 2` and
        // no `stop` anywhere near it. That is the shape of the report: a
        // terminal call that never returns.
        //
        // Six seconds is the bound: two for the drain, plus room for a loaded
        // machine. It cannot be reached by the ping expiring.
        let began = std::time::Instant::now();
        let ran = tokio::time::timeout(
            std::time::Duration::from_secs(6),
            shell_call(json!({
                "command": format!("start /b ping -n 100 -w {tag} 127.0.0.1"),
                "wait": 2,
                "conversation": conversation,
            })),
        )
        .await
        .expect("the command call never returned: it is waiting on a pipe the grandchild holds");
        let started_in = began.elapsed();
        println!("PHASE start-command: {started_in:?}");
        assert!(
            started_in < std::time::Duration::from_secs(6),
            "the call took {started_in:?}, which is the grandchild's lifetime and not a wait"
        );
        let _ = ran;

        // The call has to COME BACK, and come back PROMPTLY. Before the job
        // object it did not: `stop` waited on a pipe the grandchild held open.
        //
        // The bound is asserted rather than just awaited, because this test can
        // pass for the wrong reason: the ping it starts runs for about ninety
        // seconds and then exits on its own, so a `stop` that hangs until the
        // grandchild dies of old age still ends with nothing running. Four
        // seconds is far below that and far above a real stop, which is
        // milliseconds.
        let began = std::time::Instant::now();
        let stopped = tokio::time::timeout(
            std::time::Duration::from_secs(20),
            shell_call(json!({ "stop": true, "conversation": conversation })),
        )
        .await
        .expect("stop never returned: it is waiting on a pipe a grandchild still holds");
        let took = began.elapsed();
        println!("PHASE stop: {took:?}");
        assert_eq!(stopped["running"], false, "{stopped:?}");
        assert!(
            took < std::time::Duration::from_secs(4),
            "stop took {took:?}: it did not end the tree, it waited for the grandchild to exit"
        );

        // And nothing is left. Waited FOR rather than waited a bit and hoped:
        // ending a job and the system reaping what was in it are two moments.
        //
        // Counted by this test's OWN tag, not by the program name. `tasklist`
        // cannot filter on a command line, so the count comes from CIM, where
        // the arguments are readable: every `ping` on the machine would
        // otherwise make a leftover from another run fail this for something it
        // did not do.
        // BOUNDED WELL BELOW THE PING'S OWN LIFETIME, which is about ninety
        // seconds. The first version of this polled for a hundred, and passed
        // because the ping expired on its own while the job kill did nothing at
        // all: `stop` returned in 127us and the count only reached zero at
        // 100.8s. A poll that outlasts the fixture cannot tell a kill from a
        // timeout, so it proves nothing. Eight seconds can only be a kill.
        let mut orphans = 1;
        for _ in 0..8 {
            let still = std::process::Command::new("powershell")
                .args([
                    "-NoProfile",
                    "-Command",
                    &format!(
                        "@(Get-CimInstance Win32_Process -Filter \"Name='PING.EXE'\" \
                          -ErrorAction SilentlyContinue | \
                          Where-Object {{ $_.CommandLine -like '*-w {tag}*' }}).Count"
                    ),
                ])
                .output()
                .expect("ask what is running");
            orphans = String::from_utf8_lossy(&still.stdout)
                .trim()
                .parse()
                .unwrap_or(0);
            if orphans == 0 {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(200)).await;
        }
        println!("PHASE orphan-poll: {:?}", began.elapsed());
        assert_eq!(
            orphans, 0,
            "stopping left {orphans} orphaned processes behind"
        );
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

        // A slow one, left going. Longer than the wait in either shell.
        #[cfg(not(windows))]
        let slowly = "sleep 5; echo slow done";
        #[cfg(windows)]
        let slowly = "ping -n 6 127.0.0.1 >nul & echo slow done";

        let slow = shell_call(json!({
            "command": slowly,
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
        assert_eq!(
            quick["running"], false,
            "the second terminal was blocked: {quick:?}"
        );
        assert!(quick["output"].as_str().unwrap().contains("quick done"));

        // And they are separate shells: what one sets, the other does not see.
        //
        // Unix only, and for the reason the three gated tests above carry: the
        // demonstration is an exported variable, and a cmd terminal keeps the
        // folder between calls and not the environment, deliberately. The two
        // assertions above this one are the part that holds on both platforms,
        // and they still run there.
        #[cfg(not(windows))]
        {
            shell_call(json!({ "command": "export SAG_WHICH=build", "session": "build2", "conversation": conversation })).await;
            let other = shell_call(json!({ "command": "echo \"[$SAG_WHICH]\"", "session": "tests", "conversation": conversation })).await;
            assert!(
                other["output"].as_str().unwrap().contains("[]"),
                "one terminal saw another's variables: {other:?}"
            );
        }

        shell_call(json!({ "stop": true, "session": "build", "conversation": conversation })).await;
        shell_call(json!({ "stop": true, "session": "tests", "conversation": conversation })).await;
        shell_call(json!({ "stop": true, "session": "build2", "conversation": conversation }))
            .await;
    }

    /// A call with no session is the one called "main", so everything written
    /// before this existed behaves exactly as it did.
    ///
    /// NOT ON WINDOWS: it shows the identity with a variable, and variables are
    /// the known defect (see `the_shell_remembers_between_calls`).
    #[cfg(not(windows))]
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
        // One terminal left going, one that finishes, so status has both to
        // report. The slow one has to outlast the wait in either shell.
        //
        // `sleep` is not a Windows program: it resolves here only because Git
        // Bash happens to be on this machine's PATH, which is luck rather than
        // a property of the platform. cmd waits with `ping`.
        #[cfg(not(windows))]
        let slowly = "sleep 5";
        // The cmd stand-in for `sleep 5`. Deliberately not widened: see
        // `something_slow_comes_back_and_keeps_going`. It matters more here
        // than anywhere else, because this test STOPS its terminals instead of
        // waiting for them and `end` does not kill the process tree on
        // Windows, so a longer ping outlives the test and becomes load for the
        // next one.
        #[cfg(windows)]
        let slowly = "ping -n 6 127.0.0.1 >nul";

        shell_call(json!({ "command": slowly, "session": "one", "wait": 1, "conversation": conversation })).await;
        shell_call(json!({ "command": "echo done", "session": "two", "wait": 10, "conversation": conversation })).await;

        let status = shell_call(json!({ "status": true, "conversation": conversation })).await;
        let terminals = status["terminals"].as_array().unwrap();
        assert_eq!(terminals.len(), 2, "{status:?}");

        let one = terminals
            .iter()
            .find(|t| t["session"] == "one")
            .expect("the slow one");
        assert_eq!(one["running"], true);
        assert!(
            one["running_for_seconds"].as_u64().is_some(),
            "it must say for how long"
        );
        let two = terminals
            .iter()
            .find(|t| t["session"] == "two")
            .expect("the quick one");
        assert_eq!(two["running"], false);
        assert_eq!(two["last_exit_code"], 0);

        // And another conversation's terminals are not in it.
        let elsewhere = shell_call(json!({ "status": true, "conversation": 9503 })).await;
        assert_eq!(
            elsewhere["count"], 0,
            "one conversation saw another's terminals"
        );

        shell_call(json!({ "stop": true, "session": "one", "conversation": conversation })).await;
        shell_call(json!({ "stop": true, "session": "two", "conversation": conversation })).await;
    }
}
