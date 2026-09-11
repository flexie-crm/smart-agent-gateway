//! Boot, serve, stop.
//!
//! The order below is the whole of it, and each step is there because the one
//! after it depends on it: a node that cannot read its own disk must not open a
//! port, and a node whose engine will not start must not accept a question it
//! cannot answer.
//!
//! Shutdown mirrors the server's (KB/27): the listener stops taking new work and
//! whatever is in flight is given a bounded moment to finish. A download is
//! deliberately NOT waited for. It can take an hour, the orchestrator's job row
//! is the record of it, and the next boot clears what was half fetched. Holding
//! a deploy open for a transfer that has no deadline is the opposite of graceful.

use std::sync::Arc;

use clap::{Parser, Subcommand};
use sag_inference::{
    api::{self, App},
    catalog::Catalog,
    config::Config,
    engine::Engine,
    error::{Error, Result},
    hub::Hub,
    join::{self, Identity},
    pull::Pulls,
    tls::{self, Credentials},
};
use tokio::sync::Mutex;

#[derive(Parser)]
#[command(name = "sag-inference", about = "SAG inference node", version)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Run the node.
    Serve,
    /// Print what this machine is and stop.
    ///
    /// For the person setting a node up, before anything depends on it. It
    /// answers the two questions worth asking first: is the configuration
    /// readable, and is there room here.
    Check,
}

#[tokio::main]
async fn main() -> std::process::ExitCode {
    match run().await {
        Ok(()) => std::process::ExitCode::SUCCESS,
        Err(err) => {
            // Before tracing is up this is the only channel there is, and a
            // configuration failure is exactly the case where tracing is not up.
            eprintln!("sag-inference: {err}");
            std::process::ExitCode::FAILURE
        }
    }
}

async fn run() -> Result<()> {
    let cli = Cli::parse();
    let config = Arc::new(Config::from_env()?);
    init_logging();
    tls::choose_cryptography();

    match cli.command {
        Command::Check => {
            let machine = sag_inference::machine::Machine::read(&config.data_dir);
            println!("node:       {}", config.name);
            println!("listening:  {}", config.addr);
            println!("data:       {}", config.data_dir.display());
            println!("processors: {}", machine.processors);
            println!(
                "memory:     {} free of {}",
                machine.memory_free, machine.memory_total
            );
            println!(
                "disk:       {} free of {}",
                machine.disk_free, machine.disk_total
            );
            println!("inference:  {}", if HAS_ENGINE { "yes" } else { "NO" });
            // A machine serves nothing without one, so this is the difference
            // between "not started yet" and "started and unreachable".
            println!(
                "identity:   {}",
                match Credentials::load_or_mint(&config.data_dir).await {
                    Ok(creds) if creds.signed().is_some() => "enrolled".to_string(),
                    Ok(_) => "not enrolled yet, so it cannot serve".to_string(),
                    Err(err) => format!("unreadable: {err}"),
                }
            );
            println!(
                "registers:  {}",
                config
                    .gateway_url
                    .as_deref()
                    .unwrap_or("no, not configured")
            );
            Ok(())
        }
        Command::Serve => serve(config).await,
    }
}

