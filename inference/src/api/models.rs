//! The models this node holds.
//!
//! Two rules from KB/19 apply here as much as they do to a screen in the
//! console. **One request per dialog**: the settings form comes back with the
//! values already in it, because a form and its contents are one question.
//! **A write says what it did**: load, unload and delete all answer with the
//! state that resulted, so the caller never has to ask again to find out whether
//! anything happened.

use std::collections::BTreeMap;

use axum::{
    Json,
    extract::{Path, State},
};
use serde::{Deserialize, Serialize};

use crate::{
    api::App,
    catalog::ModelEntry,
    engine::{Residency, residency_of},
    error::{Error, Result},
    form::{self, Section},
};

/// A model, with where it currently is.
#[derive(Serialize)]
pub struct ModelView {
    #[serde(flatten)]
    pub entry: ModelEntry,
    pub residency: Residency,
    /// Why the last attempt to load it failed, when one did.
    ///
    /// An entry saying resident beside a residency saying released is the
    /// honest picture of a failed load, and on its own it is a picture with no
    /// caption. This is the caption.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub load_error: Option<String>,
}

/// One model's view: where it is, and why it is not where it was asked to be.
///
/// A helper rather than five copies, because the third field was added to three
/// of the five sites and forgotten at the other two the first time.
async fn view_of(app: &App, entry: ModelEntry) -> ModelView {
    let uid = entry.uid.clone();
    ModelView {
        residency: residency_of(app.engine.as_ref(), &uid).await,
        load_error: app.engine.failures().await.unwrap_or_default().remove(&uid),
        entry,
    }
}

pub async fn list(State(app): State<App>) -> Result<Json<Vec<ModelView>>> {
    // One call to the engine for all of them, not one per model: the screen
    // that asks wants every row, and asking per row is the same lock N times.
    let residency = app.engine.residency().await.unwrap_or_default();
    let mut failures = app.engine.failures().await.unwrap_or_default();
    let models = app
        .catalog
        .list()
        .await
        .into_iter()
        .map(|entry| ModelView {
            residency: residency
                .get(&entry.uid)
                .copied()
                .unwrap_or(Residency::Absent),
            load_error: failures.remove(&entry.uid),
            entry,
        })
        .collect();
    Ok(Json(models))
}

pub async fn get(State(app): State<App>, Path(uid): Path<String>) -> Result<Json<ModelView>> {
    let entry = app.catalog.get(&uid).await?;
    Ok(Json(view_of(&app, entry).await))
}

/// The form and its current values, in one answer.
#[derive(Serialize)]
pub struct FormView {
    pub sections: Vec<Section>,
    pub values: BTreeMap<String, String>,
}

pub async fn form(State(app): State<App>, Path(uid): Path<String>) -> Result<Json<FormView>> {
    // Fetched even though the sections do not depend on it, because a form for a
    // model that is not here should be a 404 and not an empty form somebody
    // fills in and cannot save.
    let entry = app.catalog.get(&uid).await?;
    Ok(Json(FormView {
        sections: form::settings_form(),
        values: entry.settings,
    }))
}

#[derive(Deserialize)]
pub struct SettingsInput {
    pub values: BTreeMap<String, String>,
}

/// Store what an administrator set.
///
/// A key the form never offered is REFUSED rather than stored. A setting nothing
/// reads is worse than a missing one: somebody believes it is in effect.
pub async fn settings(
    State(app): State<App>,
    Path(uid): Path<String>,
    Json(input): Json<SettingsInput>,
) -> Result<Json<ModelView>> {
    let known = form::keys();
    if let Some(unknown) = input.values.keys().find(|k| !known.contains(k)) {
        return Err(Error::invalid(format!(
            "this node has no setting called {unknown:?}"
        )));
    }

    let entry = app
        .catalog
        .update(&uid, |entry| {
            // Blank means "unset", so it is removed rather than stored as an
            // empty string that later reads as a value somebody chose.
            entry.settings = input
                .values
                .into_iter()
                .filter(|(_, v)| !v.trim().is_empty())
                .collect();
            Ok(())
        })
        .await?;

    // Told to the ENGINE, not just written down. It keeps its own picture of
    // what a model is, built when the model was registered, and reads the
    // settings from there when it loads. Saving without this stored the value,
    // showed it back in the form, and built the pipeline as if it had never been
    // set: every field here was writable and none of them did anything.
    app.engine
        .register(&entry, &app.catalog.weights(&uid))
        .await?;

    tracing::info!(uid = %uid, settings = entry.settings.len(), "model settings saved");
    Ok(Json(view_of(&app, entry).await))
}

