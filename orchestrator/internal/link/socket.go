package link

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// The two sockets, and the auth on them.
//
// Both are dialled BY the chat application, which is what makes any of this work
// from behind a router: nothing here ever connects outwards.

const (
	// authDeadline is how long a fresh control socket has to authenticate. It
	// gets to do nothing else first.
	authDeadline = 5 * time.Second
	pingInterval = 30 * time.Second
	writeTimeout = 10 * time.Second
)

// Validator answers who a link token belongs to, and which installation it was
// minted for. Same shape as the notification hub's, with the device: a route
// into somebody's own network belongs to one computer, not to a person in
// general.
type Validator func(token string) (userID, workspaceID int64, deviceID string, expiresAt time.Time, ok bool)

// hello is the first message on a control socket, and the only kind of message
// the application sends unprompted.
type hello struct {
	Type  string `json:"type"`
	Token string `json:"token"`
	// Runs is what this application can do: each tool it knows, and which
	// version of that tool's arguments it speaks.
	//
	// Declared here rather than discovered, and versioned per tool rather than
	// per application, because the two halves ship separately: somebody's
	// laptop is often a release or two behind the gateway. A tool the machine
	// does not list is not offered that turn, so nothing breaks in the middle
	// of a conversation and nobody is asked to update before they can use the
	// things that do work.
	Runs map[string]int `json:"runs,omitempty"`
}

// reply is what the application says about a request we made: it dialled, or it
// would not.
type reply struct {
	Type   string `json:"type"`
	Ticket string `json:"ticket"`
	Reason string `json:"reason"`
}

// newTicket mints the one-shot secret that joins a dialled-back socket to the
// connection waiting for it. It is never stored and never reused.
func newTicket() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ServeControl runs one chat application's control socket for as long as it is
// there: authenticate, register, then wait. Almost nothing happens on it, which
// is the idea.
func (r *Registry) ServeControl(w http.ResponseWriter, req *http.Request) {
	sock, err := websocket.Accept(w, req, &websocket.AcceptOptions{OriginPatterns: r.origins})
	if err != nil {
		return
	}
	defer func() { _ = sock.CloseNow() }()

	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()

	authCtx, authCancel := context.WithTimeout(ctx, authDeadline)
	var first hello
	err = wsjson.Read(authCtx, sock, &first)
	authCancel()
	if err != nil || first.Type != "authenticate" {
		_ = sock.Close(websocket.StatusPolicyViolation, "authenticate first")
		return
	}
	userID, workspaceID, deviceID, expiresAt, ok := r.validate(first.Token)
	if !ok {
		_ = sock.Close(websocket.StatusPolicyViolation, "invalid token")
		return
	}

	// One writer, because two goroutines writing a websocket is a corrupt
	// frame rather than an error. Every request goes through this lock.
	var writing sync.Mutex
	m := &machine{
		workspaceID: workspaceID,
		userID:      userID,
		deviceID:    deviceID,
		runs:        first.Runs,
		seenAt:      time.Now().UTC(),
		drop:        func() { _ = sock.Close(websocket.StatusPolicyViolation, "the session ended") },
		ask: func(request openRequest) error {
			writing.Lock()
			defer writing.Unlock()
			wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
			defer wcancel()
			return wsjson.Write(wctx, sock, request)
		},
	}

	if err := m.ask(openRequest{Type: "linked"}); err != nil {
		return
	}
	r.add(m)
	defer r.remove(m)

	go r.keepAlive(ctx, sock, &writing)
	go r.watchSession(ctx, m, first.Token, expiresAt)

	// The application answers requests here. A refusal is the only thing worth
	// hearing: a success arrives as a socket on the other endpoint.
	for {
		var answer reply
		if err := wsjson.Read(ctx, sock, &answer); err != nil {
			return
		}
		if answer.Type == "refused" && answer.Ticket != "" {
			r.refuse(answer.Ticket, answer.Reason)
		}
	}
}