async fn serve(config: Arc<Config>) -> Result<()> {
    config.prepare_dirs().await?;

    // Nothing in `incoming/` survives a restart: a partial download has no
    // record of which files finished. Cleared before the catalogue is opened, so
    // a boot never sees both.
    Pulls::clear_incoming(&config.incoming_dir()).await?;

    // Who this machine is, minted on first boot and kept. It carries the key
    // every request must present, so it is resolved before anything listens.
    let identity = Identity::load_or_mint(&config.data_dir).await?;
    let key = config.key.clone().unwrap_or_else(|| identity.key.clone());
    // And what it proves that with. The key is ours and permanent; the
    // certificate over it is the gateway's to issue and is asked for below.
    let credentials = Arc::new(Mutex::new(
        Credentials::load_or_mint(&config.data_dir).await?,
    ));

    let catalog = Catalog::open(config.models_dir()).await?;
    // Never fatal: a machine serves the models it already has whether or not it
    // can reach a library to fetch more.
    let hub = Arc::new(Hub::new(&config.hub_url, config.hub_token.clone()));
    let engine = build_engine(&config).await?;

    if !HAS_ENGINE {
        tracing::error!(
            "this build cannot run models: it serves the control surface only and will refuse questions"
        );
    }

    restore(&catalog, engine.as_ref()).await;

    // A machine serves over a channel both ends authenticate, so a certificate
    // is not an optional extra: without one there is no way to reach this
    // machine at all.
    let held = credentials.lock().await.signed();
    let signed = match held {
        // Already enrolled, so listen NOW and refresh in the background. A
        // machine restarted while its gateway is down must come back serving
        // with what it already holds rather than waiting for permission it does
        // not need.
        Some(held) => held,
        None if config.joins() => {
            tracing::info!(
                "this machine has no certificate yet, so it is registering before it listens"
            );
            join::keep_joining((*config).clone(), identity.clone(), credentials.clone())
                .await
                .ok_or_else(|| Error::invalid("this machine could not be given a certificate"))?
        }
        // Nothing to serve with and no way to be given anything. Which of the
        // two settings is missing is the whole of the difference between "this
        // is a machine nobody finished configuring" and "somebody meant to run
        // it standalone", so it is said rather than left to be worked out.
        None if config.gateway_url.is_none() => {
            return Err(Error::invalid(
                "this machine was not told where the gateway is (SAG_URL), so it cannot be \
                 given the certificate it needs to serve, and there is no unencrypted way \
                 to reach it",
            ));
        }
        None => {
            return Err(Error::invalid(
                "this machine has no join token (SAG_JOIN_TOKEN), so it cannot register and \
                 cannot be given the certificate it needs to serve. The gateway prints one: \
                 sag join-token",
            ));
        }
    };

    let tls_config = tls::serving(credentials.lock().await.key_pem(), &signed)?;

    let app = App::new(config.clone(), key, catalog, hub, engine);
    let listener = std::net::TcpListener::bind(config.addr)?;
    listener.set_nonblocking(true)?;
    let bound = listener.local_addr()?;

    tracing::info!(node = %config.name, id = %identity.node_id, address = %bound, "node ready");

    // Re-enrolling on a cadence rather than at expiry. Every enrolment is a
    // fresh certificate, so a machine that checks in daily never approaches the
    // end of one, and the certificate is replaced under the running listener:
    // a renewal that needed a restart is a renewal somebody forgets about until
    // the morning it expires.
    if config.reaches_gateway() {
        tokio::spawn(keep_enrolled(
            (*config).clone(),
            identity.clone(),
            credentials.clone(),
            tls_config.clone(),
        ));
    } else {
        tracing::warn!(
            "this machine was not told where the gateway is, so its certificate will not be renewed"
        );
    }

    let shutdown = axum_server::Handle::new();
    tokio::spawn({
        let shutdown = shutdown.clone();
        async move {
            stop_signal().await;
            shutdown.graceful_shutdown(Some(SHUTDOWN_GRACE));
        }
    });

    axum_server::from_tcp_rustls(listener, tls_config)?
        .handle(shutdown)
        .serve(api::router(app).into_make_service())
        .await?;

    tracing::info!("node stopped");
    Ok(())
}

/// How long work already in flight is given when a stop is asked for.
///
/// An answer being streamed is worth finishing; nothing here is worth waiting
/// minutes for. A download is not covered by this at all: it has no deadline,
/// the gateway's job row is the record of it, and the next boot clears what was
/// half fetched.
const SHUTDOWN_GRACE: std::time::Duration = std::time::Duration::from_secs(30);

/// How often a machine checks back in.
///
/// Each check-in is a fresh certificate, so this is also the renewal: at a day
/// apart, a ninety-day certificate is replaced dozens of times before anything
/// near its expiry, and a machine that was unreachable for a week catches up on
/// its next successful attempt.
const RE_ENROL: std::time::Duration = std::time::Duration::from_secs(24 * 60 * 60);

