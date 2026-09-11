package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// Brains: the knowledge base an administrator curates and an agent navigates.
//
// The routes are the shape of the thing, so the console's URLs can be the same
// shape: a brain has categories, a category has documents, a document relates to
// documents. A person who refreshes the page lands where they were, and a link
// they send somebody opens what they were looking at.

type brainHandlers struct{ app *app.App }

func mountBrains(r chi.Router, a *app.App) {
	h := &brainHandlers{app: a}

	// Create and delete are about the brain itself; edit covers everything
	// inside one (categories, documents, links). A curator who may reshape a
	// brain's content cannot necessarily make brains appear or disappear.
	r.Route("/brains", func(r chi.Router) {
		r.With(requirePermission(a, model.PermBrainsView)).Get("/", h.listBrains)
		r.With(requirePermission(a, model.PermBrainsCreate)).Post("/", h.createBrain)
		r.With(requirePermission(a, model.PermBrainsView)).Get("/search", h.search)
		// The whole screen in one answer. See app.BrainView: four questions asked
		// in a row is four round trips to paint one page, and the person watching
		// sees three empty columns fill in one at a time.
		r.With(requirePermission(a, model.PermBrainsView)).Get("/view", h.view)

		r.With(requirePermission(a, model.PermBrainsView)).Get("/{brainID}", h.getBrain)
		r.With(requirePermission(a, model.PermBrainsEdit)).Put("/{brainID}", h.updateBrain)
		r.With(requirePermission(a, model.PermBrainsDelete)).Delete("/{brainID}", h.deleteBrain)

		r.With(requirePermission(a, model.PermBrainsView)).Get("/{brainID}/categories", h.listCategories)
		r.With(requirePermission(a, model.PermBrainsEdit)).Post("/{brainID}/categories", h.createCategory)
	})

	// Categories and documents are addressed by their own id, not through their
	// parent: a document has one identity, and a URL that reaches it two ways has
	// two truths.
	r.Route("/brain-categories", func(r chi.Router) {
		r.With(requirePermission(a, model.PermBrainsEdit)).Put("/{categoryID}", h.updateCategory)
		r.With(requirePermission(a, model.PermBrainsEdit)).Delete("/{categoryID}", h.deleteCategory)
		r.With(requirePermission(a, model.PermBrainsView)).Get("/{categoryID}/documents", h.listDocuments)
		r.With(requirePermission(a, model.PermBrainsEdit)).Post("/{categoryID}/documents", h.createDocument)
	})

	r.Route("/brain-documents", func(r chi.Router) {
		r.With(requirePermission(a, model.PermBrainsView)).Get("/{documentID}", h.getDocument)
		r.With(requirePermission(a, model.PermBrainsEdit)).Put("/{documentID}", h.updateDocument)
		r.With(requirePermission(a, model.PermBrainsEdit)).Delete("/{documentID}", h.deleteDocument)
	})
}

// --- brains ------------------------------------------------------------------------

type brainBody struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Locked: the agent may read this brain and never write to it. An
	// administrator still edits it by hand.
	Locked bool `json:"locked"`
}

func (h *brainHandlers) listBrains(w http.ResponseWriter, r *http.Request) {
	brains, err := h.app.Store.Brains().Brains(r.Context(), claimsFrom(r).WorkspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, brains)
}

// view answers with the brains, the categories of the selected brain, the
// documents of the selected category, and the open document.
//
// The selection is a hint, not a demand: what is asked for is honoured when it
// exists, and the first of everything answers when it does not. A deleted brain
// in somebody's bookmark is a stale link, not a 404 they have to think about.
func (h *brainHandlers) view(w http.ResponseWriter, r *http.Request) {
	want := app.BrainSelection{
		BrainID:    queryID(r, "brain"),
		CategoryID: queryID(r, "category"),
		DocumentID: queryID(r, "document"),
	}
	overview, err := h.app.BrainView(r.Context(), claimsFrom(r).WorkspaceID, want)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

// queryID reads an optional id from the query string. Anything that is not a
// positive number is "not given": a garbage parameter selects the default, which
// is what a person with a mangled URL wants to happen.
func queryID(r *http.Request, name string) int64 {
	id, err := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	if err != nil || id < 1 {
		return 0
	}
	return id
}

func (h *brainHandlers) getBrain(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "brainID")
	if !ok {
		return
	}
	brain, err := h.app.Store.Brains().Brain(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, brain)
}

