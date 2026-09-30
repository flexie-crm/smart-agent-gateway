package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/ws"
)

// Opening an agent from its chip: what it has done so far through the API, and
// every step after that on the socket, to the person whose chat it is and only
// while they have it open. Real server, real database, real socket, real loop;
// only the models are stand-ins.

// topicFrames is everything one socket heard on the topics it watches.
type topicFrames struct {
	mu    sync.Mutex
	heard []ws.Envelope
}

// watch opens the chat's socket as the person, subscribes it to topics, and
// keeps every topic frame it is sent. With no topics it is a tab that is open
// and watching nothing.
func (e *testEnv) watch(token string, topics ...string) *topicFrames {
	e.t.Helper()
	server := httptest.NewServer(e.router)
	e.t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/ws", nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		e.t.Fatalf("dial the socket: %v", err)
	}
	e.t.Cleanup(func() { _ = conn.CloseNow() })
	if err := wsjson.Write(ctx, conn, ws.Envelope{Type: ws.TypeAuthenticate, Token: token, Source: ws.SourceChat}); err != nil {
		e.t.Fatalf("authenticate: %v", err)
	}
	var ack ws.Envelope
	if err := wsjson.Read(ctx, conn, &ack); err != nil || ack.Type != ws.TypeAuthenticated {
		e.t.Fatalf("the socket did not authenticate: %v %q", err, ack.Type)
	}
	for _, topic := range topics {
		if err := wsjson.Write(ctx, conn, ws.Envelope{Type: ws.TypeSubscribe, Topic: topic}); err != nil {
			e.t.Fatalf("subscribe: %v", err)
		}
	}
	frames := &topicFrames{}
	go func() {
		for {
			var env ws.Envelope
			if err := wsjson.Read(ctx, conn, &env); err != nil {
				return
			}
			if env.Type != ws.TypeTopic {
				continue
			}
			frames.mu.Lock()
			frames.heard = append(frames.heard, env)
			frames.mu.Unlock()
		}
	}()
	return frames
}

// awaitWatching waits for a subscription to land on the hub.
func (e *testEnv) awaitWatching(userID int64, topic string) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.app.WS.Watching(e.ws.ID, userID, topic) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("user %d never came to be watching %s", userID, topic)
}

func (f *topicFrames) all() []ws.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ws.Envelope(nil), f.heard...)
}

// steps is every step pushed on one agent's topic, in the order it arrived.
func (f *topicFrames) steps(t *testing.T, topic string) []app.ChatMessage {
	t.Helper()
	var out []app.ChatMessage
	for _, env := range f.all() {
		if env.Topic != topic {
			continue
		}
		var push struct {
			Kind    string          `json:"kind"`
			Message app.ChatMessage `json:"message"`
		}
		if err := json.Unmarshal(env.Payload, &push); err != nil || push.Kind != "step" {
			t.Fatalf("an agent topic carried something that is not a step: %s", env.Payload)
		}
		out = append(out, push.Message)
	}
	return out
}

// awaitStep waits for a pushed step that matches.
func (f *topicFrames) awaitStep(t *testing.T, topic string, match func(app.ChatMessage) bool) app.ChatMessage {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range f.steps(t, topic) {
			if match(m) {
				return m
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no such step was pushed on %s; heard %+v", topic, f.steps(t, topic))
	return app.ChatMessage{}
}

// gatedProxy stands in front of a fake model and holds every call until it is
// opened, so a test can be watching before the agent does anything.
func gatedProxy(t *testing.T, target string) (string, func()) {
	t.Helper()
	to, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse %s: %v", target, err)
	}
	proxy := httputil.NewSingleHostReverseProxy(to)
	proxy.FlushInterval = -1
	gate := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { open(); server.Close() })
	return server.URL, open
}

func (e *testEnv) agentWork(token, chatUID string, delegationID int64) (*httptest.ResponseRecorder, app.AgentWork) {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/v1/chat/delegation", token, map[string]any{
		"chat_id": chatUID, "delegation_id": delegationID,
	})
	var work app.AgentWork
	if rec.Code == http.StatusOK {
		e.decode(rec, &work)
	}
	return rec, work
}

