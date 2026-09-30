//! Where the browser lives on this computer, and how it gets there.
//!
//! It is not in the installer. A headless Chromium is about a hundred megabytes
//! per architecture and the application ships universal, so carrying it would
//! roughly triple what somebody downloads to get a chat window. It is fetched
//! once, on the first run that has a moment for it, from our own repository.
//!
//! **Ours, not upstream's.** A pinned version stays available when upstream
//! moves its files, an installation on a closed network reaches us rather than
//! the wider internet, and what a customer downloads is a file we have seen.
//! The archives are mirrored UNCHANGED, so the digest we pin can be checked
//! against the one their authors published.
//!
//! **The version is in the path.** `<state>/browser/<version>/` is either
//! complete and current or it is absent: there is no freshness check anywhere
//! here, because staleness has nowhere to live. Publishing a new pin makes a
//! new directory and leaves the old one alone. It is the same reasoning the
//! skills install uses, and it is what makes "look for it, fetch it if it is
//! not there" the whole of the logic.
//!
//! **The digest is compiled in.** It rides in `desktop/browser.json`, inside
//! the signed application, and is deliberately NOT the `.sha256` published
//! beside the download: a hash fetched from the same place as the bytes proves
//! only that the two agree. A repository that has been tampered with serves a
//! different browser and a matching hash for it, and is refused here.

use std::collections::BTreeMap;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::OnceLock;
use std::time::Duration;

use serde::Deserialize;
use sha2::{Digest, Sha256};

/// How long to wait for the first byte before giving up on a download.
///
/// This is the failure that matters and the one worth being impatient about: a
/// repository that is down, a captive portal that swallows the request, a
/// network that is up but goes nowhere. None of them answer, and waiting longer
/// does not make them. The download is abandoned and tried again the next time
/// the application opens, which costs the person nothing because they were
/// never told it was happening.
const FIRST_BYTE: Duration = Duration::from_secs(30);

/// How long a download may go without producing anything before it is
/// abandoned.
///
/// The same failure as above, arriving later: a connection that opens, sends a
/// little, and then stops. Without this, a stalled transfer holds the
/// background task for as long as the operating system keeps the socket, which
/// on a sleeping laptop is a very long time. There is deliberately no ceiling
/// on the TOTAL time, because a hundred megabytes over a slow line is a long
/// download rather than a broken one.
const STALL: Duration = Duration::from_secs(60);

/// The pinned browser, read from the one file both halves of this read.
#[derive(Debug, Deserialize)]
struct Pin {
    chrome: String,
    #[serde(default)]
    archives: BTreeMap<String, Archive>,
}

/// One platform's published archive: what it is called, how big it is, and what
/// it must hash to.
#[derive(Debug, Clone, Deserialize)]
struct Archive {
    name: String,
    bytes: u64,
    sha256: String,
}

/// The pin, parsed once.
///
/// Compiled in with `include_str!` rather than read from disk, so there is no
/// arrangement in which the application is running with a pin somebody edited
/// underneath it, and no file to be missing from a bundle.
fn pin() -> &'static Pin {
    static PIN: OnceLock<Pin> = OnceLock::new();
    PIN.get_or_init(|| {
        serde_json::from_str(include_str!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../browser.json"
        )))
        .expect("desktop/browser.json is not readable, and it is compiled in")
    })
}

/// Which browser this build is pinned to.
pub fn version() -> &'static str {
    &pin().chrome
}

/// What this computer is, in the words the published archives are named with.
///
/// A build knows its own platform at compile time, so this cannot be wrong at
/// run time the way reading it from the operating system could. A platform with
/// no arm here is one we publish nothing for, and it says so rather than
/// guessing at a name.
fn platform() -> Option<&'static str> {
    #[cfg(all(target_os = "macos", target_arch = "aarch64"))]
    {
        Some("mac-arm64")
    }
    #[cfg(all(target_os = "macos", target_arch = "x86_64"))]
    {
        Some("mac-x64")
    }
    #[cfg(all(target_os = "windows", target_arch = "x86_64"))]
    {
        Some("win64")
    }
    #[cfg(not(any(
        all(target_os = "macos", target_arch = "aarch64"),
        all(target_os = "macos", target_arch = "x86_64"),
        all(target_os = "windows", target_arch = "x86_64"),
    )))]
    {
        None
    }
}