/// Keep this machine's registration and its certificate current.
///
/// **It checks in immediately, and then every day.** The wait used to come
/// first, which meant a machine that already held a certificate said nothing to
/// the gateway for twenty-four hours after starting. That is fine while the two
/// agree and silently wrong the moment they do not: a machine whose row was
/// removed, or whose gateway was restored from a backup taken before it joined,
/// serves happily while the gateway believes it does not exist, and comes back
/// only on a timer nobody can see.
///
/// Registering is idempotent (the gateway matches on node id and updates the row
/// it already has), so doing it at boot costs one request and removes a whole
/// class of "it is running but the console cannot see it". The machine is
/// already listening with the certificate it holds by the time this runs, so
/// none of it is on the path to serving: a gateway that is down delays the
/// check-in and nothing else.
async fn keep_enrolled(
    config: Config,
    identity: Identity,
    credentials: Arc<Mutex<Credentials>>,
    serving: axum_server::tls_rustls::RustlsConfig,
) {
    loop {
        let Some(signed) =
            join::keep_joining(config.clone(), identity.clone(), credentials.clone()).await
        else {
            return;
        };
        let key_pem = credentials.lock().await.key_pem().to_string();
        match tls::server_config(&key_pem, &signed) {
            // Swapped under the running listener: connections already open are
            // untouched and the next handshake uses the new certificate.
            Ok(config) => {
                serving.reload_from_config(config);
                tracing::info!("this machine's certificate was renewed");
            }
            Err(err) => tracing::error!(%err, "the renewed certificate cannot be served with"),
        }
        tokio::time::sleep(RE_ENROL).await;
    }
}

/// Tell the engine about everything on the disk, and START bringing back what
/// somebody asked to be ready.
///
/// Registering is cheap and happens for all of them. Loading is not, and happens
/// only for the models an administrator marked resident, which is why residency
/// is stored rather than inferred: a cache could not know which specialist
/// somebody is waiting on (KB/35).
///
/// A model the engine refuses is logged and SKIPPED. Nineteen working models
/// must not be held up by the twentieth, on a machine somebody is relying on.
///
/// # Started, not waited for
///
/// This used to await each load, so nothing was served until every resident
/// model was in memory. The argument for it was that "a node that announced
/// itself ready and then took four minutes to answer the first question would
/// have been better off saying nothing yet", and it is a false choice: saying
/// nothing yet does not mean the caller waits patiently, it means every call to
/// the control surface runs to its ceiling and the screen reports THE MACHINE
/// DID NOT ANSWER. Six minutes of a machine that looks dead, on every restart,
/// which is how it was reported three times.
///
/// Nothing about listing models, reading the disk, watching a download or
/// showing this machine at all depends on a model being in memory. So the
/// server comes up, the models it is bringing in read `waking`, which is a state
/// that exists precisely so a cold start is visible rather than a stall, and a
/// question asked meanwhile is answered "not now" rather than hanging. What was
/// lost by waiting was the ability to SEE any of that.
async fn restore(catalog: &Catalog, engine: &dyn Engine) {
    let models = catalog.list().await;
    let mut registered = 0;
    let mut waking = 0;

    for entry in &models {
        let weights = catalog.weights(&entry.uid);
        if let Err(err) = engine.register(entry, &weights).await {
            tracing::error!(uid = %entry.uid, name = %entry.name, %err, "model could not be registered");
            continue;
        }
        registered += 1;

        if entry.resident {
            // Returns as soon as the work is under way. A failure after that
            // has no caller left to reach, which is what `Engine::failures`
            // carries to the screen.
            match engine.load(&entry.uid).await {
                Ok(()) => waking += 1,
                Err(err) => {
                    tracing::error!(uid = %entry.uid, name = %entry.name, %err, "model could not be loaded");
                }
            }
        }
    }

    tracing::info!(
        models = models.len(),
        registered,
        waking,
        "catalogue restored"
    );
}

/// Whether this build can actually run a model.
const HAS_ENGINE: bool = cfg!(feature = "engine");

#[cfg(feature = "engine")]
async fn build_engine(config: &Config) -> Result<Arc<dyn Engine>> {
    Ok(Arc::new(
        sag_inference::engine::mistral::LocalEngine::start(config).await?,
    ))
}

#[cfg(not(feature = "engine"))]
async fn build_engine(_config: &Config) -> Result<Arc<dyn Engine>> {
    // One that refuses honestly, not the tests' double. See engine::absent.
    Ok(Arc::new(sag_inference::engine::absent::AbsentEngine::new()))
}

fn init_logging() {
    use tracing_subscriber::{EnvFilter, fmt};

    let filter = EnvFilter::try_from_env("SAG_NODE_LOG")
        .unwrap_or_else(|_| EnvFilter::new("sag_inference=info,warn"));
    fmt().with_env_filter(filter).with_target(true).init();
}

