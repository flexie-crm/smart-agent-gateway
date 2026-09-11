//! Reading and changing files on this computer.
//!
//! Five tools, because they are five decisions an administrator makes
//! separately: somebody may be allowed to read a project without being allowed
//! to write to it, and to search it without either. One tool with a `mode`
//! would collapse that into one grant.
//!
//! The same rule as the terminal, and for the same reason (KB/39): the chosen
//! folder is a WORKING DIRECTORY, not a boundary. A relative path is measured
//! from it; an absolute path reaches what it names, because the person could
//! open that file themselves and these tools act as them. What bounds this is
//! the account the application runs as, and the grants an administrator gave.
//!
//! Nothing here decides whether it MAY run. That was decided before the call
//! left the gateway: the tool has to be granted to the agent, and if the
//! administrator put it in the agent's confirm set, the person has already
//! answered a card. This half owns the doing.

use std::collections::HashSet;
use std::path::{Path, PathBuf};

use serde::Deserialize;
use serde_json::{json, Value};

use super::Response;
use crate::workspace;

/// How much text one read carries back. It goes into a model's context, so a
/// log file has to be trimmed rather than allowed to end the turn on tokens.
const MAX_READ: usize = 256 * 1024;

/// How many lines a read returns when nothing says otherwise.
const DEFAULT_LINES: usize = 2000;

/// How much may be written in one call. The link carries larger messages than
/// this, so the limit is about what a model should be composing in one go.
const MAX_WRITE: usize = 4 * 1024 * 1024;

/// How many results a search or a listing carries back.
const MAX_MATCHES: usize = 200;
const MAX_FILES: usize = 500;

/// How much of a file is examined for the one thing that decides whether it is
/// text: a zero byte. Every format that is not text has one early.
const SNIFF: usize = 8 * 1024;

/// What has been READ in each conversation, so that nothing is overwritten
/// blind.
///
/// The rule: a file that already exists may only be replaced by somebody who
/// has read it first, in this same conversation. An assistant that rewrites a
/// file it never looked at is not editing, it is guessing, and the person whose
/// work was in there finds out afterwards.
///
/// It lives here because only this side can say which path is which: `./a.txt`,
/// `a.txt` and `/Users/me/p/a.txt` are one file, and settling that is what a
/// filesystem does. The conversation comes from the gateway, which is the only
/// half that knows one call from another.
///
/// Forgotten when the application closes, and that is the right amount of
/// memory: what it costs is one extra read.
mod seen {
    use std::collections::{HashMap, HashSet};
    use std::path::{Path, PathBuf};
    use std::sync::Mutex;

    /// Bounded, because this is a memory that nothing ever asks to be emptied:
    /// a long day's conversation reading a large project must not grow it
    /// without limit. When a conversation goes past its share the oldest
    /// entries are dropped, which costs a re-read and nothing else.
    const PATHS_PER_CONVERSATION: usize = 512;
    const CONVERSATIONS: usize = 64;

    static READ: Mutex<Option<HashMap<i64, HashSet<PathBuf>>>> = Mutex::new(None);

    fn with<T>(work: impl FnOnce(&mut HashMap<i64, HashSet<PathBuf>>) -> T) -> T {
        let mut held = READ.lock().unwrap_or_else(|poisoned| poisoned.into_inner());
        work(held.get_or_insert_with(HashMap::new))
    }

    /// remember that this conversation has read this file.
    pub fn read(conversation: i64, path: &Path) {
        if conversation == 0 {
            return; // nobody is asking on behalf of a conversation
        }
        let path = path.to_path_buf();
        with(|all| {
            if all.len() >= CONVERSATIONS && !all.contains_key(&conversation) {
                // The oldest conversation by id, which is the oldest one there
                // is: they are handed out in order.
                if let Some(oldest) = all.keys().min().copied() {
                    all.remove(&oldest);
                }
            }
            let paths = all.entry(conversation).or_default();
            if paths.len() >= PATHS_PER_CONVERSATION {
                paths.clear();
            }
            paths.insert(path);
        })
    }

    /// was this file read in this conversation?
    pub fn was_read(conversation: i64, path: &Path) -> bool {
        with(|all| all.get(&conversation).is_some_and(|paths| paths.contains(path)))
    }

    #[cfg(test)]
    pub fn forget_everything() {
        with(|all| all.clear())
    }
}

/// A file's hash: which version of it this is.
///
/// It is what makes an edit safe to write. A model reads a file, thinks, and
/// writes a change; in between, a build can have rewritten it, a formatter can
/// have moved every brace, the person can have saved it in their editor. An
/// edit that lands on a different file than the one it was written against is
/// how work is lost quietly, and no amount of careful text matching notices:
/// the text is still there, in a file that has moved on.
///
/// Short, because it is read by a model and repeated back: sixteen hex
/// characters of a sha256 is more than enough to notice a change, which is what
/// it is for. It is not a defence against somebody constructing a collision on
/// purpose; that is not the failure being prevented.
fn hash_of(text: &str) -> String {
    use sha2::{Digest, Sha256};
    let digest = Sha256::digest(text.as_bytes());
    digest.iter().take(8).map(|byte| format!("{byte:02x}")).collect()
}

/// What line ending a file uses, so an edit written with the other kind does
/// not rewrite every line of it.
///
/// A file written on Windows ends its lines with a carriage return and a line
/// feed; a model writes a line feed. Exact matching then fails on a file that
/// LOOKS identical, and the mistake is invisible in every message about it,
/// because a carriage return does not print. Worse is when it half works: the
/// replacement lands with the wrong endings and the file becomes a mixture,
/// which the next diff shows as every line changed.
fn windows_endings(text: &str) -> bool {
    let crlf = text.matches("\r\n").count();
    crlf > 0 && crlf * 2 >= text.matches('\n').count()
}

/// The same text with one kind of line ending, for comparing.
fn one_kind(text: &str) -> String {
    text.replace("\r\n", "\n")
}

/// The text as this file writes lines.
fn as_the_file_writes(text: &str, windows: bool) -> String {
    if windows {
        one_kind(text).replace('\n', "\r\n")
    } else {
        one_kind(text)
    }
}

