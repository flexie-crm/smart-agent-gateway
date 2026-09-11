package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
)

// Whether this installation can answer a question yet, and what is missing if
// it cannot.
//
// One answer serving two screens. The chat asks so it can say what is missing
// instead of offering a box that types into nothing; the console asks so it can
// put setup in front of somebody the first time and never again.
//
// Unlike the posture, this is RUNTIME and has to be: the posture is what kind of
// installation this is and never changes, while this changes the moment somebody
// finishes setting it up. Baking it into a build would mean shipping a product
// that is permanently either finished or unfinished.
type setupState struct {
	// Ready is the only field that decides anything. The rest explain it.
	Ready bool `json:"ready"`
	// HasName is whether the person has said what to call them. Not part of
	// Ready: an assistant with a model can answer, and being addressed as
	// "Owner" is a poor greeting rather than a broken installation.
	HasName bool `json:"has_name"`
	// OwnerID is who to rename, so the screen does not have to work it out.
	OwnerID int64 `json:"owner_id,omitempty"`
	// HasVendor is whether anywhere to get a model from has been configured.
	HasVendor bool `json:"has_vendor"`
	// HasModel is whether a model exists at all, from any source: one bought
	// from a vendor and one downloaded onto this computer count the same.
	HasModel bool `json:"has_model"`
	// GatewayModel names the model the Gateway thinks with, when it has one.
	GatewayModel string `json:"gateway_model,omitempty"`
}

func mountSetup(r chi.Router, a *app.App) {
	h := &setupHandlers{app: a}
	r.Get("/setup", h.state)
}

// seededOwnerName is what the one person is called until they say otherwise.
// It has to match what seeds them (cmd/sag/personal_owner.go); a placeholder
// nobody replaced is the same thing as an unanswered question.
const seededOwnerName = "Owner"

type setupHandlers struct{ app *app.App }

func (h *setupHandlers) state(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspace := claimsFrom(r).WorkspaceID

	vendors, err := h.app.Store.Vendors().List(ctx, workspace)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	models, err := h.app.Store.AIModels().List(ctx, workspace)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	agents, err := h.app.Store.Agents().List(ctx, workspace)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	out := setupState{
		HasVendor: len(vendors) > 0,
		HasModel:  len(models) > 0,
	}

	// The seeded owner is called "Owner" until somebody says otherwise, so that
	// placeholder IS the unanswered question. It is not part of Ready: an
	// assistant with a model can answer, and being greeted by the wrong name is
	// a poor greeting rather than a broken installation.
	if h.app.Config.Personal && h.app.Config.PersonalOwnerEmail != "" {
		if owner, err := h.app.Store.Users().GetByEmail(ctx, h.app.Config.PersonalOwnerEmail); err == nil {
			out.OwnerID = owner.ID
			out.HasName = owner.Name != seededOwnerName
		}
	}

	// The real test, and the only one worth gating on: the Gateway has a model
	// AND that model still exists. A row pointing at a model somebody deleted is
	// a configuration that looks complete and answers nothing.
	for _, agent := range agents {
		if agent.Key != model.DefaultAgentKey {
			continue
		}
		if agent.ModelID == nil {
			break
		}
		for _, m := range models {
			if m.ID == *agent.ModelID {
				out.Ready = true
				out.GatewayModel = m.ModelKey
			}
		}
		break
	}
	writeJSON(w, http.StatusOK, out)
}
