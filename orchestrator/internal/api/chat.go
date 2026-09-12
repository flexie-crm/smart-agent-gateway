package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/auth"
	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/run"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
	"flexie.io/sag/internal/tools/toolkit"
)

type chatHandlers struct{ app *app.App }

// agentResolver binds a turn to the app's agent resolution. It delegates
// to the app so the chat surface and the server-initiated completion turn resolve
// agents through one path.
func (h *chatHandlers) agentResolver(workspaceID, userID int64, c app.Computer) agent.AgentResolver {
	return h.app.AgentResolver(workspaceID, userID, c)
}

func mountChat(r chi.Router, a *app.App) {
	h := &chatHandlers{app: a}
	// A turn is started by one request and LISTENED TO by any number of them.
	// The stream endpoint does both at once, because the common case is a
	// client that wants to hear the answer to its own question.
	r.Post("/chat/stream", h.stream)
	// Attach rejoins a turn already in flight: a reloaded page, a laptop that
	// woke up, a second tab. The answer kept being written while nobody was
	// listening, and this is how they hear the rest of it.
	r.Post("/chat/attach", h.attach)
	// Stop is now an explicit act. A reader disconnecting no longer ends the
	// turn, so ending it has to be something a person says.
	r.Post("/chat/cancel", h.cancel)
	// Something said while the conversation is still answering. It goes INTO the
	// turn rather than being refused, and is picked up at its next step.
	r.Post("/chat/say", h.say)
	// Stop a background delegation (Mode C): a long task the person no longer
	// wants. Distinct from cancelling the current turn.
	r.Post("/chat/delegation/cancel", h.cancelDelegation)
	// History reloads a conversation the user owns. Its metadata carries the
	// conversation's approval mode and the background agents still running, so
	// one read gives the chat everything it needs to render, chips included.
	r.Post("/chat/history", h.history)
	// ApprovalMode switches whether a conversation asks every time or
	// auto-approves for the rest of the session. Its current value is read as
	// part of the history response (historyMeta), not a separate request.
	r.Post("/chat/approval-mode", h.approvalMode)
	// What a tool was sent and what it answered, fetched when somebody opens
	// the row rather than carried on every turn: it is the largest thing in a
	// conversation and the one thing a person may not be allowed to see.
	r.With(requirePermission(a, model.PermChatsSeeTools)).Post("/chat/tool-call", h.toolCall)
}

type streamRequest struct {
	// Prompt is the user's message. It is empty on a resume, where the
	// decision is the whole input.
	Prompt string `json:"prompt"`
	// ChatID continues an existing conversation, named by its public id.
	// Empty starts a new one. A row id never crosses this boundary.
	ChatID string `json:"chat_id"`
	// Files are the public ids of what the person attached, uploaded before
	// this call. They travel WITH a prompt, never instead of one: a file is
	// something the person is asking about, and a question is the asking.
	Files []string `json:"files"`
	// ModelID is the caller's pick from the model picker. It is a preference:
	// a workflow that pins a model outranks it, because an administrator who
	// pinned one was giving an instruction.
	ModelID int64 `json:"model_id"`

	// ResumeToken and ResumeAction answer a confirmation card. They arrive on
	// a fresh request: nothing was held open waiting for the human.
	ResumeToken  string `json:"resume_token"`
	ResumeAction string `json:"resume_action"` // approved | rejected
	// ApproveAll, with an approval, also switches the whole conversation to
	// auto-approve, so this card is the last one this session asks. It is the
	// "approve and allow the rest this session" choice on the card.
	ApproveAll bool `json:"approve_all"`

	// DeviceID is which installation of the chat application this came from,
	// sent by the desktop application and empty from a browser.
	//
	// It is here rather than derived because it cannot be derived: the same
	// person may be signed in on a laptop and a desktop, and a tool that reaches
	// their own network has to reach the one they are typing on. The request is
	// the only thing that knows, so it says, and the answer travels with the
	// turn to the tool call.
	DeviceID string `json:"device_id"`

	// WorkingFolder is the folder on that computer the person gave the
	// assistant to work in, or empty when they have given none. Sent for the
	// same reason as the device: it is chosen in the application, kept on that
	// machine, and read by the tools that run there, so nothing on this side
	// could know it. Told to the model, an assistant with the file tools stops
	// answering "I cannot see your project" while holding them.
	WorkingFolder string `json:"working_folder"`

	// Machine is what kind of computer that is: the system, the shell a command
	// will actually run in, how paths are written there, and which of a short
	// list of programs are installed.
	//
	// Sent with every message rather than asked for, because it can change
	// while a conversation is open and there is nowhere to ask: the control
	// socket's first message is its only unprompted one. Nil from a browser and
	// from an application too old to say, and then nothing is said about the
	// machine rather than something guessed.
	Machine *app.MachineEnv `json:"machine"`

	// Timezone is where the person is, by IANA name ("Europe/Tirane"), as their
	// own browser or application reports it.
	//
	// Here for the reason the device is: the server cannot derive it. It runs
	// in UTC in a rack, and an assistant grounded in that tells somebody it is
	// nine in the morning while their screen says eleven. It is remembered
	// against the person rather than used for this turn alone, so a background
	// agent finishing at midnight is in their evening too.
	Timezone string `json:"timezone"`
}

const (
	resumeApproved = "approved"
	resumeRejected = "rejected"
)

