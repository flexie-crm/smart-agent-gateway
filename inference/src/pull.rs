//! Fetching a model, and why nothing waits for it.
//!
//! Weights are tens of gigabytes and take minutes to hours, so **no request
//! waits for one** (KB/35). [`Pulls::start`] validates everything it can, mints
//! an id, and returns; the work happens on a task the node owns. The
//! orchestrator writes a job row and polls, and because the truth is on that row
//! rather than in a held connection, an orchestrator that is down for the whole
//! download finds "ready" on its next poll and writes it then.
//!
//! Everything that CAN be checked is checked before the id is minted: that the
//! repository exists, that it publishes weights this node can read, that the
//! choice between compression levels is not ours to make, that the model is not
//! already here, and that the disk has room. Approval must equal success
//! (CLAUDE.md), and a pull that was going to fail on the first byte should say
//! so while somebody is still looking at it.
//!
//! **A half-downloaded model is never loadable.** Files land in `incoming/`,
//! are verified there, and become a model by being RENAMED into place. There is
//! no window in which the catalogue names a directory that is still filling up.

use std::{collections::HashMap, sync::Arc};

use chrono::{DateTime, Utc};
use serde::Serialize;
use tokio::sync::RwLock;
use tokio_util::sync::CancellationToken;

use crate::{
    catalog::{Catalog, Facts, ModelEntry},
    engine::Engine,
    error::{Error, Result},
    hub::{Hub, HubFile},
    machine::Machine,
};

/// How long a finished pull stays readable before it is forgotten.
///
/// The poller needs to see the terminal state at least once, and the row it
/// writes is the lasting record. An hour is far longer than any poll interval
/// and short enough that a node that has pulled a thousand models is not still
/// holding a thousand answers.
const KEEP_FINISHED: chrono::TimeDelta = chrono::TimeDelta::hours(1);

/// How often the shared progress figure is refreshed while a file is arriving.
///
/// Not per chunk: that would take a write lock thousands of times a second for a
/// number nobody reads more than once every couple of seconds. Not per file
/// either, which is what this replaced: a model published as one forty gigabyte
/// file would then sit at zero for an hour and finish in a single jump, and the
/// person watching it has no way to tell that from a download that has hung.
const PROGRESS_INTERVAL: std::time::Duration = std::time::Duration::from_secs(1);

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum PullState {
    Downloading,
    Verifying,
    Ready,
    Failed,
    Cancelled,
}

impl PullState {
    /// Whether nothing more will happen to this pull.
    pub fn finished(self) -> bool {
        matches!(self, Self::Ready | Self::Failed | Self::Cancelled)
    }
}

/// One download, as the poller sees it.
#[derive(Debug, Clone, Serialize)]
pub struct Pull {
    pub id: String,
    pub repo: String,
    pub revision: String,
    pub state: PullState,
    pub bytes_done: u64,
    pub bytes_total: u64,
    pub files_done: u32,
    pub files_total: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    /// The model this became. Set only once it is whole.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub model_uid: Option<String>,
    pub started_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

struct Tracked {
    pull: Pull,
    cancel: CancellationToken,
    /// What this download is FOR, kept so it can be resumed without asking the
    /// library again. Re-describing would be a second network call and, worse, a
    /// second answer: a repository that moved between the two would resume a
    /// different set of files into the same directory.
    described: crate::hub::HubRepo,
}

/// Every download this node has started.
#[derive(Clone)]
pub struct Pulls {
    inner: Arc<RwLock<HashMap<String, Tracked>>>,
}

/// What a pull needs to do its work, gathered once rather than threaded through
/// six arguments.
#[derive(Clone)]
pub struct PullContext {
    pub hub: Arc<Hub>,
    pub catalog: Catalog,
    pub engine: Arc<dyn Engine>,
    pub incoming: std::path::PathBuf,
}

impl Pulls {
    pub fn new() -> Self {
        Self {
            inner: Arc::new(RwLock::new(HashMap::new())),
        }
    }

    /// Clear out anything a previous run left behind.
    ///
    /// Nothing in `incoming/` survives a restart: a partial download has no
    /// record of which files were finished, and resuming from a directory whose
    /// history is unknown is how a model ends up with one truncated shard and no
    /// sign of it. The orchestrator's job row is what retries.
    pub async fn clear_incoming(incoming: &std::path::Path) -> Result<()> {
        let mut dir = match tokio::fs::read_dir(incoming).await {
            Ok(dir) => dir,
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(()),
            Err(err) => return Err(err.into()),
        };
        let mut cleared = 0;
        while let Some(item) = dir.next_entry().await? {
            tokio::fs::remove_dir_all(item.path()).await.ok();
            cleared += 1;
        }
        if cleared > 0 {
            tracing::info!(cleared, "abandoned downloads cleared");
        }
        Ok(())
    }

