//! The machine link: a route from the gateway into this computer's network.
//!
//! A customer's older systems are often not on a network a server can reach. An
//! accounts database on a machine in an office, a server on an isolated
//! network: there is no address the gateway could be given for those. This
//! computer can see them, and it is already talking to the gateway, so it
//! carries the connection.
//!
//! What this half does is small, and deliberately so. It holds one socket open
//! and waits. When the gateway asks for a connection, it opens an ordinary TCP
//! connection to the address on this network, opens a second socket back to the
//! gateway, and copies bytes between the two until one of them ends.
//!
//! It understands nothing about what it carries. A database connection
//! negotiates its own encryption with the database, an SSH session verifies its
//! own host key: both do that THROUGH this, end to end, so what passes here is
//! ciphertext and this half holds no certificate and terminates no TLS. The
//! only TLS it has is its own, to the gateway.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio_tungstenite::tungstenite::Message;
use tokio_tungstenite::MaybeTlsStream;

/// How long to wait before dialling the control socket again, and the ceiling
/// it backs off to. A gateway that is restarting is back in seconds; one that
/// is gone for the afternoon must not be asked every second all afternoon.
const RECONNECT_MIN: Duration = Duration::from_secs(1);
const RECONNECT_MAX: Duration = Duration::from_secs(30);

/// How long a connection to something on this network may take to open. It is a
/// person waiting on an answer at the other end of it.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);

/// The most a call's arguments may be. The gateway caps what it sends and this
/// caps what is kept, because a cap that exists on one side only is a cap that
/// protects the wrong half.
const MAX_CALL: usize = 4 << 20;

/// The event the page listens for. Rust cannot sign in; when its credential is
/// about to expire, or was refused, it asks the window for another.
pub const NEEDS_TOKEN: &str = "link://needs-token";

/// The event that says whether the route is up, emitted when it changes.
///
/// A person can see whether their assistant can reach their own network, and
/// the alternative to telling them is a tool that fails for a reason nobody in
/// the room can see. The page shows it on the workspace, quietly.
pub const STATE_CHANGED: &str = "link://state";

/// What the gateway sends on the control socket.
#[derive(Debug, Deserialize)]
struct Request {
    // What the message is: "open", or "linked". Named for the field it reads,
    // because the one below is also a kind and mixing them up is a bug that
    // compiles.
    #[serde(rename = "type")]
    message: String,
    #[serde(default)]
    ticket: String,
    // What the stream is FOR: a connection to something on this network, or a
    // tool running on this computer. Absent means a connection, which is what
    // it always was before there were tools.
    #[serde(default)]
    kind: String,
    #[serde(default)]
    host: String,
    #[serde(default)]
    port: u16,
}

/// What this half sends back: it authenticates once, and afterwards only ever
/// reports that it could not do something.
#[derive(Debug, Serialize)]
struct Reply<'a> {
    #[serde(rename = "type")]
    kind: &'a str,
    #[serde(skip_serializing_if = "str::is_empty")]
    token: &'a str,
    #[serde(skip_serializing_if = "str::is_empty")]
    ticket: &'a str,
    #[serde(skip_serializing_if = "str::is_empty")]
    reason: &'a str,
    // What this build can do, sent once when the link opens. The gateway
    // offers only what is in it, so an application a release or two behind is
    // not offered a tool it has never heard of.
    #[serde(skip_serializing_if = "Option::is_none")]
    runs: Option<serde_json::Map<String, serde_json::Value>>,
}