func (h *chatHandlers) stream(w http.ResponseWriter, r *http.Request) {
	var req streamRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	claims := claimsFrom(r)

	if req.ResumeToken != "" {
		h.resume(w, r, req, claims.UserID, claims.WorkspaceID)
		return
	}
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "prompt is required")
		return
	}
	// The attachments are resolved BEFORE the turn starts, so a file that is
	// not this person's, or not there at all, is a refusal they can act on
	// rather than a turn that runs and quietly answers about nothing.
	attachments, ok := h.resolveAttachments(w, r, req.Files)
	if !ok {
		return
	}

	ctx := r.Context()
	// Where the person is, remembered before the prompt is assembled, because
	// the prompt reads it from the person rather than from this request: a turn
	// nobody is watching (a background agent's completion, a scheduled run) has
	// no request to ask and the person is still in the same place.
	h.app.RememberZone(ctx, claims.UserID, req.Timezone)
	// What this turn is allowed to be: the model, the instructions, the tools,
	// and whether it thinks. It is resolved per turn, from the configuration
	// as it stands right now.
	preq := app.ProfileRequest{
		WorkspaceID:      claims.WorkspaceID,
		UserID:           claims.UserID,
		DeviceID:         req.DeviceID,
		WorkingFolder:    req.WorkingFolder,
		Machine:          req.Machine,
		Channel:          model.ChannelChat,
		PreferredModelID: req.ModelID,
	}
	profile, err := h.app.ResolveProfile(ctx, preq)
	if err != nil {
		h.app.Log.Error().Err(err).Int64("user_id", claims.UserID).Msg("resolve profile")
		writeError(w, http.StatusInternalServerError, "server_error", "this assistant is not available right now")
		return
	}
	// A workspace answers only through a configured main agent (or a workflow).
	// With neither, nothing shaped this turn but the code defaults, and the
	// built-in default assistant no longer answers on its own: the person is
	// told, in the chat, to set one up. This is deliberate, explicit setup
	// (KB/15): the floor is now a configured assistant, not a working default.
	if profile.AgentID == nil && profile.WorkflowVersionID == nil {
		h.streamNotice(w, noMainAgentNotice)
		return
	}
	if profile.ModelID == 0 {
		// Nothing pinned a model and the caller did not pick one.
		writeError(w, http.StatusBadRequest, "invalid_request", "model_id is required")
		return
	}

	session, created, err := h.session(r, req, claims.UserID, claims.WorkspaceID, profile)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	// The tools are bound after the conversation exists, because the Gateway IS
	// its conversation as far as a tool that keeps state between calls is
	// concerned: bind before it and every new chat would share one owner
	// (tool.Owner).
	preq.SessionID = session.ID
	loadout, err := h.app.ResolveTools(ctx, preq, profile)
	if err != nil {
		h.app.Log.Error().Err(err).Int64("user_id", claims.UserID).Msg("resolve tools")
		writeError(w, http.StatusInternalServerError, "server_error", "this assistant is not available right now")
		return
	}

	show := h.maySee(ctx, claims.UserID)

	// Which computer this turn came from, and therefore which machine tools are
	// in it. Logged because a turn with no device and a turn whose device is not
	// linked look identical from the outside: the tool is simply absent, the
	// model says it cannot do the thing, and nothing anywhere says why.
	if h.app.Log.Debug().Enabled() {
		names := make([]string, 0, len(loadout.Schemas))
		for _, schema := range loadout.Schemas {
			names = append(names, schema.Name)
		}
		h.app.Log.Debug().Int64("user_id", claims.UserID).Str("device_id", req.DeviceID).
			Strs("tools", names).Msg("turn")
	}

	turn := agent.Turn{
		WorkspaceID:     claims.WorkspaceID,
		UserID:          claims.UserID,
		SessionID:       session.ID,
		DeviceID:        req.DeviceID,
		Show:            &show,
		ModelID:         profile.ModelID,
		Prompt:          req.Prompt,
		Attachments:     attachments,
		ReadAttachments: h.app.AttachmentText,
		SystemPrompt:    profile.SystemPrompt,
		Tools:           loadout,
		Reasoning:       profile.Reasoning,
		ApprovalTTL:     profile.ApprovalTTL,
		MaxIterations:   profile.MaxIterations,
		MaxFleetAgents:  profile.MaxFleetAgents,
		// This conversation's own approval setting: manual asks every time, auto
		// runs approval-gated tools without a card (the person turned it on).
		AutoApprove: session.ApprovalMode == model.ApprovalAuto,
		// A conversation is named when it has no name, and not only on its very
		// first turn.
		//
		// It used to also require MessageCount == 0, which made the first
		// exchange the ONLY chance. Naming is fire-and-forget and can come back
		// with nothing (a timeout, a provider hiccup, or an answer that cleans
		// up to an empty string, which returns silently), and one miss then left
		// a conversation called "New chat" for good however long it went on.
		// That is what "it randomly fails" was: a transient cause with a
		// permanent effect, and no second attempt to correct it.
		//
		// The right question was already written down one path over, in
		// sessionNeedsTitle, where a turn that stopped for approval had the same
		// problem and got the same answer. Both paths now ask it.
		NameConversation: session.Title == "",
		Agent:            h.agentResolver(claims.WorkspaceID, claims.UserID, app.Computer{DeviceID: req.DeviceID, Folder: req.WorkingFolder, Env: req.Machine}),
		StartBackground:  h.app.StartBackground,
		StartFleet:       h.app.StartFleet,
	}

	// The turn is started, not run here. It gets a life of its own: this
	// request merely listens to it, and when this request ends the turn does
	// not. That is the whole point, and it is why the failure below is still a
	// status code: nothing has been streamed yet.
	active, err := h.app.Runs.Start(ctx, turn)
	if errors.Is(err, run.ErrShuttingDown) {
		// A restart is under way. Said plainly and NOT as a failure of the
		// question: the person's message was not lost, it was not accepted.
		writeError(w, http.StatusServiceUnavailable, "restarting",
			"this service is restarting. Send that again in a moment.")
		return
	}
	if errors.Is(err, run.ErrBusy) {
		writeError(w, http.StatusConflict, "busy", "this conversation is already answering")
		return
	}
	if err != nil {
		h.app.Log.Error().Err(err).Int64("session_id", session.ID).Msg("start run")
		writeError(w, http.StatusInternalServerError, "server_error", "this turn could not be started")
		return
	}

	if created {
		// The client learns the conversation id before anything else, so a
		// retry continues this conversation instead of starting a second one.
		// It is written straight to the response rather than into the run,
		// because it is about THIS request, not about the turn.
		h.listen(w, r, active, -1, &chat.Frame{Type: chat.FrameChatCreated, ChatID: session.UID})
		return
	}
	h.listen(w, r, active, -1, nil)
}

// noMainAgentNotice is what the chat answers when a workspace has configured no
// main agent and no workflow. It is a person-facing message, not an error.
const noMainAgentNotice = "No assistant is set up for this workspace yet. Ask an administrator to set up the main agent, and I'll be ready to help."

// streamNotice answers a turn with a single message and no model call: it opens
// the stream and writes the text as the whole answer (a delta so it renders as
// it arrives, then the final result). Used when there is nothing configured to
// run, so the person is told rather than met with silence or a raw error. It
// creates no conversation: an unconfigured workspace leaves nothing to reload.
func (h *chatHandlers) streamNotice(w http.ResponseWriter, text string) {
	sse, err := chat.NewSSE(w)
	if err != nil {
		h.app.Log.Error().Err(err).Msg("open notice stream")
		writeError(w, http.StatusInternalServerError, "server_error", "streaming is not available")
		return
	}
	_ = sse.Write(chat.Frame{Type: chat.FrameDelta, Message: text})
	_ = sse.Write(chat.Frame{Type: chat.FrameResult, Final: true, Message: text})
}

// --- attaching ------------------------------------------------------------------

type attachRequest struct {
	ChatID string `json:"chat_id"`
	// After is the last frame index the client saw. A client that has nothing
	// (a reloaded page) omits it and gets the turn from the beginning.
	After *int `json:"after"`
	// Replay asks for a run's frames even if it has already finished. It is how a
	// client attaches to a SERVER-INITIATED turn it was nudged about (a background
	// completion, KB/27): that run is not in the history the client holds, so
	// replaying it does not double-paint, and a fast narration may have finished
	// in the moment between the nudge and this request. Without it, that race left
	// the person having to reload to see the result.
	Replay bool `json:"replay"`
	// Run names a specific run to attach to, rather than the conversation's most
	// recent. A completion nudge (KB/27) carries the run it is about, so several
	// completions finishing at once are each heard on their own run instead of all
	// resolving to the latest and losing the earlier ones.
	Run string `json:"run"`
}