/// Wait for the operator or the supervisor to say stop.
///
/// Both are handled: a person pressing ctrl-c and a container runtime sending a
/// termination signal are the same event, and a node that only listened for one
/// of them would be killed outright by the other.
async fn stop_signal() {
    let interrupt = async {
        tokio::signal::ctrl_c().await.ok();
    };

    #[cfg(unix)]
    let terminate = async {
        use tokio::signal::unix::{SignalKind, signal};
        match signal(SignalKind::terminate()) {
            Ok(mut stream) => {
                stream.recv().await;
            }
            Err(err) => {
                tracing::error!(%err, "cannot listen for a shutdown signal");
                std::future::pending::<()>().await;
            }
        }
    };
    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = interrupt => tracing::info!("stopping: interrupted"),
        _ = terminate => tracing::info!("stopping: asked to"),
    }
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;
    use std::sync::{Arc, Mutex};

    use sag_inference::{
        catalog::{Catalog, ModelEntry, ModelKind},
        engine::{Engine, Residency},
        error::Result,
    };

    /// An engine that records which way it was asked to load.
    ///
    /// The distinction is the whole point of the test below and it cannot be
    /// seen any other way: both calls succeed, both leave the model on its way
    /// in, and only one of them BLOCKS until it arrives. A fake that merely
    /// slept would turn a regression into a slow test rather than a failing one.
    #[derive(Default)]
    struct Recorder {
        started: Mutex<Vec<String>>,
        waited_for: Mutex<Vec<String>>,
        registered: Mutex<Vec<String>>,
    }

    #[async_trait::async_trait]
    impl Engine for Recorder {
        async fn register(&self, entry: &ModelEntry, _weights: &std::path::Path) -> Result<()> {
            self.registered.lock().unwrap().push(entry.uid.clone());
            Ok(())
        }
        async fn forget(&self, _uid: &str) -> Result<()> {
            Ok(())
        }
        async fn load(&self, uid: &str) -> Result<()> {
            self.started.lock().unwrap().push(uid.to_string());
            Ok(())
        }
        async fn wait_loaded(&self, uid: &str) -> Result<()> {
            self.waited_for.lock().unwrap().push(uid.to_string());
            Ok(())
        }
        async fn unload(&self, _uid: &str) -> Result<()> {
            Ok(())
        }
        async fn residency(&self) -> Result<BTreeMap<String, Residency>> {
            Ok(BTreeMap::new())
        }
        fn routes(&self) -> Option<axum::Router> {
            None
        }
    }

    async fn catalogue_of(entries: Vec<ModelEntry>) -> (Catalog, tempfile::TempDir) {
        let dir = tempfile::tempdir().expect("temp dir");
        let catalog = Catalog::open(dir.path()).await.expect("open catalogue");
        for entry in entries {
            catalog.insert(entry).await.expect("insert");
        }
        (catalog, dir)
    }

    fn entry(uid: &str, resident: bool) -> ModelEntry {
        ModelEntry {
            uid: uid.to_string(),
            name: uid.to_string(),
            handle: uid.to_string(),
            repo: "vendor/model".into(),
            revision: "abc".into(),
            kind: ModelKind::Chat,
            facts: Default::default(),
            settings: Default::default(),
            resident,
            added_at: chrono::Utc::now(),
        }
    }

    #[tokio::test]
    async fn a_resident_model_is_started_rather_than_waited_for() {
        // The failure this pins: the node used to load every resident model to
        // completion BEFORE it served anything, so for the minutes that takes
        // it answered nothing at all and the screen said the machine did not
        // answer. Nothing about listing models or showing this machine depends
        // on a model being in memory.
        let (catalog, _dir) = catalogue_of(vec![entry("uid-1", true)]).await;
        let engine = Arc::new(Recorder::default());

        super::restore(&catalog, engine.as_ref()).await;

        assert_eq!(
            engine.started.lock().unwrap().as_slice(),
            ["uid-1"],
            "the load was not started"
        );
        assert!(
            engine.waited_for.lock().unwrap().is_empty(),
            "restore waited for the model, so nothing is served until it is in"
        );
    }

    #[tokio::test]
    async fn every_model_is_registered_and_only_the_resident_ones_are_loaded() {
        // Registering is cheap and is what makes a model addressable at all;
        // loading is minutes and is a decision somebody made (KB/35).
        let (catalog, _dir) =
            catalogue_of(vec![entry("wanted", true), entry("on-disk-only", false)]).await;
        let engine = Arc::new(Recorder::default());

        super::restore(&catalog, engine.as_ref()).await;

        let mut registered = engine.registered.lock().unwrap().clone();
        registered.sort();
        assert_eq!(registered, ["on-disk-only", "wanted"]);
        assert_eq!(engine.started.lock().unwrap().as_slice(), ["wanted"]);
    }
}
