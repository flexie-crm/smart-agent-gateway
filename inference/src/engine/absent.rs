//! The engine a build without one has.
//!
//! A build compiled without the `engine` feature serves the control surface and
//! cannot run a model. It still needs SOMETHING behind the seam, and what that
//! something does matters: this used to be the scripted engine the tests drive,
//! whose `load` succeeds. So a node built without an engine would accept a load,
//! report the model resident, and draw "in memory" on the screen for weights
//! that could not answer a single question.
//!
//! That is an approved action that does not do what it says (CLAUDE.md), and the
//! screen said the opposite of the truth. This refuses instead, in words that
//! name the actual problem.
//!
//! The scripted engine is now `#[cfg(test)]`, so the substitution that caused it
//! cannot be made again: there is no test double for a binary to reach for.

use std::{collections::BTreeMap, path::Path};

use crate::{
    catalog::ModelEntry,
    engine::{Engine, Residency},
    error::{Error, Result},
};

/// What a build with no engine can do: remember what is on the disk, and say no
/// to everything that would need to execute it.
#[derive(Default)]
pub struct AbsentEngine;

impl AbsentEngine {
    pub fn new() -> Self {
        Self
    }

    fn refuse() -> Error {
        // Not "the engine refused", because there is no engine to refuse: this
        // build was compiled without one, which is a fact about the binary and
        // is what somebody needs to be told.
        Error::invalid("this node was built without the ability to run models")
    }
}

#[async_trait::async_trait]
impl Engine for AbsentEngine {
    /// Accepted, because being told a model exists costs nothing and the
    /// catalogue screen is the half of this node that does work.
    async fn register(&self, _entry: &ModelEntry, _weights: &Path) -> Result<()> {
        Ok(())
    }

    async fn forget(&self, _uid: &str) -> Result<()> {
        Ok(())
    }

    async fn load(&self, _uid: &str) -> Result<()> {
        Err(Self::refuse())
    }

    /// Nothing is in memory, so nothing has to come out of it. Refusing here
    /// would be refusing to reach a state this node is permanently in.
    async fn unload(&self, _uid: &str) -> Result<()> {
        Ok(())
    }

    /// Empty, so every model reads as absent: the engine does not hold any of
    /// them, and saying "released" would imply somebody had decided that.
    async fn residency(&self) -> Result<BTreeMap<String, Residency>> {
        Ok(BTreeMap::new())
    }

    fn routes(&self) -> Option<axum::Router> {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::catalog::{Facts, ModelKind};

    fn entry() -> ModelEntry {
        ModelEntry {
            uid: "uid-1".into(),
            repo: "vendor/model".into(),
            revision: "abc".into(),
            name: "model".into(),
            handle: "model".into(),
            kind: ModelKind::Chat,
            facts: Facts::default(),
            settings: Default::default(),
            resident: false,
            added_at: chrono::Utc::now(),
        }
    }

    #[tokio::test]
    async fn loading_is_refused_rather_than_pretended() {
        // The bug this exists for: the test double's `load` succeeded, so a
        // build with no engine drew "in memory" for weights that could not
        // answer anything.
        let engine = AbsentEngine::new();
        let err = engine.load("uid-1").await.expect_err("a load was accepted");
        assert!(
            err.to_string()
                .contains("without the ability to run models"),
            "{err}"
        );
    }

    #[tokio::test]
    async fn a_refusal_says_it_is_the_build_and_not_the_engine() {
        // There is no engine here to have an opinion, and "the engine refused"
        // would send somebody looking at the wrong thing.
        let engine = AbsentEngine::new();
        let err = engine.load("uid-1").await.unwrap_err();
        assert!(!err.to_string().contains("engine refused"), "{err}");
    }

    #[tokio::test]
    async fn the_catalogue_still_works() {
        // The half of this node that does not need an engine goes on working:
        // it can be told what is on the disk, and asked to forget it.
        let engine = AbsentEngine::new();
        engine.register(&entry(), Path::new("/w")).await.unwrap();
        engine.forget("uid-1").await.unwrap();
        // And releasing is a state it is permanently in, so it is not refused.
        engine.unload("uid-1").await.unwrap();
    }

    #[tokio::test]
    async fn every_model_reads_as_absent() {
        let engine = AbsentEngine::new();
        engine.register(&entry(), Path::new("/w")).await.unwrap();
        assert!(engine.residency().await.unwrap().is_empty());
        assert_eq!(
            crate::engine::residency_of(&engine, "uid-1").await,
            Residency::Absent
        );
    }

    #[tokio::test]
    async fn it_serves_no_inference() {
        assert!(AbsentEngine::new().routes().is_none());
    }
}