// attach rejoins a turn that is already running.
//
// It answers 204 when there is nothing to rejoin, which is the ordinary case:
// most conversations are not mid-answer when you open them. A client calls this
// on load, gets nothing, and carries on.
func (h *chatHandlers) attach(w http.ResponseWriter, r *http.Request) {
	var req attachRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	claims := claimsFrom(r)
	if req.ChatID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "chat_id is required")
		return
	}

	session, err := h.app.Store.Agent().GetSessionByUID(r.Context(), claims.WorkspaceID, req.ChatID)
	if err != nil || session.UserID != claims.UserID {
		// A conversation you do not own does not exist as far as you are
		// concerned, and neither does anything happening inside it.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// A nudge names its run; otherwise attach to whatever is most recent. A named
	// run must belong to this conversation, or it does not exist for this caller.
	var (
		active *run.Run
		ok     bool
	)
	if req.Run != "" {
		active, ok = h.app.Runs.Get(req.Run)
		if ok && active.SessionID != session.ID {
			ok = false
		}
	} else {
		active, ok = h.app.Runs.Latest(session.ID)
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	after := -1
	if req.After != nil {
		after = *req.After
	}

	// A finished run's whole record is already in the history the client just
	// loaded. A fresh reader (after < 0, nothing seen) that resubscribed would
	// replay the turn from the start and paint it a SECOND time, on top of the
	// history, so there is nothing to rejoin. A reader that already has frames
	// (after >= 0) is mid-stream and still gets the tail it missed. A client
	// explicitly replaying a nudged completion (Replay) is the exception: that run
	// is not in its history, so it is streamed from the start even when done.
	if active.Done() && after < 0 && !req.Replay {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.listen(w, r, active, after, nil)
}

// listen pumps a run's frames onto this response until the turn ends or the
// reader goes away.
//
// When the reader goes away, this function returns and nothing else happens:
// the run is not cancelled, not paused, not told. It keeps answering, keeps
// writing the conversation down, and keeps the frames, so whoever comes back
// gets the rest of it.
func (h *chatHandlers) listen(w http.ResponseWriter, r *http.Request, active *run.Run, after int, first *chat.Frame) {
	frames, detach := active.Subscribe(after)
	defer detach()

	// From here the response is a stream: errors are frames, not status codes,
	// because the status line is already sent.
	sse, err := chat.NewSSE(w)
	if err != nil {
		h.app.Log.Error().Err(err).Msg("open stream")
		writeError(w, http.StatusInternalServerError, "server_error", "streaming is not available")
		return
	}
	if first != nil {
		if err := sse.Write(*first); err != nil {
			return
		}
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			// The reader left. The turn does not care.
			return
		case frame, open := <-frames:
			if !open {
				return
			}
			if err := sse.Write(frame); err != nil {
				return
			}
		}
	}
}

// --- cancelling -----------------------------------------------------------------

type cancelRequest struct {
	ChatID string `json:"chat_id"`
}

// cancel stops a turn.
//
// It exists because detaching no longer stops anything. Before, closing the tab
// killed the vendor call, which was a bug dressed up as a feature: a network
// blip looked exactly like a decision. Now the only thing that means "stop" is
// a person saying so, and this is where they say it.
func (h *chatHandlers) cancel(w http.ResponseWriter, r *http.Request) {
	var req cancelRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	claims := claimsFrom(r)

	session, err := h.app.Store.Agent().GetSessionByUID(r.Context(), claims.WorkspaceID, req.ChatID)
	if err != nil || session.UserID != claims.UserID {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.app.Runs.Cancel(session.ID)
	w.WriteHeader(http.StatusNoContent)
}

type sayRequest struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`
}

// say hands the running turn something the person has just typed.
//
// A conversation answers one thing at a time and the server is strict about it:
// a second turn is refused with "this conversation is already answering". That
// rule is right, and it used to mean anything typed mid-answer was thrown away
// silently. But somebody typing while the assistant works is usually correcting
// what they asked for, and making them wait for an answer they have already
// changed their mind about is the wrong end of the trade.
//
// So it is neither refused nor queued behind the turn: it is given to the turn,
// and the loop folds it in at its next step, where a tool result would go. The
// model sees it before it decides what to do next.
//
// 204 means it was handed over. 409 means there was nothing running to hand it
// to, or the turn ended on the way, and the caller should send it as an ordinary
// message instead. Which is a race nobody can close, because a turn can finish
// between a person pressing a key and the request arriving.
func (h *chatHandlers) say(w http.ResponseWriter, r *http.Request) {
	var req sayRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "empty", "there is nothing to say")
		return
	}
	claims := claimsFrom(r)

	session, err := h.app.Store.Agent().GetSessionByUID(r.Context(), claims.WorkspaceID, req.ChatID)
	if err != nil || session.UserID != claims.UserID {
		writeError(w, http.StatusConflict, "not_running", "this conversation is not answering")
		return
	}
	if !h.app.Runs.Answering(session.ID) {
		writeError(w, http.StatusConflict, "not_running", "this conversation is not answering")
		return
	}
	// Put where the turn will look, not handed to the turn. The two are kept
	// apart on purpose: what was said is a value with a key, and the turn holding
	// it would make it a property of one run rather than of the conversation.
	if err := h.app.Said.Add(r.Context(), session.ID, strings.TrimSpace(req.Text)); err != nil {
		h.app.Log.Error().Err(err).Int64("session_id", session.ID).Msg("keep what was said")
		writeError(w, http.StatusInternalServerError, "server_error", "that could not be delivered")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type cancelDelegationRequest struct {
	ChatID       string `json:"chat_id"`
	DelegationID int64  `json:"delegation_id"`
	// FleetID cancels a whole batch instead of one agent. A chip is one agent
	// or one fleet, never both, so a request carries one of these or the other.
	FleetID int64 `json:"fleet_id"`
}

// cancelDelegation stops a background delegation the person no longer wants. It
// authorizes by the conversation: the delegation must belong to a session this
// person owns, or it does not exist as far as they are concerned.
func (h *chatHandlers) cancelDelegation(w http.ResponseWriter, r *http.Request) {
	var req cancelDelegationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	claims := claimsFrom(r)
	ctx := r.Context()

	session, err := h.app.Store.Agent().GetSessionByUID(ctx, claims.WorkspaceID, req.ChatID)
	if err != nil || session.UserID != claims.UserID {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if req.FleetID != 0 {
		// A fleet is authorized the same way and by the same rule: it belongs to
		// this conversation or it does not exist as far as this person goes.
		fleet, err := h.app.Store.Agent().Fleet(ctx, req.FleetID)
		if err != nil || fleet.SessionID != session.ID {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.app.CancelFleet(ctx, claims.WorkspaceID, req.FleetID)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	del, err := h.app.Store.Agent().GetDelegation(ctx, req.DelegationID)
	if err != nil || del.SessionID != session.ID {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.app.CancelBackground(req.DelegationID)
	w.WriteHeader(http.StatusNoContent)
}

// resume finishes a turn that was waiting on a person.
func (h *chatHandlers) resume(w http.ResponseWriter, r *http.Request, req streamRequest, userID, workspaceID int64) {
	if req.ResumeAction != resumeApproved && req.ResumeAction != resumeRejected {
		writeError(w, http.StatusBadRequest, "invalid_request", "resume_action must be approved or rejected")
		return
	}

	ctx := r.Context()
	// Claiming is atomic: two clicks on approve cannot run the action twice.
	snapshot, err := h.app.Store.Agent().ClaimPark(ctx, agent.HashToken(req.ResumeToken), req.ResumeAction)
	switch {
	case errors.Is(err, store.ErrExpired):
		// The approval was real, it simply came too late. Say so, rather
		// than pretending the token never existed.
		writeError(w, http.StatusGone, "expired", "this request expired and can no longer be approved")
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusGone, "invalid_token", "this request is no longer awaiting a decision")
		return
	case err != nil:
		writeStoreError(w, h.app, err)
		return
	}

	// The snapshot names the only person who may answer it. A token that
	// leaked to another user is not a way to approve their action.
	if snapshot.UserID != userID || snapshot.WorkspaceID != workspaceID {
		h.app.Log.Warn().Int64("user_id", userID).Int64("park_id", snapshot.ID).
			Msg("confirmation answered by the wrong user")
		writeError(w, http.StatusGone, "invalid_token", "this request is no longer awaiting a decision")
		return
	}

	// A DETACHED agent's card is answered off the Gateway's turn: that turn
	// already ended, and the agent runs somewhere else (a goroutine here, or a
	// worker for a fleet member). So the decision does not start a Gateway turn;
	// it re-enters the agent wherever it lives, and the card simply clears. The
	// eventual result arrives as its own server-initiated completion turn.
	if agent.Detached(snapshot.HandoffMode) {
		h.resumeDetached(w, r, snapshot, req)
		return
	}

	// The profile is resolved again rather than restored from the snapshot: an
	// administrator who revoked a tool while the card was on screen has
	// revoked it, and approving the card is not a way around that. The model
	// stays the one the parked turn ran on, because the transcript belongs to
	// it.
	profile, loadout, err := h.app.Resolve(ctx, app.ProfileRequest{
		WorkspaceID:      workspaceID,
		UserID:           userID,
		DeviceID:         req.DeviceID,
		WorkingFolder:    req.WorkingFolder,
		Machine:          req.Machine,
		Channel:          model.ChannelChat,
		PreferredModelID: snapshot.ModelID,
		SessionID:        snapshot.SessionID,
	})
	if err != nil {
		h.app.Log.Error().Err(err).Int64("user_id", userID).Msg("resolve profile")
		writeError(w, http.StatusInternalServerError, "server_error", "this assistant is not available right now")
		return
	}

	// "Approve and allow the rest this session": the approval switches the whole
	// conversation to auto, so the resumed continuation (and every later turn)
	// runs approval-gated tools without a card. Only on an approval, never a
	// rejection.
	if req.ResumeAction == resumeApproved && req.ApproveAll {
		if err := h.app.Store.Agent().SetSessionApprovalMode(ctx, snapshot.SessionID, model.ApprovalAuto); err != nil {
			h.app.Log.Error().Err(err).Int64("session_id", snapshot.SessionID).Msg("set auto-approve")
			writeError(w, http.StatusInternalServerError, "server_error", "this turn could not be resumed")
			return
		}
	}
	autoApprove, err := h.sessionAutoApproves(ctx, workspaceID, snapshot.SessionID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	resumeShow := h.maySee(ctx, userID)
	turn := agent.Turn{
		WorkspaceID: workspaceID,
		UserID:      userID,
		SessionID:   snapshot.SessionID,
		// The device that ANSWERED the card, not the one that asked for it.
		//
		// They are the same computer nearly always, because the card is in the
		// conversation the person was already having. When they differ, the
		// approved call runs where the person is now, which is at least
		// something they can see; the alternative is remembering the asking
		// device on the park row, which is a migration and a decision about a
		// case nobody has had yet.
		DeviceID:        req.DeviceID,
		Show:            &resumeShow,
		ModelID:         snapshot.ModelID,
		SystemPrompt:    profile.SystemPrompt,
		Tools:           loadout,
		Reasoning:       profile.Reasoning,
		ApprovalTTL:     profile.ApprovalTTL,
		MaxIterations:   profile.MaxIterations,
		MaxFleetAgents:  profile.MaxFleetAgents,
		AutoApprove:     autoApprove,
		Agent:           h.agentResolver(workspaceID, userID, app.Computer{DeviceID: req.DeviceID, Folder: req.WorkingFolder, Env: req.Machine}),
		StartBackground: h.app.StartBackground,
		StartFleet:      h.app.StartFleet,
		// The turn that parked could not name the conversation: it had no answer
		// yet. This one finishes the work, so it names it.
		NameConversation: h.sessionNeedsTitle(ctx, workspaceID, snapshot.SessionID),
		Resume: &agent.Resume{
			Snapshot: snapshot,
			Approved: req.ResumeAction == resumeApproved,
			Token:    req.ResumeToken,
		},
	}

	// The answer to a confirmation starts a new run, exactly like a new prompt.
	// The approved tool may take minutes, and the person who approved it is
	// free to close the tab: the work is under way and it is not theirs to hold
	// open.
	active, err := h.app.Runs.Start(ctx, turn)
	if err != nil {
		// The approval was CLAIMED above, and a claim is single-use. If the turn
		// did not start, nothing ran, so spending it would throw the person's
		// decision away: the card vanishes on the next reload and the tool call
		// behind it reads "approval required" for good. Put it back, token and
		// all, so the card they are looking at still works and they can answer
		// again. (The lane is busy when a message they sent while the card was
		// up is still being answered, which is ordinary, not exceptional.)
		if rerr := h.app.Store.Agent().ReopenPark(ctx, snapshot.ID); rerr != nil {
			h.app.Log.Error().Err(rerr).Int64("park_id", snapshot.ID).Msg("reopen park after a failed resume")
		}
		if errors.Is(err, run.ErrShuttingDown) {
			// The approval was put back above, so this is a retry, not a loss.
			writeError(w, http.StatusServiceUnavailable, "restarting",
				"this service is restarting. Approve that again in a moment.")
			return
		}
		if errors.Is(err, run.ErrBusy) {
			writeError(w, http.StatusConflict, "busy", "this conversation is already answering")
			return
		}
		h.app.Log.Error().Err(err).Int64("session_id", snapshot.SessionID).Msg("start run")
		writeError(w, http.StatusInternalServerError, "server_error", "this turn could not be resumed")
		return
	}
	h.listen(w, r, active, -1, nil)
}

// resumeDetached answers a card raised by an agent running away from the turn
// that started it. Unlike the Gateway's resume it starts no turn on this
// request: the agent continues where it lives, and the person's card clears. The
// result, if any, arrives later as its own completion turn (KB/27).
func (h *chatHandlers) resumeDetached(w http.ResponseWriter, r *http.Request, snapshot *model.ParkSnapshot, req streamRequest) {
	approved := req.ResumeAction == resumeApproved

	// "Approve and allow the rest this session" switches the whole conversation to
	// auto, so later approval-gated tools, the Gateway's or a background
	// agent's, run without a card.
	if approved && req.ApproveAll {
		if err := h.app.Store.Agent().SetSessionApprovalMode(r.Context(), snapshot.SessionID, model.ApprovalAuto); err != nil {
			h.app.Log.Error().Err(err).Int64("session_id", snapshot.SessionID).Msg("set auto-approve")
		}
	}

	// Where the agent lives decides how it is re-entered: a background agent is
	// a goroutine in this process, a fleet member is a job for whichever worker
	// is free. Through the app, so "approve all" goes the same way.
	h.app.ResumeDetached(snapshot, approved)

	// Answering this card advances the queue (KB/27): "approve all" lets every
	// card still waiting through with no further prompt, and an ordinary answer
	// surfaces just the next one. Either way the person is never shown more than
	// one background card at a time.
	if approved && req.ApproveAll {
		h.app.DrainApprovalQueue(snapshot.SessionID)
	} else {
		h.app.ReleaseNextApprovalCard(snapshot.SessionID)
	}

	// The conversation is no longer waiting on a person, so anything queued
	// behind that wait can run. Without this a completion turn scheduled while
	// the card was up waits for something else to come along and start it.
	h.app.Runs.Drain(snapshot.SessionID)

	h.streamConfirmResolved(w, req.ResumeToken, approved)
}

// streamConfirmResolved answers a resume with a single frame reporting the card
// resolved, then closes. It is the whole response when nothing streams behind the
// decision, as with a background agent's card.
func (h *chatHandlers) streamConfirmResolved(w http.ResponseWriter, token string, approved bool) {
	sse, err := chat.NewSSE(w)
	if err != nil {
		h.app.Log.Error().Err(err).Msg("open resume stream")
		writeError(w, http.StatusInternalServerError, "server_error", "streaming is not available")
		return
	}
	status := model.ParkApproved
	if !approved {
		status = model.ParkRejected
	}
	_ = sse.Write(chat.Frame{
		Type:    chat.FrameConfirmResolved,
		Final:   true,
		Message: chat.ConfirmResolved{Token: token, Status: status},
	})
}

// session continues the named conversation or starts one.
func (h *chatHandlers) session(r *http.Request, req streamRequest, userID, workspaceID int64, profile *model.Profile) (*model.AgentSession, bool, error) {
	if req.ChatID != "" {
		session, err := h.app.Store.Agent().GetSessionByUID(r.Context(), workspaceID, req.ChatID)
		if err != nil {
			return nil, false, err
		}
		// A session belongs to the person who started it.
		if session.UserID != userID {
			return nil, false, store.ErrNotFound
		}
		return session, false, nil
	}

	// The session records what shaped it. Months later, "why did it answer
	// that" is answerable: this agent, this version of this workflow.
	session := &model.AgentSession{
		WorkspaceID:       workspaceID,
		UserID:            userID,
		Channel:           model.ChannelChat,
		AgentID:           profile.AgentID,
		WorkflowVersionID: profile.WorkflowVersionID,
	}
	if err := h.app.Store.Agent().CreateSession(r.Context(), session); err != nil {
		return nil, false, err
	}
	return session, true, nil
}

// --- history -----------------------------------------------------------------

type historyRequest struct {
	ChatID string `json:"chat_id"`
	// Before is where to read back from: the seq of the oldest row the client
	// already has, or zero for the newest part of the conversation.
	//
	// A conversation is read in pages because it grows without end, and what a
	// person is looking at is the bottom of it. Sending the whole thing on
	// every open was a payload, a memory cost, and about half the cost of
	// typing a character with it on the screen.
	Before int `json:"before,omitempty"`
}

// historyResponse is what a reload gets: the conversation's own metadata and its
// messages, together, so the client needs one request, not one per field. New
// per-conversation metadata is added to `meta`, never as another endpoint.
type historyResponse struct {
	Meta     historyMeta      `json:"meta"`
	Messages []historyMessage `json:"messages"`
}

// historyMeta is the conversation's metadata the chat UI needs to render itself
// correctly on load: the approval mode, and the background delegations still in
// flight, so a reload rebuilds their chips (KB/27) rather than losing them until
// the next socket message.
type historyMeta struct {
	ApprovalMode       string           `json:"approval_mode"`
	RunningDelegations []delegationChip `json:"running_delegations"`
	// More says there is older conversation behind what was sent, and Oldest is
	// the seq to ask for it with. Both are about the page, not the
	// conversation: a client scrolling back sends Oldest as `before`.
	More   bool `json:"more"`
	Oldest int  `json:"oldest,omitempty"`
	// LiveRun names a turn still being answered in this conversation, or is
	// empty when nothing is happening in it.
	//
	// A turn outlives the page that asked for it (KB/17), so opening a
	// conversation means rejoining whatever is in flight. The client used to
	// find out by ASKING: every history load was followed by a POST to
	// /chat/attach, which in the overwhelmingly common case answered 204,
	// nothing is happening. That is a second round trip to be told no, on every
	// conversation anybody opens.
	//
	// The history knows. It is the same question, answered by the request
	// already being made, and the client rejoins only when there is something
	// to rejoin.
	LiveRun string `json:"live_run,omitempty"`
	// Accepts is what the composer may offer: the file types, whether anything
	// can be spoken, the ceiling on one file.
	//
	// A property of the Gateway rather than of this conversation, and it was its
	// own endpoint for that reason. But it is needed exactly when a conversation
	// is opened and never at any other time, so it was a second load request
	// beside this one, for a screen that had already asked its question.
	Accepts chatAcceptsResponse `json:"accepts"`
}

// delegationChip is one background delegation as the right-rail card renders it:
// the agent by name, what state it is in, when it started (for an elapsed
// clock), and its live progress (step, tokens, current activity). The id lets a
// socket message address the same card a reload built.
// delegationChip is the chat's chip, defined once in the app so the three
// producers (this reload, a background agent's live push, a fleet report's)
// cannot drift into three spellings of the same keys.
type delegationChip = app.Chip

// historyMessage is a past turn, in the shape the chat renders. It is the same
// timeline the live stream produces, rebuilt from what was persisted, so a
// reloaded conversation and a live one render through one code path.
type historyMessage struct {
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	Reasoning string          `json:"reasoning,omitempty"`
	Tools     []historyTool   `json:"tools,omitempty"`
	Confirm   *historyConfirm `json:"confirmation,omitempty"`
	// Attachments are the files sent with this message, so a reloaded
	// conversation still shows what was attached rather than a question with no
	// visible reason for the answer it got.
	Attachments []historyAttachment `json:"attachments,omitempty"`
}

// historyAttachment is a file as the chat shows it back: enough to draw the
// card, and the id to fetch the bytes with. The account of what it contained is
// not here, being for the model rather than for the person.
type historyAttachment struct {
	ID        string `json:"id"`
	FileName  string `json:"file_name"`
	FileType  string `json:"file_type"`
	SizeBytes int64  `json:"size_bytes"`
}

type historyTool struct {
	// ID names this call, so the chat can ask what it carried when somebody
	// opens it.
	ID                string `json:"id"`
	Name              string `json:"name"`
	FriendlyName      string `json:"friendly_name"`
	Status            string `json:"status"`
	DurationMS        int64  `json:"duration_ms"`
	RequestedApproval bool   `json:"requested_approval,omitempty"`
}

type historyConfirm struct {
	Token       string          `json:"token"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Severity    string          `json:"severity"`
	Details     json.RawMessage `json:"details,omitempty"`
	Status      string          `json:"status"`
}

// history returns a conversation. An empty session id means a fresh chat,
// which has no history rather than an error.
//
// The timeline is rebuilt from the steps: the text the assistant produced, the
// reasoning it produced on the way there, and every tool it called with the
// outcome. A step that only called tools has no text and is rendered as its
// chips, which is exactly what the live stream showed at the time.
func (h *chatHandlers) history(w http.ResponseWriter, r *http.Request) {
	var req historyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	claims := claimsFrom(r)
	show := h.maySee(r.Context(), claims.UserID)
	// A fresh or not-yet-owned conversation has no history and the default
	// (manual) approval mode.
	// A draft conversation still has a composer, so it still needs to know what
	// may be sent: `accepts` is on the empty answer too.
	empty := historyResponse{
		Meta: historyMeta{
			ApprovalMode:       model.ApprovalManual,
			RunningDelegations: []delegationChip{},
			Accepts:            h.acceptsFor(r),
		},
		Messages: []historyMessage{},
	}
	if req.ChatID == "" {
		writeJSON(w, http.StatusOK, empty)
		return
	}

	ctx := r.Context()
	session, err := h.app.Store.Agent().GetSessionByUID(ctx, claims.WorkspaceID, req.ChatID)
	if err != nil || session.UserID != claims.UserID {
		// A conversation you do not own does not exist as far as you are
		// concerned.
		writeJSON(w, http.StatusOK, empty)
		return
	}

	// The newest rows, or the ones before what the client already has.
	steps, more, err := h.app.Store.Agent().TranscriptPage(ctx, session.ID, req.Before, model.HistoryRows)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	// A turn still in flight is about to be streamed, frame by frame, by the
	// attach that follows this call. Its half-written step is left out here, or
	// the client would render the checkpoint AND then the live stream of the
	// same words. When no run is live, the partial step is exactly what should
	// be shown: it is what the assistant managed to say before the lights went
	// out, and it is the record.
	if active, ok := h.app.Runs.Latest(session.ID); ok && !active.Done() {
		steps = withoutTrailingPartial(steps)
	}

	out := make([]historyMessage, 0, len(steps))
	for _, step := range steps {
		// An agent's inner steps are durable now, but a reloaded conversation
		// shows a delegation the way the live one did: as the Gateway's `agent`
		// chip and its result, not the agent's own steps replayed inline.
		if step.AgentKey != "" {
			continue
		}
		message := historyMessage{
			ID:      itoa64(step.ID),
			Role:    step.Kind,
			Content: step.Text,
		}
		// The same rule the live stream applies, applied again here. A person
		// who was not shown the thinking while it happened must not find it by
		// reloading, and the transcript keeps it either way: what this decides
		// is who is shown the record, not whether it is kept.
		if show.Reasoning {
			message.Reasoning = step.Reasoning
		}
		// The same rule again, for what the assistant DID. A person the live
		// stream withheld the tool rows from must not find them by opening the
		// conversation again: the frames and this answer say the same thing, or
		// the permission is a delay rather than a decision.
		for _, call := range step.ToolCalls {
			if !show.Tools {
				break
			}
			// The row, and the id to open it with. What it was sent and what it
			// answered are not here: they are the largest thing in a
			// conversation and the one thing a person may not be allowed to
			// see, so they are fetched when somebody asks (`/chat/tool-call`).
			//
			// No id on an INTERNAL tool, so it cannot be opened at all. Looking
			// up an ability, handing work to an agent, keeping a note: these are
			// how the assistant is put together, not work it did for somebody,
			// and their arguments are our own plumbing. Withholding the id is
			// structural — there is nothing to open rather than a panel that
			// refuses.
			message.Tools = append(message.Tools, historyTool{
				ID:                openableID(h.app, call),
				Name:              call.ToolName,
				FriendlyName:      call.FriendlyName,
				Status:            call.Status,
				DurationMS:        call.DurationMS,
				RequestedApproval: call.RequestedApproval,
			})
		}
		for _, id := range step.Attachments {
			at, err := h.app.Store.Attachments().ByID(ctx, claims.WorkspaceID, id)
			if err != nil {
				// A file that has been cleaned up is not a reason to lose the
				// message it came with.
				continue
			}
			message.Attachments = append(message.Attachments, historyAttachment{
				ID: at.PublicID, FileName: at.FileName,
				FileType: at.FileType, SizeBytes: at.SizeBytes,
			})
		}
		if message.Content == "" && len(message.Tools) == 0 && message.Reasoning == "" &&
			len(message.Attachments) == 0 {
			// An empty step is the wreckage of an interrupted turn. There is
			// nothing to show and nothing worth explaining.
			continue
		}
		out = append(out, message)
	}

	// A conversation waiting on a person shows its card again. Confirmation cards
	// are live frames, never written into the transcript, so the reload rebuilds
	// the pending one from the park it is waiting on.
	if card := h.restorePendingCard(ctx, claims, session); card != nil {
		out = append(out, *card)
	}

	// Every delegation this conversation has had, not only the ones still going.
	// A chip that vanishes when the work finishes leaves the person with no
	// record of what was done for them, which is most of what they wanted to
	// know. Newest first, so a new one appears at the top and pushes the rest
	// down, and capped so a long conversation does not become a log.
	running, err := h.app.Store.Agent().SessionDelegations(ctx, session.ID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	// Something still answering in this conversation is named here, so the client
	// rejoins it instead of asking whether there is anything to rejoin. Only a
	// run that is still going: a finished one is already in the messages above,
	// and rejoining it would paint the turn a second time on top of them.
	live := ""
	if active, ok := h.app.Runs.Latest(session.ID); ok && !active.Done() {
		live = active.UID
	}

	writeJSON(w, http.StatusOK, historyResponse{
		Meta: historyMeta{
			ApprovalMode:       session.ApprovalMode,
			RunningDelegations: h.delegationChips(ctx, claims.WorkspaceID, session.ID, running),
			LiveRun:            live,
			Accepts:            h.acceptsFor(r),
			More:               more,
			Oldest:             oldestSeq(steps),
		},
		Messages: out,
	})
}

// oldestSeq is where a client asks for the page before this one.
func oldestSeq(steps []*model.AgentStep) int {
	if len(steps) == 0 {
		return 0
	}
	return steps[0].Seq
}

// delegationChips turns the still-running background delegations into the chips a
// reload renders, resolving each agent's friendly name from the workspace's
// agents (falling back to the key if it was removed).
// maxDelegationChips bounds the column. A conversation that ran forty agents
// should show what it has been doing lately, not become a scrolling log.
const maxDelegationChips = 20

func (h *chatHandlers) delegationChips(ctx context.Context, workspaceID, sessionID int64, all []*model.AgentDelegation) []delegationChip {
	chips := []delegationChip{}
	if len(all) == 0 {
		return chips
	}
	names := h.app.AgentNames(ctx, workspaceID)

	// A fleet is ONE chip, so its members come out of the list and are replaced
	// by a chip of their own (KB/27). Twenty chips saying almost the same thing
	// is a wall; what somebody wants to know about a batch is how many there
	// are and how many are back.
	alone, byFleet := splitFleets(all)
	if len(byFleet) > 0 {
		fleets, err := h.app.Store.Agent().SessionFleets(ctx, sessionID)
		if err != nil {
			h.app.Log.Error().Err(err).Int64("session_id", sessionID).Msg("load fleets for chips")
		}
		for _, f := range fleets {
			members := byFleet[f.ID]
			if len(members) == 0 {
				continue
			}
			chips = append(chips, app.FleetChip(f, members, names))
		}
	}

	for _, d := range alone {
		chips = append(chips, app.AgentChip(d, names[d.AgentKey]))
	}

	// Newest first: the chip that just appeared belongs at the top. Capped, so a
	// conversation that ran forty agents shows what it has been doing lately
	// rather than becoming a scrolling log.
	sort.SliceStable(chips, func(i, j int) bool { return chips[i].CreatedAt > chips[j].CreatedAt })
	if len(chips) > maxDelegationChips {
		chips = chips[:maxDelegationChips]
	}
	return chips
}

// splitFleets separates the delegations that stand alone from the ones that
// belong to a batch, keeping each batch's members in the order they were asked
// for.
func splitFleets(all []*model.AgentDelegation) ([]*model.AgentDelegation, map[int64][]*model.AgentDelegation) {
	alone := make([]*model.AgentDelegation, 0, len(all))
	fleets := map[int64][]*model.AgentDelegation{}
	for _, d := range all {
		if d.FleetID == nil {
			alone = append(alone, d)
			continue
		}
		fleets[*d.FleetID] = append(fleets[*d.FleetID], d)
	}
	return alone, fleets
}

type approvalModeRequest struct {
	ChatID string `json:"chat_id"`
	Mode   string `json:"mode"` // manual | auto
}

// approvalMode switches a conversation between asking for every approval and
// auto-approving for the rest of the session. It is the person's own choice on
// their own conversation, and it is reversible.
func (h *chatHandlers) approvalMode(w http.ResponseWriter, r *http.Request) {
	var req approvalModeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !model.IsApprovalMode(req.Mode) {
		writeError(w, http.StatusBadRequest, "invalid_request", "mode must be manual or auto")
		return
	}
	claims := claimsFrom(r)
	ctx := r.Context()
	session, err := h.app.Store.Agent().GetSessionByUID(ctx, claims.WorkspaceID, req.ChatID)
	if err != nil || session.UserID != claims.UserID {
		// A conversation you do not own does not exist as far as you are concerned.
		writeError(w, http.StatusNotFound, "not_found", "no such conversation")
		return
	}
	if err := h.app.Store.Agent().SetSessionApprovalMode(ctx, session.ID, req.Mode); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"mode": req.Mode})
}

