//! The model library, and what we are willing to take from it.
//!
//! **The node talks to the library, not the orchestrator** (KB/35). It holds the
//! disk and it knows what this machine can run, so it is the only side that can
//! answer "can this machine use this model" before an administrator commits to
//! a download that takes an hour. Approval must equal success (CLAUDE.md), and
//! only the node can make that promise.
//!
//! Searching is broad and cheap and promises nothing. [`Hub::describe`] is the
//! promise: it resolves a repository to ONE commit, lists exactly the files that
//! would be fetched, and adds them up. That is the number an administrator says
//! yes to.
//!
//! The established client for this library (`hf-hub`) is deliberately not used,
//! and it is worth recording why rather than leaving it to look like an
//! oversight. It owns its own cache layout under the user's home directory,
//! reports progress through a synchronous trait shaped for a terminal bar, and
//! has no notion of being cancelled halfway. This node needs the opposite of all
//! three: files land under a minted id inside the directory the node owns, bytes
//! are counted for a poller on the other side of an HTTP call, and a pull can be
//! called off. What is left of the library once those are ours is two documented
//! JSON endpoints and a download, which is what is below.

use std::path::Path;

use serde::Deserialize;
use tokio::io::AsyncWriteExt;

use crate::{
    catalog::ModelKind,
    error::{Error, Result},
};

/// Files that are always taken when they exist: what a loader needs besides the
/// weights themselves. All small.
const ESSENTIAL_FILES: &[&str] = &[
    "config.json",
    "generation_config.json",
    "preprocessor_config.json",
    "processor_config.json",
    "tokenizer.json",
    "tokenizer_config.json",
    "special_tokens_map.json",
    "chat_template.json",
    "chat_template.jinja",
    "vocab.json",
    "merges.txt",
    "tokenizer.model",
];

/// One file to fetch, and how much of it is already here.
///
/// The five of these travelled as five arguments and the call read as a row of
/// anonymous strings at every site. Named together, a resume is legible: this
/// file, from that commit, and we already have `have` bytes of it.
#[derive(Debug, Clone, Copy)]
pub struct Fetch<'a> {
    pub repo: &'a str,
    pub revision: &'a str,
    pub file: &'a str,
    pub into: &'a Path,
    /// Bytes already on disk from a download that was stopped. Zero starts from
    /// the beginning.
    pub have: u64,
}

/// A search hit. Cheap, and promises nothing about whether it will run here.
#[derive(Debug, Clone, serde::Serialize)]
pub struct HubModel {
    pub repo: String,
    pub name: String,
    pub kind: ModelKind,
    pub downloads: u64,
    pub likes: u64,
}

/// One file in a repository.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct HubFile {
    pub path: String,
    pub size: u64,
}

/// A repository resolved to one commit, with the files that would be fetched.
#[derive(Debug, Clone, serde::Serialize)]
pub struct HubRepo {
    pub repo: String,
    pub name: String,
    /// The commit. Every later mention of this model means this and not "latest".
    pub revision: String,
    pub kind: ModelKind,
    pub license: Option<String>,
    /// Whether the library will require credentials before it hands the weights
    /// over. Known here and NOT in a search result, because the search endpoint
    /// does not report it: claiming it there marked every model on the screen as
    /// gated, which is worse than not saying.
    pub gated: bool,
    /// The class the weights declare themselves to be, e.g. `Qwen3ForCausalLM`.
    ///
    /// This is what an engine matches on to decide which loader to build, so it
    /// is the one field that answers "could this run here at all", and it is
    /// read HERE rather than after the download because that is the only time
    /// the answer is worth anything. The library already carries it in the same
    /// answer that gives us the commit, so knowing costs no extra request.
    ///
    /// `None` for a repository that did not say. Allowed rather than refused:
    /// see `Engine::can_run`.
    pub architecture: Option<String>,
    pub files: Vec<HubFile>,
    pub size_bytes: u64,
    /// Every weights file on offer, whether selected or not, so that a caller
    /// refused for ambiguity is told what to choose from.
    pub choices: Vec<HubFile>,
}

pub struct Hub {
    /// None when the client could not be built at all.
    ///
    /// **Not a reason to refuse to start.** This machine's job is to answer
    /// questions with the models on its disk; the library is where NEW ones come
    /// from, which is a thing somebody asks for later and not a thing serving
    /// depends on. It used to be fatal at boot, and the failure it produced was
    /// the one that matters most here: a machine with no CA certificates
    /// installed could not build an HTTPS client, so it exited 1 at startup with
    /// "the model library is unreachable", which names the network when the
    /// cause is a missing package on the machine itself. A rack of GPUs on a
    /// closed network is exactly the deployment this product is for, and it
    /// would never have come up.
    http: Option<reqwest::Client>,
    /// Why there is no client, kept so the refusal can say something true when
    /// somebody actually searches.
    unavailable: Option<String>,
    base: String,
    token: Option<String>,
}

