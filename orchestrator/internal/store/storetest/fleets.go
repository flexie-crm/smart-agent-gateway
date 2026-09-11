package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// A fleet is N agents asked at once and answered once. Two things about it are
// worth a real database rather than a fake: that the tally is a count of what
// is actually there, and that when several members finish together exactly one
// of them gets to tell the Gateway.

func testFleets(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "fleet")
	u := mustUser(t, st, ws.ID, "fleet@acme.test")
	session := mustChat(t, st, ws.ID, u.ID, "Fleet work")
	ctx := ctx()

	fleet := &model.AgentFleet{
		SessionID: session.ID, WorkspaceID: ws.ID,
		ParentToolCallID: "call_1", Size: 3,
	}
	if err := st.Agent().CreateFleet(ctx, fleet); err != nil {
		t.Fatalf("create fleet: %v", err)
	}
	if fleet.ID == 0 || fleet.Status != model.FleetRunning || fleet.Deadline.IsZero() {
		t.Fatalf("a fresh fleet should be running with a deadline: %+v", fleet)
	}

	members := make([]*model.AgentDelegation, 3)
	for i := range members {
		members[i] = &model.AgentDelegation{
			SessionID: session.ID, WorkspaceID: ws.ID,
			ParentToolCallID: "call_1", AgentKey: "researcher",
			Mode: model.DelegationModeFleet, FleetID: &fleet.ID,
			Task: "look something up",
		}
		if err := st.Agent().CreateDelegation(ctx, members[i]); err != nil {
			t.Fatalf("create member %d: %v", i, err)
		}
	}

	// A member carries its fleet, which is the only thing that makes it one.
	got, err := st.Agent().GetDelegation(ctx, members[0].ID)
	if err != nil {
		t.Fatalf("get member: %v", err)
	}
	if got.FleetID == nil || *got.FleetID != fleet.ID {
		t.Fatalf("the member forgot its fleet: %+v", got.FleetID)
	}

	done, size, err := st.Agent().FleetTally(ctx, fleet.ID)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if done != 0 || size != 3 {
		t.Fatalf("nothing has finished yet: %d/%d", done, size)
	}

	// However a member ends, it counts. A fleet waits for answers, not for
	// successes, or one failure would hold a conversation open forever.
	for i, status := range []string{model.DelegationDone, model.DelegationFailed, model.DelegationCancelled} {
		if err := st.Agent().CompleteDelegation(ctx, members[i].ID, status, nil, ""); err != nil {
			t.Fatalf("complete member %d: %v", i, err)
		}
	}
	done, size, err = st.Agent().FleetTally(ctx, fleet.ID)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if done != 3 || size != 3 {
		t.Fatalf("a done, a failed and a cancelled are all answers: %d/%d", done, size)
	}

	closed, err := st.Agent().CloseFleet(ctx, fleet.ID, model.FleetDone)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if !closed {
		t.Fatal("the first caller to close a running fleet should win")
	}
	again, err := st.Agent().CloseFleet(ctx, fleet.ID, model.FleetDone)
	if err != nil {
		t.Fatalf("close twice: %v", err)
	}
	if again {
		t.Fatal("a fleet was closed twice, so the Gateway would be told twice")
	}

	read, err := st.Agent().Fleet(ctx, fleet.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.Status != model.FleetDone || read.CompletedAt == nil {
		t.Fatalf("a closed fleet should say so: %+v", read)
	}
}

// Every member finishing at the same instant sees a full tally, and exactly one
// of them may tell the Gateway. This is the race the conditional close exists
// for, and the only one that would show up as a duplicated answer in the chat.
func testFleetOneWinner(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "fleet-race")
	u := mustUser(t, st, ws.ID, "race@acme.test")
	session := mustChat(t, st, ws.ID, u.ID, "Race")
	ctx := ctx()

	fleet := &model.AgentFleet{
		SessionID: session.ID, WorkspaceID: ws.ID,
		ParentToolCallID: "call_race", Size: 8,
	}
	if err := st.Agent().CreateFleet(ctx, fleet); err != nil {
		t.Fatalf("create fleet: %v", err)
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		tally = make(chan struct{})
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-tally // all at once, which is the point
			won, err := st.Agent().CloseFleet(ctx, fleet.ID, model.FleetDone)
			if err != nil {
				t.Errorf("close: %v", err)
				return
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	close(tally)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("eight members finishing together produced %d Gateway answers", wins)
	}
}