/// resolve works out which file or folder is meant.
///
/// Nothing given is an error here, unlike the terminal, where nothing given is
/// the working folder: a read with no path is a call that forgot its argument.
fn resolve(given: &str) -> Result<PathBuf, String> {
    let given = given.trim();
    if given.is_empty() {
        return Err("give the path of the file".into());
    }
    let root = workspace::folder().ok_or_else(|| {
        "no folder has been chosen on this computer yet. The person can choose one in the chat \
         application, and a relative path is measured from it."
            .to_string()
    })?;
    let candidate = Path::new(given);
    Ok(if candidate.is_absolute() { candidate.to_path_buf() } else { root.join(candidate) })
}

/// A folder to work in: the one given, or the chosen one when none is.
fn resolve_folder(given: &str) -> Result<PathBuf, String> {
    let given = given.trim();
    if given.is_empty() {
        return workspace::folder().ok_or_else(|| {
            "no folder has been chosen on this computer yet. The person can choose one in the \
             chat application."
                .to_string()
        });
    }
    let folder = resolve(given)?;
    if !folder.is_dir() {
        return Err(format!("{} is not a folder on this computer", folder.to_string_lossy()));
    }
    Ok(folder)
}

/// text reads a file as text, or says why it is not one.
///
/// A zero byte in the first few kilobytes is what every tool that has ever had
/// to answer this uses, and it is right for the same reason: text does not
/// contain one, and everything that is not text contains one almost at once.
fn text(path: &Path) -> Result<String, Response> {
    let raw = match std::fs::read(path) {
        Ok(raw) => raw,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
            return Err(Response::bad_arguments(format!(
                "{} is not a file on this computer",
                path.to_string_lossy()
            )))
        }
        Err(err) => {
            return Err(Response::failed(format!("{} could not be read: {err}", path.to_string_lossy())))
        }
    };
    if raw.iter().take(SNIFF).any(|byte| *byte == 0) {
        return Err(Response::refused(format!(
            "{} is not a text file, so there is nothing here to read as text",
            path.to_string_lossy()
        )));
    }
    Ok(String::from_utf8_lossy(&raw).into_owned())
}

/// settled is the one name a file has, so that `./a.txt`, `a.txt` and the full
/// path are one file to the rule above. A path that cannot be resolved (it is
/// not there yet) is left as it was built, which is enough: a file that does
/// not exist cannot be overwritten.
fn settled(path: &Path) -> PathBuf {
    path.canonicalize().unwrap_or_else(|_| path.to_path_buf())
}

/// How a path is reported back: as the person would say it, relative to their
/// folder when it is inside it, and in full when it is not.
fn spoken(path: &Path) -> String {
    if let Some(root) = workspace::folder() {
        if let Ok(inside) = path.strip_prefix(&root) {
            return inside.to_string_lossy().into_owned();
        }
    }
    path.to_string_lossy().into_owned()
}

// ── read ────────────────────────────────────────────────────────────────────

pub mod read {
    use super::*;

    pub const NAME: &str = "read_file";
    pub const VERSION: i64 = 1;

    #[derive(Debug, Deserialize)]
    pub struct Args {
        pub path: String,
        /// The first line to return, counting from 1.
        #[serde(default)]
        pub offset: usize,
        /// How many lines. Zero means the default.
        #[serde(default)]
        pub limit: usize,
        /// Which conversation is asking. Added by the gateway, never by a
        /// model: it is how a write knows whether this file has been read.
        #[serde(default)]
        pub conversation: i64,
    }

    pub async fn run(args: Value) -> Response {
        let args: Args = match serde_json::from_value(args) {
            Ok(args) => args,
            Err(err) => {
                return Response::bad_arguments(format!("the arguments could not be read: {err}"))
            }
        };
        let path = match resolve(&args.path) {
            Ok(path) => path,
            Err(reason) => return Response::bad_arguments(reason),
        };
        let whole = match text(&path) {
            Ok(whole) => whole,
            Err(response) => return response,
        };

        // Read, and remembered as read: a write to this file may now go ahead
        // in this conversation. Recorded even when only part of it was asked
        // for, because somebody who read the first page has still looked.
        seen::read(args.conversation, &settled(&path));

        let lines: Vec<&str> = whole.lines().collect();
        let total = lines.len();
        let from = args.offset.max(1);
        if from > total && total > 0 {
            return Response::bad_arguments(format!(
                "that file has {total} lines, so there is nothing at line {from}"
            ));
        }
        let take = if args.limit == 0 { DEFAULT_LINES } else { args.limit };
        let slice: Vec<&str> = lines.iter().skip(from - 1).take(take).copied().collect();
        let to = from + slice.len().saturating_sub(1);

        let mut content = slice.join("\n");
        let mut trimmed = false;
        if content.len() > MAX_READ {
            // The BEGINNING is kept here, the opposite of a command's output:
            // a file is read from the top, and what was asked for is the part
            // the offset points at.
            content.truncate(floor_char(&content, MAX_READ));
            trimmed = true;
        }

        Response::ok(json!({
            "path": spoken(&path),
            // Which version of the file this is. An edit that carries it back
            // is refused if the file has moved on since.
            "hash": hash_of(&whole),
            "content": content,
            "from_line": from,
            "to_line": to,
            "total_lines": total,
            // Said rather than implied: a model that cannot tell a whole file
            // from the first part of one will answer about the part.
            "more": trimmed || to < total,
        }))
    }
}

// ── write ───────────────────────────────────────────────────────────────────

pub mod write {
    use super::*;

    pub const NAME: &str = "write_file";

    /// TWO: the arguments grew expect_hash.
    pub const VERSION: i64 = 2;

    #[derive(Debug, Deserialize)]
    pub struct Args {
        pub path: String,
        pub content: String,
        /// The hash the file had when it was read. Given, the write is refused
        /// if the file has changed since: somebody else's work is not
        /// overwritten by a change written against what it used to say.
        #[serde(default)]
        pub expect_hash: String,
        /// Which conversation is asking (added by the gateway).
        #[serde(default)]
        pub conversation: i64,
    }

