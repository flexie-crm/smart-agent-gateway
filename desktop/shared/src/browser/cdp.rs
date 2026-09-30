//! Speaking to the browser.
//!
//! One websocket to the browser, and every request and answer on it. The
//! protocol itself is plain: `{id, method, params}` out, `{id, result}` or
//! `{id, error}` back, and anything arriving with no id is an event. There is no
//! library here because there is nothing a library would do that this does not,
//! and a dependency on somebody's protocol bindings is a dependency on their
//! idea of which version of the protocol we speak.
//!
//! **Requests are multiplexed.** Several can be in flight at once and answers
//! come back in whatever order the browser produces them, matched by id. That
//! is not a luxury: one page waiting for a navigation must not stop another
//! being asked what is on it, and a tool call that blocked the whole connection
//! would make a fleet of agents queue behind each other.
//!
//! **A page is a session.** Attaching to a target gives a session id, and every
//! message for that page carries it. Sessions are flattened onto this one
//! socket (`flatten: true`), which is the arrangement that makes one connection
//! enough for the whole browser.
//!
//! **Nothing here knows what a selector is.** This carries messages. What to
//! say is `inject` and the tools above it.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use serde_json::{json, Value};
use tokio::sync::{broadcast, oneshot};
use tokio_tungstenite::tungstenite::Message;

/// How long any one request may wait for its answer.
///
/// Generous, because some of them are genuinely slow: a navigation to a page
/// that is fetching half a megabyte of JavaScript is a real wait, not a stall.
/// What this is for is the answer that is never coming, which without a bound
/// would hold a tool call until somebody closed the application.
const ANSWER_WITHIN: Duration = Duration::from_secs(60);

/// How many events may queue for a listener before the oldest are dropped.
///
/// Events are a running commentary (a page loaded, a console line, a request
/// finished) and a slow reader must not be able to hold the connection still.
/// Dropping the oldest is right for what these are: a listener that has fallen
/// two hundred events behind has already missed the one it was waiting for.
const EVENT_BACKLOG: usize = 256;

/// One event the browser announced.
#[derive(Debug, Clone)]
pub struct Event {
    pub method: String,
    pub session: Option<String>,
    pub params: Value,
}

/// A connection to a running browser.
#[derive(Clone)]
pub struct Connection {
    inner: Arc<Inner>,
}

/// Written out by hand rather than derived, and it says almost nothing on
/// purpose.
///
/// What this holds is a socket, a table of callers waiting for answers, and a
/// channel of events. Derived, it would print all three, and the third carries
/// whatever the browser has been announcing: the contents of somebody's page.
/// A type that is cheap to put in a log line should not be the thing that puts
/// a page into one.
impl std::fmt::Debug for Connection {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Connection(browser)")
    }
}

struct Inner {
    /// Requests out. The writing half is owned by one task, so nothing here
    /// needs a lock on the socket itself.
    outbox: tokio::sync::mpsc::UnboundedSender<Message>,
    /// Who is waiting for which answer.
    waiting: Mutex<HashMap<u64, oneshot::Sender<Result<Value, String>>>>,
    /// Events, for anybody who has asked to hear them.
    events: broadcast::Sender<Event>,
    next: AtomicU64,
    /// The task holding the socket, so dropping the connection ends it rather
    /// than leaving a reader on a socket nobody will ever answer.
    ///
    /// Tauri's handle, not tokio's: the task is spawned on the runtime the
    /// application already has, which is the same one `link.rs` uses for its own
    /// socket. They are different types with similar names.
    held: Mutex<Option<tauri::async_runtime::JoinHandle<()>>>,
}

impl Drop for Inner {
    fn drop(&mut self) {
        if let Some(task) = self.held.lock().ok().and_then(|mut h| h.take()) {
            task.abort();
        }
    }
}

impl Connection {
    /// Open a connection to the browser at this address.
    ///
    /// The address is the one the browser wrote down when it started, token and
    /// all: see `supervise::Endpoint`, which reads it from the browser's own
    /// file rather than discovering it by asking.
    pub async fn open(websocket: &str) -> Result<Self, String> {
        let (socket, _) = tokio_tungstenite::connect_async(websocket)
            .await
            .map_err(|err| format!("the browser would not accept a connection: {err}"))?;
        let (mut writing, mut reading) = socket.split();

        let (outbox, mut queued) = tokio::sync::mpsc::unbounded_channel::<Message>();
        let (events, _) = broadcast::channel(EVENT_BACKLOG);
        let inner = Arc::new(Inner {
            outbox,
            waiting: Mutex::new(HashMap::new()),
            events: events.clone(),
            next: AtomicU64::new(1),
            held: Mutex::new(None),
        });

        // One task owning both halves of the socket. Writing from many callers
        // through a channel rather than by locking the sink: a lock held across
        // an await is a lock held for as long as the network takes.
        let carrying = Arc::downgrade(&inner);
        let task = tauri::async_runtime::spawn(async move {
            loop {
                tokio::select! {
                    outgoing = queued.recv() => {
                        let Some(message) = outgoing else { return };
                        if writing.send(message).await.is_err() {
                            break;
                        }
                    }
                    incoming = reading.next() => {
                        match incoming {
                            Some(Ok(Message::Text(text))) => {
                                let Some(inner) = carrying.upgrade() else { return };
                                inner.deliver(&text);
                            }
                            Some(Ok(_)) => {}
                            Some(Err(_)) | None => break,
                        }
                    }
                }
            }
            // The socket has gone. Everybody waiting is told, rather than left
            // to time out one by one over the next minute: a browser that has
            // died is a fact we have, and sixty seconds of silence per caller
            // is a fact we would be withholding.
            if let Some(inner) = carrying.upgrade() {
                inner.give_up("the connection to the browser was lost");
            }
        });
        *inner.held.lock().unwrap_or_else(|e| e.into_inner()) = Some(task);

        Ok(Self { inner })
    }

