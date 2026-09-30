//! Signed-in sessions, kept on disk so closing the application does not undo
//! them.
//!
//! **Only for sites somebody actually signed into.** An ordinary page leaves
//! nothing here: it has no session worth keeping, and a folder per site visited
//! would be a directory of rubbish that pushed the real ones out. What creates
//! one is the assistant saying so, after a login, because nothing else knows.
//! Detecting it would be guesswork (an analytics cookie looks like a session
//! cookie), and guessing wrong writes credentials to disk nobody asked to keep.
//!
//! **The assistant never sees a cookie.** It says "keep this site" and "forget
//! this site"; the jar is read and written here. That is the whole reason this
//! is a folder rather than something handed back in an answer: a tool result is
//! stored (`agent_tool_calls.result`) and shown in the chat, so a session
//! cookie returned to the model would be a live credential in the database, on
//! screen, and sent to the model's vendor on every later turn.
//!
//! **Fifty, and the oldest goes.** A hard rule rather than a sweep, for the
//! reason the browser has ten tabs: a limit that is checked when something is
//! created cannot be exceeded, where one that is tidied up afterwards can.

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

/// How many signed-in sites are kept.
const MOST_SITES: usize = 50;

/// One site's stored session.
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct Jar {
    /// The site this belongs to, as `host` or `host:port`. Written into the
    /// file as well as being the folder's name, so a folder that is moved or
    /// renamed by hand still says what it is.
    pub site: String,
    /// When it was last written, and what the eviction order reads.
    pub saved_at: String,
    /// Cookies exactly as the browser reported them, passed back unchanged.
    pub cookies: Vec<serde_json::Value>,
    /// localStorage for this origin, as key and value pairs.
    #[serde(default)]
    pub local_storage: Vec<(String, String)>,
    /// The origin the localStorage belongs to, which is not derivable from the
    /// site alone: `example.com` is reached over http or https and they are
    /// different origins with different storage.
    #[serde(default)]
    pub origin: String,
}

/// Where this installation keeps its files, if it knows.
pub fn state_dir() -> Option<PathBuf> {
    crate::workspace::state_dir()
}

/// Where sessions live.
pub fn home(state: &Path) -> PathBuf {
    state.join("browser-auth")
}

/// The site an address belongs to, as a folder name.
///
/// Host AND port, because `localhost:3000` and `localhost:8080` are different
/// applications that must never share a jar, which is the single most likely
/// case on a developer's own machine.
///
/// The scheme is deliberately NOT part of it. A cookie set over https is sent
/// over http to the same host, so the browser treats them as one jar and so
/// does this. The origin is kept inside the file for localStorage, which does
/// distinguish them.
pub fn site_of(url: &str) -> Option<String> {
    let rest = url.split_once("://")?.1;
    let authority = rest.split(['/', '?', '#']).next()?;
    // Credentials in an address are not part of which site it is.
    let authority = authority
        .rsplit_once('@')
        .map_or(authority, |(_, host)| host);
    if authority.is_empty() {
        return None;
    }
    Some(safe_name(&authority.to_ascii_lowercase()))
}

/// The origin an address belongs to, which is what localStorage is keyed by.
pub fn origin_of(url: &str) -> Option<String> {
    let (scheme, rest) = url.split_once("://")?;
    let authority = rest.split(['/', '?', '#']).next()?;
    let authority = authority
        .rsplit_once('@')
        .map_or(authority, |(_, host)| host);
    if authority.is_empty() {
        return None;
    }
    Some(format!(
        "{}://{}",
        scheme.to_ascii_lowercase(),
        authority.to_ascii_lowercase()
    ))
}

/// A site name that is safe to be a folder.
///
/// A host cannot contain a separator, but this is a name derived from something
/// a model supplied, and "it cannot" is a weaker guarantee than "it does not
/// matter if it does". Anything that is not a host character becomes an
/// underscore, so no address can name a folder outside this directory.
fn safe_name(host: &str) -> String {
    host.chars()
        .map(|c| match c {
            'a'..='z' | '0'..='9' | '.' | '-' => c,
            ':' => '_',
            _ => '_',
        })
        .collect()
}

/// Whether a cookie belongs to this site.
///
/// The browser's jar is not per site, so saving one means picking out the
/// cookies that belong to it. A cookie's domain is either the host itself or a
/// parent with a leading dot, which is what makes a cookie set on
/// `.example.com` arrive at `www.example.com`.
pub fn cookie_is_for(cookie: &serde_json::Value, site: &str) -> bool {
    let Some(domain) = cookie.get("domain").and_then(serde_json::Value::as_str) else {
        return false;
    };
    // The site key carries a port and a cookie domain never does.
    let host = site.split('_').next().unwrap_or(site);
    let domain = domain.trim_start_matches('.').to_ascii_lowercase();
    if domain.is_empty() {
        return false;
    }
    host == domain || host.ends_with(&format!(".{domain}"))
}

/// Read a site's stored session.
pub fn load(state: &Path, site: &str) -> Option<Jar> {
    let text = std::fs::read_to_string(home(state).join(site).join("session.json")).ok()?;
    serde_json::from_str(&text).ok()
}

