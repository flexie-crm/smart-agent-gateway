//! A browser that is running, and a page per agent.
//!
//! What the tools reach for. Everything under this is machinery: starting the
//! browser, speaking its protocol, putting Playwright's script into a page.
//!
//! **A page belongs to an agent, not to a conversation.** This is the lesson
//! the terminal learned the expensive way (KB/39): a background agent runs with
//! its Gateway's session id, so keying on the conversation puts a Gateway and
//! every agent it started onto one page, and the second one to ask navigates
//! the first away mid-read. The gateway sends an identity that is already
//! per-agent, and that is what keys this.
//!
//! **The connection is shared and the pages are not.** One websocket carries
//! every page, because the protocol flattens sessions onto it; opening one per
//! page would be a socket per agent for no gain.
//!
//! **An agent has TABS, one of which is current.** Playwright's `browser_tabs`
//! is what made this a list rather than a single page: every other action works
//! on the current one, so the common case is unchanged and nothing outside this
//! file had to learn that tabs exist.
//!
//! **There are only ever ten tabs, and that is the load-bearing rule here.** A
//! tab is not bookkeeping, it is a process: measured on the browser we ship, an
//! idle one costs 83 MB and each tab adds about 64 MB, and that was near-blank
//! pages, so it is a floor rather than an estimate. An agent that starts a
//! fleet of a hundred would otherwise leave a hundred tabs open doing nothing,
//! which is six gigabytes at best.
//!
//! Every other way of cleaning up is a SIGNAL THAT MIGHT NOT ARRIVE: the model
//! may not call close, an agent may crash, the gateway may restart. A cap is
//! not a signal. It is checked when a tab is opened, so the eleventh closes the
//! least recently used one and the build-up cannot happen rather than being
//! tidied up afterwards. The idle sweep below is the same rule at rest, giving
//! memory back when nothing is going on.
//!
//! **A leased tab is never evicted.** A tab handed to a running call is busy
//! until the lease is dropped, and the drop runs whether the call returned or
//! panicked, so "do not close a tab somebody is using" is a property of the
//! type rather than a rule somebody has to remember.

use std::collections::{HashMap, HashSet};
use std::sync::Mutex as Sync;
use std::time::{Duration, Instant};

use tokio::sync::Mutex;

use super::cdp::Connection;
use super::page::Page;
use super::{binary, profile, supervise};

/// How many tabs may be open at once, across every agent.
///
/// Ten, from the arithmetic above and from what an agent actually does: two or
/// three tabs at a time is the working shape, so ten is several agents' worth
/// and an eleventh is a sign that something has stopped tidying up after
/// itself rather than that somebody needs one more.
const MOST_TABS: usize = 10;

/// How long a tab may sit untouched before it is closed.
///
/// The cap keeps the worst case bounded; this is what gives memory back in the
/// ordinary case, where nothing is wrong and a conversation has simply moved
/// on. Five minutes is long enough that somebody reading a result and asking a
/// follow-up keeps their page, and short enough that a finished fleet is not
/// still resident at lunchtime.
const IDLE_FOR: Duration = Duration::from_secs(5 * 60);

/// How often the idle sweep looks.
const SWEEP_EVERY: Duration = Duration::from_secs(30);

/// The connection to the browser, and which browser it is.
///
/// The address is kept beside it because a browser that was restarted (it
/// crashed, the supervisor replaced it) listens somewhere new with a new token,
/// and a connection to the old one is a connection to nothing. Comparing
/// addresses is how that is noticed without asking.
struct Held {
    at: String,
    connection: Connection,
}

static HELD: Mutex<Option<Held>> = Mutex::const_new(None);

/// The tabs one agent has open, and which of them it is looking at.
///
/// `current` is always a valid position while there is at least one tab: the
/// places that remove a tab are what keep it so, because an index left
/// pointing past the end is a panic waiting for whoever opens the next page.
struct Tabs {
    open: Vec<Slot>,
    current: usize,
}

