//! One page in the browser.
//!
//! A page is a target the browser opened and a session attached to it. Both ids
//! are held: the session is what every message for this page carries, and the
//! target is what closing it needs.
//!
//! Pages are the unit of isolation the browser gives us, and there will be
//! several: one per agent, eventually, for the reason the terminal keeps one
//! shell per agent (KB/39). Two agents sharing a page would navigate each other
//! away mid-read.

use std::time::Duration;

use serde_json::{json, Value};

use super::cdp::Connection;
use super::inject;
use super::record::Records;
use super::session::{self, Jar};

/// How long a navigation may take before it is reported as not having happened.
///
/// Thirty seconds, which is a slow page on a bad connection rather than a
/// broken one. What it bounds is the navigation that never completes at all: a
/// URL that hangs, a server that accepts and never answers.
const NAVIGATE_WITHIN: Duration = Duration::from_secs(30);

/// A page, and the browser it belongs to.
#[derive(Debug, Clone)]
pub struct Page {
    browser: Connection,
    target: String,
    session: String,
    /// What this page has been seen to do: its console, its requests, and
    /// whether it is waiting on a dialog. See `record`.
    records: Records,
    /// The seeding script installed for one navigation, so it can be taken
    /// away again. See `restore`.
    seeding: std::sync::Arc<std::sync::Mutex<Option<String>>>,
}

impl Page {
    /// Open a new page, ready to be asked about.
    ///
    /// It comes back blank, with the automation script installed. Opening and
    /// navigating are separate because they fail differently: a browser that
    /// will not open a page is broken, where a page that will not load an
    /// address is usually the address.
    pub async fn open(browser: &Connection) -> Result<Self, String> {
        let opened = browser
            .call("Target.createTarget", json!({ "url": "about:blank" }))
            .await?;
        let target = opened
            .get("targetId")
            .and_then(Value::as_str)
            .ok_or("the browser opened a page and did not say which")?
            .to_string();

        // Flattened, which is what puts this page's messages on the connection
        // we already have rather than needing another socket for it.
        let attached = browser
            .call(
                "Target.attachToTarget",
                json!({ "targetId": target, "flatten": true }),
            )
            .await?;
        let session = attached
            .get("sessionId")
            .and_then(Value::as_str)
            .ok_or("the browser attached to a page and did not say how to reach it")?
            .to_string();

        // Recording starts BEFORE the domains are enabled, which is the same
        // rule `goto` follows for its listener: enabling a domain makes the
        // browser start announcing at once, and a recorder attached afterwards
        // has already missed whatever it announced first.
        let records = Records::start(browser, &session);

        let page = Self {
            browser: browser.clone(),
            target,
            session,
            records,
            seeding: std::sync::Arc::new(std::sync::Mutex::new(None)),
        };

        // Page events, which is what a navigation is waited on with. Runtime,
        // because evaluating anything needs it.
        page.browser
            .call_on(&page.session, "Page.enable", json!({}))
            .await?;
        page.browser
            .call_on(&page.session, "Runtime.enable", json!({}))
            .await?;
        // What the console said. Two sources rather than one: `Runtime` carries
        // what the page printed, and `Log` carries what the BROWSER has to say
        // about it (a blocked request, a refused certificate, a content policy
        // violation), which a page never prints for itself and which is usually
        // the answer to why something did not work.
        page.browser
            .call_on(&page.session, "Log.enable", json!({}))
            .await?;
        // What the page fetched. Bodies are not kept: the browser holds them
        // and `network_request` asks for one when somebody wants it.
        page.browser
            .call_on(&page.session, "Network.enable", json!({}))
            .await?;
        // File choosers come to us instead of to a window there is not one of.
        // Without this, clicking a file input in a headless browser opens a
        // chooser nobody can see and nothing can answer.
        page.browser
            .call_on(
                &page.session,
                "Page.setInterceptFileChooserDialog",
                json!({ "enabled": true }),
            )
            .await?;
        // Installed now, so it is in every document this page ever holds,
        // including ones the page navigates itself to.
        inject::install(&page.browser, &page.session).await?;

        Ok(page)
    }

    /// Which session this page is, for anything that needs to talk to it.
    pub fn session(&self) -> &str {
        &self.session
    }

    /// What this page has been seen to do.
    pub fn records(&self) -> &Records {
        &self.records
    }