/// What this platform's archive is, or why there is not one.
fn archive() -> Result<(&'static str, &'static Archive), String> {
    let platform = platform()
        .ok_or_else(|| "no browser is published for this kind of computer".to_string())?;
    let archive = pin().archives.get(platform).ok_or_else(|| {
        format!(
            "no browser has been published for {platform} at version {}",
            version()
        )
    })?;
    Ok((platform, archive))
}

/// Where this version of the browser lives.
pub fn home(state: &Path) -> PathBuf {
    state.join("browser").join(version())
}

/// The program inside it.
pub fn executable(state: &Path) -> PathBuf {
    let name = if cfg!(windows) {
        "chrome-headless-shell.exe"
    } else {
        "chrome-headless-shell"
    };
    home(state).join(name)
}

/// Whether the browser is already here.
///
/// A file, not a directory: the directory is only renamed into place once every
/// file in it has arrived and hashed, so its existence is the whole check, and
/// this asks about the program because that is what the supervisor needs.
pub fn installed(state: &Path) -> Option<PathBuf> {
    let exe = executable(state);
    exe.is_file().then_some(exe)
}

/// Fetch the browser, and answer with the program.
///
/// Answers at once if it is already here. Otherwise it downloads, checks what
/// arrived against the digest compiled in, unpacks it beside where it will
/// live, and renames it into place. Nothing part-finished is ever visible under
/// the name the supervisor looks for.
pub async fn fetch(state: &Path, base: &str) -> Result<PathBuf, String> {
    if let Some(here) = installed(state) {
        return Ok(here);
    }
    // The platform decided which archive; the unpack reads its own shape.
    let (_, archive) = archive()?;
    let home = home(state);
    let parent = home
        .parent()
        .ok_or_else(|| "the browser has nowhere to live".to_string())?
        .to_path_buf();
    tokio::fs::create_dir_all(&parent)
        .await
        .map_err(|e| format!("the browser's folder could not be made: {e}"))?;

    // Beside the final directory, so the rename that finishes this is on one
    // filesystem and is therefore atomic. A rename across devices is a copy,
    // and a copy can be interrupted halfway, which is the one thing this whole
    // arrangement exists to prevent.
    let staging = parent.join(format!(".{}.part", version()));
    let _ = tokio::fs::remove_dir_all(&staging).await;
    tokio::fs::create_dir_all(&staging)
        .await
        .map_err(|e| format!("the browser's folder could not be made: {e}"))?;

    let url = format!(
        "{}/browser/{}/{}",
        base.trim_end_matches('/'),
        version(),
        archive.name
    );
    let zip = staging.join(&archive.name);
    let outcome = match download(&url, &zip, archive).await {
        Ok(()) => unpacked(zip, staging.join("out")).await,
        Err(why) => Err(why),
    };
    if let Err(why) = outcome {
        let _ = tokio::fs::remove_dir_all(&staging).await;
        return Err(why);
    }

    // The one step that makes it visible. Until it happens there is nothing
    // under the name the supervisor looks for; after it, all of it.
    match tokio::fs::rename(staging.join("out"), &home).await {
        Ok(()) => {}
        Err(_) if home.is_dir() => {
            // Another run installed the same version while this one was
            // working. Theirs is as good as ours by definition: same version,
            // same digest, same bytes.
        }
        Err(err) => {
            let _ = tokio::fs::remove_dir_all(&staging).await;
            return Err(format!("the browser could not be put in place: {err}"));
        }
    }
    let _ = tokio::fs::remove_dir_all(&staging).await;

    installed(state).ok_or_else(|| {
        "the browser was unpacked but its program is not where it should be".to_string()
    })
}

