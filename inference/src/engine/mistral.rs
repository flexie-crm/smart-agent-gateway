//! The engine, as this node uses it.
//!
//! Everything specific to the library that executes weights is in this file and
//! nowhere else. That is the point of [`super::Engine`]: the rest of the crate
//! talks about models being resident or released, and only here does that become
//! a call to something with its own vocabulary.
//!
//! Three facts about that library shape what is below.
//!
//! **An instance cannot exist with no models.** Its constructor takes a
//! pipeline, so there is nothing to build on a node that has not downloaded
//! anything yet. So the instance is created LAZILY, by the first load, and the
//! inference routes are served through a handler that looks it up per request.
//! A node with an empty disk answers control calls normally and says plainly
//! that it has nothing to answer questions with, which is the honest state and
//! not an error.
//!
//! **Registering and loading are genuinely different costs.** Being told a model
//! exists is a map insertion here; loading it reads tens of gigabytes and builds
//! a pipeline. Boot registers everything and loads only what an administrator
//! marked resident, which is why residency is stored (KB/35).
//!
//! **The settings an administrator filled in are applied at load.** They are
//! read from the entry each time rather than captured once, so changing one and
//! reloading is all it takes for it to be in effect.

use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
    sync::Arc,
};

use axum::{
    Router,
    extract::Request,
    http::StatusCode,
    response::{IntoResponse, Response},
};
use mistralrs_core::{
    DeviceLayerMapMetadata, DeviceMapMetadata, DeviceMapSetting, EmbeddingLoaderType,
    MemoryGpuConfig, MistralRs, MistralRsBuilder, ModelStatus, MultimodalLoaderType,
    NormalLoaderType, PagedAttentionConfig, PagedCacheType, parse_isq_value,
};
use tokio::sync::RwLock;

use crate::{
    catalog::ModelEntry,
    config::Config,
    engine::{Engine, Residency},
    error::{Error, Result},
    machine::Verdict,
};

/// A model this node knows how to build, whether or not it is built.
#[derive(Clone)]
struct Known {
    weights: PathBuf,
    settings: BTreeMap<String, String>,
    /// What the gateway asks for. Registered with the engine as a second name
    /// for the same model, so a caller may use either.
    handle: String,
}

pub struct LocalEngine {
    /// Everything on the disk, loaded or not.
    known: Arc<RwLock<BTreeMap<String, Known>>>,
    /// Created by the first load, because it cannot be created before one.
    live: Arc<RwLock<Option<Live>>>,
    /// What each live pipeline was actually BUILT with.
    ///
    /// Not the same thing as `known`, and the difference is the whole point:
    /// `known` is what the administrator has asked for, this is what the engine
    /// is currently running. They part company the moment somebody changes a
    /// setting on a model that is already loaded, and `bring_in` compares them
    /// to decide whether a reload will do.
    built: Arc<RwLock<BTreeMap<String, BTreeMap<String, String>>>>,
    /// Models on their way into memory.
    ///
    /// Loading reads tens of gigabytes and builds a pipeline, which for a large
    /// model is minutes. No caller can hold a request open for that: a browser
    /// gives up, reports a failure, and the model finishes loading anyway, which
    /// is the shape of an approved action that then fails (CLAUDE.md). So a load
    /// is STARTED by the call and watched through residency, and this is what
    /// makes `waking` a state somebody can see rather than a stall they cannot
    /// tell from a hang.
    loading: Arc<RwLock<std::collections::BTreeSet<String>>>,

    /// Why a model's last load attempt failed, for the ones that failed.
    ///
    /// A load runs after its request has been answered, so when it fails there
    /// is no caller left to return the reason to and it went into a log line
    /// nobody reads. What the screen then had was a model saying it was wanted
    /// and a residency saying it was released, with no explanation, which is
    /// how somebody spends an evening on a model the engine was never able to
    /// load: it says so precisely, once, in a file.
    ///
    /// Kept per model rather than one "last error", because several can fail
    /// and each is about its own model. Cleared when a load is attempted again
    /// and when one succeeds, so it only ever describes the state on screen.
    failures: Arc<RwLock<BTreeMap<String, String>>>,

    /// The threads every piece of model work runs on, and nothing else does.
    ///
    /// Model work is not like the rest of this process. Building a pipeline
    /// reads tens of gigabytes and is minutes of blocking CPU that internally
    /// takes a thread over; releasing one frees twelve gigabytes inside a single
    /// synchronous call. Both used to run on the runtime that serves HTTP, on
    /// the reasoning that a spawned task is a spawned task.
    ///
    /// It is not, and the node said so in three ways at once: releasing a model
    /// hung the console AND the whole node, freeing the memory but never
    /// answering; and an outbound TLS handshake in the same process stopped
    /// completing, so a machine that could serve a model perfectly well could no
    /// longer reach the model library, timing out at the connect deadline while
    /// the same request from the same machine took 0.4 seconds.
    ///
    /// So the engine gets threads of its own. Nothing it does can reach the ones
    /// answering requests, which is structural rather than a tuning: listing
    /// models, watching a download and showing this machine do not share a
    /// thread with a model being built, so they cannot be starved by one.
    runtime: Arc<EngineRuntime>,
}

/// Threads owned by the engine, given back when it goes.
///
/// A wrapper only so that dropping it is safe: dropping a runtime waits for its
/// threads, and doing that from inside another runtime's worker panics. The node
/// holds this for the life of the process, but a test drops it inside the test's
/// own runtime, which is exactly that case.
struct EngineRuntime(Option<tokio::runtime::Runtime>);

impl EngineRuntime {
    /// Two threads, because this runtime drives model work and never serves a
    /// request. It has to be a multi-thread runtime all the same: the engine
    /// library takes a thread over mid-build, which a single-threaded runtime
    /// refuses outright.
    fn start() -> Result<Self> {
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .thread_name("sag-model")
            .enable_all()
            .build()
            .map_err(|err| Error::engine(format!("the engine has no threads to run on: {err}")))?;
        Ok(Self(Some(runtime)))
    }

