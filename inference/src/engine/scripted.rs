//! An engine that does exactly what a test tells it to.
//!
//! Same idea as the scripted provider behind the Gateway seam (KB/28): the
//! control plane is most of this crate and none of it needs a machine that can
//! execute weights, so the questions worth asking about it (does a failed pull
//! leave anything behind, does a refused settings change reach the disk, does
//! deleting a model tell the engine first) are asked against this.
//!
//! It is not a mock of the engine's internals. It records what it was asked and
//! answers what it was told to, so a test asserts the node's behaviour and never
//! this file's.

use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
    sync::Mutex,
};

use crate::{
    catalog::ModelEntry,
    engine::{Engine, Residency},
    error::{Error, Result},
};

/// One thing the node asked the engine to do.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Call {
    Register { uid: String, weights: PathBuf },
    Forget(String),
    Load(String),
    Unload(String),
}

#[derive(Default)]
struct State {
    models: BTreeMap<String, Residency>,
    calls: Vec<Call>,
    /// Set to make the next call fail, whatever it is.
    fail_next: Option<String>,
}

#[derive(Default)]
pub struct ScriptedEngine {
    state: Mutex<State>,
}

impl ScriptedEngine {
    pub fn new() -> Self {
        Self::default()
    }

    /// Make the next call fail, once.
    pub fn fail_next(&self, reason: &str) {
        self.lock().fail_next = Some(reason.to_string());
    }

    /// Everything the node has asked for, in order.
    pub fn calls(&self) -> Vec<Call> {
        self.lock().calls.clone()
    }

    /// Where the engine currently thinks a model is.
    pub fn residency(&self, uid: &str) -> Residency {
        self.lock()
            .models
            .get(uid)
            .copied()
            .unwrap_or(Residency::Absent)
    }

    /// Put a model into a state directly, to set up a case rather than reach it.
    pub fn set(&self, uid: &str, residency: Residency) {
        self.lock().models.insert(uid.to_string(), residency);
    }

    /// A poisoned lock here means a test panicked while holding it, and the
    /// failure worth reporting is that panic and not this one.
    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn record(&self, call: Call) -> Result<()> {
        let mut state = self.lock();
        state.calls.push(call);
        match state.fail_next.take() {
            Some(reason) => Err(Error::Engine(reason)),
            None => Ok(()),
        }
    }
}

#[async_trait::async_trait]
impl Engine for ScriptedEngine {
    async fn register(&self, entry: &ModelEntry, weights: &Path) -> Result<()> {
        self.record(Call::Register {
            uid: entry.uid.clone(),
            weights: weights.to_path_buf(),
        })?;
        self.lock()
            .models
            .insert(entry.uid.clone(), Residency::Released);
        Ok(())
    }

    async fn forget(&self, uid: &str) -> Result<()> {
        self.record(Call::Forget(uid.to_string()))?;
        self.lock().models.remove(uid);
        Ok(())
    }

    async fn load(&self, uid: &str) -> Result<()> {
        self.record(Call::Load(uid.to_string()))?;
        self.lock()
            .models
            .insert(uid.to_string(), Residency::Resident);
        Ok(())
    }

    async fn unload(&self, uid: &str) -> Result<()> {
        self.record(Call::Unload(uid.to_string()))?;
        self.lock()
            .models
            .insert(uid.to_string(), Residency::Released);
        Ok(())
    }

    async fn residency(&self) -> Result<BTreeMap<String, Residency>> {
        let mut state = self.lock();
        if let Some(reason) = state.fail_next.take() {
            return Err(Error::Engine(reason));
        }
        Ok(state.models.clone())
    }

    fn routes(&self) -> Option<axum::Router> {
        // A build with no engine serves no inference. Saying so is the point:
        // nothing should be able to deploy this by accident and discover it
        // when a person asks a question.
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::catalog::{Facts, ModelKind};

    fn entry(uid: &str) -> ModelEntry {
        ModelEntry {
            uid: uid.into(),
            repo: "vendor/model".into(),
            revision: "abc".into(),
            name: "model".into(),
            handle: "model".into(),
            kind: ModelKind::Chat,
            facts: Facts::default(),
            settings: BTreeMap::new(),
            resident: false,
            added_at: chrono::Utc::now(),
        }
    }

    #[tokio::test]
    async fn a_registered_model_is_released_and_not_resident() {
        // Registering is cheap and loading is not. A register that loaded would
        // make boot on a node with twenty models take twenty model loads.
        let engine = ScriptedEngine::new();
        engine
            .register(&entry("uid-1"), Path::new("/w"))
            .await
            .unwrap();
        assert_eq!(engine.residency("uid-1"), Residency::Released);
    }

    #[tokio::test]
    async fn loading_and_unloading_move_a_model_between_the_two_states() {
        let engine = ScriptedEngine::new();
        engine
            .register(&entry("uid-1"), Path::new("/w"))
            .await
            .unwrap();

        engine.load("uid-1").await.unwrap();
        assert_eq!(engine.residency("uid-1"), Residency::Resident);

        engine.unload("uid-1").await.unwrap();
        assert_eq!(engine.residency("uid-1"), Residency::Released);
    }

    #[tokio::test]
    async fn forgetting_a_model_leaves_the_engine_not_knowing_it() {
        let engine = ScriptedEngine::new();
        engine
            .register(&entry("uid-1"), Path::new("/w"))
            .await
            .unwrap();
        engine.forget("uid-1").await.unwrap();
        assert_eq!(engine.residency("uid-1"), Residency::Absent);
    }

    #[tokio::test]
    async fn a_scripted_failure_happens_once_and_is_still_recorded() {
        let engine = ScriptedEngine::new();
        engine.fail_next("out of memory");

        let err = engine
            .load("uid-1")
            .await
            .expect_err("the failure was lost");
        assert!(err.to_string().contains("out of memory"), "{err}");
        // Recorded even though it failed: a test asserting the node asked has
        // to see the ask whether or not it succeeded.
        assert_eq!(engine.calls(), vec![Call::Load("uid-1".into())]);

        engine.load("uid-1").await.expect("the failure repeated");
    }

    #[tokio::test]
    async fn calls_are_recorded_in_order_with_what_they_carried() {
        let engine = ScriptedEngine::new();
        engine
            .register(&entry("uid-1"), Path::new("/data/models/uid-1/weights"))
            .await
            .unwrap();
        engine.load("uid-1").await.unwrap();
        engine.forget("uid-1").await.unwrap();

        assert_eq!(
            engine.calls(),
            vec![
                Call::Register {
                    uid: "uid-1".into(),
                    weights: PathBuf::from("/data/models/uid-1/weights"),
                },
                Call::Load("uid-1".into()),
                Call::Forget("uid-1".into()),
            ]
        );
    }

    #[tokio::test]
    async fn a_build_with_no_engine_serves_no_inference() {
        assert!(ScriptedEngine::new().routes().is_none());
    }
}
