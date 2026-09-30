//! A skill on this computer, and one of its scripts running.
//!
//! Two calls, and neither is an ability the assistant is offered. The gateway
//! makes them on its own: the model asks to run a script, and whether the
//! package had to be sent down first is not its business.
//!
//! WHAT IS KEPT, AND WHERE. `<state>/.sag/skill/<handle>/<version_id>/`, laid
//! out exactly as the package was written, because a script opens its siblings
//! by relative path and expects to find them.
//!
//! WHY THE VERSION IS THE KEY. A version of a skill is immutable on the other
//! side: an edited package is a new version with a new id. So a directory named
//! after one is either complete and current or absent, and "is what you have
//! still right?" is a question that cannot be answered wrongly. There is no
//! freshness check here and no hash comparison against what is stored, because
//! there is no staleness to detect.
//!
//! WHY A HASH IS STILL CHECKED. Not for freshness, for arrival. Every file is
//! written to a temporary directory, hashed, and only then is the whole tree
//! renamed into place. A half-written package is never runnable and a truncated
//! script never runs as a shorter script.
//!
//! WHO KEEPS TWO INSTALLS APART. Not this half. The staging directory is named
//! for the version alone, so two installs of one version running at once would
//! tread on each other. The gateway holds one install per (device, version)
//! and makes the others wait for it (orchestrator/internal/tools/skills), which
//! is where it belongs: it is the side that knows a fleet of agents is reaching
//! for one skill, and the side that can tell an agent to try again in a moment.

use std::collections::BTreeMap;
use std::path::{Component, Path, PathBuf};
use std::process::Stdio;

use base64::Engine as _;
use serde::Deserialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use tokio::io::AsyncReadExt;

use super::Response;

/// install is the gateway sending a version of a skill down.
pub mod install {
    pub const NAME: &str = "skill_install";
    pub const VERSION: i64 = 1;
}

/// run is one of that version's scripts.
pub mod run {
    pub const NAME: &str = "skill_run";
    pub const VERSION: i64 = 1;
}

/// The kind the gateway reads to mean "send it to me": a KIND and not words, so
/// rewording this message cannot change what the other half does.
const MISSING: &str = "skill_missing";

/// How long a script may run before it is ended, and how much it may take while
/// it does.
///
/// Ceilings rather than expectations. A script that does its job in a second is
/// unaffected by any of them; a script with a runaway loop, a leak, or a wait on
/// something that never comes is ended rather than left holding somebody's
/// laptop. The processor ceiling is deliberately far above the wall clock: it
/// bounds a spinning loop without punishing a script that is merely waiting for
/// a network.
static DEADLINE_SECONDS: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(300);

/// How long a script may run. A value rather than a constant, so the test that
/// proves the ceiling exists can set it to seconds: five minutes is right for
/// the product and would make that test one nobody ever runs.
fn deadline() -> std::time::Duration {
    std::time::Duration::from_secs(DEADLINE_SECONDS.load(std::sync::atomic::Ordering::Relaxed))
}
/// Held wherever the platform will take it: an address-space limit on Linux, a
/// per-process commit limit inside the job on Windows, and NOWHERE on macOS,
/// which refuses RLIMIT_AS outright.
const MAX_MEMORY: u64 = 2 * 1024 * 1024 * 1024; // 2 GiB of address space, where that is a thing

static MAX_CPU_SECONDS: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(600);

/// How much processor time a script may spend. A value rather than a constant
/// for the reason the deadline is one: the test that proves the ceiling exists
/// has to spend it.
fn max_cpu_seconds() -> u64 {
    MAX_CPU_SECONDS.load(std::sync::atomic::Ordering::Relaxed)
}
/// How large a file one script may write.
///
/// UNIX ONLY, and that is the honest shape rather than an oversight. A Windows
/// job object holds a processor ceiling and a memory ceiling but has no
/// equivalent of RLIMIT_FSIZE, so there is nothing for this to ask for there.
/// It is gated rather than left dangling because a value nothing can read is
/// dead code, and dead code that looks like a guard is worse than an absent
/// one: it reads as a ceiling somebody has in place.
#[cfg(unix)]
static MAX_FILE_SIZE: std::sync::atomic::AtomicU64 =
    std::sync::atomic::AtomicU64::new(512 * 1024 * 1024);

/// A value rather than a constant for the reason the other two are: the test
/// that proves the ceiling exists has to cross it, and crossing half a gigabyte
/// to prove a point is a test nobody runs.
#[cfg(unix)]
fn max_file_size() -> u64 {
    MAX_FILE_SIZE.load(std::sync::atomic::Ordering::Relaxed)
}
/// How much of what a script printed comes back. The same ceiling the terminal
/// uses, and for the same reason: what arrives goes into a model's context.
const MAX_OUTPUT: usize = 96 * 1024;

/// Start a console program without giving it a console (Windows).
///
/// CREATE_NO_WINDOW, and not `HideWindow`: that one still allocates the console
/// and merely asks for it not to be shown, which flashes on the way past. The
/// terminal and the gateway each reached this conclusion separately, and this
/// is the third place that needs it: a skill's script is a console program too.
#[cfg(windows)]
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

#[derive(Debug, Deserialize)]
struct InstallRequest {
    skill: String,
    version: i64,
    #[serde(default)]
    files: Vec<Carried>,
}

/// One file of a package, as it crosses the link. Text or bytes, never both:
/// raw bytes cannot travel in JSON, so an asset arrives encoded.
#[derive(Debug, Deserialize)]
struct Carried {
    path: String,
    #[serde(default)]
    text: Option<String>,
    #[serde(default)]
    base64: Option<String>,
    #[serde(default)]
    sha256: String,
}

#[derive(Debug, Deserialize)]
struct RunRequest {
    skill: String,
    version: i64,
    script: String,
    // Absent and null both mean no arguments. `#[serde(default)]` alone covers
    // only the absent one: a field that IS there holding null is still read as
    // a Vec and refused, which is what an older gateway sends for a script
    // that takes no arguments.
    #[serde(default)]
    args: Option<Vec<String>>,
}

/// where a version of a skill lives on this computer.
fn home(skill: &str, version: i64) -> Result<PathBuf, String> {
    // The handle is checked rather than trusted. The format allows lowercase
    // letters, digits and hyphens and nothing else, so anything with a
    // separator or a dot in it is not a handle, whatever sent it: a name that
    // arrived over a link never becomes a path component on faith.
    if skill.is_empty()
        || skill.len() > 64
        || !skill
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
    {
        return Err(format!("{skill:?} is not a skill handle"));
    }
    if version <= 0 {
        return Err(format!("{version} is not a version"));
    }
    let state = crate::workspace::state_dir()
        .ok_or_else(|| "this installation has no state directory".to_string())?;
    Ok(state
        .join(".sag")
        .join("skill")
        .join(skill)
        .join(version.to_string()))
}