/// download streams the archive to disk, bounded by silence rather than by a
/// total time, and hashes it as it goes.
///
/// Hashed on the way past rather than by reading it back: the file is a hundred
/// megabytes, and reading it twice to learn something we could have counted the
/// first time is a minute of somebody's disk for nothing.
async fn download(url: &str, to: &Path, archive: &Archive) -> Result<(), String> {
    let client = reqwest::Client::builder()
        // The first byte, which is the failure worth being impatient about.
        .connect_timeout(FIRST_BYTE)
        .build()
        .map_err(|e| format!("the downloader could not be built: {e}"))?;

    let answer = tokio::time::timeout(FIRST_BYTE, client.get(url).send())
        .await
        .map_err(|_| format!("{url} did not answer within {}s", FIRST_BYTE.as_secs()))?
        .map_err(|e| format!("the browser could not be fetched: {e}"))?;
    if !answer.status().is_success() {
        return Err(format!(
            "the browser is not published at {url} ({})",
            answer.status()
        ));
    }

    let mut file =
        std::fs::File::create(to).map_err(|e| format!("the download could not be written: {e}"))?;
    let mut hasher = Sha256::new();
    let mut written: u64 = 0;
    let mut answer = answer;
    loop {
        // Each chunk, not the whole transfer. A download that is going slowly
        // is a download; one that has stopped is a failure, and only this can
        // tell them apart.
        let chunk = tokio::time::timeout(STALL, answer.chunk())
            .await
            .map_err(|_| {
                format!(
                    "the download stopped for {}s and was abandoned",
                    STALL.as_secs()
                )
            })?
            .map_err(|e| format!("the download failed: {e}"))?;
        let Some(chunk) = chunk else { break };
        written += chunk.len() as u64;
        if written > archive.bytes {
            return Err(format!(
                "the download is larger than the {} bytes that were published, and was not kept",
                archive.bytes
            ));
        }
        hasher.update(&chunk);
        file.write_all(&chunk)
            .map_err(|e| format!("the download could not be written: {e}"))?;
    }
    file.flush()
        .map_err(|e| format!("the download could not be written: {e}"))?;

    if written != archive.bytes {
        return Err(format!(
            "the download is {written} bytes and {} were published, so it arrived incomplete",
            archive.bytes
        ));
    }
    let got = format!("{:x}", hasher.finalize());
    if !got.eq_ignore_ascii_case(&archive.sha256) {
        return Err(
            "the browser that arrived is not the one that was published, and was not kept"
                .to_string(),
        );
    }
    Ok(())
}

/// unpacked is `unpack`, on the threads kept for work that blocks.
///
/// It is the whole browser through the disk (measured on Windows: 291 files,
/// 270 megabytes), and on the runtime's own threads it held one of them for as
/// long as that took. The link's control socket runs on those threads and
/// has to answer the gateway's heartbeat meanwhile: a thread held long enough
/// is a computer the gateway decides has gone, which is what a large search
/// did through the file tools (KB/29). This is the same work, moved.
async fn unpacked(zip: PathBuf, into: PathBuf) -> Result<(), String> {
    tokio::task::spawn_blocking(move || unpack(&zip, &into))
        .await
        .unwrap_or_else(|_| Err("the browser could not be unpacked".to_string()))
}

