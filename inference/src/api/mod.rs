//! The node's surface.
//!
//! Two halves on one port, deliberately. The engine's own routes are merged in
//! untouched, so the orchestrator reaches this node's models through the exact
//! surface it already talks to every hosted vendor through: a local model is
//! indistinguishable from a cloud one everywhere except the row it came from
//! (KB/35). Ours live under `/node` and cannot collide with them.
//!
//! One key guards both, because they are one machine. A caller that could ask a
//! model a question but not list the models would be a distinction without a
//! boundary.

mod auth;
mod library;
mod models;
mod node;
mod stream;

use std::sync::Arc;

use axum::{
    Router,
    extract::Request,
    middleware::Next,
    response::Response,
    routing::{delete, get, post, put},
};
use chrono::{DateTime, Utc};

use crate::{catalog::Catalog, config::Config, engine::Engine, hub::Hub, pull::Pulls};

/// Everything a handler can reach. Cloned per request, so every field in it is
/// cheap to clone by design.
#[derive(Clone)]
pub struct App {
    pub config: Arc<Config>,
    /// The key every request must carry, resolved at boot: what an administrator
    /// set, or what this machine minted for itself. `config.key` is only the
    /// former, so nothing may read it in place of this.
    pub key: Arc<str>,
    pub catalog: Catalog,
    pub pulls: Pulls,
    pub hub: Arc<Hub>,
    pub engine: Arc<dyn Engine>,
    pub started_at: DateTime<Utc>,
}

impl App {
    pub fn new(
        config: Arc<Config>,
        key: impl Into<Arc<str>>,
        catalog: Catalog,
        hub: Arc<Hub>,
        engine: Arc<dyn Engine>,
    ) -> Self {
        Self {
            config,
            key: key.into(),
            catalog,
            pulls: Pulls::new(),
            hub,
            engine,
            started_at: Utc::now(),
        }
    }

    /// What a pull needs, gathered from what the node already holds.
    pub fn pull_context(&self) -> crate::pull::PullContext {
        crate::pull::PullContext {
            hub: self.hub.clone(),
            catalog: self.catalog.clone(),
            engine: self.engine.clone(),
            incoming: self.config.incoming_dir(),
        }
    }
}

/// One line per request, the same record the gateway keeps.
///
/// It used to have none. The trace layer that was here logs at debug, and the
/// filter runs at info, so nothing a person asked this machine to do appeared in
/// its log at all. That was invisible while the control surface was the only
/// thing being used (it logs its own decisions), and it mattered the moment
/// inference started: a turn failed, and the machine that answered it had
/// nothing to say about being asked.
///
/// Deliberately including the ENGINE's routes. An answer that streamed fine from
/// here and broke on the way back is a very different problem from one that
/// never arrived, and the only way to tell them apart is a record of what was
/// asked.
async fn record(request: Request, next: Next) -> Response {
    let method = request.method().clone();
    let path = request.uri().path().to_string();
    let started = std::time::Instant::now();

    let response = next.run(request).await;

    let status = response.status().as_u16();
    let took = started.elapsed();
    // A refusal or a fault is worth finding in a quiet log; everything else is
    // the ordinary record.
    if response.status().is_client_error() || response.status().is_server_error() {
        tracing::warn!(%method, path, status, ?took, "request");
    } else {
        tracing::info!(%method, path, status, ?took, "request");
    }
    response
}

/// The whole surface this node serves.
pub fn router(app: App) -> Router {
    let engine_routes = app.engine.routes();

    // Everything that needs the key. The engine's routes are inside this, not
    // beside it: an inference call is the most sensitive thing here, not the
    // least.
    let mut guarded = Router::new()
        .nest("/node", node_routes())
        .with_state(app.clone());
    if let Some(routes) = engine_routes {
        // Under our own layer. The engine serves the answer; the FRAMING of the
        // stream that carries it is ours, so a version bump cannot change our
        // protocol behind our back. See api::stream.
        guarded = guarded.merge(routes.layer(axum::middleware::from_fn(stream::own_the_stream)));
    }
    let guarded = guarded.layer(axum::middleware::from_fn_with_state(
        app.clone(),
        auth::require_key,
    ));

    Router::new()
        // Outside the key, and deliberately empty of information. A container
        // has to be able to ask whether this process is alive without holding a
        // credential, and the answer is the only thing it gets.
        .route("/live", get(|| async { "ok" }))
        .merge(guarded)
        .layer(axum::middleware::from_fn(record))
}

fn node_routes() -> Router<App> {
    Router::new()
        .route("/", get(node::describe))
        .route("/stats", get(node::stats))
        .route("/models", get(models::list))
        .route("/models/{uid}", get(models::get))
        .route("/models/{uid}", delete(models::remove))
        .route("/models/{uid}/form", get(models::form))
        .route("/models/{uid}/settings", put(models::settings))
        .route("/models/{uid}/load", post(models::load))
        .route("/models/{uid}/unload", post(models::unload))
        .route("/library/search", get(library::search))
        .route("/library/describe", get(library::describe))
        .route("/pulls", get(library::pulls).post(library::pull))
        .route("/pulls/{id}", get(library::pull_status))
        // Three different things, said as three different things. Stopping keeps
        // what arrived; resuming continues it; deleting is the only one that
        // throws bytes away.
        .route("/pulls/{id}/cancel", post(library::pull_cancel))
        .route("/pulls/{id}/resume", post(library::pull_resume))
        .route("/pulls/{id}", delete(library::pull_delete))
}

#[cfg(test)]
pub(crate) mod testing {
    //! One place that builds a node against the scripted engine, so a test says
    //! what it is testing instead of assembling five things first.

    use super::*;
    use crate::engine::scripted::ScriptedEngine;

    pub struct Harness {
        pub app: App,
        pub engine: Arc<ScriptedEngine>,
        pub router: Router,
        _temp: tempfile::TempDir,
    }

    impl Harness {
        pub async fn start() -> Self {
            Self::with_hub("https://example.invalid").await
        }

        pub async fn with_hub(hub_url: &str) -> Self {
            let temp = tempfile::tempdir().expect("no temporary directory");
            const KEY: &str = "0123456789abcdef0123456789abcdef";
            let config = Arc::new(Config {
                addr: "127.0.0.1:0".parse().unwrap(),
                key: Some(KEY.into()),
                data_dir: temp.path().to_path_buf(),
                name: "test-node".into(),
                hub_url: hub_url.into(),
                hub_token: None,
                gateway_url: None,
                join_token: None,
                advertise: None,
            });
            config.prepare_dirs().await.expect("directories");

            let catalog = Catalog::open(config.models_dir()).await.expect("catalogue");
            let hub = Arc::new(Hub::new(&config.hub_url, None));
            let engine = Arc::new(ScriptedEngine::new());

            let app = App::new(config, KEY, catalog, hub, engine.clone());
            let router = router(app.clone());
            Self {
                app,
                engine,
                router,
                _temp: temp,
            }
        }

        pub fn key(&self) -> &str {
            &self.app.key
        }
    }
}