/// inside resolves one of a package's own paths under its directory.
///
/// The path came over a link and is treated as data. Only plain names are
/// allowed through: no root, no `..`, no Windows prefix or drive. A package is
/// checked this way when it is imported on the other side too, and this is the
/// half that is holding a filesystem, so it checks again rather than trusting
/// that it was.
fn inside(root: &Path, path: &str) -> Result<PathBuf, String> {
    if path.is_empty() {
        return Err("a file with no name".to_string());
    }
    let mut resolved = root.to_path_buf();
    for part in Path::new(path).components() {
        match part {
            Component::Normal(name) => resolved.push(name),
            _ => return Err(format!("{path:?} is not a path inside the package")),
        }
    }
    Ok(resolved)
}

/// run_install writes a version of a skill, whole or not at all.
pub async fn run_install(args: Value) -> Response {
    let request: InstallRequest = match serde_json::from_value(args) {
        Ok(request) => request,
        Err(err) => return Response::bad_arguments(format!("that is not an install: {err}")),
    };
    if request.files.is_empty() {
        return Response::bad_arguments("an install with no files in it");
    }
    let home = match home(&request.skill, request.version) {
        Ok(home) => home,
        Err(why) => return Response::bad_arguments(why),
    };
    // Already here. Answered rather than rewritten: the id names an immutable
    // version, so what is on disk under it is what was asked for, and an
    // install that arrives for a version already present costs one look at a
    // directory.
    if home.is_dir() {
        return Response::ok(json!({"installed": true, "already": true}));
    }

    // Beside the final directory rather than in the system's temporary space,
    // so the rename that follows is on one filesystem and is therefore atomic.
    // A rename across devices is a copy, and a copy can be interrupted halfway,
    // which is the one thing this arrangement exists to prevent.
    let parent = match home.parent() {
        Some(parent) => parent.to_path_buf(),
        None => return Response::failed("the skill has nowhere to live"),
    };
    if let Err(err) = tokio::fs::create_dir_all(&parent).await {
        return Response::failed(format!("the skill's folder could not be made: {err}"));
    }
    let staging = parent.join(format!(".{}.part", request.version));
    // Anything left by a previous attempt that died mid-write. The gateway
    // holds one install per (device, version), so nothing else is writing it
    // (internal/tools/skills).
    let _ = tokio::fs::remove_dir_all(&staging).await;

    for file in &request.files {
        let target = match inside(&staging, &file.path) {
            Ok(target) => target,
            Err(why) => {
                let _ = tokio::fs::remove_dir_all(&staging).await;
                return Response::bad_arguments(why);
            }
        };
        let body: Vec<u8> = match (&file.text, &file.base64) {
            (Some(text), _) => text.as_bytes().to_vec(),
            (None, Some(encoded)) => {
                match base64::engine::general_purpose::STANDARD.decode(encoded) {
                    Ok(raw) => raw,
                    Err(err) => {
                        let _ = tokio::fs::remove_dir_all(&staging).await;
                        return Response::bad_arguments(format!(
                            "{} did not arrive as readable data: {err}",
                            file.path
                        ));
                    }
                }
            }
            (None, None) => Vec::new(),
        };
        // The hash is of what ARRIVED, checked before anything is runnable. A
        // file that was cut short on the way is refused here rather than run as
        // a shorter script.
        if !file.sha256.is_empty() {
            let got = format!("{:x}", Sha256::digest(&body));
            if !got.eq_ignore_ascii_case(&file.sha256) {
                let _ = tokio::fs::remove_dir_all(&staging).await;
                return Response::failed(format!(
                    "{} did not arrive intact and was not kept",
                    file.path
                ));
            }
        }
        if let Some(folder) = target.parent() {
            if let Err(err) = tokio::fs::create_dir_all(folder).await {
                let _ = tokio::fs::remove_dir_all(&staging).await;
                return Response::failed(format!("{} could not be made: {err}", file.path));
            }
        }
        if let Err(err) = tokio::fs::write(&target, &body).await {
            let _ = tokio::fs::remove_dir_all(&staging).await;
            return Response::failed(format!("{} could not be written: {err}", file.path));
        }
    }

    // The one step that makes the package visible, and it is a rename: until it
    // happens there is nothing here to run, and after it there is all of it.
    match tokio::fs::rename(&staging, &home).await {
        Ok(()) => Response::ok(json!({"installed": true, "files": request.files.len()})),
        Err(_) if home.is_dir() => {
            // Another call installed the same version while this one was
            // writing. Theirs is as good as ours, by definition: same id, same
            // immutable version.
            let _ = tokio::fs::remove_dir_all(&staging).await;
            Response::ok(json!({"installed": true, "already": true}))
        }
        Err(err) => {
            let _ = tokio::fs::remove_dir_all(&staging).await;
            Response::failed(format!("the skill could not be put in place: {err}"))
        }
    }
}