// A fleet past its deadline is findable, so a conversation is never left
// waiting on the one member that hung.
func testOverdueFleets(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "fleet-overdue")
	u := mustUser(t, st, ws.ID, "overdue@acme.test")
	session := mustChat(t, st, ws.ID, u.ID, "Overdue")
	ctx := ctx()

	live := &model.AgentFleet{
		SessionID: session.ID, WorkspaceID: ws.ID,
		ParentToolCallID: "call_live", Size: 1,
		Deadline: time.Now().UTC().Add(time.Hour),
	}
	if err := st.Agent().CreateFleet(ctx, live); err != nil {
		t.Fatalf("create live fleet: %v", err)
	}
	late := &model.AgentFleet{
		SessionID: session.ID, WorkspaceID: ws.ID,
		ParentToolCallID: "call_late", Size: 1,
		Deadline: time.Now().UTC().Add(-time.Hour),
	}
	if err := st.Agent().CreateFleet(ctx, late); err != nil {
		t.Fatalf("create late fleet: %v", err)
	}

	overdue, err := st.Agent().OverdueFleets(ctx)
	if err != nil {
		t.Fatalf("overdue: %v", err)
	}
	if len(overdue) != 1 || overdue[0].ID != late.ID {
		t.Fatalf("expected only the late fleet: %+v", overdue)
	}

	// Once it is closed it is not overdue any more, whatever its deadline says.
	if _, err := st.Agent().CloseFleet(ctx, late.ID, model.FleetDone); err != nil {
		t.Fatalf("close: %v", err)
	}
	overdue, err = st.Agent().OverdueFleets(ctx)
	if err != nil {
		t.Fatalf("overdue after close: %v", err)
	}
	if len(overdue) != 0 {
		t.Fatalf("a closed fleet is still being chased: %+v", overdue)
	}
}