/// One open tab, and what the pool needs to know about it.
struct Slot {
    page: Page,
    /// Where it is. Remembered rather than asked for, because the one thing it
    /// is needed for (never holding the same address twice) is checked on every
    /// navigation, and asking ten tabs where they are would be ten round trips
    /// to answer a question we already knew the answer to.
    url: String,
    /// When it was last handed to a call. What "least recently used" reads.
    used: Instant,
    /// Handed out and not yet given back. Never evicted while this is true.
    busy: bool,
}

/// Every tab, by the agent that owns it.
///
/// A PLAIN mutex rather than the async one, deliberately. Nothing under this
/// lock talks to the browser: opening and closing a page happen with the lock
/// let go, and what is held is a few milliseconds of bookkeeping. That is what
/// lets a lease clear its own busy flag when it is dropped, which cannot be
/// async and is the whole reason a busy tab can never be evicted.
static TABS: Sync<Option<Registry>> = Sync::new(None);

#[derive(Default)]
struct Registry {
    owners: HashMap<i64, Tabs>,
    /// Tabs being opened right now, counted so two callers arriving together
    /// cannot both decide there is room for one more.
    opening: usize,
    /// Agents whose tab was closed to make room while they were away. Read
    /// once, by the call that finds out.
    replaced: HashSet<i64>,
    /// Sites whose stored sign-in has been put back already.
    ///
    /// Once per site per BROWSER, not per process, and the difference is the
    /// whole of it: after a site has been visited the live browser holds the
    /// truth, so restoring an older jar over a token it refreshed would put the
    /// session backwards. A new browser holds nothing, so this is cleared with
    /// the pages when one replaces another.
    restored: HashSet<String>,
}

fn registry<T>(with: impl FnOnce(&mut Registry) -> T) -> T {
    let mut held = TABS.lock().unwrap_or_else(|e| e.into_inner());
    with(held.get_or_insert_with(Registry::default))
}

/// A tab, borrowed for the length of one call.
///
/// Dropping it gives the tab back. That is the entire mechanism behind "never
/// evict a tab somebody is using": the flag is cleared by the drop, the drop
/// runs on the way out of a panic as well as a return, and there is no way to
/// hold a page without holding one of these.
pub struct Lease {
    page: Page,
    owner: i64,
    /// True when this agent's previous tab had been closed to make room, so
    /// the page it is holding is a fresh blank one rather than where it was.
    replaced: bool,
}

impl Lease {
    /// Whether the page this is holding is a new blank one because the old tab
    /// was evicted.
    pub fn replaced(&self) -> bool {
        self.replaced
    }
}

impl std::ops::Deref for Lease {
    type Target = Page;
    fn deref(&self) -> &Page {
        &self.page
    }
}

impl Drop for Lease {
    fn drop(&mut self) {
        let owner = self.owner;
        let session = self.page.session().to_string();
        registry(|all| {
            if let Some(tabs) = all.owners.get_mut(&owner) {
                for slot in &mut tabs.open {
                    if slot.page.session() == session {
                        slot.busy = false;
                        slot.used = Instant::now();
                    }
                }
            }
        });
    }
}

/// A connection to the running browser, starting it if it is not up.
///
/// The browser is normally already running: the application starts it when it
/// opens. This is for the call that arrives before that finished, and for the
/// one after a crash.
pub async fn connection() -> Result<Connection, String> {
    // Started here because this is what every path to a tab goes through, and
    // a sweeper that is never started is a cap with no floor under it.
    sweep_idle_tabs();

    let state = crate::workspace::state_dir()
        .ok_or("this installation does not know where it keeps its files")?;
    // Said with the reason, because the two cases want different things from
    // whoever reads it: a browser that has not finished downloading will be
    // there shortly, and one that failed will not be until the next launch.
    let exe = binary::installed(&state).ok_or(
        "the browser is not on this computer yet. It downloads itself the first time the \
         application runs; if this persists, the download did not finish.",
    )?;
    let endpoint = supervise::ensure(&exe, &profile(&state)).await?;

    let mut held = HELD.lock().await;
    if let Some(open) = held.as_ref() {
        if open.at == endpoint.websocket {
            return Ok(open.connection.clone());
        }
        // A different address means a different browser: the one we were
        // talking to is gone. Its pages went with it, and holding them would
        // mean handing out sessions that answer nothing.
        forget_pages();
    }
    let connection = Connection::open(&endpoint.websocket).await?;
    *held = Some(Held {
        at: endpoint.websocket,
        connection: connection.clone(),
    });
    Ok(connection)
}