/// Who this node is prepared to believe the model library is.
///
/// The authorities are carried in this binary rather than read from the machine,
/// which is a deliberate choice and not a convenience. Two reasons, one of them
/// found the hard way.
///
/// A node is often a box in a rack with no CA certificates installed, and the
/// default is to consult the machine's own trust store: that used to be fatal at
/// boot, reported as "the model library is unreachable", naming the network when
/// the cause was a missing package.
///
/// And on Windows, consulting that store is a BLOCKING call into the operating
/// system, made in the middle of an otherwise asynchronous handshake. A node
/// holding a model in memory could serve it perfectly well and yet never finish
/// a handshake to the library, failing at the connect deadline every time, while
/// the same request from the same machine took 0.4 seconds. Inbound handshakes,
/// which are checked against our own authority and never touch the operating
/// system, were unaffected throughout: that difference is what pointed here.
///
/// This talks to one known public API. A fixed, audited root set is the more
/// precise statement of that trust anyway, and it is the same on every machine.
/// The cryptography is named here rather than taken from the process, which is
/// the difference between a client and a landmine: the ambient version reads a
/// global that something else is expected to have set, and PANICS when it has
/// not. This is a constructor that promises it cannot fail, so it must not be
/// able to depend on somebody else's startup order.
fn library_trust() -> std::result::Result<rustls::ClientConfig, rustls::Error> {
    let roots = rustls::RootCertStore {
        roots: webpki_roots::TLS_SERVER_ROOTS.to_vec(),
    };
    Ok(
        rustls::ClientConfig::builder_with_provider(
            rustls::crypto::aws_lc_rs::default_provider().into(),
        )
        .with_safe_default_protocol_versions()?
        .with_root_certificates(roots)
        .with_no_client_auth(),
    )
}

impl Hub {
    pub fn new(base: impl Into<String>, token: Option<String>) -> Self {
        let built = library_trust()
            .map_err(|err| format!("the library's authorities could not be prepared: {err}"))
            .and_then(|tls| {
                reqwest::Client::builder()
                    // A library that has stopped answering must not hold a pull
                    // open forever. This is the wait for headers, not for the
                    // body: a slow forty gigabyte download is not a timeout.
                    .connect_timeout(std::time::Duration::from_secs(15))
                    .user_agent("flexie-sag-node")
                    .use_preconfigured_tls(tls)
                    .build()
                    .map_err(|err| err.to_string())
            });

        let (http, unavailable) = match built {
            Ok(client) => (Some(client), None),
            Err(err) => {
                // Loud, because it IS a fault worth fixing, and survivable,
                // because the models already here still answer.
                tracing::error!(
                    %err,
                    "this machine cannot reach the model library, so it can serve what it \
                     already has but cannot search for or download anything. The usual cause \
                     is missing CA certificates: install the ca-certificates package."
                );
                (None, Some(err.to_string()))
            }
        };

        Self {
            http,
            unavailable,
            base: base.into(),
            token,
        }
    }

    /// The client, or a refusal that says what is actually wrong.
    fn client(&self) -> Result<&reqwest::Client> {
        match (&self.http, &self.unavailable) {
            (Some(http), _) => Ok(http),
            (None, why) => Err(Error::invalid(format!(
                "this machine cannot reach the model library, so it cannot search or download: {}. \
                 The usual cause is missing CA certificates.",
                why.as_deref().unwrap_or("no client")
            ))),
        }
    }

    /// Whether this node holds credentials for the library.
    ///
    /// Asked before a gated model is fetched, so that the refusal happens while
    /// somebody is looking rather than as a 401 forty minutes into a download.
    pub fn has_token(&self) -> bool {
        self.token.is_some()
    }

    fn get(&self, url: &str) -> Result<reqwest::RequestBuilder> {
        let req = self.client()?.get(url);
        Ok(match &self.token {
            Some(token) => req.bearer_auth(token),
            None => req,
        })
    }

    /// Where a call to the library actually stalls, measured rather than argued.
    ///
    /// Run when one fails. The client reports "operation timed out" and nothing
    /// else: the stage is not in the error, so it has to be timed separately.
    /// Four numbers, each independent, so the next failure says which of them
    /// never finished instead of leaving somebody to reason from correlation.
    /// Several theories died to correlation before this existed.
    async fn probe(&self) {
        use tokio::net::TcpStream;
        let host = self
            .base
            .trim_start_matches("https://")
            .trim_start_matches("http://")
            .trim_end_matches('/')
            .to_string();

        let started = std::time::Instant::now();
        let addresses = match tokio::time::timeout(
            std::time::Duration::from_secs(10),
            tokio::net::lookup_host(format!("{host}:443")),
        )
        .await
        {
            Ok(Ok(found)) => found.collect::<Vec<_>>(),
            Ok(Err(err)) => {
                tracing::error!(%host, took = ?started.elapsed(), %err, "probe: the name did not resolve");
                return;
            }
            Err(_) => {
                tracing::error!(%host, took = ?started.elapsed(), "probe: resolving the name did not finish");
                return;
            }
        };
        tracing::info!(
            %host,
            took = ?started.elapsed(),
            addresses = ?addresses,
            "probe: resolved"
        );

        // Every address it offered, tried on its own. One family answering and
        // the other silently not is invisible when a client is left to choose.
        for address in addresses {
            let at = std::time::Instant::now();
            match tokio::time::timeout(
                std::time::Duration::from_secs(10),
                TcpStream::connect(address),
            )
            .await
            {
                Ok(Ok(_)) => {
                    tracing::info!(%address, took = ?at.elapsed(), "probe: connected")
                }
                Ok(Err(err)) => {
                    tracing::error!(%address, took = ?at.elapsed(), %err, "probe: refused")
                }
                Err(_) => {
                    tracing::error!(%address, took = ?at.elapsed(), "probe: no answer at all")
                }
            }
        }
    }

