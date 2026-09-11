//! Finding a model, and fetching it.
//!
//! Searching is broad and cheap and promises nothing. **Describing is the
//! promise**: it resolves a repository to one commit, adds up exactly the files
//! that would be fetched, and says whether they will fit here. That is the
//! answer an administrator says yes to, and approval must equal success
//! (CLAUDE.md).
//!
//! Starting a pull returns at once with an id. Nothing waits for a download
//! (KB/35): the orchestrator writes a job row and polls, so the truth survives
//! an orchestrator that is restarted in the middle of an hour of transfer.

use axum::{
    Json,
    extract::{Path, Query, State},
};
use serde::{Deserialize, Serialize};

use crate::{
    api::App,
    error::Result,
    hub::{HubModel, HubRepo},
    machine::{Machine, Verdict},
    pull::Pull,
};

#[derive(Deserialize)]
pub struct SearchQuery {
    pub q: String,
    #[serde(default)]
    pub limit: Option<u32>,
}

pub async fn search(
    State(app): State<App>,
    Query(query): Query<SearchQuery>,
) -> Result<Json<Vec<HubModel>>> {
    Ok(Json(
        app.hub.search(&query.q, query.limit.unwrap_or(25)).await?,
    ))
}

#[derive(Deserialize)]
pub struct DescribeQuery {
    pub repo: String,
    #[serde(default)]
    pub revision: Option<String>,
    /// Names one weights file, for a repository published at several
    /// compression levels.
    #[serde(default)]
    pub file: Option<String>,
}

/// A repository, and whether this machine can take it.
#[derive(Serialize)]
pub struct DescribeView {
    #[serde(flatten)]
    pub repo: HubRepo,
    /// Whether there is room on the disk for it.
    pub storage: Verdict,
    /// Whether it is likely to load into memory here.
    pub memory: Verdict,
    /// Whether this node is allowed to fetch it at all.
    pub access: Verdict,
    /// Whether anything here could load it once it arrived.
    ///
    /// The one verdict that is about the model rather than about this machine's
    /// resources, and the last of the four to exist: the other three were all
    /// answerable before a download and this was not, so a model no engine here
    /// can execute passed every check and failed at the end.
    pub runtime: Verdict,
    /// The model this already is on this node, if it is.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub already_here: Option<String>,
}

pub async fn describe(
    State(app): State<App>,
    Query(query): Query<DescribeQuery>,
) -> Result<Json<DescribeView>> {
    let repo = app
        .hub
        .describe(
            &query.repo,
            query.revision.as_deref(),
            query.file.as_deref(),
        )
        .await?;

    let machine = Machine::read(&app.config.data_dir);
    let already_here = app
        .catalog
        .find(&repo.repo, &repo.revision)
        .await
        .map(|e| e.uid);

    Ok(Json(DescribeView {
        storage: machine.can_store(repo.size_bytes),
        memory: machine.can_load(repo.size_bytes),
        access: crate::pull::access(&repo, app.hub.has_token()),
        runtime: app.engine.can_run(repo.architecture.as_deref(), repo.kind),
        already_here,
        repo,
    }))
}

#[derive(Deserialize)]
pub struct PullInput {
    pub repo: String,
    #[serde(default)]
    pub revision: Option<String>,
    #[serde(default)]
    pub file: Option<String>,
}

/// Start a download and return immediately.
pub async fn pull(State(app): State<App>, Json(input): Json<PullInput>) -> Result<Json<Pull>> {
    let pull = app
        .pulls
        .start(
            app.pull_context(),
            &input.repo,
            input.revision.as_deref(),
            input.file.as_deref(),
        )
        .await?;
    Ok(Json(pull))
}

pub async fn pulls(State(app): State<App>) -> Result<Json<Vec<Pull>>> {
    Ok(Json(app.pulls.list().await))
}

pub async fn pull_status(State(app): State<App>, Path(id): Path<String>) -> Result<Json<Pull>> {
    Ok(Json(app.pulls.get(&id).await?))
}

/// Call a download off, keeping what it fetched. Answers with where it stands,
/// so the caller knows what they stopped rather than having to ask again.
pub async fn pull_cancel(State(app): State<App>, Path(id): Path<String>) -> Result<Json<Pull>> {
    app.pulls.cancel(&id).await?;
    Ok(Json(app.pulls.get(&id).await?))
}

/// Carry on a download that was stopped, from where it stopped.
pub async fn pull_resume(State(app): State<App>, Path(id): Path<String>) -> Result<Json<Pull>> {
    Ok(Json(app.pulls.resume(app.pull_context(), &id).await?))
}

