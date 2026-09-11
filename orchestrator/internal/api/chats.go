package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// The chat list: finding a conversation again, and managing the ones you have.
//
// Everything here is scoped to the person asking. A conversation belongs to the
// user who started it, so another user's chat does not merely fail to load: it
// does not exist.

func mountChats(r chi.Router, a *app.App) {
	h := &chatHandlers{app: a}
	r.Route("/chat/chats", func(r chi.Router) {
		r.Get("/", h.listChats)
		r.Post("/create", h.createChat)
		// Conversations are addressed by their public id, never by a row id.
		r.Post("/update/{uid}", h.updateChat)
		// Deleting is the one act here an organisation may want to withhold: a
		// conversation is the record of what was asked and what the assistant
		// was allowed to do about it. Naming and pinning are the person's own
		// housekeeping and are not gated (see model.PermChatsDelete).
		r.With(requirePermission(a, model.PermChatsDelete)).Post("/delete/{uid}", h.deleteChat)
	})
	// There is no /chat/accepts. What the composer may offer is a reading of the
	// Gateway's configuration, needed exactly when a conversation opens and at
	// no other time, so it rides on the history answer (historyMeta.Accepts)
	// rather than being a second request beside it.
}

type chatListItem struct {
	ID            string  `json:"id"`
	Title         *string `json:"title"`
	LastMessageAt *string `json:"last_message_at"`
	IsPinned      bool    `json:"is_pinned"`
	MessageCount  int     `json:"message_count"`
}

type chatListResponse struct {
	Chats []chatListItem `json:"chats"`
	// CanDelete is a fact about the whole list, not about a row: the permission
	// is the person's and every conversation in it is theirs, so it is answered
	// once. The sidebar reads it to decide whether to offer deleting at all,
	// because an action that is going to be refused should never be offered.
	//
	// It rides here rather than being a second request. The sidebar asks one
	// question ("what conversations do I have?") and this is part of the answer.
	CanDelete bool `json:"can_delete"`
}

// listChats lists, or searches. The search runs over the titles AND what was
// said, because people look for a conversation by what it was about.
func (h *chatHandlers) listChats(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)

	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		limit = 30
	}

	chats, err := h.app.Store.Agent().ListChats(r.Context(), store.ChatQuery{
		WorkspaceID: claims.WorkspaceID,
		UserID:      claims.UserID,
		Query:       r.URL.Query().Get("q"),
		Limit:       limit,
	})
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	out := make([]chatListItem, 0, len(chats))
	for _, chat := range chats {
		item := chatListItem{
			ID:           chat.UID,
			IsPinned:     chat.IsPinned,
			MessageCount: chat.MessageCount,
		}
		if chat.Title != "" {
			title := chat.Title
			item.Title = &title
		}
		if chat.LastMessageAt != nil {
			at := chat.LastMessageAt.Format(time.RFC3339)
			item.LastMessageAt = &at
		}
		out = append(out, item)
	}

	// Asked live, like every other permission check (KB/08): a role edited
	// while somebody is sitting on this screen bites on their next request,
	// not at the token's expiry.
	canDelete, err := h.app.Authorize(r.Context(), claims.UserID, model.PermChatsDelete)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, chatListResponse{Chats: out, CanDelete: canDelete})
}

type createChatRequest struct {
	Title string `json:"title"`
}

// createChat opens an empty conversation. The first turn gives it a name; this
// only reserves the row so the sidebar has something to select.
func (h *chatHandlers) createChat(w http.ResponseWriter, r *http.Request) {
	var req createChatRequest
	if r.ContentLength > 0 && !decodeJSON(w, r, &req) {
		return
	}
	claims := claimsFrom(r)

	session := &model.AgentSession{
		WorkspaceID: claims.WorkspaceID,
		UserID:      claims.UserID,
		Channel:     model.ChannelChat,
		Title:       req.Title,
	}
	if err := h.app.Store.Agent().CreateSession(r.Context(), session); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": session.UID})
}

type updateChatRequest struct {
	// A nil field is left alone, so renaming a chat cannot silently unpin it.
	Title    *string `json:"title"`
	IsPinned *bool   `json:"is_pinned"`
}

func (h *chatHandlers) updateChat(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "a chat id is required")
		return
	}
	var req updateChatRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	claims := claimsFrom(r)

	err := h.app.Store.Agent().UpdateChat(r.Context(), claims.WorkspaceID, claims.UserID, uid,
		store.ChatUpdate{Title: req.Title, IsPinned: req.IsPinned})
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteChat removes the conversation and everything it produced. Deleting a
// chat means it is gone, not hidden.
func (h *chatHandlers) deleteChat(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "a chat id is required")
		return
	}
	claims := claimsFrom(r)

	// Stop any background agents working under this conversation before it is
	// removed, so no goroutine keeps running against records about to disappear
	// (Mode C, KB/27). The ownership check mirrors DeleteChat's own.
	if session, err := h.app.Store.Agent().GetSessionByUID(r.Context(), claims.WorkspaceID, uid); err == nil && session.UserID == claims.UserID {
		h.app.CancelSessionBackground(session.ID)
	}

	if err := h.app.Store.Agent().DeleteChat(r.Context(), claims.WorkspaceID, claims.UserID, uid); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// What the chat may offer to send.