/// The link's state: where the gateway is, the credential the page last handed
/// down, and whether a task is already running.
///
/// It knows nothing about a window. What it does when it needs a fresh
/// credential is a function it was given, which in the application emits an
/// event to the page and in a test records that it was asked. That is not
/// tidiness: it is what lets the REAL client be driven against a real gateway
/// by a test, which is the only way any of this is more than a claim.
pub struct Link {
    ask_for_token: Box<dyn Fn() + Send + Sync>,
    base: std::sync::Mutex<String>,
    token: std::sync::Mutex<String>,
    running: AtomicBool,
    /// Told when the credential this connection is using has been replaced, so
    /// the loop comes back with the new one INSTEAD OF waiting to be refused.
    ///
    /// A credential lasts an hour and the gateway asks for a fresh one before
    /// then; handing it down is not enough by itself, because the socket that
    /// is open was opened with the old one and the gateway closes it when that
    /// runs out. Reconnecting on the new credential is what turns an outage in
    /// the middle of somebody's work into a reconnection at a moment of our
    /// choosing.
    replaced: tokio::sync::Notify,
    /// Told when a credential arrives, so a loop that is backing off after a
    /// refusal stops waiting and uses it.
    credential: tokio::sync::Notify,
    // up is whether the gateway has accepted us and the socket is still there.
    // Read by the window, so somebody can see that the route to their own
    // network is working without having to try something that uses it.
    up: AtomicBool,
    // announce is how a change in that reaches the window.
    announce: Box<dyn Fn(bool) + Send + Sync>,
    // held is the task holding the control socket. Kept so that stopping can
    // END it: clearing the credential only stops the NEXT connection, and the
    // one already open would have stayed open until somebody closed a laptop.
    held: std::sync::Mutex<Option<tauri::async_runtime::JoinHandle<()>>>,
}

impl Link {
    /// Build a link that asks the given function when it needs a credential,
    /// and tells the second one whenever the route comes up or goes down.
    pub fn new(
        ask_for_token: impl Fn() + Send + Sync + 'static,
        announce: impl Fn(bool) + Send + Sync + 'static,
    ) -> Arc<Self> {
        Arc::new(Self {
            ask_for_token: Box::new(ask_for_token),
            announce: Box::new(announce),
            up: AtomicBool::new(false),
            base: std::sync::Mutex::new(String::new()),
            token: std::sync::Mutex::new(String::new()),
            running: AtomicBool::new(false),
            replaced: tokio::sync::Notify::new(),
            credential: tokio::sync::Notify::new(),
            held: std::sync::Mutex::new(None),
        })
    }

    /// say records what the link is doing, where somebody can see it.
    ///
    /// Not a logging framework, and not silence either. This half runs with no
    /// window of its own and nothing else reports on it, so a link that never
    /// connects looks from the outside exactly like a link nobody asked for:
    /// the server sees no request, the page sees no error, and the only thing
    /// left is to guess. One line per event, on stderr, which the shell keeps.
    fn say(&self, what: &str) {
        eprintln!("link: {what}");
    }

    /// Start, or hand a fresh credential to a link that is already running.
    ///
    /// Called by the page: once when it signs in, and again whenever this half
    /// says it needs another token. Starting twice does not start twice.
    pub fn start(self: &Arc<Self>, base: String, token: String) {
        let fresh = *held(&self.token) != token;
        *held(&self.base) = base;
        *held(&self.token) = token;
        if self.running.swap(true, Ordering::SeqCst) {
            if fresh {
                // A DIFFERENT credential, for a link that is already up: the
                // open socket is still using the old one and the gateway will
                // close it when that expires. Come back now, on the new one.
                self.say("a replacement credential: reconnecting on it");
                self.replaced.notify_waiters();
            }
            self.say("a fresh credential, for the link already running");
            // And it is used NOW, rather than whenever the backoff happens to
            // wake up. A refused credential is the common reason to be
            // reconnecting at all, so the loop is usually asleep with a dead
            // token in its hand: it asked the page for another and then waited
            // out a doubling that reached thirty seconds. One person watched
            // eighty five seconds of "not connected" while a valid credential
            // sat here.
            self.credential.notify_waiters();
            return;
        }
        self.say("starting");
        let link = Arc::clone(self);
        *held(&self.held) = Some(tauri::async_runtime::spawn(async move { link.hold().await }));
    }

    /// Stop, and mean it.
    ///
    /// Signing out has to reach this half, and forgetting the credential is not
    /// enough: that only stops the NEXT connection, and the one already open
    /// would have carried on until the gateway or the network ended it. So the
    /// task is ended, which drops the socket with it.
    pub fn stop(&self) {
        held(&self.token).clear();
        if let Some(task) = held(&self.held).take() {
            task.abort();
        }
        self.running.store(false, Ordering::SeqCst);
        self.set_up(false);
    }

    /// Whether the route is up right now, for a window that has just opened and
    /// missed whatever was announced before it did.
    pub fn is_up(&self) -> bool {
        self.up.load(Ordering::SeqCst)
    }

