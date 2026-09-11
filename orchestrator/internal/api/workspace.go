package api

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
)

// Workspaces partition the tenant. This is the admin surface that manages the
// partitions themselves; the switcher in auth.go answers a different question
// (where may I act) and stays membership-scoped on purpose.

type workspaceHandlers struct{ app *app.App }

func mountWorkspaces(r chi.Router, a *app.App) {
	h := &workspaceHandlers{app: a}
	r.Route("/workspaces", func(r chi.Router) {
		r.With(requirePermission(a, model.PermWorkspacesView)).Get("/", h.list)
		r.With(requirePermission(a, model.PermWorkspacesCreate)).Post("/", h.create)
		r.With(requirePermission(a, model.PermWorkspacesEdit)).Put("/{id}", h.update)
		r.With(requirePermission(a, model.PermWorkspacesDelete)).Delete("/{id}", h.delete)
	})
}

// workspaceAdminBody is the admin shape of a workspace. The switcher's
// workspaceBody stays lean (id, slug, name); the admin also sees the status,
// because a suspended workspace looks exactly like an active one otherwise.
type workspaceAdminBody struct {
	ID          int64     `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func newWorkspaceAdminBody(w *model.Workspace) *workspaceAdminBody {
	return &workspaceAdminBody{
		ID: w.ID, Slug: w.Slug, Name: w.Name, Description: w.Description, Status: w.Status,
		CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}
}

// workspaceRequest carries what an administrator fills in: a name and, in their
// own words, what the workspace is for. The slug is not theirs to type, it is
// derived from the name; an explicit one is still honored (an API caller, or a
// migration) but the console never sends it.
type workspaceRequest struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

func (h *workspaceHandlers) list(w http.ResponseWriter, r *http.Request) {
	workspaces, err := h.app.Store.Workspaces().List(r.Context())
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, mapSlice(workspaces, newWorkspaceAdminBody))
}

func (h *workspaceHandlers) create(w http.ResponseWriter, r *http.Request) {
	var req workspaceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// A new workspace is born active, whatever the request says: creating
	// something suspended is creating something that lies in the switcher.
	ws := &model.Workspace{
		Name:        strings.TrimSpace(req.Name),
		Description: strings.TrimSpace(req.Description),
		Slug:        workspaceSlug(req.Slug, req.Name),
		Status:      model.StatusActive,
	}
	if problems := workspaceProblems(ws); len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}
	if err := h.app.Store.Workspaces().Create(r.Context(), ws); err != nil {
		writeSaveError(w, h.app, err, "slug", "another workspace already uses this slug")
		return
	}
	// The creator joins what they created: a workspace you cannot enter is a
	// workspace you cannot configure.
	if err := h.app.Store.Workspaces().AddMember(r.Context(), ws.ID, claimsFrom(r).UserID); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	// And it is offered what this build can do. The server reconciles every
	// workspace's tools at boot, which is right for a build that gained a tool
	// and wrong for a workspace made after that boot: it would have none at all
	// until somebody restarted the server, and an agent in it could do nothing
	// while every screen looked correctly configured.
	if err := h.app.SyncTools(r.Context(), ws.ID); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusCreated, newWorkspaceAdminBody(ws))
}

func (h *workspaceHandlers) update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ws, err := h.app.Store.Workspaces().GetByID(r.Context(), id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	var req workspaceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	problems := fieldErrors{}
	if req.Status != model.StatusActive && req.Status != model.StatusSuspended {
		problems["status"] = "must be active or suspended"
	}

	ws.Name = strings.TrimSpace(req.Name)
	ws.Description = strings.TrimSpace(req.Description)
	ws.Slug = workspaceSlug(req.Slug, req.Name)
	ws.Status = req.Status
	problems.merge(workspaceProblems(ws))
	if len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}
	if err := h.app.Store.Workspaces().Update(r.Context(), ws); err != nil {
		writeSaveError(w, h.app, err, "slug", "another workspace already uses this slug")
		return
	}
	writeJSON(w, http.StatusOK, newWorkspaceAdminBody(ws))
}

func (h *workspaceHandlers) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	// The workspace the caller is standing in is off limits: deleting the
	// ground under your own token leaves a session that speaks for nothing.
	// Switch somewhere else first.
	if id == claimsFrom(r).WorkspaceID {
		writeError(w, http.StatusConflict, "conflict",
			"you cannot delete the workspace you are working in")
		return
	}
	if err := h.app.Store.Workspaces().Delete(r.Context(), id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// slugShape is what Slugify produces and what a caller may ask for directly:
// lowercase words joined by single dashes.
var slugShape = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// workspaceSlug prefers what the caller asked for, and derives a handle from
// the name when they asked for nothing.
func workspaceSlug(slug, name string) string {
	slug = strings.TrimSpace(slug)
	if slug != "" {
		return slug
	}
	derived := model.Slugify(name)
	if len(derived) > 64 {
		derived = strings.Trim(derived[:64], "-")
	}
	return derived
}

func workspaceProblems(ws *model.Workspace) fieldErrors {
	problems := fieldErrors{}
	if ws.Name == "" {
		problems["name"] = "a name is required"
	}
	switch {
	case ws.Slug == "":
		problems["slug"] = "a slug is required, and the name gave nothing to derive one from"
	case len(ws.Slug) > 64:
		problems["slug"] = "at most 64 characters"
	case !slugShape.MatchString(ws.Slug):
		problems["slug"] = "lowercase letters, digits and single dashes only"
	}
	if len(ws.Description) > 1024 {
		problems["description"] = "at most 1024 characters"
	}
	return problems
}