/// unpack expands the archive into `into`, without its top level directory.
///
/// The published archive holds everything inside one folder named for the
/// platform (`chrome-headless-shell-win64/`, and so on), which would put the
/// program at a different path on every kind of computer. Stripping it means
/// the supervisor looks in one place.
///
/// The name of that folder is READ FROM THE ARCHIVE rather than built from the
/// platform. Building it means two places have to agree about a string nobody
/// controls, and the day upstream renames a folder the unpack silently keeps
/// nothing: every entry fails the prefix check, is skipped as "not under the
/// expected folder", and a complete-looking empty directory is renamed into
/// place. Reading it cannot disagree with anything.
fn unpack(zip: &Path, into: &Path) -> Result<(), String> {
    let file =
        std::fs::File::open(zip).map_err(|e| format!("the download could not be read: {e}"))?;
    let mut archive =
        zip::ZipArchive::new(file).map_err(|e| format!("the download is not readable: {e}"))?;
    std::fs::create_dir_all(into)
        .map_err(|e| format!("the browser's folder could not be made: {e}"))?;

    let prefix = one_root(&mut archive)?;
    for index in 0..archive.len() {
        let mut entry = archive
            .by_index(index)
            .map_err(|e| format!("the download is not readable: {e}"))?;

        // The name the ARCHIVE gives, held to our own rule rather than trusted.
        // `enclosed_name` is the zip crate's own refusal of absolute paths, of
        // `..`, and of the Windows shapes that look relative and are not. A
        // name it will not vouch for is not written anywhere.
        let Some(name) = entry.enclosed_name() else {
            return Err("the download holds a file with a name that is not a path".to_string());
        };
        let Ok(inside) = name.strip_prefix(&prefix) else {
            // Anything not under the one expected folder. Not an error worth
            // failing the install for (upstream adds top level notices), but
            // not something to write either.
            continue;
        };
        let target = into.join(inside);
        if entry.is_dir() {
            std::fs::create_dir_all(&target)
                .map_err(|e| format!("{} could not be made: {e}", inside.display()))?;
            continue;
        }
        if let Some(folder) = target.parent() {
            std::fs::create_dir_all(folder)
                .map_err(|e| format!("{} could not be made: {e}", inside.display()))?;
        }
        let mut out = std::fs::File::create(&target)
            .map_err(|e| format!("{} could not be written: {e}", inside.display()))?;
        std::io::copy(&mut entry, &mut out)
            .map_err(|e| format!("{} could not be written: {e}", inside.display()))?;

        // The executable bit, which a zip carries and which matters: a browser
        // that cannot be run is a capability that silently does not exist. The
        // personal shell sets it by hand on its own payload for the same reason.
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            if let Some(mode) = entry.unix_mode() {
                let _ = std::fs::set_permissions(&target, std::fs::Permissions::from_mode(mode));
            }
        }
    }
    Ok(())
}

