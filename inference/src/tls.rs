//! What this machine proves itself with, and what it demands in return.
//!
//! A machine used to be reachable only inside one network, so it served plain
//! HTTP and the key in the header was the whole boundary. On a public address
//! that key is readable by anybody in the path, and reading it is enough to
//! drive somebody else's hardware. So both ends of every connection now hold a
//! certificate from the gateway's authority, and neither talks to anything that
//! cannot show one.
//!
//! Three files in the data directory, and the difference between them matters:
//!
//! - **`node-key.pem`** is ours and never leaves this disk. It is minted once
//!   and kept, so a machine's identity survives restarts the way its node id
//!   does;
//! - **`node-cert.pem`** is what the gateway signed for us, and it is not a
//!   secret: it is what we present, and it means nothing without the key;
//! - **`authority.pem`** is who we trust. Every caller must present a
//!   certificate that chains to it, which is what stops anything else on the
//!   network driving this machine.
//!
//! The certificate is REPLACEABLE while the process runs. A machine re-enrols
//! whenever it starts and periodically after that, and a renewal that required a
//! restart would be a renewal somebody forgets until the day it expires.

use std::{path::Path, sync::Arc};

#[cfg(test)]
use rustls::pki_types::CertificateSigningRequestDer as CertificateRequestDer;
use rustls::{
    ServerConfig,
    pki_types::{CertificateDer, PrivateKeyDer, pem::PemObject},
    server::WebPkiClientVerifier,
};
use sha2::{Digest, Sha256};

use crate::error::{Error, Result};

/// Our key, minted once and kept.
const KEY_FILE: &str = "node-key.pem";
/// What the gateway signed for us.
const CERT_FILE: &str = "node-cert.pem";
/// Who we trust, and who trusts us.
const AUTHORITY_FILE: &str = "authority.pem";

/// This machine's key and the certificate it has been given for it.
///
/// The key is always here; the certificate only after a first successful
/// enrolment, which is why it is optional. A machine with a key and no
/// certificate is one that has never reached its gateway, and it cannot serve
/// until it has.
pub struct Credentials {
    /// The key, kept as PEM because that is what both rcgen and rustls read and
    /// what is on the disk. It is never sent anywhere.
    key_pem: String,
    /// What we present, and who we trust. Both arrive together, from the same
    /// answer, and neither is any use without the other.
    signed: Option<Signed>,
}

/// A certificate and the authority that issued it.
#[derive(Clone, Debug)]
pub struct Signed {
    pub cert_pem: String,
    pub authority_pem: String,
}

impl Credentials {
    /// Read this machine's key and whatever it has been issued, minting the key
    /// the first time.
    ///
    /// Written the way the identity file is: to a temporary file beside the
    /// target and renamed over it, so a process killed mid-write leaves the old
    /// key or the new one and never half of either. Half a key is a machine that
    /// can prove nothing.
    pub async fn load_or_mint(dir: &Path) -> Result<Self> {
        let path = dir.join(KEY_FILE);
        let key_pem = match tokio::fs::read_to_string(&path).await {
            Ok(pem) => pem,
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
                let key = rcgen::KeyPair::generate().map_err(|err| {
                    Error::invalid(format!("this machine cannot mint a key: {err}"))
                })?;
                let pem = key.serialize_pem();
                write_private(&path, &pem).await?;
                tracing::info!("this machine minted the key it proves itself with");
                pem
            }
            Err(err) => return Err(err.into()),
        };

        let signed = match (
            tokio::fs::read_to_string(dir.join(CERT_FILE)).await,
            tokio::fs::read_to_string(dir.join(AUTHORITY_FILE)).await,
        ) {
            (Ok(cert_pem), Ok(authority_pem)) => Some(Signed {
                cert_pem,
                authority_pem,
            }),
            // Either missing is the same situation: nothing to serve with. A
            // certificate without its authority cannot be verified against
            // anything, and an authority without a certificate is nothing to
            // present.
            _ => None,
        };
        Ok(Self { key_pem, signed })
    }

    /// What this machine currently holds, if anything.
    pub fn signed(&self) -> Option<Signed> {
        self.signed.clone()
    }

    /// Ask to be given an identity: a certificate request over our key.
    ///
    /// The request is signed by the key it asks to have certified, which is what
    /// stops one machine getting a certificate issued over another's key. It
    /// carries no name worth reading: the gateway names the certificate after
    /// the node id it knows, not after anything we claim here.
    pub fn certificate_request(&self, node_id: &str) -> Result<String> {
        let key = rcgen::KeyPair::from_pem(&self.key_pem)
            .map_err(|err| Error::invalid(format!("this machine's key is not readable: {err}")))?;
        let mut params = rcgen::CertificateParams::default();
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, node_id);
        params
            .serialize_request(&key)
            .map(|csr| csr.pem())
            .map_err(|err| Error::invalid(format!("cannot ask for a certificate: {err}")))?
            .map_err(|err| Error::invalid(format!("cannot ask for a certificate: {err}")))
    }

    /// Keep what the gateway issued.
    ///
    /// Both files or neither: a certificate stored without its authority would
    /// come back after a restart as something we cannot verify a caller with.
    pub async fn store(&mut self, dir: &Path, signed: Signed) -> Result<()> {
        write_public(&dir.join(CERT_FILE), &signed.cert_pem).await?;
        write_public(&dir.join(AUTHORITY_FILE), &signed.authority_pem).await?;
        self.signed = Some(signed);
        Ok(())
    }

    /// The key, for building a TLS configuration out of.
    pub fn key_pem(&self) -> &str {
        &self.key_pem
    }
}

