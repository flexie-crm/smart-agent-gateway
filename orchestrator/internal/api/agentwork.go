package api

import (
	"net/http"

	"flexie.io/sag/internal/app"
)

// Opening an agent from its chip (KB/27). Both answers are what has happened so
// far; what happens next is pushed on the socket to whoever has it open.

type agentWorkRequest struct {
	ChatID       string `json:"chat_id"`
	DelegationID int64  `json:"delegation_id"`
}

// agentWork answers one agent run: the run, and everything it has done.
//
// Authorized by the conversation, like stopping one: the run belongs to a
// conversation this person owns, or it does not exist as far as they are
// concerned.
func (h *chatHandlers) agentWork(w http.ResponseWriter, r *http.Request) {
	var req agentWorkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	claims := claimsFrom(r)
	ctx := r.Context()

	session, err := h.app.Store.Agent().GetSessionByUID(ctx, claims.WorkspaceID, req.ChatID)
	if err != nil || session.UserID != claims.UserID {
		writeError(w, http.StatusNotFound, "not_found", "no such agent")
		return
	}
	del, err := h.app.Store.Agent().GetDelegation(ctx, req.DelegationID)
	if err != nil || del.SessionID != session.ID {
		writeError(w, http.StatusNotFound, "not_found", "no such agent")
		return
	}
	work, err := h.app.AgentWork(ctx, claims.UserID, del)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, work)
}

type fleetRunsRequest struct {
	ChatID  string `json:"chat_id"`
	FleetID int64  `json:"fleet_id"`
}

type fleetRunsResponse struct {
	Runs []app.AgentRun `json:"runs"`
}

// fleetRuns answers every agent of a batch, where each of them stands.
func (h *chatHandlers) fleetRuns(w http.ResponseWriter, r *http.Request) {
	var req fleetRunsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	claims := claimsFrom(r)
	ctx := r.Context()

	session, err := h.app.Store.Agent().GetSessionByUID(ctx, claims.WorkspaceID, req.ChatID)
	if err != nil || session.UserID != claims.UserID {
		writeError(w, http.StatusNotFound, "not_found", "no such batch")
		return
	}
	fleet, err := h.app.Store.Agent().Fleet(ctx, req.FleetID)
	if err != nil || fleet.SessionID != session.ID {
		writeError(w, http.StatusNotFound, "not_found", "no such batch")
		return
	}
	runs, err := h.app.FleetRuns(ctx, fleet)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, fleetRunsResponse{Runs: runs})
}