    /// Where this page is and what it is called, asked of the BROWSER rather
    /// than of the page.
    ///
    /// That is the whole point of it. Reading `location.href` means running
    /// JavaScript, and a page showing a dialog runs none until the dialog is
    /// answered, so listing tabs that way would hang on exactly the tab
    /// somebody is trying to get away from.
    pub async fn where_and_what(&self) -> Result<(String, String), String> {
        let info = self
            .browser
            .call("Target.getTargetInfo", json!({ "targetId": self.target }))
            .await?;
        let at = info.get("targetInfo").unwrap_or(&info);
        Ok((
            at.get("url")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string(),
            at.get("title")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string(),
        ))
    }

    /// The connection this page is on, for a tool that needs to speak to the
    /// browser about it rather than to the page.
    pub fn browser(&self) -> &Connection {
        &self.browser
    }

    /// Go to an address, and wait until the page has loaded.
    ///
    /// The listener is taken BEFORE the navigation is asked for. A page that
    /// loads quickly can fire its event before a listener opened afterwards
    /// exists, and the wait that followed would be a wait for something that
    /// has already happened: thirty seconds, and then a timeout reported on a
    /// page that is sitting there perfectly loaded.
    pub async fn goto(&self, url: &str) -> Result<(), String> {
        let mut listening = self.browser.listen();

        let went = self
            .browser
            .call_on(&self.session, "Page.navigate", json!({ "url": url }))
            .await?;
        // A navigation that failed answers SUCCESSFULLY and says so in a field.
        // Checking only for a protocol error would report a page that never
        // loaded as one that did, and the next question about it would come
        // back with nothing found.
        if let Some(why) = went.get("errorText").and_then(Value::as_str) {
            return Err(format!("{url} could not be loaded: {why}"));
        }

        self.browser
            .wait_for(
                &mut listening,
                &self.session,
                "Page.loadEventFired",
                NAVIGATE_WITHIN,
            )
            .await?;
        Ok(())
    }

    /// Run the assistant's own JavaScript in this page. See `inject::run_script`.
    pub async fn run_script(
        &self,
        body: &str,
        within: std::time::Duration,
    ) -> Result<Value, String> {
        inject::run_script(&self.browser, &self.session, body, within).await
    }

    /// Read this page's signed-in state out, for keeping.
    ///
    /// Cookies come from the browser (they are not the page's to give) and
    /// localStorage from the page (it is per origin and the browser has no
    /// view of one origin's). Only the cookies for THIS site are taken: the
    /// jar is shared across every site open, so saving it whole would put one
    /// site's session in another site's folder.
    pub async fn signed_in_state(&self, url: &str) -> Result<Jar, String> {
        let site = session::site_of(url).ok_or("that is not an address with a site in it")?;
        let origin = session::origin_of(url).unwrap_or_default();

        let all = self.browser.call("Storage.getCookies", json!({})).await?;
        let cookies: Vec<Value> = all
            .get("cookies")
            .and_then(Value::as_array)
            .map(|found| {
                found
                    .iter()
                    .filter(|cookie| session::cookie_is_for(cookie, &site))
                    .cloned()
                    .collect()
            })
            .unwrap_or_default();

        // Absent rather than an error: a site may keep everything in cookies,
        // and a page that has no local storage is not a page that failed.
        let local_storage = self
            .browser
            .call_on(
                &self.session,
                "DOMStorage.getDOMStorageItems",
                json!({ "storageId": { "securityOrigin": origin, "isLocalStorage": true } }),
            )
            .await
            .ok()
            .and_then(|got| got.get("entries").and_then(Value::as_array).cloned())
            .map(|entries| {
                entries
                    .iter()
                    .filter_map(|pair| {
                        let pair = pair.as_array()?;
                        Some((
                            pair.first()?.as_str()?.to_string(),
                            pair.get(1)?.as_str()?.to_string(),
                        ))
                    })
                    .collect()
            })
            .unwrap_or_default();

        Ok(Jar {
            site,
            saved_at: session::now(),
            cookies,
            local_storage,
            origin,
        })
    }

