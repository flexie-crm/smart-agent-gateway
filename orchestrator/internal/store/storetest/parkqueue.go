package storetest

import (
	"encoding/json"
	"errors"
	"testing"

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