    fn handle(&self) -> &tokio::runtime::Handle {
        // Only `drop` empties this, and nothing reaches it afterwards.
        self.0
            .as_ref()
            .expect("the engine's threads outlive every use of them")
            .handle()
    }
}

impl Drop for EngineRuntime {
    fn drop(&mut self) {
        if let Some(runtime) = self.0.take() {
            // Returns at once and lets the threads finish on their own, rather
            // than blocking here. Whoever is dropping this may be a worker of
            // another runtime, where blocking is a panic.
            runtime.shutdown_background();
        }
    }
}

/// How long a release is given before the caller is told it did not happen.
///
/// The engine takes a plain blocking lock over its model map to release one, and
/// that call has been seen never to return: the weights stayed in memory, the
/// request stayed open, and the screen showed a button that did nothing at all.
///
/// This bounds the PERSON, not the work. The release is still running on the
/// engine's own threads and may yet finish; what ends is the pretence that
/// somebody is being served. Generous, because a real release of tens of
/// gigabytes is seconds and must not be cut short by impatience.
const RELEASE_DEADLINE: std::time::Duration = std::time::Duration::from_secs(30);

struct Live {
    instance: Arc<MistralRs>,
    routes: Router,
}

impl LocalEngine {
    /// Prepare the engine. Deliberately does no work: a node with nothing on its
    /// disk has nothing to build, and one with twenty models must not build
    /// twenty pipelines before it will answer a health check.
    pub async fn start(_config: &Config) -> Result<Self> {
        Self::empty()
    }

    fn empty() -> Result<Self> {
        Ok(Self {
            known: Arc::new(RwLock::new(BTreeMap::new())),
            live: Arc::new(RwLock::new(None)),
            built: Arc::new(RwLock::new(BTreeMap::new())),
            loading: Arc::new(RwLock::new(Default::default())),
            failures: Arc::new(RwLock::new(BTreeMap::new())),
            runtime: Arc::new(EngineRuntime::start()?),
        })
    }

    /// Whether this model is the only one the engine is holding.
    ///
    /// Asked when a removal is refused, because the engine refuses to remove its
    /// last model and that refusal is the one case where the answer is to
    /// replace the engine rather than the model.
    async fn holds_only(&self, uid: &str) -> bool {
        match self.residency().await {
            Ok(all) => all
                .iter()
                .filter(|(_, r)| matches!(r, Residency::Resident | Residency::Waking))
                .all(|(id, _)| id == uid),
            // Cannot tell, so do not tear anything down.
            Err(_) => false,
        }
    }

    /// What this model's live pipeline was built with, if it has one.
    async fn built_with(&self, uid: &str) -> Option<BTreeMap<String, String>> {
        self.built.read().await.get(uid).cloned()
    }

    /// Everything `load` does, once it has decided there is work to do.
    ///
    /// Split out so it can be awaited (at boot, where a node genuinely should
    /// not start serving until the models somebody marked resident are in) or
    /// spawned (from a request, where nothing can wait).
    async fn bring_in(&self, uid: &str) -> Result<()> {
        let known = self
            .known
            .read()
            .await
            .get(uid)
            .cloned()
            .ok_or_else(|| Error::not_found("model"))?;

        // Known to the engine but released: it can rebuild this itself, which is
        // cheaper than building a second pipeline for the same weights.
        //
        // ONLY when the settings have not changed since it was built. A reload
        // restores the pipeline the engine already holds, and every setting that
        // shapes a pipeline (the compression, the sequence limit, the chat
        // template) is applied in `build_model`, which a reload never reaches.
        // Taking this branch after a change meant the new setting was saved,
        // shown back on the screen, and silently not in force: a model asked to
        // load quantised came back at full precision, in seconds, with nothing
        // anywhere saying so.
        //
        // The instance is cloned OUT of the lock before the reload is awaited.
        // Holding a read lock across a wait that can run into minutes would stop
        // any other model being loaded for the whole of it.
        let instance = self.live.read().await.as_ref().map(|l| l.instance.clone());
        if let Some(instance) = instance {
            // An engine that cannot say whether it has this model is treated as
            // not having it: building a fresh pipeline is slower than a reload
            // but always correct, and the alternative is refusing a load over a
            // question we could not get an answer to.
            if instance.model_exists(uid).unwrap_or(false) {
                if !needs_rebuild(self.built_with(uid).await.as_ref(), &known.settings) {
                    return instance.reload_model(uid).await.map_err(Error::engine);
                }
                // Built with something else. The engine has to let it go before
                // it will build it again, and if it will not, there is no point
                // going on: `add_model` finds the name taken and the load fails
                // with "already exists", leaving the model reading `waking`
                // forever because nothing ever comes back to say otherwise.
                //
                // That is exactly what shipping this without the check did. A
                // failed removal was logged at debug and the very next line
                // depended on it having worked, which is the shape this codebase
                // has a rule against: do not treat a refusal as harmless and then
                // rely on it.
                tracing::info!(
                    uid,
                    "settings changed since this model was built, rebuilding it"
                );
                // The engine will NOT remove its last model. Its own words, and
                // its own source: `if engines.len() <= 1 { return Err("Cannot
                // remove the last model from MistralRs") }`. So on a machine
                // holding one model, which is most of them, there is no way to
                // take that model out and put a differently built one back.
                //
                // Then the instance IS the model, and replacing the model means
                // replacing the instance. Dropping it here is what a process
                // restart does, without restarting the process: the next step
                // builds a fresh one, with the routes that go with it.
                //
                // Only when it is the last one. With others loaded, removing
                // this one works and must not take them down with it.
                match instance.remove_model(uid) {
                    Ok(()) => {}
                    Err(err) if self.holds_only(uid).await => {
                        tracing::info!(uid, %err, "this is the only model here, so the engine is replaced with it");
                        *self.live.write().await = None;
                    }
                    Err(err) => {
                        return Err(Error::engine(format!(
                            "{uid}: the engine will not release this model: {err}"
                        )));
                    }
                }
                self.built.write().await.remove(uid);
            }
        }

        tracing::info!(uid, weights = %known.weights.display(), "loading model");
        self.bring_up(uid, &known).await
    }

