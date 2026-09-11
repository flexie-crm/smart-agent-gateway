package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
)

// The credential the chat application's Rust half holds.
//
// The page is the only half that knows how to sign in: it has the session
// cookie and the access token in memory, and it keeps them. Rust has neither,
// and asks the page for this instead, over the application's own bridge.
//
// So this endpoint is the whole of Rust's authentication. It is answered on the
// page's ordinary session, mints a token that opens ONE socket, and is called
// again an hour later when Rust says it needs another. There is no second way
// to sign in, no token custody outside the page, and nothing to keep in step.

type linkHandlers struct{ app *app.App }

func mountLink(r chi.Router, a *app.App) {
	// Every edition has a link, the personal one included: its gateway is on the
	// same computer, and the link is what carries a call whose far end is the
	// chat application itself (KB/39). Guarded all the same, because a build
	// without one must not mount a route that would answer nil.
	if a.Link == nil {
		return
	}
	h := &linkHandlers{app: a}
	// No permission beyond being signed in: the link reaches the person's own
	// computer, on their behalf, and what it may then be used FOR is decided by
	// the tools they are granted.
	r.Post("/link/token", h.token)
}

// linkTokenRequest is what the page asks for: a credential for THIS
// installation. The device id is the application's own, minted on the computer
// it runs on; the server only carries it, so that a tool reaching somebody's
// network reaches the computer they are sitting at rather than whichever of
// their machines connected last.
type linkTokenRequest struct {
	DeviceID string `json:"device_id"`
}

type linkTokenBody struct {
	Token string `json:"token"`
	// ExpiresAt is when Rust must have asked for another. It is sent so the
	// application can renew before the deadline rather than discover it.
	ExpiresAt time.Time `json:"expires_at"`
}

func (h *linkHandlers) token(w http.ResponseWriter, r *http.Request) {
	var req linkTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.DeviceID) == "" {
		writeInvalidFields(w, fieldErrors{"device_id": "the application must say which installation it is"})
		return
	}
	claims := claimsFrom(r)
	token, expiresAt, err := h.app.Tokens.IssueLinkToken(
		claims.UserID, claims.WorkspaceID, claims.SessionID, req.DeviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "the link token could not be issued")
		return
	}
	writeJSON(w, http.StatusOK, linkTokenBody{Token: token, ExpiresAt: expiresAt})
}
