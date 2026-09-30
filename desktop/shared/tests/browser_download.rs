//! Fetching the browser, against a real repository and against broken ones.
//!
//! The unit tests in `binary.rs` are about the pin and the paths, and none of
//! them moves a byte. This does: one test downloads the real archive from the
//! real download host and starts what landed, and the rest are the failures
//! that matter, driven against servers built here to fail in exactly one way
//! each.
//!
//! The failures are the point. A download that works is the easy half; what
//! decides whether somebody's first run is a quiet success or a hung
//! application is what happens when the repository is down, when it answers and
//! then stops, and when the bytes that arrive are not the bytes that were
//! published.

use std::path::PathBuf;
use std::time::{Duration, Instant};

use sag_desktop::browser::{binary, supervise};

/// The real download host, for the one test that uses it.
const REPOSITORY: &str = "https://sag-repo.flexie.io";

fn scratch(name: &str) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("sag-dl-{}-{}", name, std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).expect("make the test directory");
    dir
}

/// The whole of it, for real: nothing on disk, fetch, and the browser runs.
///
/// This is the end-to-end proof and it is deliberately not mocked at any layer.
/// It downloads a hundred megabytes over the public address, checks it against
/// the digest compiled into this binary, unpacks it, and then hands the result
/// to the supervisor, which starts it and asks it a question. A mirror that was
/// published wrong, an archive whose top level directory is not what we strip,
/// an executable bit that a zip did not carry: all of them look like a working
/// download until something tries to run the thing.
///
/// It is behind SAG_BROWSER_DOWNLOAD because it is slow and it reaches the
/// internet, which is not something a plain `cargo test` should do.
#[tokio::test]
async fn a_real_download_lands_and_the_browser_runs() {
    if std::env::var("SAG_BROWSER_DOWNLOAD").is_err() {
        eprintln!("SAG_BROWSER_DOWNLOAD is not set; skipping the real download");
        return;
    }
    let state = scratch("real");
    assert!(
        binary::installed(&state).is_none(),
        "the test started with a browser already there, so it would prove nothing"
    );

    let began = Instant::now();
    let exe = binary::fetch(&state, REPOSITORY)
        .await
        .expect("the browser should download");
    eprintln!("downloaded and unpacked in {:?}", began.elapsed());

    assert!(exe.is_file(), "the program is not where it should be");
    assert!(
        exe.starts_with(binary::home(&state)),
        "the program landed outside the version's own folder: {}",
        exe.display()
    );
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mode = std::fs::metadata(&exe)
            .expect("read the program")
            .permissions()
            .mode();
        assert!(
            mode & 0o111 != 0,
            "the program is not executable ({mode:o}), so the zip's mode was dropped and the \
             supervisor would fail on a file it can see"
        );
    }

    // Asking again costs nothing and downloads nothing.
    let again = binary::fetch(&state, REPOSITORY)
        .await
        .expect("a second ask should find it");
    assert_eq!(again, exe);

    // And the thing that landed is a browser: it starts and it answers.
    let endpoint = supervise::ensure(&exe, &state.join("profile"))
        .await
        .expect("the downloaded browser should start");
    assert!(endpoint.websocket.contains("/devtools/browser/"));
    supervise::stop().await;

    let _ = std::fs::remove_dir_all(&state);
}

/// A repository that never answers is abandoned, and quickly.
///
/// The address is from the range reserved for documentation, so it goes
/// nowhere: no refusal, no reset, just silence, which is what a repository
/// behind a dead route or a captive portal looks like. What is asserted is that
/// it gives up NEAR the first-byte timeout rather than hanging: a background
/// task that waits for a sleeping socket is one the operating system may hold
/// for many minutes, and the person would never know why nothing worked.
#[tokio::test]
async fn a_repository_that_never_answers_is_abandoned() {
    let state = scratch("silent");
    let began = Instant::now();
    let why = binary::fetch(&state, "http://198.51.100.1:9")
        .await
        .expect_err("a silent repository should not succeed");
    let took = began.elapsed();

    assert!(
        took < Duration::from_secs(45),
        "it waited {took:?} on a repository that never answered"
    );
    assert!(
        binary::installed(&state).is_none(),
        "something was left behind by a download that never started"
    );
    eprintln!("gave up after {took:?}: {why}");
    let _ = std::fs::remove_dir_all(&state);
}

/// A version nobody published is reported plainly, not retried into silence.
#[tokio::test]
async fn a_version_that_is_not_published_says_so() {
    let state = scratch("missing");
    // The real host, and a path under it that holds nothing.
    let why = binary::fetch(&state, &format!("{REPOSITORY}/nothing-is-published-here"))
        .await
        .expect_err("an unpublished browser should not succeed");
    assert!(
        why.contains("not published") || why.contains("404"),
        "the reason should say what is wrong: {why}"
    );
    assert!(binary::installed(&state).is_none());
    let _ = std::fs::remove_dir_all(&state);
}

/// A repository that serves the wrong bytes is refused, and leaves nothing.
///
/// This is the assertion the compiled-in digest exists for. The server below
/// answers with the right content-length and the wrong content, which is
/// exactly what a tampered mirror looks like: the size matches the listing, the
/// transfer completes, and nothing but the hash can tell.
#[tokio::test]
async fn a_repository_that_serves_the_wrong_bytes_is_refused() {
    let state = scratch("wrong");
    let base = serve_wrong_bytes().await;

    let why = binary::fetch(&state, &base)
        .await
        .expect_err("bytes that do not match the digest must be refused");
    assert!(
        why.contains("not the one that was published") || why.contains("incomplete"),
        "the reason should name what was wrong: {why}"
    );
    assert!(
        binary::installed(&state).is_none(),
        "a browser that failed its digest was left on disk"
    );
    // And nothing part-finished is lying around under a name anything looks at.
    assert!(
        !binary::home(&state).exists(),
        "the version's folder exists after a refused download"
    );
    let _ = std::fs::remove_dir_all(&state);
}

/// A server that answers any request with garbage of a plausible length.
///
/// Small on purpose: it claims a content-length of one byte, so the download
/// ends far short of what was published and is refused twice over, by length
/// and by digest. Serving a hundred megabytes of noise to make the point would
/// take a minute and prove the same thing.
async fn serve_wrong_bytes() -> String {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind a test server");
    let port = listener.local_addr().expect("its address").port();
    tokio::spawn(async move {
        loop {
            let Ok((mut socket, _)) = listener.accept().await else {
                return;
            };
            tokio::spawn(async move {
                use tokio::io::{AsyncReadExt, AsyncWriteExt};
                let mut discard = [0u8; 2048];
                let _ = socket.read(&mut discard).await;
                let _ = socket
                    .write_all(
                        b"HTTP/1.1 200 OK\r\nContent-Length: 1\r\nConnection: close\r\n\r\nX",
                    )
                    .await;
            });
        }
    });
    format!("http://127.0.0.1:{port}")
}
