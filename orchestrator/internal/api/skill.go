package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/skill"
	"flexie.io/sag/internal/store"
)

// Skills: importing a package, and reading what was stored.
//
// The routes are the shape of the thing, as the brains routes are: a skill has
// versions, a version has files, a file has content. A person who refreshes
// lands where they were, and the address they send somebody opens what they were
// looking at.

type skillHandlers struct{ app *app.App }

func mountSkills(r chi.Router, a *app.App) {
	h := &skillHandlers{app: a}

	r.Route("/skills", func(r chi.Router) {
		// The whole screen in one answer (app.SkillView).
		r.With(requirePermission(a, model.PermSkillsView)).Get("/view", h.view)

		// Which skills match what somebody typed. Its own route rather than a
		// parameter on /view: a search does not resolve a selection, and a
		// screen answer that sometimes means "everything" and sometimes means
		// "the matches" has two contracts.
		r.With(requirePermission(a, model.PermSkillsView)).Get("/search", h.search)

		// Importing is CREATE, because it is the only way a skill comes into
		// existence here.
		r.With(requirePermission(a, model.PermSkillsCreate)).Post("/", h.upload)

		// Renaming, describing, enabling, disabling and rolling back are EDIT,
		// and they are ONE route because they are one form. None of them changes
		// a version: a version cannot be changed.
		//
		// PUBLISHING A DRAFT comes through here too, as the version to make
		// live, because it is the same act as a rollback and there must not be
		// a second way to move the pointer. So the split is: writing a version
		// is create, choosing which one the agent uses is edit.
		r.With(requirePermission(a, model.PermSkillsEdit)).Put("/{skillID}", h.update)

		// Editing a file WRITES a version, which is what importing does, so it
		// is the same permission. It does not make it live: a draft is a
		// version nobody is using until somebody with edit publishes it.
		r.With(requirePermission(a, model.PermSkillsCreate)).Post("/{skillID}/versions", h.draft)

		// And withdrawing one you wrote. Create rather than delete: delete is
		// about the skill itself, and turning down a draft is part of authoring
		// rather than destroying anything anybody is using.
		r.With(requirePermission(a, model.PermSkillsCreate)).
			Delete("/{skillID}/versions/{versionID}", h.discard)

		r.With(requirePermission(a, model.PermSkillsDelete)).Delete("/{skillID}", h.deleteSkill)
	})

	// A file is addressed by its own id, not through its version: it has one
	// identity, and a URL that reaches it two ways has two truths.
	r.Route("/skill-files", func(r chi.Router) {
		r.With(requirePermission(a, model.PermSkillsView)).Get("/{fileID}", h.file)
		r.With(requirePermission(a, model.PermSkillsView)).Get("/{fileID}/download", h.download)
	})
}

// --- the screen ------------------------------------------------------------------

func (h *skillHandlers) view(w http.ResponseWriter, r *http.Request) {
	overview, err := h.app.SkillView(r.Context(), claimsFrom(r).WorkspaceID, app.SkillSelection{
		SkillID:   queryID(r, "skill"),
		VersionID: queryID(r, "version"),
	})
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

// search answers which skills match what was typed, most relevant first.
//
// Whole skills, not hits: the console draws them as its skills pane, so the
// answer is the same shape the list is. An empty box is an empty answer rather
// than everything, because the caller asking with nothing typed is a race with
// its own debounce, and "everything" is what it already has.
func (h *skillHandlers) search(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if strings.TrimSpace(query) == "" {
		writeJSON(w, http.StatusOK, []*model.Skill{})
		return
	}
	found, err := h.app.Store.Skills().Search(r.Context(),
		claimsFrom(r).WorkspaceID, query, searchLimit(r))
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, found)
}

// --- importing -------------------------------------------------------------------

// importResponse says what actually happened to every archive.
//
// A list and not a single answer, even for a single archive, because there is
// one endpoint and it must have one contract. "Imported as version 3", "that is
// already version 2" and "that is not a zip archive" are three different
// answers, they can all occur in one import, and none of them can be carried by
// a status code once there is more than one package in the request.
type importResponse struct {
	Results []app.SkillImport `json:"results"`
}