/// The page this caller is working in, opening one the first time.
///
/// `owner` is the identity the gateway sent: per running agent, not per
/// conversation. See the note at the top of this file.
///
/// The tab comes back LEASED, and is not evictable until that lease is
/// dropped. A caller that holds one across several protocol round trips is
/// exactly the caller whose tab must not be taken, which is why this is a type
/// and not a flag.
pub async fn page(owner: i64) -> Result<Lease, String> {
    let connection = connection().await?;

    // Already have one? Take it, and say whether the last one was evicted.
    if let Some(lease) = registry(|all| claim(all, owner)) {
        return Ok(lease);
    }

    // Room for another, or is somebody else's going to have to go? Decided
    // under the lock, acted on outside it.
    //
    // Three outcomes and they must stay three. The first cut folded "there was
    // room" and "nothing could be evicted" into one None, so reserving a slot
    // successfully was read as failing to find one and the tenth tab was
    // refused. The fleet test caught it on its first run.
    let evicting = registry(make_room)?;
    if let Some(Evicted { page }) = evicting {
        // Closing talks to the browser, so it happens with the lock let go.
        let _ = page.close().await;
    }

    let opened = Page::open(&connection).await;
    let fresh = match opened {
        Ok(page) => page,
        Err(why) => {
            registry(|all| all.opening = all.opening.saturating_sub(1));
            return Err(why);
        }
    };

    Ok(registry(|all| {
        all.opening = all.opening.saturating_sub(1);
        let tabs = all.owners.entry(owner).or_insert_with(|| Tabs {
            open: Vec::new(),
            current: 0,
        });
        tabs.open.push(Slot {
            page: fresh.clone(),
            url: String::new(),
            used: Instant::now(),
            busy: true,
        });
        tabs.current = tabs.open.len() - 1;
        Lease {
            page: fresh,
            owner,
            replaced: all.replaced.remove(&owner),
        }
    }))
}

/// Reserve room for one more tab, evicting if that is what it takes.
///
/// `Ok(None)` there was room. `Ok(Some(..))` room was made and this tab has to
/// be closed. `Err` every tab is mid-call, so there is nothing to take and
/// opening anyway is how a hundred agents end up with a hundred tabs.
///
/// In every Ok case the slot is RESERVED (`opening`), so two callers arriving
/// together cannot both be told there is room for one more.
fn make_room(all: &mut Registry) -> Result<Option<Evicted>, String> {
    if counted(all) + all.opening < MOST_TABS {
        all.opening += 1;
        return Ok(None);
    }
    match take_idlest(all) {
        Some(taken) => {
            all.opening += 1;
            Ok(Some(taken))
        }
        None => Err(format!(
            "all {MOST_TABS} browser tabs are in use by other work right now. Try again in a \
             moment, or close a page you have finished with."
        )),
    }
}

/// Take this owner's current tab, if it has one that is free.
fn claim(all: &mut Registry, owner: i64) -> Option<Lease> {
    // The flag is taken LAST, once there is certainly a lease to carry it.
    // Taking it first and then finding no tab loses it, and the agent whose
    // page was closed is handed a blank one with no explanation, which is
    // exactly the outcome the flag exists to prevent.
    let tabs = all.owners.get_mut(&owner)?;
    let slot = tabs.open.get_mut(tabs.current)?;
    // Busy means this agent already has a call running on this page. It cannot
    // happen through the gateway, which runs one tool call per agent at a
    // time, so handing the same page to both would be a bug somewhere else
    // rather than something to serialise here.
    if slot.busy {
        return None;
    }
    slot.busy = true;
    slot.used = Instant::now();
    let page = slot.page.clone();
    Some(Lease {
        page,
        owner,
        replaced: all.replaced.remove(&owner),
    })
}

