package ws

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	// sendBuffer is how many frames a connection may fall behind by before it
	// is dropped. The hub kept nothing on the connection's behalf, so a dropped
	// client just reconnects; a slow socket must never back up the hub.
	sendBuffer = 64
	// authDeadline is how long a fresh socket has to send its authenticate
	// message before it is closed. It never gets to do anything else first.
	authDeadline = 5 * time.Second
	pingInterval = 30 * time.Second
	writeTimeout = 10 * time.Second
)

// conn is one authenticated socket. The hub owns the set of them; conn owns the
// two goroutines that move bytes on and off the wire.
type conn struct {
	id  uint64
	ws  *websocket.Conn
	hub *Hub
	// send carries pre-encoded frames to the write pump.
	send   chan []byte
	cancel context.CancelFunc
	closes sync.Once

	userID      int64
	workspaceID int64
	// source is which app this socket is (chat or console); only chat counts
	// toward presence.
	source string
}

// enqueue hands a frame to the write pump, dropping the connection if it cannot
// keep up rather than stalling the hub. It is called from the hub goroutine, so
// it must never block.
func (c *conn) enqueue(frame []byte) {
	select {
	case c.send <- frame:
	default:
		// This person's socket is dropped, and they are told nothing: the client
		// reconnects and reads the conversation again, so what it missed comes
		// back. It is worth a line all the same, because from the outside it
		// looks exactly like a push that was never sent, and the two are found
		// in completely different places.
		c.hub.log.Warn().Int64("workspace_id", c.workspaceID).Int64("user_id", c.userID).
			Int("behind", cap(c.send)).Msg("socket dropped: the client fell too far behind")
		c.close()
	}
}

// close tears the connection down once: the pumps stop and the socket closes.
func (c *conn) close() {
	c.closes.Do(func() {
		c.cancel()
		_ = c.ws.Close(websocket.StatusNormalClosure, "")
	})
}

// Serve upgrades the request, authenticates the socket on its first message,
// registers it, and runs it until it closes. It is the whole HTTP surface of
// the hub.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request) {
	sock, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.origins})
	if err != nil {
		// Accept has already answered the request (a bad upgrade or origin).
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	c := &conn{
		id:     h.nextID.Add(1),
		ws:     sock,
		hub:    h,
		send:   make(chan []byte, sendBuffer),
		cancel: cancel,
	}
	defer c.close()

	// The first message must authenticate, within the deadline. Nothing else is
	// honoured until it does.
	authCtx, authCancel := context.WithTimeout(ctx, authDeadline)
	var first Envelope
	err = wsjson.Read(authCtx, sock, &first)
	authCancel()
	if err != nil || first.Type != TypeAuthenticate {
		_ = sock.Close(websocket.StatusPolicyViolation, "authenticate first")
		return
	}
	userID, workspaceID, ok := h.validate(first.Token)
	if !ok {
		_ = sock.Close(websocket.StatusPolicyViolation, "invalid token")
		return
	}
	c.userID, c.workspaceID, c.source = userID, workspaceID, first.Source

	// Acknowledge, then start moving frames. The ack sits in the buffer until
	// the write pump starts, which is immediately.
	c.enqueue(ackFrame())
	go c.writePump(ctx)

	h.send(h.register, c)
	defer h.send(h.unregister, c)
	c.readPump(ctx)
}

// readPump reads inbound messages until the socket closes. After authenticate,
// the only messages a client sends are subscribe/unsubscribe; anything else is
// ignored rather than fatal.
func (c *conn) readPump(ctx context.Context) {
	for {
		var env Envelope
		if err := wsjson.Read(ctx, c.ws, &env); err != nil {
			return
		}
		switch env.Type {
		case TypeSubscribe:
			c.hub.sendSub(subRequest{c: c, topic: env.Topic, on: true})
		case TypeUnsubscribe:
			c.hub.sendSub(subRequest{c: c, topic: env.Topic, on: false})
		}
	}
}

// writePump writes outbound frames and pings the peer to keep the socket alive.
// A write or ping that fails or times out closes the connection.
func (c *conn) writePump(ctx context.Context) {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-c.send:
			wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Write(wctx, websocket.MessageText, frame)
			wcancel()
			if err != nil {
				c.close()
				return
			}
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Ping(pctx)
			pcancel()
			if err != nil {
				c.close()
				return
			}
		}
	}
}
