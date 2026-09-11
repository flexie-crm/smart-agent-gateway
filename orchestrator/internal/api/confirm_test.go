package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/run"
	"flexie.io/sag/internal/tool"
)

// Confirmation is park-and-resume: the turn stops, the stream CLOSES, and the
// decision arrives later as a fresh request. Nothing is held open waiting for
// a person, which is what lets an approval take a minute or a day.

// parkingVendor scripts a model that calls the approval-gated tool, then (on
// the resumed turn) narrates whatever came back.
func parkingVendor(t *testing.T, modelID int64, narration string) *fakeVendor {
	t.Helper()
	args := `{\"model_id\":` + strconv.FormatInt(modelID, 10) + `,\"status\":\"disabled\"}`
	return newFakeVendor(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"set_model_status","arguments":"` + args + `"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		},
		[]string{
			`{"choices":[{"index":0,"delta":{"content":"` + narration + `"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
	)
}

func TestConfirmationParksTheTurnAndClosesTheStream(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	// The vendor row is created first so its model id can be the target.
	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	vendor := parkingVendor(t, modelID, "Done.")
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{
		"prompt": "disable that model", "model_id": modelID,
	})

	// The card is the LAST frame: the stream is over, and no connection is
	// being held while a human decides.
	last := frames[len(frames)-1]
	if last.Type != chat.FrameConfirmRequest || !last.Final {
		t.Fatalf("the turn did not park with a confirmation: %+v", frames)
	}
	card := confirmRequest(t, last)
	if card.Token == "" || card.ExpiresAt == 0 {
		t.Fatalf("the card is not answerable: %+v", card)
	}
	if card.Title == "" || card.Description == "" {
		t.Fatalf("the card does not say what it is asking for: %+v", card)
	}
	// The severity tells a UI how loudly to warn.
	if card.Severity != string(tool.RiskAdminAction) {
		t.Fatalf("the card does not carry the action's risk: %q", card.Severity)
	}

	// Nothing happened yet: the tool has NOT run.
	ctx := context.Background()
	target, err := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, modelID)
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	if target.Status != model.StatusActive {
		t.Fatal("the action was performed before the user approved it")
	}

	// The session says what it is doing: waiting on a person, not running.
	session, err := env.app.Store.Agent().GetSession(ctx, env.ws.ID, env.sessionOf(frames).ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.Status != model.SessionWaitingApproval {
		t.Fatalf("session status is %q, expected waiting_approval", session.Status)
	}
}

// Approve: the action runs with the arguments from the card, and the model
// gets to narrate the outcome.
func TestApprovalRunsTheActionAndResumesTheTurn(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	vendor := parkingVendor(t, modelID, "The model is now disabled.")
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{
		"prompt": "disable that model", "model_id": modelID,
	})
	card := confirmRequest(t, frames[len(frames)-1])

	resumed := env.streamTurn(token, map[string]any{
		"resume_token": card.Token, "resume_action": "approved",
	})

	// The decision is echoed, the tool reports completion, and the model
	// speaks again: the turn genuinely continued.
	if len(framesOfType(resumed, chat.FrameConfirmResolved)) != 1 {
		t.Fatalf("the decision was not reported: %+v", resumed)
	}
	done := framesOfType(resumed, chat.FrameTool)
	if len(done) != 1 {
		t.Fatalf("the approved tool did not report a result: %+v", resumed)
	}
	msg := toolMessage(t, done[0])
	if msg.Status != model.ToolCallCompleted || !msg.RequestedApproval {
		t.Fatalf("tool frame does not record an approved execution: %+v", msg)
	}
	if deltaText(resumed) != "The model is now disabled." {
		t.Fatalf("the model did not narrate the outcome: %q", deltaText(resumed))
	}

	// And the world actually changed.
	ctx := context.Background()
	target, err := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, modelID)
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	if target.Status != model.StatusDisabled {
		t.Fatal("the approved action did not take effect")
	}
}

// statusCall scripts the model calling set_model_status on a given model.
func statusCall(modelID int64, status, callID string) []string {
	args := `{\"model_id\":` + strconv.FormatInt(modelID, 10) + `,\"status\":\"` + status + `\"}`
	return []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"` + callID + `","function":{"name":"set_model_status","arguments":"` + args + `"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

// "Approve and allow the rest this session" runs this action AND switches the
// conversation to auto, so the NEXT approval-gated action runs with no card.
func TestApproveAllTurnsOnAutoApproveForTheSession(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	target := env.spareModel() // toggled without disturbing the model the agent runs on

	vendor := newFakeVendor(t,
		statusCall(target, "disabled", "call_a"), // turn 1: parks
		answerChunks("Disabled it."),             // turn 1: resume narration
		statusCall(target, "active", "call_b"),   // turn 2: must NOT park (auto)
		answerChunks("Enabled it."),              // turn 2: answer
	)
	env.pointModelAt(modelID, vendor)

	ctx := context.Background()
	// A conversation that already has a title, so the turn that finishes the
	// parked work does not also name it. Naming is a model call like any other,
	// and this vendor answers from a script: an unscripted call would be served
	// the next reply in the list and every turn after it would read the wrong one.
	session := env.seededChat(user.ID)
	chatID := session.UID
	frames := env.streamTurn(token, map[string]any{
		"prompt": "disable the spare", "model_id": modelID, "chat_id": chatID,
	})
	card := confirmRequest(t, frames[len(frames)-1])

	// Approve AND allow the rest this session.
	resumed := env.streamTurn(token, map[string]any{
		"resume_token": card.Token, "resume_action": "approved", "approve_all": true,
	})
	if fr := resumed[len(resumed)-1]; fr.Type != chat.FrameResult {
		t.Fatalf("the approval did not complete: %+v", fr)
	}
	if m, _ := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, target); m.Status != model.StatusDisabled {
		t.Fatal("the approved action did not run")
	}
	if sess, _ := env.app.Store.Agent().GetSessionByUID(ctx, env.ws.ID, chatID); sess.ApprovalMode != model.ApprovalAuto {
		t.Fatal("approve-all did not switch the session to auto")
	}

	// Turn 2: the same kind of action now runs WITHOUT a card.
	next := env.streamTurn(token, map[string]any{"prompt": "now enable it", "model_id": modelID, "chat_id": chatID})
	if cards := framesOfType(next, chat.FrameConfirmRequest); len(cards) != 0 {
		t.Fatalf("auto-approve still asked for a card: %+v", next)
	}
	if last := next[len(next)-1]; last.Type != chat.FrameResult {
		t.Fatalf("the auto-approved turn did not complete: %+v", last)
	}
	if m, _ := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, target); m.Status != model.StatusActive {
		t.Fatal("the auto-approved action did not run")
	}
}

// The in-session toggle reads and switches the approval mode, and refuses a
// nonsense mode.
func TestApprovalModeToggle(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")
	vendor := newFakeVendor(t)
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": modelID})
	chatID := chatIDOf(t, frames)

	// The current mode is read as part of the history response, not a separate
	// request.
	read := func() string {
		rec := env.do(http.MethodPost, "/v1/chat/history", token, map[string]string{"chat_id": chatID})
		env.expectStatus(rec, http.StatusOK)
		var resp historyResponse
		env.decode(rec, &resp)
		return resp.Meta.ApprovalMode
	}

	if got := read(); got != model.ApprovalManual {
		t.Fatalf("a fresh conversation should default to manual, got %q", got)
	}
	env.expectStatus(env.do(http.MethodPost, "/v1/chat/approval-mode", token,
		map[string]string{"chat_id": chatID, "mode": "auto"}), http.StatusOK)
	if got := read(); got != model.ApprovalAuto {
		t.Fatalf("the toggle did not switch to auto, got %q", got)
	}
	env.expectStatus(env.do(http.MethodPost, "/v1/chat/approval-mode", token,
		map[string]string{"chat_id": chatID, "mode": "manual"}), http.StatusOK)
	if got := read(); got != model.ApprovalManual {
		t.Fatalf("the toggle did not switch back to manual, got %q", got)
	}
	// A nonsense mode is refused.
	env.expectStatus(env.do(http.MethodPost, "/v1/chat/approval-mode", token,
		map[string]string{"chat_id": chatID, "mode": "bogus"}), http.StatusBadRequest)
}

// A pending card survives a page reload. It is a live frame, never written to
// the transcript, so the history endpoint rebuilds it from the park: with a
// FRESH token (the original was only ever hashed), and it genuinely works, the
// action runs when the reloaded card is approved.
func TestReloadRestoresThePendingCard(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	vendor := parkingVendor(t, modelID, "The model is now disabled.")
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{"prompt": "disable that model", "model_id": modelID})
	liveCard := confirmRequest(t, frames[len(frames)-1])
	chatID := chatIDOf(t, frames)

	// Reload the conversation: the pending card comes back.
	rec := env.do(http.MethodPost, "/v1/chat/history", token, map[string]string{"chat_id": chatID})
	env.expectStatus(rec, http.StatusOK)
	var resp historyResponse
	env.decode(rec, &resp)
	history := resp.Messages

	last := history[len(history)-1]
	if last.Role != "confirm" || last.Confirm == nil {
		t.Fatalf("the pending card was not restored on reload: %+v", history)
	}
	card := last.Confirm
	if card.Token == "" || card.Title == "" || len(card.Details) == 0 {
		t.Fatalf("the restored card is missing token, title, or details: %+v", card)
	}
	// It is a fresh token (the original was never stored, only hashed), and it
	// works: approving with it runs the action.
	if card.Token == liveCard.Token {
		t.Fatal("the reload reused the original token instead of minting a fresh one")
	}
	resumed := env.streamTurn(token, map[string]any{"resume_token": card.Token, "resume_action": "approved"})
	if fr := resumed[len(resumed)-1]; fr.Type != chat.FrameResult {
		t.Fatalf("the reloaded card could not be approved: %+v", fr)
	}
	if m, _ := env.app.Store.AIModels().GetByID(context.Background(), env.ws.ID, modelID); m.Status != model.StatusDisabled {
		t.Fatal("approving the reloaded card did not run the action")
	}
}

// Reject is a HARD STOP. The action does not run, and the model is NOT called
// again: the model is handed control back to RESPOND (acknowledge, take another
// path, or ask), but it cannot raise another card. Here the model tries to
// re-call the very tool it was refused; that is silently refused (no new card),
// and it then speaks. So the person is not left in silence, and there is no
// re-ask loop.
func TestRejectionHandsControlBackWithoutRecarding(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	// After the rejection the model stubbornly re-calls the same tool, then, when
	// that is refused without a card, it responds.
	vendor := newFakeVendor(t,
		statusCall(modelID, "disabled", "call_a"),                              // turn 1: parks
		statusCall(modelID, "disabled", "call_b"),                              // resume: tries AGAIN -> refused, no card
		answerChunks("Okay, I won't change it. Want me to do something else?"), // then responds
	)
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{"prompt": "disable that model", "model_id": modelID})
	card := confirmRequest(t, frames[len(frames)-1])

	resumed := env.streamTurn(token, map[string]any{"resume_token": card.Token, "resume_action": "rejected"})

	// The card is resolved, and the model is handed control back and RESPONDS
	// (never silence).
	if len(framesOfType(resumed, chat.FrameConfirmResolved)) != 1 {
		t.Fatalf("the decision was not reported: %+v", resumed)
	}
	if got := deltaText(resumed); got != "Okay, I won't change it. Want me to do something else?" {
		t.Fatalf("the model did not respond after the rejection: %q", got)
	}
	// But it cannot raise ANOTHER card: the re-call was refused, not carded.
	if len(framesOfType(resumed, chat.FrameConfirmRequest)) != 0 {
		t.Fatalf("a rejected turn asked for approval again: %+v", resumed)
	}
	// And the world is unchanged.
	if m, _ := env.app.Store.AIModels().GetByID(context.Background(), env.ws.ID, modelID); m.Status != model.StatusActive {
		t.Fatal("a rejected action ran anyway")
	}
}

