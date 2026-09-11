package api

import (
	"context"
	"strings"
	"testing"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
)

// The write tool through the whole loop: the pre-park validator plans the write
// FIRST, so a card is only ever shown for one that will run. These two turns are
// the "approval must equal success" promise, end to end.

// A valid write parks with a card that NAMES it, and approving it lands the
// document.
func TestBrainWriteParksAndLandsOnApproval(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")
	ctx := context.Background()

	notes := &model.Brain{WorkspaceID: env.ws.ID, Name: "Notes"}
	if err := env.app.Store.Brains().CreateBrain(ctx, notes); err != nil {
		t.Fatalf("create brain: %v", err)
	}
	ideas := &model.BrainCategory{BrainID: notes.ID, Name: "Ideas"}
	if err := env.app.Store.Brains().CreateCategory(ctx, env.ws.ID, ideas); err != nil {
		t.Fatalf("create category: %v", err)
	}
	brainWriteGateway(t, env, notes.ID)

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	args := `{\"operation\":\"save_document\",\"brain\":\"Notes\",\"category\":\"Ideas\",\"title\":\"Refunds\",\"content\":\"within 30 days\"}`
	vendor := newFakeVendor(t, brainWriteCall(args, "call_1"), answerChunks("Saved it."))
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{"prompt": "note the refund policy", "model_id": modelID})

	// It parked, with a card whose copy names the actual write (from Validate).
	last := frames[len(frames)-1]
	if last.Type != chat.FrameConfirmRequest || !last.Final {
		t.Fatalf("a valid write did not park: %+v", frames)
	}
	card := confirmRequest(t, last)
	if !strings.Contains(card.Description, "Refunds") || !strings.Contains(card.Description, "Notes") {
		t.Fatalf("the card did not name the write: %+v", card)
	}
	// Nothing is written until the person approves.
	if docs, _ := env.app.Store.Brains().Documents(ctx, env.ws.ID, ideas.ID); len(docs) != 0 {
		t.Fatal("the write ran before it was approved")
	}

	// Approve, and the document lands: approval equalled success.
	resumed := env.streamTurn(token, map[string]any{"resume_token": card.Token, "resume_action": "approved"})
	if fr := resumed[len(resumed)-1]; fr.Type != chat.FrameResult {
		t.Fatalf("the approved write did not complete: %+v", resumed)
	}
	docs, _ := env.app.Store.Brains().Documents(ctx, env.ws.ID, ideas.ID)
	if len(docs) != 1 || docs[0].Title != "Refunds" {
		t.Fatalf("the approved write did not land: %+v", docs)
	}
}

// An INVALID write never reaches a card: the validator refuses it before the
// park decision, the model reads the refusal and responds, and the turn ends
// with no approval ever asked.
func TestBrainWriteInvalidNeverCards(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")
	ctx := context.Background()

	notes := &model.Brain{WorkspaceID: env.ws.ID, Name: "Notes"}
	if err := env.app.Store.Brains().CreateBrain(ctx, notes); err != nil {
		t.Fatalf("create brain: %v", err)
	}
	brainWriteGateway(t, env, notes.ID)

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	// A category that does not exist: refused BEFORE any card.
	args := `{\"operation\":\"save_document\",\"brain\":\"Notes\",\"category\":\"Ghost\",\"title\":\"X\",\"content\":\"y\"}`
	vendor := newFakeVendor(t, brainWriteCall(args, "call_1"), answerChunks("That category does not exist yet."))
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{"prompt": "note it", "model_id": modelID})

	if cards := framesOfType(frames, chat.FrameConfirmRequest); len(cards) != 0 {
		t.Fatalf("an invalid write raised a card instead of being refused: %+v", frames)
	}
	if fr := frames[len(frames)-1]; fr.Type != chat.FrameResult {
		t.Fatalf("the turn did not complete: %+v", frames)
	}
	if deltaText(frames) == "" {
		t.Fatal("the model was not handed the refusal to respond to")
	}
}

// brainWriteGateway is a Gateway that may write, is assigned one brain, and
// confirms every write (so a valid one parks).
func brainWriteGateway(t *testing.T, env *testEnv, brainID int64) {
	t.Helper()
	if err := env.app.Store.Agents().Create(context.Background(), &model.Agent{
		WorkspaceID: env.ws.ID, Key: model.DefaultAgentKey, Name: "Assistant",
		Tools: []string{"brain_write"}, ConfirmTools: []string{"brain_write"},
		Brains: []int64{brainID},
	}); err != nil {
		t.Fatalf("create Gateway agent: %v", err)
	}
}

func brainWriteCall(argsJSON, callID string) []string {
	return []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"` + callID + `","function":{"name":"brain_write","arguments":"` + argsJSON + `"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}