// sessionAutoApproves reports whether a conversation is set to run
// approval-gated tools without a card.
func (h *chatHandlers) sessionAutoApproves(ctx context.Context, workspaceID, sessionID int64) (bool, error) {
	sess, err := h.app.Store.Agent().GetSession(ctx, workspaceID, sessionID)
	if err != nil {
		return false, err
	}
	return sess.ApprovalMode == model.ApprovalAuto, nil
}

// sessionNeedsTitle reports that a conversation is still unnamed.
//
// A turn that stops to ask a person ends without naming the conversation, since
// it has no answer to name it from yet. So the turn that finishes the work has
// to pick it up, or a conversation whose very first message needed an approval
// keeps its default name for good.
func (h *chatHandlers) sessionNeedsTitle(ctx context.Context, workspaceID, sessionID int64) bool {
	sess, err := h.app.Store.Agent().GetSession(ctx, workspaceID, sessionID)
	return err == nil && sess.Title == ""
}

// withoutTrailingPartial drops the step a live run is still writing.
func withoutTrailingPartial(steps []*model.AgentStep) []*model.AgentStep {
	if n := len(steps); n > 0 && steps[n-1].Partial {
		return steps[:n-1]
	}
	return steps
}

// restorePendingCard rebuilds the confirmation card a session is waiting on, so
// a reloaded conversation can still be answered. The card is not in the
// transcript (it is a live frame), so it is rebuilt from the park: the display
// text is re-derived from the parked tool's own schema (live, so a revoked tool
// yields no card), and a FRESH token is minted, because the original was never
// stored, only hashed. Returns nil when nothing is pending or the tool is gone.
func (h *chatHandlers) restorePendingCard(ctx context.Context, claims *auth.Claims, session *model.AgentSession) *historyMessage {
	park, err := h.app.Store.Agent().PendingPark(ctx, session.ID)
	if err != nil {
		return nil // nothing pending (ErrNotFound), or a read error: no card
	}
	schema, ok := h.parkedToolSchema(ctx, claims, park)
	if !ok {
		return nil // the tool can no longer be resolved; the card cannot be honoured
	}
	token, tokenHash, err := agent.NewToken()
	if err != nil {
		h.app.Log.Error().Err(err).Msg("mint reload card token")
		return nil
	}
	if err := h.app.Store.Agent().RotateParkToken(ctx, park.ID, tokenHash); err != nil {
		return nil // another request resolved it first: no card to show
	}
	return &historyMessage{
		ID:   "confirm_" + park.ToolCallID,
		Role: "confirm",
		Confirm: &historyConfirm{
			Token:       token,
			Title:       schema.ConfirmTitle(),
			Description: schema.ConfirmDescription(),
			Severity:    string(schema.Risk),
			Details:     park.ToolArgs,
			Status:      "pending",
		},
	}
}