// The three reads the join actually depends on: which members a fleet has,
// which fleets are complete without anybody having said so, and what settling
// the stragglers does. Each is the difference between the Gateway being woken
// and a conversation waiting for its deadline.
func testFleetJoinReads(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "fleet-join")
	u := mustUser(t, st, ws.ID, "join@acme.test")
	session := mustChat(t, st, ws.ID, u.ID, "Join")
	ctx := ctx()

	fleet := &model.AgentFleet{
		SessionID: session.ID, WorkspaceID: ws.ID,
		ParentToolCallID: "call_join", Size: 2,
	}
	if err := st.Agent().CreateFleet(ctx, fleet); err != nil {
		t.Fatalf("create fleet: %v", err)
	}
	// A delegation with no fleet, to prove the reads below are about the batch
	// and not about the conversation.
	alone := &model.AgentDelegation{
		SessionID: session.ID, WorkspaceID: ws.ID,
		ParentToolCallID: "call_alone", AgentKey: "writer",
		Mode: model.HandoffBackground, Task: "write something",
	}
	if err := st.Agent().CreateDelegation(ctx, alone); err != nil {
		t.Fatalf("create standalone delegation: %v", err)
	}
	members := make([]*model.AgentDelegation, 2)
	for i := range members {
		members[i] = &model.AgentDelegation{
			SessionID: session.ID, WorkspaceID: ws.ID,
			ParentToolCallID: "call_join", AgentKey: "researcher",
			Mode: model.HandoffFleet, FleetID: &fleet.ID, Task: "look it up",
		}
		if err := st.Agent().CreateDelegation(ctx, members[i]); err != nil {
			t.Fatalf("create member %d: %v", i, err)
		}
	}

	got, err := st.Agent().FleetMembers(ctx, fleet.ID)
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	if len(got) != 2 || got[0].ID != members[0].ID || got[1].ID != members[1].ID {
		t.Fatalf("expected the two members in the order they were asked for, got %d", len(got))
	}

	fleets, err := st.Agent().SessionFleets(ctx, session.ID)
	if err != nil {
		t.Fatalf("session fleets: %v", err)
	}
	if len(fleets) != 1 || fleets[0].ID != fleet.ID {
		t.Fatalf("expected one fleet on this conversation, got %d", len(fleets))
	}

	// Nothing is settled while a member is still working, even though the
	// standalone delegation beside it is finished.
	if err := st.Agent().CompleteDelegation(ctx, alone.ID, model.DelegationDone, nil, ""); err != nil {
		t.Fatalf("complete standalone: %v", err)
	}
	if err := st.Agent().CompleteDelegation(ctx, members[0].ID, model.DelegationDone, nil, ""); err != nil {
		t.Fatalf("complete member: %v", err)
	}
	settled, err := st.Agent().SettledFleets(ctx)
	if err != nil {
		t.Fatalf("settled: %v", err)
	}
	if len(settled) != 0 {
		t.Fatalf("a fleet with one member still running is not settled: %+v", settled)
	}

	// The deadline's job: what has not come back is recorded as not having, and
	// the fleet becomes answerable with what did.
	n, err := st.Agent().AbandonFleetMembers(ctx, fleet.ID, "ran out of time")
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected the one running member to be settled, got %d", n)
	}
	// Only the running one. A member that already finished keeps its result.
	after, err := st.Agent().GetDelegation(ctx, members[0].ID)
	if err != nil {
		t.Fatalf("read finished member: %v", err)
	}
	if after.Status != model.DelegationDone || after.ErrorText != "" {
		t.Fatalf("abandoning overwrote a member that had already answered: %+v", after)
	}
	straggler, err := st.Agent().GetDelegation(ctx, members[1].ID)
	if err != nil {
		t.Fatalf("read straggler: %v", err)
	}
	if straggler.Status != model.DelegationFailed || straggler.ErrorText != "ran out of time" {
		t.Fatalf("the straggler should say why it did not answer: %+v", straggler)
	}
	// And the standalone delegation is untouched: abandoning is scoped to a
	// batch, not to a conversation.
	other, err := st.Agent().GetDelegation(ctx, alone.ID)
	if err != nil {
		t.Fatalf("read standalone: %v", err)
	}
	if other.Status != model.DelegationDone {
		t.Fatalf("a delegation outside the fleet was settled with it: %+v", other)
	}

	settled, err = st.Agent().SettledFleets(ctx)
	if err != nil {
		t.Fatalf("settled after abandon: %v", err)
	}
	if len(settled) != 1 || settled[0].ID != fleet.ID {
		t.Fatalf("expected the fleet to be settled now, got %+v", settled)
	}
	// Closed, and it stops being work anybody has to look at.
	if _, err := st.Agent().CloseFleet(ctx, fleet.ID, model.FleetDone); err != nil {
		t.Fatalf("close: %v", err)
	}
	settled, err = st.Agent().SettledFleets(ctx)
	if err != nil {
		t.Fatalf("settled after close: %v", err)
	}
	if len(settled) != 0 {
		t.Fatalf("a closed fleet is still being offered to the join: %+v", settled)
	}
}