    pub async fn run(args: Value) -> Response {
        let args: Args = match serde_json::from_value(args) {
            Ok(args) => args,
            Err(err) => {
                return Response::bad_arguments(format!("the arguments could not be read: {err}"))
            }
        };
        if args.content.len() > MAX_WRITE {
            return Response::bad_arguments(format!(
                "that is {} bytes, and one call writes at most {MAX_WRITE}. Write it in parts.",
                args.content.len()
            ));
        }
        let path = match resolve(&args.path) {
            Ok(path) => path,
            Err(reason) => return Response::bad_arguments(reason),
        };
        // The folder is made if it is not there, the way `mkdir -p` would.
        //
        // It was refused at first, on the grounds that a path with a typo in it
        // would leave a directory nobody asked for. That was wrong, and the
        // reason is the grants: this tool can be given to an agent WITHOUT the
        // terminal, and then a refusal here means new work can never be put
        // anywhere new. A stray folder is visible in the answer and costs
        // somebody one delete; not being able to create one is a tool that
        // cannot do its job.
        let mut made_folder = false;
        if let Some(parent) = path.parent() {
            if !parent.as_os_str().is_empty() && !parent.is_dir() {
                if let Err(err) = std::fs::create_dir_all(parent) {
                    return Response::failed(format!(
                        "{} could not be made: {err}",
                        parent.to_string_lossy()
                    ));
                }
                made_folder = true;
            }
        }
        let existed = path.is_file();
        // Written against a version of the file, if the caller said which.
        if existed && !args.expect_hash.is_empty() {
            let whole = match text(&path) {
                Ok(whole) => whole,
                Err(response) => return response,
            };
            let now = hash_of(&whole);
            if now != args.expect_hash {
                return Response::bad_arguments(format!(
                    "{} has changed since it was read (it was {}, it is now {}). Read it again, so \
                     that what somebody else did to it is not written over.",
                    spoken(&path),
                    args.expect_hash,
                    now
                ));
            }
        }
        // Nothing is overwritten blind. A file that is already there may only
        // be replaced by somebody who has read it in this conversation:
        // rewriting a file nobody looked at is not editing, it is guessing,
        // and the person whose work was in it finds out afterwards.
        //
        // Creating is untouched by this, and so is edit_file, which reads the
        // file itself in order to find the text it is replacing.
        //
        // With no conversation the rule cannot be applied at all: there is no
        // history to consult, by construction. It passes rather than refuses,
        // because this is a rule against a mistake rather than a boundary
        // against an attacker, and refusing everything a worker or a command
        // line asked for would be a capability quietly lost. Where it matters,
        // which is a person in a conversation, the conversation is always there.
        if existed && args.conversation != 0 && !seen::was_read(args.conversation, &settled(&path)) {
            return Response::bad_arguments(format!(
                "{} already exists and has not been read in this conversation. Read it first, so \
                 that what is in it is not lost; use edit_file to change part of it, or write it \
                 again once you have seen it.",
                spoken(&path)
            ));
        }
        if let Err(err) = std::fs::write(&path, args.content.as_bytes()) {
            return Response::failed(format!("{} could not be written: {err}", spoken(&path)));
        }
        Response::ok(json!({
            "path": spoken(&path),
            // The version it is now, so the next edit can be written against
            // this one without reading it again.
            "hash": hash_of(&args.content),
            "created": !existed,
            // Said, because it is a thing that happened to somebody's disk that
            // they did not ask for in so many words.
            "created_folder": made_folder,
            "bytes": args.content.len(),
        }))
    }
}

// ── edit ────────────────────────────────────────────────────────────────────

pub mod edit {
    use super::*;

    pub const NAME: &str = "edit_file";

    /// TWO: the arguments grew a hash and a line range.
    pub const VERSION: i64 = 2;

    #[derive(Debug, Deserialize)]
    pub struct Args {
        pub path: String,
        /// The exact text to replace. One of this or a line range.
        #[serde(default)]
        pub find: String,
        /// What goes in its place, or in place of the lines.
        #[serde(default)]
        pub replace: String,
        /// Every occurrence rather than the one. Off by default, because the
        /// one is what somebody means when they say "this line".
        #[serde(default)]
        pub all: bool,
        /// The first line to replace, counting from 1. With this and end_line,
        /// no text has to match at all.
        #[serde(default)]
        pub start_line: usize,
        /// The last line to replace, included. Defaults to start_line.
        #[serde(default)]
        pub end_line: usize,
        /// The hash the file had when it was read. Given, the edit is refused
        /// if the file has changed since.
        #[serde(default)]
        pub expect_hash: String,
        /// Which conversation is asking (added by the gateway).
        #[serde(default)]
        pub conversation: i64,
    }

    pub async fn run(args: Value) -> Response {
        let args: Args = match serde_json::from_value(args) {
            Ok(args) => args,
            Err(err) => {
                return Response::bad_arguments(format!("the arguments could not be read: {err}"))
            }
        };
        let by_lines = args.start_line > 0;
        if !by_lines && args.find.is_empty() {
            return Response::bad_arguments(
                "give the exact text to replace in find, or a line range in start_line and end_line",
            );
        }
        if by_lines && !args.find.is_empty() {
            return Response::bad_arguments(
                "give a line range or text to find, not both: they are two ways of saying which part to change",
            );
        }
        let path = match resolve(&args.path) {
            Ok(path) => path,
            Err(reason) => return Response::bad_arguments(reason),
        };
        let whole = match text(&path) {
            Ok(whole) => whole,
            Err(response) => return response,
        };

        // Written against a version of the file, and this is that version.
        let now = hash_of(&whole);
        if !args.expect_hash.is_empty() && args.expect_hash != now {
            return Response::bad_arguments(format!(
                "{} has changed since it was read (it was {}, it is now {}). Read it again and \
                 write the edit against what is in it now.",
                spoken(&path),
                args.expect_hash,
                now
            ));
        }
        // An edit has read the file, so a write after it is not writing blind.
        seen::read(args.conversation, &settled(&path));

        let windows = windows_endings(&whole);
        let (updated, replacements) = if by_lines {
            match by_line_range(&whole, args.start_line, args.end_line, &args.replace, windows) {
                Ok(done) => done,
                Err(reason) => return Response::bad_arguments(reason),
            }
        } else {
            match by_exact_text(&whole, &args.find, &args.replace, args.all, windows) {
                Ok(done) => done,
                Err(reason) => return Response::bad_arguments(format!("{reason} in {}", spoken(&path))),
            }
        };

        if updated == whole {
            return Response::bad_arguments(
                "that change would leave the file exactly as it is, so nothing was written",
            );
        }
        if let Err(err) = std::fs::write(&path, updated.as_bytes()) {
            return Response::failed(format!("{} could not be written: {err}", spoken(&path)));
        }
        Response::ok(json!({
            "path": spoken(&path),
            "replacements": replacements,
            // The version it is NOW, so a second edit can be written against
            // this one without reading the file again.
            "hash": hash_of(&updated),
            "lines": updated.lines().count(),
        }))
    }