// watchSession re-asks whether this link is still allowed, for as long as it is
// open.
//
// The token was checked once, when the socket opened, and a socket lives for as
// long as somebody's laptop stays awake. Everything else in the product
// re-checks a session on every request precisely so that signing out, being
// disabled, or being removed from a workspace takes effect at once (KB/08); a
// route into somebody's own network is the last thing that should be the
// exception. So it is asked again on a clock, and a link that is no longer
// allowed is closed rather than left to expire.
//
// The token's own expiry is part of it: the application renews before then, and
// a renewal is a fresh socket, so a credential that stopped being valid cannot
// outlive this interval either.
func (r *Registry) watchSession(ctx context.Context, m *machine, token string, expiresAt time.Time) {
	ticker := time.NewTicker(recheck)
	defer ticker.Stop()
	asked := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Ask for a new credential BEFORE this one runs out.
			//
			// The application only ever asked after being refused, which is
			// after this side had already closed the socket at expiry: every
			// hour, a gap of seconds in which a tool call in flight failed and
			// the person was told their computer had not answered. Renewing is
			// a reconnection either way; the difference is whether it happens
			// at a moment of the product's choosing or in the middle of
			// somebody's work.
			//
			// Once: a second ask would arrive while the page is already
			// minting, and the application asks again by itself if this one
			// goes unanswered and the credential does expire.
			if !asked && !expiresAt.IsZero() && time.Now().After(expiresAt.Add(-renewBefore)) {
				asked = true
				r.log.Info().Int64("workspace_id", m.workspaceID).Int64("user_id", m.userID).
					Str("device_id", m.deviceID).Time("expires_at", expiresAt).
					Msg("asking the chat application to renew its credential")
				if err := m.ask(openRequest{Type: "renew"}); err != nil {
					return
				}
			}
			if _, _, _, _, ok := r.validate(token); !ok {
				r.log.Info().Int64("workspace_id", m.workspaceID).Int64("user_id", m.userID).
					Msg("a link was closed: its session is no longer valid")
				m.drop()
				return
			}
		}
	}
}

// keepAlive pings, so a machine that has silently gone (a closed laptop, a
// dropped VPN) is noticed and taken out of the registry rather than being
// offered to a tool that will then wait for a dial-back that cannot come.
func (r *Registry) keepAlive(ctx context.Context, sock *websocket.Conn, writing *sync.Mutex) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			writing.Lock()
			pctx, pcancel := context.WithTimeout(ctx, writeTimeout)
			err := sock.Ping(pctx)
			pcancel()
			writing.Unlock()
			if err != nil {
				_ = sock.CloseNow()
				return
			}
		}
	}
}

// ServeStream takes a dialled-back socket and hands it to the connection
// waiting for its ticket.
//
// Two things are checked, and the second one is why this is not just a ticket.
//
// The ticket is a one-shot secret: minted seconds ago for one connection, sent
// to one machine over an authenticated socket, spent by this handler. That is
// strong on its own, and it was all this asked for. What it does not say is WHO
// is presenting it. A ticket that ever escaped the socket it was sent on (a log
// line, an error report, a proxy in between) could be redeemed by anybody who
// had it, and whoever redeemed it would BECOME the far end of that connection:
// they would receive the database traffic and could answer it with anything
// they liked.
//
// So the socket authenticates like every other one here, with the link token as
// its first message, and the ticket must have been issued to that same person.
// Holding the ticket is then not enough; you have to be the machine it was sent
// to.
func (r *Registry) ServeStream(w http.ResponseWriter, req *http.Request) {
	ticket := req.URL.Query().Get("ticket")
	if ticket == "" {
		http.Error(w, "a ticket is required", http.StatusBadRequest)
		return
	}
	sock, err := websocket.Accept(w, req, &websocket.AcceptOptions{OriginPatterns: r.origins})
	if err != nil {
		return
	}

	authCtx, authCancel := context.WithTimeout(req.Context(), authDeadline)
	var first hello
	err = wsjson.Read(authCtx, sock, &first)
	authCancel()
	if err != nil || first.Type != "authenticate" {
		_ = sock.Close(websocket.StatusPolicyViolation, "authenticate first")
		return
	}
	userID, workspaceID, deviceID, _, ok := r.validate(first.Token)
	if !ok {
		_ = sock.Close(websocket.StatusPolicyViolation, "invalid token")
		return
	}

	// The socket becomes an ordinary net.Conn, and everything above it is the
	// driver's business. Its context is the connection's own life, not the
	// request's: the request returns as soon as this handler does, and the
	// carried connection outlives it.
	conn := newStream(context.WithoutCancel(req.Context()), sock)

	if !r.deliver(ticket, conn, workspaceID, userID, deviceID) {
		conn.cancel()
		_ = sock.Close(websocket.StatusPolicyViolation, "unknown or spent ticket")
		return
	}
	// Hold the handler open: returning would close the socket under the
	// connection we just handed over.
	<-conn.ctx.Done()
}
