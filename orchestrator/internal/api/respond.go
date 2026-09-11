package api

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/store"
)

// maxBodyBytes caps every JSON request body.
const maxBodyBytes = 1 << 20

type errorBody struct {
	Error       string      `json:"error"`
	Description string      `json:"error_description,omitempty"`
	Fields      fieldErrors `json:"fields,omitempty"`
}

// fieldErrors are the problems of a request body, keyed by the JSON field name
// the client sent, so a form can put each message on the input that caused it.
// A message never repeats the field's name: it is read next to the field.
type fieldErrors map[string]string

// merge copies problems in, keeping the first message a field was given.
func (fe fieldErrors) merge(other fieldErrors) {
	for field, message := range other {
		if _, taken := fe[field]; !taken {
			fe[field] = message
		}
	}
}

func writeError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, errorBody{Error: code, Description: description})
}

// writeFieldErrors answers a request whose problems belong to named fields.
// The description stays generic on purpose: the fields carry the specifics,
// and a client that renders forms shows each message on its input.
func writeFieldErrors(w http.ResponseWriter, status int, code string, fields fieldErrors) {
	writeJSON(w, status, errorBody{
		Error:       code,
		Description: "some fields need a change",
		Fields:      fields,
	})
}

// writeInvalidFields is the validation answer: every problem found in the
// body, in one response. A form corrected one rejection at a time is a form
// submitted five times.
func writeInvalidFields(w http.ResponseWriter, fields fieldErrors) {
	writeFieldErrors(w, http.StatusBadRequest, "invalid_request", fields)
}

// writeStoreError maps store failures onto HTTP without leaking internals:
// anything unexpected is logged and answered as a generic server error.
func writeStoreError(w http.ResponseWriter, a *app.App, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "resource already exists")
	case errors.Is(err, store.ErrInUse):
		writeError(w, http.StatusConflict, "conflict", "resource is still in use")
	case errors.Is(err, store.ErrWrongBrain):
		writeError(w, http.StatusConflict, "conflict", "a document cannot move to another brain")
	default:
		a.Log.Error().Err(err).Msg("request failed")
		writeError(w, http.StatusInternalServerError, "server_error", "temporary failure")
	}
}

// writeSaveError is writeStoreError for creates and updates of things with a
// unique key: a collision is answered on the field that collided, with a
// message written for a person, and everything else falls through unchanged.
func writeSaveError(w http.ResponseWriter, a *app.App, err error, field, message string) {
	if errors.Is(err, store.ErrConflict) {
		writeFieldErrors(w, http.StatusConflict, "conflict", fieldErrors{field: message})
		return
	}
	writeStoreError(w, a, err)
}

// decodeJSON rejects unknown fields so a typo in a client payload is an error
// instead of a silently ignored setting, and it requires an application/json
// content type. That content-type requirement is a CSRF defence: it makes every
// state-changing request a NON-"simple" cross-origin request, so a cross-site
// POST (which could otherwise slip through as CORS-safelisted text/plain with no
// preflight, e.g. a login-CSRF that plants the attacker's session) is forced into
// a preflight the origin allow-list rejects. Same-origin and allow-listed callers
// send application/json already, so nothing legitimate is turned away.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"requests must be sent as application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return false
	}
	return true
}