    /// by_exact_text replaces text the caller copied out of the file.
    ///
    /// Matched twice if it has to be: once exactly, and once with line endings
    /// set aside. A file written on Windows and an edit written by a model
    /// differ by a character that does not print, and refusing that is refusing
    /// something that is right in every way a person can see.
    fn by_exact_text(
        whole: &str,
        find: &str,
        replace: &str,
        all: bool,
        windows: bool,
    ) -> Result<(String, usize), String> {
        if find == replace {
            return Err("find and replace are the same text, so there is nothing to do".into());
        }
        // The straightforward way first: what is in the file, as it is.
        let exact = whole.matches(find).count();
        if exact > 0 {
            return apply(whole, find, replace, exact, all, windows);
        }
        // Then with line endings set aside, on both sides.
        let flattened = one_kind(whole);
        let wanted = one_kind(find);
        let found = flattened.matches(&wanted).count();
        if found == 0 {
            return Err(format!(
                "that text is not there. Read the file and copy the lines exactly, spaces \
                 included{}",
                if windows { " (this file's lines end the Windows way, which is handled for you)" } else { "" }
            ));
        }
        let (updated, count) = apply(&flattened, &wanted, &one_kind(replace), found, all, false)?;
        Ok((as_the_file_writes(&updated, windows), count))
    }

    /// apply does the replacement, once the text has been found.
    fn apply(
        whole: &str,
        find: &str,
        replace: &str,
        found: usize,
        all: bool,
        windows: bool,
    ) -> Result<(String, usize), String> {
        // Refused rather than guessed at: replacing the first of four is a
        // decision this cannot make on somebody's behalf, and the assistant can
        // correct it by including more of the surrounding lines.
        if found > 1 && !all {
            return Err(format!(
                "that text appears {found} times. Include enough of the lines around it to name \
                 one, set all to replace every occurrence, or use start_line and end_line"
            ));
        }
        let replace = as_the_file_writes(replace, windows);
        let updated = if all {
            whole.replace(find, &replace)
        } else {
            whole.replacen(find, &replace, 1)
        };
        Ok((updated, if all { found } else { 1 }))
    }

    /// by_line_range replaces lines by number, which nothing about whitespace
    /// or line endings can make ambiguous. It is the safer way to change a
    /// block, and read_file hands out the numbers to use.
    fn by_line_range(
        whole: &str,
        start: usize,
        end: usize,
        replace: &str,
        windows: bool,
    ) -> Result<(String, usize), String> {
        let flattened = one_kind(whole);
        let lines: Vec<&str> = flattened.split('\n').collect();
        // A file ending in a newline splits into a last empty piece, which is
        // not a line somebody can address.
        let addressable = if lines.last() == Some(&"") { lines.len() - 1 } else { lines.len() };
        let end = if end == 0 { start } else { end };
        if start > addressable {
            return Err(format!("that file has {addressable} lines, so there is no line {start}"));
        }
        if end < start {
            return Err(format!("end_line {end} is before start_line {start}"));
        }
        let end = end.min(addressable);

        let mut out: Vec<String> = Vec::with_capacity(lines.len());
        out.extend(lines[..start - 1].iter().map(|line| line.to_string()));
        if !replace.is_empty() {
            out.extend(one_kind(replace).split('\n').map(|line| line.to_string()));
        }
        out.extend(lines[end..].iter().map(|line| line.to_string()));
        Ok((as_the_file_writes(&out.join("\n"), windows), end - start + 1))
    }
}

/// floor_char is the largest cut that does not land inside a character.
fn floor_char(text: &str, at: usize) -> usize {
    let mut at = at.min(text.len());
    while at > 0 && !text.is_char_boundary(at) {
        at -= 1;
    }
    at
}

/// Both listings walk the same way, so they skip the same things: what a
/// project's own ignore files say to skip, and the version control directory
/// itself. A search that answers with ten thousand hits from node_modules is a
/// search nobody can use.
fn walk(folder: &Path) -> ignore::Walk {
    ignore::WalkBuilder::new(folder)
        .hidden(false) // a dotfile is a file somebody may be looking for
        .git_ignore(true)
        .git_global(true)
        .git_exclude(true)
        .filter_entry(|entry| entry.file_name() != ".git")
        .build()
}

// ── find ────────────────────────────────────────────────────────────────────

pub mod find {
    use super::*;

    pub const NAME: &str = "find_files";
    pub const VERSION: i64 = 1;

    #[derive(Debug, Deserialize)]
    pub struct Args {
        /// A glob, as a person would type it: `**/*.go`, `src/**/test_*.py`.
        pub pattern: String,
        #[serde(default)]
        pub folder: String,
    }

    pub async fn run(args: Value) -> Response {
        let args: Args = match serde_json::from_value(args) {
            Ok(args) => args,
            Err(err) => {
                return Response::bad_arguments(format!("the arguments could not be read: {err}"))
            }
        };
        if args.pattern.trim().is_empty() {
            return Response::bad_arguments("give the pattern to look for in pattern");
        }
        let folder = match resolve_folder(&args.folder) {
            Ok(folder) => folder,
            Err(reason) => return Response::bad_arguments(reason),
        };
        let matcher = match globset::Glob::new(args.pattern.trim()) {
            Ok(glob) => glob.compile_matcher(),
            Err(err) => {
                return Response::bad_arguments(format!("that pattern could not be read: {err}"))
            }
        };

        let mut files = Vec::new();
        let mut more = false;
        for entry in walk(&folder).flatten() {
            if !entry.file_type().is_some_and(|kind| kind.is_file()) {
                continue;
            }
            let relative = entry.path().strip_prefix(&folder).unwrap_or(entry.path());
            if !matcher.is_match(relative) && !matcher.is_match(entry.path()) {
                continue;
            }
            if files.len() == MAX_FILES {
                more = true;
                break;
            }
            files.push(spoken(entry.path()));
        }
        // In order, so the same question twice reads the same way.
        files.sort();
        Response::ok(json!({ "files": files, "count": files.len(), "more": more }))
    }
}