    /// Put a stored session back, BEFORE going to the site.
    ///
    /// The order is the whole of this and it was measured rather than reasoned.
    /// Cookies can be set with no page open, so they go in first. localStorage
    /// cannot: it belongs to an origin, and a document has to exist. Restoring
    /// it after the page loads is too late, because a site reads it on load and
    /// has already decided you are signed out: measured, the page said SIGNED
    /// OUT and only a second load would have fixed it.
    ///
    /// So it is seeded by a script the browser runs on every new document
    /// BEFORE any of the page's own. That script is installed for one
    /// navigation and taken away again, and it checks the origin before writing
    /// anything, because a script left in place would otherwise write one
    /// site's storage into the next site this page visits.
    pub async fn restore(&self, jar: &Jar) -> Result<(), String> {
        if !jar.cookies.is_empty() {
            self.browser
                .call("Storage.setCookies", json!({ "cookies": jar.cookies }))
                .await?;
        }
        if jar.local_storage.is_empty() || jar.origin.is_empty() {
            return Ok(());
        }
        let seeding = format!(
            "(function(){{ if (location.origin !== {}) return; try {{ var v = {};              for (var k in v) localStorage.setItem(k, v[k]); }} catch (e) {{}} }})();",
            serde_json::to_string(&jar.origin).unwrap_or_else(|_| "\"\"".into()),
            serde_json::to_string(
                &jar.local_storage
                    .iter()
                    .cloned()
                    .collect::<std::collections::BTreeMap<_, _>>()
            )
            .unwrap_or_else(|_| "{}".into()),
        );
        let installed = self
            .browser
            .call_on(
                &self.session,
                "Page.addScriptToEvaluateOnNewDocument",
                json!({ "source": seeding }),
            )
            .await?;
        let identifier = installed
            .get("identifier")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_string();
        *self.seeding.lock().unwrap_or_else(|e| e.into_inner()) = Some(identifier);
        Ok(())
    }

    /// Take away the seeding script, once the page it was for has loaded.
    ///
    /// Left in place it would run on every later document this page holds, and
    /// the origin check inside it is what makes that harmless rather than a
    /// leak. Removing it as well means neither has to be relied on alone.
    pub async fn stop_seeding(&self) {
        let identifier = self
            .seeding
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .take();
        let Some(identifier) = identifier.filter(|id| !id.is_empty()) else {
            return;
        };
        let _ = self
            .browser
            .call_on(
                &self.session,
                "Page.removeScriptToEvaluateOnNewDocument",
                json!({ "identifier": identifier }),
            )
            .await;
    }

    /// Go back a page, and say whether there was anywhere to go.
    ///
    /// Through the protocol's own history rather than `history.back()` in the
    /// page, and that is not a preference: evaluating JavaScript that navigates
    /// destroys the execution context the evaluation is running in, so the call
    /// comes back as "Inspected target navigated or closed" whether or not it
    /// worked. Asking the browser for its history list instead is one round
    /// trip, cannot fail that way, and answers the question a page cannot: is
    /// there an earlier entry at all.
    pub async fn back(&self) -> Result<bool, String> {
        let history = self
            .browser
            .call_on(&self.session, "Page.getNavigationHistory", json!({}))
            .await?;
        let at = history
            .get("currentIndex")
            .and_then(Value::as_i64)
            .ok_or("the browser did not say where it is in its history")?;
        if at <= 0 {
            return Ok(false);
        }
        let entries = history
            .get("entries")
            .and_then(Value::as_array)
            .ok_or("the browser did not say what its history holds")?;
        let previous = entries
            .get((at - 1) as usize)
            .and_then(|e| e.get("id"))
            .and_then(Value::as_i64)
            .ok_or("the browser's history has no earlier entry")?;

        // Listening BEFORE asking, for the reason `goto` does: a page that
        // comes back from cache fires its load event immediately.
        let mut listening = self.browser.listen();
        self.browser
            .call_on(
                &self.session,
                "Page.navigateToHistoryEntry",
                json!({ "entryId": previous }),
            )
            .await?;
        self.browser
            .wait_for(
                &mut listening,
                &self.session,
                "Page.loadEventFired",
                NAVIGATE_WITHIN,
            )
            .await?;
        Ok(true)
    }

    /// Ask the injected script something about this page.
    ///
    /// The script is available as `s`. See `inject::ask`.
    pub async fn ask(&self, expression: &str) -> Result<Value, String> {
        inject::ask(&self.browser, &self.session, expression).await
    }

    /// Close the page.
    ///
    /// By target rather than by session: closing a session detaches us and
    /// leaves the page open, which on a browser that stays up for a working day
    /// is a tab leak nobody would ever see.
    pub async fn close(&self) -> Result<(), String> {
        self.browser
            .call("Target.closeTarget", json!({ "targetId": self.target }))
            .await?;
        Ok(())
    }
}
