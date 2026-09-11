package storetest

import (
	"encoding/json"
	"errors"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// A background delegation is a durable record with a one-way lifecycle: it is
// created running, its progress updates in place, and it is completed exactly
// once. A finished record cannot be re-completed or have its progress moved.
func testDelegations(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	u := mustUser(t, st, ws.ID, "u@acme.test")
	chat := mustChat(t, st, ws.ID, u.ID, "Background work")

	d := &model.AgentDelegation{
		SessionID: chat.ID, WorkspaceID: ws.ID, ParentToolCallID: "call_1",
		AgentKey: "research", Mode: model.HandoffBackground,
		Progress: json.RawMessage(`{"step":"starting"}`),
	}
	if err := st.Agent().CreateDelegation(ctx(), d); err != nil {
		t.Fatalf("create delegation: %v", err)
	}
	if d.ID == 0 {
		t.Fatal("delegation id was not stamped")
	}

	// It shows as running for its session, with its progress.
	running, err := st.Agent().RunningDelegations(ctx(), chat.ID)
	if err != nil || len(running) != 1 || running[0].ID != d.ID {
		t.Fatalf("running delegations: %v %+v", err, running)
	}
	if running[0].Status != model.DelegationRunning || string(running[0].Progress) != `{"step":"starting"}` {
		t.Fatalf("delegation not stored running with progress: %+v", running[0])
	}

	// Progress updates in place while running.
	if err := st.Agent().UpdateDelegationProgress(ctx(), d.ID, json.RawMessage(`{"step":"halfway"}`)); err != nil {
		t.Fatalf("update progress: %v", err)
	}
	if got, _ := st.Agent().GetDelegation(ctx(), d.ID); string(got.Progress) != `{"step":"halfway"}` {
		t.Fatalf("progress not updated: %q", got.Progress)
	}

	// Completing it moves it to done with a result and a completed_at.
	if err := st.Agent().CompleteDelegation(ctx(), d.ID, model.DelegationDone, json.RawMessage(`{"answer":42}`), ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, _ := st.Agent().GetDelegation(ctx(), d.ID)
	if got.Status != model.DelegationDone || string(got.Result) != `{"answer":42}` || got.CompletedAt == nil {
		t.Fatalf("delegation not completed: %+v", got)
	}
	// It no longer shows as running.
	if running, _ := st.Agent().RunningDelegations(ctx(), chat.ID); len(running) != 0 {
		t.Fatalf("a completed delegation still shows running: %+v", running)
	}

	// The lifecycle is one-way: a second completion and a late progress tick are
	// both refused, and neither disturbs the finished record.
	if err := st.Agent().CompleteDelegation(ctx(), d.ID, model.DelegationFailed, nil, "too late"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second completion should be refused, got: %v", err)
	}
	if err := st.Agent().UpdateDelegationProgress(ctx(), d.ID, json.RawMessage(`{"step":"late"}`)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("progress on a finished delegation should be refused, got: %v", err)
	}
	if got, _ := st.Agent().GetDelegation(ctx(), d.ID); string(got.Progress) != `{"step":"halfway"}` {
		t.Fatalf("a finished delegation's progress was overwritten: %q", got.Progress)
	}

	// An unknown id is not found.
	if _, err := st.Agent().GetDelegation(ctx(), 999999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown delegation should be not found, got: %v", err)
	}

	// A parked delegation carries its waiting state in progress; Waiting reads it.
	waiting := &model.AgentDelegation{
		SessionID: chat.ID, WorkspaceID: ws.ID, ParentToolCallID: "call_2",
		AgentKey: "ops", Mode: model.HandoffBackground,
		Progress: json.RawMessage(`{"waiting":true,"activity":"Waiting for your approval"}`),
	}
	if err := st.Agent().CreateDelegation(ctx(), waiting); err != nil {
		t.Fatalf("create waiting delegation: %v", err)
	}
	if !waiting.Waiting() {
		t.Fatal("a delegation with waiting:true in progress should report Waiting")
	}
	plain := &model.AgentDelegation{
		SessionID: chat.ID, WorkspaceID: ws.ID, ParentToolCallID: "call_3",
		AgentKey: "ops", Mode: model.HandoffBackground,
	}
	if err := st.Agent().CreateDelegation(ctx(), plain); err != nil {
		t.Fatalf("create plain delegation: %v", err)
	}
	if plain.Waiting() {
		t.Fatal("a delegation with no progress should not report Waiting")
	}

	// Boot recovery reads every still-running delegation across all sessions, so
	// the app layer can settle each and narrate that it did not finish (KB/27). The
	// two running rows are returned oldest first; the completed one above is not.
	all, err := st.Agent().AllRunningDelegations(ctx())
	if err != nil {
		t.Fatalf("all running delegations: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 running delegations, got %d", len(all))
	}
	if all[0].ID != waiting.ID || all[1].ID != plain.ID {
		t.Fatalf("running delegations not oldest-first: %d then %d", all[0].ID, all[1].ID)
	}
	for _, d := range all {
		if d.Status != model.DelegationRunning {
			t.Fatalf("a listed delegation was not running: %+v", d)
		}
	}

	// SessionDelegations returns EVERY delegation of a session, whatever its
	// status, in id order: unlike RunningDelegations it includes the finished one.
	// This is what the Gateway's on-request status tool reads to picture the set.
	sess, err := st.Agent().SessionDelegations(ctx(), chat.ID)
	if err != nil {
		t.Fatalf("session delegations: %v", err)
	}
	if len(sess) != 3 {
		t.Fatalf("expected 3 session delegations (1 done, 2 running), got %d", len(sess))
	}
	if sess[0].ID != d.ID || sess[1].ID != waiting.ID || sess[2].ID != plain.ID {
		t.Fatalf("session delegations not id-ordered: %d, %d, %d", sess[0].ID, sess[1].ID, sess[2].ID)
	}
	if sess[0].Status != model.DelegationDone {
		t.Fatalf("the finished delegation should still be listed by SessionDelegations: %+v", sess[0])
	}
	// A session with nothing delegated gets an empty slice, not an error.
	if empty, err := st.Agent().SessionDelegations(ctx(), 999999); err != nil || len(empty) != 0 {
		t.Fatalf("SessionDelegations for an unknown session: %v %+v", err, empty)
	}
}