// One approval, one action. A token cannot be replayed: two clicks on approve
// must not disable the model twice, and a stolen token cannot be reused after
// the fact.
func TestApprovalCannotBeReplayed(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	vendor := parkingVendor(t, modelID, "Done.")
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{
		"prompt": "disable that model", "model_id": modelID,
	})
	card := confirmRequest(t, frames[len(frames)-1])

	env.streamTurn(token, map[string]any{
		"resume_token": card.Token, "resume_action": "approved",
	})

	// The second attempt must be refused outright, before any tool runs.
	rec := env.do(http.MethodPost, "/v1/chat/stream", token, map[string]any{
		"resume_token": card.Token, "resume_action": "approved",
	})
	env.expectStatus(rec, http.StatusGone)
}

// A confirmation belongs to the person it was shown to. Another user holding
// the token is not an approver.
func TestAnotherUserCannotAnswerYourConfirmation(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("owner@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	ownerToken, _ := env.login("owner@acme.test", "dev-Passw0rd!")
	env.createUser("other@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	otherToken, _ := env.login("other@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	vendor := parkingVendor(t, modelID, "Done.")
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(ownerToken, map[string]any{
		"prompt": "disable that model", "model_id": modelID,
	})
	card := confirmRequest(t, frames[len(frames)-1])

	rec := env.do(http.MethodPost, "/v1/chat/stream", otherToken, map[string]any{
		"resume_token": card.Token, "resume_action": "approved",
	})
	env.expectStatus(rec, http.StatusGone)

	ctx := context.Background()
	target, err := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, modelID)
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	if target.Status != model.StatusActive {
		t.Fatal("another user's approval performed the action")
	}
}

func TestUnknownConfirmationTokenIsRefused(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	env.expectStatus(env.do(http.MethodPost, "/v1/chat/stream", token, map[string]any{
		"resume_token": "sag_cf_not-a-real-token", "resume_action": "approved",
	}), http.StatusGone)

	env.expectStatus(env.do(http.MethodPost, "/v1/chat/stream", token, map[string]any{
		"resume_token": "sag_cf_x", "resume_action": "maybe",
	}), http.StatusBadRequest)
}

// A tool call is only as powerful as the person who triggered it. A user
// without the permission gets a refusal the model can explain, not an action.
func TestToolRefusesAUserWithoutThePermission(t *testing.T) {
	env := newTestEnv(t)
	// This user may chat, but may not edit models.
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermModelsView)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	vendor := parkingVendor(t, modelID, "I was not allowed to do that.")
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{
		"prompt": "disable that model", "model_id": modelID,
	})
	card := confirmRequest(t, frames[len(frames)-1])

	resumed := env.streamTurn(token, map[string]any{
		"resume_token": card.Token, "resume_action": "approved",
	})

	// The turn continues and the model explains, but nothing changed.
	if deltaText(resumed) == "" {
		t.Fatal("the model was not given a chance to explain the refusal")
	}
	ctx := context.Background()
	target, err := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, modelID)
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	if target.Status != model.StatusActive {
		t.Fatal("a user without models:edit disabled a model through the agent")
	}
}