// parkedToolSchema resolves the parked tool's schema LIVE, from the Gateway's
// loadout or, for a delegation, the agent's. Live resolution means a tool
// an administrator revoked while the card waited yields nothing, and the card is
// not restored, matching how a resume re-checks permissions.
func (h *chatHandlers) parkedToolSchema(ctx context.Context, claims *auth.Claims, park *model.ParkSnapshot) (tool.Schema, bool) {
	if park.AgentKey != "" {
		// No computer: this only reads the parked tool's schema, to decide
		// whether the card can still be drawn. Nothing is run, so nothing needs
		// to reach anywhere.
		sub, err := h.app.ResolveAgent(ctx, claims.WorkspaceID, claims.UserID, app.Computer{}, park.AgentKey)
		if err != nil {
			return tool.Schema{}, false
		}
		return sub.Tools.Schema(park.ToolName)
	}
	_, loadout, err := h.app.Resolve(ctx, app.ProfileRequest{
		WorkspaceID:      claims.WorkspaceID,
		UserID:           claims.UserID,
		Channel:          model.ChannelChat,
		PreferredModelID: park.ModelID,
		SessionID:        park.SessionID,
	})
	if err != nil {
		return tool.Schema{}, false
	}
	return loadout.Schema(park.ToolName)
}

