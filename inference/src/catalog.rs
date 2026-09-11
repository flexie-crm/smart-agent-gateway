//! What is on this disk.
//!
//! The node is the authority on its own storage and the orchestrator is the only
//! writer of rows (KB/35): this module is the node's half of that bargain. It
//! remembers which models were pulled, what was learned about them while they
//! were pulled, and what an administrator has since set on them.
//!
//! Two rules shape the whole thing.
//!
//! **A name a person typed never reaches a path.** A model lives under a minted
//! id, exactly as an uploaded file does on the server side (KB/23). The
//! repository it came from is data inside the entry, not a directory name, so
//! there is no sanitising step here to get wrong: a repository called
//! `../../etc` is a string in a JSON file and nothing else.
//!
//! **A half-written entry is never read.** Every write goes to a temporary file
//! beside its target and is renamed over it, so a reader sees the old entry or
//! the new one. A process killed mid-write leaves a stray temporary file, which
//! the next scan ignores, and never a truncated model.

use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
    sync::Arc,
};

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use tokio::sync::RwLock;

use crate::error::{Error, Result};

/// The file inside a model's directory that describes it.
const ENTRY_FILE: &str = "model.json";

/// The directory inside a model's directory that holds the weights.
const WEIGHTS_DIR: &str = "weights";

/// What kind of work a model does.
///
/// Exactly the five the registry already has (`ai_models.type`), spelled the
/// same, so what this node answers drops into a row without translation. A
/// sixth kind here that the registry does not have would be a model nothing can
/// be pinned to.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum ModelKind {
    Chat,
    Embedding,
    Rerank,
    Stt,
    Tts,
}

impl ModelKind {
    /// Guess from what the library says about a repository.
    ///
    /// A guess, and named one: it is the starting value of a field an
    /// administrator can correct, not a fact. Getting it wrong costs a dropdown
    /// change; refusing to guess costs one on every single pull.
    pub fn infer(pipeline_tag: Option<&str>, tags: &[String]) -> Self {
        let has = |t: &str| tags.iter().any(|tag| tag.eq_ignore_ascii_case(t));
        match pipeline_tag.unwrap_or("") {
            "automatic-speech-recognition" => Self::Stt,
            "text-to-speech" | "text-to-audio" => Self::Tts,
            "sentence-similarity" | "feature-extraction" => Self::Embedding,
            _ if has("sentence-transformers") => Self::Embedding,
            _ if has("reranker") || has("cross-encoder") => Self::Rerank,
            _ => Self::Chat,
        }
    }
}

/// What was read off the weights, once, and cannot be changed.
///
/// Shown read-only. These are properties of what was downloaded, and an input
/// around one invites somebody to change something that is not a choice.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Facts {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub architecture: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub parameters: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub context_length: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub license: Option<String>,
    /// How the weights were published, if they were published compressed. This
    /// is not the `quantization` setting, which is what we do to them on load.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub published_format: Option<String>,
    pub files: u32,
    pub size_bytes: u64,
}

/// One model this node holds.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ModelEntry {
    /// Minted here. The directory name, and the id every other call uses.
    pub uid: String,
    /// Where it came from, as data.
    pub repo: String,
    /// The exact commit that was fetched.
    ///
    /// A repository is a moving target, and "the model we tested" has to mean
    /// one set of bytes. Recording the resolved commit is what makes a pull
    /// reproducible and what lets us say later that a node is behind.
    pub revision: String,
    /// What to call it. Starts as the repository's own name.
    pub name: String,
    /// What the gateway asks for when it wants this model.
    ///
    /// Readable, unique on this node, and stable once minted. It exists because
    /// `uid` is a uuid, and a model list reading "gpu-box-1 / 48e61d05-9f36" is
    /// a list nobody can use.
    ///
    /// It is NOT the directory name and must never be used as one. That is the
    /// whole reason the two are separate: `uid` is derived from nothing and is
    /// safe as a path by construction, while this is derived from a repository
    /// name and is only ever an identifier. The worst a bad handle can do is
    /// make a model unaddressable.
    #[serde(default)]
    pub handle: String,
    pub kind: ModelKind,
    pub facts: Facts,
    /// What an administrator set, by the keys the settings form declared.
    #[serde(default)]
    pub settings: BTreeMap<String, String>,
    /// Whether this node should keep it in memory. The administrator's decision
    /// (KB/35), not a policy: a cache cannot infer which specialist somebody is
    /// waiting on.
    #[serde(default)]
    pub resident: bool,
    pub added_at: DateTime<Utc>,
}

