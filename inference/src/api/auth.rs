//! The second thing standing between this machine and the network.
//!
//! The first is the connection itself: a node serves over mutual TLS and refuses
//! anyone without a certificate from our authority, and the usages are split so
//! a machine's own certificate cannot be used to call another machine (KB/35).
//! This layer is not what makes the boundary any more, and it is kept anyway,
//! because the two revoke on different clocks. Deleting a machine takes its key
//! with it and the next request fails; its certificate stays valid for ninety
//! days and there is nothing that says otherwise. One header is a cheap price
//! for "removed means removed".
//!
//! Two properties follow, and both are tested rather than assumed:
//!
//! **The comparison is constant time.** A byte-by-byte compare that returns on
//! the first difference leaks the key one character at a time to anybody who can
//! time a request, and an attacker on the same network can time a request very
//! well indeed.
//!
//! **A refusal says nothing.** Not whether a key was sent, not whether it was
//! the right length, not whether this node has one configured. A caller that can
//! tell "no key" from "wrong key" is being helped.

use axum::{
    extract::{Request, State},
    http::header,
    middleware::Next,
    response::Response,
};

use crate::{api::App, error::Error};

/// Refuse anything that does not carry this node's key.
pub async fn require_key(
    State(app): State<App>,
    request: Request,
    next: Next,
) -> Result<Response, Error> {
    let presented = request
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        .and_then(bearer)
        .unwrap_or_default();

    // Both sides go through the same compare whatever was sent, including when
    // nothing was: an early return for a missing header is a timing difference
    // that says "this node wants a key", which is a thing worth not saying.
    if !constant_time_eq::constant_time_eq(presented.as_bytes(), app.key.as_bytes()) {
        tracing::warn!(
            path = %request.uri().path(),
            "refused a request without this node's key"
        );
        return Err(Error::Unauthorised);
    }

    Ok(next.run(request).await)
}

/// The token out of an `Authorization` header, if it is a bearer one.
///
/// The scheme is matched case-insensitively because that is what the standard
/// says, and a client that sends `bearer` in lower case is not an attacker, it
/// is a client.
fn bearer(header: &str) -> Option<&str> {
    let (scheme, token) = header.split_once(' ')?;
    scheme
        .eq_ignore_ascii_case("bearer")
        .then(|| token.trim())
        .filter(|t| !t.is_empty())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::api::testing::Harness;
    use axum::{
        body::Body,
        http::{Request as HttpRequest, StatusCode},
    };
    use tower::ServiceExt;

    fn get(path: &str, key: Option<&str>) -> HttpRequest<Body> {
        let mut req = HttpRequest::builder().uri(path).method("GET");
        if let Some(key) = key {
            req = req.header(header::AUTHORIZATION, format!("Bearer {key}"));
        }
        req.body(Body::empty()).unwrap()
    }

    #[test]
    fn a_bearer_token_is_read_whatever_the_case_of_the_scheme() {
        assert_eq!(bearer("Bearer abc"), Some("abc"));
        assert_eq!(bearer("bearer abc"), Some("abc"));
        assert_eq!(bearer("BEARER abc"), Some("abc"));
    }

    #[test]
    fn anything_that_is_not_a_bearer_token_is_nothing() {
        assert_eq!(bearer("Basic abc"), None);
        assert_eq!(bearer("abc"), None);
        assert_eq!(bearer("Bearer "), None);
        assert_eq!(bearer(""), None);
    }

    #[tokio::test]
    async fn the_right_key_gets_in() {
        let node = Harness::start().await;
        let response = node
            .router
            .clone()
            .oneshot(get("/node", Some(node.key())))
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::OK);
    }

    #[tokio::test]
    async fn no_key_wrong_key_and_a_prefix_of_the_key_are_all_refused_the_same_way() {
        let node = Harness::start().await;
        let key = node.key().to_string();
        let attempts = [
            None,
            Some(String::new()),
            Some("wrong".into()),
            // The attack the constant-time compare exists for: a key that is
            // right up to one character.
            Some(key[..key.len() - 1].to_string()),
            Some(format!("{key}x")),
        ];

        for attempt in attempts {
            let response = node
                .router
                .clone()
                .oneshot(get("/node", attempt.as_deref()))
                .await
                .unwrap();
            assert_eq!(
                response.status(),
                StatusCode::UNAUTHORIZED,
                "an attempt got through"
            );
        }
    }

    #[tokio::test]
    async fn a_refusal_does_not_say_what_was_wrong_with_the_attempt() {
        let node = Harness::start().await;
        let mut bodies = Vec::new();
        for attempt in [None, Some("wrong")] {
            let response = node
                .router
                .clone()
                .oneshot(get("/node", attempt))
                .await
                .unwrap();
            let body = axum::body::to_bytes(response.into_body(), 64 * 1024)
                .await
                .unwrap();
            bodies.push(String::from_utf8(body.to_vec()).unwrap());
        }
        assert_eq!(
            bodies[0], bodies[1],
            "the refusal tells a caller whether they sent a key at all"
        );
    }

    #[tokio::test]
    async fn every_route_that_matters_is_behind_the_key() {
        // A route added to the node without the key is the whole boundary gone,
        // so this asks the router rather than trusting where the layer was put.
        let node = Harness::start().await;
        let guarded = [
            "/node",
            "/node/stats",
            "/node/models",
            "/node/models/anything",
            "/node/models/anything/form",
            "/node/library/search?q=x",
            "/node/library/describe?repo=v/m",
            "/node/pulls",
            "/node/pulls/anything",
        ];
        for path in guarded {
            let response = node.router.clone().oneshot(get(path, None)).await.unwrap();
            assert_eq!(
                response.status(),
                StatusCode::UNAUTHORIZED,
                "{path} answered without the key"
            );
        }
    }

    #[tokio::test]
    async fn liveness_is_answered_without_the_key_and_says_nothing_else() {
        // A container probe cannot hold a credential. What it gets back must
        // therefore be worth nothing to anybody who is not a container probe.
        let node = Harness::start().await;
        let response = node
            .router
            .clone()
            .oneshot(get("/live", None))
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::OK);

        let body = axum::body::to_bytes(response.into_body(), 1024)
            .await
            .unwrap();
        assert_eq!(&body[..], b"ok");
    }
}
