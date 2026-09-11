package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
)

// The dashboard: what the gateway is doing, and what it has cost.
//
// It answers from two places, and the difference matters.
//
//   - What is happening RIGHT NOW comes from the run manager, in memory. It is
//     exact, because the runs are here: the process holding this HTTP request is
//     the process running the turns.
//   - What HAS happened comes from the database. It is the record, and it
//     outlives any process.
//
// When the queue lands and turns run on other nodes, the live half stops being
// answerable from memory alone. That is why it goes through one interface today:
// the swap will be invisible to this handler and to the console (KB/17).

type statsHandlers struct{ app *app.App }

func mountStats(r chi.Router, a *app.App) {
	h := &statsHandlers{app: a}
	// Anyone signed in may see the state of their own workspace. It is their
	// work: what it is doing and what it is costing them is not privileged.
	r.Get("/stats", h.stats)
}

type statsResponse struct {
	// Live is what is happening now.
	Live liveStats `json:"live"`
	// Today and Total are what has happened.
	Today  usageStats `json:"today"`
	Total  usageStats `json:"total"`
	Models []modelUse `json:"models"`
	Tools  []toolUse  `json:"tools"`
	// Configured is what the workspace has set up, so an empty dashboard can
	// say WHY it is empty: no vendor, no model, no agent.
	Configured configuredStats `json:"configured"`
}

type liveStats struct {
	// Connected is the number of distinct people in this workspace's chat right
	// now, and Sessions the sockets they hold (one person may have several tabs).
	Connected int `json:"connected"`
	Sessions  int `json:"sessions"`
	// Running is the number of turns being answered at this moment.
	Running int `json:"running"`
	// WaitingApproval is the number of turns stopped, waiting on a person. They
	// consume nothing and can sit for hours: they are not stuck, they are asked.
	WaitingApproval int `json:"waiting_approval"`
}

type usageStats struct {
	Runs         int64 `json:"runs"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	ToolCalls    int64 `json:"tool_calls"`
	// Failed is a count worth its own line: a dashboard that reports only
	// throughput is a dashboard that looks healthy while it is failing.
	Failed int64 `json:"failed"`
	// Refused is tool calls a person declined. Kept apart from Failed: a refusal
	// is the approval system working, not a fault.
	Refused int64 `json:"refused"`
}

type modelUse struct {
	ModelID      int64  `json:"model_id"`
	Name         string `json:"name"`
	Vendor       string `json:"vendor"`
	Calls        int64  `json:"calls"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

type toolUse struct {
	Name         string `json:"name"`
	FriendlyName string `json:"friendly_name"`
	Calls        int64  `json:"calls"`
	Failed       int64  `json:"failed"`
	Refused      int64  `json:"refused"`
}

type configuredStats struct {
	Vendors   int `json:"vendors"`
	Models    int `json:"models"`
	Agents    int `json:"agents"`
	Tools     int `json:"tools"`
	Workflows int `json:"workflows"`
}

func (h *statsHandlers) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID := claimsFrom(r).WorkspaceID

	usage, err := h.app.Store.Stats().Usage(ctx, workspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	// The live half is the SAME snapshot the socket pushes, built from the same
	// sources, so the endpoint on load and the deltas after it can never disagree
	// (KB/30). This is what makes the dashboard whole on the first paint, connected
	// count included, instead of 0 until the socket speaks.
	live, err := h.app.LiveDashboardSnapshot(ctx, workspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	response := statsResponse{
		Live: liveStats{
			Connected:       live.Connected,
			Sessions:        live.Sessions,
			Running:         live.Running,
			WaitingApproval: live.WaitingApproval,
		},
		Today: usageStats{
			Runs:         usage.Today.Runs,
			InputTokens:  usage.Today.InputTokens,
			OutputTokens: usage.Today.OutputTokens,
			ToolCalls:    usage.Today.ToolCalls,
			Failed:       usage.Today.Failed,
			Refused:      usage.Today.Refused,
		},
		Total: usageStats{
			Runs:         usage.Total.Runs,
			InputTokens:  usage.Total.InputTokens,
			OutputTokens: usage.Total.OutputTokens,
			ToolCalls:    usage.Total.ToolCalls,
			Failed:       usage.Total.Failed,
			Refused:      usage.Total.Refused,
		},
		Configured: configuredStats{
			Vendors:   usage.Configured.Vendors,
			Models:    usage.Configured.Models,
			Agents:    usage.Configured.Agents,
			Tools:     usage.Configured.Tools,
			Workflows: usage.Configured.Workflows,
		},
		Models: make([]modelUse, 0, len(usage.Models)),
		Tools:  make([]toolUse, 0, len(usage.Tools)),
	}
	for _, m := range usage.Models {
		response.Models = append(response.Models, modelUse{
			ModelID:      m.ModelID,
			Name:         m.ModelKey,
			Vendor:       m.VendorName,
			Calls:        m.Calls,
			InputTokens:  m.InputTokens,
			OutputTokens: m.OutputTokens,
		})
	}
	for _, t := range usage.Tools {
		response.Tools = append(response.Tools, toolUse{
			Name:         t.Name,
			FriendlyName: t.FriendlyName,
			Calls:        t.Calls,
			Failed:       t.Failed,
			Refused:      t.Refused,
		})
	}

	writeJSON(w, http.StatusOK, response)
}