impl ModelEntry {
    /// Where the weights are, given the directory this model owns.
    pub fn weights_path(dir: &Path) -> PathBuf {
        dir.join(WEIGHTS_DIR)
    }
}

/// Every model on this disk, and the disk itself.
#[derive(Clone)]
pub struct Catalog {
    root: PathBuf,
    entries: Arc<RwLock<BTreeMap<String, ModelEntry>>>,
}

impl Catalog {
    /// Read what is on disk into memory.
    ///
    /// A directory that will not parse is LOGGED AND SKIPPED rather than
    /// failing the boot. One unreadable entry out of twenty is a model that
    /// needs looking at; refusing to start over it takes the other nineteen
    /// down with it, on a machine somebody is relying on.
    pub async fn open(root: impl Into<PathBuf>) -> Result<Self> {
        let root = root.into();
        tokio::fs::create_dir_all(&root).await?;

        let mut entries = BTreeMap::new();
        let mut dir = tokio::fs::read_dir(&root).await?;
        while let Some(item) = dir.next_entry().await? {
            if !item.file_type().await?.is_dir() {
                continue;
            }
            let path = item.path().join(ENTRY_FILE);
            match read_entry(&path).await {
                Ok(entry) => {
                    entries.insert(entry.uid.clone(), entry);
                }
                Err(err) => {
                    tracing::warn!(path = %path.display(), %err, "skipping unreadable model");
                }
            }
        }

        tracing::info!(models = entries.len(), root = %root.display(), "catalogue open");
        Ok(Self {
            root,
            entries: Arc::new(RwLock::new(entries)),
        })
    }

    /// The directory a model owns.
    pub fn dir(&self, uid: &str) -> PathBuf {
        self.root.join(uid)
    }

    /// Where a model's weights are.
    pub fn weights(&self, uid: &str) -> PathBuf {
        ModelEntry::weights_path(&self.dir(uid))
    }

    pub async fn list(&self) -> Vec<ModelEntry> {
        self.entries.read().await.values().cloned().collect()
    }

    pub async fn get(&self, uid: &str) -> Result<ModelEntry> {
        self.entries
            .read()
            .await
            .get(uid)
            .cloned()
            .ok_or_else(|| Error::not_found("model"))
    }

    /// Mint a handle nothing else on this node is using.
    ///
    /// Derived from what the repository calls itself, reduced to characters that
    /// are safe in a model id, and suffixed until it is free. A repository whose
    /// name survives none of that falls back to the word "model", which is
    /// unhelpful and still addressable, where an empty handle would not be.
    pub async fn mint_handle(&self, from: &str) -> String {
        let cleaned: String = from
            .chars()
            .map(|c| {
                if c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.') {
                    c
                } else {
                    '-'
                }
            })
            .collect();
        let trimmed = cleaned.trim_matches(['-', '.']);
        let base: String = if trimmed.is_empty() {
            "model".into()
        } else {
            trimmed.chars().take(96).collect()
        };

        let taken: std::collections::BTreeSet<String> = self
            .entries
            .read()
            .await
            .values()
            .map(|e| e.handle.clone())
            .collect();

