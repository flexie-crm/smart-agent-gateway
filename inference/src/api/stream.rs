//! The bytes of a stream are ours, even when the answer is not.
//!
//! The engine serves inference and we let it: reimplementing its endpoint would
//! throw away everything else it does (vision, speech, embeddings) to rewrite
//! something that works. But the FRAMING of a stream is not a detail of how a
//! model is executed, it is our protocol, and leaving it to a library means a
//! version bump can change our wire with no compile error and no failing test.
//!
//! That is not hypothetical. The engine emits an SSE comment between every
//! token while it waits for the next one, which is legal and ordinary. The
//! gateway's stream reader turned each of those into an empty event and failed
//! with "unexpected end of JSON input", killing a healthy answer on its first
//! keep-alive. Nobody on either side had ever said what a frame looks like.
//!
//! So this layer sits over the engine's router and owns what leaves the machine:
//!
//! - **a comment never ends an event.** The specification says a dispatch with
//!   an empty data buffer must not happen, and this makes that true of our wire
//!   whatever the engine emits;
//! - **keep-alives are ours**, on our own clock, and only when the stream has
//!   genuinely gone quiet. One between every token is not keeping a connection
//!   alive, it is noise with a failure mode;
//! - **everything else passes through byte for byte.** A layer that rewrote a
//!   chunk would be a worse bug than the one it fixes.
//!
//! It applies to responses that say they are an event stream and to nothing
//! else, so an ordinary JSON answer never enters it.

use std::time::Duration;

use axum::{
    body::{Body, Bytes},
    extract::Request,
    http::header,
    middleware::Next,
    response::Response,
};
use futures::StreamExt;

/// How long a stream may be silent before we say something.
///
/// Ours, not the engine's. Long enough that an ordinary answer never triggers
/// one, short enough that an intermediary with a common idle timeout does not
/// drop a model that is thinking hard.
const QUIET: Duration = Duration::from_secs(15);

/// What we send to hold a connection open: a comment, which the specification
/// says every reader must ignore.
const KEEP_ALIVE: &[u8] = b": keep-alive\n\n";

/// How much of a partial line to hold before giving up on finding its end.
///
/// A chunk carrying a long tool call is legitimately large. A "line" that grows
/// past this is not a line, and holding it forever would be a stream that stops
/// with no explanation.
const MAX_LINE: usize = 8 * 1024 * 1024;

pub async fn own_the_stream(request: Request, next: Next) -> Response {
    let path = request.uri().path().to_string();
    let response = next.run(request).await;
    if !is_event_stream(&response) {
        return response;
    }

    let (parts, body) = response.into_parts();
    let (sender, receiver) = tokio::sync::mpsc::channel::<Result<Bytes, std::io::Error>>(32);

    tokio::spawn(async move {
        // What a person watching the log actually wants to know about a stream,
        // and could not get before: an answer takes minutes on a processor, and
        // the only line about it was written the microsecond the headers went
        // out. "took=233µs" for a request that ran for twelve minutes is worse
        // than no line at all.
        let opened = std::time::Instant::now();
        let mut first_event: Option<std::time::Duration> = None;
        let mut events: u64 = 0;
        let mut bytes_out: u64 = 0;
        let mut source = body.into_data_stream();
        // Bytes arrive in whatever sizes the network chose, so a line can span
        // two of them and two lines can share one. Everything below works on
        // complete lines, which means holding the remainder.
        let mut partial: Vec<u8> = Vec::new();
        // Whether anything has been written since the last blank line: a blank
        // line only means "dispatch" when there is something to dispatch.
        let mut pending = false;

        loop {
            let next_chunk = tokio::time::timeout(QUIET, source.next()).await;

            let chunk = match next_chunk {
                // Quiet for long enough to be worth saying something. Sent only
                // between events, never inside one, so it cannot split a frame.
                Err(_elapsed) => {
                    if !pending
                        && sender
                            .send(Ok(Bytes::from_static(KEEP_ALIVE)))
                            .await
                            .is_err()
                    {
                        return;
                    }
                    continue;
                }
                Ok(None) => break,
                Ok(Some(Err(err))) => {
                    let _ = sender.send(Err(std::io::Error::other(err))).await;
                    return;
                }
                Ok(Some(Ok(chunk))) => chunk,
            };

            partial.extend_from_slice(&chunk);
            if partial.len() > MAX_LINE {
                let _ = sender
                    .send(Err(std::io::Error::other(
                        "a stream frame grew past its limit",
                    )))
                    .await;
                return;
            }

            let mut out: Vec<u8> = Vec::with_capacity(partial.len());
            while let Some(end) = partial.iter().position(|b| *b == b'\n') {
                let line: Vec<u8> = partial.drain(..=end).collect();
                let body = &line[..line.len() - 1];
                // A line ending in \r\n is the same line.
                let body = body.strip_suffix(b"\r").unwrap_or(body);

                match body.first() {
                    // The end of an event. Passed on only when there is an event
                    // to end, so a lone comment never becomes an empty dispatch.
                    None => {
                        if !pending {
                            continue;
                        }
                        pending = false;
                    }
                    // A comment. Dropped, so the blank line after it has nothing
                    // to terminate. Ours replace them, on our own clock.
                    Some(b':') => continue,
                    _ => {
                        if !pending {
                            events += 1;
                            // The first one is the answer beginning: everything
                            // before it is the model reading the prompt, which
                            // is where the time goes on a large one.
                            if first_event.is_none() {
                                let waited = opened.elapsed();
                                first_event = Some(waited);
                                tracing::info!(path, ?waited, "stream answering");
                            }
                        }
                        pending = true;
                    }
                }
                out.extend_from_slice(&line);
            }

            if !out.is_empty() {
                bytes_out += out.len() as u64;
                if sender.send(Ok(Bytes::from(out))).await.is_err() {
                    // The reader went away. Worth a line: an answer nobody is
                    // listening to is usually somebody's browser giving up.
                    tracing::info!(path, events, ?opened, "stream abandoned by its reader");
                    return;
                }
            }
        }

        // Whatever was left without its newline. Passing it on unchanged is the
        // honest end: it is the engine's bytes, and truncating them silently
        // would be this layer inventing an ending.
        if !partial.is_empty() {
            let _ = sender.send(Ok(Bytes::from(partial))).await;
        }

        let took = opened.elapsed();
        tracing::info!(
            path,
            events,
            bytes = bytes_out,
            answering_after = ?first_event.unwrap_or(took),
            ?took,
            "stream finished"
        );
    });

    Response::from_parts(
        parts,
        Body::from_stream(tokio_stream::wrappers::ReceiverStream::new(receiver)),
    )
}