/// Ask for this model to be kept in memory.
///
/// Returns as soon as the loading has STARTED, because reading tens of gigabytes
/// is minutes and nothing can hold a request open for it. The answer says
/// `waking`, and the caller watches for `resident`.
///
/// Residency is an administrator's decision and not a policy (KB/35), so the
/// decision is recorded whether or not the loading later succeeds: a restart
/// brings back the models somebody said should be ready. When a load does fail,
/// the entry says resident and the residency says released, and that
/// disagreement is the honest thing to show.
pub async fn load(State(app): State<App>, Path(uid): Path<String>) -> Result<Json<ModelView>> {
    let entry = app.catalog.get(&uid).await?;

    // The engine first, so a model it refuses outright (one it has never been
    // told about) is a failure the caller sees rather than a decision recorded
    // about something that will never load.
    app.engine.load(&uid).await?;
    let entry = app
        .catalog
        .update(&entry.uid, |e| {
            e.resident = true;
            Ok(())
        })
        .await?;

    // STARTED, not finished, and the word matters: this line used to say "model
    // loaded" and it was printed before the engine had read a byte. In the log
    // of a model that could not be loaded at all it appeared ABOVE the refusal,
    // which reads as a load that succeeded and then broke.
    tracing::info!(uid = %uid, "loading started");
    Ok(Json(view_of(&app, entry).await))
}

/// Let this model out of memory. It stays here and wakes when called.
pub async fn unload(State(app): State<App>, Path(uid): Path<String>) -> Result<Json<ModelView>> {
    app.catalog.get(&uid).await?;

    app.engine.unload(&uid).await?;
    let entry = app
        .catalog
        .update(&uid, |e| {
            e.resident = false;
            Ok(())
        })
        .await?;

    tracing::info!(uid = %uid, "model released");
    Ok(Json(view_of(&app, entry).await))
}

#[derive(Serialize)]
pub struct Removed {
    pub uid: String,
    pub name: String,
    pub freed_bytes: u64,
}

