//! How this machine adds itself.
//!
//! A machine is told where the gateway is and given a shared token, and it
//! registers itself: the row appears, with its address and its key, and nobody
//! typed anything. The same shape a node uses to join a swarm.
//!
//! **This is the only call this node ever makes to the gateway.** Everything
//! else, the control surface and inference alike, is the gateway calling here.
//! That is worth keeping true: it means a machine holding weights needs no
//! standing route out and no second credential, and the one exception is a
//! single request at boot.
//!
//! Two secrets doing different jobs, and the difference is the point:
//!
//! - the **join token** is an invitation. It belongs to the person who asked for
//!   it, stands for an hour, and the gateway destroys it the moment a machine
//!   uses it, so it admits exactly one machine, once.
//! - the **node key** is minted HERE, is unique to this machine, never leaves
//!   its disk except in that one request, and is what every later call is
//!   authenticated with, INCLUDING this machine's own daily check-in.
//!
//! That second half is what lets the first be true. A machine checks back in
//! every day for a fresh certificate, and if it presented its invitation each
//! time, the invitation could never be spent and never expire: it would be a
//! permanent key to the fleet sitting in a file on every box. It signs as itself
//! instead, so a decommissioned machine loses its own key and no other machine
//! is affected.
//!
//! # Neither side assumes the other
//!
//! This one request is the only moment where two machines that have never met
//! have to establish that they are talking to each other, so it is worth being
//! precise about what proves what.
//!
//! The token is `<authority-fingerprint>_<secret>` and **the secret never leaves
//! this machine**. We sign the request body with it and send the
//! signature; the gateway recomputes it from the same bytes. So somebody in the
//! path learns nothing reusable by watching, and cannot alter our address, our
//! key or our certificate request without the signature failing.
//!
//! Coming back, the gateway sends its authority, and we hash it and compare with
//! the fingerprint we were expecting BEFORE we keep a byte of it. That is the
//! half that stops us enrolling into somebody else's authority: without it, the
//! first thing to answer would become the thing we trust for the life of the
//! machine. What we compare against is the fingerprint in the token the first
//! time, and the authority we already hold every time after, which is the
//! stronger of the two and does not need the token to still exist.

use std::{path::Path, sync::Arc};

use serde::{Deserialize, Serialize};
use tokio::sync::Mutex;

use crate::{
    config::Config,
    error::{Error, Result},
    tls::Credentials,
};

/// The file this machine's identity lives in, inside the directory it owns.
const IDENTITY_FILE: &str = "identity.json";

/// How long to wait between attempts when the gateway is not answering.
///
/// A machine that boots before the gateway does, or during a deploy, must not
/// give up and sit there unregistered: it is the gateway that will be asked
/// where its models are, and until this succeeds the answer is "what machine?".
const RETRY: std::time::Duration = std::time::Duration::from_secs(10);

/// What this machine remembers about itself between restarts.
///
/// Minted once, on first boot, and never again. The id is what makes a machine
/// that comes back the SAME machine to the gateway: without it, a container
/// rescheduled onto a different address would leave a dead row behind and add a
/// duplicate beside it.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Identity {
    pub node_id: String,
    pub key: String,
}

impl Identity {
    /// Read this machine's identity, minting one the first time.
    ///
    /// Written the same way a catalogue entry is: to a temporary file beside the
    /// target and renamed over it, so a process killed mid-write leaves the old
    /// identity or the new one and never half of either. Half an identity is a
    /// machine that cannot prove who it is.
    pub async fn load_or_mint(dir: &Path) -> Result<Self> {
        let path = dir.join(IDENTITY_FILE);
        match tokio::fs::read(&path).await {
            Ok(raw) => {
                let identity: Identity = serde_json::from_slice(&raw)?;
                if identity.node_id.is_empty() || identity.key.is_empty() {
                    return Err(Error::invalid(
                        "this machine's identity file is incomplete: remove it to mint a new one",
                    ));
                }
                Ok(identity)
            }
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
                let identity = Identity {
                    node_id: format!("nd_{}", uuid::Uuid::new_v4().simple()),
                    key: mint_key(),
                };
                let temp = path.with_extension("writing");
                tokio::fs::write(&temp, serde_json::to_vec_pretty(&identity)?).await?;
                tokio::fs::rename(&temp, &path).await?;
                tracing::info!(node = %identity.node_id, "this machine minted its identity");
                Ok(identity)
            }
            Err(err) => Err(err.into()),
        }
    }
}