    /// Models matching a phrase, most downloaded first.
    pub async fn search(&self, query: &str, limit: u32) -> Result<Vec<HubModel>> {
        let query = query.trim();
        if query.is_empty() {
            return Err(Error::invalid("a search needs something to search for"));
        }
        let limit = limit.clamp(1, 100);
        // Ask for more than is wanted, because most of what comes back cannot
        // be run here and is dropped below. Four times, capped at what the API
        // will give in one page: enough that a filtered page is still a page,
        // and one request rather than paging.
        let asked = (limit * 4).min(100);

        // Built through the URL type rather than by formatting a string: the
        // phrase came from a person and a `&` or a `#` in it would otherwise
        // become part of the request instead of part of the search.
        let mut url =
            reqwest::Url::parse(&format!("{}/api/models", self.base)).map_err(Error::hub)?;
        url.query_pairs_mut()
            .append_pair("search", query)
            .append_pair("limit", &asked.to_string())
            .append_pair("sort", "downloads")
            .append_pair("direction", "-1");

        let sent = match self.get(url.as_str())?.send().await {
            Ok(response) => response,
            Err(err) => {
                // Measured at the moment it fails, in the process it failed in,
                // which is the only place the answer exists.
                self.probe().await;
                return Err(Error::hub(err));
            }
        };

        let hits: Vec<RawSearchHit> = sent
            .error_for_status()
            .map_err(Error::hub)?
            .json()
            .await
            .map_err(Error::hub)?;

        // Only what this machine could actually load.
        //
        // The library holds every kind of model there is, and this engine reads
        // two file formats: safetensors, or GGUF (see `pick_weights`). Searching
        // it unfiltered returns diffusion models, ONNX exports, datasets-shaped
        // repositories and adapters, all of which describe, verify and then fail
        // to load, and the person had no way to tell which was which before
        // spending the download.
        //
        // Filtered on the SAME predicate the downloader uses, rather than on a
        // list of architectures kept somewhere else: a repository is offered
        // here exactly when there is something in it we would fetch. The
        // library states both as tags, so this costs nothing beyond the request
        // already being made.
        Ok(hits
            .into_iter()
            .filter(RawSearchHit::is_runnable_here)
            .take(limit as usize)
            .map(RawSearchHit::into_model)
            .collect())
    }

    /// One repository, resolved to one commit, with exactly what would be
    /// fetched and what it weighs.
    ///
    /// `want` names a single weights file, for a repository that publishes the
    /// same model at several compression levels. Without it, a repository that
    /// offers a choice is REFUSED rather than guessed at: the difference between
    /// two of those files can be fifty gigabytes, and picking for somebody is
    /// picking with their disk.
    pub async fn describe(
        &self,
        repo: &str,
        revision: Option<&str>,
        want: Option<&str>,
    ) -> Result<HubRepo> {
        let repo = validate_repo(repo)?;
        let revision = validate_revision(revision.unwrap_or("main"))?;

        let info: RawRepoInfo = self
            .get(&format!(
                "{}/api/models/{repo}/revision/{revision}",
                self.base
            ))?
            .send()
            .await
            .map_err(Error::hub)?
            .error_for_status()
            .map_err(|err| match err.status() {
                Some(reqwest::StatusCode::NOT_FOUND) => Error::not_found(format!("model {repo}")),
                Some(reqwest::StatusCode::UNAUTHORIZED | reqwest::StatusCode::FORBIDDEN) => {
                    Error::invalid(format!(
                        "{repo} is not public: this node needs credentials for the model library"
                    ))
                }
                _ => Error::hub(err),
            })?
            .json()
            .await
            .map_err(Error::hub)?;

        // The commit, resolved once. Everything after this refers to bytes, not
        // to a branch that may move between now and the end of the download.
        let sha = info
            .sha
            .clone()
            .ok_or_else(|| Error::hub_answer(format!("{repo} did not say which commit it is")))?;

        let tree: Vec<RawTreeItem> = self
            .get(&format!(
                "{}/api/models/{repo}/tree/{sha}?recursive=true",
                self.base
            ))?
            .send()
            .await
            .map_err(Error::hub)?
            .error_for_status()
            .map_err(Error::hub)?
            .json()
            .await
            .map_err(Error::hub)?;

        let all: Vec<HubFile> = tree
            .into_iter()
            .filter(|item| item.kind.as_deref() != Some("directory"))
            .map(RawTreeItem::into_file)
            .collect();

        let choices = weight_files(&all);
        // Ambiguity is an ANSWER here, not a failure.
        //
        // A repository routinely publishes one model at twenty or thirty
        // compression levels, and which to take is not ours to decide. Refusing
        // the whole question made that a wall: describe returned an error, so
        // the caller never learned WHAT the levels were and had nothing to
        // choose from, and the person was told to choose one by a screen that
        // offered none.
        //
        // Answering with no files and every choice says the same thing and
        // leaves a way through it. Nothing can be downloaded from it, because
        // the pull refuses a description with nothing in it (pull.rs), so
        // approval still equals success.
        let files = files_or_ambiguity(&all, want, &choices)?;
        let size_bytes = files.iter().map(|f| f.size).sum();

        Ok(HubRepo {
            name: short_name(repo),
            repo: repo.to_string(),
            revision: sha,
            kind: ModelKind::infer(info.pipeline_tag.as_deref(), &info.tags),
            license: info.license(),
            gated: info.gated(),
            architecture: info.architecture(),
            files,
            size_bytes,
            choices,
        })
    }

