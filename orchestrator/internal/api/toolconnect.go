package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
)

// Connecting a custom tool to a service that signs a PERSON in.
//
// Two routes and they are authenticated differently, which is the whole shape
// of an OAuth round trip: starting one is an ordinary administrator action, and
// coming back is a browser mid-redirect that carries no token of ours.
type toolConnectHandlers struct{ app *app.App }

// mountToolCallback registers where a service sends the person back.
//
// UNAUTHENTICATED on purpose, exactly as the MCP callback is: a browser
// mid-redirect carries no bearer token, so the sealed state parameter is the
// whole authentication. It was minted here, sealed with this installation's
// key, and it says which tool and which person.
//
// One address for every tool, with which tool it is inside the state rather
// than in the path: the address is registered with the service BY HAND, and an
// address per tool would mean registering a new one for every tool somebody
// adds, which is the step that would stop people using this at all.
func mountToolCallback(r chi.Router, a *app.App) {
	h := &toolConnectHandlers{app: a}
	r.Get("/connect/tool/callback", h.callback)
}

// begin answers with the address the browser should visit. Nothing is written:
// a consent that is started and abandoned leaves no trace.
func (h *toolConnectHandlers) begin(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	claims := claimsFrom(r)
	where, err := h.app.BeginToolConnect(r.Context(), claims.WorkspaceID, id, claims.UserID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"authorize_url": where})
}

// disconnect takes the grant away and leaves the tool, because revoking access
// is not deleting a configuration.
func (h *toolConnectHandlers) disconnect(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	claims := claimsFrom(r)
	if err := h.app.DisconnectTool(r.Context(), claims.WorkspaceID, id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// callback is where the person lands after consenting.
//
// It answers with a PAGE rather than JSON, because what reads it is a browser
// somebody is looking at: in the desktop application it is a different
// application from the one that started this, and cannot tell it anything, so
// this page is the only account of the outcome they will get.
func (h *toolConnectHandlers) callback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	state, code := query.Get("state"), query.Get("code")

	// The service's own refusal, in its own words: "this account is not
	// permitted" is the answer somebody needs, not a failure we invented over
	// the top of it.
	if remoteErr := query.Get("error"); remoteErr != "" {
		h.page(w, http.StatusBadRequest, connectOutcome{
			title:    "Not connected",
			detail:   "The service did not allow the sign-in.",
			reported: firstNonEmpty(query.Get("error_description"), remoteErr),
		})
		return
	}
	if state == "" || code == "" {
		h.page(w, http.StatusBadRequest, connectOutcome{
			title:  "Nothing to finish here",
			detail: "This address is the last step of a sign-in, and there is no sign-in under way.",
		})
		return
	}

	claim, err := h.app.OpenToolState(state)
	if err != nil {
		// The detail goes to the LOG and not to the page: which of the three
		// ways a state can fail to be ours is a diagnosis, and telling a
		// browser would describe our sealing to whoever sent them here.
		h.app.Log.Error().Err(err).Msg("open tool connect state")
		h.page(w, http.StatusBadRequest, connectOutcome{
			title:  "Not connected",
			detail: "That sign-in took too long, or it was not started here.",
		})
		return
	}
	if err := h.app.CompleteToolConnect(r.Context(), claim, code); err != nil {
		h.app.Log.Error().Err(err).Int64("tool_id", claim.ToolID).Msg("complete tool connect")
		h.page(w, http.StatusBadRequest, connectOutcome{
			title:    "Not connected",
			detail:   "The sign-in did not finish.",
			reported: serviceWords(err),
		})
		return
	}
	h.page(w, http.StatusOK, connectOutcome{
		ok:     true,
		title:  "Connected",
		detail: "The tool can now act at the service as you. Nothing else is needed here.",
	})
}

// page fills in the two things every outcome here shares: where to try again,
// and that the console is listening.
func (h *toolConnectHandlers) page(w http.ResponseWriter, status int, out connectOutcome) {
	out.back, out.kind = "the tool", "tool-connect"
	writeConnectPage(w, status, out)
}