    /// Build a pipeline for a model and put it in the engine.
    ///
    /// The first one creates the instance and the routes with it; every later
    /// one is added to what is already running, so loading a second model does
    /// not disturb the first.
    async fn bring_up(&self, uid: &str, known: &Known) -> Result<()> {
        let builder = build_model(uid, known)?;
        let (pipeline, scheduler, add_config) = builder
            .build_pipeline()
            .await
            .map_err(|err| Error::engine(format!("{uid}: {err}")))?;

        let mut live = self.live.write().await;
        match live.as_ref() {
            Some(existing) => existing
                .instance
                .add_model(uid.to_string(), pipeline, scheduler, add_config)
                .await
                .map_err(Error::engine)?,
            None => {
                let mut builder = MistralRsBuilder::new(pipeline, scheduler, false, None)
                    .with_model_id(uid.to_string());
                // Carried across deliberately: without it the engine has no way
                // to rebuild this model, and unloading it would be a one way
                // trip rather than the reversible decision it is meant to be.
                if let Some(loader) = add_config.loader_config {
                    builder = builder.with_loader_config(loader);
                }
                let instance = builder.build().await;
                let routes = mistralrs_server_core::mistralrs_server_router_builder::MistralRsServerRouterBuilder::new()
                    .with_mistralrs(instance.clone())
                    .build()
                    .await
                    .map_err(Error::engine)?;
                *live = Some(Live { instance, routes });
            }
        }

        // What it was built with, recorded now that it is in. Read by `bring_in`
        // to tell a reload it may take from one it may not.
        self.built
            .write()
            .await
            .insert(uid.to_string(), known.settings.clone());

        // The readable name, attached once the model is in. Failing to attach it
        // is logged and not fatal: the model works under its own id, and losing
        // a whole load over the name it is ALSO known by would be the wrong
        // trade.
        if let Some(live) = live.as_ref()
            && !known.handle.is_empty()
            && let Err(err) = live
                .instance
                .register_model_alias(known.handle.clone(), uid)
        {
            tracing::warn!(uid, handle = %known.handle, %err, "model loaded without its readable name");
        }
        Ok(())
    }
}

#[async_trait::async_trait]
impl Engine for LocalEngine {
    async fn register(&self, entry: &ModelEntry, weights: &Path) -> Result<()> {
        self.known.write().await.insert(
            entry.uid.clone(),
            Known {
                weights: weights.to_path_buf(),
                settings: entry.settings.clone(),
                handle: entry.handle.clone(),
            },
        );
        Ok(())
    }

    async fn forget(&self, uid: &str) -> Result<()> {
        self.known.write().await.remove(uid);
        self.built.write().await.remove(uid);

        let instance = self.live.read().await.as_ref().map(|l| l.instance.clone());
        if let Some(instance) = instance {
            // Taking a model out of the engine drops its pipeline, which is the
            // same synchronous release of gigabytes that `unload` performs, so
            // it belongs on the same threads for the same reason.
            let owned = uid.to_string();
            let removed = self
                .runtime
                .handle()
                .spawn_blocking(move || instance.remove_model(&owned))
                .await;
            // Not knowing the model is the state we want, so an engine that has
            // already forgotten it is a success and not a failure.
            match removed {
                Ok(Ok(())) => {}
                Ok(Err(err)) => {
                    tracing::debug!(uid, %err, "the engine had already let this model go");
                }
                Err(err) => {
                    tracing::warn!(uid, %err, "the engine stopped while letting a model go");
                }
            }
        }
        Ok(())
    }

    async fn load(&self, uid: &str) -> Result<()> {
        // Checked before anything is started, so a model this node does not have
        // is refused to the caller's face rather than in a log line they will
        // never read.
        if !self.known.read().await.contains_key(uid) {
            return Err(Error::not_found("model"));
        }

        // Already in memory, or already on its way. Asking again is not an
        // error: two administrators pressing the same button want the same
        // outcome, and this is it. It also stops a second press building a
        // second pipeline for weights that are already being read.
        //
        // UNLESS what is in memory is no longer what was asked for. This is the
        // path an administrator actually takes: change the compression, press
        // Load. Returning early there did nothing at all, and the only sequence
        // that applied a setting was release-then-load, which nothing on the
        // screen says and which takes the model out of service in between.
        let settings = self.known.read().await.get(uid).map(|k| k.settings.clone());
        let stale = match &settings {
            Some(wanted) => needs_rebuild(self.built_with(uid).await.as_ref(), wanted),
            None => false,
        };
        if !stale
            && let Some(Residency::Resident | Residency::Waking) = self.residency().await?.get(uid)
        {
            return Ok(());
        }
        if !self.loading.write().await.insert(uid.to_string()) {
            return Ok(());
        }

        // Whatever went wrong last time is no longer the reason it is not
        // loaded: it is being tried again, and a stale explanation on screen
        // beside a model that is currently loading is worse than none.
        self.failures.write().await.remove(uid);

        let engine = self.clone_handles();
        let uid = uid.to_string();
        // On the engine's own threads, never the caller's. This is minutes of
        // blocking work and it must not be able to touch whatever runtime asked
        // for it.
        self.runtime.handle().spawn(async move {
            let outcome = engine.bring_in(&uid).await;
            // Removed whatever happened, so a model that failed to load can be
            // tried again rather than reading as waking forever.
            engine.loading.write().await.remove(&uid);
            match outcome {
                Ok(()) => {
                    engine.failures.write().await.remove(&uid);
                    tracing::info!(uid, "model resident");
                }
                // KEPT, not only logged. There is no caller left to return it
                // to, so residency says released while the entry still says the
                // administrator wanted it resident. That disagreement is what
                // the screen shows, and this is the sentence that makes it mean
                // something: "the engine cannot load this architecture" and
                // "there was no room" are the same picture without it.
                Err(err) => {
                    engine
                        .failures
                        .write()
                        .await
                        .insert(uid.clone(), err.to_string());
                    tracing::error!(uid, %err, "model could not be loaded");
                }
            }
        });
        Ok(())
    }

