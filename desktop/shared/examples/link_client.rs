//! The real link client, runnable on its own.
//!
//! It exists so the Go side can drive THIS code rather than a fake of it. Every
//! test of the link until now had a Go server talking to a Go stand-in written
//! from the same understanding of the protocol as the server, which proves that
//! understanding is self-consistent and nothing else. The two halves are in
//! different languages, built on different websocket libraries, and what has to
//! agree is exactly the part a shared assumption cannot check: whether an empty
//! binary message survives, whether a refusal parses, whether a dial-back
//! arrives at the ticket it was issued for.
//!
//! Run with the gateway's address and a link token:
//!
//!     SAG_LINK_URL=http://127.0.0.1:8080 SAG_LINK_TOKEN=... cargo run --example link_client
//!
//! It runs until it is killed, which is what the application does too.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;

use std::sync::Mutex;

use sag_desktop::link::Link;

#[tokio::main]
async fn main() {
    let url = std::env::var("SAG_LINK_URL").unwrap_or_else(|_| {
        eprintln!("SAG_LINK_URL is required");
        std::process::exit(2);
    });
    let token = std::env::var("SAG_LINK_TOKEN").unwrap_or_else(|_| {
        eprintln!("SAG_LINK_TOKEN is required");
        std::process::exit(2);
    });

    // How many times it asked for a credential, printed so a test can tell a
    // refusal apart from an ordinary reconnection.
    let asked = Arc::new(AtomicUsize::new(0));
    let counting = Arc::clone(&asked);
    // The page's part, which this example has to stand in for: when the link
    // says it needs a credential, hand it the next one. In the application the
    // page mints it, because the page is the half that is signed in; here it is
    // given, so a gate can drive a real renewal without a browser.
    let next = std::env::var("SAG_LINK_TOKEN_NEXT").ok();
    let renewing: Arc<Mutex<Option<Arc<Link>>>> = Arc::new(Mutex::new(None));
    let handing = Arc::clone(&renewing);
    let renewing_url = url.clone();
    let link = Link::new(
        move || {
            let n = counting.fetch_add(1, Ordering::SeqCst) + 1;
            println!("needs-token {n}");
            if let (Some(token), Some(link)) = (next.clone(), handing.lock().unwrap().clone()) {
                link.start(renewing_url.clone(), token);
            }
        },
        // Printed so a test can watch the route come up and go down from
        // outside, which is also what the window does with it.
        |up| println!("link {}", if up { "up" } else { "down" }),
    );

    // The folder this run may work in, for a gate that drives the terminal. In
    // the application a person picks one with the system's folder dialog; here
    // it is given, because a test cannot open a dialog and should not pretend
    // to.
    if let Ok(dir) = std::env::var("SAG_LINK_STATE") {
        sag_desktop::workspace::use_state_dir(std::path::PathBuf::from(dir));
    }
    if let Ok(folder) = std::env::var("SAG_LINK_FOLDER") {
        if let Err(err) = sag_desktop::workspace::choose(std::path::PathBuf::from(folder)) {
            eprintln!("the folder could not be used: {err}");
        }
    }

    *renewing.lock().unwrap() = Some(Arc::clone(&link));
    link.start(url, token);
    println!("started");

    // Until killed. The application's window is what usually keeps this alive.
    loop {
        tokio::time::sleep(std::time::Duration::from_secs(3600)).await;
    }
}