fn mint_key() -> String {
    // A uuid is 16 bytes of randomness and needs no dependency of its own; two
    // of them is 32, which is past any reasonable floor and stays URL safe.
    format!(
        "{}{}",
        uuid::Uuid::new_v4().simple(),
        uuid::Uuid::new_v4().simple()
    )
}

/// What this machine tells the gateway about itself.
///
/// Note what is NOT here: the token. We prove we hold one by signing these
/// bytes with it, so the shared secret never crosses the network even once.
#[derive(Serialize)]
struct JoinRequest<'a> {
    node_id: &'a str,
    name: &'a str,
    /// Where the gateway should reach us. Blank means "use the address you saw
    /// this request come from", which is right far more often than it is wrong:
    /// a machine behind NAT or in a container does not know the address that
    /// reaches it from outside.
    advertise: &'a str,
    /// What we listen on. The gateway sees which host this request came from and
    /// cannot see which port we serve on, so it needs this to have an address to
    /// write down. It is why `advertise` is only for the cases it is really for.
    port: u16,
    key: &'a str,
    version: &'a str,
    /// Asking to be given an identity, over a key that never leaves this disk.
    certificate_request: &'a str,
    /// When we signed this, so that a captured request cannot be replayed later
    /// to point our row at somebody else's address.
    issued_at: String,
}

#[derive(Deserialize, Debug)]
struct JoinResponse {
    #[serde(default)]
    name: String,
    #[serde(default)]
    base_url: String,
    #[serde(default)]
    joined: bool,
    /// The certificate both ends chain to. Checked against the authority we
    /// were expecting before it is believed.
    #[serde(default)]
    authority: String,
    /// Ours, signed by that authority.
    #[serde(default)]
    certificate: String,
}

/// The header the signature travels in.
///
/// A header rather than a field, because what is signed is the body exactly as
/// it is sent: a signature inside the thing it signs is a thing to be removed
/// before checking, which is a canonicalisation rule and the usual place these
/// go wrong.
const SIGNATURE_HEADER: &str = "X-Sag-Join-Signature";

/// Read the authority fingerprint out of a join token.
///
/// The token is `<fingerprint>_<secret>`. The fingerprint is hex and the secret
/// may contain underscores, so the FIRST underscore is the boundary and there is
/// nothing ambiguous about it.
pub fn fingerprint_in(token: &str) -> Option<&str> {
    let (fingerprint, secret) = token.split_once('_')?;
    if fingerprint.is_empty() || secret.is_empty() {
        return None;
    }
    Some(fingerprint)
}

/// What proves a check-in, and what its answer is checked against.
///
/// A machine registers once and checks back in every day for the rest of its
/// life, and those two are not the same act. Only the first is an admission to
/// the fleet; the rest are a machine the gateway already has, saying where it is
/// and asking for a fresh certificate.
///
/// Giving them the same credential is what made the join token a permanent
/// shared key: it could not be spent, because tomorrow's check-in needed it
/// again, and it could not expire, for the same reason. Separating them is what
/// lets an invitation be used exactly once and thrown away.
enum Proof {
    /// A machine the gateway has never seen, presenting what it was installed
    /// with. The gateway destroys it the moment it works, so this happens once
    /// in a machine's life and the copy left in its configuration is dead.
    Invitation(String),
    /// A machine the gateway already has, signing with its own key: the same
    /// secret the gateway uses to call it, minted on this disk and never
    /// changed. Costs no invitation, and needs nothing an administrator has to
    /// keep secret afterwards.
    Own {
        key: String,
        /// The authority this machine already holds. A better answer than the
        /// fingerprint in a token, and one that does not depend on the token
        /// still being around: it is the certificate we have been serving with.
        authority_pem: String,
    },
}