/// run_script runs one script of a version, if that version is here.
pub async fn run_script(args: Value) -> Response {
    let request: RunRequest = match serde_json::from_value(args) {
        Ok(request) => request,
        Err(err) => return Response::bad_arguments(format!("that is not a script to run: {err}")),
    };
    let home = match home(&request.skill, request.version) {
        Ok(home) => home,
        Err(why) => return Response::bad_arguments(why),
    };
    if !home.is_dir() {
        // NOT a failure. Nothing is wrong and nothing ran: this computer has
        // never been sent this version. The gateway reads the kind, sends the
        // package, and asks again.
        return Response {
            ok: false,
            content: Some(json!({"skill_dir": home})),
            kind: MISSING,
            message: format!(
                "this computer does not have version {} of {}",
                request.version, request.skill
            ),
        };
    }
    let script = match inside(&home, &request.script) {
        Ok(script) => script,
        Err(why) => return Response::bad_arguments(why),
    };
    if !script.is_file() {
        // The gateway checks the path against the package before asking, so
        // this is a file that was in the version and is not on the disk: the
        // install was interrupted, or somebody emptied the folder by hand.
        return Response::failed(format!(
            "{} is not in the copy of {} on this computer",
            request.script, request.skill
        ));
    }

    let Some(runtime) = Runtime::of(&script) else {
        return Response::bad_arguments(format!(
            "{} is not a kind of script this computer knows how to run",
            request.script
        ));
    };
    let Some(program) = runtime.program().await else {
        // Said as what it is: something to install, not a tool that broke.
        return Response::failed(format!(
            "{} needs {} and it is not installed on this computer",
            request.script,
            runtime.needs()
        ));
    };

    // WHERE it runs is the folder this person chose, not the skill's own
    // directory. A script is here to work on their files; the package is
    // library code. What lets it find its siblings is the two variables below,
    // which is how both languages resolve an import that is not relative.
    let working = crate::workspace::folder().unwrap_or_else(|| home.clone());

    let mut command = tokio::process::Command::new(&program);
    command
        // ARGV, never a shell. The arguments came from a model, and a shell
        // would expand, split and interpret them; this passes each one through
        // untouched, so there is nothing to quote and nothing to escape. Even a
        // shell script goes this way: the script is a FILE argument to bash
        // rather than a string after -c, so there is nothing to interpolate.
        .arg(&script)
        .args(request.args.clone().unwrap_or_default())
        .current_dir(&working)
        // PYTHONPATH is the directory itself, because that is where Python
        // looks for a module: `<home>/helper.py` is `import helper`.
        .env("PYTHONPATH", &home)
        // NODE_PATH is the directory whose CHILDREN are modules, which is
        // node_modules and not the package root. Measured, because the obvious
        // guess is wrong and fails silently: with NODE_PATH set to the package
        // root, `require("csv-parse")` resolves nothing at all.
        //
        // It is belt to a brace that usually holds on its own: a script at
        // `<home>/scripts/x.js` already walks up and finds `<home>/node_modules`
        // by ordinary resolution. This is for a script run from somewhere else,
        // and it is why `npm install --prefix "$SAG_SKILL_DIR"` is the
        // instruction a skill should document.
        .env("NODE_PATH", home.join("node_modules"))
        // What the skill's own directory is, for a script that would rather ask
        // than guess where its data files are.
        .env("SAG_SKILL_DIR", &home)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    // The person's PATH, not this process's: an application opened from the
    // Finder is started with a PATH that has none of what they installed
    // themselves (terminal.rs says the same, having been caught by it).
    if let Some(path) = crate::environment::run_path() {
        command.env("PATH", path);
    }
    // Its own process group, so ending it ends everything it started. A script
    // that spawns a compiler and is killed alone leaves the compiler behind.
    #[cfg(unix)]
    command.process_group(0);
    #[cfg(unix)]
    unsafe {
        // Between fork and exec, in the child. The ceilings are set on the
        // child so nothing here is bounded by them, and they are set as HARD
        // limits so the script cannot raise its own.
        command.pre_exec(|| {
            bound();
            Ok(())
        });
    }
    // No console window, which is the Windows half of a different problem. This
    // is a child of an application somebody launched from an icon, and a black
    // window flashing past on every script is what people report as a virus;
    // terminal.rs was caught by exactly this and says so where it spawns. The
    // ceilings and the process tree cannot be asked for until there is a
    // process to put in them, so those come after the spawn.
    #[cfg(windows)]
    command.creation_flags(CREATE_NO_WINDOW);

    let mut child = match command.spawn() {
        Ok(child) => child,
        Err(err) => {
            return Response::failed(format!("{} could not be started: {err}", request.script))
        }
    };
    // Held for as long as the script runs. Dropping it at the end of this
    // function is what takes any straggler with it, KILL_ON_JOB_CLOSE being
    // what makes the handle worth keeping.
    //
    // `bounded` rather than `hold`: unlike the terminal, a script is held to
    // the ceilings this file declares.
    #[cfg(windows)]
    let job = super::windows_job::Job::bounded(
        &child,
        super::windows_job::Ceilings {
            cpu_seconds: max_cpu_seconds(),
            memory: MAX_MEMORY,
        },
    );

    let mut out = Vec::new();
    let mut err = Vec::new();
    let mut stdout = child.stdout.take();
    let mut stderr = child.stderr.take();
    let reading = async {
        let child = &mut child;
        // Both channels, to their own buffers: a script's answer is usually on
        // stdout and its complaint on stderr, and a caller that has to guess
        // which of the two it is reading has been given one string.
        let a = async {
            if let Some(pipe) = stdout.as_mut() {
                let _ = pipe.read_to_end(&mut out).await;
            }
        };
        let b = async {
            if let Some(pipe) = stderr.as_mut() {
                let _ = pipe.read_to_end(&mut err).await;
            }
        };
        tokio::join!(a, b);
        child.wait().await
    };

    let finished = tokio::time::timeout(deadline(), reading).await;
    let status = match finished {
        Ok(Ok(status)) => status,
        Ok(Err(err)) => return Response::failed(format!("{} ended badly: {err}", request.script)),
        Err(_) => {
            // The deadline. The GROUP is ended, not the process, for the reason
            // it was given one. On Windows the job is the same idea and holds
            // everything the script started, so it goes first and by itself.
            #[cfg(windows)]
            if let Some(job) = &job {
                job.end();
            }
            end(&mut child).await;
            return Response::failed(format!(
                "{} was still running after {} seconds and was stopped. {}",
                request.script,
                deadline().as_secs(),
                said(&trimmed(&out), &trimmed(&err))
            ));
        }
    };

    let printed = trimmed(&out);
    let complained = trimmed(&err);
    let code = status.code().unwrap_or(-1);
    // A script that exits non-zero RAN, and what it said is the answer. The
    // gateway hands both back either way; what the exit code decides is
    // whether the assistant reads this as done or as something to correct.
    // The package's own directory rides on the answer, always.
    //
    // It is here because of what happened without it. A script failed on a
    // missing library, the skill's instructions said to install it with
    // `--prefix "$SAG_SKILL_DIR"`, and that variable is set for a SCRIPT and
    // not for the terminal the assistant then used. So it went looking: `find /
    // -type d -name csv-reports`, sixty seconds of it, before hard-coding the
    // path it found. Saying where the package is costs one field and removes
    // the whole search.
    if status.success() {
        return Response::ok(json!({
            "output": printed,
            "stderr": complained,
            "exit_code": code,
            "skill_dir": home,
        }));
    }
    Response {
        ok: false,
        content: Some(json!({"skill_dir": home})),
        kind: "failed",
        message: format!(
            "{} exited {}. {} (this skill's files are in {})",
            request.script,
            code,
            said(&printed, &complained),
            home.to_string_lossy()
        ),
    }
}

/// said is what a script left behind, for a message a person or a model reads.
fn said(printed: &str, complained: &str) -> String {
    match (printed.trim().is_empty(), complained.trim().is_empty()) {
        (true, true) => "It printed nothing.".to_string(),
        (true, false) => complained.to_string(),
        (false, true) => printed.to_string(),
        (false, false) => format!("{complained}\n{printed}"),
    }
}

