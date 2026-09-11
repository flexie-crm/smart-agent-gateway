package api

import (
	"errors"
	"io"
	"net/http"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/store"
	"github.com/go-chi/chi/v5"
)

// Talking instead of typing.
//
// A recording is not an attachment and does not go through the upload route.
// Two reasons, and the second is the one that decides it:
//
//   - the upload route gates on the Gateway's FILE rules, so a recording would
//     be refused unless somebody added webm as a document type, which is a
//     confusing thing to have to do and a wrong thing to have configured;
//   - a recording is not something the person is asking ABOUT. It is the asking.
//     What is kept is the words it turned into, in the message they become; the
//     audio itself has no life beyond this request and is never written down.
//
// So this is the same shape (multipart, field "file", the same ceiling, the
// same auth) pointed at a different question.

func mountAudio(r chi.Router, a *app.App) {
	h := &audioHandlers{app: a}
	r.Post("/chat/transcribe", h.transcribe)
}

type audioHandlers struct{ app *app.App }

type transcribeResponse struct {
	// Text is what was said. It goes into the composer for the person to read
	// and correct before they send it: a transcription is a good guess, and
	// sending one unseen would put words in somebody's mouth.
	Text string `json:"text"`
}

func (h *audioHandlers) transcribe(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)

	gateway, err := h.app.Store.Agents().GetByKey(r.Context(), claims.WorkspaceID, model.DefaultAgentKey)
	if errors.Is(err, store.ErrNotFound) || (err == nil && gateway.AudioModelID == nil) {
		writeError(w, http.StatusBadRequest, "audio_not_configured",
			"this assistant does not take audio")
		return
	}
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	resolved, err := h.app.Gateway.Resolve(r.Context(), claims.WorkspaceID, *gateway.AudioModelID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	if !resolved.Provider.Media().Transcribes {
		// The chat is told this in advance and does not offer a microphone, so
		// reaching here means somebody called it directly or the configuration
		// changed underneath them.
		writeError(w, http.StatusBadRequest, "audio_not_supported",
			"the model set up for audio cannot transcribe")
		return
	}

	// Bounded on the request, so a body that keeps arriving is stopped by the
	// connection rather than after something has read it.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
	//nolint:gosec // G120: the request is bounded above; this caps only what the
	// parse holds in memory before spilling to a temporary file.
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "that was not a recording")
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	part, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "no recording was sent")
		return
	}
	defer func() { _ = part.Close() }()

	audio, err := io.ReadAll(io.LimitReader(part, maxUploadBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "the recording could not be read")
		return
	}
	if int64(len(audio)) > maxUploadBytes {
		writeError(w, http.StatusBadRequest, "file_too_large", "that recording is too long")
		return
	}
	if len(audio) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "the recording was empty")
		return
	}

	// The NAME is carried through and is not decoration: the vendor reads its
	// extension to know how to decode the bytes, so a recording has to arrive
	// called what it is.
	text, err := resolved.Provider.Transcribe(r.Context(), resolved.Model.ModelKey,
		displayName(header.Filename), audio)
	if errors.Is(err, provider.ErrTranscriptionUnsupported) {
		writeError(w, http.StatusBadRequest, "audio_not_supported",
			"the model set up for audio cannot transcribe")
		return
	}
	if err != nil {
		h.app.Log.Warn().Err(err).Int64("workspace", claims.WorkspaceID).Msg("transcription failed")
		writeError(w, http.StatusBadGateway, "transcription_failed",
			"that recording could not be turned into words")
		return
	}

	writeJSON(w, http.StatusOK, transcribeResponse{Text: text})
}