impl Proof {
    /// The secret the request is signed with.
    fn secret(&self) -> &str {
        match self {
            Proof::Invitation(token) => token,
            Proof::Own { key, .. } => key,
        }
    }

    /// Who the answer has to come from. Established BEFORE anything is sent,
    /// because a machine that works this out from the reply is a machine that
    /// trusts whatever replied.
    fn expected_authority(&self) -> Result<String> {
        match self {
            Proof::Invitation(token) => {
                fingerprint_in(token).map(str::to_string).ok_or_else(|| {
                    Error::invalid("the join token is not one of ours: it names no authority")
                })
            }
            Proof::Own { authority_pem, .. } => crate::tls::fingerprint(authority_pem),
        }
    }

    fn describe(&self) -> &'static str {
        match self {
            Proof::Invitation(_) => "with the token it was installed with",
            Proof::Own { .. } => "as itself",
        }
    }
}

/// Prove we hold the token, without sending it.
fn signature(token: &str, body: &[u8]) -> String {
    use hmac::Mac as _;
    let mut mac = <hmac::Hmac<sha2::Sha256> as hmac::Mac>::new_from_slice(token.as_bytes())
        .expect("hmac takes a key of any length");
    mac.update(body);
    use std::fmt::Write as _;
    mac.finalize()
        .into_bytes()
        .iter()
        .fold(String::new(), |mut out, b| {
            let _ = write!(out, "{b:02x}");
            out
        })
}

/// What a successful enrolment produces: an identity to serve with.
///
/// Handed back rather than applied here, because who installs it differs: the
/// first one decides whether this machine can listen at all, and a later one
/// replaces a certificate under a listener that is already running.
pub type Enrolled = crate::tls::Signed;

/// Register with the gateway, retrying until it works.
///
/// Returns what the gateway issued. It retries for as long as it takes: a
/// machine that boots before the gateway does, or during a deploy, must not give
/// up and sit there unregistered, because until this succeeds the answer to
/// "where are my models" is "what machine?".
pub async fn keep_joining(
    config: Config,
    identity: Identity,
    credentials: Arc<Mutex<Credentials>>,
) -> Option<Enrolled> {
    let gateway = config.gateway_url.clone()?;
    if config.join_token.is_none() && credentials.lock().await.signed().is_none() {
        tracing::error!(
            "this machine has never registered and has no token to register with, so it cannot be \
             given a certificate"
        );
        return None;
    }

    let http = reqwest::Client::builder()
        .connect_timeout(std::time::Duration::from_secs(10))
        .timeout(std::time::Duration::from_secs(30))
        .user_agent("flexie-sag-node")
        .build();
    let Ok(http) = http else {
        tracing::error!("this machine cannot make requests and so cannot register");
        return None;
    };

    let mut told = false;
    loop {
        let mut refusal = None;
        for proof in proofs(&config, &identity, &credentials).await {
            match join_once(&http, &gateway, &proof, &config, &identity, &credentials).await {
                Ok((answer, signed)) => {
                    tracing::info!(
                        name = %answer.name,
                        reachable_at = %answer.base_url,
                        joined = answer.joined,
                        proof = proof.describe(),
                        "registered with the gateway"
                    );
                    return Some(signed);
                }
                Err(err) => refusal = Some(err),
            }
        }

        // Logged once at error and then quietly, so a gateway that is down for
        // an hour does not fill the log with the same line six times a minute
        // while still being visible when it starts.
        if let Some(err) = refusal {
            if !told {
                told = true;
                tracing::error!(%err, "could not register with the gateway, still trying");
            } else {
                tracing::debug!(%err, "still cannot register");
            }
        }
        tokio::time::sleep(RETRY).await;
    }
}

/// What this machine can prove it is, best first.
///
/// Usually one thing. Two only in the case that needs it: a machine holding a
/// certificate from a deployment that no longer knows it, whose own key means
/// nothing here, and which must be able to fall back on the invitation it was
/// given. Trying the key first costs one refused request in that case and saves
/// an invitation in every other.
async fn proofs(
    config: &Config,
    identity: &Identity,
    credentials: &Arc<Mutex<Credentials>>,
) -> Vec<Proof> {
    let mut ways = Vec::new();
    if let Some(held) = credentials.lock().await.signed() {
        ways.push(Proof::Own {
            key: identity.key.clone(),
            authority_pem: held.authority_pem,
        });
    }
    if let Some(token) = config.join_token.clone() {
        ways.push(Proof::Invitation(token));
    }
    ways
}