/// Take a model off this node.
///
/// The engine is told FIRST. Deleting the files under a model the engine still
/// has open is how a node answers a question with weights that are half gone.
pub async fn remove(State(app): State<App>, Path(uid): Path<String>) -> Result<Json<Removed>> {
    let entry = app.catalog.get(&uid).await?;

    app.engine.forget(&uid).await?;
    let entry = app.catalog.remove(&entry.uid).await?;

    Ok(Json(Removed {
        uid: entry.uid,
        name: entry.name,
        freed_bytes: entry.facts.size_bytes,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        api::testing::Harness,
        catalog::{Catalog, Facts, ModelKind},
        engine::{Engine, scripted::Call},
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
        let json = serde_json::from_slice(&bytes).unwrap_or(serde_json::Value::Null);
        (status, json)
    }

    async fn given_a_model(node: &Harness, name: &str) -> String {
        let uid = Catalog::mint_uid();
        let entry = ModelEntry {
            uid: uid.clone(),
            repo: format!("vendor/{name}"),
            revision: "abc123".into(),
            name: name.into(),
            handle: name.into(),
            kind: ModelKind::Chat,
            facts: Facts {
                size_bytes: 4_000_000_000,
                files: 3,
                ..Facts::default()
            },
            settings: BTreeMap::new(),
            resident: false,
            added_at: chrono::Utc::now(),
        };
        node.app.catalog.insert(entry.clone()).await.unwrap();
        node.engine
            .register(&entry, &node.app.catalog.weights(&uid))
            .await
            .unwrap();
        uid
    }

    #[tokio::test]
    async fn a_model_is_listed_with_where_it_actually_is() {
        let node = Harness::start().await;
        let uid = given_a_model(&node, "small").await;

        let (status, body) = call(&node, "GET", "/node/models", None).await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(body[0]["uid"], uid);
        assert_eq!(body[0]["name"], "small");
        assert_eq!(body[0]["residency"], "released");
        // The facts ride on the row, so nothing has to ask a second time.
        assert_eq!(body[0]["facts"]["size_bytes"], 4_000_000_000u64);
    }

    #[tokio::test]
    async fn a_model_the_engine_never_took_reads_as_absent_not_released() {
        let node = Harness::start().await;
        let uid = Catalog::mint_uid();
        node.app
            .catalog
            .insert(ModelEntry {
                uid: uid.clone(),
                repo: "vendor/orphan".into(),
                revision: "abc".into(),
                name: "orphan".into(),
                handle: "orphan".into(),
                kind: ModelKind::Chat,
                facts: Facts::default(),
                settings: BTreeMap::new(),
                resident: false,
                added_at: chrono::Utc::now(),
            })
            .await
            .unwrap();

        let (_, body) = call(&node, "GET", &format!("/node/models/{uid}"), None).await;
        assert_eq!(
            body["residency"], "absent",
            "a model the engine never took looked like a decision somebody made"
        );
    }

    #[tokio::test]
    async fn the_form_comes_back_with_its_values_already_in_it() {
        let node = Harness::start().await;
        let uid = given_a_model(&node, "small").await;
        call(
            &node,
            "PUT",
            &format!("/node/models/{uid}/settings"),
            Some(serde_json::json!({"values": {"quantization": "Q4K"}})),
        )
        .await;

        let (status, body) = call(&node, "GET", &format!("/node/models/{uid}/form"), None).await;
        assert_eq!(status, StatusCode::OK);
        assert!(!body["sections"].as_array().unwrap().is_empty());
        assert_eq!(
            body["values"]["quantization"], "Q4K",
            "the form and its contents took two requests"
        );
    }

    #[tokio::test]
    async fn a_form_for_a_model_that_is_not_here_is_a_not_found() {
        let node = Harness::start().await;
        let (status, _) = call(&node, "GET", "/node/models/nope/form", None).await;
        assert_eq!(status, StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn a_setting_the_form_never_offered_is_refused() {
        let node = Harness::start().await;
        let uid = given_a_model(&node, "small").await;

        let (status, body) = call(
            &node,
            "PUT",
            &format!("/node/models/{uid}/settings"),
            Some(serde_json::json!({"values": {"make_it_fast": "yes"}})),
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert!(
            body["error"]["message"]
                .as_str()
                .unwrap()
                .contains("make_it_fast"),
            "{body}"
        );

        // And nothing was stored on the way to refusing.
        assert!(
            node.app
                .catalog
                .get(&uid)
                .await
                .unwrap()
                .settings
                .is_empty()
        );
    }

    #[tokio::test]
    async fn saving_a_setting_tells_the_engine_and_not_only_the_disk() {
        // The failure this exists for: quantization was saved, shown back in the
        // form, and ignored at load, because the engine reads settings from its
        // own picture of a model and nothing updated it. Every field on that
        // form was writable and inert. It cost somebody an hour of downloading
        // and a load that failed for the exact reason the setting would have
        // fixed.
        let node = Harness::start().await;
        let uid = given_a_model(&node, "small").await;
        let before = node.engine.calls().len();

        call(
            &node,
            "PUT",
            &format!("/node/models/{uid}/settings"),
            Some(serde_json::json!({"values": {"quantization": "Q4K"}})),
        )
        .await;

        let told = node
            .engine
            .calls()
            .into_iter()
            .skip(before)
            .any(|c| matches!(c, Call::Register { uid: ref u, .. } if *u == uid));
        assert!(told, "the engine was never told the setting changed");
    }

    #[tokio::test]
    async fn a_blank_value_unsets_rather_than_storing_an_empty_choice() {
        let node = Harness::start().await;
        let uid = given_a_model(&node, "small").await;

        call(
            &node,
            "PUT",
            &format!("/node/models/{uid}/settings"),
            Some(serde_json::json!({"values": {"quantization": "Q4K"}})),
        )
        .await;
        call(
            &node,
            "PUT",
            &format!("/node/models/{uid}/settings"),
            Some(serde_json::json!({"values": {"quantization": "  "}})),
        )
        .await;

        assert!(
            node.app
                .catalog
                .get(&uid)
                .await
                .unwrap()
                .settings
                .is_empty(),
            "a cleared field was stored as a choice of nothing"
        );
    }

    #[tokio::test]
    async fn loading_asks_the_engine_and_records_the_decision() {
        let node = Harness::start().await;
        let uid = given_a_model(&node, "small").await;

        let (status, body) = call(&node, "POST", &format!("/node/models/{uid}/load"), None).await;
        assert_eq!(status, StatusCode::OK);
        // The scripted engine loads at once, so this reads resident. Against the
        // real one it would read waking, and the screen would watch for the rest.
        assert_eq!(body["residency"], "resident");
        assert_eq!(body["resident"], true);

        assert!(node.engine.calls().contains(&Call::Load(uid.clone())));
        // Persisted, so a restart brings back what somebody asked for.
        let reopened = Catalog::open(node.app.config.models_dir()).await.unwrap();
        assert!(reopened.get(&uid).await.unwrap().resident);
    }

    #[tokio::test]
    async fn an_engine_that_refuses_to_start_a_load_leaves_the_decision_unrecorded() {
        // A refusal the engine can make immediately (it has never been told
        // about this model) must not be recorded as a decision. A load that
        // starts and later fails is a different case: the decision stands and
        // the residency disagrees with it, which is what the screen shows.
        let node = Harness::start().await;
        let uid = given_a_model(&node, "small").await;
        node.engine.fail_next("not enough memory");

        let (status, _) = call(&node, "POST", &format!("/node/models/{uid}/load"), None).await;
        assert_eq!(status, StatusCode::INTERNAL_SERVER_ERROR);
        assert!(
            !node.app.catalog.get(&uid).await.unwrap().resident,
            "a refused load was recorded as a success"
        );
    }

    #[tokio::test]
    async fn unloading_releases_the_model_and_keeps_it() {
        let node = Harness::start().await;
        let uid = given_a_model(&node, "small").await;
        call(&node, "POST", &format!("/node/models/{uid}/load"), None).await;

        let (status, body) = call(&node, "POST", &format!("/node/models/{uid}/unload"), None).await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(body["residency"], "released");
        assert_eq!(body["resident"], false);
        // Released, not gone.
        assert!(node.app.catalog.get(&uid).await.is_ok());
    }

    #[tokio::test]
    async fn deleting_tells_the_engine_before_it_touches_the_files() {
        // The order is the point: files deleted under a model the engine still
        // holds open is a question answered with half a set of weights.
        let node = Harness::start().await;
        let uid = given_a_model(&node, "small").await;
        let weights = node.app.catalog.weights(&uid);
        tokio::fs::create_dir_all(&weights).await.unwrap();
        tokio::fs::write(weights.join("model.safetensors"), b"bytes")
            .await
            .unwrap();

        let (status, body) = call(&node, "DELETE", &format!("/node/models/{uid}"), None).await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(body["name"], "small");
        assert_eq!(body["freed_bytes"], 4_000_000_000u64);

        assert!(node.engine.calls().contains(&Call::Forget(uid.clone())));
        assert!(
            !node.app.catalog.dir(&uid).exists(),
            "the files are still here"
        );
    }

    #[tokio::test]
    async fn an_engine_that_will_not_let_go_stops_the_delete() {
        let node = Harness::start().await;
        let uid = given_a_model(&node, "small").await;
        node.engine.fail_next("the model is in use");

        let (status, _) = call(&node, "DELETE", &format!("/node/models/{uid}"), None).await;
        assert_eq!(status, StatusCode::INTERNAL_SERVER_ERROR);
        assert!(
            node.app.catalog.get(&uid).await.is_ok(),
            "the model was dropped even though the engine still had it"
        );
        assert!(node.app.catalog.dir(&uid).exists());
    }

    #[tokio::test]
    async fn acting_on_a_model_that_is_not_here_is_a_not_found_and_touches_nothing() {
        let node = Harness::start().await;
        for (method, path) in [
            ("GET", "/node/models/nope"),
            ("POST", "/node/models/nope/load"),
            ("POST", "/node/models/nope/unload"),
            ("DELETE", "/node/models/nope"),
        ] {
            let (status, _) = call(&node, method, path, None).await;
            assert_eq!(status, StatusCode::NOT_FOUND, "{method} {path}");
        }
        assert!(
            node.engine.calls().is_empty(),
            "the engine was asked about a model this node does not have"
        );
    }
}
