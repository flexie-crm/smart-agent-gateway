//! The seam between this node and the thing that actually runs weights.
//!
//! Everything else in this crate is the control plane: what is on the disk, what
//! was pulled, what an administrator set. None of it needs to know how a model
//! is executed. This module is the whole of that knowledge, stated as a trait so
//! that the control plane can be tested without a machine that can run a model,
//! the same way the Gateway is tested against a scripted provider (KB/28).
//!
//! The vocabulary is ours (CLAUDE.md): a model is **resident**, **released** or
//! **waking**, and the engine's own words for those never leave this module.
//!
//! **Waking is not an implementation detail.** With a fleet of rarely-used
//! specialists, loading is the normal case rather than an edge (KB/35), so a
//! turn that silently blocks while a model wakes is the failure people would
//! report. It is a state that can be asked about, not a stall.

use std::{collections::BTreeMap, path::Path};

use serde::Serialize;

use crate::{catalog::ModelEntry, error::Result, machine::Verdict};

pub mod absent;
#[cfg(feature = "engine")]
pub mod mistral;
// Tests only, and enforced rather than intended. It was reachable from the
// binary, and a build with no engine used it: its `load` succeeds, so a node
// that could not run a model reported one resident and drew "in memory" on the
// screen. A test double a binary cannot reach cannot be substituted by mistake.
#[cfg(test)]
pub mod scripted;

/// Where a model stands on this node right now.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Residency {
    /// In memory, answering immediately.
    Resident,
    /// Known and configured, not in memory. The next request wakes it.
    Released,
    /// On its way into memory. A request will be answered, after a wait.
    Waking,
    /// The engine has never been told about this model.
    ///
    /// Distinct from released on purpose: released is a decision somebody made,
    /// absent is a model the engine could not be given, and only one of those
    /// is a problem.
    Absent,
}

/// What this node can do with a set of weights.
#[async_trait::async_trait]
pub trait Engine: Send + Sync + 'static {
    /// Tell the engine a model exists, without loading it.
    ///
    /// Called for everything in the catalogue at boot and for each model as it
    /// finishes downloading. Registering is cheap; loading is not, which is why
    /// they are two calls.
    async fn register(&self, entry: &ModelEntry, weights: &Path) -> Result<()>;

    /// Take a model out of the engine entirely, because it is being deleted.
    async fn forget(&self, uid: &str) -> Result<()>;

    /// START bringing a model into memory, and keep it there.
    ///
    /// Returns as soon as the work is under way, not when it is done. Loading a
    /// large model is minutes, and no caller can hold a request open for that:
    /// a browser gives up, shows a failure, and the model finishes loading
    /// anyway, which is an approved action that then fails (CLAUDE.md). What the
    /// caller gets back is a refusal it could not have known about (a model this
    /// node does not have); everything after that is watched through
    /// [`Engine::residency`], where it reads as [`Residency::Waking`].
    async fn load(&self, uid: &str) -> Result<()>;

    /// Load, and wait for it.
    ///
    /// For boot, which is the one caller that genuinely should not start serving
    /// until the models an administrator marked resident are in, and for tests.
    /// Nothing serving a request should use it.
    async fn wait_loaded(&self, uid: &str) -> Result<()> {
        self.load(uid).await
    }

    /// Let a model out of memory. It stays registered and wakes on demand.
    async fn unload(&self, uid: &str) -> Result<()>;

    /// Where every model the engine knows about stands.
    ///
    /// One call rather than one per model, because the screen that asks wants
    /// all of them and asking N times would be N locks on the engine's map.
    async fn residency(&self) -> Result<BTreeMap<String, Residency>>;

    /// Why a model's last load failed, for the ones that failed.
    ///
    /// [`Engine::load`] returns once the work has STARTED, so a failure after
    /// that has no caller to be returned to. Without this it reaches a log line
    /// and stops there, and what the screen has left is a model that says it is
    /// wanted beside a residency that says it is released: true, and useless.
    /// "the engine cannot load this architecture" and "there was no room" are
    /// the same picture without the sentence.
    ///
    /// Shaped like [`Engine::residency`] for the same reason: one call for the
    /// screen that wants them all. Empty by default, because an engine that
    /// cannot fail asynchronously has nothing to report.
    async fn failures(&self) -> Result<BTreeMap<String, String>> {
        Ok(BTreeMap::new())
    }

    /// Whether this engine could load a model of this design AT ALL, asked
    /// before anything is downloaded.
    ///
    /// Before, because that is the only time the answer is worth having. Every
    /// other check on a pull (the disk, the memory, the credentials) happens
    /// while somebody is still looking at the model, and this one did not: a
    /// speech recognition model went through all three, transferred for as long
    /// as it took, registered, and then failed at load, which is an hour spent
    /// to learn something that was knowable in the first second.
    ///
    /// # It is the architecture that is asked about, never the kind
    ///
    /// A kind is a label on a repository and the engine never reads it. What an
    /// engine matches on is the class the weights declare, and the two do not
    /// agree: models sharing one kind can differ on whether an implementation
    /// exists, so a gate on kind would refuse models that run and admit models
    /// that cannot. The kind is passed in only so the refusal can be SAID in
    /// words a person uses, and it decides nothing.
    ///
    /// `None` means the repository did not say what it is. That is allowed on
    /// purpose, and it is the same judgement the disk check makes about a mount
    /// it cannot read: refusing what cannot be judged would block models that
    /// work perfectly well, and a download that fails honestly is a better
    /// outcome than a model wrongly declared unusable. The cost of being wrong
    /// in this direction is the old behaviour, once, for the rare repository
    /// that publishes no configuration.
    ///
    /// Allowed by default, because an engine that has no table to consult has
    /// no grounds to refuse.
    fn can_run(&self, architecture: Option<&str>, kind: crate::catalog::ModelKind) -> Verdict {
        let (_, _) = (architecture, kind);
        Verdict::allowed()
    }

    /// The inference surface, to be served alongside the node's own routes.
    ///
    /// `None` means this build cannot infer, which is the scripted engine and
    /// nothing an operator would deploy.
    fn routes(&self) -> Option<axum::Router>;
}

/// Where one model stands, from the whole picture.
pub async fn residency_of(engine: &dyn Engine, uid: &str) -> Residency {
    match engine.residency().await {
        Ok(map) => map.get(uid).copied().unwrap_or(Residency::Absent),
        Err(err) => {
            // An engine that cannot say is not an engine that says "released":
            // the honest answer is that we do not know where this model is, and
            // absent is the value that does not claim otherwise.
            tracing::warn!(%err, uid, "the engine could not report residency");
            Residency::Absent
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn residency_is_named_in_product_words_on_the_wire() {
        // The console shows these. They must not be the engine's vocabulary.
        let pairs = [
            (Residency::Resident, "resident"),
            (Residency::Released, "released"),
            (Residency::Waking, "waking"),
            (Residency::Absent, "absent"),
        ];
        for (state, name) in pairs {
            assert_eq!(serde_json::to_value(state).unwrap(), name);
        }
    }

    #[tokio::test]
    async fn an_engine_that_cannot_answer_says_absent_rather_than_released() {
        // Released means somebody decided. Absent means we do not know. An
        // engine that has fallen over must not be reported as a decision.
        let engine = scripted::ScriptedEngine::new();
        engine.fail_next("the engine is not answering");
        assert_eq!(residency_of(&engine, "anything").await, Residency::Absent);
    }
}
