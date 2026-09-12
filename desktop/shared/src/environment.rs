//! What computer this is, told rather than guessed.
//!
//! A model writes `ls -la` because that is the median of everything it has
//! read, and on Windows the terminal here is `cmd.exe`, so it is wrong twice:
//! wrong about the system and wrong about the shell. Nothing in a conversation
//! says otherwise today, and none of it can be deduced server-side: the gateway
//! may be on another continent, and the same person may be signed in from a
//! laptop and a desktop at once.
//!
//! So the application says. It is gathered here, sent with every message beside
//! the device and the time zone, and the gateway decides whether it is worth
//! putting in front of the model (it is not, when no tool can reach this
//! computer anyway).
//!
//! Everything here is READ, never run. Presence of a program is answered by
//! looking along PATH, which is a handful of file lookups; asking each one for
//! its version would be a process per name, each able to hang, at the moment
//! somebody is waiting for an answer.

use serde::Serialize;
use std::path::{Path, PathBuf};

/// What the assistant is told about the computer it can reach.
#[derive(Serialize, Debug, Clone, PartialEq, Eq)]
pub struct Environment {
    /// `windows`, `macos` or `linux`, as the compiler knows it.
    pub os: String,
    pub arch: String,
    /// What this machine calls itself, or empty. For a sentence, not an id.
    pub name: String,
    /// What the terminal tool ACTUALLY starts. Read from the terminal itself
    /// rather than worked out again here, because two answers to "which shell"
    /// is a conversation where the assistant writes for one and the command
    /// runs in the other.
    pub shell: String,
    /// `\` or `/`.
    pub path_separator: String,
    /// Whether two paths differing only in case are two files.
    pub case_sensitive_paths: bool,
    /// `\r\n` or `\n`.
    pub line_ending: String,
    /// The person's home directory, or empty when it cannot be read.
    pub home: String,
    /// Which of the programs below are on PATH. Present, not permitted: what
    /// may actually be RUN is an administrator's policy and is applied by the
    /// gateway, which is the only side that knows it.
    pub has: Vec<String>,
    /// Which of them are NOT. Sent as well as `has`, because the other side
    /// does not hold the list and so cannot tell a program that was looked for
    /// and missing from one that was never looked for at all. Saying "make is
    /// not installed" has to be something this side actually checked.
    pub missing: Vec<String>,
}

/// The programs worth asking about.
///
/// Chosen by one test: does knowing change what the assistant would write?
/// `python3` against `python` does, and so does `rg` against `grep`. A library
/// it would never invoke from a command line does not, and every name costs a
/// line in every system prompt for the rest of the conversation.
/// How long somebody's shell may take to say what its PATH is, in seconds.
const SHELL_PATIENCE: u64 = 3;

const PROBE: &[&str] = &[
    // Shells and runtimes.
    "bash", "zsh", "pwsh", "powershell", "python3", "python", "node", "deno", "bun", "ruby",
    "perl", "php", "java", "dotnet", "go", "cargo",
    // Source control, packaging, building.
    "git", "make", "cmake", "docker", "npm", "pnpm", "yarn", "gradle", "mvn",
    // Finding things, filtering them, moving them.
    "rg", "fd", "grep", "sed", "awk", "jq", "curl", "wget", "tar", "ssh",
];

/// Work out the person's PATH now, so no message ever waits for it.
///
/// Called when the application starts. The answer is remembered, so this is
/// the only time the shell is run, and it happens while somebody is still
/// looking at a window rather than at a cursor that has not answered.
pub fn warm() {
    std::thread::spawn(|| {
        let _ = path_dirs();
    });
}

/// What this computer is, right now.
pub fn describe() -> Environment {
    Environment {
        os: os().to_string(),
        arch: arch().to_string(),
        name: hostname(),
        shell: crate::tools::terminal::shell(),
        path_separator: if cfg!(windows) { "\\" } else { "/" }.to_string(),
        // macOS is the reason this is not simply "not Windows": its default
        // volume preserves case and does not distinguish it, so a file found as
        // README is opened as readme and an assistant told otherwise will
        // "fix" a path that was never broken.
        case_sensitive_paths: !cfg!(any(windows, target_os = "macos")),
        line_ending: if cfg!(windows) { "\r\n" } else { "\n" }.to_string(),
        home: home().map(|p| p.to_string_lossy().into_owned()).unwrap_or_default(),
        has: found_in(PROBE, &path_dirs(), true),
        missing: found_in(PROBE, &path_dirs(), false),
    }
}

/// The operating system, as one word.
pub fn os() -> &'static str {
    std::env::consts::OS
}

/// The processor architecture, as one word.
pub fn arch() -> &'static str {
    std::env::consts::ARCH
}