    async fn wait_loaded(&self, uid: &str) -> Result<()> {
        // Waits for the work, but does not RUN it here: the caller is told when
        // the model is in, while the building happens on the engine's threads.
        let engine = self.clone_handles();
        let uid = uid.to_string();
        self.runtime
            .handle()
            .spawn(async move { engine.bring_in(&uid).await })
            .await
            .map_err(|err| Error::engine(format!("the engine stopped while loading: {err}")))?
    }

    async fn unload(&self, uid: &str) -> Result<()> {
        // Cloned OUT of the lock before anything is awaited. Releasing weights
        // takes real time, and holding a read lock across it would stop every
        // other model being loaded for the whole of it.
        let instance = self.live.read().await.as_ref().map(|l| l.instance.clone());
        let Some(instance) = instance else {
            // Nothing is loaded at all, so this model is already out of memory.
            return Ok(());
        };

        // Releasing is ONE synchronous call that returns when the weights have
        // gone back to the operating system, which for a model of any size is
        // seconds. On the runtime serving HTTP it held a worker for all of it,
        // and that is how releasing a model hung the console and the node with
        // it: the memory was freed and the answer never came. It belongs on the
        // engine's threads, and on a blocking one, because it does not await.
        let uid = uid.to_string();
        let released = tokio::time::timeout(
            RELEASE_DEADLINE,
            self.runtime
                .handle()
                .spawn_blocking(move || instance.unload_model(&uid)),
        )
        .await
        .map_err(|_| {
            Error::engine(
                "the engine did not release this model. The memory it holds can only be \
                 reclaimed by restarting this machine's service",
            )
        })?
        .map_err(|err| Error::engine(format!("the engine stopped while releasing: {err}")))?;

        match released {
            Ok(()) => Ok(()),
            // Already released is the state that was asked for.
            Err(err) if is_already_unloaded(&err) => Ok(()),
            Err(err) => Err(Error::engine(err)),
        }
    }

    async fn failures(&self) -> Result<BTreeMap<String, String>> {
        Ok(self.failures.read().await.clone())
    }

    async fn residency(&self) -> Result<BTreeMap<String, Residency>> {
        // Everything on the disk starts as released: this node can produce it,
        // it is simply not in memory. What the engine says then overrides that
        // for the models it actually holds.
        let mut all: BTreeMap<String, Residency> = self
            .known
            .read()
            .await
            .keys()
            .map(|uid| (uid.clone(), Residency::Released))
            .collect();

        // What is on its way in, before the engine is asked: a model being built
        // is not yet in the engine's own map, so without this it would report as
        // released and a screen would show nothing happening.
        for uid in self.loading.read().await.iter() {
            if let Some(slot) = all.get_mut(uid) {
                *slot = Residency::Waking;
            }
        }

        if let Some(live) = self.live.read().await.as_ref() {
            let reported = live
                .instance
                .list_models_with_status()
                .map_err(Error::engine)?;
            for (uid, status) in reported {
                // A model the engine holds that this node has no record of is
                // not reported at all. It cannot be acted on through any of our
                // calls, so listing it would be a row nothing can be done with.
                // A model the engine holds that this node has no record of is
                // not reported at all, and one that is still being built keeps
                // `waking`: the engine's own map does not know about it yet.
                if let Some(slot) = all.get_mut(&uid)
                    && *slot != Residency::Waking
                {
                    *slot = translate(status);
                }
            }
        }

        Ok(all)
    }

    fn can_run(&self, architecture: Option<&str>, kind: crate::catalog::ModelKind) -> Verdict {
        can_run(architecture, kind)
    }

    fn routes(&self) -> Option<Router> {
        let live = self.live.clone();
        // A fallback rather than a copy of the engine's route list. Its routes
        // do not exist until a model is loaded, and restating them here would be
        // a second list to keep in step with a library we do not own.
        Some(Router::new().fallback(move |request: Request| {
            let live = live.clone();
            async move { infer(live, request).await }
        }))
    }
}

impl LocalEngine {
    /// A second handle on the same engine, for the task a load runs on.
    ///
    /// Every field is already behind an `Arc`, so this shares the state rather
    /// than copying it: the spawned load writes to the same maps the request
    /// that started it will be asked about.
    fn clone_handles(&self) -> Self {
        Self {
            known: self.known.clone(),
            live: self.live.clone(),
            built: self.built.clone(),
            loading: self.loading.clone(),
            failures: self.failures.clone(),
            runtime: self.runtime.clone(),
        }
    }
}

// Hand a request to the engine's own routes, if there are any yet.
async fn infer(live: Arc<RwLock<Option<Live>>>, request: Request) -> Response {
    use tower::ServiceExt;

    let routes = match live.read().await.as_ref() {
        Some(live) => live.routes.clone(),
        None => {
            // The honest answer for a node that has been set up but has nothing
            // loaded. A 503 says "not now" rather than "no such thing", which is
            // what lets a caller retry rather than give up on this node.
            return (
                StatusCode::SERVICE_UNAVAILABLE,
                "no model is loaded on this node",
            )
                .into_response();
        }
    };

    match routes.oneshot(request).await {
        Ok(response) => response.into_response(),
        Err(never) => match never {},
    }
}