func (h *skillHandlers) upload(w http.ResponseWriter, r *http.Request) {
	// The ceiling goes on the REQUEST, so a body that keeps arriving is stopped
	// by the connection rather than after something has read it. The parse then
	// bounds what it holds in memory; anything larger spills to a temporary
	// file, and either way what comes back can be read at an offset, which is
	// what a zip needs.
	//
	// The slack above the ceiling is the multipart framing: a boundary and a
	// couple of headers per part, which is bytes, not megabytes.
	r.Body = http.MaxBytesReader(w, r.Body, skill.MaxBatchBytes+(1<<20))
	//nolint:gosec // G120: the request body is bounded on the line above, which
	// is the bound that matters; this one only caps what the parse holds in
	// memory before spilling to a temporary file.
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		// Why it failed, because "that was not an uploaded file" is a lie told
		// to somebody who dropped forty packages on the zone and is owed the
		// real reason.
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeInvalidFields(w, fieldErrors{"file": fmt.Sprintf(
				"One import may carry up to %d MB, and that was more.", skill.MaxBatchBytes>>20)})
			return
		}
		writeInvalidFields(w, fieldErrors{"file": "That was not an uploaded file."})
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	// Every part sent under the same name, in the order the sender wrote them,
	// which is the order the answer comes back in.
	parts := r.MultipartForm.File["file"]
	if len(parts) == 0 {
		writeInvalidFields(w, fieldErrors{"file": "Choose a skill package to import."})
		return
	}
	if len(parts) > skill.MaxPackages {
		writeInvalidFields(w, fieldErrors{"file": fmt.Sprintf(
			"Up to %d packages at a time, and that was %d.", skill.MaxPackages, len(parts))})
		return
	}

	uploads := make([]app.SkillUpload, 0, len(parts))
	for _, part := range parts {
		open, err := part.Open()
		if err != nil {
			// Ours: the parse said it had the part and then would not hand it
			// over. Nothing about it is the sender's to fix.
			writeStoreError(w, h.app, fmt.Errorf("open the uploaded package: %w", err))
			return
		}
		defer func() { _ = open.Close() }()
		uploads = append(uploads, app.SkillUpload{
			// The sender's own name for the file, echoed back so a person can
			// tell which of forty was refused. It reaches no path: what a skill
			// is stored under comes from the manifest inside it.
			Name:    part.Filename,
			Archive: open,
			Size:    part.Size,
		})
	}

	// Who is importing, resolved before the archives are read: the name is
	// frozen into the rows, so it is read at the moment of the write.
	by, err := h.app.Acting(r.Context(), claimsFrom(r).UserID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	results, err := h.app.ImportSkills(r.Context(), claimsFrom(r).WorkspaceID, uploads, by)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, importResponse{Results: results})
}

// --- administering ---------------------------------------------------------------

// skillBody is what the skill's own form edits. The handle is not in it: it is
// the package's, and the agent addresses the skill by it.
type skillBody struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	// VersionID is which version should be live. It rides on the same form
	// because rolling back IS one of the things the form decides, and because a
	// rename and a rollback saved separately can half land.
	VersionID int64 `json:"version_id"`
}