    pub async fn get(&self, id: &str) -> Result<Pull> {
        self.inner
            .read()
            .await
            .get(id)
            .map(|t| t.pull.clone())
            .ok_or_else(|| Error::not_found("download"))
    }

    pub async fn list(&self) -> Vec<Pull> {
        let mut pulls: Vec<Pull> = self
            .inner
            .read()
            .await
            .values()
            .map(|t| t.pull.clone())
            .collect();
        pulls.sort_by_key(|p| std::cmp::Reverse(p.started_at));
        pulls
    }

    /// Whether anything is downloading right now.
    pub async fn active(&self) -> usize {
        self.inner
            .read()
            .await
            .values()
            .filter(|t| !t.pull.state.finished())
            .count()
    }

    /// Call off a download, keeping what it has already fetched.
    ///
    /// The task notices within one chunk, so this returns as soon as it has
    /// asked. Cancelling something already finished is a conflict rather than a
    /// silent success: the caller believes they stopped something, and did not.
    pub async fn cancel(&self, id: &str) -> Result<()> {
        let tracked = self.inner.read().await;
        let tracked = tracked
            .get(id)
            .ok_or_else(|| Error::not_found("download"))?;
        if tracked.pull.state.finished() {
            return Err(Error::conflict("that download has already finished"));
        }
        tracked.cancel.cancel();
        Ok(())
    }

    /// Carry on a download that was stopped.
    ///
    /// It picks up where it left off rather than starting again: whole files are
    /// skipped and a partial one continues from its own length. On a model
    /// measured in tens of gigabytes that is the difference between a resume and
    /// a button that throws away an hour.
    pub async fn resume(&self, ctx: PullContext, id: &str) -> Result<Pull> {
        let (described, pull) = {
            let mut pulls = self.inner.write().await;
            let tracked = pulls
                .get_mut(id)
                .ok_or_else(|| Error::not_found("download"))?;

            match tracked.pull.state {
                PullState::Ready => {
                    return Err(Error::conflict("that download has already finished"));
                }
                PullState::Downloading | PullState::Verifying => {
                    // Already going. Saying so beats starting a second task
                    // writing the same bytes into the same directory.
                    return Ok(tracked.pull.clone());
                }
                PullState::Failed | PullState::Cancelled => {}
            }

            // A fresh token: the old one is spent, and reusing it would cancel
            // the new attempt the instant it started.
            tracked.cancel = CancellationToken::new();
            tracked.pull.state = PullState::Downloading;
            tracked.pull.error = None;
            tracked.pull.updated_at = Utc::now();
            (tracked.described.clone(), tracked.pull.clone())
        };

        tracing::info!(pull = %id, repo = %described.repo, "download resumed");
        self.spawn(ctx, id.to_string(), described);
        Ok(pull)
    }

    /// Forget a download, and remove whatever it fetched.
    ///
    /// The one thing here that throws work away, which is why it is its own verb
    /// rather than something cancelling does quietly. A download still running
    /// is stopped first, so this never races the task that is writing.
    pub async fn forget(&self, incoming: &std::path::Path, id: &str) -> Result<()> {
        {
            let pulls = self.inner.read().await;
            let tracked = pulls.get(id).ok_or_else(|| Error::not_found("download"))?;
            tracked.cancel.cancel();
        }
        // Dropped from the map first, so nothing can resume it while its bytes
        // are being removed.
        self.inner.write().await.remove(id);
        tokio::fs::remove_dir_all(incoming.join(id)).await.ok();
        tracing::info!(pull = %id, "download deleted");
        Ok(())
    }

    /// Run a download on its own task, and settle it however it ends.
    fn spawn(&self, ctx: PullContext, id: String, described: crate::hub::HubRepo) {
        let pulls = self.clone();
        tokio::spawn(async move {
            let cancel = {
                let pulls_map = pulls.inner.read().await;
                match pulls_map.get(&id) {
                    Some(tracked) => tracked.cancel.clone(),
                    // Deleted between being asked for and starting.
                    None => return,
                }
            };
            let outcome = pulls.run(&ctx, &id, &described, &cancel).await;
            pulls.settle(&ctx, &id, outcome).await;
        });
    }