/// Whether this engine has a loader for a model that declares itself this way.
///
/// # Why it asks the engine's own tables and keeps no list
///
/// The obvious implementation is a list of architectures we support. It would
/// be wrong within one release, and wrong SILENTLY, in both directions: models
/// added upstream would be refused here for no reason a person could see, and
/// models dropped upstream would be promised and then fail at load, which is
/// the exact failure this function exists to prevent. So the question is put to
/// the three tables the engine actually consults when it decides which loader
/// to build. There is one list, it belongs to the thing that does the work, and
/// it cannot drift from itself.
///
/// The three are the whole of what can be built from a repository like this:
/// text generation, multimodal, and embeddings. Speech synthesis and image
/// generation are reached another way and are not offered here.
pub fn can_run(architecture: Option<&str>, kind: crate::catalog::ModelKind) -> Verdict {
    // Nothing declared. Allowed, deliberately: see `Engine::can_run`.
    let Some(architecture) = architecture.map(str::trim).filter(|a| !a.is_empty()) else {
        return Verdict::allowed();
    };

    let known = NormalLoaderType::from_causal_lm_name(architecture).is_ok()
        || MultimodalLoaderType::from_causal_lm_name(architecture).is_ok()
        || EmbeddingLoaderType::from_causal_lm_name(architecture).is_ok();

    if known {
        Verdict::allowed()
    } else {
        Verdict::refused(why_not(kind))
    }
}

/// The refusal, in words somebody who has never read any of this would use.
///
/// The decision above was made on the architecture, which is exact and is also
/// a class name nobody says out loud. This is the other half: what a person
/// gets told. It is phrased from the KIND, which is a poor thing to decide on
/// and a good thing to speak with, and it says three things in order, because
/// leaving any of them out sends somebody back to the same screen: this will
/// not work here, what this model is, and what would work instead.
///
/// It names no part of the stack, and the suite checks that it does not.
fn why_not(kind: crate::catalog::ModelKind) -> String {
    use crate::catalog::ModelKind;

    // What this machine CAN do, said once, because every branch needs it.
    const INSTEAD: &str = "This machine runs models that write text, models that read images and \
                           write about them, and models that turn text into numbers for search.";

    let what_it_is = match kind {
        ModelKind::Stt => "This one listens to recordings and writes down the words",
        ModelKind::Tts => "This one turns text into speech",
        ModelKind::Rerank => "This one scores search results against a question",
        // Chat and Embedding are kinds this machine does run, so arriving here
        // means the LABEL says something it can do while the weights are built
        // in a way it has no loader for. Saying "this is a text model and we do
        // not run text models" would be a lie; what is true is narrower.
        ModelKind::Chat | ModelKind::Embedding => {
            return format!(
                "This machine cannot run this particular model. It is built in a way that nothing \
                 here knows how to load, even though it does the same job as models that do work \
                 here. {INSTEAD} Another model of the same sort will very likely load."
            );
        }
    };

    format!(
        "This machine cannot run this model. {what_it_is}, which is not something it can do. {INSTEAD}"
    )
}

/// Whether a failure to unload is just the model already being out of memory.
fn is_already_unloaded(err: &mistralrs_core::MistralRsError) -> bool {
    matches!(
        err,
        mistralrs_core::MistralRsError::ModelAlreadyUnloaded(_)
            | mistralrs_core::MistralRsError::ModelNotFound(_)
    )
}

/// The engine's word for where a model is, in ours.
fn translate(status: ModelStatus) -> Residency {
    match status {
        ModelStatus::Loaded => Residency::Resident,
        ModelStatus::Unloaded => Residency::Released,
        ModelStatus::Reloading => Residency::Waking,
    }
}

/// Whether a model already in the engine has to be built again rather than
/// simply reloaded.
///
/// Every setting that shapes a pipeline is applied in `build_model`, and a
/// reload never reaches it: it restores what the engine already holds. So the
/// question is only ever "is what it holds what we now want".
///
/// An unknown answer (nothing recorded) means rebuild. That happens when the
/// engine has a model this process did not put there, after a restart, and
/// guessing that it matches would be guessing in the direction that fails
/// silently.
fn needs_rebuild(
    built: Option<&BTreeMap<String, String>>,
    wanted: &BTreeMap<String, String>,
) -> bool {
    built != Some(wanted)
}

/// Turn a model and its settings into something the engine can build.
///
/// A setting that will not parse is REFUSED here rather than dropped. Silently
/// ignoring "16x" in a number field would load the model with a different value
/// than the one on the screen, and nothing would ever say so.
fn build_model(uid: &str, known: &Known) -> Result<mistralrs::AnyModelBuilder> {
    // The auto builder reads the weights and picks the right kind of pipeline,
    // which is the one decision we would otherwise be making from a filename.
    let mut builder = mistralrs::ModelBuilder::new(known.weights.display().to_string());

    if let Some(raw) = setting(known, "quantization") {
        // Resolved without naming a device on purpose: this decides WHICH
        // compression, and where it will run is the load's business. Passing a
        // device here would make the same stored setting mean different things
        // on two machines.
        let isq = parse_isq_value(raw, None).map_err(|_| {
            Error::invalid(format!("{raw:?} is not a compression this node offers"))
        })?;
        builder = builder.with_isq(isq);
    }

    if let Some(raw) = setting(known, "max_sequences") {
        builder = builder.with_max_num_seqs(number(raw, "concurrent requests")?);
    }

    if let Some(raw) = setting(known, "prefix_cache")
        && raw.eq_ignore_ascii_case("off")
    {
        builder = builder.with_prefix_cache_n(None);
    }

    if let Some(raw) = setting(known, "chat_template") {
        builder = builder.with_chat_template(raw);
    }

    if let Some(raw) = setting(known, "device_layers") {
        let layers: usize = number(raw, "layers on the accelerator")?;
        builder = builder.with_device_mapping(DeviceMapSetting::Map(
            DeviceMapMetadata::from_num_device_layers(vec![DeviceLayerMapMetadata {
                ordinal: 0,
                layers,
            }]),
        ));
    }

    // The two cache settings are one call, so they are read together.
    let cache_mb = setting(known, "cache_memory_mb")
        .map(|raw| number::<usize>(raw, "cache memory"))
        .transpose()?;
    let context = setting(known, "context_length")
        .map(|raw| number::<usize>(raw, "context length"))
        .transpose()?;
    if cache_mb.is_some() || context.is_some() {
        let memory = match cache_mb {
            Some(mb) => MemoryGpuConfig::MbAmount(mb),
            // Asking for a context length without saying how much memory to
            // reserve means "work it out", which is what the engine's own
            // default does.
            None => MemoryGpuConfig::ContextSize(context.unwrap_or_default()),
        };
        let paged = PagedAttentionConfig::new(None, memory, PagedCacheType::Auto)
            .map_err(|err| Error::invalid(format!("those cache settings do not work: {err}")))?;
        builder = builder.with_paged_attn(paged);
    }

    tracing::debug!(uid, settings = known.settings.len(), "model prepared");
    Ok(builder.into())
}