// update saves the skill's form: what it is called, what it is for, whether it
// is on, and which version is live. One route for the four, because they are
// one dialog.
func (h *skillHandlers) update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "skillID")
	if !ok {
		return
	}
	var body skillBody
	if !decodeJSON(w, r, &body) {
		return
	}
	// Only the two an administrator chooses between. Draft and archived are
	// states the system moves a skill through, not buttons on a screen, and
	// accepting them here would let a skill be put into a state nothing sets.
	if body.Status != model.SkillActive && body.Status != model.SkillDisabled {
		writeInvalidFields(w, fieldErrors{"status": "A skill is either active or disabled."})
		return
	}
	// The title may be emptied: that is how somebody goes back to being called
	// by the handle. The description may be emptied too. Neither is required,
	// and requiring them would make a package that carried none unsaveable.
	if len([]rune(body.Title)) > 255 {
		writeInvalidFields(w, fieldErrors{"title": "A name can be at most 255 characters."})
		return
	}
	if len([]rune(body.Description)) > 1024 {
		writeInvalidFields(w, fieldErrors{"description": "A description can be at most 1024 characters."})
		return
	}

	// Who is editing, resolved at the moment of the write: the name is frozen
	// onto the row and has to be the one they had when they did it.
	by, err := h.app.Acting(r.Context(), claimsFrom(r).UserID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	updated, err := h.app.Store.Skills().Update(r.Context(), claimsFrom(r).WorkspaceID, id,
		store.SkillUpdate{
			Title:       strings.TrimSpace(body.Title),
			Description: strings.TrimSpace(body.Description),
			Status:      body.Status,
			VersionID:   body.VersionID,
		}, by)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *skillHandlers) deleteSkill(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "skillID")
	if !ok {
		return
	}
	if err := h.app.Store.Skills().DeleteSkill(r.Context(), claimsFrom(r).WorkspaceID, id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- reading a file --------------------------------------------------------------

// file hands back one file of one version.
//
// Text comes back in the body, because reading it is the point. Bytes do not:
// they are fetched by the download route, since a binary template arriving as
// base64 inside a screen's JSON is a file nobody can use in a form nothing can
// read.
func (h *skillHandlers) file(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "fileID")
	if !ok {
		return
	}
	file, err := h.app.SkillFile(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	file.Bytes = nil
	writeJSON(w, http.StatusOK, file)
}

func (h *skillHandlers) download(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "fileID")
	if !ok {
		return
	}
	file, err := h.app.SkillFile(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	// Never inline, and never the file's own declared type. A skill package is
	// content somebody uploaded, and this origin serves the console: an SVG or
	// an HTML file served inline from here would be a script running as us.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment")
	//nolint:gosec // G705: the taint is real and the mitigation is the three
	// headers above, which the analysis does not read. This is a file somebody
	// uploaded, served from the console's own origin, so it is served as a
	// download of unknown bytes: octet-stream, nosniff so the browser does not
	// decide otherwise, and attachment so it is never a page. There is no
	// rewriting of the content that would satisfy a taint analyser short of not
	// serving the file at all, which is the feature.
	if file.Binary {
		_, _ = w.Write(file.Bytes)
		return
	}
	//nolint:gosec // G705: as above. Text, and still never rendered as a page.
	_, _ = io.WriteString(w, file.Text)
}

// --- editing a package -----------------------------------------------------------

// draft saves an edit as a new version that is not live.
//
// The body carries which version the edits were written against, so a save
// cannot be applied to a version somebody else published while this one was
// being typed. Every edit names a file that version holds: this route edits
// contents, and a package gains or loses a file by being imported.
func (h *skillHandlers) draft(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "skillID")
	if !ok {
		return
	}
	var body struct {
		From  int64 `json:"from"`
		Edits []struct {
			Path string `json:"path"`
			Text string `json:"text"`
		} `json:"edits"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Edits) == 0 {
		writeInvalidFields(w, fieldErrors{"edits": "Nothing was edited."})
		return
	}
	edits := make([]skill.Edit, 0, len(body.Edits))
	for _, e := range body.Edits {
		if strings.TrimSpace(e.Path) == "" {
			writeInvalidFields(w, fieldErrors{"edits": "An edit has no file."})
			return
		}
		edits = append(edits, skill.Edit{Path: e.Path, Text: e.Text})
	}

	by, err := h.app.Acting(r.Context(), claimsFrom(r).UserID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	version, changed, err := h.app.DraftSkillVersion(r.Context(), claimsFrom(r).WorkspaceID, id,
		app.SkillEdit{From: body.From, Edits: edits}, by)
	if err != nil {
		// A refusal is the sender's to act on and says which file and why, the
		// same as a refused import. Anything else is ours.
		var rejection *skill.Rejection
		if errors.As(err, &rejection) {
			writeInvalidFields(w, fieldErrors{"edits": rejection.Reason})
			return
		}
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"version": version,
		// What changed, so the screen can say it without working it out again.
		"changed": changed,
	})
}

// discard turns a draft down.
func (h *skillHandlers) discard(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "skillID")
	if !ok {
		return
	}
	versionID, ok := pathID(w, r, "versionID")
	if !ok {
		return
	}
	by, err := h.app.Acting(r.Context(), claimsFrom(r).UserID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	if err := h.app.DiscardSkillVersion(r.Context(), claimsFrom(r).WorkspaceID, id, versionID, by); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