/// trimmed is what was printed, cut to what one answer carries.
///
/// The END is kept, which is the terminal's rule and the right one here: a
/// script that printed a great deal says what went wrong last, and a traceback
/// is at the bottom.
fn trimmed(raw: &[u8]) -> String {
    let text = String::from_utf8_lossy(raw);
    if text.len() <= MAX_OUTPUT {
        return text.into_owned();
    }
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

/// end stops a script and everything it started.
///
/// The same two steps the terminal takes, in the same order and for the same
/// reason: the GROUP first, which is what control-C does and what reaches a
/// compiler the script spawned, and then the child's own kill as the backstop
/// for anything that missed.
async fn end(child: &mut tokio::process::Child) {
    #[cfg(unix)]
    if let Some(id) = child.id() {
        let _ = nix::sys::signal::killpg(
            nix::unistd::Pid::from_raw(id as i32),
            nix::sys::signal::Signal::SIGKILL,
        );
    }
    let _ = child.kill().await;
}

/// bound sets this child's ceilings. It runs in the child, between fork and
/// exec, where nothing it does can affect the application.
///
/// WHICH OF THESE ACTUALLY BITE IS A PLATFORM QUESTION, AND IT WAS MEASURED
/// rather than assumed, because two of them were claimed in writing before
/// anybody checked. On macOS 14:
///
///   - RLIMIT_CPU holds. A spinning script is killed at the ceiling.
///   - RLIMIT_FSIZE holds. A script writing past it gets "File too large".
///   - RLIMIT_AS is REFUSED OUTRIGHT: setrlimit answers EINVAL, because Darwin
///     does not implement an address-space limit. RLIMIT_DATA answers EINVAL
///     too. So there is NO memory ceiling on macOS, whatever this asks for.
///
/// It is still asked for, because it costs nothing and Linux does implement it.
/// What must not happen is somebody reading the ignored error below as a bug and
/// "fixing" it into a refusal to run scripts on macOS at all.
///
/// The failures are ignored on purpose: a ceiling the kernel will not set is not
/// a reason to refuse to run the script, and the WALL CLOCK is the backstop that
/// depends on nothing the kernel agrees to.
#[cfg(unix)]
fn bound() {
    use nix::sys::resource::{setrlimit, Resource};
    let _ = setrlimit(Resource::RLIMIT_AS, MAX_MEMORY, MAX_MEMORY);
    let cpu = max_cpu_seconds();
    let _ = setrlimit(Resource::RLIMIT_CPU, cpu, cpu);
    let fsize = max_file_size();
    let _ = setrlimit(Resource::RLIMIT_FSIZE, fsize, fsize);
}

/// Runtime is which of the four a file is run by, decided by its EXTENSION.
///
/// Not by its shebang. A shebang is a claim about a path on the machine the
/// author was using, Windows has no notion of one, and a file unpacked from a
/// package may not even be executable. One rule, the same on every platform.
#[derive(Debug, Clone, Copy, PartialEq)]
enum Runtime {
    Python,
    Node,
    Bash,
    PowerShell,
}

impl Runtime {
    fn of(script: &Path) -> Option<Self> {
        let extension = script.extension()?.to_str()?.to_ascii_lowercase();
        match extension.as_str() {
            "py" => Some(Self::Python),
            "js" | "mjs" | "cjs" => Some(Self::Node),
            "sh" => Some(Self::Bash),
            "ps1" => Some(Self::PowerShell),
            _ => None,
        }
    }

    /// What a person would have to install, for a message that says so.
    fn needs(self) -> &'static str {
        match self {
            Self::Python => "Python 3",
            Self::Node => "Node.js",
            Self::Bash => "bash",
            Self::PowerShell => "PowerShell 7",
        }
    }

    /// The names to look for, in the order they are preferred. Windows calls
    /// Python two other things, and `py -3` is the launcher rather than a
    /// program, so it is not here: what is here is what can be exec'd directly.
    fn candidates(self) -> &'static [&'static str] {
        match self {
            Self::Python => &["python3", "python"],
            Self::Node => &["node"],
            Self::Bash => &["bash"],
            Self::PowerShell => &["pwsh"],
        }
    }

    /// program finds the runtime on this computer, or nothing.
    async fn program(self) -> Option<String> {
        for name in self.candidates() {
            if found(name).await {
                return Some((*name).to_string());
            }
        }
        None
    }
}

/// found asks whether a program is on the person's PATH.
///
/// By running it. Reading PATH and looking for a file means reimplementing what
/// the loader does (extensions on Windows, symlinks, a shim that is a directory)
/// and being wrong about it somewhere; asking the program what it is costs a few
/// milliseconds once and is the truth.
async fn found(name: &str) -> bool {
    let mut probe = tokio::process::Command::new(name);
    probe
        .arg("--version")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    if let Some(path) = crate::environment::run_path() {
        probe.env("PATH", path);
    }
    match tokio::time::timeout(std::time::Duration::from_secs(10), probe.status()).await {
        Ok(Ok(status)) => status.success(),
        _ => false,
    }
}

/// Which runtimes this computer has and which version each is, for the
/// machine's own description of itself (tools::info).
///
/// The VERSION matters as much as the presence. A skill whose scripts need
/// Python 3.10 cannot run on 3.9, and without this the assistant finds that out
/// by running one and reading a syntax error. Absent is reported as absent
/// rather than left out, because "we looked and it is not there" and "nobody
/// looked" are different answers.
///
/// Nothing depends on this to RUN a script: that is decided when one is run, by
/// asking. So a probe that fails costs a field and not a refusal.
pub async fn runtimes() -> BTreeMap<String, Value> {
    let mut held = BTreeMap::new();
    for runtime in [
        Runtime::Python,
        Runtime::Node,
        Runtime::Bash,
        Runtime::PowerShell,
    ] {
        held.insert(
            runtime.needs().to_string(),
            match runtime.program().await {
                Some(program) => json!({"installed": true, "version": version_of(&program).await}),
                None => json!({"installed": false}),
            },
        );
    }
    held
}