// maxAttachmentsPerTurn bounds one message. Reading each file is a model call,
// so a hundred at once is a hundred calls and a very long wait; a handful is
// what a person actually attaches to a question.
const maxAttachmentsPerTurn = 10

// resolveAttachments turns the public ids a caller sent into the files they
// name, refusing anything that is not this person's.
//
// It answers before the turn starts. A file that is missing or somebody else's
// has to be a refusal the person can act on, not a turn that runs and answers
// about a document it never had.
func (h *chatHandlers) resolveAttachments(w http.ResponseWriter, r *http.Request, ids []string) ([]string, bool) {
	if len(ids) == 0 {
		return nil, true
	}
	if len(ids) > maxAttachmentsPerTurn {
		writeError(w, http.StatusBadRequest, "too_many_files",
			fmt.Sprintf("send at most %d files with one message", maxAttachmentsPerTurn))
		return nil, false
	}
	claims := claimsFrom(r)
	for _, id := range ids {
		if _, err := h.app.Store.Attachments().ByPublicID(r.Context(), claims.WorkspaceID, claims.UserID, id); err != nil {
			writeError(w, http.StatusBadRequest, "unknown_file",
				"one of those files is not available; upload it again")
			return nil, false
		}
	}
	return ids, true
}

// maySee is what this person may be told of HOW an answer was reached.
//
// Asked live, per request, like every other permission here: somebody whose
// role changed this morning sees the change on their next message rather than
// when their token expires.
//
// A failure to ask is a refusal, not a pass. This is the one direction that is
// safe to be wrong in: showing less than somebody is entitled to is a support
// question, and showing more is a disclosure.
func (h *chatHandlers) maySee(ctx context.Context, userID int64) chat.Show {
	show := chat.Show{}
	for _, allowed := range []struct {
		permission string
		field      *bool
	}{
		{model.PermChatsSeeReasoning, &show.Reasoning},
		{model.PermChatsSeeTools, &show.Tools},
	} {
		ok, err := h.app.Authorize(ctx, userID, allowed.permission)
		if err != nil {
			h.app.Log.Error().Err(err).Int64("user_id", userID).
				Str("permission", allowed.permission).Msg("resolve what a person may see")
			continue
		}
		*allowed.field = ok
	}
	return show
}