// ── search ──────────────────────────────────────────────────────────────────

pub mod search {
    use super::*;

    use grep_regex::RegexMatcher;
    use grep_searcher::sinks::UTF8;
    use grep_searcher::Searcher;

    pub const NAME: &str = "search_files";
    pub const VERSION: i64 = 1;

    #[derive(Debug, Deserialize)]
    pub struct Args {
        /// A regular expression, the same one a person would give ripgrep.
        pub pattern: String,
        #[serde(default)]
        pub folder: String,
        /// Only files matching this glob.
        #[serde(default)]
        pub glob: String,
        #[serde(default)]
        pub ignore_case: bool,
    }

    pub async fn run(args: Value) -> Response {
        let args: Args = match serde_json::from_value(args) {
            Ok(args) => args,
            Err(err) => {
                return Response::bad_arguments(format!("the arguments could not be read: {err}"))
            }
        };
        if args.pattern.trim().is_empty() {
            return Response::bad_arguments("give the text or expression to look for in pattern");
        }
        let folder = match resolve_folder(&args.folder) {
            Ok(folder) => folder,
            Err(reason) => return Response::bad_arguments(reason),
        };
        let matcher = match RegexMatcher::new_line_matcher(&pattern(&args)) {
            Ok(matcher) => matcher,
            Err(err) => {
                return Response::bad_arguments(format!(
                    "that expression could not be read: {err}"
                ))
            }
        };
        let only = if args.glob.trim().is_empty() {
            None
        } else {
            match globset::Glob::new(args.glob.trim()) {
                Ok(glob) => Some(glob.compile_matcher()),
                Err(err) => {
                    return Response::bad_arguments(format!("that glob could not be read: {err}"))
                }
            }
        };

        let mut matches = Vec::new();
        let mut files = HashSet::new();
        let mut more = false;
        let mut searcher = Searcher::new();
        for entry in walk(&folder).flatten() {
            if matches.len() >= MAX_MATCHES {
                more = true;
                break;
            }
            if !entry.file_type().is_some_and(|kind| kind.is_file()) {
                continue;
            }
            let relative = entry.path().strip_prefix(&folder).unwrap_or(entry.path());
            if let Some(only) = &only {
                if !only.is_match(relative) && !only.is_match(entry.path()) {
                    continue;
                }
            }
            let path = spoken(entry.path());
            let _ = searcher.search_path(
                &matcher,
                entry.path(),
                UTF8(|line, text| {
                    if matches.len() < MAX_MATCHES {
                        files.insert(path.clone());
                        matches.push(json!({
                            "file": path,
                            "line": line,
                            "text": text.trim_end(),
                        }));
                    }
                    Ok(matches.len() < MAX_MATCHES)
                }),
            );
        }

        Response::ok(json!({
            "matches": matches,
            "files": files.len(),
            "count": matches.len(),
            "more": more,
        }))
    }

    /// The expression as given, or made case-insensitive by asking for it in
    /// the expression itself, which is how the engine takes it.
    fn pattern(args: &Args) -> String {
        if args.ignore_case {
            format!("(?i){}", args.pattern)
        } else {
            args.pattern.clone()
        }
    }
}

#[cfg(test)]
pub(super) mod tests {
    use super::*;

