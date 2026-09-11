//! What this node is, and what it is doing.
//!
//! The orchestrator reads this to draw a machine on a screen and to decide
//! whether a download will fit. It is recomputed on every call rather than
//! cached, because a figure about free disk is worth exactly as much as it is
//! current: a cached one would approve a pull onto space another pull has since
//! taken.

use axum::{Json, extract::State};
use serde::Serialize;

use crate::{api::App, engine::Residency, error::Result, machine::Machine};

#[derive(Serialize)]
pub struct NodeInfo {
    pub name: String,
    /// Whether this build can actually run a model. False is a build with no
    /// engine, which is a thing tests use and nothing an operator deploys.
    pub can_infer: bool,
    pub version: &'static str,
    pub uptime_seconds: i64,
    pub models: usize,
    pub resident: usize,
    pub downloads_active: usize,
    pub machine: Machine,
}

pub async fn describe(State(app): State<App>) -> Result<Json<NodeInfo>> {
    let residency = app.engine.residency().await.unwrap_or_default();

    Ok(Json(NodeInfo {
        name: app.config.name.clone(),
        can_infer: app.engine.routes().is_some(),
        version: env!("CARGO_PKG_VERSION"),
        uptime_seconds: (chrono::Utc::now() - app.started_at).num_seconds(),
        models: app.catalog.list().await.len(),
        resident: residency
            .values()
            .filter(|r| **r == Residency::Resident)
            .count(),
        downloads_active: app.pulls.active().await,
        machine: Machine::read(&app.config.data_dir),
    }))
}

#[derive(Serialize)]
pub struct NodeStats {
    pub machine: Machine,
    /// What the models on this node take up on disk.
    pub models_bytes: u64,
    pub residency: std::collections::BTreeMap<String, Residency>,
}

pub async fn stats(State(app): State<App>) -> Result<Json<NodeStats>> {
    Ok(Json(NodeStats {
        machine: Machine::read(&app.config.data_dir),
        models_bytes: app.catalog.size_bytes().await,
        // An engine that cannot answer reports nothing rather than failing the
        // whole call: the disk figures beside it are still true and still worth
        // having on a screen.
        residency: app.engine.residency().await.unwrap_or_default(),
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::api::testing::Harness;
    use axum::{
        body::Body,
        http::{Request, StatusCode, header},
    };
    use tower::ServiceExt;

    async fn json(node: &Harness, path: &str) -> serde_json::Value {
        let request = Request::builder()
            .uri(path)
            .header(header::AUTHORIZATION, format!("Bearer {}", node.key()))
            .body(Body::empty())
            .unwrap();
        let response = node.router.clone().oneshot(request).await.unwrap();
        assert_eq!(response.status(), StatusCode::OK, "{path} failed");
        let body = axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .unwrap();
        serde_json::from_slice(&body).expect("the node answered something that is not JSON")
    }

    #[tokio::test]
    async fn a_fresh_node_describes_itself_honestly() {
        let node = Harness::start().await;
        let info = json(&node, "/node").await;

        assert_eq!(info["name"], "test-node");
        assert_eq!(info["models"], 0);
        assert_eq!(info["resident"], 0);
        assert_eq!(info["downloads_active"], 0);
        // The scripted engine serves no inference, and the node says so rather
        // than looking like a machine that can answer questions.
        assert_eq!(info["can_infer"], false);
        assert!(info["machine"]["memory_total"].as_u64().unwrap() > 0);
    }

    #[tokio::test]
    async fn the_count_of_resident_models_is_what_the_engine_says() {
        let node = Harness::start().await;
        node.engine.set("uid-1", Residency::Resident);
        node.engine.set("uid-2", Residency::Released);
        node.engine.set("uid-3", Residency::Waking);

        let info = json(&node, "/node").await;
        assert_eq!(
            info["resident"], 1,
            "waking or released counted as resident"
        );
    }

    #[tokio::test]
    async fn stats_survive_an_engine_that_cannot_answer() {
        // The disk figures are still true when the engine has fallen over, and
        // a screen that goes blank because of it is worse than one missing a
        // column.
        let node = Harness::start().await;
        node.engine.fail_next("the engine is not answering");

        let stats = json(&node, "/node/stats").await;
        assert!(stats["residency"].as_object().unwrap().is_empty());
        assert_eq!(stats["models_bytes"], 0);
        assert!(stats["machine"]["disk_total"].is_u64());
    }
}
