package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/filestore"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"github.com/go-chi/chi/v5"
)

// Uploading a file.
//
// Three things have to be true before any bytes are kept, and all three are
// checked before the file is written rather than after:
//
//   - the Gateway has a rule that can READ this type. Storing a file nothing
//     can read wastes the disk and hands the person a failure one turn later,
//     when they have already asked their question;
//   - it is not bigger than the ceiling;
//   - it is one file, in a request that says it is a file upload.
//
// What is NOT checked is the name. The name is data: it is kept so it can be
// shown back, and it never decides where anything is written. That is
// structural rather than careful (internal/filestore): the bytes go under an id
// this product minted, so there is no sanitising step to get wrong.

// maxUploadBytes is the ceiling on one file. Comfortable for a document or a
// long recording, and still far short of what a model would refuse to read
// anyway.
const maxUploadBytes = 50 << 20 // 50 MiB

func mountUploads(r chi.Router, a *app.App) {
	h := &uploadHandlers{app: a}
	r.Post("/chat/uploads", h.upload)
	r.Get("/chat/uploads/{id}", h.download)
}

type uploadHandlers struct{ app *app.App }

type uploadResponse struct {
	ID        string `json:"id"`
	FileName  string `json:"file_name"`
	FileType  string `json:"file_type"`
	SizeBytes int64  `json:"size_bytes"`
}

func (h *uploadHandlers) upload(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)

	// The ceiling is put on the REQUEST, not on the parse: a body that keeps
	// arriving is stopped by the connection rather than after something has
	// already read it. The multipart parse then bounds what it holds in memory,
	// and the file itself streams past both of those to disk.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
	//nolint:gosec // G120: the request body is bounded on the line above, which
	// is the bound that matters; this one only caps what the parse holds in
	// memory before spilling to a temporary file.
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "that was not a file upload")
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	part, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "no file was sent")
		return
	}
	defer func() { _ = part.Close() }()

	fileType := typeOf(header.Filename)
	if fileType == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "that file has no type this can recognise")
		return
	}

	// Can anything read it? The answer is the Gateway's own routing, which is
	// the same answer the chat asked for when it decided whether to offer the
	// button at all.
	ok, err := h.readable(r, claims.WorkspaceID, fileType)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "unsupported_file",
			fmt.Sprintf("a %s file cannot be read here, so it was not kept", fileType))
		return
	}

	id, err := model.NewAttachmentUID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "the upload could not be started")
		return
	}

	size, err := h.app.Files.Write(claims.WorkspaceID, id, part, maxUploadBytes)
	if err != nil {
		// The only failure a caller can act on is the size, and it is the likely
		// one; the rest are ours and are not described.
		h.app.Log.Warn().Err(err).Str("attachment", id).Msg("upload rejected")
		writeError(w, http.StatusBadRequest, "file_too_large", "the file could not be stored; it may be too large")
		return
	}

	attachment := &model.Attachment{
		PublicID: id, WorkspaceID: claims.WorkspaceID, UserID: claims.UserID,
		FileName: displayName(header.Filename), FileType: fileType, SizeBytes: size,
	}
	if err := h.app.Store.Attachments().Create(r.Context(), attachment); err != nil {
		// The row is what makes the bytes findable, so bytes without a row are
		// litter. Take them back out.
		_ = h.app.Files.Remove(claims.WorkspaceID, id)
		writeStoreError(w, h.app, err)
		return
	}

	writeJSON(w, http.StatusCreated, uploadResponse{
		ID: attachment.PublicID, FileName: attachment.FileName,
		FileType: attachment.FileType, SizeBytes: attachment.SizeBytes,
	})
}

// download hands a file back to the person who uploaded it, so a chat can show
// what was attached. Scoped to the uploader: somebody else's attachment is not
// refused, it is not found.
func (h *uploadHandlers) download(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	id := chi.URLParam(r, "id")

	attachment, err := h.app.Store.Attachments().ByPublicID(r.Context(), claims.WorkspaceID, claims.UserID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	f, err := h.app.Files.Open(claims.WorkspaceID, attachment.PublicID)
	if errors.Is(err, filestore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "that file is no longer here")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "that file could not be read")
		return
	}
	defer func() { _ = f.Close() }()

	// Never inline: a file somebody uploaded is not a page this origin should
	// render, and an attachment header is what stops it being one.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment")
	_, _ = io.Copy(w, f)
}

// readable reports whether the Gateway has a rule that covers this file type.
func (h *uploadHandlers) readable(r *http.Request, workspaceID int64, fileType string) (bool, error) {
	gateway, err := h.app.Store.Agents().GetByKey(r.Context(), workspaceID, model.DefaultAgentKey)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, rule := range gateway.FileRules {
		if rule.Matches(fileType) {
			return true, nil
		}
	}
	return false, nil
}

// typeOf is the file's type, lower case and without the dot, taken from its
// name. Only the extension is read, never the path a browser may have put in
// front of it.
func typeOf(name string) string {
	ext := strings.TrimPrefix(filepath.Ext(filepath.Base(name)), ".")
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext == "" || len(ext) > 16 {
		return ""
	}
	for _, c := range ext {
		letter := c >= 'a' && c <= 'z'
		digit := c >= '0' && c <= '9'
		if !letter && !digit {
			return ""
		}
	}
	// jpeg and jpg are the same picture, and a rule naming one should catch the
	// other. This is the only aliasing here: everything else is the extension.
	if ext == "jpeg" {
		return "jpg"
	}
	return ext
}

// displayName is what the person sees, with any path a browser included taken
// off. It is never used to build a path.
func displayName(name string) string {
	base := filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == "/" {
		return "file"
	}
	if len(base) > 200 {
		base = base[:200]
	}
	return base
}