async fn join_once(
    http: &reqwest::Client,
    gateway: &str,
    proof: &Proof,
    config: &Config,
    identity: &Identity,
    credentials: &Arc<Mutex<Credentials>>,
) -> Result<(JoinResponse, Enrolled)> {
    // Checked before anything is sent. A credential we cannot work out the
    // expected authority from is one that could never verify the answer, and
    // registering on it would mean trusting whatever replied.
    let expected = proof.expected_authority()?;

    let certificate_request = {
        let held = credentials.lock().await;
        held.certificate_request(&identity.node_id)?
    };

    // Serialised ONCE, and those exact bytes are both signed and sent. Building
    // the body twice would be signing something the gateway never receives.
    let body = serde_json::to_vec(&JoinRequest {
        node_id: &identity.node_id,
        name: &config.name,
        advertise: config.advertise.as_deref().unwrap_or_default(),
        port: config.addr.port(),
        key: &identity.key,
        version: env!("CARGO_PKG_VERSION"),
        certificate_request: &certificate_request,
        issued_at: chrono::Utc::now().to_rfc3339(),
    })?;

    let response = http
        .post(format!("{gateway}/v1/nodes/join"))
        .header("content-type", "application/json")
        .header(SIGNATURE_HEADER, signature(proof.secret(), &body))
        .body(body)
        .send()
        .await
        .map_err(|err| Error::invalid(format!("the gateway did not answer: {err}")))?;

    let status = response.status();
    let text = response.text().await.unwrap_or_default();
    if !status.is_success() {
        // A refused token is not going to become accepted by trying again, but
        // it is still worth retrying rather than exiting: an administrator
        // fixing the token should not also have to remember to restart every
        // machine that was started with the wrong one.
        return Err(Error::invalid(format!(
            "the gateway refused this machine ({status}): {}",
            text.trim()
        )));
    }
    let answer: JoinResponse = serde_json::from_str(&text).map_err(|err| {
        Error::invalid(format!("the gateway answered something unexpected: {err}"))
    })?;

    // Who did we just talk to? The one question that cannot be deferred: after
    // this we serve using what it gave us and trust callers it vouches for.
    let offered = crate::tls::fingerprint(&answer.authority)?;
    if offered != expected {
        // Deliberately loud and deliberately fatal to this attempt. Either
        // somebody is impersonating the gateway, or this machine was given a
        // token from a different deployment. Both are worth a person looking at.
        return Err(Error::invalid(format!(
            "the gateway that answered is not the one this machine was told about \
             (it presented {offered}, we expected {expected})"
        )));
    }
    if answer.certificate.trim().is_empty() {
        return Err(Error::invalid(
            "the gateway registered this machine but gave it no certificate to serve with",
        ));
    }

    let signed = Enrolled {
        cert_pem: answer.certificate.clone(),
        authority_pem: answer.authority.clone(),
    };
    credentials
        .lock()
        .await
        .store(&config.data_dir, signed.clone())
        .await?;
    Ok((answer, signed))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn an_identity_is_minted_once_and_then_kept() {
        let temp = tempfile::tempdir().unwrap();
        let first = Identity::load_or_mint(temp.path()).await.unwrap();
        let second = Identity::load_or_mint(temp.path()).await.unwrap();

        // The whole reason it is written down: a machine that comes back is the
        // same machine, so it updates its own row instead of adding one.
        assert_eq!(first.node_id, second.node_id);
        assert_eq!(first.key, second.key);
    }

    #[tokio::test]
    async fn two_machines_are_two_identities() {
        let a = tempfile::tempdir().unwrap();
        let b = tempfile::tempdir().unwrap();
        let one = Identity::load_or_mint(a.path()).await.unwrap();
        let two = Identity::load_or_mint(b.path()).await.unwrap();
        assert_ne!(one.node_id, two.node_id);
        assert_ne!(one.key, two.key);
    }

    #[tokio::test]
    async fn a_minted_key_is_past_the_floor_the_gateway_enforces() {
        let temp = tempfile::tempdir().unwrap();
        let identity = Identity::load_or_mint(temp.path()).await.unwrap();
        assert!(identity.key.len() >= 32, "key is {}", identity.key.len());
        assert!(identity.node_id.starts_with("nd_"));
        // URL safe, so nobody has to quote it in a shell or a compose file.
        assert!(identity.key.chars().all(|c| c.is_ascii_alphanumeric()));
    }

    #[tokio::test]
    async fn a_half_written_identity_is_refused_rather_than_used() {
        // Half an identity is a machine that cannot prove who it is, and using
        // it would register a machine under a blank id.
        let temp = tempfile::tempdir().unwrap();
        tokio::fs::write(
            temp.path().join(IDENTITY_FILE),
            br#"{"node_id":"","key":""}"#,
        )
        .await
        .unwrap();
        assert!(Identity::load_or_mint(temp.path()).await.is_err());
    }

    #[tokio::test]
    async fn a_leftover_temporary_file_is_not_the_identity() {
        let temp = tempfile::tempdir().unwrap();
        let minted = Identity::load_or_mint(temp.path()).await.unwrap();
        tokio::fs::write(temp.path().join("identity.writing"), b"half")
            .await
            .unwrap();
        let again = Identity::load_or_mint(temp.path()).await.unwrap();
        assert_eq!(minted.node_id, again.node_id);
    }

    /// A fake gateway that answers one join with whatever we hand it.
    async fn a_gateway(answer: serde_json::Value) -> (String, tokio::task::JoinHandle<()>) {
        use axum::{Router, routing::post};
        let router = Router::new().route(
            "/v1/nodes/join",
            post(move || {
                let answer = answer.clone();
                async move { axum::Json(answer) }
            }),
        );
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let serving = tokio::spawn(async move {
            axum::serve(listener, router).await.ok();
        });
        (format!("http://{addr}"), serving)
    }

    /// Everything a join needs on this side, in a temporary directory.
    async fn a_machine(
        temp: &Path,
        gateway: &str,
        token: &str,
    ) -> (Config, Identity, Arc<Mutex<Credentials>>) {
        let config = Config {
            addr: "127.0.0.1:0".parse().unwrap(),
            key: None,
            data_dir: temp.to_path_buf(),
            name: "gpu-test".into(),
            hub_url: "https://example.test".into(),
            hub_token: None,
            gateway_url: Some(gateway.to_string()),
            join_token: Some(token.to_string()),
            advertise: None,
        };
        let identity = Identity::load_or_mint(temp).await.unwrap();
        let credentials = Arc::new(Mutex::new(Credentials::load_or_mint(temp).await.unwrap()));
        (config, identity, credentials)
    }

    fn an_authority() -> (String, String) {
        let made = rcgen::generate_simple_self_signed(["authority.test".to_string()]).unwrap();
        let pem = made.cert.pem();
        let fingerprint = crate::tls::fingerprint(&pem).unwrap();
        (pem, fingerprint)
    }

    #[tokio::test]
    async fn a_gateway_that_is_not_the_one_we_were_told_about_is_refused() {
        // The attack this whole mechanism exists to stop: something in the path
        // answers the join, and from then on the machine trusts ITS authority
        // and serves whoever it vouches for. The token names the authority we
        // are expecting, and an impostor cannot produce a certificate that
        // hashes to it.
        crate::tls::choose_cryptography();
        let (impostor_pem, _) = an_authority();
        let (_, expected) = an_authority();

        let (gateway, serving) = a_gateway(serde_json::json!({
            "name": "gpu-test",
            "base_url": "https://10.0.0.9:8081/v1",
            "joined": true,
            "authority": impostor_pem,
            "certificate": "-----BEGIN CERTIFICATE-----\nanything\n-----END CERTIFICATE-----\n",
        }))
        .await;

        let temp = tempfile::tempdir().unwrap();
        let token = format!("{expected}_asecretnobodyelseknows");
        let (config, identity, credentials) = a_machine(temp.path(), &gateway, &token).await;

        let http = reqwest::Client::new();
        let err = join_once(
            &http,
            &gateway,
            &Proof::Invitation(token.clone()),
            &config,
            &identity,
            &credentials,
        )
        .await
        .expect_err("a machine enrolled into an authority it was never told about");
        assert!(
            err.to_string()
                .contains("not the one this machine was told about"),
            "unhelpful refusal: {err}"
        );

        // And nothing was kept. A machine that stored the impostor's authority
        // would serve callers it vouched for after the next restart.
        assert!(credentials.lock().await.signed().is_none());
        assert!(!temp.path().join("authority.pem").exists());
        serving.abort();
    }

    #[tokio::test]
    async fn a_machine_that_has_registered_checks_in_as_itself() {
        // The property the single-use invitation rests on. A machine checks in
        // every day for a fresh certificate; if those check-ins presented the
        // invitation, it could never be spent and never expire, and would be a
        // permanent key to the fleet sitting on every box.
        crate::tls::choose_cryptography();
        let (authority_pem, fingerprint) = an_authority();
        let temp = tempfile::tempdir().unwrap();
        let token = format!("{fingerprint}_asecretnobodyelseknows");
        let (config, identity, credentials) =
            a_machine(temp.path(), "https://example.test", &token).await;

        // Nothing issued yet: the only thing it can present is the invitation.
        let first = proofs(&config, &identity, &credentials).await;
        assert_eq!(first.len(), 1);
        assert_eq!(first[0].secret(), token);

        // Now it holds a certificate, the way it would after one join.
        credentials
            .lock()
            .await
            .store(
                &config.data_dir,
                Enrolled {
                    cert_pem: rcgen::generate_simple_self_signed([
                        "machine.sag.internal".to_string()
                    ])
                    .unwrap()
                    .cert
                    .pem(),
                    authority_pem: authority_pem.clone(),
                },
            )
            .await
            .unwrap();

        let after = proofs(&config, &identity, &credentials).await;
        assert_eq!(
            after[0].secret(),
            identity.key,
            "checked in on the invitation"
        );
        assert_ne!(after[0].secret(), token);
        // And it now expects the authority it already holds, not the one named
        // by a token that may be long gone.
        assert_eq!(after[0].expected_authority().unwrap(), fingerprint);

        // The invitation stays as a fallback, and only as one: a machine carried
        // over from a deployment that no longer knows it has a key that means
        // nothing there.
        assert_eq!(after.len(), 2);
        assert_eq!(after[1].secret(), token);
    }

    #[tokio::test]
    async fn a_machine_with_a_certificate_needs_no_token_at_all() {
        // What lets the installer stop caring about the value it wrote: an hour
        // later it is dead, and by then nothing reads it.
        crate::tls::choose_cryptography();
        let (authority_pem, _) = an_authority();
        let temp = tempfile::tempdir().unwrap();
        let (mut config, identity, credentials) =
            a_machine(temp.path(), "https://example.test", "unused").await;
        config.join_token = None;

        credentials
            .lock()
            .await
            .store(
                &config.data_dir,
                Enrolled {
                    cert_pem: rcgen::generate_simple_self_signed([
                        "machine.sag.internal".to_string()
                    ])
                    .unwrap()
                    .cert
                    .pem(),
                    authority_pem,
                },
            )
            .await
            .unwrap();

        let ways = proofs(&config, &identity, &credentials).await;
        assert_eq!(ways.len(), 1);
        assert_eq!(ways[0].secret(), identity.key);
        // And the certificate is still renewed, which it would not be if renewal
        // were gated on having a token.
        assert!(config.reaches_gateway());
        assert!(!config.joins());
    }

    #[tokio::test]
    async fn a_gateway_that_is_the_one_we_were_told_about_is_believed() {
        crate::tls::choose_cryptography();
        let (authority_pem, fingerprint) = an_authority();
        let cert = rcgen::generate_simple_self_signed(["machine.sag.internal".to_string()])
            .unwrap()
            .cert
            .pem();

        let (gateway, serving) = a_gateway(serde_json::json!({
            "name": "gpu-test",
            "base_url": "https://10.0.0.9:8081/v1",
            "joined": true,
            "authority": authority_pem,
            "certificate": cert,
        }))
        .await;

        let temp = tempfile::tempdir().unwrap();
        let token = format!("{fingerprint}_asecretnobodyelseknows");
        let (config, identity, credentials) = a_machine(temp.path(), &gateway, &token).await;

        let http = reqwest::Client::new();
        let (answer, signed) = join_once(
            &http,
            &gateway,
            &Proof::Invitation(token.clone()),
            &config,
            &identity,
            &credentials,
        )
        .await
        .expect("a legitimate gateway was refused");
        assert!(answer.joined);
        assert_eq!(signed.cert_pem, cert);

        // Kept, so a restart while the gateway is down still comes back serving.
        assert!(temp.path().join("node-cert.pem").exists());
        assert!(temp.path().join("authority.pem").exists());
        assert!(
            Credentials::load_or_mint(temp.path())
                .await
                .unwrap()
                .signed()
                .is_some()
        );
        serving.abort();
    }

    #[tokio::test]
    async fn a_gateway_that_registers_us_but_gives_no_certificate_has_not_finished() {
        // It would leave a machine listed as reachable and unable to serve,
        // which reads as a broken network rather than an incomplete enrolment.
        crate::tls::choose_cryptography();
        let (authority_pem, fingerprint) = an_authority();
        let (gateway, serving) = a_gateway(serde_json::json!({
            "name": "gpu-test",
            "joined": true,
            "authority": authority_pem,
            "certificate": "",
        }))
        .await;

        let temp = tempfile::tempdir().unwrap();
        let token = format!("{fingerprint}_asecretnobodyelseknows");
        let (config, identity, credentials) = a_machine(temp.path(), &gateway, &token).await;

        assert!(
            join_once(
                &reqwest::Client::new(),
                &gateway,
                &Proof::Invitation(token.clone()),
                &config,
                &identity,
                &credentials
            )
            .await
            .is_err()
        );
        serving.abort();
    }

    #[test]
    fn a_token_says_which_authority_to_expect() {
        assert_eq!(fingerprint_in("abc123_secret"), Some("abc123"));
        // The secret may contain underscores; the fingerprint is hex and cannot,
        // so the first underscore is the boundary and nothing is ambiguous.
        assert_eq!(fingerprint_in("abc123_se_cr_et"), Some("abc123"));

        for not_one in ["", "hello", "_", "abc123", "_secret", "abc123_"] {
            assert_eq!(
                fingerprint_in(not_one),
                None,
                "{not_one:?} was read as a token"
            );
        }
    }

    #[test]
    fn the_signature_is_the_one_the_gateway_will_compute() {
        // The gateway is written in another language, so this is the contract
        // between them stated as a number. Computed independently rather than by
        // running this code: a test that asserts what the code does cannot catch
        // the two sides drifting apart.
        assert_eq!(
            signature("abc_secret", br#"{"node_id":"nd_1"}"#),
            "0b8a9318c0a506d45b715d44549c90f095e9c86fefec3ba1ce8d235ae9ee5f47"
        );
    }

    #[tokio::test]
    async fn a_machine_told_nothing_does_not_try_to_register() {
        // Returning rather than looping is what lets a node be run by hand, with
        // a key somebody set, and never mention a gateway.
        let temp = tempfile::tempdir().unwrap();
        let config = Config {
            addr: "127.0.0.1:0".parse().unwrap(),
            key: Some("0123456789abcdef0123456789abcdef".into()),
            data_dir: temp.path().to_path_buf(),
            name: "solo".into(),
            hub_url: "https://example.test".into(),
            hub_token: None,
            gateway_url: None,
            join_token: None,
            advertise: None,
        };
        let identity = Identity::load_or_mint(temp.path()).await.unwrap();
        let credentials = Arc::new(Mutex::new(
            Credentials::load_or_mint(temp.path()).await.unwrap(),
        ));
        // Completes rather than looping forever.
        assert!(keep_joining(config, identity, credentials).await.is_none());
    }
}