/// version_of asks a program what it is. One line of whatever it prints, since
/// each of the four says it differently and none of them says it wrongly.
async fn version_of(program: &str) -> Option<String> {
    let mut probe = tokio::process::Command::new(program);
    probe
        .arg("--version")
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    if let Some(path) = crate::environment::run_path() {
        probe.env("PATH", path);
    }
    let out = tokio::time::timeout(std::time::Duration::from_secs(10), probe.output())
        .await
        .ok()?
        .ok()?;
    // Some of them answer on stderr, which is not an error here.
    let said = if out.stdout.is_empty() {
        out.stderr
    } else {
        out.stdout
    };
    String::from_utf8_lossy(&said)
        .lines()
        .next()
        .map(|line| line.trim().to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    /// The bytes of a file, hashed the way the gateway hashes them.
    fn hash(body: &str) -> String {
        format!("{:x}", Sha256::digest(body.as_bytes()))
    }

    fn a_package(version: i64) -> Value {
        json!({
            "skill": "pdf-processing",
            "version": version,
            "files": [
                {"path": "SKILL.md", "text": "# PDF\n", "sha256": hash("# PDF\n")},
                {"path": "scripts/hello.py", "text": "print('hi')\n", "sha256": hash("print('hi')\n")},
                {"path": "scripts/lib/helper.py", "text": "X = 1\n", "sha256": hash("X = 1\n")},
            ]
        })
    }

    /// A version is installed once and is then simply there. The id is the key,
    /// and it names something immutable, so there is nothing to check and no
    /// way to be stale.
    #[tokio::test]
    async fn a_version_is_installed_and_then_it_is_here() {
        let root = super::super::test_workspace();
        let _ = root;

        // Before: not here, and saying so as a KIND rather than as a failure.
        let missing = run_script(json!({
            "skill": "pdf-processing", "version": 7, "script": "scripts/hello.py"
        }))
        .await;
        assert!(!missing.ok);
        assert_eq!(
            missing.kind, MISSING,
            "the gateway reads the kind, not the words"
        );

        let put = run_install(a_package(7)).await;
        assert!(put.ok, "the install failed: {}", put.message);

        // The tree is laid out as the package was written, because a script
        // opens its siblings by relative path.
        let home = home("pdf-processing", 7).expect("a home");
        for path in ["SKILL.md", "scripts/hello.py", "scripts/lib/helper.py"] {
            assert!(home.join(path).is_file(), "{path} is not on the disk");
        }

        // And asking again installs nothing: it answers that it is already here.
        let again = run_install(a_package(7)).await;
        assert!(again.ok);
        let content = again.content.expect("an answer");
        assert_eq!(content["already"], json!(true));
    }

    /// A different version is a different directory, so an updated package
    /// cannot be confused with the one it replaced.
    #[tokio::test]
    async fn a_new_version_is_a_new_directory() {
        let root = super::super::test_workspace();
        let _ = root;
        assert!(run_install(a_package(11)).await.ok);

        let mut changed = a_package(12);
        changed["files"][1] = json!({
            "path": "scripts/hello.py",
            "text": "print('changed')\n",
            "sha256": hash("print('changed')\n"),
        });
        assert!(run_install(changed).await.ok);

        let eleven = home("pdf-processing", 11).expect("a home");
        let twelve = home("pdf-processing", 12).expect("a home");
        assert_eq!(
            std::fs::read_to_string(eleven.join("scripts/hello.py")).unwrap(),
            "print('hi')\n",
            "the old version was overwritten"
        );
        assert_eq!(
            std::fs::read_to_string(twelve.join("scripts/hello.py")).unwrap(),
            "print('changed')\n"
        );
    }

    /// A file that did not arrive intact is not kept, and NOTHING of the package
    /// is: the whole tree is visible only once every hash has been checked, so a
    /// truncated script can never run as a shorter script.
    #[tokio::test]
    async fn a_package_that_did_not_arrive_intact_is_not_kept() {
        let root = super::super::test_workspace();
        let _ = root;
        let mut wrong = a_package(21);
        wrong["files"][1]["sha256"] = json!(hash("something else entirely"));

        let put = run_install(wrong).await;
        assert!(!put.ok, "a package with a bad hash was accepted");
        assert!(
            put.message.contains("intact"),
            "the reason is {}",
            put.message
        );

        let home = home("pdf-processing", 21).expect("a home");
        assert!(!home.exists(), "a half-written package is on the disk");
        // Including the file that DID arrive intact before the bad one.
        assert!(!home.join("SKILL.md").exists());
    }

    /// A version is INVISIBLE until it is complete, which is what the staging
    /// directory and the rename are for.
    ///
    /// The half-written state has a signature worth knowing: `run_script` finds
    /// the directory, looks for the script inside it, and says it "is not in the
    /// copy on this computer". If a package were written straight into its final
    /// place, that is what a script run during an install would get.
    ///
    /// This test exists because the obvious one does not prove it: asserting
    /// that nothing is left behind after a failure passes just as well when the
    /// files are written into the final directory and deleted again. Only
    /// looking WHILE the install runs can tell the two apart.
    #[tokio::test]
    async fn a_version_is_invisible_until_it_is_complete() {
        let root = super::super::test_workspace();
        let _ = root;

        // Enough files that writing them takes long enough to look at, with
        // the script LAST so a reader that found the directory early would be
        // looking for something not written yet.
        let mut files = Vec::new();
        for i in 0..900 {
            let body = format!("# filler {i}\n");
            files.push(json!({
                "path": format!("references/doc{i}.md"),
                "text": body, "sha256": hash(&body),
            }));
        }
        let script = "print('done')\n";
        files.push(json!({
            "path": "scripts/last.py", "text": script, "sha256": hash(script),
        }));

        let installing = tokio::spawn(run_install(json!({
            "skill": "pdf-processing", "version": 81, "files": files
        })));

        // Look repeatedly while it writes. Every answer must be one of the two
        // honest ones: not here yet, or here and ran. Never a directory that
        // exists without the script in it.
        let mut looked = 0;
        loop {
            let seen = run_script(json!({
                "skill": "pdf-processing", "version": 81, "script": "scripts/last.py"
            }))
            .await;
            looked += 1;
            assert!(
                seen.kind == MISSING || seen.ok || seen.message.contains("needs Python 3"),
                "a half-written package was visible on look {looked}: {} / {}",
                seen.kind,
                seen.message
            );
            if installing.is_finished() {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_micros(200)).await;
        }
        let put = installing.await.expect("the install panicked");
        assert!(put.ok, "the install failed: {}", put.message);
        assert!(
            looked > 1,
            "the install finished before anything could look at it"
        );
    }

    /// No arguments is not a broken call, whether the field is missing or null.
    ///
    /// `args` is optional on the other side, so a model running a script that
    /// takes none leaves it out, and the gateway used to put a null there. This
    /// half read that as a malformed request and refused it, so every script
    /// with no arguments failed with "that is not a script to run". The gateway
    /// no longer sends null, and this half no longer minds if it does, because
    /// an application is paired with whatever gateway it is pointed at.
    #[tokio::test]
    async fn a_script_with_no_arguments_runs() {
        let root = super::super::test_workspace();
        let _ = root;
        // A version of its own. The id is the cache key, so two tests sharing
        // one number share a directory, and whichever installs first decides
        // what the other one finds there.
        assert!(run_install(a_package(43)).await.ok);

        for args in [json!(null), json!([])] {
            let ran = run_script(json!({
                "skill": "pdf-processing", "version": 43,
                "script": "scripts/hello.py", "args": args,
            }))
            .await;
            assert!(
                ran.ok || ran.message.contains("needs Python 3"),
                "args={args}: {}",
                ran.message
            );
        }
        // And with the field left out altogether.
        let ran = run_script(json!({
            "skill": "pdf-processing", "version": 43, "script": "scripts/hello.py"
        }))
        .await;
        assert!(
            ran.ok || ran.message.contains("needs Python 3"),
            "no args at all: {}",
            ran.message
        );
    }

    /// Every file of a large package ends up with its OWN content.
    ///
    /// Thin on purpose, because the neighbours hold the rest: integrity on
    /// arrival is `a_package_that_did_not_arrive_intact_is_not_kept`, the
    /// atomic publish is `a_version_is_invisible_until_it_is_complete` (and the
    /// leftovers check that looks like it would prove that does not: writing
    /// straight into the final directory leaves nothing behind either), and a
    /// second install of a version already there is the last assertion of
    /// `a_version_is_installed_and_then_it_is_here`.
    ///
    /// What is left over is worth one test: a hash is checked against the
    /// buffer in memory, not against the file, so nothing else here would
    /// notice a package whose bodies each reached the wrong path.
    #[tokio::test]
    async fn every_file_arrives_with_its_own_content() {
        let root = super::super::test_workspace();
        let _ = root;

        let mut files = Vec::new();
        for i in 0..400 {
            let body = format!("# part {i}\n");
            files.push(json!({
                "path": format!("references/p{i}.md"), "text": body, "sha256": hash(&body),
            }));
        }
        let put = run_install(json!({
            "skill": "pdf-processing", "version": 99, "files": files
        }))
        .await;
        assert!(put.ok, "the install failed: {}", put.message);

        let home = home("pdf-processing", 99).expect("a home");
        for i in 0..400 {
            let path = home.join(format!("references/p{i}.md"));
            assert_eq!(
                std::fs::read_to_string(&path).unwrap_or_default(),
                format!("# part {i}\n"),
                "{path:?} does not hold what was sent for it"
            );
        }
    }

    /// A path that is not inside the package never becomes one on this disk.
    /// The other half checks this too; this half is the one holding a
    /// filesystem, so it checks again rather than trusting that it was.
    #[tokio::test]
    async fn a_path_that_climbs_out_is_refused() {
        let root = super::super::test_workspace();
        let _ = root;
        for path in ["../escaped.py", "scripts/../../escaped.py", "/etc/passwd"] {
            let mut bad = a_package(31);
            bad["files"] = json!([{"path": path, "text": "x", "sha256": hash("x")}]);
            let put = run_install(bad).await;
            assert!(!put.ok, "{path} was written");
            let home = home("pdf-processing", 31).expect("a home");
            assert!(!home.exists(), "{path} left a package behind");
        }
    }

    /// And neither does a handle. It reaches a path as a directory name, so it
    /// is checked against what the format allows rather than trusted.
    #[tokio::test]
    async fn a_handle_that_is_not_a_handle_is_refused() {
        let root = super::super::test_workspace();
        let _ = root;
        for handle in [
            "../../etc",
            "has/slash",
            "has.dot",
            "Has-Capital",
            "",
            "has space",
        ] {
            assert!(
                home(handle, 1).is_err(),
                "{handle:?} was accepted as a handle"
            );
        }
        assert!(home("pdf-processing", 1).is_ok());
        // A version has to be one, for the same reason.
        assert!(home("pdf-processing", 0).is_err());
        assert!(home("pdf-processing", -1).is_err());
    }

    /// Which runtime runs a file is decided by its EXTENSION, the same rule on
    /// every platform: a shebang is a claim about somebody else's machine, and
    /// Windows has no notion of one.
    #[test]
    fn a_script_is_run_by_what_it_is() {
        for (name, want) in [
            ("scripts/x.py", Some(Runtime::Python)),
            ("scripts/x.js", Some(Runtime::Node)),
            ("scripts/x.mjs", Some(Runtime::Node)),
            ("scripts/x.cjs", Some(Runtime::Node)),
            ("scripts/x.sh", Some(Runtime::Bash)),
            ("scripts/x.ps1", Some(Runtime::PowerShell)),
            ("scripts/X.PY", Some(Runtime::Python)),
            ("scripts/x.rb", None),
            ("scripts/x", None),
            ("SKILL.md", None),
        ] {
            assert_eq!(Runtime::of(Path::new(name)), want, "{name}");
        }
    }

    /// The script actually runs, with its arguments passed through untouched.
    #[tokio::test]
    async fn a_script_runs_with_its_arguments() {
        let root = super::super::test_workspace();
        let _ = root;
        if Runtime::Python.program().await.is_none() {
            return; // no Python on this machine; the rest of the suite still holds
        }
        let body = "import sys\nprint('|'.join(sys.argv[1:]))\n";
        assert!(
            run_install(json!({
                "skill": "pdf-processing", "version": 41,
                "files": [{"path": "scripts/echo.py", "text": body, "sha256": hash(body)}]
            }))
            .await
            .ok
        );

        let ran = run_script(json!({
            "skill": "pdf-processing", "version": 41, "script": "scripts/echo.py",
            // A pattern and a space, neither of which a shell would leave alone.
            "args": ["*.pdf", "two words", "$HOME"]
        }))
        .await;
        assert!(ran.ok, "the script failed: {}", ran.message);
        let out = ran.content.expect("an answer")["output"]
            .as_str()
            .unwrap()
            .to_string();
        assert_eq!(
            out.trim(),
            "*.pdf|two words|$HOME",
            "the arguments were expanded or split: there is no shell in this path"
        );
    }

    /// A script finds a library installed into its own package, which is what
    /// makes "install the missing library and run it again" work at all.
    ///
    /// WHAT THIS COVERS, exactly: ordinary resolution. A script inside the
    /// package walks up from its own directory and finds `<home>/node_modules`
    /// on its own, and Python finds a sibling module the same way. That is the
    /// mechanism the "install the library and run it again" story actually
    /// rests on, and it is worth a test.
    ///
    /// What it does NOT cover is `NODE_PATH`, and the control says so: point
    /// that back at the package root, as it was written first, and this still
    /// passes, because walk-up never needed it. It was wrong all the same
    /// (measured: node resolves from the directory whose CHILDREN are modules,
    /// so the package root resolves nothing and says nothing) and a variable
    /// that promises a search path it cannot serve is worse than no variable.
    /// It earns its place for a script that changes directory or spawns a node
    /// child from somewhere else, which is not tested here.
    ///
    /// Python's is the other way round: PYTHONPATH is the directory that
    /// CONTAINS the module file, so it is the package root.
    #[tokio::test]
    async fn a_script_finds_a_library_installed_into_its_package() {
        let root = super::super::test_workspace();
        let _ = root;

        // A hand-made module, so this needs no network and no npm.
        let pkg = "{\"name\":\"pretend\",\"main\":\"index.js\"}";
        let index = "module.exports = { greeting: 'from the installed library' };\n";
        let user = "const lib = require('pretend');\nconsole.log(lib.greeting);\n";
        let pymod = "GREETING = 'from the installed module'\n";
        let pyuser = "import pretend_mod\nprint(pretend_mod.GREETING)\n";
        assert!(
            run_install(json!({
                "skill": "pdf-processing", "version": 97,
                "files": [
                    {"path": "node_modules/pretend/package.json", "text": pkg, "sha256": hash(pkg)},
                    {"path": "node_modules/pretend/index.js", "text": index, "sha256": hash(index)},
                    {"path": "scripts/uses.js", "text": user, "sha256": hash(user)},
                    {"path": "pretend_mod.py", "text": pymod, "sha256": hash(pymod)},
                    {"path": "scripts/uses.py", "text": pyuser, "sha256": hash(pyuser)},
                ]
            }))
            .await
            .ok
        );

        if Runtime::Node.program().await.is_some() {
            let ran = run_script(json!({
                "skill": "pdf-processing", "version": 97, "script": "scripts/uses.js"
            }))
            .await;
            assert!(ran.ok, "node could not find the library: {}", ran.message);
            let out = ran.content.expect("an answer")["output"]
                .as_str()
                .unwrap()
                .to_string();
            assert!(out.contains("from the installed library"), "{out}");
        }
        if Runtime::Python.program().await.is_some() {
            let ran = run_script(json!({
                "skill": "pdf-processing", "version": 97, "script": "scripts/uses.py"
            }))
            .await;
            assert!(ran.ok, "python could not find the module: {}", ran.message);
            let out = ran.content.expect("an answer")["output"]
                .as_str()
                .unwrap()
                .to_string();
            assert!(out.contains("from the installed module"), "{out}");
        }
    }

    /// EVERY runtime this build claims, actually running one.
    ///
    /// The mapping from extension to runtime is unit-tested above, which proves
    /// the table and nothing about whether any of them can be started, find
    /// their arguments, or come back. Python was exercised and the other three
    /// were not, so three quarters of the claim rested on the table being
    /// right.
    ///
    /// A runtime this machine does not have is SKIPPED rather than failed, and
    /// the message it produces is checked instead: "needs X and it is not
    /// installed" is the other half of the behaviour and the half a person
    /// actually meets.
    #[tokio::test]
    async fn every_runtime_it_claims_can_run_something() {
        let root = super::super::test_workspace();
        let _ = root;

        // Each one prints its two arguments joined, so one assertion covers
        // starting it, finding the script, and passing arguments through
        // untouched.
        let cases: [(&str, &str, Runtime); 4] = [
            (
                "scripts/say.py",
                "import sys\nprint('|'.join(sys.argv[1:]))\n",
                Runtime::Python,
            ),
            (
                "scripts/say.js",
                "console.log(process.argv.slice(2).join('|'))\n",
                Runtime::Node,
            ),
            (
                "scripts/say.sh",
                "#!/usr/bin/env bash\nIFS='|'; echo \"$*\"\n",
                Runtime::Bash,
            ),
            ("scripts/say.ps1", "$args -join '|'\n", Runtime::PowerShell),
        ];

        let mut version = 100;
        let mut ran_any = false;
        for (path, body, runtime) in cases {
            version += 1;
            assert!(
                run_install(json!({
                    "skill": "pdf-processing", "version": version,
                    "files": [{"path": path, "text": body, "sha256": hash(body)}]
                }))
                .await
                .ok,
                "{path} could not be installed"
            );

            let ran = run_script(json!({
                "skill": "pdf-processing", "version": version, "script": path,
                // A glob and a space: neither survives a shell, and there is no
                // shell in this path for any of the four.
                "args": ["*.pdf", "two words"]
            }))
            .await;

            if runtime.program().await.is_none() {
                // Not installed here. The ANSWER is what matters then, and it
                // has to name what to install rather than read as a failure of
                // the skill.
                assert!(!ran.ok, "{path} ran with no runtime for it");
                assert!(
                    ran.message.contains(runtime.needs()),
                    "{path}: the answer does not say what is missing: {}",
                    ran.message
                );
                continue;
            }

            assert!(ran.ok, "{path} did not run: {}", ran.message);
            let out = ran.content.expect("an answer")["output"]
                .as_str()
                .unwrap_or_default()
                .to_string();
            assert_eq!(
                out.trim(),
                "*.pdf|two words",
                "{path}: the arguments were expanded or split"
            );
            ran_any = true;
        }
        assert!(
            ran_any,
            "not one runtime was available, so this proved nothing"
        );
    }

    /// A script that exits non-zero RAN. What it said is the answer, and the
    /// answer says which it was.
    #[tokio::test]
    async fn a_script_that_fails_says_what_it_said() {
        let root = super::super::test_workspace();
        let _ = root;
        if Runtime::Python.program().await.is_none() {
            return;
        }
        let body = "import sys\nsys.stderr.write('no such file: in.pdf\\n')\nsys.exit(2)\n";
        assert!(
            run_install(json!({
                "skill": "pdf-processing", "version": 51,
                "files": [{"path": "scripts/bad.py", "text": body, "sha256": hash(body)}]
            }))
            .await
            .ok
        );

        let ran = run_script(json!({
            "skill": "pdf-processing", "version": 51, "script": "scripts/bad.py"
        }))
        .await;
        assert!(!ran.ok);
        assert!(ran.message.contains("exited 2"), "{}", ran.message);
        assert!(
            ran.message.contains("no such file"),
            "what it printed is lost: {}",
            ran.message
        );
    }

    /// A script can import its siblings, which is why the whole package is sent
    /// rather than the one file that was asked for.
    #[tokio::test]
    async fn a_script_can_import_what_came_with_it() {
        let root = super::super::test_workspace();
        let _ = root;
        if Runtime::Python.program().await.is_none() {
            return;
        }
        let helper = "VALUE = 'from the sibling'\n";
        let main = "import helper\nprint(helper.VALUE)\n";
        assert!(
            run_install(json!({
                "skill": "pdf-processing", "version": 61,
                "files": [
                    {"path": "helper.py", "text": helper, "sha256": hash(helper)},
                    {"path": "scripts/main.py", "text": main, "sha256": hash(main)},
                ]
            }))
            .await
            .ok
        );

        let ran = run_script(json!({
            "skill": "pdf-processing", "version": 61, "script": "scripts/main.py"
        }))
        .await;
        assert!(
            ran.ok,
            "a script could not import its sibling: {}",
            ran.message
        );
        let out = ran.content.expect("an answer")["output"]
            .as_str()
            .unwrap()
            .to_string();
        assert!(out.contains("from the sibling"), "{out}");
    }

    /// Ending a script ends EVERYTHING IT STARTED, which is why it is given a
    /// process group of its own.
    ///
    /// Killing the script alone leaves what it spawned: a compiler still
    /// holding the files, a server still on the port, with nothing left that
    /// knows about it. The terminal learned this and says so where it kills;
    /// this is the same claim for a script, and it was made in writing before
    /// it was checked.
    ///
    /// The grandchild writes a file every tenth of a second. If the group died
    /// the file stops changing; if only the script died it goes on growing
    /// after the call has come back.
    #[tokio::test]
    async fn ending_a_script_ends_what_it_started() {
        let root = super::super::test_workspace();
        if Runtime::Python.program().await.is_none() {
            return;
        }
        let witness = root.join("grandchild.log");
        let _ = std::fs::remove_file(&witness);

        // A script that starts a child which outlives it, then hangs. The child
        // is its OWN file in the package rather than Python embedded in a -c
        // string: the first version of this built one by hand inside a Rust
        // format string, the quoting did not survive, and the child never
        // started. Its own control caught that, which is the only reason this
        // test is not quietly passing on nothing.
        let child = "import sys, time\nwhile True:\n    open(sys.argv[1], 'a').write('.')\n    time.sleep(0.05)\n";
        let spawner = "import subprocess, sys, time, os\n\
             here = os.path.dirname(os.path.abspath(__file__))\n\
             subprocess.Popen([sys.executable, os.path.join(here, 'child.py'), sys.argv[1]])\n\
             print('started', flush=True)\n\
             time.sleep(600)\n";
        assert!(
            run_install(json!({
                "skill": "pdf-processing", "version": 95,
                "files": [
                    {"path": "scripts/child.py", "text": child, "sha256": hash(child)},
                    {"path": "scripts/spawner.py", "text": spawner, "sha256": hash(spawner)},
                ]
            }))
            .await
            .ok
        );

        let was = DEADLINE_SECONDS.swap(2, std::sync::atomic::Ordering::Relaxed);
        let ran = run_script(json!({
            "skill": "pdf-processing", "version": 95, "script": "scripts/spawner.py",
            "args": [witness.to_string_lossy()]
        }))
        .await;
        DEADLINE_SECONDS.store(was, std::sync::atomic::Ordering::Relaxed);
        assert!(!ran.ok, "the script was not stopped at all");

        // The grandchild wrote while the script ran, so there is something to
        // measure. Without this the assertion below would pass on a child that
        // never started.
        let alive = std::fs::metadata(&witness).map(|m| m.len()).unwrap_or(0);
        assert!(
            alive > 0,
            "the grandchild never ran, so this proves nothing"
        );

        // And it stopped when the group did.
        tokio::time::sleep(std::time::Duration::from_millis(600)).await;
        let after = std::fs::metadata(&witness).map(|m| m.len()).unwrap_or(0);
        let _ = std::fs::remove_file(&witness);
        assert_eq!(
            alive,
            after,
            "the grandchild outlived the script: it wrote {} more bytes after the call came back",
            after.saturating_sub(alive)
        );
    }

    /// WHICH CEILINGS ACTUALLY BITE, measured on this machine rather than
    /// claimed.
    ///
    /// This test exists because two of them were written down as guards before
    /// anybody checked, and one of them does not exist: Darwin refuses
    /// RLIMIT_AS outright. A limit that cannot be set is not a limit, and a
    /// document saying otherwise is worse than no document.
    ///
    /// It asserts what holds and RECORDS what does not, so a platform that
    /// gains the missing one shows up as a surprise rather than staying
    /// invisible.
    ///
    /// What each platform actually holds, and by what mechanism:
    ///
    /// | ceiling    | Linux       | macOS        | Windows            |
    /// |------------|-------------|--------------|--------------------|
    /// | wall clock | yes         | yes          | yes                |
    /// | processor  | RLIMIT_CPU  | RLIMIT_CPU   | job, process time  |
    /// | memory     | RLIMIT_AS   | **none**     | job, commit limit  |
    /// | file size  | RLIMIT_FSIZE| RLIMIT_FSIZE | **none**           |
    ///
    /// So the two gaps are on different platforms and neither is hidden: macOS
    /// has no memory ceiling because Darwin implements none, and Windows has no
    /// file-size ceiling because a job object has no equivalent of it. Each
    /// section below runs only where the thing it tests can exist.
    #[tokio::test]
    async fn the_ceilings_that_hold_are_the_ones_the_kernel_agrees_to() {
        let root = super::super::test_workspace();
        let _ = root;
        if Runtime::Python.program().await.is_none() {
            return;
        }

        // 1. The processor ceiling holds. A script that only spins is stopped
        //    by it, not by the wall clock, which this test leaves long.
        let spin = "x = 0\nwhile True:\n  x += 1\n";
        assert!(
            run_install(json!({
                "skill": "pdf-processing", "version": 91,
                "files": [{"path": "scripts/spin.py", "text": spin, "sha256": hash(spin)}]
            }))
            .await
            .ok
        );
        let was = MAX_CPU_SECONDS.swap(2, std::sync::atomic::Ordering::Relaxed);
        let started = std::time::Instant::now();
        let spun = run_script(json!({
            "skill": "pdf-processing", "version": 91, "script": "scripts/spin.py"
        }))
        .await;
        MAX_CPU_SECONDS.store(was, std::sync::atomic::Ordering::Relaxed);
        assert!(
            !spun.ok,
            "a script that spins for ever was reported as fine"
        );
        assert!(
            started.elapsed() < std::time::Duration::from_secs(20),
            "it ran for {:?}: the processor ceiling did not stop it, the wall clock did",
            started.elapsed()
        );

        // 2. The file-size ceiling holds, and the script hears about it rather
        //    than the disk filling up. Lowered to 8 MiB, because the first
        //    version of this wrote 200 MB against a 512 MB ceiling and failed
        //    for the obvious reason: it never crossed it.
        //
        //    UNIX ONLY. A Windows job object has no file-size limit to ask for,
        //    so there is nothing here to test rather than something that fails:
        //    the table above says so, and `MAX_FILE_SIZE` is gated to match.
        #[cfg(unix)]
        {
            let fat = "open('fat.bin', 'wb').write(b'x' * (64 * 1024 * 1024))\n";
            assert!(
                run_install(json!({
                    "skill": "pdf-processing", "version": 92,
                    "files": [{"path": "scripts/fat.py", "text": fat, "sha256": hash(fat)}]
                }))
                .await
                .ok
            );
            let was_size = MAX_FILE_SIZE.swap(8 << 20, std::sync::atomic::Ordering::Relaxed);
            let wrote = run_script(json!({
                "skill": "pdf-processing", "version": 92, "script": "scripts/fat.py"
            }))
            .await;
            MAX_FILE_SIZE.store(was_size, std::sync::atomic::Ordering::Relaxed);
            // Removed BEFORE the assertion, so a failure does not leave
            // megabytes of it in the workspace every time the suite runs.
            let _ = std::fs::remove_file(root.join("fat.bin"));
            assert!(!wrote.ok, "a script wrote past the file-size ceiling");
        }

        // 3. The memory ceiling, which is a different answer on each platform.
        //
        //    macOS: NOT a ceiling. Asserted as what it is, so the day Darwin
        //    starts honouring RLIMIT_AS this test says so rather than nobody
        //    noticing either way.
        #[cfg(unix)]
        {
            use nix::sys::resource::{setrlimit, Resource};
            let asked = setrlimit(Resource::RLIMIT_AS, MAX_MEMORY, MAX_MEMORY);
            if cfg!(target_os = "macos") {
                assert!(
                    asked.is_err(),
                    "macOS now honours RLIMIT_AS: the comment in bound() and the KB both say it \
                     does not, and a real memory ceiling is worth having"
                );
            }
        }

        //    Windows: the job carries a real commit limit, and it has NO test
        //    here. Testing it needs the ceiling lowered, `MAX_MEMORY` is a
        //    constant, and turning it into something a test can write was a
        //    change to a line macOS runs, made for a Windows test's
        //    convenience. That is not a trade worth making on a platform that
        //    is already verified, so the ceiling is applied and left unproved.
    }

    /// A script that will not stop is stopped, and what it printed first is
    /// still worth having.
    #[tokio::test]
    async fn a_script_that_never_ends_is_ended() {
        let root = super::super::test_workspace();
        let _ = root;
        if Runtime::Python.program().await.is_none() {
            return;
        }
        // Seconds rather than five minutes, which is the only reason this is a
        // test somebody runs rather than one nobody ever checks.
        let body = "import sys, time\nprint('started', flush=True)\ntime.sleep(600)\n";
        assert!(
            run_install(json!({
                "skill": "pdf-processing", "version": 71,
                "files": [{"path": "scripts/forever.py", "text": body, "sha256": hash(body)}]
            }))
            .await
            .ok
        );

        let was = DEADLINE_SECONDS.swap(2, std::sync::atomic::Ordering::Relaxed);
        let started = std::time::Instant::now();
        let ran = tokio::time::timeout(
            std::time::Duration::from_secs(30),
            run_script(json!({
                "skill": "pdf-processing", "version": 71, "script": "scripts/forever.py"
            })),
        )
        .await
        .expect("it outlived even the test's own patience");
        DEADLINE_SECONDS.store(was, std::sync::atomic::Ordering::Relaxed);

        assert!(!ran.ok);
        assert!(ran.message.contains("was stopped"), "{}", ran.message);
        assert!(
            started.elapsed() < std::time::Duration::from_secs(15),
            "it waited the full deadline"
        );
    }
}