// toolCall answers what one finished call carried.
//
// A finished one only: a call still running has no result to show, and half of
// one is a screen that has to explain itself. The row says "running" until it
// does not, and there is nothing to open until then.
type toolCallRequest struct {
	ID string `json:"id"`
}

type toolCallBody struct {
	Name         string `json:"name"`
	FriendlyName string `json:"friendly_name"`
	Status       string `json:"status"`
	DurationMS   int64  `json:"duration_ms"`
	// Where it ran, what it was asked, and what came back: three groups,
	// already decided here. The client renders what it is given and knows no
	// tool's field by name, because what is worth reading is the TOOL's
	// account of itself (tool.Display) and not the chat's guess at it.
	Where    []shownField `json:"where,omitempty"`
	Sent     []shownField `json:"sent,omitempty"`
	Answered []shownField `json:"answered,omitempty"`
	// Error is what went wrong, when something did. It is the tool's own
	// words, which is what somebody opening a failed call wants.
	Error string `json:"error,omitempty"`
}

// shownField is one field of a call, with what it IS rather than only what it
// is called: a command is drawn the way a terminal draws one, and output keeps
// its lines and needs no label.
type shownField struct {
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
	As    string          `json:"as,omitempty"`
}

func (h *chatHandlers) toolCall(w http.ResponseWriter, r *http.Request) {
	var req toolCallRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(req.ID), 10, 64)
	if err != nil || id <= 0 {
		writeInvalidFields(w, fieldErrors{"id": "which tool call?"})
		return
	}

	claims := claimsFrom(r)
	call, err := h.app.Store.Agent().ToolCall(r.Context(), claims.WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	// And it has to be theirs. The store scopes by workspace; this scopes by
	// person, because a conversation is somebody's own and its tool calls are
	// part of it.
	session, err := h.app.Store.Agent().GetSession(r.Context(), claims.WorkspaceID, call.SessionID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	if session.UserID != claims.UserID {
		writeError(w, http.StatusNotFound, "not_found", "no such tool call")
		return
	}
	// And what the list does not offer, this does not serve. The rule lives in
	// one place (openableID); asking it again here is what stops it being a
	// property of the drawing rather than of the answer.
	if openableID(h.app, call) == "" {
		writeError(w, http.StatusNotFound, "not_found", "no such tool call")
		return
	}

	where, sent, answered := h.presentCall(r.Context(), claims.WorkspaceID, call)
	writeJSON(w, http.StatusOK, toolCallBody{
		Name:         call.ToolName,
		FriendlyName: call.FriendlyName,
		Status:       call.Status,
		DurationMS:   call.DurationMS,
		Where:        where,
		Sent:         sent,
		Answered:     answered,
		Error:        call.ErrorText,
	})
}

// presentCall turns a stored call into the three groups a person reads, under
// the tool's own account of what is worth reading.
//
// A call that FAILED is shown whole, whatever the tool says. Every field is
// potentially the answer there, and a panel that hides the exit code of a
// command that did not work wastes somebody's afternoon.
//
// A tool that has declared nothing is also shown whole. That is the right
// default: the alternative is a panel quietly hiding the one field somebody
// needed, and a tool nobody has thought about has not been thought about.
func (h *chatHandlers) presentCall(ctx context.Context, workspaceID int64, call *model.ToolCall) (where, sent, answered []shownField) {
	return presentUnder(h.displayOf(ctx, workspaceID, call.ToolName), call)
}

// presentUnder is that decision, given the declaration: which fields, in which
// order, and what each one is. Separate from the lookup so it can be tested as
// what it is, a rule about a call and a declaration, with no server around it.
func presentUnder(display tool.Display, call *model.ToolCall) (where, sent, answered []shownField) {
	args, result := fieldsOf(call.Args), fieldsOf(call.Result)
	failed := call.Status == model.ToolCallFailed || call.Status == model.ToolCallRejected

	// Where it ran, or what it read, comes from whichever side carries it: a
	// tool is often not TOLD where and reports where it went.
	//
	// When both sides carry it, the ANSWER wins. They usually differ only in
	// spelling (a model asks for an absolute path, the tool answers with the
	// short one), and where it actually happened is the truthful half of that.
	// It used to keep neither in that case, which is how a read of a file
	// showed an offset and a limit and never said which file: a field that
	// does not lift is not in the sent or answered lists either, so it fell
	// through both and vanished.
	for _, name := range display.Where {
		asked, inAsked := args[name]
		got, inGot := result[name]
		if !inAsked && !inGot {
			continue
		}
		value := got
		if !inGot {
			value = asked
		}
		if empty(value) {
			continue
		}
		where = append(where, shownField{Name: name, Value: value})
		delete(args, name)
		delete(result, name)
	}

	// Nothing is said twice. A failed call carries its error in its own
	// section, and the result almost always carries the same words again; the
	// success flag beside them is the row's own status a third time.
	dropWhatIsSaidElsewhere(result, call)

	// A tool that has declared nothing is shown whole: a tool nobody has
	// thought about has not been thought about, and hiding the one field
	// somebody needed is the worse way to be wrong.
	if len(display.Sent) == 0 && len(display.Answered) == 0 {
		return where, everything(args), everything(result)
	}

	sent, answered = chosen(display.Sent, args), chosen(display.Answered, result)

	// A call that FAILED is read by a person who wants to know what went
	// wrong, and that is the command, one error line, and whatever it printed.
	// Showing the whole payload instead was this panel's own idea of being
	// careful, and what it produced was the error twice, a success flag and an
	// exit code around it: three ways of reading one fact.
	//
	// The one thing worth keeping from being careful: when the tool's own
	// account leaves NOTHING to show, fall back to the rest. A person looking
	// at a failure must never be handed a blank.
	if failed && len(answered) == 0 {
		answered = everything(result)
	}
	return where, sent, answered
}

// dropWhatIsSaidElsewhere removes the fields of a result that repeat something
// the panel already shows: the error, which has a section of its own, and the
// success flag that only agrees with the row's status.
//
// Both names come from the toolkit that WRITES them (toolkit.FieldSuccess), so
// this is our own result shape rather than any tool's, and the two cannot
// drift apart into a filter that quietly stops matching.
//
// Only an EXACT repeat. A field that elaborates ("error_detail", a message
// that says more than the error line) is not the same thing said twice and
// stays, because on a call that went wrong the extra sentence is often the
// useful one.
func dropWhatIsSaidElsewhere(result map[string]json.RawMessage, call *model.ToolCall) {
	failed := call.Status == model.ToolCallFailed || call.Status == model.ToolCallRejected
	for name, value := range result {
		var text string
		if call.ErrorText != "" && json.Unmarshal(value, &text) == nil && text == call.ErrorText {
			delete(result, name)
			continue
		}
		if name != toolkit.FieldSuccess {
			continue
		}
		var flag bool
		if json.Unmarshal(value, &flag) == nil && flag == !failed {
			delete(result, name)
		}
	}
}

// displayOf is a tool's own account of what is worth reading, from wherever
// that tool is defined: a built-in carries it on its schema, and a tool an
// administrator created carries it on the template it was made from, because
// the shape of a call is the template's and every instance shares it.
//
// A tool PROJECTED from an MCP server has no account and never will: its
// results are a third party's, in whatever shape that party chose, and there
// is nobody here who can honestly say which of their fields matters. Those
// calls are shown exactly as they came back. The same goes for a tool that no
// longer exists (revoked, or a service that dropped it), which is what
// somebody asking "what happened here" wants of a call whose tool is gone.
func (h *chatHandlers) displayOf(ctx context.Context, workspaceID int64, name string) tool.Display {
	if schema, known := h.app.Tools.Lookup(name); known {
		return schema.Shown
	}
	tools, err := h.app.Store.Tools().List(ctx, workspaceID)
	if err != nil {
		return tool.Display{}
	}
	for _, t := range tools {
		// No template means nothing was built from a recipe of ours: an MCP
		// projection, whose answer is shown as it came.
		if t.Name != name || t.Template == "" {
			continue
		}
		if made, ok := h.app.Templates.Get(t.Template); ok {
			if says, ok := made.(template.Displayed); ok {
				return says.Display()
			}
		}
		return tool.Display{}
	}
	return tool.Display{}
}

// fieldsOf reads a call's arguments or result as named values. Anything that is
// not an object (a bare string a tool answered with) comes back under no name,
// which is what it is.
func fieldsOf(raw json.RawMessage) map[string]json.RawMessage {
	fields := map[string]json.RawMessage{}
	if len(raw) == 0 {
		return fields
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		fields[""] = raw
	}
	return fields
}

// everything is every field there is, named in a stable order so the same call
// reads the same way twice.
func everything(fields map[string]json.RawMessage) []shownField {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]shownField, 0, len(names))
	for _, name := range names {
		if empty(fields[name]) {
			continue
		}
		out = append(out, shownField{Name: name, Value: fields[name]})
	}
	return out
}