    /// Ask the browser something, and wait for the answer.
    pub async fn call(&self, method: &str, params: Value) -> Result<Value, String> {
        self.send(method, params, None).await
    }

    /// Ask a particular page something.
    pub async fn call_on(
        &self,
        session: &str,
        method: &str,
        params: Value,
    ) -> Result<Value, String> {
        self.send(method, params, Some(session)).await
    }

    async fn send(
        &self,
        method: &str,
        params: Value,
        session: Option<&str>,
    ) -> Result<Value, String> {
        let id = self.inner.next.fetch_add(1, Ordering::SeqCst);
        let mut message = json!({ "id": id, "method": method, "params": params });
        if let Some(session) = session {
            message["sessionId"] = json!(session);
        }

        let (answered, answer) = oneshot::channel();
        self.inner
            .waiting
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(id, answered);

        if self
            .inner
            .outbox
            .send(Message::Text(message.to_string().into()))
            .is_err()
        {
            self.inner.forget(id);
            return Err("the browser is not connected".into());
        }

        match tokio::time::timeout(ANSWER_WITHIN, answer).await {
            Ok(Ok(result)) => result,
            // The waiter was dropped, which happens when the socket died and
            // everybody was told at once.
            Ok(Err(_)) => Err("the browser stopped answering".into()),
            Err(_) => {
                // Nothing is coming. The slot goes, or a connection used for a
                // long session accumulates one dead entry per timed-out call.
                self.inner.forget(id);
                Err(format!(
                    "{method} was not answered in {}s",
                    ANSWER_WITHIN.as_secs()
                ))
            }
        }
    }

    /// Listen to what the browser announces.
    ///
    /// Taken BEFORE the thing that causes the event is asked for, always. A
    /// listener opened afterwards has already missed it, and the wait that
    /// follows is a wait for an event that has been and gone.
    pub fn listen(&self) -> broadcast::Receiver<Event> {
        self.inner.events.subscribe()
    }

    /// Wait for one event on one page.
    pub async fn wait_for(
        &self,
        listening: &mut broadcast::Receiver<Event>,
        session: &str,
        method: &str,
        within: Duration,
    ) -> Result<Value, String> {
        let wanted = async {
            loop {
                match listening.recv().await {
                    Ok(event)
                        if event.method == method && event.session.as_deref() == Some(session) =>
                    {
                        return Ok(event.params)
                    }
                    Ok(_) => continue,
                    // Behind by more than the backlog. Worth saying rather than
                    // waiting on, because the event may have been one of the
                    // ones dropped and this would otherwise wait out its clock.
                    Err(broadcast::error::RecvError::Lagged(by)) => {
                        return Err(format!("fell {by} events behind the browser"))
                    }
                    Err(broadcast::error::RecvError::Closed) => {
                        return Err("the connection to the browser was lost".into())
                    }
                }
            }
        };
        tokio::time::timeout(within, wanted)
            .await
            .map_err(|_| format!("{method} did not happen within {}s", within.as_secs()))?
    }
}

impl Inner {
    /// deliver routes one message from the browser.
    fn deliver(&self, text: &str) {
        let Ok(message) = serde_json::from_str::<Value>(text) else {
            return;
        };

        // An answer, if it carries an id we are waiting on.
        if let Some(id) = message.get("id").and_then(Value::as_u64) {
            let Some(waiting) = self.forget(id) else {
                return;
            };
            let answer = match message.get("error") {
                // The browser's own words. Its `message` is what a person or a
                // model can act on ("Cannot find context with specified id"),
                // where a code alone is something to go and look up.
                Some(error) => Err(error
                    .get("message")
                    .and_then(Value::as_str)
                    .unwrap_or("the browser refused the request")
                    .to_string()),
                None => Ok(message.get("result").cloned().unwrap_or(Value::Null)),
            };
            let _ = waiting.send(answer);
            return;
        }

        // Otherwise it is an event. `send` fails when nobody is listening,
        // which is the ordinary case and not a problem.
        if let Some(method) = message.get("method").and_then(Value::as_str) {
            let _ = self.events.send(Event {
                method: method.to_string(),
                session: message
                    .get("sessionId")
                    .and_then(Value::as_str)
                    .map(str::to_string),
                params: message.get("params").cloned().unwrap_or(Value::Null),
            });
        }
    }

    fn forget(&self, id: u64) -> Option<oneshot::Sender<Result<Value, String>>> {
        self.waiting
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .remove(&id)
    }

    /// give_up tells everybody still waiting that nothing is coming.
    fn give_up(&self, why: &str) {
        let waiting = std::mem::take(&mut *self.waiting.lock().unwrap_or_else(|e| e.into_inner()));
        for (_, answered) in waiting {
            let _ = answered.send(Err(why.to_string()));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A browser that is not there is refused at once, with a reason.
    #[tokio::test]
    async fn a_browser_that_is_not_listening_is_said_plainly() {
        let why = Connection::open("ws://127.0.0.1:1/devtools/browser/nope")
            .await
            .expect_err("nothing is listening on port 1");
        assert!(
            why.contains("would not accept"),
            "the reason should name the problem: {why}"
        );
    }
}