/// How many tabs are open, across everybody.
fn counted(all: &Registry) -> usize {
    all.owners.values().map(|tabs| tabs.open.len()).sum()
}

/// A tab taken out of the registry, waiting to be closed.
struct Evicted {
    page: Page,
}

/// Take the least recently used tab that nobody is using.
///
/// Busy tabs are skipped rather than waited for, so this answers None when
/// every tab is mid-call and the caller can say so instead of stalling.
fn take_idlest(all: &mut Registry) -> Option<Evicted> {
    let mut oldest: Option<(i64, usize, Instant)> = None;
    for (owner, tabs) in all.owners.iter() {
        for (at, slot) in tabs.open.iter().enumerate() {
            if slot.busy {
                continue;
            }
            if oldest.is_none_or(|(_, _, when)| slot.used < when) {
                oldest = Some((*owner, at, slot.used));
            }
        }
    }
    let (owner, at, _) = oldest?;
    let page = drop_tab(all, owner, at)?;
    // Said to the agent that lost it, once, by the next call it makes. A blank
    // page with no explanation is the confusing outcome; being told the page
    // was closed and to navigate again is a recoverable one.
    all.replaced.insert(owner);
    Some(Evicted { page })
}

/// Remove one tab from an owner, keeping `current` pointing at a real tab.
fn drop_tab(all: &mut Registry, owner: i64, at: usize) -> Option<Page> {
    let tabs = all.owners.get_mut(&owner)?;
    if at >= tabs.open.len() {
        return None;
    }
    let slot = tabs.open.remove(at);
    if tabs.current > at || tabs.current >= tabs.open.len() {
        tabs.current = tabs.current.saturating_sub(1);
    }
    if tabs.open.is_empty() {
        all.owners.remove(&owner);
    }
    Some(slot.page)
}

/// Every tab this caller has open, and which one is current.
///
/// Cloned out rather than handed a lock, because the caller then asks each page
/// its title and address, and holding this lock across those round trips would
/// stop every other agent opening a page.
pub async fn tabs(owner: i64) -> (Vec<Page>, usize) {
    registry(|all| match all.owners.get(&owner) {
        Some(tabs) => (
            tabs.open.iter().map(|slot| slot.page.clone()).collect(),
            tabs.current,
        ),
        None => (Vec::new(), 0),
    })
}

/// Open another tab and make it the current one.
///
/// Held to the same cap as everything else: a model that asks for tab after tab
/// is exactly the case the cap exists for.
pub async fn new_tab(owner: i64) -> Result<Page, String> {
    let connection = connection().await?;

    let evicting = registry(make_room)?;
    if let Some(Evicted { page }) = evicting {
        let _ = page.close().await;
    }

    let fresh = match Page::open(&connection).await {
        Ok(page) => page,
        Err(why) => {
            registry(|all| all.opening = all.opening.saturating_sub(1));
            return Err(why);
        }
    };
    registry(|all| {
        all.opening = all.opening.saturating_sub(1);
        let tabs = all.owners.entry(owner).or_insert_with(|| Tabs {
            open: Vec::new(),
            current: 0,
        });
        tabs.open.push(Slot {
            page: fresh.clone(),
            url: String::new(),
            used: Instant::now(),
            busy: false,
        });
        tabs.current = tabs.open.len() - 1;
    });
    Ok(fresh)
}