// chosen is what the tool named, in the order it named them, skipping what is
// not there. A field a tool asks for and never sent is not an empty row.
func chosen(want []tool.Shown, fields map[string]json.RawMessage) []shownField {
	out := make([]shownField, 0, len(want))
	for _, shown := range want {
		value, ok := fields[shown.Field]
		if !ok || empty(value) {
			continue
		}
		// Some kinds are TWO fields and one thing: a table is its rows and
		// their column names, a body is the payload and the content type that
		// says how to read it. They are put together here, so what reaches the
		// page is the thing rather than its parts.
		if shown.With != "" && !empty(fields[shown.With]) {
			paired, err := json.Marshal(map[string]json.RawMessage{
				shown.As.Paired(): fields[shown.With],
				shown.Field:       value,
			})
			if err != nil {
				continue
			}
			out = append(out, shownField{Name: shown.Field, Value: paired, As: string(shown.As)})
			continue
		}
		out = append(out, shownField{Name: shown.Field, Value: value, As: string(shown.As)})
	}
	return out
}

// empty is a value with nothing in it TO READ: null, an empty object or list,
// and a string that is blank or nothing but whitespace.
//
// The whitespace part is not fussiness. A pager quit with `q` answers with a
// hundred and fifty spaces, which is a screen being cleared: it passed as a
// value, and the panel drew a heading over something invisible, which reads as
// a bug in the panel rather than a command that printed nothing.
func empty(value json.RawMessage) bool {
	switch strings.TrimSpace(string(value)) {
	case "", "null", "{}", "[]":
		return true
	}
	var text string
	if err := json.Unmarshal(value, &text); err == nil {
		return strings.TrimSpace(text) == ""
	}
	return false
}

// openableID is the id a tool row is opened by, or nothing for a tool that
// should not be opened.
//
// An internal tool is infrastructure: tool_guide, delegation, remember, the
// background controls. What they were sent is our own wiring and reads as noise
// beside the work somebody actually asked for. Everything else is openable, a
// custom tool and a projected one included: those do real work, and what they
// carried is exactly what somebody wants to see.
//
// Asked of the app, not of the registry. Half of these tools are never
// registered (they are built per turn from the roster), so a registry lookup
// answers "not ours" for delegate and delegate_fleet, which is how the fleet
// tool ended up with an arrow on it.
func openableID(a *app.App, call *model.ToolCall) string {
	if a.InternalTool(call.ToolName) {
		return ""
	}
	return itoa64(call.ID)
}