/// What this machine calls itself, or an honest blank.
///
/// Read from the environment rather than a system call, because the answer is
/// for a sentence in a conversation and not for identifying anything: nothing
/// depends on it being unique or even present.
pub fn hostname() -> String {
    for key in ["HOSTNAME", "COMPUTERNAME", "NAME"] {
        if let Ok(name) = std::env::var(key) {
            let name = name.trim();
            if !name.is_empty() {
                return name.to_string();
            }
        }
    }
    String::new()
}

fn home() -> Option<PathBuf> {
    for key in ["HOME", "USERPROFILE"] {
        if let Some(value) = std::env::var_os(key) {
            let path = PathBuf::from(value);
            if !path.as_os_str().is_empty() {
                return Some(path);
            }
        }
    }
    None
}

/// The directories PATH names, in order.
///
/// The PERSON's PATH, which on a Mac is not this process's. An application
/// opened from the Finder is started by launchd and inherits
/// `/usr/bin:/bin:/usr/sbin:/sbin`, with no `/usr/local/bin` and nothing a
/// package manager added, so a probe run against it reports node, docker,
/// ripgrep and the rest as absent on a machine that has all of them. That is
/// not a gap in the answer, it is a WRONG answer: the assistant was told the
/// front end could not be built for lack of node, and said so, while node sat
/// in /usr/local/bin and the terminal tool could run it, because the terminal
/// goes through the person's shell and this did not.
///
/// So the login shell is asked, once, and the directories are remembered. The
/// files in them are looked at on every call as before, so something installed
/// mid-conversation still appears; it is the LIST OF DIRECTORIES that is
/// settled once, and that is what a login shell is needed for.
///
/// Windows needs none of this: a program started from Explorer inherits the
/// system and user PATH already.
fn path_dirs() -> Vec<PathBuf> {
    let mut dirs: Vec<PathBuf> = std::env::var_os("PATH")
        .map(|raw| std::env::split_paths(&raw).collect())
        .unwrap_or_default();
    for dir in login_path() {
        if !dirs.contains(&dir) {
            dirs.push(dir);
        }
    }
    dirs
}

/// The PATH to give anything we run on this computer, as a single string.
///
/// The reason this is shared rather than local to the probe: what the list says
/// is installed and what the terminal can actually RUN have to be the same
/// thing, and they were not. An application opened from the Finder inherits
/// launchd's PATH, and the terminal spawns the person's shell with `-c`, which
/// does not read the files their PATH is set in. So `node -v` failed in the
/// application while working in their terminal, and the probe, reading the same
/// impoverished PATH, at least said so. Fixing only the probe would have been
/// worse than the bug: a list promising a program the terminal then could not
/// find.
///
/// This is what a tool run from a terminal gets for free, and it is why one
/// works and the other did not.
pub fn run_path() -> Option<std::ffi::OsString> {
    let dirs = path_dirs();
    if dirs.is_empty() {
        return None;
    }
    std::env::join_paths(dirs).ok()
}

/// What the person's own shell would have on PATH, asked once.
///
/// One process, on the first call of the application's life, and never again:
/// the answer is remembered. A shell that cannot be run, or says nothing, costs
/// nothing here, because what this returns is ADDED to the process's own PATH
/// rather than replacing it.
#[cfg(not(target_os = "windows"))]
fn login_path() -> Vec<PathBuf> {
    use std::sync::OnceLock;
    static DIRS: OnceLock<Vec<PathBuf>> = OnceLock::new();
    DIRS.get_or_init(|| {
        let shell = std::env::var("SHELL").unwrap_or_else(|_| "/bin/sh".to_string());
        // A LOGIN shell (-l), which is the one that reads the files a person
        // puts their PATH in. `-c` alone reads a different set, or none.
        //
        // BOUNDED, because somebody else's shell is not ours to trust with the
        // time. A profile that loads a version manager takes seconds, and one
        // that waits on something takes for ever; unbounded, the first message
        // of the session would wait on it. Out of time is answered the same way
        // as no shell at all: this list is ADDED to what the process already
        // has, so the worst case is the behaviour we had before, not a failure.
        let Ok(mut child) = std::process::Command::new(&shell)
            .args(["-lc", "printf %s \"$PATH\""])
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::null())
            .spawn()
        else {
            return Vec::new();
        };
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(SHELL_PATIENCE);
        loop {
            match child.try_wait() {
                Ok(Some(status)) if status.success() => break,
                Ok(Some(_)) | Err(_) => return Vec::new(),
                Ok(None) if std::time::Instant::now() >= deadline => {
                    let _ = child.kill();
                    let _ = child.wait();
                    return Vec::new();
                }
                Ok(None) => std::thread::sleep(std::time::Duration::from_millis(20)),
            }
        }
        let mut raw = String::new();
        if let Some(mut out) = child.stdout.take() {
            use std::io::Read;
            let _ = out.read_to_string(&mut raw);
        }
        std::env::split_paths(raw.trim()).collect()
    })
    .clone()
}