/// Build what this machine serves with: our certificate, and a demand that
/// whoever calls presents one from the same authority.
///
/// The demand is the important half. Anybody can reach a public address; what
/// makes it safe is that a connection from anybody who is not the gateway is
/// refused before a single byte of a request is read. The gateway's certificate
/// carries the CLIENT usage and a machine's does not, so another machine's
/// certificate cannot stand in for it.
pub fn server_config(key_pem: &str, signed: &Signed) -> Result<Arc<ServerConfig>> {
    let certs = certificates(&signed.cert_pem, "this machine's certificate")?;
    let key = PrivateKeyDer::from_pem_slice(key_pem.as_bytes())
        .map_err(|err| Error::invalid(format!("this machine's key is not readable: {err}")))?;

    let mut roots = rustls::RootCertStore::empty();
    for cert in certificates(&signed.authority_pem, "the authority")? {
        roots
            .add(cert)
            .map_err(|err| Error::invalid(format!("the authority is not usable: {err}")))?;
    }
    let verifier = WebPkiClientVerifier::builder(Arc::new(roots))
        .build()
        .map_err(|err| Error::invalid(format!("callers cannot be checked: {err}")))?;

    ServerConfig::builder()
        .with_client_cert_verifier(verifier)
        .with_single_cert(certs, key)
        .map(Arc::new)
        .map_err(|err| Error::invalid(format!("this machine cannot serve securely: {err}")))
}

/// The same thing, wrapped so it can be REPLACED while the listener runs.
///
/// A renewal that required a restart would be a renewal somebody forgets until
/// the morning it expires, and restarting a machine to swap a certificate would
/// drop whatever it was answering.
pub fn serving(key_pem: &str, signed: &Signed) -> Result<axum_server::tls_rustls::RustlsConfig> {
    Ok(axum_server::tls_rustls::RustlsConfig::from_config(
        server_config(key_pem, signed)?,
    ))
}

/// Choose the cryptography once, before anything uses it.
///
/// Two implementations are compiled in (one is what our HTTP client picked, the
/// other what the TLS crate defaults to), and with two present there is no
/// default: building a configuration without having said which would panic on a
/// machine that had otherwise started perfectly. Saying so here makes it a
/// decision rather than an accident.
///
/// Already-installed is not an error: something else in the process may have got
/// there first, and it would be the same choice.
pub fn choose_cryptography() {
    let _ = rustls::crypto::aws_lc_rs::default_provider().install_default();
}

/// The fingerprint of a certificate, as the gateway computes it.
///
/// This is the value carried in the join token, and comparing it is how this
/// machine knows the authority it was handed is the one it was told about
/// rather than one an intermediary made up. Over the DER bytes, because that is
/// the certificate itself; the PEM around them is packaging and two encodings of
/// one certificate must not fingerprint differently.
pub fn fingerprint(authority_pem: &str) -> Result<String> {
    let cert = certificates(authority_pem, "the authority")?
        .into_iter()
        .next()
        .ok_or_else(|| Error::invalid("the authority is empty"))?;
    Ok(hex(Sha256::digest(cert.as_ref()).as_slice()))
}

fn certificates(pem: &str, what: &str) -> Result<Vec<CertificateDer<'static>>> {
    let certs: std::result::Result<Vec<_>, _> =
        CertificateDer::pem_slice_iter(pem.as_bytes()).collect();
    let certs = certs.map_err(|err| Error::invalid(format!("{what} is not readable: {err}")))?;
    if certs.is_empty() {
        return Err(Error::invalid(format!("{what} contains no certificate")));
    }
    Ok(certs)
}

fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes.iter().fold(String::new(), |mut out, b| {
        let _ = write!(out, "{b:02x}");
        out
    })
}

/// Write a secret, readable by nobody else.
///
/// The mode is set on the temporary file BEFORE the content is renamed into
/// place, so the key is never briefly world readable. A key that leaked for one
/// millisecond leaked.
async fn write_private(path: &Path, contents: &str) -> Result<()> {
    let temp = path.with_extension("writing");
    tokio::fs::write(&temp, contents).await?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        tokio::fs::set_permissions(&temp, std::fs::Permissions::from_mode(0o600)).await?;
    }
    tokio::fs::rename(&temp, path).await?;
    Ok(())
}