    /// Stream one file to disk, reporting bytes as they land.
    ///
    /// `on_bytes` is called with each chunk's length, not with a total, so the
    /// caller owns the running count across however many files a model is. It
    /// is called on the download task and must not block.
    /// Stream one file to disk, reporting bytes as they land.
    ///
    /// `have` is how much of it is already on disk, from a download that was
    /// stopped. Anything above zero asks the library to continue from there
    /// rather than send the file again, which on a forty gigabyte model is the
    /// difference between resuming and starting over.
    pub async fn download_from(
        &self,
        fetch: Fetch<'_>,
        cancel: &tokio_util::sync::CancellationToken,
        mut on_bytes: impl FnMut(u64),
    ) -> Result<()> {
        use futures::StreamExt;

        let Fetch {
            repo,
            revision,
            file,
            into,
            have,
        } = fetch;
        let url = format!("{}/{repo}/resolve/{revision}/{file}", self.base);
        let mut request = self.get(&url)?;
        if have > 0 {
            request = request.header(reqwest::header::RANGE, format!("bytes={have}-"));
        }
        let response = request
            .send()
            .await
            .map_err(Error::hub)?
            .error_for_status()
            .map_err(Error::hub)?;

        // A server may ignore a Range and send the whole file. Appending that to
        // what we already have would produce a file of the right length made of
        // the wrong bytes, which verification would pass and a loader would not.
        // So what we keep is decided by what the server actually answered.
        let continuing = have > 0 && response.status() == reqwest::StatusCode::PARTIAL_CONTENT;
        if have > 0 && !continuing {
            tracing::info!(
                file,
                have,
                "the library would not continue this file, fetching it again"
            );
        }

        if let Some(parent) = into.parent() {
            tokio::fs::create_dir_all(parent).await?;
        }
        let mut sink = if continuing {
            use tokio::io::AsyncSeekExt;
            let mut file = tokio::fs::OpenOptions::new().write(true).open(into).await?;
            file.seek(std::io::SeekFrom::Start(have)).await?;
            file
        } else {
            tokio::fs::File::create(into).await?
        };
        let mut stream = response.bytes_stream();

        loop {
            tokio::select! {
                // Cancellation is checked in the same select as the next chunk,
                // so a pull called off during a forty gigabyte file stops within
                // one chunk rather than at the end of the file.
                _ = cancel.cancelled() => {
                    return Err(Error::conflict("the download was called off"));
                }
                chunk = stream.next() => {
                    let Some(chunk) = chunk else { break };
                    let chunk = chunk.map_err(Error::hub)?;
                    sink.write_all(&chunk).await?;
                    on_bytes(chunk.len() as u64);
                }
            }
        }

        // Flushed and closed before the caller is told this file is done, so
        // that a verification step reads what was actually written.
        sink.flush().await?;
        sink.sync_all().await?;
        Ok(())
    }
}

/// Reject a repository name that is not one.
///
/// The name never becomes a directory (KB: a minted id does), but it DOES become
/// part of a URL, so a name carrying a query string or a path escape would ask
/// the library a different question than the one shown to the administrator.
fn validate_repo(repo: &str) -> Result<&str> {
    let repo = repo.trim();
    let shape_ok = !repo.is_empty()
        && repo.len() <= 200
        && !repo.starts_with('/')
        && !repo.ends_with('/')
        && repo.matches('/').count() <= 1
        && !repo.contains("..")
        && repo
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.' | '/'));

    if shape_ok {
        Ok(repo)
    } else {
        Err(Error::invalid(format!("{repo:?} is not a model name")))
    }
}

/// Reject a revision that could reach past the repository it belongs to.
///
/// Same reason as the repository name: this becomes part of a URL path, so a
/// value carrying `..` or a query string would ask the library a different
/// question than the one an administrator approved. Slashes are allowed because
/// a branch may legitimately contain one.
fn validate_revision(revision: &str) -> Result<&str> {
    let revision = revision.trim();
    let shape_ok = !revision.is_empty()
        && revision.len() <= 120
        && !revision.starts_with('/')
        && !revision.contains("..")
        && revision
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.' | '/'));

    if shape_ok {
        Ok(revision)
    } else {
        Err(Error::invalid(format!("{revision:?} is not a version")))
    }
}

/// The part of `vendor/model` a person would call it.
fn short_name(repo: &str) -> String {
    repo.rsplit('/').next().unwrap_or(repo).to_string()
}

fn is_essential(path: &str) -> bool {
    ESSENTIAL_FILES.contains(&path)
}

fn has_extension(path: &str, ext: &str) -> bool {
    path.rsplit('.')
        .next()
        .is_some_and(|e| e.eq_ignore_ascii_case(ext))
}

/// Every file in a repository that is weights of some form.
fn weight_files(all: &[HubFile]) -> Vec<HubFile> {
    all.iter()
        .filter(|f| has_extension(&f.path, "safetensors") || has_extension(&f.path, "gguf"))
        .cloned()
        .collect()
}