/// one_root is the single folder everything in the archive sits under.
///
/// Asserted rather than assumed: an archive with two roots, or none, is not the
/// shape this unpacks, and finding that out here is a refusal with a reason
/// instead of a directory that looks installed and holds half a browser.
fn one_root(archive: &mut zip::ZipArchive<std::fs::File>) -> Result<String, String> {
    let mut roots = std::collections::BTreeSet::new();
    for index in 0..archive.len() {
        let entry = archive
            .by_index(index)
            .map_err(|e| format!("the download is not readable: {e}"))?;
        let Some(name) = entry.enclosed_name() else {
            return Err("the download holds a file with a name that is not a path".to_string());
        };
        if let Some(root) = name.components().next() {
            roots.insert(root.as_os_str().to_string_lossy().into_owned());
        }
    }
    match roots.len() {
        1 => Ok(format!("{}/", roots.into_iter().next().unwrap_or_default())),
        0 => Err("the download is empty".to_string()),
        n => Err(format!(
            "the download holds {n} top level folders and this expects one; \
             it is not the archive this knows how to unpack"
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Build a zip with the given entries, for the unpack tests.
    fn a_zip(at: &Path, entries: &[(&str, &str)]) {
        let file = std::fs::File::create(at).expect("create the zip");
        let mut writing = zip::ZipWriter::new(file);
        let how = zip::write::SimpleFileOptions::default()
            .compression_method(zip::CompressionMethod::Deflated);
        for (name, body) in entries {
            writing.start_file(*name, how).expect("start the entry");
            writing.write_all(body.as_bytes()).expect("write the entry");
        }
        writing.finish().expect("finish the zip");
    }

    fn open(at: &Path) -> zip::ZipArchive<std::fs::File> {
        zip::ZipArchive::new(std::fs::File::open(at).expect("open")).expect("read")
    }

    /// The pin is readable, and it names a browser and the archive for this
    /// computer.
    ///
    /// It is compiled in, so a pin that cannot be parsed is a panic on first
    /// use rather than a compile error, which is the kind of failure that ships.
    #[test]
    fn the_pin_names_a_browser_for_this_computer() {
        assert!(!version().is_empty(), "the pin names no browser version");
        let (platform, archive) = archive().expect("this computer has a published browser");
        assert!(
            archive.name.contains(platform),
            "{} is not named for {platform}",
            archive.name
        );
        assert_eq!(archive.sha256.len(), 64, "the digest is not a sha256");
        assert!(
            archive.bytes > 1_000_000,
            "the published size is implausible"
        );
    }

    /// Every platform the pin publishes for is described the same way.
    ///
    /// Checkable from ANY machine, which is the point: the Windows entry is
    /// exercised by nobody on a Mac, and a typo in its name or a digest pasted
    /// short would otherwise be found by the first person who installed on
    /// Windows. The naming convention is the one the download URL is built
    /// from, so a name that does not follow it is a 404 at first run.
    #[test]
    fn every_published_platform_is_described_the_same_way() {
        let archives = &pin().archives;
        for platform in ["mac-arm64", "mac-x64", "win64"] {
            let archive = archives
                .get(platform)
                .unwrap_or_else(|| panic!("nothing is published for {platform}"));
            assert_eq!(
                archive.name,
                format!("chrome-headless-shell-{platform}.zip"),
                "{platform} is published under a name the download URL would not build"
            );
            assert_eq!(
                archive.sha256.len(),
                64,
                "{platform} has a digest that is not a sha256"
            );
            assert!(
                archive.sha256.chars().all(|c| c.is_ascii_hexdigit()),
                "{platform} has a digest that is not hexadecimal"
            );
            assert!(
                archive.bytes > 50_000_000,
                "{platform} is published as {} bytes, which is too small to be a browser",
                archive.bytes
            );
        }
    }

    /// The version is in the path, which is what makes staleness impossible.
    #[test]
    fn where_it_lives_is_named_for_the_version() {
        let state = Path::new("/tmp/state");
        let home = home(state);
        assert!(
            home.to_string_lossy().contains(version()),
            "the browser's folder does not carry its version: {}",
            home.display()
        );
        assert!(executable(state).starts_with(&home));
    }

    /// Nothing is installed until the program is actually there.
    ///
    /// The distinction matters: the directory appears at the moment of the
    /// rename, and if this asked about the directory instead, a future change
    /// that created it earlier would make a half-finished install look done.
    #[test]
    fn a_folder_without_the_program_is_not_an_install() {
        let state = std::env::temp_dir().join(format!("sag-bin-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&state);
        std::fs::create_dir_all(home(&state)).expect("make the folder");
        assert!(
            installed(&state).is_none(),
            "an empty folder was read as an installed browser"
        );
        std::fs::write(executable(&state), b"#!/bin/sh\n").expect("write");
        assert!(
            installed(&state).is_some(),
            "the program is there and was not found"
        );
        let _ = std::fs::remove_dir_all(&state);
    }

    /// A browser that is already here is not fetched again.
    ///
    /// Every launch after the first takes this path, so getting it wrong means
    /// a hundred megabyte download every time somebody opens the application.
    /// Nothing would report that: the download succeeds, the same bytes land in
    /// the same place, and the only sign is somebody's data allowance.
    ///
    /// The guard belongs HERE rather than in the caller, and that is what this
    /// test is really pinning. The caller checks too, but only to decide
    /// whether to say it is fetching; a test pointed at the caller passed with
    /// its check deleted, because this one caught it anyway. The control that
    /// does fail is removing the early return below.
    ///
    /// Proved by TIME, against a black hole address: a fetch that actually went
    /// looking would sit there for the full thirty second first-byte timeout,
    /// so finishing at once means it never asked.
    #[tokio::test]
    async fn a_browser_already_here_is_not_fetched_again() {
        let state = std::env::temp_dir().join(format!("sag-refetch-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&state);
        std::fs::create_dir_all(home(&state)).expect("make the folder");
        std::fs::write(executable(&state), b"#!/bin/sh\n").expect("write");

        let began = std::time::Instant::now();
        let found = fetch(&state, "http://198.51.100.1:9")
            .await
            .expect("a browser already here needs no repository at all");
        let took = began.elapsed();

        assert_eq!(found, executable(&state));
        assert!(
            took < Duration::from_secs(5),
            "it took {took:?} with the browser already installed, which is long enough to have \
             gone looking for one: it downloads on every launch"
        );
        let _ = std::fs::remove_dir_all(&state);
    }

    /// The folder everything sits under is read from the archive.
    ///
    /// It used to be built from the platform name, and the failure that would
    /// have caused is worth stating: upstream renames the folder, every entry
    /// fails the prefix check, every entry is skipped as "not under the
    /// expected folder", and an EMPTY directory is renamed into place looking
    /// exactly like a finished install. Reading it cannot disagree with
    /// anything.
    #[test]
    fn the_archives_own_root_is_what_is_stripped() {
        let dir = std::env::temp_dir().join(format!("sag-unzip-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("make the directory");

        // A root nothing here could have guessed, which is the point.
        let zipped = dir.join("odd.zip");
        a_zip(
            &zipped,
            &[
                (
                    "something-upstream-renamed/chrome-headless-shell",
                    "#!/bin/sh\n",
                ),
                ("something-upstream-renamed/resources/x.pak", "data"),
            ],
        );
        assert_eq!(
            one_root(&mut open(&zipped)).expect("one root"),
            "something-upstream-renamed/"
        );

        let into = dir.join("out");
        unpack(&zipped, &into).expect("it should unpack");
        assert!(
            into.join("chrome-headless-shell").is_file(),
            "the program is not at the top of the unpacked folder"
        );
        assert!(
            into.join("resources/x.pak").is_file(),
            "a nested file is missing"
        );

        let _ = std::fs::remove_dir_all(&dir);
    }

    /// An archive that is not this shape is refused, rather than unpacked into
    /// something that looks finished.
    #[test]
    fn an_archive_with_two_roots_or_none_is_refused() {
        let dir = std::env::temp_dir().join(format!("sag-unzip-bad-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("make the directory");

        let two = dir.join("two.zip");
        a_zip(&two, &[("one/a", "a"), ("two/b", "b")]);
        let why = one_root(&mut open(&two)).expect_err("two roots is not this shape");
        assert!(why.contains("2 top level folders"), "{why}");

        let none = dir.join("none.zip");
        a_zip(&none, &[]);
        assert!(
            one_root(&mut open(&none)).is_err(),
            "an empty archive was accepted"
        );

        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Unpacking the browser must not hold the thread the link answers the
    /// gateway on (KB/29).
    ///
    /// `ticks_while` counts what the runtime managed to do meanwhile, on the one
    /// thread `#[tokio::test]` runs on: an unpack done there stops it dead, and
    /// one on the blocking pool does not stop it at all. The archive is big
    /// enough that unpacking it takes a while.
    #[tokio::test]
    async fn unpacking_leaves_the_thread_the_link_runs_on_free() {
        let dir = std::env::temp_dir().join(format!("sag-unzip-free-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("make the directory");

        let zipped = dir.join("browser.zip");
        let part = "a part of a browser ".repeat(50_000);
        let names: Vec<String> = (0..24)
            .map(|n| format!("chrome-headless-shell/part{n}.pak"))
            .collect();
        let entries: Vec<(&str, &str)> = names
            .iter()
            .map(|name| (name.as_str(), part.as_str()))
            .collect();
        a_zip(&zipped, &entries);

        let into = dir.join("out");
        let (unpacked_it, ticks) =
            crate::tools::ticks_while(unpacked(zipped.clone(), into.clone())).await;
        unpacked_it.expect("it should unpack");
        assert!(
            into.join("part23.pak").is_file(),
            "the archive was not unpacked, so this proves nothing"
        );
        assert!(
            ticks > 0,
            "the unpack held the runtime's only thread for all of its work, so the link could not have answered the gateway meanwhile"
        );

        let _ = std::fs::remove_dir_all(&dir);
    }
}
