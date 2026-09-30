package storetest

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// A conversation shows one background approval card at a time (KB/27): the first
// park is live, the rest wait as queued, and each is released to live when the
// one before it is answered. "Approve all" drains the queue without a card.
func testParkQueue(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "queue")
	u := mustUser(t, st, ws.ID, "q@acme.test")
	chat := mustChat(t, st, ws.ID, u.ID, "Approvals")

	park := func(tokenHash, callID string) *model.ParkSnapshot {
		return &model.ParkSnapshot{
			TokenHash: tokenHash, WorkspaceID: ws.ID, SessionID: chat.ID, UserID: u.ID,
			ModelID: 1, ToolName: "current_time", ToolCallID: callID,
			ToolArgs: json.RawMessage(`{}`), ActionHash: "a",
			AgentKey: "worker", ParentToolCallID: "call_" + callID, ExpiresAt: future(),
		}
	}

	// The first park is live; the next two wait their turn.
	first := park("h1", "c1")
	live, err := st.Agent().CreateParkSequenced(ctx(), first)
	if err != nil || !live {
		t.Fatalf("first park should be live: live=%v err=%v", live, err)
	}
	if first.Status != model.ParkPending {
		t.Fatalf("first park is not pending: %q", first.Status)
	}
	second := park("h2", "c2")
	if live, err := st.Agent().CreateParkSequenced(ctx(), second); err != nil || live {
		t.Fatalf("second park should be queued (not live): live=%v err=%v", live, err)
	}
	third := park("h3", "c3")
	if _, err := st.Agent().CreateParkSequenced(ctx(), third); err != nil {
		t.Fatalf("third park: %v", err)
	}

	// Only the live one is what a reload would show; the other two are queued.
	if p, err := st.Agent().PendingPark(ctx(), chat.ID); err != nil || p.ID != first.ID {
		t.Fatalf("pending park is not the first live one: %+v %v", p, err)
	}
	queued, err := st.Agent().QueuedParks(ctx(), chat.ID)
	if err != nil || len(queued) != 2 || queued[0].ID != second.ID || queued[1].ID != third.ID {
		t.Fatalf("queued parks not [second, third] oldest-first: %+v %v", queued, err)
	}

	// Answering the live one releases the next, in order.
	if _, err := st.Agent().ClaimPark(ctx(), "h1", model.ParkApproved); err != nil {
		t.Fatalf("claim first: %v", err)
	}
	released, err := st.Agent().ReleaseNextQueuedPark(ctx(), chat.ID)
	if err != nil || released.ID != second.ID || released.Status != model.ParkPending {
		t.Fatalf("release did not promote the second park to live: %+v %v", released, err)
	}
	if p, _ := st.Agent().PendingPark(ctx(), chat.ID); p == nil || p.ID != second.ID {
		t.Fatalf("second park is not live after release")
	}

	// "Approve all" drains whatever is still queued (the third), no card.
	if err := st.Agent().ResolveParkByID(ctx(), third.ID, model.ParkApproved); err != nil {
		t.Fatalf("resolve third by id: %v", err)
	}
	if left, _ := st.Agent().QueuedParks(ctx(), chat.ID); len(left) != 0 {
		t.Fatalf("queue not empty after draining: %+v", left)
	}
	if _, err := st.Agent().ReleaseNextQueuedPark(ctx(), chat.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("releasing an empty queue should be not found, got: %v", err)
	}
}

// A card that cannot be shown loses its place instead of holding it.
//
// This is the failure that reached a real installation: a card nobody could
// draw sat live in a conversation, and because a conversation shows one card at
// a time, every later card queued behind something invisible. Five
// conversations were found shut that way, and from the outside it looked like
// approval had stopped working entirely.
//
// Passed over, never destroyed: what makes a card undrawable is usually
// temporary (a tool revoked while it waited, a computer not linked just now),
// so it goes to the back and can be drawn again later.
func testParkPassOver(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "passover")
	u := mustUser(t, st, ws.ID, "p@acme.test")
	chat := mustChat(t, st, ws.ID, u.ID, "Cards")

	park := func(hash string) *model.ParkSnapshot {
		return &model.ParkSnapshot{
			TokenHash: hash, TokenSeed: "seed-" + hash, WorkspaceID: ws.ID, SessionID: chat.ID,
			UserID: u.ID, ModelID: 1, ToolName: "terminal", ToolCallID: "call_" + hash,
			ToolArgs: json.RawMessage(`{}`), ActionHash: "a" + hash,
			ExpiresAt: time.Now().UTC().Add(time.Hour),
		}
	}
	first, second, third := park("one"), park("two"), park("three")
	for _, p := range []*model.ParkSnapshot{first, second, third} {
		if _, err := st.Agent().CreateParkSequenced(ctx(), p); err != nil {
			t.Fatalf("create park: %v", err)
		}
	}
	if first.Status != model.ParkPending {
		t.Fatalf("the first card should be the live one, got %q", first.Status)
	}

	// All three are waiting, which is what bounds the walk for a drawable one.
	if n, err := st.Agent().CountWaitingParks(ctx(), chat.ID); err != nil || n != 3 {
		t.Fatalf("waiting cards: %d (%v), want 3", n, err)
	}

	// The live one cannot be drawn: it goes to the back and the next is live.
	promoted, err := st.Agent().PassOverPark(ctx(), first.ID)
	if err != nil {
		t.Fatalf("pass over: %v", err)
	}
	if !promoted {
		t.Fatal("nothing was made live in its place, so the conversation stays shut")
	}
	live, err := st.Agent().PendingPark(ctx(), chat.ID)
	if err != nil {
		t.Fatalf("pending park: %v", err)
	}
	if live.ID != second.ID {
		t.Fatalf("the live card is %d, want the second one (%d)", live.ID, second.ID)
	}

	// Nothing was destroyed: the passed-over card is still waiting its turn.
	if n, err := st.Agent().CountWaitingParks(ctx(), chat.ID); err != nil || n != 3 {
		t.Fatalf("a passed-over card was destroyed: %d waiting (%v), want 3", n, err)
	}

	// And it is never promoted straight back, or the walk would draw the same
	// undrawable card for ever.
	if _, err := st.Agent().PassOverPark(ctx(), second.ID); err != nil {
		t.Fatalf("pass over the second: %v", err)
	}
	live, err = st.Agent().PendingPark(ctx(), chat.ID)
	if err != nil {
		t.Fatalf("pending park: %v", err)
	}
	if live.ID == second.ID {
		t.Fatal("the card just passed over was made live again")
	}

	// The last one alone: there is nothing to promote, and it says so rather
	// than claiming it moved.
	only := park("only")
	only.SessionID = mustChat(t, st, ws.ID, u.ID, "One card").ID
	if _, err := st.Agent().CreateParkSequenced(ctx(), only); err != nil {
		t.Fatalf("create lone park: %v", err)
	}
	promoted, err = st.Agent().PassOverPark(ctx(), only.ID)
	if err != nil {
		t.Fatalf("pass over the lone card: %v", err)
	}
	if promoted {
		t.Fatal("a conversation with one card claimed to have promoted another")
	}
}