    /// set_up records the state and announces a CHANGE. Announcing every time
    /// would put a message on the bridge for every reconnection attempt while a
    /// gateway is down, which is a lot of noise saying the same thing.
    fn set_up(&self, up: bool) {
        if self.up.swap(up, Ordering::SeqCst) != up {
            (self.announce)(up);
        }
    }

    /// hold keeps one control socket open for as long as there is a credential,
    /// reconnecting when it drops.
    async fn hold(self: Arc<Self>) {
        let mut wait = RECONNECT_MIN;
        loop {
            let (base, token) = (held(&self.base).clone(), held(&self.token).clone());
            if token.is_empty() {
                self.say("stopped: there is no credential");
                self.running.store(false, Ordering::SeqCst);
                return; // signed out
            }
            match self.serve(&base, &token).await {
                // A socket that lived and then closed is an ordinary event (the
                // gateway restarted, the network moved): try again at once.
                Ok(()) => wait = RECONNECT_MIN,
                Err(refused) => {
                    // A refusal is usually a credential that has expired, and
                    // the page is the only half that can mint another.
                    if refused {
                        (self.ask_for_token)();
                    }
                    // Whichever comes first: the backoff, or somebody handing
                    // down a credential. A new one makes the wait pointless.
                    tokio::select! {
                        _ = tokio::time::sleep(wait) => {
                            wait = (wait * 2).min(RECONNECT_MAX);
                        }
                        _ = self.credential.notified() => {
                            self.say("a credential arrived; connecting now");
                            wait = RECONNECT_MIN;
                        }
                    }
                }
            }
        }
    }

    /// serve holds one control socket. Ok means it closed; Err(true) means the
    /// gateway refused the credential.
    ///
    /// One writer, on a channel. A websocket written by two tasks at once is a
    /// corrupt frame rather than an error, and a refusal has to go back on THIS
    /// socket: it is the authenticated one, and a second connection would have
    /// to sign in again to say one sentence.
    async fn serve(&self, base: &str, token: &str) -> Result<(), bool> {
        // Whether the gateway ever accepted us. A socket that closes having
        // never said "linked" is a refused credential, and the page is asked for
        // another; one that closes after a day of work is a gateway restarting,
        // and asking for a fresh token every time that happens is noise that
        // hides the real thing.
        let mut accepted = false;
        let url = format!("{}/v1/link", socket_base(base));
        self.say(&format!("connecting to {url}"));
        let (socket, _) = tokio_tungstenite::connect_async(&url).await.map_err(|err| {
            self.say(&format!("could not connect: {err}"));
            false
        })?;
        promptly(&socket);
        let (mut out, mut socket) = socket.split();

        let hello = Reply {
            kind: "authenticate",
            token,
            ticket: "",
            reason: "",
            runs: Some(crate::tools::runs()),
        };
        out.send(Message::Text(encode(&hello).into()))
            .await
            .map_err(|_| false)?;

        let (replies, mut waiting) = tokio::sync::mpsc::unbounded_channel::<String>();
        let writer = tauri::async_runtime::spawn(async move {
            while let Some(text) = waiting.recv().await {
                if out.send(Message::Text(text.into())).await.is_err() {
                    return;
                }
            }
        });

        // Why this connection ended, when it was the socket rather than us: Some
        // means it came apart, and the bool is whether the credential was
        // refused. Set in the loop and answered after the ending below has run.
        let mut ended: Option<bool> = None;
        loop {
            let frame = tokio::select! {
                frame = socket.next() => frame,
                // The page handed down a new credential. End this connection so
                // the loop dials again with it, rather than holding a socket
                // the gateway is about to close.
                _ = self.replaced.notified() => {
                    self.say("reconnecting with the replacement credential");
                    break;
                }
            };
            let Some(frame) = frame else { break };
            let text = match frame {
                Ok(Message::Text(text)) => text,
                // The socket ended. NOT an early return: everything below this
                // loop is how the rest of the application learns the link is
                // down, and skipping it left the indicator green through an
                // outage and the page never told to do anything about it.
                Ok(Message::Close(_)) | Err(_) => {
                    ended = Some(!accepted);
                    break;
                }
                Ok(_) => continue,
            };
            let request: Request = match serde_json::from_str(&text) {
                Ok(request) => request,
                Err(_) => continue,
            };
            if request.message == "linked" {
                accepted = true;
                self.say("linked");
                self.set_up(true);
                continue;
            }
            if request.message == "renew" {
                // The gateway says this credential is nearly out. Ask the page
                // for another BEFORE it expires, rather than after being
                // refused: a refusal comes after the gateway has already closed
                // the socket, which is a gap of seconds in the middle of
                // whatever was running. The page mints one and hands it down
                // (start), and that is where the reconnection happens.
                self.say("the gateway asked for a fresh credential");
                (self.ask_for_token)();
                continue;
            }
            if request.message != "open" {
                continue; // anything a later gateway adds
            }

            // Each connection is its own task and its own socket, so a slow one
            // cannot hold up the next request or anybody else's traffic.
            let base = base.to_string();
            let token = token.to_string();
            let replies = replies.clone();
            tauri::async_runtime::spawn(async move { carry(base, token, request, replies).await });
        }
        drop(replies);
        writer.abort();
        self.set_up(false);
        self.say(if accepted { "the link closed" } else { "the credential was refused" });
        if let Some(refused) = ended {
            return Err(refused);
        }
        if accepted {
            Ok(())
        } else {
            Err(true)
        }
    }
}