    /// Check everything, then start.
    ///
    /// Returns as soon as the work is under way. Every failure this can produce
    /// is one that would otherwise have surfaced minutes later.
    pub async fn start(
        &self,
        ctx: PullContext,
        repo: &str,
        revision: Option<&str>,
        want: Option<&str>,
    ) -> Result<Pull> {
        // The library is asked first, because it is the only thing that can say
        // what a repository actually contains and what commit it is at.
        let described = ctx.hub.describe(repo, revision, want).await?;

        // Nothing to fetch means the description was an ambiguity rather than a
        // plan: the repository publishes its weights at several compression
        // levels and none was named. Describe answers that with every choice
        // and no files, so somebody can be shown what to pick; a pull is the
        // commitment, and it has to refuse.
        if described.files.is_empty() {
            return Err(Error::invalid(format!(
                "this model is published at {} compression levels: name the one to fetch",
                described.choices.len()
            )));
        }

        if let Some(existing) = ctx.catalog.find(&described.repo, &described.revision).await {
            return Err(Error::conflict(format!(
                "this node already has {} at this version",
                existing.name
            )));
        }

        // Refused here rather than by a 401 partway through the transfer. The
        // library says which repositories are gated, and a node with no
        // credentials cannot fetch one however much room it has.
        let allowed = access(&described, ctx.hub.has_token());
        if !allowed.ok {
            return Err(Error::invalid(allowed.reason.unwrap_or_else(|| {
                "this model cannot be fetched by this node".into()
            })));
        }

        // And refused here rather than only on the screen that offered it.
        //
        // The screen disables the button, which is what a person sees, but a
        // verdict that lives only in a rendering is a verdict anything else can
        // walk past: the console is not the only caller, and a check that has to
        // be repeated by every future one is a check that will eventually be
        // missed. This is the route every download goes through.
        let runnable = ctx
            .engine
            .can_run(described.architecture.as_deref(), described.kind);
        if !runnable.ok {
            return Err(Error::invalid(
                runnable
                    .reason
                    .unwrap_or_else(|| "this machine cannot run this model".into()),
            ));
        }

        let machine = Machine::read(&ctx.incoming);
        let room = machine.can_store(described.size_bytes);
        if !room.ok {
            return Err(Error::invalid(
                room.reason
                    .unwrap_or_else(|| "there is not enough room".into()),
            ));
        }

        let id = uuid::Uuid::new_v4().to_string();
        let now = Utc::now();
        let pull = Pull {
            id: id.clone(),
            repo: described.repo.clone(),
            revision: described.revision.clone(),
            state: PullState::Downloading,
            bytes_done: 0,
            bytes_total: described.size_bytes,
            files_done: 0,
            files_total: described.files.len() as u32,
            error: None,
            model_uid: None,
            started_at: now,
            updated_at: now,
        };

        {
            let mut pulls = self.inner.write().await;
            self.forget_old(&mut pulls, now);
            pulls.insert(
                id.clone(),
                Tracked {
                    pull: pull.clone(),
                    cancel: CancellationToken::new(),
                    described: described.clone(),
                },
            );
        }

        tracing::info!(
            pull = %id,
            repo = %described.repo,
            revision = %described.revision,
            files = described.files.len(),
            bytes = described.size_bytes,
            "download started"
        );

        self.spawn(ctx, id, described);
        Ok(pull)
    }

    /// Fetch, verify, and put the model in place. Every early return here is
    /// something [`Self::settle`] has to clean up after.
    async fn run(
        &self,
        ctx: &PullContext,
        id: &str,
        described: &crate::hub::HubRepo,
        cancel: &CancellationToken,
    ) -> Result<ModelEntry> {
        let staging = ctx.incoming.join(id);
        tokio::fs::create_dir_all(&staging).await?;

        // Counted by the download and read by the ticker below, so bytes are
        // reported while a file is arriving rather than only once it has.
        let done = Arc::new(std::sync::atomic::AtomicU64::new(0));
        let ticker = self.clone().tick(id.to_string(), done.clone());

        let outcome = self
            .fetch(ctx, id, described, cancel, &staging, &done)
            .await;
        // Stopped whatever happened, so a failed pull does not leave a task
        // updating a record nobody is going to look at again.
        ticker.abort();
        outcome?;

        self.set_state(id, PullState::Verifying).await;
        verify(&staging, &described.files).await?;

        // The rename. Before it, nothing in the catalogue points here; after it,
        // the weights are whole. There is no state in between.
        let uid = Catalog::mint_uid();
        let dir = ctx.catalog.dir(&uid);
        tokio::fs::create_dir_all(&dir).await?;
        let weights = ModelEntry::weights_path(&dir);
        tokio::fs::rename(&staging, &weights).await?;

        let entry = ModelEntry {
            uid,
            repo: described.repo.clone(),
            revision: described.revision.clone(),
            handle: ctx.catalog.mint_handle(&described.name).await,
            name: described.name.clone(),
            kind: described.kind,
            facts: Facts {
                license: described.license.clone(),
                files: described.files.len() as u32,
                size_bytes: described.size_bytes,
                ..read_facts(&weights).await
            },
            settings: Default::default(),
            resident: false,
            added_at: Utc::now(),
        };

        ctx.catalog.insert(entry.clone()).await?;

        // Registered, not loaded. A model arrives cold; making it resident is a
        // separate decision somebody makes (KB/35).
        if let Err(err) = ctx.engine.register(&entry, &weights).await {
            // The weights are on disk and the catalogue knows them, so this is
            // not a failed pull: it is a model that will be registered at the
            // next boot. Recording it loudly and keeping the download is better
            // than throwing away an hour of transfer.
            tracing::error!(uid = %entry.uid, %err, "model downloaded but the engine would not take it");
        }

        Ok(entry)
    }