/// Whether a site has one, without reading it.
pub fn have(state: &Path, site: &str) -> bool {
    home(state).join(site).join("session.json").is_file()
}

/// Keep a site's session, and hold the whole store to fifty.
///
/// The eviction happens BEFORE the write and counts the new one, so the store
/// is never briefly fifty-one. Writing first and tidying after is how a limit
/// becomes a thing that is usually true.
pub fn save(state: &Path, jar: &Jar) -> Result<(), String> {
    let root = home(state);
    let folder = root.join(&jar.site);
    if !folder.exists() {
        make_room(&root, MOST_SITES.saturating_sub(1));
    }
    std::fs::create_dir_all(&folder)
        .map_err(|e| format!("the session folder could not be made: {e}"))?;

    let text = serde_json::to_string_pretty(jar)
        .map_err(|e| format!("the session could not be written: {e}"))?;
    // Written beside and renamed, so a session that is being saved when the
    // application is killed is either the old one or the new one and never
    // half of either.
    let staging = folder.join("session.json.writing");
    std::fs::write(&staging, text).map_err(|e| format!("the session could not be saved: {e}"))?;
    std::fs::rename(&staging, folder.join("session.json"))
        .map_err(|e| format!("the session could not be saved: {e}"))?;
    Ok(())
}

/// Forget a site. Answers whether there was one.
pub fn forget(state: &Path, site: &str) -> bool {
    let folder = home(state).join(site);
    if !folder.is_dir() {
        return false;
    }
    std::fs::remove_dir_all(&folder).is_ok()
}

/// Every site with a stored session, oldest first.
pub fn stored(state: &Path) -> Vec<(String, String)> {
    let Ok(entries) = std::fs::read_dir(home(state)) else {
        return Vec::new();
    };
    let mut all: Vec<(String, String)> = entries
        .flatten()
        .filter(|entry| entry.path().is_dir())
        .map(|entry| {
            let site = entry.file_name().to_string_lossy().to_string();
            // A folder with no readable session sorts first and is evicted
            // first, which is right: it is either half written or not ours.
            let when = load(state, &site)
                .map(|jar| jar.saved_at)
                .unwrap_or_default();
            (site, when)
        })
        .collect();
    all.sort_by(|a, b| a.1.cmp(&b.1));
    all
}

/// Remove the oldest until there is room for `keeping` more.
fn make_room(root: &Path, keeping: usize) {
    let state = root.parent().unwrap_or(root);
    let mut all = stored(state);
    while all.len() > keeping {
        let (site, _) = all.remove(0);
        let _ = std::fs::remove_dir_all(root.join(site));
    }
}