/// A setting that was actually filled in.
fn setting<'a>(known: &'a Known, key: &str) -> Option<&'a str> {
    known
        .settings
        .get(key)
        .map(String::as_str)
        .map(str::trim)
        .filter(|v| !v.is_empty())
}

fn number<T: std::str::FromStr>(raw: &str, what: &str) -> Result<T> {
    raw.parse()
        .map_err(|_| Error::invalid(format!("{raw:?} is not a number for {what}")))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn known(pairs: &[(&str, &str)]) -> Known {
        Known {
            weights: PathBuf::from("/data/models/uid-1/weights"),
            handle: "Model-3B".into(),
            settings: pairs
                .iter()
                .map(|(k, v)| ((*k).to_string(), (*v).to_string()))
                .collect(),
        }
    }

    // --- what this engine can be asked to load ------------------------------
    //
    // The cost of getting these wrong is asymmetric and both directions are
    // real. Refusing something loadable hides a model from somebody with no way
    // to find out why. Accepting something unloadable is the hour of downloading
    // that this whole verdict exists to stop.

    use crate::catalog::ModelKind;

    #[test]
    fn every_architecture_we_believe_is_loadable_still_is() {
        // What this guards is DRIFT IN THE DIRECTION THAT LIES. `can_run` asks
        // the engine's own table, so a new architecture upstream is picked up
        // for free and needs no test. The failure a test can catch is the other
        // one: an upstream release that RENAMES or DROPS a class we are today
        // telling people they can download. That turns into a model offered on
        // the screen and refused at load, which is the fault this whole verdict
        // exists to prevent, arriving through the back door of a dependency
        // bump. If this list stops matching the engine, that is the signal.
        for class in KNOWN_LOADABLE_CLASSES {
            let verdict = can_run(Some(class), ModelKind::Chat);
            assert!(
                verdict.ok,
                "{class} was loadable and is not any more: {:?}. Either the engine dropped it, in \
                 which case remove it here and expect models to disappear from the screen, or the \
                 gate is broken.",
                verdict.reason
            );
        }
    }

    /// Class names this engine had a loader for when this was written.
    ///
    /// NOT the gate. The gate is the engine's own table; this is the tripwire
    /// described above, and it is deliberately a plain list of the strings that
    /// appear in a repository's configuration, because that is the form the
    /// question is actually asked in.
    const KNOWN_LOADABLE_CLASSES: &[&str] = &[
        "MistralForCausalLM",
        "MixtralForCausalLM",
        "GemmaForCausalLM",
        "Gemma2ForCausalLM",
        "PhiForCausalLM",
        "Phi3ForCausalLM",
        "LlamaForCausalLM",
        "Qwen2ForCausalLM",
        "Starcoder2ForCausalLM",
        "PhiMoEForCausalLM",
        "DeepseekV2ForCausalLM",
        "DeepseekV3ForCausalLM",
        "Qwen3ForCausalLM",
        "Glm4ForCausalLM",
        "Glm4MoeLiteForCausalLM",
        "Glm4MoeForCausalLM",
        "Qwen3MoeForCausalLM",
        "SmolLM3ForCausalLM",
        "GraniteMoeHybridForCausalLM",
        "GptOssForCausalLM",
        "HunYuanDenseV1ForCausalLM",
        "HunYuanMoEV1ForCausalLM",
        "Qwen3NextForCausalLM",
        "Lfm2ForCausalLM",
        "Lfm2MoeForCausalLM",
        // Multimodal, which is the second of the three tables and the reason a
        // model that reads images is not turned away as "not text".
        "Qwen2VLForConditionalGeneration",
        "Qwen2_5_VLForConditionalGeneration",
        "Qwen3VLForConditionalGeneration",
        "LlavaForConditionalGeneration",
        "LlavaNextForConditionalGeneration",
        "MllamaForConditionalGeneration",
        "Gemma3ForConditionalGeneration",
        "Mistral3ForConditionalGeneration",
        "Llama4ForConditionalGeneration",
        "Idefics3ForConditionalGeneration",
        "Phi3VForCausalLM",
        "Phi4MMForCausalLM",
        // Embeddings, the third. Two only, which is worth knowing: most of what
        // people reach for when they want embeddings is a sentence-transformers
        // model this engine has no loader for, and it is now told so before the
        // download rather than after.
        "Gemma3TextModel",
    ];

    #[test]
    fn the_model_that_cost_an_evening_is_refused_before_it_is_fetched() {
        // A real one: 1.3 GB of speech recognition weights that passed the disk
        // check, the memory check and the credentials check, downloaded, and
        // then could not be loaded by anything on the machine.
        let verdict = can_run(Some("Wav2Vec2ForCTC"), ModelKind::Stt);
        assert!(!verdict.ok, "the speech model was approved again");
        let reason = verdict.reason.expect("refused without saying why");
        assert!(
            reason.contains("recordings") || reason.contains("words"),
            "the reason does not say what the model is: {reason}"
        );
    }

    #[test]
    fn a_refusal_is_plain_english_and_names_no_part_of_the_stack() {
        // The same rule the machine's own verdicts are held to. A person
        // reading this has not heard of any of these words, and a refusal that
        // uses them is a refusal they cannot act on.
        for kind in [
            ModelKind::Stt,
            ModelKind::Tts,
            ModelKind::Rerank,
            ModelKind::Chat,
            ModelKind::Embedding,
        ] {
            let verdict = can_run(Some("SomethingNobodyImplements"), kind);
            assert!(!verdict.ok);
            let reason = verdict.reason.unwrap();
            for banned in [
                "architecture",
                "loader",
                "mistral",
                "cuda",
                "gpu",
                "vram",
                "causallm",
                "safetensors",
                "pipeline",
            ] {
                assert!(
                    !reason.to_lowercase().contains(banned),
                    "{reason:?} names {banned}"
                );
            }
            // And it must say what to do, not only that it will not work.
            assert!(
                reason.contains("This machine runs") || reason.contains("very likely load"),
                "the refusal does not say what would work instead: {reason}"
            );
        }
    }

    #[test]
    fn a_model_that_declares_nothing_is_allowed_rather_than_guessed_at() {
        // Refusing what cannot be judged would hide working models. The
        // download's own failure is the backstop, which is the same trade the
        // disk check makes for a mount it cannot read.
        assert!(can_run(None, ModelKind::Chat).ok);
        assert!(can_run(Some(""), ModelKind::Chat).ok);
        assert!(can_run(Some("   "), ModelKind::Chat).ok);
    }

    #[test]
    fn a_supported_kind_built_an_unsupported_way_is_not_told_it_is_the_wrong_kind() {
        // The lie worth avoiding: this machine DOES run text models, so telling
        // somebody it does not, because one repository is built unusually,
        // sends them away from every model that would have worked.
        let reason = can_run(Some("NotAThingForCausalLM"), ModelKind::Chat)
            .reason
            .unwrap();
        assert!(
            reason.contains("this particular model"),
            "a text model was refused as though the whole kind were unsupported: {reason}"
        );
    }

    #[test]
    fn the_engines_words_become_ours() {
        assert_eq!(translate(ModelStatus::Loaded), Residency::Resident);
        assert_eq!(translate(ModelStatus::Unloaded), Residency::Released);
        // The state that exists so a cold start is visible rather than a stall.
        assert_eq!(translate(ModelStatus::Reloading), Residency::Waking);
    }

    #[test]
    fn a_model_with_no_settings_builds() {
        assert!(build_model("uid-1", &known(&[])).is_ok());
    }

    #[test]
    fn every_setting_the_form_offers_is_understood_here() {
        // The rule this enforces: a field on the form that nothing reads is a
        // setting somebody believes is in effect. Each key is applied on its
        // own, so a failure names the one that was forgotten.
        let samples = [
            ("quantization", "Q4K"),
            ("device_layers", "24"),
            ("max_sequences", "16"),
            ("context_length", "8192"),
            ("cache_memory_mb", "4096"),
            ("prefix_cache", "off"),
            ("chat_template", "{{ messages }}"),
        ];
        let offered = crate::form::keys();
        for (key, value) in samples {
            assert!(
                offered.contains(&key.to_string()),
                "{key} is not on the form"
            );
            build_model("uid-1", &known(&[(key, value)]))
                .unwrap_or_else(|e| panic!("{key} was on the form but not understood: {e}"));
        }
        assert_eq!(
            offered.len(),
            samples.len(),
            "the form has a field this test does not cover"
        );
    }

    #[test]
    fn a_model_is_rebuilt_when_its_settings_changed_and_reloaded_when_they_did_not() {
        // The defect this exists for: a reload restores the pipeline the engine
        // already holds, and every setting that shapes one is applied where a
        // reload never reaches. Taking that branch after a change meant a model
        // asked to load quantised came back at full precision, in seconds, and
        // nothing anywhere said so.
        let none: BTreeMap<String, String> = BTreeMap::new();
        let q4k: BTreeMap<String, String> = [("quantization".to_string(), "Q4K".to_string())]
            .into_iter()
            .collect();
        let q8: BTreeMap<String, String> = [("quantization".to_string(), "Q8_0".to_string())]
            .into_iter()
            .collect();

        // Unchanged: a reload is right, and is far cheaper.
        assert!(!needs_rebuild(Some(&q4k), &q4k));
        assert!(!needs_rebuild(Some(&none), &none));

        // Changed in any direction: rebuild.
        assert!(needs_rebuild(Some(&none), &q4k), "adding compression");
        assert!(needs_rebuild(Some(&q4k), &none), "removing compression");
        assert!(needs_rebuild(Some(&q4k), &q8), "changing compression");

        // Never built by this process, so nothing is known about it. Guessing
        // that it matches is guessing in the direction that fails silently.
        assert!(needs_rebuild(None, &q4k));
        assert!(needs_rebuild(None, &none));
    }

    #[test]
    fn a_number_that_is_not_one_is_refused_rather_than_ignored() {
        // Ignoring it would load the model with a value nobody chose, and the
        // screen would go on showing the one that was typed.
        for key in [
            "max_sequences",
            "device_layers",
            "context_length",
            "cache_memory_mb",
        ] {
            let Err(err) = build_model("uid-1", &known(&[(key, "sixteen")])) else {
                panic!("{key}: a value that is not a number was accepted");
            };
            assert!(err.to_string().contains("sixteen"), "{key}: {err}");
        }
    }

    #[test]
    fn a_compression_the_engine_does_not_have_is_refused() {
        let Err(err) = build_model("uid-1", &known(&[("quantization", "Q99K")])) else {
            panic!("an unknown compression was accepted");
        };
        assert!(err.to_string().contains("Q99K"), "{err}");
    }

    #[test]
    fn every_compression_the_form_offers_is_one_the_engine_has() {
        // The form is a promise: an administrator picking from a list must not
        // then be told at load time that the choice was not real.
        for section in crate::form::settings_form() {
            for field in section.fields {
                if field.key == "quantization" {
                    for option in field.options.iter().filter(|o| !o.is_empty()) {
                        assert!(
                            parse_isq_value(option, None).is_ok(),
                            "the form offers {option:?}, which the engine does not have"
                        );
                    }
                    return;
                }
            }
        }
        panic!("the form no longer has a quantization field");
    }

    #[test]
    fn a_blank_setting_is_treated_as_unset() {
        let known = known(&[("quantization", "   "), ("max_sequences", "")]);
        assert!(setting(&known, "quantization").is_none());
        assert!(setting(&known, "max_sequences").is_none());
        assert!(build_model("uid-1", &known).is_ok());
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn model_work_runs_on_the_engines_own_threads() {
        // The property the whole separation exists for, asserted as itself.
        // Building a model is minutes of blocking work and releasing one frees
        // gigabytes in a single synchronous call; both used to run on whatever
        // runtime asked for them, which was the one serving HTTP. A release then
        // hung the console and the node together, and an outbound TLS handshake
        // in the same process stopped completing.
        //
        // Named threads, because the name is the only evidence that survives
        // into the running system: if this ever reads as the test's own thread,
        // model work is back on the caller's runtime.
        let engine = LocalEngine::empty().expect("the engine needs threads");

        let ran_on = engine
            .runtime
            .handle()
            .spawn(async { std::thread::current().name().map(str::to_string) })
            .await
            .expect("the engine's threads ran the work");

        assert_eq!(
            ran_on.as_deref(),
            Some("sag-model"),
            "model work ran on the caller's runtime instead of the engine's"
        );
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn letting_the_engine_go_from_inside_a_runtime_is_not_a_panic() {
        // Dropping a runtime waits for its threads to stop, and waiting from
        // inside another runtime's worker panics. Giving the engine threads of
        // its own created exactly that hazard, since the engine is dropped
        // wherever the last handle to it happens to go. This is that case, and
        // it is a test rather than a comment because the failure is a crash on
        // shutdown that no other test would reach.
        let engine = LocalEngine::empty().expect("the engine needs threads");
        drop(engine);

        // And the runtime this test is on is still working afterwards.
        tokio::task::yield_now().await;
    }

    #[tokio::test]
    async fn a_fresh_engine_knows_nothing_and_serves_no_inference_yet() {
        let engine = LocalEngine::empty().expect("the engine needs threads");
        assert!(engine.residency().await.unwrap().is_empty());
        // The routes exist from boot even though the engine behind them does
        // not, which is what lets a node be wired up before it has a model.
        assert!(engine.routes().is_some());
    }

    #[tokio::test]
    async fn a_registered_model_is_released_before_anything_is_loaded() {
        let engine = LocalEngine::empty().expect("the engine needs threads");
        let entry = ModelEntry {
            uid: "uid-1".into(),
            repo: "vendor/model".into(),
            revision: "abc".into(),
            name: "model".into(),
            handle: "model".into(),
            kind: crate::catalog::ModelKind::Chat,
            facts: Default::default(),
            settings: Default::default(),
            resident: false,
            added_at: chrono::Utc::now(),
        };
        engine
            .register(&entry, Path::new("/data/models/uid-1/weights"))
            .await
            .unwrap();

        assert_eq!(
            engine.residency().await.unwrap().get("uid-1"),
            Some(&Residency::Released)
        );
        // Unloading something nothing has loaded is the state that was asked
        // for, not a failure.
        engine.unload("uid-1").await.expect("unload refused");
        engine.forget("uid-1").await.expect("forget refused");
        assert!(engine.residency().await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn a_load_returns_at_once_and_the_model_reads_as_waking() {
        // The whole reason a load is started rather than awaited: a browser
        // cannot hold a request open for minutes, so the answer has to be a
        // state somebody can watch.
        let engine = LocalEngine::empty().expect("the engine needs threads");
        let entry = ModelEntry {
            uid: "uid-1".into(),
            repo: "vendor/model".into(),
            revision: "abc".into(),
            name: "model".into(),
            handle: "model".into(),
            kind: crate::catalog::ModelKind::Chat,
            facts: Default::default(),
            settings: Default::default(),
            resident: false,
            added_at: chrono::Utc::now(),
        };
        engine
            .register(&entry, Path::new("/data/models/uid-1/weights"))
            .await
            .unwrap();

        engine.load("uid-1").await.expect("the load was refused");
        // The weights do not exist, so the spawned build will fail. What is
        // asserted is that the CALL came back without waiting for it.
        assert_eq!(
            engine.residency().await.unwrap().get("uid-1"),
            Some(&Residency::Waking)
        );

        // Pressing again while it is on its way does not start a second build.
        engine
            .load("uid-1")
            .await
            .expect("a second press was refused");
        assert_eq!(engine.loading.read().await.len(), 1);
    }

    #[tokio::test]
    async fn loading_a_model_this_node_never_heard_of_is_a_not_found() {
        let engine = LocalEngine::empty().expect("the engine needs threads");
        assert!(matches!(engine.load("nope").await, Err(Error::NotFound(_))));
    }

    #[tokio::test]
    async fn a_node_with_nothing_loaded_says_not_now_rather_than_no_such_thing() {
        use tower::ServiceExt;

        let engine = LocalEngine::empty().expect("the engine needs threads");
        let router = engine.routes().expect("no routes");
        let response = router
            .oneshot(
                axum::http::Request::builder()
                    .uri("/v1/chat/completions")
                    .method("POST")
                    .body(axum::body::Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        // 503 and not 404: the caller should try this node again later, not
        // conclude it does not serve inference.
        assert_eq!(response.status(), StatusCode::SERVICE_UNAVAILABLE);
    }
}