func (h *brainHandlers) createBrain(w http.ResponseWriter, r *http.Request) {
	var body brainBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeInvalidFields(w, fieldErrors{"name": "a brain needs a name"})
		return
	}

	brain := &model.Brain{
		WorkspaceID: claimsFrom(r).WorkspaceID,
		Name:        strings.TrimSpace(body.Name),
		Description: body.Description,
		Locked:      body.Locked,
	}
	if err := h.app.Store.Brains().CreateBrain(r.Context(), brain); err != nil {
		writeSaveError(w, h.app, err, "name", "another brain already has this name")
		return
	}
	writeJSON(w, http.StatusCreated, brain)
}

func (h *brainHandlers) updateBrain(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "brainID")
	if !ok {
		return
	}
	var body brainBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeInvalidFields(w, fieldErrors{"name": "a brain needs a name"})
		return
	}

	brain := &model.Brain{
		ID:          id,
		WorkspaceID: claimsFrom(r).WorkspaceID,
		Name:        strings.TrimSpace(body.Name),
		Description: body.Description,
		Locked:      body.Locked,
	}
	if err := h.app.Store.Brains().UpdateBrain(r.Context(), brain); err != nil {
		writeSaveError(w, h.app, err, "name", "another brain already has this name")
		return
	}
	writeJSON(w, http.StatusOK, brain)
}

func (h *brainHandlers) deleteBrain(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "brainID")
	if !ok {
		return
	}
	if err := h.app.Store.Brains().DeleteBrain(r.Context(), claimsFrom(r).WorkspaceID, id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- categories --------------------------------------------------------------------

type categoryBody struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Weight      int    `json:"weight"`
}

func (h *brainHandlers) listCategories(w http.ResponseWriter, r *http.Request) {
	brainID, ok := pathID(w, r, "brainID")
	if !ok {
		return
	}
	categories, err := h.app.Store.Brains().Categories(r.Context(), claimsFrom(r).WorkspaceID, brainID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, categories)
}

func (h *brainHandlers) createCategory(w http.ResponseWriter, r *http.Request) {
	brainID, ok := pathID(w, r, "brainID")
	if !ok {
		return
	}
	var body categoryBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeInvalidFields(w, fieldErrors{"name": "a category needs a name"})
		return
	}

	category := &model.BrainCategory{
		BrainID:     brainID,
		Name:        strings.TrimSpace(body.Name),
		Description: body.Description,
		Weight:      body.Weight,
	}
	if err := h.app.Store.Brains().CreateCategory(r.Context(), claimsFrom(r).WorkspaceID, category); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusCreated, category)
}

func (h *brainHandlers) updateCategory(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "categoryID")
	if !ok {
		return
	}
	var body categoryBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeInvalidFields(w, fieldErrors{"name": "a category needs a name"})
		return
	}

	category := &model.BrainCategory{
		ID:          id,
		Name:        strings.TrimSpace(body.Name),
		Description: body.Description,
		Weight:      body.Weight,
	}
	if err := h.app.Store.Brains().UpdateCategory(r.Context(), claimsFrom(r).WorkspaceID, category); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, category)
}

func (h *brainHandlers) deleteCategory(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "categoryID")
	if !ok {
		return
	}
	if err := h.app.Store.Brains().DeleteCategory(r.Context(), claimsFrom(r).WorkspaceID, id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- documents ---------------------------------------------------------------------

type documentBody struct {
	Title string `json:"title"`
	// Content is Markdown. It is stored exactly as written and rendered in the
	// browser: the server never turns it into HTML, because an agent writes here
	// too, and what an agent writes must not become a script tag.
	Content string `json:"content"`
	Weight  int    `json:"weight"`
	// CategoryID lets a document be MOVED between categories of the same brain.
	CategoryID int64 `json:"category_id"`
	// Related is the graph. The links are made symmetric, and only documents in
	// the same brain can be joined.
	Related []int64 `json:"related"`
}

func (h *brainHandlers) listDocuments(w http.ResponseWriter, r *http.Request) {
	categoryID, ok := pathID(w, r, "categoryID")
	if !ok {
		return
	}
	documents, err := h.app.Store.Brains().Documents(r.Context(), claimsFrom(r).WorkspaceID, categoryID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, documents)
}

func (h *brainHandlers) getDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "documentID")
	if !ok {
		return
	}
	document, err := h.app.Store.Brains().Document(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, document)
}