fn is_event_stream(response: &Response) -> bool {
    response
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .is_some_and(|value| value.starts_with("text/event-stream"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{Router, body::to_bytes, routing::get};
    use tower::ServiceExt;

    /// Serve a fixed body with a content type, through the layer.
    async fn through(content_type: &'static str, body: &'static str) -> String {
        let app = Router::new()
            .route(
                "/",
                get(move || async move {
                    Response::builder()
                        .header(header::CONTENT_TYPE, content_type)
                        .body(Body::from(body))
                        .unwrap()
                }),
            )
            .layer(axum::middleware::from_fn(own_the_stream));

        let response = app
            .oneshot(
                axum::http::Request::builder()
                    .uri("/")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        let bytes = to_bytes(response.into_body(), 16 * 1024 * 1024)
            .await
            .unwrap();
        String::from_utf8(bytes.to_vec()).unwrap()
    }

    #[tokio::test]
    async fn a_comment_never_becomes_an_empty_event() {
        // Byte for byte what the engine sends between tokens. Each of these used
        // to reach the gateway as an event with no data, and fail to parse.
        let got = through(
            "text/event-stream",
            ":\n\n:\n\ndata: {\"a\":1}\n\n:\n\ndata: [DONE]\n\n",
        )
        .await;
        assert_eq!(got, "data: {\"a\":1}\n\ndata: [DONE]\n\n");
    }

    #[tokio::test]
    async fn a_real_event_passes_through_byte_for_byte() {
        // A layer that edited a chunk would be worse than the bug it fixes.
        let raw = "event: message\ndata: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n";
        assert_eq!(through("text/event-stream", raw).await, raw);
    }

    #[tokio::test]
    async fn a_data_field_split_across_lines_survives() {
        // SSE allows it and the reader joins them. Dropping the blank line in
        // the middle would merge two events into one.
        let raw = "data: {\"a\":1,\ndata: \"b\":2}\n\n";
        assert_eq!(through("text/event-stream", raw).await, raw);
    }

    #[tokio::test]
    async fn carriage_returns_do_not_defeat_it() {
        let got = through("text/event-stream", ":\r\n\r\ndata: {\"a\":1}\r\n\r\n").await;
        assert_eq!(got, "data: {\"a\":1}\r\n\r\n");
    }

    #[tokio::test]
    async fn nothing_but_comments_produces_nothing() {
        assert_eq!(through("text/event-stream", ":\n\n: ping\n\n").await, "");
    }

    #[tokio::test]
    async fn a_stream_with_no_comments_is_untouched() {
        // The overwhelming majority. The layer must be invisible.
        let raw = "data: {\"a\":1}\n\ndata: {\"a\":2}\n\ndata: [DONE]\n\n";
        assert_eq!(through("text/event-stream", raw).await, raw);
    }

    #[tokio::test]
    async fn an_ordinary_answer_does_not_enter_the_layer() {
        // It is installed over every route, so a JSON body must pass untouched,
        // comments and all: a colon at the start of a line is ordinary there.
        let raw = "{\"a\":1}";
        assert_eq!(through("application/json", raw).await, raw);
    }

    #[tokio::test]
    async fn a_large_frame_survives() {
        // A chunk carrying a long tool call is legitimately large, and a stream
        // that died on one would fail only on the interesting answers.
        let big = format!("data: {{\"x\":\"{}\"}}\n\n", "y".repeat(300_000));
        let raw: &'static str = Box::leak(big.into_boxed_str());
        assert_eq!(through("text/event-stream", raw).await.len(), raw.len());
    }

    #[tokio::test]
    async fn a_final_line_with_no_newline_is_not_swallowed() {
        // Truncating it would be this layer inventing an ending.
        assert_eq!(
            through("text/event-stream", "data: {\"a\":1}").await,
            "data: {\"a\":1}"
        );
    }
}
