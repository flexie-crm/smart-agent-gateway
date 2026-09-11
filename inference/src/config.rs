//! What a node is told about itself.
//!
//! All of it from the environment, all of it validated at boot, and a node that
//! is misconfigured refuses to start rather than starting wrong.
//!
//! The rule that matters most: **a running node always has a strong key**. It
//! used to be demanded of whoever started the process, and starting without one
//! was a boot failure. It is now GUARANTEED instead: a machine mints a key on
//! first boot and keeps it (`join::Identity`), which is what lets it register
//! itself without anybody having to choose a secret. Setting one by hand still
//! works and still has a floor; the difference is that there is no longer a way
//! to end up with a node answering anyone.

use std::{net::SocketAddr, path::PathBuf};

use crate::error::{Error, Result};

/// The shortest key we will start with.
///
/// The orchestrator seals a generated key per node, so this is not a limit on
/// anybody's typing: it is the floor that stops "test" or "changeme" from
/// reaching a machine holding a model.
const MIN_KEY_LEN: usize = 24;

/// What a machine listens on when nobody said.
///
/// Not 8081, which is what this used to be and is one of the most contended
/// ports on a Linux box: alternate HTTP, Jenkins, Tomcat, and half the dev
/// servers anybody has ever run. A GPU server is a machine other people also use
/// and a default that collides is a machine that will not start.
///
/// Below 32768 on purpose. Linux hands out ephemeral ports from 32768 upward, so
/// a listener up there can find its port already taken by an outgoing connection
/// somebody else's process made a second earlier, which is a failure that comes
/// and goes and is miserable to diagnose. 19443 is in the unassigned range,
/// reads as a TLS port (which it is), and nothing common wants it.
///
/// `--port` on the installer, or `SAG_NODE_ADDR`, overrides it.
pub const DEFAULT_PORT: u16 = 19443;

/// Where model directories live under the data directory.
pub const MODELS_DIR: &str = "models";

/// Where a pull writes before it has finished.
///
/// Deliberately a sibling of `models/` under the same root, because the last
/// step of a pull is a rename and a rename is only atomic within one
/// filesystem. Putting this in a temp directory would make the final step a
/// copy, and a copy has a halfway point where a model is neither absent nor
/// whole.
pub const INCOMING_DIR: &str = "incoming";

#[derive(Debug, Clone)]
pub struct Config {
    /// What to listen on.
    pub addr: SocketAddr,
    /// A key an administrator set by hand, for a machine configured the old way.
    ///
    /// Usually absent. A machine mints its own key on first boot and keeps it
    /// (see `join::Identity`), which is what lets it register itself without
    /// anybody choosing a secret. This overrides that when somebody has a reason
    /// to pick one.
    pub key: Option<String>,
    /// The root this node owns. Nothing is written outside it.
    pub data_dir: PathBuf,
    /// What this node calls itself, for logs and for the console.
    pub name: String,
    /// Where models are searched for and fetched from.
    pub hub_url: String,
    /// A token for the library, needed only for models that are gated.
    pub hub_token: Option<String>,

    /// Where the gateway is, for a machine that registers itself.
    pub gateway_url: Option<String>,
    /// The shared secret that proves this machine may register.
    pub join_token: Option<String>,
    /// Where the gateway should reach this machine, host and port.
    ///
    /// Absent means "use the address you saw the join come from", which is right
    /// far more often than it is wrong: a machine behind NAT or in a container
    /// does not know the address that reaches it from outside, and guessing is
    /// how one registers `127.0.0.1` and is then unreachable.
    pub advertise: Option<String>,
}