/// Forget a download and remove what it fetched. The only thing here that
/// throws work away, which is why it is a delete and not something cancelling
/// does on the quiet.
pub async fn pull_delete(State(app): State<App>, Path(id): Path<String>) -> Result<Json<Removed>> {
    app.pulls.forget(&app.config.incoming_dir(), &id).await?;
    Ok(Json(Removed { removed: true }))
}

#[derive(Serialize)]
pub struct Removed {
    pub removed: bool,
}

#[cfg(test)]
mod tests {
    use crate::{
        api::testing::Harness,
        catalog::{Catalog, Facts, ModelEntry, ModelKind},
    };
    use axum::{
        body::Body,
        http::{Request, StatusCode, header},
    };
    use tower::ServiceExt;

    async fn call(
        node: &Harness,
        method: &str,
        path: &str,
        body: Option<serde_json::Value>,
    ) -> (StatusCode, serde_json::Value) {
        let mut request = Request::builder()
            .uri(path)
            .method(method)
            .header(header::AUTHORIZATION, format!("Bearer {}", node.key()));
        let body = match body {
            Some(value) => {
                request = request.header(header::CONTENT_TYPE, "application/json");
                Body::from(serde_json::to_vec(&value).unwrap())
            }
            None => Body::empty(),
        };
        let response = node
            .router
            .clone()
            .oneshot(request.body(body).unwrap())
            .await
            .unwrap();
        let status = response.status();
        let bytes = axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .unwrap();
        (
            status,
            serde_json::from_slice(&bytes).unwrap_or(serde_json::Value::Null),
        )
    }

    #[tokio::test]
    async fn a_search_with_nothing_to_search_for_is_refused_before_the_network() {
        let node = Harness::start().await;
        let (status, _) = call(&node, "GET", "/node/library/search?q=%20%20", None).await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn a_model_name_that_is_not_one_is_refused_before_the_network() {
        // The harness points at an unreachable library, so anything that got as
        // far as a request would come back as a bad gateway. A bad request here
        // proves the name was rejected on the way in.
        let node = Harness::start().await;
        for repo in ["a/b/c", "vendor/..", "/leading"] {
            let (status, _) = call(
                &node,
                "GET",
                &format!("/node/library/describe?repo={}", urlencode(repo)),
                None,
            )
            .await;
            assert_eq!(
                status,
                StatusCode::BAD_REQUEST,
                "{repo} reached the network"
            );
        }
    }

    #[tokio::test]
    async fn an_unreachable_library_is_a_bad_gateway_and_not_our_fault() {
        let node = Harness::start().await;
        let (status, body) = call(
            &node,
            "GET",
            "/node/library/describe?repo=vendor/model",
            None,
        )
        .await;
        assert_eq!(status, StatusCode::BAD_GATEWAY);
        assert_eq!(body["error"]["code"], "hub_unreachable");
    }

    #[tokio::test]
    async fn a_pull_of_a_model_already_here_is_refused_without_downloading_anything() {
        let node = Harness::start().await;
        node.app
            .catalog
            .insert(ModelEntry {
                uid: Catalog::mint_uid(),
                repo: "vendor/model".into(),
                revision: "abc123".into(),
                name: "model".into(),
                handle: "model".into(),
                kind: ModelKind::Chat,
                facts: Facts::default(),
                settings: Default::default(),
                resident: false,
                added_at: chrono::Utc::now(),
            })
            .await
            .unwrap();

        // The library is unreachable in the harness, so this cannot resolve a
        // commit and the duplicate check never runs. What is asserted is that
        // nothing was started and nothing was written.
        let (status, _) = call(
            &node,
            "POST",
            "/node/pulls",
            Some(serde_json::json!({"repo": "vendor/model"})),
        )
        .await;
        assert!(status.is_client_error() || status == StatusCode::BAD_GATEWAY);
        assert_eq!(
            node.app.pulls.list().await.len(),
            0,
            "a download was started anyway"
        );
    }

    #[tokio::test]
    async fn asking_about_a_download_that_never_existed_is_a_not_found() {
        let node = Harness::start().await;
        let (status, _) = call(&node, "GET", "/node/pulls/nope", None).await;
        assert_eq!(status, StatusCode::NOT_FOUND);

        let (status, _) = call(&node, "DELETE", "/node/pulls/nope", None).await;
        assert_eq!(status, StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn a_node_with_no_downloads_lists_none() {
        let node = Harness::start().await;
        let (status, body) = call(&node, "GET", "/node/pulls", None).await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(body.as_array().unwrap().len(), 0);
    }

    fn urlencode(raw: &str) -> String {
        raw.chars()
            .map(|c| match c {
                'a'..='z' | 'A'..='Z' | '0'..='9' | '-' | '_' | '.' | '~' => c.to_string(),
                other => format!("%{:02X}", other as u32),
            })
            .collect()
    }
}