    /// One workspace for every test in this file, because the chosen folder is
    /// this installation's and there is one of it. Each test works in a folder
    /// of its own underneath, so they cannot tread on each other while the test
    /// runner runs them at the same time.
    fn workspace() -> &'static PathBuf {
        crate::tools::test_workspace()
    }

    /// A folder of this test's own, inside the workspace.
    pub(super) fn folder(name: &str) -> PathBuf {
        let path = workspace().join(name);
        std::fs::create_dir_all(&path).expect("make the test folder");
        path
    }

    pub(super) fn put(path: &Path, content: &str) {
        std::fs::write(path, content).expect("write a test file");
    }

    pub(super) async fn call<F, T>(run: F, args: Value) -> Response
    where
        F: FnOnce(Value) -> T,
        T: std::future::Future<Output = Response>,
    {
        run(args).await
    }

    pub(super) fn content(response: &Response) -> &Value {
        response.content.as_ref().expect("a successful call carries content")
    }

    #[tokio::test]
    async fn reads_a_file_and_says_how_much_of_it_there_is() {
        let here = folder("read");
        put(&here.join("poem.txt"), "one\ntwo\nthree\nfour\n");

        let answer = call(read::run, json!({ "path": "read/poem.txt" })).await;
        assert!(answer.ok, "{}", answer.message);
        let body = content(&answer);
        assert_eq!(body["content"], "one\ntwo\nthree\nfour");
        assert_eq!(body["total_lines"], 4);
        assert_eq!(body["from_line"], 1);
        assert_eq!(body["more"], false);

        // A part of it, and the answer says which part.
        let part = call(read::run, json!({ "path": "read/poem.txt", "offset": 2, "limit": 2 })).await;
        let body = content(&part);
        assert_eq!(body["content"], "two\nthree");
        assert_eq!(body["from_line"], 2);
        assert_eq!(body["to_line"], 3);
        assert_eq!(body["more"], true, "there is a fourth line it did not give");
    }

    #[tokio::test]
    async fn refuses_a_file_that_is_not_text() {
        let here = folder("binary");
        std::fs::write(here.join("picture.png"), [0x89, 0x50, 0x00, 0x01, 0x02]).unwrap();

        let answer = call(read::run, json!({ "path": "binary/picture.png" })).await;
        assert!(!answer.ok);
        assert_eq!(answer.kind, "denied");
        assert!(answer.message.contains("not a text file"), "{}", answer.message);
    }

    #[tokio::test]
    async fn says_which_file_is_missing() {
        let answer = call(read::run, json!({ "path": "nowhere/at/all.txt" })).await;
        assert!(!answer.ok);
        assert_eq!(answer.kind, "bad_arguments", "the assistant can correct a wrong path");
    }

    #[tokio::test]
    async fn writes_a_file_and_says_whether_it_made_it() {
        let here = folder("write");
        let made = call(
            write::run,
            json!({ "path": "write/notes.md", "content": "# Notes\n" }),
        )
        .await;
        assert!(made.ok, "{}", made.message);
        assert_eq!(content(&made)["created"], true);
        assert_eq!(std::fs::read_to_string(here.join("notes.md")).unwrap(), "# Notes\n");

        let again = call(
            write::run,
            json!({ "path": "write/notes.md", "content": "# Other\n" }),
        )
        .await;
        assert_eq!(content(&again)["created"], false, "replacing is not creating");
        assert_eq!(std::fs::read_to_string(here.join("notes.md")).unwrap(), "# Other\n");
    }

    // A folder that is not there is made, because this tool can be granted
    // without the terminal and then there would be no other way to put new
    // work anywhere new.
    #[tokio::test]
    async fn makes_the_folder_when_there_is_none() {
        let here = folder("write2");
        let answer = call(
            write::run,
            json!({ "path": "write2/nested/deep/file.txt", "content": "x" }),
        )
        .await;
        assert!(answer.ok, "{}", answer.message);
        assert_eq!(content(&answer)["created_folder"], true, "and it says it did");
        assert_eq!(
            std::fs::read_to_string(here.join("nested/deep/file.txt")).unwrap(),
            "x"
        );
    }

    // Nothing is overwritten blind.
    //
    // The file is already there and this conversation has not looked at it, so
    // the write is refused with what to do instead. Read it, and the same
    // write goes through: what the rule wants is that somebody has SEEN what
    // they are replacing.
    #[tokio::test]
    async fn refuses_to_replace_a_file_this_conversation_has_not_read() {
        let here = folder("unread");
        put(&here.join("theirs.txt"), "work somebody did\n");

        let blind = call(
            write::run,
            json!({ "path": "unread/theirs.txt", "content": "mine", "conversation": 7001 }),
        )
        .await;
        assert!(!blind.ok, "a file nobody read was overwritten");
        assert_eq!(blind.kind, "bad_arguments", "the assistant can correct this");
        assert!(blind.message.contains("has not been read"), "{}", blind.message);
        assert_eq!(
            std::fs::read_to_string(here.join("theirs.txt")).unwrap(),
            "work somebody did\n",
            "a refused write changed the file"
        );

        // Having read it, the same write is fine.
        let looked = call(read::run, json!({ "path": "unread/theirs.txt", "conversation": 7001 })).await;
        assert!(looked.ok, "{}", looked.message);
        let again = call(
            write::run,
            json!({ "path": "unread/theirs.txt", "content": "mine", "conversation": 7001 }),
        )
        .await;
        assert!(again.ok, "{}", again.message);
        assert_eq!(std::fs::read_to_string(here.join("theirs.txt")).unwrap(), "mine");
    }

    // With no conversation the rule cannot be applied, so it does not refuse.
    // A worker or a command line has no read history by construction, and
    // refusing everything they asked for would be a capability lost quietly.
    #[tokio::test]
    async fn with_no_conversation_the_rule_does_not_bite() {
        let here = folder("nobody");
        put(&here.join("file.txt"), "before\n");

        let answer = call(write::run, json!({ "path": "nobody/file.txt", "content": "after" })).await;
        assert!(answer.ok, "{}", answer.message);
        assert_eq!(std::fs::read_to_string(here.join("file.txt")).unwrap(), "after");
    }

    // The memory is per conversation, which is the point of it: reading a file
    // an hour ago in somebody else's chat is not having seen it here.
    #[tokio::test]
    async fn reading_it_in_another_conversation_does_not_count() {
        let here = folder("elsewhere");
        put(&here.join("shared.txt"), "original\n");

        let looked = call(read::run, json!({ "path": "elsewhere/shared.txt", "conversation": 7002 })).await;
        assert!(looked.ok, "{}", looked.message);

        let other = call(
            write::run,
            json!({ "path": "elsewhere/shared.txt", "content": "new", "conversation": 7003 }),
        )
        .await;
        assert!(!other.ok, "another conversation's read let this one write blind");
        assert_eq!(std::fs::read_to_string(here.join("shared.txt")).unwrap(), "original\n");
    }

    // The same file by another name is the same file. Reading `a.txt` and
    // writing `./a.txt` is not a way around the rule, and more importantly is
    // not a false refusal for somebody doing nothing clever.
    #[tokio::test]
    async fn the_same_file_under_another_name_counts_as_read() {
        let here = folder("names");
        put(&here.join("same.txt"), "first\n");

        let looked = call(read::run, json!({ "path": "names/same.txt", "conversation": 7004 })).await;
        assert!(looked.ok, "{}", looked.message);

        let absolute = here.join("same.txt");
        let answer = call(
            write::run,
            json!({ "path": absolute.to_string_lossy(), "content": "second", "conversation": 7004 }),
        )
        .await;
        assert!(answer.ok, "the same file under another name was refused: {}", answer.message);
    }

    // A NEW file is not held to it: there is nothing to lose.
    #[tokio::test]
    async fn a_new_file_needs_no_reading_first() {
        folder("fresh");
        let answer = call(
            write::run,
            json!({ "path": "fresh/new.txt", "content": "x", "conversation": 7005 }),
        )
        .await;
        assert!(answer.ok, "{}", answer.message);
    }

    // And an edit counts as having read it, because it read the file to find
    // the text it replaced.
    #[tokio::test]
    async fn an_edit_counts_as_having_read_it() {
        let here = folder("edited");
        put(&here.join("code.rs"), "fn one() {}\n");

        let edited = call(
            edit::run,
            json!({ "path": "edited/code.rs", "find": "one", "replace": "two", "conversation": 7006 }),
        )
        .await;
        assert!(edited.ok, "{}", edited.message);

        let written = call(
            write::run,
            json!({ "path": "edited/code.rs", "content": "fn three() {}\n", "conversation": 7006 }),
        )
        .await;
        assert!(written.ok, "an edit did not count as having seen the file: {}", written.message);
        let _ = seen::forget_everything;
    }

    #[tokio::test]
    async fn replaces_exact_text() {
        let here = folder("edit");
        put(&here.join("main.go"), "package main\n\nfunc main() {}\n");

        let answer = call(
            edit::run,
            json!({ "path": "edit/main.go", "find": "func main() {}", "replace": "func main() { run() }" }),
        )
        .await;
        assert!(answer.ok, "{}", answer.message);
        assert_eq!(content(&answer)["replacements"], 1);
        assert_eq!(
            std::fs::read_to_string(here.join("main.go")).unwrap(),
            "package main\n\nfunc main() { run() }\n"
        );
    }

    // The one it cannot decide: which of four. Refused with the count, so the
    // assistant can include more of the lines around it and try again.
    #[tokio::test]
    async fn refuses_text_that_appears_more_than_once() {
        let here = folder("edit2");
        put(&here.join("list.txt"), "item\nitem\nitem\n");

        let answer = call(
            edit::run,
            json!({ "path": "edit2/list.txt", "find": "item", "replace": "thing" }),
        )
        .await;
        assert!(!answer.ok);
        assert!(answer.message.contains("3 times"), "{}", answer.message);
        assert_eq!(
            std::fs::read_to_string(here.join("list.txt")).unwrap(),
            "item\nitem\nitem\n",
            "a refused edit changes nothing"
        );

        let every = call(
            edit::run,
            json!({ "path": "edit2/list.txt", "find": "item", "replace": "thing", "all": true }),
        )
        .await;
        assert!(every.ok, "{}", every.message);
        assert_eq!(content(&every)["replacements"], 3);
    }

    #[tokio::test]
    async fn refuses_text_that_is_not_there() {
        let here = folder("edit3");
        put(&here.join("a.txt"), "hello\n");

        let answer = call(
            edit::run,
            json!({ "path": "edit3/a.txt", "find": "goodbye", "replace": "hello" }),
        )
        .await;
        assert!(!answer.ok);
        assert_eq!(answer.kind, "bad_arguments");
        assert!(answer.message.contains("is not there"), "{}", answer.message);
    }

    #[tokio::test]
    async fn finds_files_by_name_and_leaves_out_what_the_project_ignores() {
        let here = folder("find");
        std::fs::create_dir_all(here.join("src")).unwrap();
        std::fs::create_dir_all(here.join("build")).unwrap();
        std::fs::create_dir_all(here.join(".git")).unwrap();
        put(&here.join("src/one.go"), "package one\n");
        put(&here.join("src/two.go"), "package two\n");
        put(&here.join("build/gen.go"), "package gen\n");
        put(&here.join(".git/config.go"), "package git\n");
        put(&here.join(".gitignore"), "build/\n");

        let answer = call(find::run, json!({ "pattern": "**/*.go", "folder": "find" })).await;
        assert!(answer.ok, "{}", answer.message);
        let files = content(&answer)["files"].as_array().unwrap().clone();
        let names: Vec<String> = files.iter().map(|f| f.as_str().unwrap().to_string()).collect();
        assert!(names.iter().any(|n| n.ends_with("src/one.go")), "{names:?}");
        assert!(names.iter().any(|n| n.ends_with("src/two.go")), "{names:?}");
        assert!(!names.iter().any(|n| n.contains("build/")), "the project's ignore file was not honoured: {names:?}");
        assert!(!names.iter().any(|n| n.contains(".git/")), "version control's own folder came back: {names:?}");
    }

    #[tokio::test]
    async fn searches_inside_files_and_says_where() {
        let here = folder("search");
        put(&here.join("a.go"), "package a\n\nfunc Open() {}\n");
        put(&here.join("b.txt"), "open sesame\n");

        let answer = call(
            search::run,
            json!({ "pattern": "func Open", "folder": "search" }),
        )
        .await;
        assert!(answer.ok, "{}", answer.message);
        let matches = content(&answer)["matches"].as_array().unwrap().clone();
        assert_eq!(matches.len(), 1, "{matches:?}");
        assert!(matches[0]["file"].as_str().unwrap().ends_with("a.go"));
        assert_eq!(matches[0]["line"], 3, "the line number is where somebody looks");
        assert_eq!(matches[0]["text"], "func Open() {}");

        // Case, and the glob that narrows where to look.
        let loosely = call(
            search::run,
            json!({ "pattern": "OPEN", "folder": "search", "ignore_case": true }),
        )
        .await;
        assert_eq!(content(&loosely)["count"], 2, "both files say open");

        let narrowed = call(
            search::run,
            json!({ "pattern": "open", "folder": "search", "glob": "*.txt", "ignore_case": true }),
        )
        .await;
        assert_eq!(content(&narrowed)["count"], 1, "the glob did not narrow the search");
    }
}