/// Look at another tab.
pub async fn select_tab(owner: i64, which: usize) -> Result<(), String> {
    registry(|all| {
        let Some(tabs) = all.owners.get_mut(&owner) else {
            return Err("there are no tabs open".to_string());
        };
        if which >= tabs.open.len() {
            return Err(format!(
                "there is no tab {which}: {} open, numbered from 0",
                tabs.open.len()
            ));
        }
        tabs.current = which;
        tabs.open[which].used = Instant::now();
        Ok(())
    })
}

/// Close one tab, or the current one.
///
/// Answers how many are left, because that is what the caller reports and
/// because closing the last one is worth knowing about: the next action opens a
/// fresh page rather than failing.
pub async fn close_tab(owner: i64, which: Option<usize>) -> Result<usize, String> {
    let taken = registry(|all| {
        let Some(tabs) = all.owners.get(&owner) else {
            return Err("there are no tabs open".to_string());
        };
        let at = which.unwrap_or(tabs.current);
        if at >= tabs.open.len() {
            return Err(format!(
                "there is no tab {at}: {} open, numbered from 0",
                tabs.open.len()
            ));
        }
        let page = drop_tab(all, owner, at);
        let left = all.owners.get(&owner).map_or(0, |tabs| tabs.open.len());
        Ok((page, left))
    })?;
    // Closing talks to the browser, so it happens after the lock is let go.
    if let Some(page) = taken.0 {
        page.close().await?;
    }
    Ok(taken.1)
}

/// Close everything this caller has open.
///
/// Answers whether there was anything to close, because "I closed it" and
/// "there was nothing open" are different things to tell somebody.
pub async fn close(owner: i64) -> Result<bool, String> {
    let taken = registry(|all| {
        all.replaced.remove(&owner);
        all.owners.remove(&owner).map(|tabs| {
            tabs.open
                .into_iter()
                .map(|slot| slot.page)
                .collect::<Vec<_>>()
        })
    });
    let Some(pages) = taken else {
        return Ok(false);
    };
    let had = !pages.is_empty();
    for page in pages {
        // Every one of them, even if an earlier one refused: a page that
        // cannot be closed must not leave the rest open behind it.
        let _ = page.close().await;
    }
    Ok(had)
}

/// One tab per address, for one agent.
///
/// Called after a page lands somewhere. Anything else this agent had open on
/// the same address is closed, because two tabs showing one page is not a
/// thing anybody wants: it is twice the memory for one view, and the agent now
/// has two sets of refs for the same controls with no way to tell them apart.
///
/// Answers how many went, so the caller can say so rather than a tab quietly
/// disappearing.
///
/// PER AGENT and not browser-wide, deliberately. Two agents on one address get
/// a tab each, because sharing would let one navigate the other away between
/// its snapshot and its click, which is the bug the terminal was fixed for
/// (KB/39) and is silent when it happens.
pub async fn one_tab_per_address(owner: i64, keeping: &Page, url: &str) -> usize {
    let session = keeping.session().to_string();
    let going = registry(|all| {
        let Some(tabs) = all.owners.get_mut(&owner) else {
            return Vec::new();
        };
        // Where the kept tab now is, so the next navigation can compare.
        for slot in tabs.open.iter_mut() {
            if slot.page.session() == session {
                slot.url = url.to_string();
            }
        }
        let duplicates: Vec<usize> = tabs
            .open
            .iter()
            .enumerate()
            .filter(|(_, slot)| {
                slot.page.session() != session && same_address(&slot.url, url) && !slot.busy
            })
            .map(|(at, _)| at)
            // Highest first, so removing one does not move the next.
            .rev()
            .collect();
        duplicates
            .into_iter()
            .filter_map(|at| drop_tab(all, owner, at))
            .collect::<Vec<_>>()
    });
    let closed = going.len();
    for page in going {
        let _ = page.close().await;
    }
    closed
}