/// Which files a pull actually fetches.
///
/// A repository routinely publishes the same weights three ways (safetensors,
/// the older pickle format, ONNX) and taking all of them costs three times the
/// disk for no gain. So:
///
/// - a named file is taken, and only it;
/// - otherwise safetensors, which is the format to prefer when both exist;
/// - otherwise a single compressed file, if there is exactly one;
/// - and a repository offering SEVERAL compressed files is refused, because
///   choosing between them is choosing how much disk somebody spends.
/// The files to fetch, or none of them because the answer is a question.
///
/// A repository routinely publishes one model at twenty or thirty compression
/// levels, and which to take is not ours to decide. Refusing the whole
/// description made that a wall: the caller got an error, never learned WHAT
/// the levels were, and was told to choose one by a screen with nothing on it
/// to choose from.
///
/// No files and every choice says the same thing and leaves a way through. It
/// is safe because a pull refuses a description with nothing in it, so nothing
/// can be downloaded from an unanswered question.
fn files_or_ambiguity(
    all: &[HubFile],
    want: Option<&str>,
    choices: &[HubFile],
) -> Result<Vec<HubFile>> {
    match select_files(all, want) {
        Ok(files) => Ok(files),
        // Only when nothing was named and there is genuinely more than one
        // thing to name. Every other refusal is still a refusal.
        Err(_) if want.is_none() && choices.len() > 1 => Ok(Vec::new()),
        Err(err) => Err(err),
    }
}

fn select_files(all: &[HubFile], want: Option<&str>) -> Result<Vec<HubFile>> {
    let essentials: Vec<HubFile> = all
        .iter()
        .filter(|f| is_essential(&f.path))
        .cloned()
        .collect();

    let take = |weights: Vec<HubFile>| -> Vec<HubFile> {
        let mut files = weights;
        files.extend(essentials.iter().cloned());
        files.sort_by(|a, b| a.path.cmp(&b.path));
        files.dedup_by(|a, b| a.path == b.path);
        files
    };

    if let Some(want) = want {
        let chosen = all
            .iter()
            .find(|f| f.path == want)
            .cloned()
            .ok_or_else(|| Error::invalid(format!("this model has no file called {want:?}")))?;
        return Ok(take(vec![chosen]));
    }

    let safetensors: Vec<HubFile> = all
        .iter()
        .filter(|f| has_extension(&f.path, "safetensors"))
        .cloned()
        .collect();
    if !safetensors.is_empty() {
        // The index names the shards and a loader needs it.
        let mut weights = safetensors;
        weights.extend(
            all.iter()
                .filter(|f| f.path.ends_with("safetensors.index.json"))
                .cloned(),
        );
        return Ok(take(weights));
    }

    let compressed: Vec<HubFile> = all
        .iter()
        .filter(|f| has_extension(&f.path, "gguf"))
        .cloned()
        .collect();
    match compressed.len() {
        0 => Err(Error::invalid(
            "this model publishes no weights in a format this node can read",
        )),
        1 => Ok(take(compressed)),
        _ => Err(Error::invalid(format!(
            "this model is published at {} compression levels: choose one",
            compressed.len()
        ))),
    }
}

#[derive(Deserialize)]
struct RawSearchHit {
    id: String,
    #[serde(default)]
    downloads: u64,
    #[serde(default)]
    likes: u64,
    #[serde(default)]
    tags: Vec<String>,
    #[serde(default)]
    pipeline_tag: Option<String>,
}

impl RawSearchHit {
    /// Whether this engine reads anything in this repository.
    ///
    /// The tags the library publishes for the two formats `pick_weights` knows
    /// how to choose between. A repository with neither holds nothing we could
    /// load, whatever else it is.
    fn is_runnable_here(&self) -> bool {
        self.tags
            .iter()
            .any(|t| t.eq_ignore_ascii_case("safetensors") || t.eq_ignore_ascii_case("gguf"))
    }

    fn into_model(self) -> HubModel {
        HubModel {
            name: short_name(&self.id),
            kind: ModelKind::infer(self.pipeline_tag.as_deref(), &self.tags),
            repo: self.id,
            downloads: self.downloads,
            likes: self.likes,
        }
    }
}

#[derive(Deserialize)]
struct RawRepoInfo {
    #[serde(default)]
    sha: Option<String>,
    #[serde(default)]
    tags: Vec<String>,
    #[serde(default)]
    pipeline_tag: Option<String>,
    #[serde(default)]
    card_data: Option<RawCardData>,
    #[serde(default, rename = "cardData")]
    card_data_camel: Option<RawCardData>,
    #[serde(default)]
    gated: serde_json::Value,
    /// What the repository's own `config.json` says, as much of it as the
    /// library repeats. It carries the architecture, which is why nothing has
    /// to be downloaded to find out whether it could run.
    #[serde(default)]
    config: Option<RawConfig>,
}

impl RawRepoInfo {
    /// The class the weights declare, preferring the explicit list.
    ///
    /// `model_type` is the fallback and is a weaker statement: it names a
    /// family where `architectures` names the exact head, and a family can
    /// contain both something an engine loads and something it does not. It is
    /// still better than nothing for the older repositories that only carry it.
    fn architecture(&self) -> Option<String> {
        let config = self.config.as_ref()?;
        config
            .architectures
            .first()
            .cloned()
            .or_else(|| config.model_type.clone())
    }