impl Config {
    /// Read the environment and refuse anything that would start a node in a
    /// state somebody would have to debug later.
    pub fn from_env() -> Result<Self> {
        let addr_raw = var("SAG_NODE_ADDR").unwrap_or_else(|| format!("0.0.0.0:{DEFAULT_PORT}"));
        let addr: SocketAddr = addr_raw
            .parse()
            .map_err(|_| Error::invalid(format!("SAG_NODE_ADDR is not an address: {addr_raw}")))?;

        // A key set by hand is checked; a key not set is minted later, and a
        // minted one is past this floor by construction. Either way a running
        // machine always has a strong key, which is the property that matters:
        // it used to be DEMANDED of whoever started the node, and is now
        // guaranteed.
        let key = var("SAG_NODE_KEY");
        if let Some(key) = &key
            && key.len() < MIN_KEY_LEN
        {
            return Err(Error::invalid(format!(
                "SAG_NODE_KEY is too short: {MIN_KEY_LEN} characters at least"
            )));
        }

        let data_dir = PathBuf::from(var("SAG_NODE_DATA").unwrap_or_else(|| "./data".into()));

        let name = var("SAG_NODE_NAME")
            .or_else(hostname)
            .unwrap_or_else(|| "node".into());

        let hub_url = var("SAG_NODE_HUB_URL")
            .unwrap_or_else(|| "https://huggingface.co".into())
            .trim_end_matches('/')
            .to_string();

        Ok(Self {
            addr,
            key,
            data_dir,
            name,
            hub_url,
            hub_token: var("SAG_NODE_HUB_TOKEN"),
            gateway_url: var("SAG_URL").map(|u| u.trim_end_matches('/').to_string()),
            join_token: var("SAG_JOIN_TOKEN"),
            advertise: var("SAG_NODE_ADVERTISE"),
        })
    }

    /// Whether this machine can register from nothing.
    ///
    /// Both or neither: an address with no token cannot register and a token
    /// with no address has nowhere to send it, and a machine that silently did
    /// nothing about half a configuration would be a machine somebody spends an
    /// afternoon looking for.
    ///
    /// This is the FIRST registration only. A machine that has already been
    /// admitted checks in as itself and needs no token at all, which is
    /// `reaches_gateway`.
    pub fn joins(&self) -> bool {
        self.gateway_url.is_some() && self.join_token.is_some()
    }

    /// Whether this machine has a gateway to check in with.
    ///
    /// The token is not part of it. An invitation is spent by the machine that
    /// uses it, so the copy left behind in this machine's configuration is a
    /// dead string within the hour; if renewal depended on it, every machine
    /// would stop renewing its certificate the moment its invitation expired.
    pub fn reaches_gateway(&self) -> bool {
        self.gateway_url.is_some()
    }

    pub fn models_dir(&self) -> PathBuf {
        self.data_dir.join(MODELS_DIR)
    }

    pub fn incoming_dir(&self) -> PathBuf {
        self.data_dir.join(INCOMING_DIR)
    }

    /// Make the directories this node writes to, so that every later write can
    /// treat their absence as a real failure rather than a first run.
    pub async fn prepare_dirs(&self) -> Result<()> {
        tokio::fs::create_dir_all(self.models_dir()).await?;
        tokio::fs::create_dir_all(self.incoming_dir()).await?;
        Ok(())
    }
}

/// An environment variable, treating blank as absent.
///
/// `FOO=` in a compose file is somebody meaning "unset", not somebody meaning
/// "the empty string", and reading it the other way is how a node ends up with
/// an empty key that passes an is-set check.
fn var(key: &str) -> Option<String> {
    match std::env::var(key) {
        Ok(v) if !v.trim().is_empty() => Some(v.trim().to_string()),
        _ => None,
    }
}

/// What this machine calls itself, when nobody said.
///
/// Asked of the OPERATING SYSTEM, not of the environment. `HOSTNAME` is a shell
/// variable that Linux shells usually export and macOS does not, so reading it
/// meant every Mac node fell through to the same word: two machines both called
/// "node", on a screen whose whole job is telling machines apart.
fn hostname() -> Option<String> {
    // The environment still wins where it is set, because a container is often
    // given a name that way and it is a better name than the random hex the
    // kernel reports.
    if let Ok(name) = std::env::var("HOSTNAME")
        && !name.trim().is_empty()
    {
        return Some(name.trim().to_string());
    }
    let name = hostname_from_os()?;
    // Trim the local-network suffix a Mac reports ("macbook.local"), which is an
    // implementation detail of how it is announced rather than part of a name.
    Some(name.trim_end_matches(".local").to_string())
}