/// This agent's tab already showing that address, made current.
///
/// What stops `tabs new` opening a second one: asking for a new tab on a page
/// you already have open is almost always a caller that has lost track, and
/// giving it the tab it already has is both what it wanted and one tab fewer.
pub async fn tab_showing(owner: i64, url: &str) -> Option<Page> {
    registry(|all| {
        let tabs = all.owners.get_mut(&owner)?;
        let at = tabs
            .open
            .iter()
            .position(|slot| same_address(&slot.url, url))?;
        tabs.current = at;
        tabs.open[at].used = Instant::now();
        Some(tabs.open[at].page.clone())
    })
}

/// Whether two addresses are the same page.
///
/// Trimmed, and a trailing slash on the root ignored, because `example.com` and
/// `example.com/` are one page and nobody means them as two. Nothing cleverer:
/// a query string or a fragment genuinely can be a different page, and deciding
/// otherwise here would merge two tabs somebody wanted apart.
fn same_address(a: &str, b: &str) -> bool {
    if a.is_empty() || b.is_empty() {
        return false;
    }
    let tidy = |u: &str| u.trim().trim_end_matches('/').to_string();
    tidy(a) == tidy(b)
}

/// Whether this site's stored sign-in still needs putting back.
///
/// Answers true ONCE per site for as long as one browser is running. See the
/// note on `Registry::restored` for why that is the right window.
pub fn first_visit_to(site: &str) -> bool {
    registry(|all| all.restored.insert(site.to_string()))
}

/// Close tabs nobody has touched for a while.
///
/// The cap bounds the worst case; this is the same rule at rest. A conversation
/// that has moved on, an agent that finished without saying so, a fleet that
/// ended an hour ago: none of them will ever call close, and without this their
/// tabs sit there until somebody else needs the room.
///
/// Started once, by the first call that needs the browser.
fn sweep_idle_tabs() {
    static STARTED: std::sync::OnceLock<()> = std::sync::OnceLock::new();
    STARTED.get_or_init(|| {
        tauri::async_runtime::spawn(async {
            loop {
                tokio::time::sleep(SWEEP_EVERY).await;
                let stale = registry(|all| {
                    let now = Instant::now();
                    let mut going = Vec::new();
                    // Collected first, removed after: taking one out from under
                    // the loop would shift the positions the loop is walking.
                    let mut which = Vec::new();
                    for (owner, tabs) in all.owners.iter() {
                        for (at, slot) in tabs.open.iter().enumerate() {
                            if !slot.busy && now.duration_since(slot.used) >= IDLE_FOR {
                                which.push((*owner, at));
                            }
                        }
                    }
                    // Highest position first, so removing one does not move the
                    // next one out from under us.
                    which.sort_by(|a, b| b.cmp(a));
                    for (owner, at) in which {
                        if let Some(page) = drop_tab(all, owner, at) {
                            going.push(page);
                        }
                    }
                    going
                });
                for page in stale {
                    let _ = page.close().await;
                }
            }
        });
    });
}

/// Forget every page, without trying to close them.
///
/// For the one case where closing is not possible and not wanted: the browser
/// they belonged to has gone. Asking a dead browser to close a page it no
/// longer has would fail slowly and change nothing.
fn forget_pages() {
    registry(|all| {
        all.owners.clear();
        all.replaced.clear();
        all.restored.clear();
        all.opening = 0;
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Asking for a page when the browser is not installed says which of the
    /// two things is wrong.
    ///
    /// "It is not here yet" and "it is here and will not start" send somebody
    /// to different places, and the first is the common one: the download runs
    /// in the background on a first launch, so an early tool call is expected
    /// rather than broken.
    #[tokio::test]
    async fn no_browser_yet_is_said_as_a_download_that_has_not_finished() {
        // A state directory with no browser in it, which is a first run.
        let state = std::env::temp_dir().join(format!("sag-live-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&state);
        std::fs::create_dir_all(&state).expect("make the state directory");
        crate::workspace::use_state_dir(state.clone());

        let why = connection()
            .await
            .expect_err("there is no browser in this state directory");
        assert!(
            why.contains("downloads itself"),
            "the reason should point at the download: {why}"
        );

        let _ = std::fs::remove_dir_all(&state);
    }
}