        if !taken.contains(&base) {
            return base;
        }
        // Two commits of one repository are two models and both have to be
        // reachable, so the second gets a number rather than the first's name.
        for n in 2..1000 {
            let candidate = format!("{base}-{n}");
            if !taken.contains(&candidate) {
                return candidate;
            }
        }
        format!("{base}-{}", uuid::Uuid::new_v4().simple())
    }

    /// Whether a repository at a revision is already here, and as what.
    ///
    /// Pulling the same commit twice is somebody clicking twice, not somebody
    /// wanting two copies of forty gigabytes.
    pub async fn find(&self, repo: &str, revision: &str) -> Option<ModelEntry> {
        self.entries
            .read()
            .await
            .values()
            .find(|e| e.repo == repo && e.revision == revision)
            .cloned()
    }

    /// Mint an id for a model about to be pulled.
    ///
    /// Minted before the download starts, because the id is what the download
    /// writes under: there is no later step that could rename a directory into
    /// place under a name derived from anything a caller sent.
    pub fn mint_uid() -> String {
        uuid::Uuid::new_v4().to_string()
    }

    /// Record a model whose weights are already in place.
    pub async fn insert(&self, entry: ModelEntry) -> Result<()> {
        write_entry(&self.dir(&entry.uid).join(ENTRY_FILE), &entry).await?;
        self.entries
            .write()
            .await
            .insert(entry.uid.clone(), entry.clone());
        tracing::info!(uid = %entry.uid, repo = %entry.repo, "model added");
        Ok(())
    }

    /// Change an entry through a closure, and persist the result.
    ///
    /// The read, the change and the write happen under ONE write lock. Read,
    /// modify, write as three separate calls is how two settings saves that
    /// arrive together end up with one of them silently gone.
    pub async fn update(
        &self,
        uid: &str,
        change: impl FnOnce(&mut ModelEntry) -> Result<()>,
    ) -> Result<ModelEntry> {
        let mut entries = self.entries.write().await;
        let entry = entries
            .get_mut(uid)
            .ok_or_else(|| Error::not_found("model"))?;

        let mut candidate = entry.clone();
        change(&mut candidate)?;
        write_entry(&self.root.join(uid).join(ENTRY_FILE), &candidate).await?;
        *entry = candidate.clone();
        Ok(candidate)
    }

    /// Take a model off this disk.
    ///
    /// The entry is dropped from memory FIRST, so that nothing can start using
    /// a model whose files are being deleted underneath it. A directory that
    /// then fails to delete leaves bytes to reclaim, which is a smaller problem
    /// than a live reference to weights that are half gone.
    pub async fn remove(&self, uid: &str) -> Result<ModelEntry> {
        let entry = {
            let mut entries = self.entries.write().await;
            entries
                .remove(uid)
                .ok_or_else(|| Error::not_found("model"))?
        };
        let dir = self.dir(uid);
        if let Err(err) = tokio::fs::remove_dir_all(&dir).await {
            tracing::error!(path = %dir.display(), %err, "model removed from the catalogue but its files remain");
        }
        tracing::info!(uid = %uid, repo = %entry.repo, "model removed");
        Ok(entry)
    }

    /// Total bytes the models on this node occupy.
    pub async fn size_bytes(&self) -> u64 {
        self.entries
            .read()
            .await
            .values()
            .map(|e| e.facts.size_bytes)
            .sum()
    }
}

async fn read_entry(path: &Path) -> Result<ModelEntry> {
    let raw = tokio::fs::read(path).await?;
    Ok(serde_json::from_slice(&raw)?)
}