/// Now, as the file records it.
///
/// Sortable as text, which is what the eviction order rests on: it compares
/// strings rather than parsing dates, and that only works because this format
/// puts the most significant part first.
pub fn now() -> String {
    chrono::Utc::now()
        .format("%Y-%m-%dT%H:%M:%S%.6fZ")
        .to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scratch(name: &str) -> PathBuf {
        let at = std::env::temp_dir().join(format!("sag-sess-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&at);
        std::fs::create_dir_all(&at).expect("a scratch directory");
        at
    }

    fn jar_for(site: &str, when: &str) -> Jar {
        Jar {
            site: site.to_string(),
            saved_at: when.to_string(),
            cookies: vec![
                serde_json::json!({"name": "sid", "value": "x", "domain": "example.com"}),
            ],
            local_storage: vec![("jwt".into(), "token".into())],
            origin: "https://example.com".into(),
        }
    }

    /// A site is a host and a port, and nothing a model sends can escape the
    /// folder it belongs in.
    #[test]
    fn a_site_is_a_host_and_a_port() {
        assert_eq!(
            site_of("https://example.com/a/b?c=1").as_deref(),
            Some("example.com")
        );
        assert_eq!(
            site_of("http://EXAMPLE.com").as_deref(),
            Some("example.com")
        );
        // The one that matters on a developer's own machine: two applications
        // on one host must never share a jar.
        assert_eq!(
            site_of("http://localhost:3000/").as_deref(),
            Some("localhost_3000")
        );
        assert_ne!(
            site_of("http://localhost:3000/"),
            site_of("http://localhost:8080/")
        );
        // Credentials in an address are not part of which site it is.
        assert_eq!(
            site_of("https://user:pw@example.com/x").as_deref(),
            Some("example.com")
        );

        // Nothing becomes a path. A host cannot hold these, but the value came
        // from a model and "cannot" is weaker than "does not matter".
        let nasty = site_of("https://..%2f..%2fetc/x").expect("still a site");
        assert!(!nasty.contains('/') && !nasty.contains('\\'));
        assert!(site_of("not-a-url").is_none());
        assert!(site_of("https://").is_none());
    }

    /// The scheme is not part of the site, and IS part of the origin.
    ///
    /// A cookie set over https reaches the same host over http, so one jar is
    /// right. localStorage does not, so the origin is kept inside the file.
    #[test]
    fn the_scheme_separates_storage_and_not_cookies() {
        assert_eq!(
            site_of("https://example.com/"),
            site_of("http://example.com/")
        );
        assert_ne!(
            origin_of("https://example.com/"),
            origin_of("http://example.com/")
        );
        assert_eq!(
            origin_of("https://Example.com/a").as_deref(),
            Some("https://example.com")
        );
    }

    /// A cookie belongs to a site when the browser would send it there.
    #[test]
    fn a_cookie_belongs_to_the_site_it_would_be_sent_to() {
        let of = |domain: &str| serde_json::json!({"name": "a", "value": "b", "domain": domain});
        assert!(cookie_is_for(&of("example.com"), "example.com"));
        // The leading dot is what makes a cookie reach subdomains.
        assert!(cookie_is_for(&of(".example.com"), "www.example.com"));
        assert!(cookie_is_for(&of("example.com"), "www.example.com"));
        // And what must NOT match: a different site that merely ends the same.
        assert!(!cookie_is_for(&of("example.com"), "notexample.com"));
        assert!(!cookie_is_for(&of("other.com"), "example.com"));
        // A port on the site key is not part of a cookie's domain.
        assert!(cookie_is_for(&of("localhost"), "localhost_3000"));
        assert!(!cookie_is_for(
            &serde_json::json!({"name": "a"}),
            "example.com"
        ));
    }

    /// A session survives being written and read back.
    #[test]
    fn a_session_is_written_and_read_back() {
        let state = scratch("roundtrip");
        assert!(!have(&state, "example.com"));
        assert!(load(&state, "example.com").is_none());

        save(&state, &jar_for("example.com", &now())).expect("save");
        assert!(have(&state, "example.com"));
        let back = load(&state, "example.com").expect("read it back");
        assert_eq!(back.site, "example.com");
        assert_eq!(back.cookies.len(), 1);
        assert_eq!(
            back.local_storage,
            vec![("jwt".to_string(), "token".to_string())]
        );
        assert_eq!(back.origin, "https://example.com");

        // Forgetting is what signing out is.
        assert!(forget(&state, "example.com"));
        assert!(!have(&state, "example.com"));
        assert!(
            !forget(&state, "example.com"),
            "forgetting twice is not an error"
        );
        let _ = std::fs::remove_dir_all(&state);
    }

    /// Fifty sites, and the fifty-first evicts the oldest.
    #[test]
    fn the_store_holds_fifty_and_the_oldest_goes() {
        let state = scratch("lru");
        for n in 0..MOST_SITES {
            // Ordered timestamps, so which one is oldest is not a guess.
            save(
                &state,
                &jar_for(
                    &format!("site{n:03}.com"),
                    &format!("2026-01-01T00:00:{n:02}Z"),
                ),
            )
            .expect("save");
        }
        assert_eq!(stored(&state).len(), MOST_SITES);
        assert!(have(&state, "site000.com"));

        save(&state, &jar_for("newcomer.com", "2026-06-01T00:00:00Z")).expect("save");
        assert_eq!(
            stored(&state).len(),
            MOST_SITES,
            "the store went over its limit instead of evicting"
        );
        assert!(!have(&state, "site000.com"), "the OLDEST should have gone");
        assert!(have(&state, "site001.com"), "and only the oldest");
        assert!(have(&state, "newcomer.com"));

        // Saving a site that is ALREADY there evicts nothing: it is a refresh,
        // not a new arrival, which is what keeps a rotating token from pushing
        // somebody else out every time it is written.
        save(&state, &jar_for("site001.com", "2026-07-01T00:00:00Z")).expect("save");
        assert_eq!(stored(&state).len(), MOST_SITES);
        assert!(have(&state, "site002.com"), "a refresh evicted somebody");
        let _ = std::fs::remove_dir_all(&state);
    }

    /// The oldest is decided by what the file says, not by the folder's name.
    #[test]
    fn the_eviction_order_is_what_the_files_say() {
        let state = scratch("order");
        save(&state, &jar_for("zzz.com", "2026-01-01T00:00:00Z")).expect("save");
        save(&state, &jar_for("aaa.com", "2026-09-01T00:00:00Z")).expect("save");
        let order: Vec<String> = stored(&state).into_iter().map(|(site, _)| site).collect();
        assert_eq!(
            order,
            vec!["zzz.com".to_string(), "aaa.com".to_string()],
            "oldest first, by saved_at and not alphabetically"
        );
        let _ = std::fs::remove_dir_all(&state);
    }

    /// A half written session is never read as a whole one.
    #[test]
    fn a_session_is_written_whole_or_not_at_all() {
        let state = scratch("atomic");
        save(&state, &jar_for("example.com", &now())).expect("save");
        // What a kill mid-write leaves: the staging file, which nothing reads.
        std::fs::write(
            home(&state)
                .join("example.com")
                .join("session.json.writing"),
            "{ broken",
        )
        .expect("write the staging file");
        let back = load(&state, "example.com").expect("the real one is still there");
        assert_eq!(
            back.cookies.len(),
            1,
            "a half written file was read instead"
        );
        let _ = std::fs::remove_dir_all(&state);
    }
}