    /// Fetch every file into the staging directory.
    ///
    /// Split out from [`Self::run`] so that the progress ticker is stopped on
    /// the way out whether this succeeded, failed or was called off. A `?` in
    /// the middle of the loop would otherwise leave the ticker running.
    async fn fetch(
        &self,
        ctx: &PullContext,
        id: &str,
        described: &crate::hub::HubRepo,
        cancel: &CancellationToken,
        staging: &std::path::Path,
        done: &Arc<std::sync::atomic::AtomicU64>,
    ) -> Result<()> {
        use std::sync::atomic::Ordering;

        for (index, file) in described.files.iter().enumerate() {
            let target = staging.join(&file.path);
            // What a previous attempt left. A file already the right size is
            // finished and is not fetched again; a shorter one is continued from
            // where it stopped. Anything LONGER than expected is not this file,
            // so it starts again rather than being trusted.
            let have = match tokio::fs::metadata(&target).await {
                Ok(found) if found.len() == file.size => {
                    done.fetch_add(file.size, Ordering::Relaxed);
                    self.files_done(id, index as u32 + 1).await;
                    continue;
                }
                Ok(found) if found.len() < file.size => found.len(),
                _ => 0,
            };
            if have > 0 {
                done.fetch_add(have, Ordering::Relaxed);
            }

            ctx.hub
                .download_from(
                    crate::hub::Fetch {
                        repo: &described.repo,
                        revision: &described.revision,
                        file: &file.path,
                        into: &target,
                        have,
                    },
                    cancel,
                    |bytes| {
                        done.fetch_add(bytes, Ordering::Relaxed);
                    },
                )
                .await?;

            // The file count is exact and is written as each one lands. The byte
            // count is the ticker's, which is why they are updated separately.
            self.files_done(id, index as u32 + 1).await;
        }
        Ok(())
    }