/// encode is a reply as the gateway reads it. A reply that will not encode
/// cannot be fixed by anything here, and an empty string is a frame the gateway
/// ignores, which is the same outcome as not sending one.
fn encode(reply: &Reply<'_>) -> String {
    serde_json::to_string(reply).unwrap_or_default()
}

/// carry opens the connection the gateway asked for and pipes it back.
///
/// The refusal path matters as much as the working one: a database that is off,
/// an address that does not resolve, a machine that is not on this network at
/// all. Saying so gives the person the reason their computer gave, instead of
/// the gateway waiting out a deadline and reporting a timeout.
async fn carry(
    base: String,
    token: String,
    request: Request,
    replies: tokio::sync::mpsc::UnboundedSender<String>,
) {
    // A tool call and a connection are the same mechanism with different far
    // ends: one ends at something on this network, the other at this computer.
    if request.kind == "call" {
        answer(base, token, request).await;
        return;
    }
    let address = format!("{}:{}", request.host, request.port);
    let opened = tokio::time::timeout(CONNECT_TIMEOUT, TcpStream::connect(&address)).await;

    let tcp = match opened {
        Ok(Ok(tcp)) => {
            // A query is a small write and then a wait. Left to Nagle, that
            // write sits in a buffer waiting for company while the far side's
            // delayed acknowledgement waits for the write: tens of milliseconds
            // added to every round trip, on a route that already has one more
            // hop than usual.
            let _ = tcp.set_nodelay(true);
            tcp
        }
        Ok(Err(err)) => return refuse(&replies, &request.ticket, &err.to_string()),
        Err(_) => return refuse(&replies, &request.ticket, "it did not answer in time"),
    };

    // The socket says who it is, like the control one does. The ticket alone
    // would let anybody who ever saw one take delivery of the connection it was
    // issued for, and whoever took it would BE the far end of a database
    // session. Holding the ticket is not enough; you have to be the machine it
    // was sent to.
    let Some(socket) = open_stream(&base, &token, &request.ticket).await else {
        return; // the gateway will time the ticket out
    };
    pipe(tcp, socket).await;
}

/// answer runs a tool on this computer and writes what it found.
///
/// The request arrives on the stream rather than in the message that asked for
/// it, so the control socket stays small whatever somebody is writing to a
/// file, and the answer goes back the same way.
async fn answer(base: String, token: String, request: Request) {
    let Some(mut socket) = open_stream(&base, &token, &request.ticket).await else {
        return; // the gateway will time the ticket out
    };

    // Everything until the gateway says it has finished sending. It says so by
    // half-closing, which arrives here as an empty binary message.
    let mut raw = Vec::new();
    while let Some(frame) = socket.next().await {
        match frame {
            Ok(Message::Binary(bytes)) if bytes.is_empty() => break,
            Ok(Message::Binary(bytes)) => {
                if raw.len() + bytes.len() > MAX_CALL {
                    break;
                }
                raw.extend_from_slice(&bytes);
            }
            Ok(Message::Close(_)) | Err(_) => break,
            Ok(_) => {}
        }
    }

    let response = match serde_json::from_slice::<crate::tools::Request>(&raw) {
        Ok(call) => crate::tools::run(call).await,
        // Unreadable arguments are the gateway's problem and not the
        // assistant's, but the assistant is the only one who can act on it.
        Err(err) => crate::tools::Response::bad_arguments(format!(
            "the arguments could not be read: {err}"
        )),
    };
    let body = serde_json::to_vec(&response).unwrap_or_default();
    let _ = socket.send(Message::Binary(body.into())).await;
    let _ = socket.close(None).await;
}