    /// The library answers `false`, or a string naming the kind of gate.
    ///
    /// A repository that said nothing at all reads as OPEN. Being cautious the
    /// other way would refuse pulls that would have worked, and the download
    /// itself refuses a gated model honestly, so the cost of being wrong here is
    /// a clear failure rather than a wrong answer.
    fn gated(&self) -> bool {
        match &self.gated {
            serde_json::Value::Bool(gated) => *gated,
            serde_json::Value::String(_) => true,
            _ => false,
        }
    }

    fn license(&self) -> Option<String> {
        self.card_data
            .as_ref()
            .or(self.card_data_camel.as_ref())
            .and_then(|c| c.license.clone())
    }
}

#[derive(Deserialize)]
struct RawCardData {
    #[serde(default)]
    license: Option<String>,
}

#[derive(Deserialize)]
struct RawConfig {
    #[serde(default)]
    architectures: Vec<String>,
    #[serde(default)]
    model_type: Option<String>,
}

#[derive(Deserialize)]
struct RawTreeItem {
    path: String,
    #[serde(default)]
    size: u64,
    #[serde(default, rename = "type")]
    kind: Option<String>,
    /// Large files report their real size here and a pointer size above.
    #[serde(default)]
    lfs: Option<RawLfs>,
}

#[derive(Deserialize)]
struct RawLfs {
    #[serde(default)]
    size: u64,
}