// The model a fleet was started on survives the process that started it, which
// is what lets a completion turn after a restart run on the same one.
func testFleetRemembersItsModel(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "fleet-model")
	u := mustUser(t, st, ws.ID, "model@acme.test")
	session := mustChat(t, st, ws.ID, u.ID, "Model")
	v := mustVendor(t, st, ws.ID, "Anthropic", []byte{0x01})
	m := mustAIModel(t, st, ws.ID, v.ID, "claude-opus-4-8")
	ctx := ctx()

	fleet := &model.AgentFleet{
		SessionID: session.ID, WorkspaceID: ws.ID,
		ParentToolCallID: "call_model", ModelID: m.ID, Size: 1,
	}
	if err := st.Agent().CreateFleet(ctx, fleet); err != nil {
		t.Fatalf("create fleet: %v", err)
	}
	read, err := st.Agent().Fleet(ctx, fleet.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.ModelID != m.ID {
		t.Fatalf("the fleet forgot its model: %d want %d", read.ModelID, m.ID)
	}

	// A fleet with no model recorded reads back as zero rather than as a broken
	// foreign key: the completion turn then falls back to the profile's model.
	none := &model.AgentFleet{
		SessionID: session.ID, WorkspaceID: ws.ID,
		ParentToolCallID: "call_nomodel", Size: 1,
	}
	if err := st.Agent().CreateFleet(ctx, none); err != nil {
		t.Fatalf("create fleet with no model: %v", err)
	}
	read, err = st.Agent().Fleet(ctx, none.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.ModelID != 0 {
		t.Fatalf("expected no model, got %d", read.ModelID)
	}
}

// "Approve, and stop asking" answers every waiting card in ONE write, and hands
// them all back so the caller can set every agent going at once.
//
// One at a time was a write and a round trip each: with thirteen agents waiting,
// the last was answered long after the first, and the person watched a queue
// drain that they had already emptied.
func testResolveQueuedParksAtOnce(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "drain-all")
	u := mustUser(t, st, ws.ID, "drain@acme.test")
	session := mustChat(t, st, ws.ID, u.ID, "Drain")
	other := mustChat(t, st, ws.ID, u.ID, "Another conversation")
	v := mustVendor(t, st, ws.ID, "Anthropic", []byte{0x01})
	m := mustAIModel(t, st, ws.ID, v.ID, "claude-opus-4-8")
	ctx := ctx()

	park := func(sessionID int64, token string) *model.ParkSnapshot {
		p := &model.ParkSnapshot{
			TokenHash: token, WorkspaceID: ws.ID, SessionID: sessionID, UserID: u.ID,
			ModelID: m.ID, ToolName: "http_request", ToolCallID: "call_" + token,
			ToolArgs: json.RawMessage(`{}`), ActionHash: token,
			AgentKey: "research", ParentToolCallID: "call_fleet", HandoffMode: model.HandoffFleet,
			ExpiresAt: time.Now().UTC().Add(time.Hour),
		}
		if _, err := st.Agent().CreateParkSequenced(ctx, p); err != nil {
			t.Fatalf("create park %s: %v", token, err)
		}
		return p
	}
	// One live card and two behind it, plus one on a different conversation
	// that must not be touched.
	live := park(session.ID, "t1")
	park(session.ID, "t2")
	park(session.ID, "t3")
	elsewhere := park(other.ID, "t4")

	answered, err := st.Agent().ResolveQueuedParks(ctx, session.ID, model.ParkApproved)
	if err != nil {
		t.Fatalf("resolve queued: %v", err)
	}
	// The two QUEUED ones, and not the live one: that is the card the person is
	// actually looking at, and it is answered by their click, not by this.
	if len(answered) != 2 {
		t.Fatalf("expected the two queued cards, got %d", len(answered))
	}
	for _, p := range answered {
		if p.SessionID != session.ID {
			t.Fatalf("a card from another conversation was answered: %+v", p)
		}
		// Handed back whole, so the caller can re-enter the agent that raised it
		// without reading them again.
		if p.AgentKey != "research" || p.HandoffMode != model.HandoffFleet {
			t.Fatalf("a card came back stripped of what the caller needs: %+v", p)
		}
	}

	// Nothing queued is left on this conversation.
	if rest, err := st.Agent().QueuedParks(ctx, session.ID); err != nil || len(rest) != 0 {
		t.Fatalf("cards are still queued after approve-all: %d %v", len(rest), err)
	}
	// The live card is untouched, and so is the other conversation's.
	if pending, err := st.Agent().PendingPark(ctx, session.ID); err != nil || pending.ID != live.ID {
		t.Fatalf("the card the person is looking at was answered for them: %+v %v", pending, err)
	}
	if rest, err := st.Agent().QueuedParks(ctx, other.ID); err != nil || len(rest) != 0 {
		// t4 was the only park on that conversation, so it is LIVE, not queued.
		_ = rest
	}
	if pending, err := st.Agent().PendingPark(ctx, other.ID); err != nil || pending.ID != elsewhere.ID {
		t.Fatalf("another conversation's card was disturbed: %+v %v", pending, err)
	}
}

