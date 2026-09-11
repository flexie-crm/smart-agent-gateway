package ws

import (
	"context"
	"sync/atomic"

	"github.com/rs/zerolog"
)

// Validator turns a token into an identity, or reports it invalid. It is the
// same check the REST middleware runs; the socket just receives the token in
// its first message instead of a header.
type Validator func(token string) (userID, workspaceID int64, ok bool)

// Hub owns every connection and is the ONLY goroutine that touches that state,
// so it needs no locks: register, unregister, subscribe, notify, count, and
// broadcast all arrive as messages on channels and are served in one select
// loop. The connections' own read/write pumps do the socket I/O on their own
// goroutines, so the hub only ever routes, never blocks on the network.
//
// The hub does not itself decide WHAT a live topic carries or WHEN to push it.
// It routes: it tracks who is connected and who subscribes to which topic,
// answers "how many are connected", and delivers a payload to a topic's
// subscribers. Composing the live numbers and coalescing the pushes is the app's
// live-dashboard publisher, off this goroutine, so a database read for the
// snapshot never blocks the hub (KB/24).
type Hub struct {
	log      zerolog.Logger
	validate Validator
	origins  []string

	register   chan *conn
	unregister chan *conn
	subscribe  chan subRequest
	notify     chan notifyRequest
	count      chan countRequest
	broadcast  chan broadcastRequest
	done       chan struct{}

	// onChange is called (on the hub goroutine) whenever the live picture a
	// dashboard shows may have shifted: a connection opened or closed, or a
	// dashboard just subscribed and needs its first snapshot. It must not block;
	// the app wires it to a non-blocking event publish. Set before Run.
	onChange func(workspaceID int64)

	nextID atomic.Uint64
}

type subRequest struct {
	c     *conn
	topic string
	on    bool
}

type notifyRequest struct {
	workspaceID int64
	userID      int64
	message     any
}

type countRequest struct {
	workspaceID int64
	reply       chan Presence
}

type broadcastRequest struct {
	workspaceID int64
	topic       string
	frame       []byte
}

// NewHub builds a hub. originPatterns are the host patterns a browser may open
// the socket from (the dev console's origins); production is same-origin.
func NewHub(log zerolog.Logger, validate Validator, originPatterns []string) *Hub {
	return &Hub{
		log:        log,
		validate:   validate,
		origins:    originPatterns,
		register:   make(chan *conn),
		unregister: make(chan *conn),
		subscribe:  make(chan subRequest),
		// Buffered so a caller pushing a notification is never blocked by the
		// hub being briefly busy; a full buffer drops rather than stalls.
		notify:    make(chan notifyRequest, 1024),
		count:     make(chan countRequest),
		broadcast: make(chan broadcastRequest, 1024),
		done:      make(chan struct{}),
	}
}

// OnChange registers the callback fired when the live dashboard picture may have
// shifted. It must be set before Run and must not block.
func (h *Hub) OnChange(fn func(workspaceID int64)) { h.onChange = fn }