// An approval is not spent by a turn that never started.
//
// The resume CLAIMS the park (single-use, so two clicks cannot run the action
// twice) and only then asks the run manager to start the turn. If that is
// refused, because a message the person sent while the card was on screen is
// still being answered, nothing ran but the approval was gone: the card
// disappeared on the next reload, the tool call behind it read "approval
// required" for good, and the client had already painted the card "Approved".
// Found by adversarial review (KB/29).
func TestABusyConversationDoesNotSwallowTheApproval(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("busy@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("busy@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t, answerChunks("done"))
	modelID := env.registerModel(vendor)
	env.GatewayAgent()
	session := env.seededChat(user.ID)

	park := &model.ParkSnapshot{
		TokenHash:   agent.HashToken("tok_busy"),
		WorkspaceID: env.ws.ID,
		SessionID:   session.ID,
		UserID:      user.ID,
		ModelID:     modelID,
		ToolName:    "http_request",
		ToolCallID:  "call_busy",
		ToolArgs:    json.RawMessage(`{"url":"https://example.test"}`),
		ActionHash:  "hash_busy",
		ExpiresAt:   time.Now().UTC().Add(time.Hour),
	}
	if err := env.app.Store.Agent().CreatePark(context.Background(), park); err != nil {
		t.Fatalf("park: %v", err)
	}

	// Hold the conversation's one lane, exactly as an unfinished turn would.
	held := make(chan struct{})
	blocker := &gatedRunner{gate: held}
	env.app.Runs = run.NewManager(env.app.Store, blocker, nil, zerolog.Nop())
	profile, loadout, err := env.app.Resolve(context.Background(), app.ProfileRequest{
		WorkspaceID: env.ws.ID, UserID: user.ID, Channel: model.ChannelChat, PreferredModelID: modelID,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := env.app.Runs.Start(context.Background(), agent.Turn{
		WorkspaceID: env.ws.ID, UserID: user.ID, SessionID: session.ID, ModelID: profile.ModelID,
		SystemPrompt: profile.SystemPrompt, Tools: loadout, MaxIterations: profile.MaxIterations,
	}); err != nil {
		t.Fatalf("hold the lane: %v", err)
	}
	defer close(held)

	rec := env.do(http.MethodPost, "/v1/chat/stream", token, map[string]any{
		"chat_id":       session.UID,
		"resume_token":  "tok_busy",
		"resume_action": "approved",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected the busy conflict, got %d: %s", rec.Code, rec.Body.String())
	}

	// THE POINT: the card still works. The person's decision was not consumed by
	// a turn that never ran, so clicking Approve again does what they asked.
	back, err := env.app.Store.Agent().PendingPark(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("the approval was spent by a turn that never started: %v", err)
	}
	if back.ID != park.ID {
		t.Fatalf("a different park came back: %d vs %d", back.ID, park.ID)
	}
	if _, err := env.app.Store.Agent().ClaimPark(
		context.Background(), agent.HashToken("tok_busy"), "approved"); err != nil {
		t.Fatalf("the token the person is holding no longer works: %v", err)
	}
}

// gatedRunner holds the lane until it is released.
type gatedRunner struct{ gate chan struct{} }

func (g *gatedRunner) Run(_ context.Context, _ agent.Turn, _ *chat.Stream) error {
	<-g.gate
	return nil
}