/// The machine's own name, asked of the operating system.
///
/// This was `libc::gethostname`, on the reasoning that it is the call every
/// platform has rather than a crate for four lines. It is not: the `libc`
/// crate's Windows bindings cover the C runtime, and `gethostname` lives in
/// winsock, so the call does not exist there and the node did not compile at
/// all for the first platform that asked it to.
///
/// `sysinfo` answers it on every platform, is ALREADY a dependency (machine.rs
/// reads memory and disks through it), and takes the unsafe block away with it.
/// So the original argument now points the other way: no crate is being added,
/// and the one being removed is `libc`.
fn hostname_from_os() -> Option<String> {
    let name = sysinfo::System::host_name()?;
    let name = name.trim().to_string();
    (!name.is_empty()).then_some(name)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Env is process-wide, so these run under one lock rather than racing each
    /// other into a failure that only happens on a loaded machine.
    static ENV: std::sync::Mutex<()> = std::sync::Mutex::new(());

    fn with_env<T>(pairs: &[(&str, Option<&str>)], f: impl FnOnce() -> T) -> T {
        let _guard = ENV.lock().unwrap_or_else(|e| e.into_inner());
        let saved: Vec<_> = pairs
            .iter()
            .map(|(k, _)| (*k, std::env::var(k).ok()))
            .collect();
        for (k, v) in pairs {
            match v {
                // SAFETY: every test that touches the environment holds ENV, so
                // no other thread here is reading or writing it.
                Some(v) => unsafe { std::env::set_var(k, v) },
                None => unsafe { std::env::remove_var(k) },
            }
        }
        let out = f();
        for (k, v) in saved {
            match v {
                Some(v) => unsafe { std::env::set_var(k, v) },
                None => unsafe { std::env::remove_var(k) },
            }
        }
        out
    }

    const GOOD_KEY: &str = "0123456789abcdef0123456789abcdef";

    #[test]
    fn a_node_told_no_key_mints_one_rather_than_refusing_to_start() {
        // The old rule was "no key, no boot", which made a machine that
        // registers itself impossible: there is nobody to choose the secret.
        // What replaced it is stronger, not weaker: the key is minted here
        // instead of demanded, so there is no path to a node without one.
        with_env(&[("SAG_NODE_KEY", None)], || {
            let cfg = Config::from_env().expect("a node with no key set refused to start");
            assert!(cfg.key.is_none(), "a key appeared from nowhere");
        });
    }

    #[test]
    fn a_blank_key_counts_as_no_key() {
        with_env(&[("SAG_NODE_KEY", Some("   "))], || {
            let cfg = Config::from_env().expect("a blank key refused to start");
            assert!(cfg.key.is_none(), "a blank key was read as a key");
        });
    }

    #[test]
    fn a_machine_told_no_name_uses_the_one_this_computer_has() {
        // It read the HOSTNAME variable, which Linux shells export and macOS
        // does not, so every Mac node was called "node" and two of them were
        // indistinguishable on the one screen meant to tell them apart.
        with_env(&[("SAG_NODE_NAME", None), ("HOSTNAME", None)], || {
            let cfg = Config::from_env().expect("valid config was refused");
            assert_ne!(
                cfg.name, "node",
                "this machine fell back to the generic name"
            );
            assert!(!cfg.name.is_empty());
            // The way a Mac announces itself is not part of its name.
            assert!(!cfg.name.ends_with(".local"), "{}", cfg.name);
        });
    }

    #[test]
    fn a_name_that_was_set_wins_over_the_computers_own() {
        with_env(
            &[
                ("SAG_NODE_NAME", Some("gpu-lab-1")),
                ("HOSTNAME", Some("ignored")),
            ],
            || {
                assert_eq!(Config::from_env().unwrap().name, "gpu-lab-1");
            },
        );
    }

    #[test]
    fn a_container_name_from_the_environment_is_used_when_there_is_no_setting() {
        // A container is often given a name that way, and it is a better name
        // than the random hex the kernel reports for one.
        with_env(
            &[("SAG_NODE_NAME", None), ("HOSTNAME", Some("worker-7"))],
            || {
                assert_eq!(Config::from_env().unwrap().name, "worker-7");
            },
        );
    }

    #[test]
    fn registering_needs_both_halves_or_neither() {
        // Half a configuration is what somebody spends an afternoon looking for.
        let cases = [
            (None, None, false),
            (Some("http://sag"), None, false),
            (None, Some("abc123_secret"), false),
            (Some("http://sag"), Some("abc123_secret"), true),
        ];
        for (url, token, want) in cases {
            with_env(
                &[
                    ("SAG_NODE_KEY", Some(GOOD_KEY)),
                    ("SAG_URL", url),
                    ("SAG_JOIN_TOKEN", token),
                ],
                || {
                    let cfg = Config::from_env().expect("valid config was refused");
                    assert_eq!(cfg.joins(), want, "{url:?} + {token:?}");
                },
            );
        }
    }

    #[test]
    fn the_gateway_address_loses_its_trailing_slash() {
        // It is joined to a path, and a double slash is a different route to
        // some proxies.
        with_env(
            &[
                ("SAG_NODE_KEY", Some(GOOD_KEY)),
                ("SAG_URL", Some("https://sag.internal/")),
                ("SAG_JOIN_TOKEN", Some("abc123_secret")),
            ],
            || {
                let cfg = Config::from_env().expect("valid config was refused");
                assert_eq!(cfg.gateway_url.as_deref(), Some("https://sag.internal"));
            },
        );
    }

    #[test]
    fn a_short_key_does_not_start() {
        with_env(&[("SAG_NODE_KEY", Some("changeme"))], || {
            let err = Config::from_env().expect_err("started with a short key");
            assert!(err.to_string().contains("too short"), "{err}");
        });
    }

    #[test]
    fn an_unparseable_address_does_not_start() {
        with_env(
            &[
                ("SAG_NODE_KEY", Some(GOOD_KEY)),
                ("SAG_NODE_ADDR", Some("not-an-address")),
            ],
            || {
                let err = Config::from_env().expect_err("started on a bad address");
                assert!(err.to_string().contains("SAG_NODE_ADDR"), "{err}");
            },
        );
    }

    #[test]
    fn defaults_are_filled_in_and_the_hub_url_loses_its_slash() {
        with_env(
            &[
                ("SAG_NODE_KEY", Some(GOOD_KEY)),
                ("SAG_NODE_ADDR", None),
                ("SAG_NODE_DATA", None),
                ("SAG_URL", None),
                ("SAG_JOIN_TOKEN", None),
                ("SAG_NODE_HUB_URL", Some("https://example.test/")),
                ("SAG_NODE_HUB_TOKEN", None),
            ],
            || {
                let cfg = Config::from_env().expect("valid config was refused");
                assert_eq!(cfg.addr.port(), DEFAULT_PORT);
                assert_eq!(cfg.data_dir, PathBuf::from("./data"));
                // Trailing slash removed once, so joining a path cannot produce
                // a double slash that some proxies treat as a different route.
                assert_eq!(cfg.hub_url, "https://example.test");
                assert!(cfg.hub_token.is_none());
                assert!(cfg.models_dir().ends_with(MODELS_DIR));
                assert!(cfg.incoming_dir().ends_with(INCOMING_DIR));
            },
        );
    }

    #[test]
    fn incoming_and_models_share_a_root_so_the_last_step_is_a_rename() {
        with_env(
            &[
                ("SAG_NODE_KEY", Some(GOOD_KEY)),
                ("SAG_NODE_DATA", Some("/srv/node")),
            ],
            || {
                let cfg = Config::from_env().expect("valid config was refused");
                assert_eq!(cfg.models_dir().parent(), cfg.incoming_dir().parent());
            },
        );
    }
}