// Run owns the hub's state until ctx is cancelled, then closes every connection
// so the server can shut down cleanly.
func (h *Hub) Run(ctx context.Context) {
	// workspace -> user -> connID -> conn. len(byWS[ws]) is the distinct-user
	// count; the inner maps give the connections to send to.
	byWS := map[int64]map[int64]map[uint64]*conn{}
	// topic -> workspace -> connID -> conn: who is watching which live topic.
	subs := map[string]map[int64]map[uint64]*conn{}

	defer close(h.done)

	changed := func(ws int64) {
		if h.onChange != nil {
			h.onChange(ws)
		}
	}

	add := func(c *conn) {
		users := byWS[c.workspaceID]
		if users == nil {
			users = map[int64]map[uint64]*conn{}
			byWS[c.workspaceID] = users
		}
		conns := users[c.userID]
		if conns == nil {
			conns = map[uint64]*conn{}
			users[c.userID] = conns
		}
		conns[c.id] = c
	}
	remove := func(c *conn) {
		users := byWS[c.workspaceID]
		if users != nil {
			if conns := users[c.userID]; conns != nil {
				delete(conns, c.id)
				if len(conns) == 0 {
					delete(users, c.userID)
				}
			}
			if len(users) == 0 {
				delete(byWS, c.workspaceID)
			}
		}
		// A dropped socket leaves no subscription behind, on any topic.
		for topic, byWSsub := range subs {
			if ws := byWSsub[c.workspaceID]; ws != nil {
				delete(ws, c.id)
				if len(ws) == 0 {
					delete(byWSsub, c.workspaceID)
				}
			}
			if len(byWSsub) == 0 {
				delete(subs, topic)
			}
		}
	}
	// countOf counts only chat connections: a person is online when they are in
	// the chat, so a console watching the dashboard is not itself counted.
	countOf := func(ws int64) Presence {
		people, conns := 0, 0
		for _, cs := range byWS[ws] {
			chat := 0
			for _, c := range cs {
				if c.source == SourceChat {
					chat++
				}
			}
			if chat > 0 {
				people++
				conns += chat
			}
		}
		return Presence{WorkspaceID: ws, Users: people, Connections: conns}
	}

	for {
		select {
		case <-ctx.Done():
			for _, users := range byWS {
				for _, conns := range users {
					for _, c := range conns {
						c.close()
					}
				}
			}
			return

		case c := <-h.register:
			add(c)
			changed(c.workspaceID)

		case c := <-h.unregister:
			remove(c)
			changed(c.workspaceID)

		case s := <-h.subscribe:
			byWSsub := subs[s.topic]
			if s.on {
				if byWSsub == nil {
					byWSsub = map[int64]map[uint64]*conn{}
					subs[s.topic] = byWSsub
				}
				conns := byWSsub[s.c.workspaceID]
				if conns == nil {
					conns = map[uint64]*conn{}
					byWSsub[s.c.workspaceID] = conns
				}
				conns[s.c.id] = s.c
				// A newly-subscribed dashboard needs its first snapshot; ask the
				// publisher to push one for this workspace.
				changed(s.c.workspaceID)
			} else if byWSsub != nil {
				if conns := byWSsub[s.c.workspaceID]; conns != nil {
					delete(conns, s.c.id)
					if len(conns) == 0 {
						delete(byWSsub, s.c.workspaceID)
					}
				}
				if len(byWSsub) == 0 {
					delete(subs, s.topic)
				}
			}

		case n := <-h.notify:
			if users := byWS[n.workspaceID]; users != nil {
				pre := notificationFrame(n.message)
				for _, c := range users[n.userID] {
					c.enqueue(pre)
				}
			}

		case req := <-h.count:
			req.reply <- countOf(req.workspaceID)

		case b := <-h.broadcast:
			if conns := subs[b.topic][b.workspaceID]; conns != nil {
				for _, c := range conns {
					c.enqueue(b.frame)
				}
			}
		}
	}
}

// Notify pushes a message to every socket a person holds in a workspace. It is
// safe to call from anywhere and never blocks the caller: a hub too backed up to
// accept the notification drops it (and says so), because a dropped notification
// is a smaller problem than a stalled request path.
func (h *Hub) Notify(workspaceID, userID int64, message any) {
	select {
	case h.notify <- notifyRequest{workspaceID: workspaceID, userID: userID, message: message}:
	case <-h.done:
	default:
		h.log.Warn().Int64("workspace_id", workspaceID).Int64("user_id", userID).
			Msg("ws notify dropped: hub is backed up")
	}
}

// Connected reports the distinct people and sockets connected to a workspace's
// chat, at this moment. It is a synchronous read served on the hub goroutine, so
// it is exact and lock-free; the live-dashboard publisher calls it while building
// a snapshot.
func (h *Hub) Connected(workspaceID int64) Presence {
	reply := make(chan Presence, 1)
	select {
	case h.count <- countRequest{workspaceID: workspaceID, reply: reply}:
	case <-h.done:
		return Presence{WorkspaceID: workspaceID}
	}
	select {
	case p := <-reply:
		return p
	case <-h.done:
		return Presence{WorkspaceID: workspaceID}
	}
}

// Broadcast delivers a payload to every socket subscribed to a topic in a
// workspace. Like Notify it never blocks: a full buffer drops the push, and the
// next one (or a reload) carries the current state. The payload is encoded once
// here, off the hub goroutine.
func (h *Hub) Broadcast(workspaceID int64, topic string, message any) {
	pre := topicFrame(topic, message)
	select {
	case h.broadcast <- broadcastRequest{workspaceID: workspaceID, topic: topic, frame: pre}:
	case <-h.done:
	default:
		h.log.Warn().Int64("workspace_id", workspaceID).Str("topic", topic).
			Msg("ws broadcast dropped: hub is backed up")
	}
}

// send delivers a control message to the hub goroutine, giving up if the hub has
// stopped so a connection tearing down at shutdown cannot block forever.
func (h *Hub) send(ch chan *conn, c *conn) {
	select {
	case ch <- c:
	case <-h.done:
	}
}

func (h *Hub) sendSub(s subRequest) {
	select {
	case h.subscribe <- s:
	case <-h.done:
	}
}