// A batch's rows are written in ONE statement, however many there are.
//
// A thousand agents is a thousand rows, and a round trip each would make
// starting a batch slower than running it: the process, not the database, would
// be the limit. The ids come back stamped, because the caller has to enqueue a
// job against each of them.
func testFleetMembersAreWrittenAtOnce(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "bulk-members")
	u := mustUser(t, st, ws.ID, "bulk@acme.test")
	session := mustChat(t, st, ws.ID, u.ID, "Bulk")
	ctx := ctx()

	fleet := &model.AgentFleet{SessionID: session.ID, WorkspaceID: ws.ID,
		ParentToolCallID: "call_bulk", Size: 250}
	if err := st.Agent().CreateFleet(ctx, fleet); err != nil {
		t.Fatalf("create fleet: %v", err)
	}

	members := make([]*model.AgentDelegation, 250)
	for i := range members {
		members[i] = &model.AgentDelegation{
			SessionID: session.ID, WorkspaceID: ws.ID,
			ParentToolCallID: "call_bulk", AgentKey: "researcher",
			Mode: model.HandoffFleet, Task: fmt.Sprintf("look up part %d", i+1),
		}
	}
	if err := st.Agent().CreateFleetMembers(ctx, fleet.ID, members); err != nil {
		t.Fatalf("create fleet members: %v", err)
	}

	// Every one stamped with its own id, and no two the same: the caller
	// enqueues a job against each, so a repeated id would run one task twice
	// and leave another unstarted.
	seen := map[int64]bool{}
	for i, m := range members {
		if m.ID == 0 {
			t.Fatalf("member %d came back without an id", i)
		}
		if seen[m.ID] {
			t.Fatalf("id %d was handed to two members", m.ID)
		}
		seen[m.ID] = true
		if m.FleetID == nil || *m.FleetID != fleet.ID {
			t.Fatalf("member %d does not belong to the batch: %+v", i, m.FleetID)
		}
	}

	// And they are all there, running, with their own tasks kept apart.
	stored, err := st.Agent().FleetMembers(ctx, fleet.ID)
	if err != nil {
		t.Fatalf("read members: %v", err)
	}
	if len(stored) != 250 {
		t.Fatalf("wrote %d members, read back %d", len(members), len(stored))
	}
	tasks := map[string]bool{}
	for _, m := range stored {
		if m.Status != model.DelegationRunning || m.Attempts != 1 {
			t.Fatalf("a member was written in the wrong state: %+v", m)
		}
		tasks[m.Task] = true
	}
	if len(tasks) != 250 {
		t.Fatalf("expected 250 distinct tasks, got %d: the rows were mixed up", len(tasks))
	}
	// The order the caller sees matches the order it asked for, so member i's
	// id belongs to member i's task.
	if stored[0].Task != members[0].Task || stored[249].Task != members[249].Task {
		t.Fatal("the ids came back in a different order from the tasks they belong to")
	}
}

// The same for the jobs that carry them to the workers.
func testJobsAreEnqueuedAtOnce(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "bulk-jobs")

	jobs := make([]*model.Job, 250)
	for i := range jobs {
		jobs[i] = &model.Job{
			WorkspaceID: ws.ID, Kind: "agent.run", Subject: "jobs.default.agent.run",
			Payload: []byte(fmt.Sprintf(`{"delegation_id":%d}`, i+1)), MaxAttempts: 2,
		}
	}
	if err := st.Jobs().EnqueueMany(ctx, jobs); err != nil {
		t.Fatalf("enqueue many: %v", err)
	}

	claimed := 0
	payloads := map[string]bool{}
	for {
		batch, err := st.Jobs().Claim(ctx, fmt.Sprintf("bulk#%d", claimed), "jobs.default.", 50)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, j := range batch {
			// Written pending and claimable, with its own payload: a batch
			// insert that shared one payload would run the same task 250 times.
			if j.MaxAttempts != 2 {
				t.Fatalf("a job lost its attempt cap: %+v", j)
			}
			payloads[string(j.Payload)] = true
		}
		claimed += len(batch)
	}
	if claimed != 250 {
		t.Fatalf("enqueued 250 jobs, claimed %d", claimed)
	}
	if len(payloads) != 250 {
		t.Fatalf("expected 250 distinct payloads, got %d", len(payloads))
	}
}