/// open_stream dials one socket back and says who it is, which is the same two
/// steps whatever the socket then carries.
async fn open_stream(
    base: &str,
    token: &str,
    ticket: &str,
) -> Option<tokio_tungstenite::WebSocketStream<MaybeTlsStream<TcpStream>>> {
    let url = format!("{}/v1/link/stream?ticket={}", socket_base(base), ticket);
    let mut socket = match tokio_tungstenite::connect_async(&url).await {
        Ok((socket, _)) => {
            promptly(&socket);
            socket
        }
        Err(_) => return None,
    };
    let hello = Reply { kind: "authenticate", token, ticket: "", reason: "", runs: None };
    if socket.send(Message::Text(encode(&hello).into())).await.is_err() {
        return None;
    }
    Some(socket)
}

/// pipe copies in both directions until BOTH are finished.
///
/// Binary frames, unread: this half is a pipe and the bytes are a database
/// protocol or an SSH session, neither of which is its business.
///
/// What it does understand is the end of a direction. TCP lets one side finish
/// while the other keeps going, and protocols are built on it: a client says
/// "that is my whole request" and waits for the answer. A websocket has no such
/// thing, so an EMPTY binary message carries it. Receiving one shuts down this
/// end's writing half and leaves it reading; sending one says the same the other
/// way. Closing the socket to mean it would take the answer with it.
async fn pipe(
    tcp: TcpStream,
    socket: tokio_tungstenite::WebSocketStream<
        tokio_tungstenite::MaybeTlsStream<TcpStream>,
    >,
) {
    let (mut reader, mut writer) = tcp.into_split();
    let (mut out, mut incoming) = socket.split();

    let upstream = async move {
        let mut buffer = vec![0u8; 64 * 1024];
        loop {
            match reader.read(&mut buffer).await {
                // The service has finished sending. Say so, and leave the socket
                // open: the request coming the other way may not be over.
                Ok(0) => {
                    let _ = out.send(Message::Binary(Vec::new().into())).await;
                    break;
                }
                Err(_) => break,
                Ok(n) => {
                    if out.send(Message::Binary(buffer[..n].to_vec().into())).await.is_err() {
                        break;
                    }
                }
            }
        }
    };

    let downstream = async move {
        while let Some(frame) = incoming.next().await {
            match frame {
                // An empty message is the gateway saying its end has finished
                // sending. The service is told the way a socket tells it, and
                // this end keeps reading whatever it answers.
                Ok(Message::Binary(bytes)) if bytes.is_empty() => {
                    let _ = writer.shutdown().await;
                }
                Ok(Message::Binary(bytes)) => {
                    if writer.write_all(&bytes).await.is_err() {
                        break;
                    }
                }
                Ok(Message::Close(_)) | Err(_) => break,
                Ok(_) => {}
            }
        }
        let _ = writer.shutdown().await;
    };

    tokio::join!(upstream, downstream);
}

/// refuse tells the gateway why this computer would not open the connection, so
/// the person reads what their own machine said ("connection refused") instead
/// of the gateway waiting out its deadline and reporting a timeout.
fn refuse(replies: &tokio::sync::mpsc::UnboundedSender<String>, ticket: &str, reason: &str) {
    let reply = Reply { kind: "refused", token: "", ticket, reason, runs: None };
    let _ = replies.send(encode(&reply));
}

/// held takes a lock, recovering one poisoned by a panic elsewhere.
///
/// A panic while holding it left the value untouched (these are two strings),
/// so the alternative is a second panic that says nothing about the first, in a
/// task whose job is to keep a connection open.
fn held<T>(lock: &std::sync::Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    lock.lock().unwrap_or_else(|poisoned| poisoned.into_inner())
}