//
// The attach button and the microphone are not preferences: they are a reading
// of the Gateway's configuration. A file nothing can read must not be
// offerable, because the offer is a promise, and a person who attaches a
// spreadsheet to a Gateway that cannot read one has been told a lie by the
// interface rather than by anybody.
//
// So the client asks, and shows only what the answer allows. The same answer
// also names the types, which is what the file picker filters on: being turned
// away by a dialog that never offered the file is better than being turned away
// by an error after uploading it.
type chatAcceptsResponse struct {
	// FileTypes are the extensions that can be attached, lower case and without
	// dots. Empty with AnyFile false means no attach button at all.
	FileTypes []string `json:"file_types"`
	// AnyFile says a rule catches everything the named types did not, so the
	// picker should not filter at all.
	AnyFile bool `json:"any_file"`
	// Audio says somebody can talk instead of typing.
	Audio bool `json:"audio"`
	// MaxBytes is the ceiling on one file, so the client can say no before
	// spending somebody's upstream on a file the server will refuse. It is here
	// rather than in the client's own config because two places holding one
	// number is two places to disagree.
	MaxBytes int64 `json:"max_bytes"`

	// FilesUnsupported and AudioUnsupported name a vendor that was configured
	// for the job and cannot do it.
	//
	// This is not us restricting what an administrator may choose: they pick any
	// model they like, and whether it can make sense of a spreadsheet is their
	// call. It is the narrower fact that the vendor has no way to be handed a
	// file, or no transcription endpoint at all, so the thing they configured can
	// never work. Empty means nothing is wrong. Set means the chat says so on
	// the screen instead of offering a button that fails.
	FilesUnsupported string `json:"files_unsupported,omitempty"`
	AudioUnsupported string `json:"audio_unsupported,omitempty"`
}

// acceptsFor reads what the composer may offer out of the Gateway's own
// configuration. It rides on the history answer (historyMeta.Accepts), which is
// the one request a conversation makes when it opens, so this is a function
// rather than only an endpoint.
func (h *chatHandlers) acceptsFor(r *http.Request) chatAcceptsResponse {
	claims := claimsFrom(r)
	gateway, err := h.app.Store.Agents().GetByKey(r.Context(), claims.WorkspaceID, model.DefaultAgentKey)
	if err != nil {
		// No Gateway configured is not an error to the chat: it simply cannot
		// be sent anything but words.
		return chatAcceptsResponse{FileTypes: []string{}, MaxBytes: maxUploadBytes}
	}

	out := chatAcceptsResponse{
		FileTypes: []string{}, Audio: gateway.AudioModelID != nil, MaxBytes: maxUploadBytes,
	}
	// A vendor configured for a job it cannot do is worth saying out loud. The
	// administrator chose the model; what they could not know is whether this
	// build can send that vendor a file at all.
	if len(gateway.FileRules) > 0 {
		if why := h.vendorCannot(r, gateway.FileRules[0].ModelID, mediaFiles); why != "" {
			out.FilesUnsupported = why
		}
	}
	if gateway.AudioModelID != nil {
		if why := h.vendorCannot(r, *gateway.AudioModelID, mediaAudio); why != "" {
			out.AudioUnsupported = why
			out.Audio = false
		}
	}
	seen := map[string]bool{}
	for _, rule := range gateway.FileRules {
		if len(rule.Types) == 0 {
			out.AnyFile = true
			continue
		}
		for _, t := range rule.Types {
			if !seen[t] {
				seen[t] = true
				out.FileTypes = append(out.FileTypes, t)
			}
		}
	}
	return out
}

// The two jobs a vendor is asked about.
const (
	mediaFiles = "files"
	mediaAudio = "audio"
)

// vendorCannot names the reason a configured vendor cannot do the job, or
// nothing when it can.
//
// It asks the ADAPTER, not the model: whether this build knows how to hand that
// vendor a file, or how to ask it for a transcription. Which model understands
// a spreadsheet is a different question and stays the administrator's, exactly
// as it should: they set it up, and they are the one who knows what they bought.
func (h *chatHandlers) vendorCannot(r *http.Request, modelID int64, job string) string {
	resolved, err := h.app.Gateway.Resolve(r.Context(), claimsFrom(r).WorkspaceID, modelID)
	if err != nil {
		// A model that cannot even be resolved is a different problem, and one
		// the turn itself will report. Nothing is claimed here.
		return ""
	}
	media := resolved.Provider.Media()
	switch {
	case job == mediaFiles && !media.ReadsFiles:
		return fmt.Sprintf("%s cannot be sent files, so the model set up to read them cannot receive one.",
			resolved.Vendor.Name)
	case job == mediaAudio && !media.Transcribes:
		return fmt.Sprintf("%s does not transcribe audio, so the model set up for it cannot be used.",
			resolved.Vendor.Name)
	}
	return ""
}