func (h *brainHandlers) createDocument(w http.ResponseWriter, r *http.Request) {
	categoryID, ok := pathID(w, r, "categoryID")
	if !ok {
		return
	}
	var body documentBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		writeInvalidFields(w, fieldErrors{"title": "a document needs a title"})
		return
	}

	document := &model.BrainDocument{
		CategoryID: categoryID,
		Title:      strings.TrimSpace(body.Title),
		Content:    body.Content,
		Weight:     body.Weight,
	}
	if err := h.app.Store.Brains().SaveDocument(r.Context(), claimsFrom(r).WorkspaceID,
		document, body.Related); err != nil {
		writeSaveError(w, h.app, err, "title", "this category already has a document with this title")
		return
	}
	h.respondWithDocument(w, r, http.StatusCreated, document.ID)
}

func (h *brainHandlers) updateDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "documentID")
	if !ok {
		return
	}
	var body documentBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		writeInvalidFields(w, fieldErrors{"title": "a document needs a title"})
		return
	}

	ctx := r.Context()
	workspaceID := claimsFrom(r).WorkspaceID

	// Load it first: the category on the request is where it is MOVING to, and it
	// still has to be a category this workspace owns, which the store checks.
	existing, err := h.app.Store.Brains().Document(ctx, workspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	category := body.CategoryID
	if category == 0 {
		category = existing.CategoryID
	}

	document := &model.BrainDocument{
		ID:         id,
		CategoryID: category,
		Title:      strings.TrimSpace(body.Title),
		Content:    body.Content,
		Weight:     body.Weight,
		CreatedAt:  existing.CreatedAt,
	}
	if err := h.app.Store.Brains().SaveDocument(ctx, workspaceID, document, body.Related); err != nil {
		if errors.Is(err, store.ErrWrongBrain) {
			writeInvalidFields(w, fieldErrors{"category_id": "a document cannot move to another brain"})
			return
		}
		writeSaveError(w, h.app, err, "title", "this category already has a document with this title")
		return
	}
	h.respondWithDocument(w, r, http.StatusOK, id)
}

// respondWithDocument reads the document back rather than echoing what was sent.
// The links are symmetric and the store made them so: what the caller asked for
// is not necessarily what the graph now says.
func (h *brainHandlers) respondWithDocument(w http.ResponseWriter, r *http.Request, status int, id int64) {
	document, err := h.app.Store.Brains().Document(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, status, document)
}

func (h *brainHandlers) deleteDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "documentID")
	if !ok {
		return
	}
	if err := h.app.Store.Brains().DeleteDocument(r.Context(), claimsFrom(r).WorkspaceID, id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- search ------------------------------------------------------------------------

// search looks across brains for a person. The agent's own search goes through
// the tool, where the allow-list applies; here the workspace is the boundary,
// because a person curating the knowledge base can see all of it.
func (h *brainHandlers) search(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if strings.TrimSpace(query) == "" {
		writeJSON(w, http.StatusOK, []model.BrainHit{})
		return
	}

	ctx := r.Context()
	workspaceID := claimsFrom(r).WorkspaceID

	// Which brains to search: the ones named, or all of this workspace's.
	var brainIDs []int64
	if raw := r.URL.Query().Get("brain_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid brain")
			return
		}
		brainIDs = []int64{id}
	} else {
		brains, err := h.app.Store.Brains().Brains(ctx, workspaceID)
		if err != nil {
			writeStoreError(w, h.app, err)
			return
		}
		for _, b := range brains {
			brainIDs = append(brainIDs, b.ID)
		}
	}

	hits, err := h.app.Store.Brains().Search(ctx, workspaceID, brainIDs, query, 20)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}