/// promptly turns off Nagle on a websocket's own socket.
///
/// Nagle holds a small write back, waiting for more to send with it, while the
/// far side's delayed acknowledgement waits for the write: the two wait for each
/// other and a round trip that should cost microseconds costs the best part of a
/// millisecond. A database conversation is thousands of small round trips, so
/// that is the difference between a route and a slow route, and the one being
/// added here is a route for databases.
///
/// It was set on the connection to the database and not on these, which is half
/// the path. Measuring the round trip is what found it: 795 microseconds added
/// per query on loopback, where the hop itself is worth tens.
fn promptly(socket: &tokio_tungstenite::WebSocketStream<MaybeTlsStream<TcpStream>>) {
    match socket.get_ref() {
        MaybeTlsStream::Plain(tcp) => {
            let _ = tcp.set_nodelay(true);
        }
        MaybeTlsStream::Rustls(tls) => {
            let _ = tls.get_ref().0.set_nodelay(true);
        }
        _ => {}
    }
}

/// socket_base turns the address of a gateway into the address of its sockets.
/// The scheme is the only difference, and getting it wrong is a connection that
/// never opens with nothing to read about why.
fn socket_base(base: &str) -> String {
    let trimmed = base.trim_end_matches('/');
    if let Some(rest) = trimmed.strip_prefix("https://") {
        format!("wss://{rest}")
    } else if let Some(rest) = trimmed.strip_prefix("http://") {
        format!("ws://{rest}")
    } else {
        format!("wss://{trimmed}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // Where a gateway's sockets are, given where the gateway is. Getting this
    // wrong is a connection that never opens, with nothing to read about why:
    // the scheme is the only difference between the two addresses.
    #[test]
    fn a_gateways_address_becomes_its_socket_address() {
        assert_eq!(socket_base("https://sag.example.com"), "wss://sag.example.com");
        assert_eq!(socket_base("http://localhost:8080"), "ws://localhost:8080");
        // A trailing slash is somebody's typing, not a different server.
        assert_eq!(socket_base("https://sag.example.com/"), "wss://sag.example.com");
        // No scheme is the secure one. The alternative is that a mistyped
        // address quietly downgrades a route into somebody's own network.
        assert_eq!(socket_base("sag.example.com"), "wss://sag.example.com");
    }

    // A reply carries only what it is about. The gateway matches a refusal by
    // its ticket, and an empty field would be a field it has to interpret.
    #[test]
    fn a_refusal_carries_its_ticket_and_its_reason() {
        let reply = Reply {
            kind: "refused",
            token: "",
            ticket: "abc",
            reason: "connection refused",
            runs: None,
        };
        let encoded = encode(&reply);
        assert!(encoded.contains(r#""type":"refused""#));
        assert!(encoded.contains(r#""ticket":"abc""#));
        assert!(encoded.contains(r#""reason":"connection refused""#));
        // The credential is not in it. It is sent once, to authenticate, and
        // repeating it in every message would put it in every log.
        assert!(!encoded.contains("token"));
    }

    // What the gateway asks for, read the way this half reads it. A port that
    // arrives as anything but a number, or a field this build does not know,
    // must not stop the request being understood.
    #[test]
    fn a_request_is_read_from_what_the_gateway_sends() {
        let request: Request = serde_json::from_str(
            r#"{"type":"open","ticket":"t1","host":"10.0.0.5","port":3306,"reason":"a query","future":"ignored"}"#,
        )
        .expect("the gateway's request could not be read");
        assert_eq!(request.message, "open");
        assert_eq!(request.ticket, "t1");
        assert_eq!(request.host, "10.0.0.5");
        assert_eq!(request.port, 3306);
    }

    // The acknowledgement carries nothing else, and must still be readable:
    // it is what tells this half it is linked rather than refused.
    #[test]
    fn the_acknowledgement_is_read_without_the_rest() {
        let request: Request =
            serde_json::from_str(r#"{"type":"linked"}"#).expect("the acknowledgement is unreadable");
        assert_eq!(request.message, "linked");
        assert!(request.ticket.is_empty());
        assert_eq!(request.port, 0);
    }
}