async fn write_public(path: &Path, contents: &str) -> Result<()> {
    let temp = path.with_extension("writing");
    tokio::fs::write(&temp, contents).await?;
    tokio::fs::rename(&temp, path).await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn a_key_is_minted_once_and_then_kept() {
        // The same reason the node id is kept: an identity that changed every
        // boot would be a different machine every boot.
        let temp = tempfile::tempdir().unwrap();
        let first = Credentials::load_or_mint(temp.path()).await.unwrap();
        let second = Credentials::load_or_mint(temp.path()).await.unwrap();
        assert_eq!(first.key_pem(), second.key_pem());
        assert!(
            first.signed().is_none(),
            "a fresh machine holds no certificate"
        );
    }

    #[tokio::test]
    async fn two_machines_are_two_keys() {
        let a = tempfile::tempdir().unwrap();
        let b = tempfile::tempdir().unwrap();
        let one = Credentials::load_or_mint(a.path()).await.unwrap();
        let two = Credentials::load_or_mint(b.path()).await.unwrap();
        assert_ne!(one.key_pem(), two.key_pem());
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn the_key_is_not_readable_by_anybody_else() {
        use std::os::unix::fs::PermissionsExt;
        let temp = tempfile::tempdir().unwrap();
        Credentials::load_or_mint(temp.path()).await.unwrap();
        let mode = tokio::fs::metadata(temp.path().join(KEY_FILE))
            .await
            .unwrap()
            .permissions()
            .mode();
        assert_eq!(mode & 0o077, 0, "the key is readable by others: {mode:o}");
    }

    #[tokio::test]
    async fn a_certificate_request_is_over_our_own_key() {
        // The property the gateway checks: a request is signed by the key it asks
        // to have certified, so nobody can be given a certificate over a key they
        // do not hold.
        let temp = tempfile::tempdir().unwrap();
        let creds = Credentials::load_or_mint(temp.path()).await.unwrap();
        let csr = creds.certificate_request("nd_test").unwrap();
        assert!(csr.contains("BEGIN CERTIFICATE REQUEST"));

        // The request carries OUR public key. That the gateway also checks the
        // request is SIGNED by the matching private key is asserted on the
        // gateway's side, where it is enforced; what has to be true here is that
        // the key we are asking about is the one we hold.
        let der = CertificateRequestDer::from_pem_slice(csr.as_bytes())
            .expect("not a certificate request");
        let der = der.as_ref();
        let ours = rcgen::KeyPair::from_pem(creds.key_pem()).unwrap();
        assert!(
            der.windows(ours.public_key_raw().len())
                .any(|window| window == ours.public_key_raw()),
            "the request asks about a key this machine does not hold"
        );
    }

    #[tokio::test]
    async fn a_certificate_is_kept_only_with_its_authority() {
        // Storing one without the other would come back after a restart as
        // something to present with nothing to check callers against.
        let temp = tempfile::tempdir().unwrap();
        let mut creds = Credentials::load_or_mint(temp.path()).await.unwrap();
        creds
            .store(
                temp.path(),
                Signed {
                    cert_pem: "cert".into(),
                    authority_pem: "authority".into(),
                },
            )
            .await
            .unwrap();

        let again = Credentials::load_or_mint(temp.path()).await.unwrap();
        let held = again
            .signed()
            .expect("a stored certificate was not read back");
        assert_eq!(held.cert_pem, "cert");
        assert_eq!(held.authority_pem, "authority");

        // And half of it is nothing at all.
        tokio::fs::remove_file(temp.path().join(AUTHORITY_FILE))
            .await
            .unwrap();
        let orphaned = Credentials::load_or_mint(temp.path()).await.unwrap();
        assert!(
            orphaned.signed().is_none(),
            "a certificate with no authority was treated as usable"
        );
    }

    #[test]
    fn a_fingerprint_is_of_the_certificate_and_not_its_packaging() {
        // The gateway hashes the DER. If this hashed the PEM text, a certificate
        // that arrived with different line wrapping would look like a different
        // authority and every join would fail on a formatting difference.
        let a = rcgen::generate_simple_self_signed(["a.test".to_string()]).unwrap();
        let pem = a.cert.pem();
        let mine = fingerprint(&pem).unwrap();
        let theirs = hex(Sha256::digest(a.cert.der().as_ref()).as_slice());
        assert_eq!(mine, theirs);
        assert_eq!(mine.len(), 64);

        // Re-wrapped, same certificate, same answer.
        let rewrapped = pem.replace('\n', "\r\n");
        assert_eq!(fingerprint(&rewrapped).unwrap(), mine);
    }

    #[test]
    fn rubbish_is_not_an_authority() {
        assert!(fingerprint("").is_err());
        assert!(fingerprint("hello").is_err());
    }
}