    /// Publish the running byte count on an interval.
    ///
    /// Returns the task's handle so the caller can stop it. Nothing here is the
    /// truth: it copies a counter the download owns into the record a poller
    /// reads, and the settle step writes the final figure regardless.
    fn tick(
        self,
        id: String,
        done: Arc<std::sync::atomic::AtomicU64>,
    ) -> tokio::task::JoinHandle<()> {
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(PROGRESS_INTERVAL);
            interval.tick().await; // The first tick is immediate and says zero.
            loop {
                interval.tick().await;
                let bytes = done.load(std::sync::atomic::Ordering::Relaxed);
                let mut pulls = self.inner.write().await;
                let Some(tracked) = pulls.get_mut(&id) else {
                    return;
                };
                tracked.pull.bytes_done = bytes;
                tracked.pull.updated_at = Utc::now();
            }
        })
    }

    /// Record how a pull ended, and leave nothing behind if it ended badly.
    async fn settle(&self, _ctx: &PullContext, id: &str, outcome: Result<ModelEntry>) {
        // Nothing is removed here. What a stopped or failed download fetched is
        // KEPT so it can be resumed, and `forget` is the only thing that throws
        // bytes away.
        let mut pulls = self.inner.write().await;
        let Some(tracked) = pulls.get_mut(id) else {
            return;
        };

        match outcome {
            Ok(entry) => {
                tracked.pull.state = PullState::Ready;
                tracked.pull.bytes_done = tracked.pull.bytes_total;
                tracked.pull.files_done = tracked.pull.files_total;
                tracked.pull.model_uid = Some(entry.uid.clone());
                tracing::info!(pull = %id, uid = %entry.uid, repo = %entry.repo, "download ready");
            }
            Err(err) => {
                let cancelled = tracked.cancel.is_cancelled();
                tracked.pull.state = if cancelled {
                    PullState::Cancelled
                } else {
                    PullState::Failed
                };
                tracked.pull.error = Some(err.to_string());
                if cancelled {
                    // What it fetched is KEPT, so resuming continues rather than
                    // starts over. On a forty gigabyte model that is the whole
                    // difference between a resume and a retry. Nothing here can
                    // be mistaken for a model: the rename into `models/` is the
                    // only way in, and only a whole verified download gets one.
                    tracing::info!(pull = %id, "download called off, keeping what arrived");
                } else {
                    tracing::error!(pull = %id, %err, "download failed, keeping what arrived");
                }
            }
        }
        tracked.pull.updated_at = Utc::now();
    }

    async fn files_done(&self, id: &str, files_done: u32) {
        let mut pulls = self.inner.write().await;
        if let Some(tracked) = pulls.get_mut(id) {
            tracked.pull.files_done = files_done;
            tracked.pull.updated_at = Utc::now();
        }
    }

    async fn set_state(&self, id: &str, state: PullState) {
        let mut pulls = self.inner.write().await;
        if let Some(tracked) = pulls.get_mut(id) {
            tracked.pull.state = state;
            tracked.pull.updated_at = Utc::now();
        }
    }

    /// Drop finished pulls nobody is going to ask about again.
    ///
    /// A download that was STOPPED is kept: its bytes are still on the disk and
    /// somebody may resume it. Forgetting the record while the bytes remain
    /// would leave a directory nothing can finish and nothing can delete.
    fn forget_old(&self, pulls: &mut HashMap<String, Tracked>, now: DateTime<Utc>) {
        pulls.retain(|_, t| {
            !t.pull.state.finished()
                || matches!(t.pull.state, PullState::Cancelled | PullState::Failed)
                || now - t.pull.updated_at < KEEP_FINISHED
        });
    }
}

impl Default for Pulls {
    fn default() -> Self {
        Self::new()
    }
}

/// Where a person goes to accept a licence and mint a token.
///
/// Written out because a refusal that says "the model's own page" names no
/// site, no address and no account: somebody reading it has to already know the
/// answer in order to know where to go.
const LIBRARY_SITE: &str = "https://huggingface.co";

/// Whether this node may fetch a repository at all.
///
/// Shared by the describe call and the pull itself on purpose: the thing an
/// administrator is shown before they commit and the thing that is checked when
/// they do have to be the same rule, or approval stops meaning success.
pub fn access(repo: &crate::hub::HubRepo, has_token: bool) -> crate::machine::Verdict {
    if !repo.gated || has_token {
        return crate::machine::Verdict::allowed();
    }

    // Said as two steps somebody can actually take, and naming the model,
    // because the refusal is read on a screen that may be showing several.
    //
    // The first version of this stated the problem and stopped there ("this
    // node has no credentials for the library"), which is true, is not
    // actionable, and reads as a fault in the product rather than as a
    // publisher's condition that a person is allowed to meet. It is also the
    // one refusal here that is nobody's mistake: this model is meant to be
    // downloadable, by this person, once they have agreed to its terms.
    //
    // What is NOT claimed is that we can agree on their behalf. That agreement
    // is between them and whoever published the weights, made under their own
    // account, and there is no way to do it for them that would not amount to
    // signing something in their name.
    crate::machine::Verdict::refused(format!(
        "{} cannot be downloaded by anyone who has not agreed to its licence. That agreement is \
         between you and whoever published it, so it has to be made under your own account and \
         nobody can make it for you. Two steps, both on the site the model comes from: open \
         {LIBRARY_SITE}/{} and accept the licence there, then create an access token in your \
         account settings. The token is what proves the agreement, and once it is saved here \
         every machine on this installation can fetch this model.",
        repo.name, repo.repo
    ))
}

/// Every file present, at the size the library said it would be.
///
/// A truncated download is the failure this catches, and it is the one that
/// matters: a short weights file loads far enough to look like it worked.
async fn verify(staging: &std::path::Path, expected: &[HubFile]) -> Result<()> {
    for file in expected {
        let path = staging.join(&file.path);
        let found = tokio::fs::metadata(&path)
            .await
            .map_err(|_| Error::invalid(format!("{} did not arrive", file.path)))?;
        if found.len() != file.size {
            return Err(Error::invalid(format!(
                "{} arrived incomplete: {} of {} bytes",
                file.path,
                found.len(),
                file.size
            )));
        }
    }
    Ok(())
}

