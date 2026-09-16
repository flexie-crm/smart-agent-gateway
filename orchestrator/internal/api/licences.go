package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/licences"
)

// What this product carries that somebody else wrote.
//
// It answers from the binary rather than from the database, because that is
// where the truth is: what is compiled in cannot change while the process runs,
// and a list in a table would be a second copy of it that could disagree.

type licenceHandlers struct{}

func mountLicences(r chi.Router) {
	h := &licenceHandlers{}
	// No permission beyond being signed in. This is attribution, which is owed
	// to the people whose work is in here rather than granted to the people
	// reading it: gating it behind an administrator role would mean the notice
	// travels only as far as the administrators, which is not what the licences
	// asking for it have in mind.
	r.Get("/licences", h.list)
}

type licencesResponse struct {
	Components []licences.Component `json:"components"`
}

func (h *licenceHandlers) list(w http.ResponseWriter, _ *http.Request) {
	all, err := licences.All()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			"the list of open source components could not be read")
		return
	}
	writeJSON(w, http.StatusOK, licencesResponse{Components: all})
}