#[cfg(test)]
mod patching {
    use super::tests::*;
    use super::*;

    /// An edit says which version of the file it was written against.
    ///
    /// Between reading a file and changing it, a build can rewrite it, a
    /// formatter can move every brace, the person can save it in their editor.
    /// An edit that lands on a file that has moved on is how work is lost
    /// quietly: the text it was looking for is still there, in a file that now
    /// says something else around it.
    #[tokio::test]
    async fn an_edit_written_against_an_old_version_is_refused() {
        let here = folder("hashes");
        put(&here.join("shared.py"), "def one():\n    return 1\n");

        let read = call(read::run, json!({ "path": "hashes/shared.py", "conversation": 81 })).await;
        let was = content(&read)["hash"].as_str().unwrap().to_string();
        assert!(!was.is_empty(), "a read must say which version it read");

        // Somebody else changes the file.
        put(&here.join("shared.py"), "def one():\n    return 2\n");

        let refused = call(
            edit::run,
            json!({
                "path": "hashes/shared.py", "find": "return 1", "replace": "return 3",
                "expect_hash": was, "conversation": 81,
            }),
        )
        .await;
        assert!(!refused.ok, "an edit against an old version was applied");
        assert!(refused.message.contains("has changed since"), "{}", refused.message);
        assert_eq!(
            std::fs::read_to_string(here.join("shared.py")).unwrap(),
            "def one():\n    return 2\n",
            "the refused edit changed the file"
        );

        // And the hash the edit ANSWERS with lets the next one follow without
        // reading the file again.
        let read = call(read::run, json!({ "path": "hashes/shared.py", "conversation": 81 })).await;
        let now = content(&read)["hash"].as_str().unwrap().to_string();
        let done = call(
            edit::run,
            json!({
                "path": "hashes/shared.py", "find": "return 2", "replace": "return 3",
                "expect_hash": now, "conversation": 81,
            }),
        )
        .await;
        assert!(done.ok, "{}", done.message);
        let after = content(&done)["hash"].as_str().unwrap().to_string();
        let again = call(
            edit::run,
            json!({
                "path": "hashes/shared.py", "find": "return 3", "replace": "return 4",
                "expect_hash": after, "conversation": 81,
            }),
        )
        .await;
        assert!(again.ok, "the hash an edit answered with was not good enough for the next: {}", again.message);
    }