/// What the weights say about themselves.
///
/// Read once, here, and never again: these are properties of what was
/// downloaded. Anything missing stays missing rather than being guessed, because
/// a wrong context length shown as a fact is worse than a blank one.
async fn read_facts(weights: &std::path::Path) -> Facts {
    let Ok(raw) = tokio::fs::read(weights.join("config.json")).await else {
        return Facts::default();
    };
    let Ok(config) = serde_json::from_slice::<serde_json::Value>(&raw) else {
        return Facts::default();
    };

    Facts {
        architecture: config["architectures"][0]
            .as_str()
            .or_else(|| config["model_type"].as_str())
            .map(str::to_string),
        context_length: config["max_position_embeddings"]
            .as_u64()
            .or_else(|| config["n_positions"].as_u64())
            .and_then(|n| u32::try_from(n).ok()),
        published_format: config["quantization_config"]["quant_method"]
            .as_str()
            .map(str::to_string),
        ..Facts::default()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn repo(gated: bool) -> crate::hub::HubRepo {
        crate::hub::HubRepo {
            repo: "vendor/model".into(),
            name: "model".into(),
            revision: "abc123".into(),
            kind: crate::catalog::ModelKind::Chat,
            license: None,
            gated,
            architecture: None,
            files: vec![],
            size_bytes: 0,
            choices: vec![],
        }
    }

    #[test]
    fn a_gated_model_is_refused_when_this_node_has_no_credentials() {
        // The failure this replaces: a 401 forty minutes into a transfer, long
        // after the person who started it stopped looking.
        let verdict = access(&repo(true), false);
        assert!(!verdict.ok);
        let reason = verdict.reason.expect("refused without saying why");
        assert!(reason.contains("model"), "{reason}");
    }

    #[test]
    fn a_gated_models_refusal_says_what_to_do_about_it() {
        // The distinguishing property of this refusal: it is the only one that
        // is nobody's mistake. The model is meant to be downloadable, by this
        // person, once they have agreed to the publisher's terms. So it has to
        // name both steps, or it reads as a dead end and as a fault in the
        // product rather than a condition somebody is allowed to meet.
        let reason = access(&repo(true), false).reason.unwrap();
        assert!(
            reason.contains("accept the licence"),
            "no first step: {reason}"
        );
        assert!(reason.contains("access token"), "no second step: {reason}");
        // The ADDRESS, because "the model's own page" names no site and no
        // account, and somebody reading it has to already know the answer.
        assert!(
            reason.contains("https://huggingface.co/vendor/model"),
            "does not say where to go: {reason}"
        );
        // And it must not send anybody to a screen that does not exist.
        assert!(
            !reason.contains("Model library"),
            "points at a screen nobody has built: {reason}"
        );
        // And it names the model, because the screen may be showing several.
        assert!(reason.contains("model"), "{reason}");
        // It must not promise something we cannot do. Agreeing to a publisher's
        // terms on somebody's behalf is signing in their name.
        for overreach in ["we will accept", "accept for you", "automatically accept"] {
            assert!(
                !reason.to_lowercase().contains(overreach),
                "{reason:?} promises to accept terms for somebody"
            );
        }
    }

    #[test]
    fn a_gated_model_is_allowed_when_this_node_does_have_credentials() {
        assert!(access(&repo(true), true).ok);
    }

    #[test]
    fn an_open_model_needs_no_credentials() {
        assert!(access(&repo(false), false).ok);
        assert!(access(&repo(false), true).ok);
    }

    #[test]
    fn only_the_three_terminal_states_are_finished() {
        assert!(PullState::Ready.finished());
        assert!(PullState::Failed.finished());
        assert!(PullState::Cancelled.finished());
        assert!(!PullState::Downloading.finished());
        assert!(!PullState::Verifying.finished());
    }

    #[test]
    fn states_are_named_in_product_words_on_the_wire() {
        let pairs = [
            (PullState::Downloading, "downloading"),
            (PullState::Verifying, "verifying"),
            (PullState::Ready, "ready"),
            (PullState::Failed, "failed"),
            (PullState::Cancelled, "cancelled"),
        ];
        for (state, name) in pairs {
            assert_eq!(serde_json::to_value(state).unwrap(), name);
        }
    }

    #[tokio::test]
    async fn a_truncated_file_fails_verification() {
        let temp = tempfile::tempdir().unwrap();
        tokio::fs::write(temp.path().join("model.safetensors"), b"short")
            .await
            .unwrap();

        let expected = vec![HubFile {
            path: "model.safetensors".into(),
            size: 5_000,
        }];
        let err = verify(temp.path(), &expected)
            .await
            .expect_err("a truncated model passed verification");
        assert!(err.to_string().contains("incomplete"), "{err}");
    }

    #[tokio::test]
    async fn a_missing_file_fails_verification() {
        let temp = tempfile::tempdir().unwrap();
        let expected = vec![HubFile {
            path: "config.json".into(),
            size: 10,
        }];
        let err = verify(temp.path(), &expected)
            .await
            .expect_err("a missing file passed verification");
        assert!(err.to_string().contains("did not arrive"), "{err}");
    }

    #[tokio::test]
    async fn a_complete_download_passes_verification() {
        let temp = tempfile::tempdir().unwrap();
        tokio::fs::create_dir_all(temp.path().join("sub"))
            .await
            .unwrap();
        tokio::fs::write(temp.path().join("sub/model.safetensors"), vec![0u8; 32])
            .await
            .unwrap();
        tokio::fs::write(temp.path().join("config.json"), vec![0u8; 8])
            .await
            .unwrap();

        let expected = vec![
            HubFile {
                path: "sub/model.safetensors".into(),
                size: 32,
            },
            HubFile {
                path: "config.json".into(),
                size: 8,
            },
        ];
        verify(temp.path(), &expected)
            .await
            .expect("a whole download was refused");
    }

    #[tokio::test]
    async fn facts_are_read_from_the_weights() {
        let temp = tempfile::tempdir().unwrap();
        tokio::fs::write(
            temp.path().join("config.json"),
            br#"{"architectures":["LlamaForCausalLM"],"max_position_embeddings":131072}"#,
        )
        .await
        .unwrap();

        let facts = read_facts(temp.path()).await;
        assert_eq!(facts.architecture.as_deref(), Some("LlamaForCausalLM"));
        assert_eq!(facts.context_length, Some(131_072));
    }

    #[tokio::test]
    async fn weights_that_say_nothing_leave_the_facts_blank_rather_than_guessed() {
        let temp = tempfile::tempdir().unwrap();
        // No config at all.
        let facts = read_facts(temp.path()).await;
        assert!(facts.architecture.is_none());
        assert!(facts.context_length.is_none());

        // A config that will not parse is the same answer, not a panic.
        tokio::fs::write(temp.path().join("config.json"), b"{ not json")
            .await
            .unwrap();
        assert!(read_facts(temp.path()).await.architecture.is_none());
    }

    #[tokio::test]
    async fn an_older_context_length_spelling_is_read_too() {
        let temp = tempfile::tempdir().unwrap();
        tokio::fs::write(
            temp.path().join("config.json"),
            br#"{"model_type":"gpt2","n_positions":1024}"#,
        )
        .await
        .unwrap();

        let facts = read_facts(temp.path()).await;
        assert_eq!(facts.architecture.as_deref(), Some("gpt2"));
        assert_eq!(facts.context_length, Some(1024));
    }

    #[tokio::test]
    async fn clearing_incoming_takes_every_abandoned_download() {
        let temp = tempfile::tempdir().unwrap();
        for id in ["pull-a", "pull-b"] {
            let dir = temp.path().join(id);
            tokio::fs::create_dir_all(&dir).await.unwrap();
            tokio::fs::write(dir.join("part.safetensors"), b"half")
                .await
                .unwrap();
        }

        Pulls::clear_incoming(temp.path()).await.unwrap();

        let mut left = tokio::fs::read_dir(temp.path()).await.unwrap();
        assert!(
            left.next_entry().await.unwrap().is_none(),
            "a partial download survived"
        );
    }

    #[tokio::test]
    async fn clearing_a_directory_that_is_not_there_is_not_a_failure() {
        // First boot on a fresh machine.
        Pulls::clear_incoming(std::path::Path::new("/nonexistent/incoming"))
            .await
            .expect("first boot failed");
    }

    /// A pull that is already in whatever state a test needs.
    async fn stopped_pull(temp: &std::path::Path, state: PullState) -> (Pulls, String) {
        let pulls = Pulls::new();
        let id = "p-1".to_string();
        let described = repo(false);
        let now = Utc::now();
        pulls.inner.write().await.insert(
            id.clone(),
            Tracked {
                pull: Pull {
                    id: id.clone(),
                    repo: described.repo.clone(),
                    revision: described.revision.clone(),
                    state,
                    bytes_done: 5,
                    bytes_total: 10,
                    files_done: 0,
                    files_total: 1,
                    error: None,
                    model_uid: None,
                    started_at: now,
                    updated_at: now,
                },
                cancel: CancellationToken::new(),
                described,
            },
        );
        tokio::fs::create_dir_all(temp.join(&id)).await.unwrap();
        tokio::fs::write(temp.join(&id).join("part"), b"half")
            .await
            .unwrap();
        (pulls, id)
    }

    #[tokio::test]
    async fn stopping_a_download_keeps_what_it_fetched() {
        // The whole point of resuming: cancel used to delete the staging
        // directory, so there was nothing left to continue from and "resume"
        // could only ever have meant "start again".
        let temp = tempfile::tempdir().unwrap();
        let (pulls, id) = stopped_pull(temp.path(), PullState::Downloading).await;

        pulls.cancel(&id).await.unwrap();
        assert!(
            temp.path().join(&id).join("part").exists(),
            "cancelling threw away what had been fetched"
        );
    }

    #[tokio::test]
    async fn deleting_a_download_removes_its_bytes_and_its_record() {
        let temp = tempfile::tempdir().unwrap();
        let (pulls, id) = stopped_pull(temp.path(), PullState::Cancelled).await;

        pulls.forget(temp.path(), &id).await.unwrap();
        assert!(!temp.path().join(&id).exists(), "the bytes are still there");
        assert!(matches!(pulls.get(&id).await, Err(Error::NotFound(_))));
        // Deleting twice is a not found, not a silent success: the caller
        // believes they removed something.
        assert!(pulls.forget(temp.path(), &id).await.is_err());
    }

    #[tokio::test]
    async fn a_download_that_is_still_running_is_not_resumed_twice() {
        // Two tasks writing the same bytes into the same directory is the one
        // outcome resume must never produce.
        let temp = tempfile::tempdir().unwrap();
        let (pulls, id) = stopped_pull(temp.path(), PullState::Downloading).await;
        let ctx = PullContext {
            hub: Arc::new(Hub::new("https://example.invalid", None)),
            catalog: Catalog::open(temp.path().join("models")).await.unwrap(),
            engine: Arc::new(crate::engine::absent::AbsentEngine::new()),
            incoming: temp.path().to_path_buf(),
        };

        let same = pulls.resume(ctx, &id).await.unwrap();
        assert_eq!(same.state, PullState::Downloading);
    }

    #[tokio::test]
    async fn a_finished_download_cannot_be_resumed() {
        let temp = tempfile::tempdir().unwrap();
        let (pulls, id) = stopped_pull(temp.path(), PullState::Ready).await;
        let ctx = PullContext {
            hub: Arc::new(Hub::new("https://example.invalid", None)),
            catalog: Catalog::open(temp.path().join("models")).await.unwrap(),
            engine: Arc::new(crate::engine::absent::AbsentEngine::new()),
            incoming: temp.path().to_path_buf(),
        };
        assert!(matches!(
            pulls.resume(ctx, &id).await,
            Err(Error::Conflict(_))
        ));
    }

    #[tokio::test]
    async fn a_stopped_download_is_not_forgotten_by_the_sweep() {
        // Its bytes are on the disk and somebody may resume it. Dropping the
        // record while the bytes remain leaves a directory nothing can finish
        // and nothing can delete.
        let pulls = Pulls::new();
        let mut map = HashMap::new();
        let long_ago = Utc::now() - chrono::TimeDelta::hours(4);
        for (id, state) in [
            ("done", PullState::Ready),
            ("stopped", PullState::Cancelled),
        ] {
            map.insert(
                id.to_string(),
                Tracked {
                    pull: Pull {
                        id: id.into(),
                        repo: "v/m".into(),
                        revision: "abc".into(),
                        state,
                        bytes_done: 0,
                        bytes_total: 0,
                        files_done: 0,
                        files_total: 0,
                        error: None,
                        model_uid: None,
                        started_at: long_ago,
                        updated_at: long_ago,
                    },
                    cancel: CancellationToken::new(),
                    described: repo(false),
                },
            );
        }
        pulls.forget_old(&mut map, Utc::now());
        assert!(!map.contains_key("done"), "a finished download was kept");
        assert!(
            map.contains_key("stopped"),
            "a stopped download was forgotten"
        );
    }

    #[tokio::test]
    async fn asking_about_a_download_that_never_existed_is_a_not_found() {
        let pulls = Pulls::new();
        assert!(matches!(pulls.get("nope").await, Err(Error::NotFound(_))));
        assert!(matches!(
            pulls.cancel("nope").await,
            Err(Error::NotFound(_))
        ));
    }
}