func toolOf(m app.ChatMessage, name string) (app.ChatTool, bool) {
	for _, tool := range m.Tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return app.ChatTool{}, false
}

// A background agent, opened from its chip before it has done anything, and
// watched while it works: the task is there from the start, each step arrives
// on the socket as it happens (the tool call, and the tool finishing, and the
// answer), and afterwards the same request reads back the same steps with the
// answer marked as what it handed back. Nobody else hears any of it: not a
// colleague subscribed to the same channel, not the owner's own tab that is not
// watching.
func TestAnAgentOpenedFromItsChipShowsItsWorkAsItHappens(t *testing.T) {
	env := newTestEnv(t)
	owner := env.createUser("owner@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("owner@acme.test", "dev-Passw0rd!")
	colleague := env.createUser("colleague@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	colleagueToken, _ := env.login("colleague@acme.test", "dev-Passw0rd!")

	target := env.spareModel()
	agentVendor := newFakeVendor(t,
		agentParkingCall(target), // a tool call, run at once in an auto conversation
		answerChunks("TECHNICAL disabled model, payload=WATCHXYZ"),
	)
	gated, open := gatedProxy(t, agentVendor.server.URL)
	env.agentOnURL("ops", "Ops", gated, "set_model_status")

	gateway := newFakeVendor(t,
		GatewayDelegatesWith("I'll have Ops handle it.", "ops", "take the spare model offline", model.HandoffBackground),
		answerChunks("I've started Ops in the background."),
		answerChunks("Done, the spare model is offline."),
	)
	modelID := env.registerModel(gateway)
	env.GatewayAgent()

	session := env.seededChat(owner.ID)
	if err := env.app.Store.Agent().SetSessionApprovalMode(context.Background(), session.ID, model.ApprovalAuto); err != nil {
		t.Fatalf("set auto-approve: %v", err)
	}
	env.streamTurn(token, map[string]any{
		"prompt": "take the spare model offline", "model_id": modelID, "chat_id": session.UID,
	})
	delID := env.latestDelegationID(session.ID)

	// Opened before it has done anything: the task, and nothing yet. The task
	// is its own step on the record, written under this run, and it is shown
	// as the task rather than as a message.
	rec, work := env.agentWork(token, session.UID, delID)
	env.expectStatus(rec, http.StatusOK)
	if work.Run.Status != model.DelegationRunning || work.Run.Task != "take the spare model offline" ||
		work.Run.Name != "Ops" || len(work.Messages) != 0 {
		t.Fatalf("the agent opened wrong before it started: %+v", work)
	}
	written, err := env.app.Store.Agent().DelegationSteps(context.Background(), session.ID, delID)
	if err != nil || len(written) != 1 || written[0].Kind != model.StepUser || written[0].DelegationID != delID {
		t.Fatalf("the task was not written under its run: %v %+v", err, written)
	}

	// Somebody else's chat does not exist for them.
	rec, _ = env.agentWork(colleagueToken, session.UID, delID)
	env.expectStatus(rec, http.StatusNotFound)

	topic := app.AgentTopic(delID)
	watching := env.watch(token, topic)
	nosy := env.watch(colleagueToken, topic)
	otherTab := env.watch(token)
	env.awaitWatching(owner.ID, topic)
	env.awaitWatching(colleague.ID, topic)

	open()
	env.waitForDelegation(delID, model.DelegationDone)

	// Every step, as it happened.
	finished := watching.awaitStep(t, topic, func(m app.ChatMessage) bool {
		call, ok := toolOf(m, "set_model_status")
		return ok && call.Status == model.ToolCallCompleted
	})
	answer := watching.awaitStep(t, topic, func(m app.ChatMessage) bool {
		return m.Content == "TECHNICAL disabled model, payload=WATCHXYZ"
	})
	pushed := watching.steps(t, topic)
	firstOf := func(id string) int {
		for i, m := range pushed {
			if m.ID == id {
				return i
			}
		}
		return -1
	}
	if firstOf(finished.ID) > firstOf(answer.ID) {
		t.Fatalf("the steps arrived out of order: %+v", pushed)
	}
	for _, m := range pushed {
		if m.Role == model.StepUser {
			t.Fatalf("the task was pushed as a message: %+v", m)
		}
	}

	// Read back, it is the same record, with the answer marked as the result.
	rec, work = env.agentWork(token, session.UID, delID)
	env.expectStatus(rec, http.StatusOK)
	if work.Run.Status != model.DelegationDone || len(work.Messages) != 2 {
		t.Fatalf("the finished agent reads back wrong: %+v", work)
	}
	if work.Messages[0].ID != finished.ID || work.Messages[1].ID != answer.ID || work.ResultID != answer.ID {
		t.Fatalf("what was pushed and what is read back disagree: pushed %+v, read %+v", pushed, work)
	}

	// And nobody else heard a word of it.
	time.Sleep(300 * time.Millisecond)
	if heard := nosy.all(); len(heard) != 0 {
		t.Fatalf("a colleague watching the same channel heard the agent: %+v", heard)
	}
	if heard := otherTab.all(); len(heard) != 0 {
		t.Fatalf("a tab not watching the agent heard it: %+v", heard)
	}
}

// A batch opened from its chip: every agent in it, each with only its own work,
// even when the same agent was given two of its tasks under the one call; and
// when the batch moves, whoever has it open is told where each agent stands.
func TestAFleetOpenedFromItsChipShowsEachAgentApart(t *testing.T) {
	env := newTestEnv(t)
	owner := env.createUser("owner@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("owner@acme.test", "dev-Passw0rd!")
	env.createUser("colleague@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	colleagueToken, _ := env.login("colleague@acme.test", "dev-Passw0rd!")
	env.agent("ops", "Ops", "Do the work.")
	ctx := context.Background()

	session := env.seededChat(owner.ID)
	agentStore := env.app.Store.Agent()
	fleet := &model.AgentFleet{
		SessionID: session.ID, WorkspaceID: env.ws.ID, ParentToolCallID: "call_fleet",
		Size: 2, Deadline: time.Now().UTC().Add(time.Hour),
	}
	if err := agentStore.CreateFleet(ctx, fleet); err != nil {
		t.Fatalf("create fleet: %v", err)
	}
	members := []*model.AgentDelegation{
		{SessionID: session.ID, WorkspaceID: env.ws.ID, ParentToolCallID: "call_fleet", AgentKey: "ops",
			Mode: model.HandoffFleet, Task: "price gold in London", Status: model.DelegationRunning},
		{SessionID: session.ID, WorkspaceID: env.ws.ID, ParentToolCallID: "call_fleet", AgentKey: "ops",
			Mode: model.HandoffFleet, Task: "price gold in New York", Status: model.DelegationRunning},
	}
	if err := agentStore.CreateFleetMembers(ctx, fleet.ID, members); err != nil {
		t.Fatalf("create members: %v", err)
	}
	// Both members write under the one parent call and the one agent key,
	// interleaved, which is exactly what could not be told apart before.
	write := func(del *model.AgentDelegation, kind, text string) *model.AgentStep {
		seq, err := agentStore.NextSeq(ctx, session.ID)
		if err != nil {
			t.Fatalf("seq: %v", err)
		}
		step := &model.AgentStep{
			SessionID: session.ID, Seq: seq, Kind: kind, Text: text,
			AgentKey: "ops", ParentToolCallID: "call_fleet", DelegationID: del.ID,
		}
		if err := agentStore.SaveStep(ctx, step); err != nil {
			t.Fatalf("save step: %v", err)
		}
		return step
	}
	write(members[0], model.StepUser, members[0].Task)
	write(members[1], model.StepUser, members[1].Task)
	london := write(members[0], model.StepAssistant, "London: 2,391.20")
	write(members[1], model.StepAssistant, "New York: 2,398.75")

	var runs struct {
		Runs []app.AgentRun `json:"runs"`
	}
	rec := env.do(http.MethodPost, "/v1/chat/fleet", token, map[string]any{"chat_id": session.UID, "fleet_id": fleet.ID})
	env.expectStatus(rec, http.StatusOK)
	env.decode(rec, &runs)
	if len(runs.Runs) != 2 || runs.Runs[0].Task != "price gold in London" || runs.Runs[1].Task != "price gold in New York" ||
		runs.Runs[0].Status != model.DelegationRunning || runs.Runs[0].Name != "Ops" {
		t.Fatalf("the batch opened wrong: %+v", runs.Runs)
	}
	rec = env.do(http.MethodPost, "/v1/chat/fleet", colleagueToken, map[string]any{"chat_id": session.UID, "fleet_id": fleet.ID})
	env.expectStatus(rec, http.StatusNotFound)

	// One member opened: its own work and nothing of its twin's.
	rec, work := env.agentWork(token, session.UID, members[0].ID)
	env.expectStatus(rec, http.StatusOK)
	if len(work.Messages) != 1 || work.Messages[0].Content != "London: 2,391.20" {
		t.Fatalf("a member shows work that is not its own: %+v", work.Messages)
	}

	// Its last words are what it handed back: marked, not repeated.
	result := func(text string) json.RawMessage {
		raw, _ := json.Marshal(map[string]string{"result": text})
		return raw
	}
	if err := agentStore.CompleteDelegation(ctx, members[0].ID, model.DelegationDone, result("London: 2,391.20"), ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	_, work = env.agentWork(token, session.UID, members[0].ID)
	if work.ResultID != strconv.FormatInt(london.ID, 10) || len(work.Messages) != 1 {
		t.Fatalf("the answer was not marked as the result: %+v", work)
	}
	// What it handed back is not always its last words (a step limit, a record
	// written before steps named their run). Then the answer is shown from the
	// row, once, at the end.
	if err := agentStore.CompleteDelegation(ctx, members[1].ID, model.DelegationDone, result("New York, final: 2,401.10"), ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	_, work = env.agentWork(token, session.UID, members[1].ID)
	if n := len(work.Messages); n != 2 || work.Messages[1].Content != "New York, final: 2,401.10" ||
		work.ResultID != work.Messages[1].ID {
		t.Fatalf("the result on the row was not shown: %+v", work)
	}

	// A third member still running is stopped with its batch, and whoever has
	// the batch open hears where every agent stands.
	topic := app.FleetTopic(fleet.ID)
	watching := env.watch(token, topic)
	env.awaitWatching(owner.ID, topic)
	third := &model.AgentDelegation{SessionID: session.ID, WorkspaceID: env.ws.ID, ParentToolCallID: "call_fleet",
		AgentKey: "ops", Mode: model.HandoffFleet, Task: "price gold in Zurich", Status: model.DelegationRunning}
	if err := agentStore.CreateFleetMembers(ctx, fleet.ID, []*model.AgentDelegation{third}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	if !env.app.CancelFleet(ctx, env.ws.ID, fleet.ID) {
		t.Fatal("the batch was not stopped")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got []app.AgentRun
		for _, frame := range watching.all() {
			var push struct {
				Kind string         `json:"kind"`
				Runs []app.AgentRun `json:"runs"`
			}
			if json.Unmarshal(frame.Payload, &push) == nil && push.Kind == "fleet" && frame.Topic == topic {
				got = push.Runs
			}
		}
		// A stopped batch's unfinished members are recorded failed, with the
		// reason (AbandonFleetMembers), which is what the person is shown.
		if len(got) == 3 && got[2].Status == model.DelegationFailed && got[2].Error == "this task was cancelled" &&
			got[0].Status == model.DelegationDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the batch being stopped was never pushed: %+v", watching.all())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A fleet member's spend is counted the way a background agent's is: the
// vendor's own usage, added up on the member's row as it works, pushed to
// whoever has the batch open while it works, and answered when the batch is
// opened. And it is KEPT when another process writes the same record: the
// server marking a member resumed starts from what the row says, not from zero.
func TestAFleetMemberSpendIsCountedAndKept(t *testing.T) {
	env := newTestEnv(t)
	owner := env.createUser("owner@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("owner@acme.test", "dev-Passw0rd!")
	ctx := context.Background()

	// The vendor reports what the call cost, on the last chunk, as they do.
	agentVendor := newFakeVendor(t, append(answerChunks("London: 2,391.20"),
		`{"choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":34,"total_tokens":1234}}`))
	env.agentOnURL("ops", "Ops", agentVendor.server.URL)

	session := env.seededChat(owner.ID)
	agentStore := env.app.Store.Agent()
	fleet := &model.AgentFleet{
		SessionID: session.ID, WorkspaceID: env.ws.ID, ParentToolCallID: "call_fleet",
		Size: 2, Deadline: time.Now().UTC().Add(time.Hour),
	}
	if err := agentStore.CreateFleet(ctx, fleet); err != nil {
		t.Fatalf("create fleet: %v", err)
	}
	members := []*model.AgentDelegation{
		{SessionID: session.ID, WorkspaceID: env.ws.ID, ParentToolCallID: "call_fleet", AgentKey: "ops",
			Mode: model.HandoffFleet, Task: "price gold in London", Status: model.DelegationRunning},
		{SessionID: session.ID, WorkspaceID: env.ws.ID, ParentToolCallID: "call_fleet", AgentKey: "ops",
			Mode: model.HandoffFleet, Task: "price gold in New York", Status: model.DelegationRunning},
	}
	if err := agentStore.CreateFleetMembers(ctx, fleet.ID, members); err != nil {
		t.Fatalf("create members: %v", err)
	}

	topic := app.FleetTopic(fleet.ID)
	watching := env.watch(token, topic)
	env.awaitWatching(owner.ID, topic)

	payload, _ := json.Marshal(map[string]int64{"delegation_id": members[0].ID, "fleet_id": fleet.ID})
	if err := env.app.RunFleetMemberJob(ctx, &model.Job{Kind: model.JobKindAgentRun, Payload: payload}); err != nil {
		t.Fatalf("run member: %v", err)
	}

	// While it worked, the batch's watcher was told what it had spent.
	deadline := time.Now().Add(10 * time.Second)
	for {
		heard := int64(0)
		for _, frame := range watching.all() {
			var push struct {
				Kind string         `json:"kind"`
				Runs []app.AgentRun `json:"runs"`
			}
			if frame.Topic == topic && json.Unmarshal(frame.Payload, &push) == nil && push.Kind == "fleet" && len(push.Runs) == 2 {
				heard = push.Runs[0].Tokens
			}
		}
		if heard == 1234 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the member's spend was never pushed to the batch: %+v", watching.all())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Opened afterwards, the batch says the same.
	var runs struct {
		Runs []app.AgentRun `json:"runs"`
	}
	rec := env.do(http.MethodPost, "/v1/chat/fleet", token, map[string]any{"chat_id": session.UID, "fleet_id": fleet.ID})
	env.expectStatus(rec, http.StatusOK)
	env.decode(rec, &runs)
	if runs.Runs[0].Tokens != 1234 || runs.Runs[0].Status != model.DelegationDone || runs.Runs[1].Tokens != 0 {
		t.Fatalf("the batch does not say what its members spent: %+v", runs.Runs)
	}

	// The second member has spent something on a worker, and the server now
	// marks it resumed after an approval. That write replaces the whole record,
	// so it has to start from what the row says.
	spent := json.RawMessage(`{"step":3,"activity":"Fetching external data","tokens":1050,"tokens_in":1000,"tokens_out":50}`)
	if err := agentStore.UpdateDelegationProgress(ctx, members[1].ID, spent); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
	id := members[1].ID
	env.app.ResumeFleetMember(&model.ParkSnapshot{
		DelegationID: &id, SessionID: session.ID, WorkspaceID: env.ws.ID, UserID: owner.ID,
		AgentKey: "ops", ParentToolCallID: "call_fleet", HandoffMode: model.HandoffFleet,
	}, true)
	after, err := agentStore.GetDelegation(ctx, members[1].ID)
	if err != nil {
		t.Fatalf("read member: %v", err)
	}
	var kept struct {
		TokensIn  int64 `json:"tokens_in"`
		TokensOut int64 `json:"tokens_out"`
	}
	if err := json.Unmarshal(after.Progress, &kept); err != nil || kept.TokensIn != 1000 || kept.TokensOut != 50 {
		t.Fatalf("marking the member resumed lost what it had spent: %s", after.Progress)
	}
}