    /// A file written on Windows is edited by text written anywhere.
    ///
    /// Its lines end with a character that does not print, so an exact match
    /// fails on a file that looks identical in every message about it. The
    /// endings are set aside for matching and KEPT for writing, so the file
    /// does not become a mixture that shows up as every line changed.
    #[tokio::test]
    async fn text_matches_a_file_whose_lines_end_the_windows_way() {
        let here = folder("crlf");
        put(&here.join("app.ts"), "const a = 1;\r\nconst b = 2;\r\nconst c = 3;\r\n");

        let done = call(
            edit::run,
            json!({
                "path": "crlf/app.ts",
                "find": "const b = 2;",
                "replace": "const b = 22;",
                "conversation": 82,
            }),
        )
        .await;
        assert!(done.ok, "an edit failed on a file with Windows line endings: {}", done.message);

        let after = std::fs::read_to_string(here.join("app.ts")).unwrap();
        assert_eq!(
            after, "const a = 1;\r\nconst b = 22;\r\nconst c = 3;\r\n",
            "the file's line endings were not kept"
        );
        assert!(!after.contains("\n\n"), "a stray line appeared");
    }

    /// A multi-line edit on a Windows file, where the replacement is written
    /// with the other kind of ending: it must not leave the file mixed.
    #[tokio::test]
    async fn a_replacement_takes_the_file_s_own_line_endings() {
        let here = folder("crlf2");
        put(&here.join("x.txt"), "one\r\ntwo\r\nthree\r\n");

        let done = call(
            edit::run,
            json!({
                "path": "crlf2/x.txt",
                "find": "two",
                "replace": "two\nand a half",
                "conversation": 83,
            }),
        )
        .await;
        assert!(done.ok, "{}", done.message);
        let after = std::fs::read_to_string(here.join("x.txt")).unwrap();
        assert_eq!(after, "one\r\ntwo\r\nand a half\r\nthree\r\n", "the file became mixed: {after:?}");
    }

    /// Lines by number, which nothing about whitespace can make ambiguous.
    #[tokio::test]
    async fn lines_can_be_replaced_by_number() {
        let here = folder("lines");
        put(&here.join("list.txt"), "one\ntwo\nthree\nfour\n");

        let done = call(
            edit::run,
            json!({
                "path": "lines/list.txt",
                "start_line": 2, "end_line": 3,
                "replace": "TWO\nTHREE",
                "conversation": 84,
            }),
        )
        .await;
        assert!(done.ok, "{}", done.message);
        assert_eq!(content(&done)["replacements"], 2, "it should say how many lines it replaced");
        assert_eq!(
            std::fs::read_to_string(here.join("list.txt")).unwrap(),
            "one\nTWO\nTHREE\nfour\n"
        );

        // One line, with no end_line.
        let one = call(
            edit::run,
            json!({ "path": "lines/list.txt", "start_line": 1, "replace": "ONE", "conversation": 84 }),
        )
        .await;
        assert!(one.ok, "{}", one.message);
        assert_eq!(
            std::fs::read_to_string(here.join("list.txt")).unwrap(),
            "ONE\nTWO\nTHREE\nfour\n"
        );

        // Deleting: a range with nothing in its place.
        let gone = call(
            edit::run,
            json!({ "path": "lines/list.txt", "start_line": 4, "end_line": 4, "replace": "", "conversation": 84 }),
        )
        .await;
        assert!(gone.ok, "{}", gone.message);
        assert_eq!(std::fs::read_to_string(here.join("list.txt")).unwrap(), "ONE\nTWO\nTHREE\n");
    }

    /// A line that is not there is a mistake the assistant can correct.
    #[tokio::test]
    async fn a_line_range_past_the_end_says_so() {
        let here = folder("lines2");
        put(&here.join("short.txt"), "one\ntwo\n");

        let answer = call(
            edit::run,
            json!({ "path": "lines2/short.txt", "start_line": 9, "replace": "x", "conversation": 85 }),
        )
        .await;
        assert!(!answer.ok);
        assert!(answer.message.contains("2 lines"), "{}", answer.message);
        assert_eq!(std::fs::read_to_string(here.join("short.txt")).unwrap(), "one\ntwo\n");
    }

    /// Both ways of saying which part to change is a call to correct, not a
    /// guess to make.
    #[tokio::test]
    async fn text_and_a_line_range_together_are_refused() {
        let here = folder("lines3");
        put(&here.join("a.txt"), "one\ntwo\n");
        let answer = call(
            edit::run,
            json!({
                "path": "lines3/a.txt", "find": "one", "replace": "1",
                "start_line": 1, "conversation": 86,
            }),
        )
        .await;
        assert!(!answer.ok);
        assert!(answer.message.contains("not both"), "{}", answer.message);
    }

    /// And a write can be written against a version too, so an agent that
    /// rewrites a whole file does not lose what somebody else did to it.
    #[tokio::test]
    async fn a_write_against_an_old_version_is_refused() {
        let here = folder("hashes2");
        put(&here.join("notes.md"), "first\n");
        let read = call(read::run, json!({ "path": "hashes2/notes.md", "conversation": 87 })).await;
        let was = content(&read)["hash"].as_str().unwrap().to_string();

        put(&here.join("notes.md"), "somebody else's work\n");
        let refused = call(
            write::run,
            json!({
                "path": "hashes2/notes.md", "content": "mine", "expect_hash": was,
                "conversation": 87,
            }),
        )
        .await;
        assert!(!refused.ok, "a write against an old version was applied");
        assert_eq!(
            std::fs::read_to_string(here.join("notes.md")).unwrap(),
            "somebody else's work\n"
        );
    }
}