#[cfg(target_os = "windows")]
fn login_path() -> Vec<PathBuf> {
    Vec::new()
}

/// Which of `names` can be found in `dirs`, in the order they were asked for.
///
/// Separated from `describe` so it can be tested against a directory a test
/// makes, rather than against whatever happens to be installed on the machine
/// running the suite, which is an assertion that passes or fails for reasons
/// having nothing to do with this code.
fn found_in(names: &[&str], dirs: &[PathBuf], present: bool) -> Vec<String> {
    names
        .iter()
        .filter(|name| dirs.iter().any(|dir| found(dir, name)) == present)
        .map(|name| (*name).to_string())
        .collect()
}

/// Whether `name` names a program in `dir`.
///
/// On Windows a program is `git.exe` or `git.cmd` rather than `git`, and which
/// suffixes count is PATHEXT's to say, so it is read rather than assumed. The
/// bare name is tried first everywhere, because a file with no extension is
/// still a file.
fn found(dir: &Path, name: &str) -> bool {
    if dir.join(name).is_file() {
        return true;
    }
    if !cfg!(windows) {
        return false;
    }
    let exts = std::env::var("PATHEXT").unwrap_or_else(|_| ".EXE;.CMD;.BAT;.COM".to_string());
    exts.split(';')
        .map(str::trim)
        .filter(|e| !e.is_empty())
        .any(|ext| dir.join(format!("{name}{ext}")).is_file())
}

/// What this computer is, for the page that sends it with the next message.
#[tauri::command]
pub fn machine_environment() -> Environment {
    describe()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A directory of its own, named for this process and this case, because
    /// the suite runs its tests in parallel in one process.
    fn scratch(case: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("sag-env-{}-{case}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("scratch dir");
        dir
    }

    #[test]
    fn finds_only_what_is_there() {
        let dir = scratch("present");
        std::fs::write(dir.join("present"), b"#!/bin/sh\n").expect("write");
        let dirs = vec![dir.clone()];

        // The control this exists for: the same call, over the same directory,
        // must answer differently for a name that is there and one that is not.
        // A probe that always said "found" would satisfy the first assertion on
        // its own and prove nothing.
        assert_eq!(found_in(&["present"], &dirs, true), vec!["present".to_string()]);
        assert!(found_in(&["absent"], &dirs, true).is_empty());
        // And the other side of the same call: what is missing is what was
        // looked for and not found, never what was never asked about.
        assert_eq!(found_in(&["absent"], &dirs, false), vec!["absent".to_string()]);
        assert!(found_in(&["present"], &dirs, false).is_empty());
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn keeps_the_order_it_was_asked_in() {
        let dir = scratch("order");
        for name in ["b", "a"] {
            std::fs::write(dir.join(name), b"x").expect("write");
        }
        let dirs = vec![dir.clone()];
        // The list is read in a prompt, so it is the caller's order and not the
        // filesystem's, which is arbitrary and differs between machines.
        assert_eq!(found_in(&["b", "a"], &dirs, true), vec!["b".to_string(), "a".to_string()]);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_directory_is_not_a_program() {
        let dir = scratch("dir");
        std::fs::create_dir(dir.join("node")).expect("mkdir");
        let dirs = vec![dir.clone()];
        // A folder somebody named after a tool must not read as the tool being
        // installed, which is what a bare `exists()` would have said.
        assert!(found_in(&["node"], &dirs, true).is_empty());
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// What the list promises and what the terminal is given must be ONE
    /// thing.
    ///
    /// This is the bug the whole module was rewritten for: the probe read this
    /// process's PATH, the terminal spawned a shell that read its own, and they
    /// disagreed, so a program could be reported installed and then not run, or
    /// reported missing while sitting in the person's own PATH. Asserted as an
    /// identity rather than as two lists that happen to match today.
    #[test]
    fn what_is_reported_is_what_a_command_will_be_given() {
        let reported = path_dirs();
        let given: Vec<PathBuf> = run_path()
            .map(|p| std::env::split_paths(&p).collect())
            .unwrap_or_default();
        assert_eq!(reported, given, "the probe and the terminal were handed different PATHs");
        assert!(!given.is_empty(), "a command would be run with no PATH at all");
    }

    #[test]
    fn describes_this_machine_consistently_with_its_own_tools() {
        let env = describe();
        assert_eq!(env.os, std::env::consts::OS);
        assert_eq!(env.arch, std::env::consts::ARCH);
        // The whole reason the shell is read from the terminal rather than
        // decided again here: an assistant told one shell while commands run in
        // another is the bug this module exists to prevent.
        assert_eq!(env.shell, crate::tools::terminal::shell());
        assert_eq!(env.path_separator, if cfg!(windows) { "\\" } else { "/" });
        assert_eq!(env.line_ending, if cfg!(windows) { "\r\n" } else { "\n" });
    }
}