/// Write an entry so that a reader sees all of it or none of it.
///
/// The temporary file is a sibling, not a file in a temp directory, because a
/// rename is only atomic within one filesystem.
async fn write_entry(path: &Path, entry: &ModelEntry) -> Result<()> {
    let dir = path
        .parent()
        .ok_or_else(|| Error::invalid("a model entry has no directory"))?;
    tokio::fs::create_dir_all(dir).await?;

    let body = serde_json::to_vec_pretty(entry)?;
    let temp = path.with_extension("writing");
    tokio::fs::write(&temp, &body).await?;
    tokio::fs::rename(&temp, path).await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn entry(uid: &str, repo: &str) -> ModelEntry {
        ModelEntry {
            uid: uid.into(),
            repo: repo.into(),
            revision: "abc123".into(),
            name: repo.rsplit('/').next().unwrap_or(repo).into(),
            handle: repo.rsplit('/').next().unwrap_or(repo).into(),
            kind: ModelKind::Chat,
            facts: Facts {
                files: 3,
                size_bytes: 1024,
                ..Facts::default()
            },
            settings: BTreeMap::new(),
            resident: false,
            added_at: Utc::now(),
        }
    }

    #[tokio::test]
    async fn a_model_survives_a_restart() {
        let temp = tempfile::tempdir().unwrap();
        let catalog = Catalog::open(temp.path()).await.unwrap();
        catalog
            .insert(entry("uid-1", "vendor/model"))
            .await
            .unwrap();

        // Same disk, new process.
        let reopened = Catalog::open(temp.path()).await.unwrap();
        let got = reopened.get("uid-1").await.expect("model was lost");
        assert_eq!(got.repo, "vendor/model");
        assert_eq!(got.revision, "abc123");
        assert_eq!(got.facts.size_bytes, 1024);
    }

    #[tokio::test]
    async fn one_unreadable_entry_does_not_take_the_others_down() {
        let temp = tempfile::tempdir().unwrap();
        let catalog = Catalog::open(temp.path()).await.unwrap();
        catalog.insert(entry("good", "vendor/good")).await.unwrap();

        let broken = temp.path().join("broken");
        tokio::fs::create_dir_all(&broken).await.unwrap();
        tokio::fs::write(broken.join(ENTRY_FILE), b"{ not json")
            .await
            .unwrap();

        let reopened = Catalog::open(temp.path()).await.unwrap();
        assert_eq!(reopened.list().await.len(), 1, "the good model was dropped");
        assert!(reopened.get("good").await.is_ok());
    }

    #[tokio::test]
    async fn a_leftover_temporary_file_is_not_read_as_a_model() {
        let temp = tempfile::tempdir().unwrap();
        let catalog = Catalog::open(temp.path()).await.unwrap();
        catalog
            .insert(entry("uid-1", "vendor/model"))
            .await
            .unwrap();

        // What a process killed mid-write leaves behind.
        let stray = catalog.dir("uid-1").join("model.writing");
        tokio::fs::write(&stray, b"half a file").await.unwrap();

        let reopened = Catalog::open(temp.path()).await.unwrap();
        assert_eq!(reopened.list().await.len(), 1);
    }

    #[tokio::test]
    async fn settings_are_kept_and_the_entry_is_rewritten() {
        let temp = tempfile::tempdir().unwrap();
        let catalog = Catalog::open(temp.path()).await.unwrap();
        catalog
            .insert(entry("uid-1", "vendor/model"))
            .await
            .unwrap();

        catalog
            .update("uid-1", |e| {
                e.settings.insert("quantization".into(), "Q4K".into());
                e.resident = true;
                Ok(())
            })
            .await
            .unwrap();

        let reopened = Catalog::open(temp.path()).await.unwrap();
        let got = reopened.get("uid-1").await.unwrap();
        assert_eq!(
            got.settings.get("quantization").map(String::as_str),
            Some("Q4K")
        );
        assert!(got.resident, "residency was not persisted");
    }

    #[tokio::test]
    async fn a_refused_change_leaves_the_entry_alone() {
        let temp = tempfile::tempdir().unwrap();
        let catalog = Catalog::open(temp.path()).await.unwrap();
        catalog
            .insert(entry("uid-1", "vendor/model"))
            .await
            .unwrap();

        let err = catalog
            .update("uid-1", |e| {
                e.name = "half applied".into();
                Err(Error::invalid("no"))
            })
            .await
            .expect_err("a refused change was accepted");
        assert!(matches!(err, Error::Invalid(_)));

        // The closure mutated a COPY, so the refusal cannot have left the first
        // half of a change behind, in memory or on disk.
        assert_eq!(catalog.get("uid-1").await.unwrap().name, "model");
        let reopened = Catalog::open(temp.path()).await.unwrap();
        assert_eq!(reopened.get("uid-1").await.unwrap().name, "model");
    }

    #[tokio::test]
    async fn removing_a_model_takes_its_files_with_it() {
        let temp = tempfile::tempdir().unwrap();
        let catalog = Catalog::open(temp.path()).await.unwrap();
        catalog
            .insert(entry("uid-1", "vendor/model"))
            .await
            .unwrap();
        let weights = catalog.weights("uid-1");
        tokio::fs::create_dir_all(&weights).await.unwrap();
        tokio::fs::write(weights.join("w.safetensors"), b"bytes")
            .await
            .unwrap();

        catalog.remove("uid-1").await.unwrap();

        assert!(matches!(
            catalog.get("uid-1").await,
            Err(Error::NotFound(_))
        ));
        assert!(!catalog.dir("uid-1").exists(), "the files are still there");
        assert!(catalog.remove("uid-1").await.is_err(), "removed twice");
    }

    #[tokio::test]
    async fn a_repository_name_cannot_escape_the_root() {
        // The point of a minted id: a hostile repository name is data, and the
        // directory is a uuid whatever it says.
        let temp = tempfile::tempdir().unwrap();
        let catalog = Catalog::open(temp.path()).await.unwrap();
        let uid = Catalog::mint_uid();
        catalog
            .insert(entry(&uid, "../../../../etc/passwd"))
            .await
            .unwrap();

        let dir = catalog.dir(&uid);
        assert!(dir.starts_with(temp.path()), "{dir:?} escaped the root");
        assert!(dir.exists());
        assert_eq!(
            catalog.get(&uid).await.unwrap().repo,
            "../../../../etc/passwd",
            "the name was mangled instead of being kept as data"
        );
    }

    #[tokio::test]
    async fn a_repeated_pull_of_the_same_commit_is_recognised() {
        let temp = tempfile::tempdir().unwrap();
        let catalog = Catalog::open(temp.path()).await.unwrap();
        catalog
            .insert(entry("uid-1", "vendor/model"))
            .await
            .unwrap();

        assert!(catalog.find("vendor/model", "abc123").await.is_some());
        // A different commit of the same repository is a different model.
        assert!(catalog.find("vendor/model", "def456").await.is_none());
    }

    #[tokio::test]
    async fn a_handle_is_the_repository_name_when_it_is_free() {
        let temp = tempfile::tempdir().unwrap();
        let catalog = Catalog::open(temp.path()).await.unwrap();
        assert_eq!(catalog.mint_handle("Qwen3-0.6B").await, "Qwen3-0.6B");
    }

    #[tokio::test]
    async fn a_second_model_of_the_same_name_gets_a_number() {
        // Two commits of one repository are two models and both have to be
        // reachable, so the first keeps its name.
        let temp = tempfile::tempdir().unwrap();
        let catalog = Catalog::open(temp.path()).await.unwrap();

        let mut first = entry("uid-1", "vendor/Qwen3-0.6B");
        first.handle = catalog.mint_handle("Qwen3-0.6B").await;
        assert_eq!(first.handle, "Qwen3-0.6B");
        catalog.insert(first).await.unwrap();

        assert_eq!(catalog.mint_handle("Qwen3-0.6B").await, "Qwen3-0.6B-2");
    }

    #[tokio::test]
    async fn a_handle_keeps_nothing_that_would_not_be_an_identifier() {
        let temp = tempfile::tempdir().unwrap();
        let catalog = Catalog::open(temp.path()).await.unwrap();

        // Not a security property (the DIRECTORY is a uuid, which is what makes
        // a path safe); this is about producing something addressable.
        assert_eq!(catalog.mint_handle("../../etc/passwd").await, "etc-passwd");
        assert_eq!(catalog.mint_handle("a b/c").await, "a-b-c");
        assert_eq!(catalog.mint_handle("---").await, "model");
        assert_eq!(catalog.mint_handle("").await, "model");
        assert_eq!(
            catalog.mint_handle("x".repeat(200).as_str()).await.len(),
            96
        );
    }

    #[tokio::test]
    async fn a_handle_never_becomes_a_directory() {
        // The rule the two identifiers exist to keep apart.
        let temp = tempfile::tempdir().unwrap();
        let catalog = Catalog::open(temp.path()).await.unwrap();
        let uid = Catalog::mint_uid();
        let mut e = entry(&uid, "vendor/model");
        e.handle = "..".into();
        catalog.insert(e).await.unwrap();

        let dir = catalog.dir(&uid);
        assert!(dir.starts_with(temp.path()), "{dir:?} escaped the root");
        assert!(dir.ends_with(&uid), "the handle was used as a path");
    }

    #[tokio::test]
    async fn a_handle_survives_a_restart() {
        let temp = tempfile::tempdir().unwrap();
        let catalog = Catalog::open(temp.path()).await.unwrap();
        let mut e = entry("uid-1", "vendor/model");
        e.handle = "Qwen3-0.6B".into();
        catalog.insert(e).await.unwrap();

        let reopened = Catalog::open(temp.path()).await.unwrap();
        assert_eq!(reopened.get("uid-1").await.unwrap().handle, "Qwen3-0.6B");
        // And a new model on the reopened catalogue still sees it as taken.
        assert_eq!(reopened.mint_handle("Qwen3-0.6B").await, "Qwen3-0.6B-2");
    }

    #[test]
    fn minted_ids_do_not_repeat() {
        let a = Catalog::mint_uid();
        let b = Catalog::mint_uid();
        assert_ne!(a, b);
    }

    #[test]
    fn the_kind_is_inferred_from_what_the_library_says() {
        assert_eq!(
            ModelKind::infer(Some("automatic-speech-recognition"), &[]),
            ModelKind::Stt
        );
        assert_eq!(
            ModelKind::infer(Some("text-to-speech"), &[]),
            ModelKind::Tts
        );
        assert_eq!(
            ModelKind::infer(Some("feature-extraction"), &[]),
            ModelKind::Embedding
        );
        assert_eq!(
            ModelKind::infer(None, &["sentence-transformers".into()]),
            ModelKind::Embedding
        );
        assert_eq!(
            ModelKind::infer(None, &["cross-encoder".into()]),
            ModelKind::Rerank
        );
        // Anything unrecognised is a chat model, which is what most of them are
        // and what an administrator is least surprised to have to correct.
        assert_eq!(
            ModelKind::infer(Some("text-generation"), &[]),
            ModelKind::Chat
        );
        assert_eq!(ModelKind::infer(None, &[]), ModelKind::Chat);
    }

    #[test]
    fn model_kinds_are_spelled_the_way_the_registry_spells_them() {
        // `ai_models.type` is an enum of exactly these. A different spelling
        // here is a row the registry refuses.
        let pairs = [
            (ModelKind::Chat, "chat"),
            (ModelKind::Embedding, "embedding"),
            (ModelKind::Rerank, "rerank"),
            (ModelKind::Stt, "stt"),
            (ModelKind::Tts, "tts"),
        ];
        for (kind, name) in pairs {
            assert_eq!(serde_json::to_value(kind).unwrap(), name);
        }
    }
}