impl RawTreeItem {
    fn into_file(self) -> HubFile {
        // The size at the top level of a large file is the size of the POINTER,
        // a few hundred bytes, not the weights. Reading that one would tell an
        // administrator a seventy gigabyte model is a kilobyte, and would make
        // the progress bar finish instantly and then keep going.
        let size = match &self.lfs {
            Some(lfs) if lfs.size > 0 => lfs.size,
            _ => self.size,
        };
        HubFile {
            path: self.path,
            size,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn file(path: &str, size: u64) -> HubFile {
        HubFile {
            path: path.into(),
            size,
        }
    }

    #[test]
    fn a_repository_name_that_is_not_one_is_refused() {
        for bad in [
            "",
            "   ",
            "/leading",
            "trailing/",
            "a/b/c",
            "vendor/../../etc",
            "vendor/model?x=1",
            "vendor/model#frag",
            "vendor/mo del",
        ] {
            assert!(
                validate_repo(bad).is_err(),
                "{bad:?} was accepted as a model name"
            );
        }
    }

    #[test]
    fn a_version_that_could_reach_past_the_repository_is_refused() {
        for bad in [
            "",
            "  ",
            "/main",
            "refs/../../other",
            "main?x=1",
            "main#frag",
        ] {
            assert!(
                validate_revision(bad).is_err(),
                "{bad:?} was accepted as a version"
            );
        }
    }

    #[test]
    fn an_ordinary_version_is_accepted() {
        for good in [
            "main",
            "refs/pr/12",
            "v1.2.3",
            "54957525b8b6c0f4a1e2d3c4b5a69788990a1b2c",
        ] {
            assert!(validate_revision(good).is_ok(), "{good:?} was refused");
        }
    }

    #[test]
    fn an_ordinary_repository_name_is_accepted() {
        for good in [
            "vendor/model",
            "model",
            "Vendor/Model-3.2-1B_v2",
            " vendor/model ",
        ] {
            assert!(validate_repo(good).is_ok(), "{good:?} was refused");
        }
        assert_eq!(validate_repo(" vendor/model ").unwrap(), "vendor/model");
    }

    #[test]
    fn safetensors_win_over_the_older_format() {
        let all = vec![
            file("model.safetensors", 100),
            file("pytorch_model.bin", 100),
            file("config.json", 1),
            file("tokenizer.json", 2),
        ];
        let chosen = select_files(&all, None).unwrap();
        let paths: Vec<_> = chosen.iter().map(|f| f.path.as_str()).collect();
        assert_eq!(
            paths,
            ["config.json", "model.safetensors", "tokenizer.json"]
        );
    }

    #[test]
    fn every_shard_and_its_index_are_taken() {
        let all = vec![
            file("model-00001-of-00002.safetensors", 50),
            file("model-00002-of-00002.safetensors", 50),
            file("model.safetensors.index.json", 1),
            file("config.json", 1),
        ];
        let chosen = select_files(&all, None).unwrap();
        assert_eq!(chosen.len(), 4, "a shard or the index was dropped");
        assert_eq!(chosen.iter().map(|f| f.size).sum::<u64>(), 102);
    }

    #[test]
    fn one_compressed_file_is_taken_without_asking() {
        let all = vec![file("model-Q4_K_M.gguf", 4_000), file("config.json", 1)];
        let chosen = select_files(&all, None).unwrap();
        assert_eq!(chosen.len(), 2);
    }

    #[test]
    fn several_compressed_files_are_refused_rather_than_guessed() {
        // The whole reason this rule exists: these two differ by tens of
        // gigabytes and only the administrator knows which they meant.
        let all = vec![
            file("model-Q4_K_M.gguf", 4_000),
            file("model-Q8_0.gguf", 8_000),
            file("config.json", 1),
        ];
        let err = select_files(&all, None).expect_err("a choice was made for somebody");
        assert!(err.to_string().contains("choose one"), "{err}");
    }

    #[test]
    fn naming_the_file_resolves_the_choice_and_takes_only_that_one() {
        let all = vec![
            file("model-Q4_K_M.gguf", 4_000),
            file("model-Q8_0.gguf", 8_000),
            file("config.json", 1),
        ];
        let chosen = select_files(&all, Some("model-Q8_0.gguf")).unwrap();
        let paths: Vec<_> = chosen.iter().map(|f| f.path.as_str()).collect();
        assert_eq!(paths, ["config.json", "model-Q8_0.gguf"]);
        assert_eq!(chosen.iter().map(|f| f.size).sum::<u64>(), 8_001);
    }

    #[test]
    fn naming_a_file_that_is_not_there_is_refused() {
        let all = vec![file("model-Q4_K_M.gguf", 4_000)];
        assert!(select_files(&all, Some("model-Q2_K.gguf")).is_err());
    }

    #[test]
    fn a_repository_with_no_readable_weights_is_refused() {
        let all = vec![file("README.md", 1), file("model.onnx", 900)];
        let err = select_files(&all, None).expect_err("a model with no weights was accepted");
        assert!(err.to_string().contains("no weights"), "{err}");
    }

    #[test]
    fn nothing_is_downloaded_twice_when_a_file_is_both_weights_and_essential() {
        let all = vec![file("model.safetensors", 10), file("tokenizer.model", 3)];
        let chosen = select_files(&all, Some("tokenizer.model")).unwrap();
        assert_eq!(chosen.len(), 1, "the same file was selected twice");
    }

    #[test]
    fn a_large_files_real_size_is_read_and_not_its_pointer() {
        // The bug this guards: the tree reports a few hundred bytes for a
        // seventy gigabyte file, and the pointer size is the one at the top.
        let raw: RawTreeItem = serde_json::from_str(
            r#"{"path":"model.safetensors","size":135,"type":"file","lfs":{"size":70000000000}}"#,
        )
        .unwrap();
        assert_eq!(raw.into_file().size, 70_000_000_000);
    }

    #[test]
    fn an_ordinary_files_size_is_read_from_the_top() {
        let raw: RawTreeItem =
            serde_json::from_str(r#"{"path":"config.json","size":812,"type":"file"}"#).unwrap();
        assert_eq!(raw.into_file().size, 812);
    }

    #[test]
    fn a_gate_is_read_whatever_shape_the_library_answers_in() {
        let open: RawRepoInfo = serde_json::from_str(r#"{"sha":"a","gated":false}"#).unwrap();
        assert!(!open.gated());

        // A real gated repository answers with the KIND of gate, not `true`.
        let named: RawRepoInfo = serde_json::from_str(r#"{"sha":"a","gated":"manual"}"#).unwrap();
        assert!(named.gated(), "a named gate read as open");

        let plain: RawRepoInfo = serde_json::from_str(r#"{"sha":"a","gated":true}"#).unwrap();
        assert!(plain.gated());

        // The case that mattered in practice: the search endpoint omits the
        // field entirely, and reading that as a gate marked every model on the
        // screen with a padlock.
        let absent: RawRepoInfo = serde_json::from_str(r#"{"sha":"a"}"#).unwrap();
        assert!(
            !absent.gated(),
            "a repository that said nothing read as gated"
        );
    }

    #[test]
    fn a_node_knows_whether_it_holds_credentials_for_the_library() {
        let without = Hub::new("https://example.test", None);
        assert!(!without.has_token());
        let with = Hub::new("https://example.test", Some("token".into()));
        assert!(with.has_token());
    }

    #[test]
    fn the_licence_is_read_under_either_spelling() {
        let camel: RawRepoInfo =
            serde_json::from_str(r#"{"sha":"abc","cardData":{"license":"apache-2.0"}}"#).unwrap();
        assert_eq!(camel.license().as_deref(), Some("apache-2.0"));

        let snake: RawRepoInfo =
            serde_json::from_str(r#"{"sha":"abc","card_data":{"license":"mit"}}"#).unwrap();
        assert_eq!(snake.license().as_deref(), Some("mit"));
    }

    #[test]
    fn weight_files_are_listed_so_a_refusal_can_say_what_to_choose_from() {
        let all = vec![
            file("model-Q4_K_M.gguf", 4_000),
            file("model-Q8_0.gguf", 8_000),
            file("config.json", 1),
            file("README.md", 1),
        ];
        let choices = weight_files(&all);
        assert_eq!(choices.len(), 2);
        assert!(choices.iter().all(|f| f.path.ends_with(".gguf")));
    }

    #[test]
    fn the_short_name_is_what_a_person_would_call_it() {
        assert_eq!(short_name("vendor/Model-3B"), "Model-3B");
        assert_eq!(short_name("model"), "model");
    }

    #[test]
    fn a_library_that_cannot_be_reached_does_not_stop_this_machine() {
        // The property a GPU box on a closed network depends on. Building the
        // library client used to be fatal at boot, and the way it failed was the
        // worst one available: a machine with no CA certificates could not build
        // an HTTPS client, so it exited 1 with "the model library is
        // unreachable", naming the network when the cause was a missing package.
        //
        // Constructing a Hub cannot fail any more. It is a value, not a Result.
        let hub = Hub::new("https://example.test", None);
        assert!(!hub.has_token());

        // And when the client really is absent, asking it something says what is
        // wrong rather than what is not.
        let broken = Hub {
            http: None,
            unavailable: Some("builder error".into()),
            base: "https://example.test".into(),
            token: None,
        };
        let err = broken
            .get("https://example.test/x")
            .unwrap_err()
            .to_string();
        assert!(
            err.contains("cannot search or download"),
            "unhelpful: {err}"
        );
        assert!(
            err.contains("CA certificates"),
            "does not name the usual cause: {err}"
        );
    }

    #[tokio::test]
    async fn a_call_that_fails_says_which_stage_of_it_failed() {
        // The end to end of the diagnostic: a REAL client failure, through the
        // real Hub, to the sentence somebody reads. The client's own Display is
        // "error sending request for url (...)", which is the same words for a
        // name that will not resolve, a connection nothing accepts and a
        // certificate that will not verify. Reporting that alone is what turned
        // one unreachable node into a day of arguing between three causes, and
        // this is the test that would have ended it in a minute.
        //
        // A port bound and released, so the address is real and routable with
        // nothing listening on it: the failure is a refusal, the one connect
        // outcome that needs neither a network nor a wait.
        let port = std::net::TcpListener::bind("127.0.0.1:0")
            .expect("a loopback port")
            .local_addr()
            .expect("its address")
            .port();

        let hub = Hub::new(format!("http://127.0.0.1:{port}"), None);
        let err = hub
            .search("llama", 5)
            .await
            .expect_err("nothing is listening, so this cannot succeed")
            .to_string();

        assert!(
            err.contains("tcp connect error"),
            "the stage that failed is missing: {err}"
        );
        assert!(
            err.contains("refused"),
            "what the stage did is missing: {err}"
        );
    }
}

#[cfg(test)]
mod search_filter_tests {
    use super::*;

    fn hit(tags: &[&str]) -> RawSearchHit {
        RawSearchHit {
            id: "org/model".into(),
            downloads: 0,
            likes: 0,
            tags: tags.iter().map(|t| t.to_string()).collect(),
            pipeline_tag: None,
        }
    }

    /// A search offers what this engine could load, and nothing else.
    ///
    /// The library holds every kind of model there is. Unfiltered, a search
    /// returns diffusion models, ONNX exports and adapters beside the weights,
    /// and the only way to tell them apart was to choose one, wait for the
    /// download, and watch it fail to load.
    #[test]
    fn only_what_this_engine_reads() {
        assert!(hit(&["safetensors", "text-generation"]).is_runnable_here());
        assert!(hit(&["gguf"]).is_runnable_here());
        // Case is the library's business, not ours.
        assert!(hit(&["SafeTensors"]).is_runnable_here());
    }

    #[test]
    fn nothing_else() {
        assert!(!hit(&["onnx", "text-generation"]).is_runnable_here());
        assert!(!hit(&["diffusers", "text-to-image"]).is_runnable_here());
        assert!(!hit(&["peft", "adapter"]).is_runnable_here());
        assert!(!hit(&[]).is_runnable_here());
    }
}

#[cfg(test)]
mod ambiguity_tests {
    use super::*;

    fn f(path: &str) -> HubFile {
        HubFile { path: path.into(), size: 100 }
    }

    /// Thirty compression levels is an ANSWER, not an error: no files, and the
    /// choices to pick from. It used to be an error, so the console showed
    /// "choose one" above nothing to choose.
    #[test]
    fn several_levels_and_none_named_answers_with_nothing_to_fetch() {
        let all = vec![f("m-Q4.gguf"), f("m-Q5.gguf"), f("m-Q8.gguf"), f("config.json")];
        let choices = weight_files(&all);
        assert_eq!(choices.len(), 3);
        let files = files_or_ambiguity(&all, None, &choices).expect("an answer, not an error");
        assert!(files.is_empty(), "nothing to fetch until one is named");
    }

    /// Name one and it is a plan again.
    #[test]
    fn naming_one_makes_it_a_plan() {
        let all = vec![f("m-Q4.gguf"), f("m-Q5.gguf"), f("config.json")];
        let choices = weight_files(&all);
        let files = files_or_ambiguity(&all, Some("m-Q4.gguf"), &choices).expect("a plan");
        assert!(files.iter().any(|x| x.path == "m-Q4.gguf"));
        assert!(!files.iter().any(|x| x.path == "m-Q5.gguf"));
    }

    /// One level needs no choosing, and safetensors win outright.
    #[test]
    fn one_way_to_read_it_is_never_ambiguous() {
        for all in [
            vec![f("m.gguf"), f("config.json")],
            vec![f("m.safetensors"), f("m-Q4.gguf"), f("m-Q5.gguf")],
        ] {
            let choices = weight_files(&all);
            let files = files_or_ambiguity(&all, None, &choices).expect("a plan");
            assert!(!files.is_empty());
        }
    }

    /// Every other refusal is still a refusal: a repository with nothing this
    /// engine can read is not a question, it is a no.
    #[test]
    fn nothing_readable_is_still_refused() {
        let all = vec![f("model.onnx"), f("config.json")];
        let choices = weight_files(&all);
        assert!(files_or_ambiguity(&all, None, &choices).is_err());
    }
}
