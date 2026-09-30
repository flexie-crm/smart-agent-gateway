package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// Compacting a conversation, through the real server, the real database and
// the real loop: only the model is a stand-in. What these prove is what the
// model is SENT, read off the requests the stand-in received, because that is
// the whole point of compacting and nothing else can show it.

// said is one scripted model reply that says text and stops.
func said(t *testing.T, text string) []string {
	t.Helper()
	content, err := json.Marshal(text)
	if err != nil {
		t.Fatalf("script a reply: %v", err)
	}
	return []string{
		`{"choices":[{"index":0,"delta":{"content":` + string(content) + `}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
}

// waitUntilNamed waits for the background naming call to finish, so it cannot
// land between two calls a test has scripted in order and take one of their
// replies.
func (e *testEnv) waitUntilNamed(sessionID int64) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fresh, err := e.app.Store.Agent().GetSession(context.Background(), e.ws.ID, sessionID)
		if err != nil {
			e.t.Fatalf("get session: %v", err)
		}
		if fresh.Title != "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatal("the conversation was never named, so the scripted replies are out of step")
}

// compact asks for a conversation to be compacted, which is accepted at once and
// done in the background, and waits for it to be done.
func (e *testEnv) compact(token string, session *model.AgentSession, modelID int64) {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/v1/chat/chats/compact/"+session.UID, token, map[string]any{"model_id": modelID})
	e.expectStatus(rec, http.StatusAccepted)
	e.waitUntilCompacted(session.ID)
}

// waitUntilCompacted waits for the compaction under way to give the
// conversation back.
func (e *testEnv) waitUntilCompacted(sessionID int64) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.app.Runs.Holding(sessionID) {
		if time.Now().After(deadline) {
			e.t.Fatal("the compaction never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sentMessages is every message the model was sent on the turn that asked
// prompt, as role and content.
func (f *fakeVendor) sentMessages(prompt string) []map[string]any {
	f.t.Helper()
	raw, _ := f.turnAsking(prompt)["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]any)
		out = append(out, m)
	}
	return out
}

// askedToSummarize is the request that asked for a summary, found by what it
// was told to do rather than by position: the naming call can land anywhere.
func (f *fakeVendor) askedToSummarize() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, body := range f.prompts {
		messages, _ := body["messages"].([]any)
		if len(messages) != 2 {
			continue
		}
		first, _ := messages[0].(map[string]any)
		second, _ := messages[1].(map[string]any)
		instruction, _ := first["content"].(string)
		if first["role"] == "system" && strings.HasPrefix(instruction, "You are compacting") {
			text, _ := second["content"].(string)
			return text, true
		}
	}
	return "", false
}

func mentions(messages []map[string]any, text string) bool {
	for _, m := range messages {
		if content, _ := m["content"].(string); strings.Contains(content, text) {
			return true
		}
	}
	return false
}

// The whole feature in one conversation. Before compacting, a turn is sent
// everything said so far; after it, the summary and nothing it covers; and the
// person's view of the conversation does not change at all.
func TestACompactedConversationIsSentItsSummaryInsteadOfItsHistory(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	const summary = "The person asked about Albania: the capital is Tirana, with about 900,000 people."
	vendor := newFakeVendor(t,
		said(t, "Tirana."),
		said(t, "Asking about Albania"), // the conversation naming itself
		said(t, "About 900,000."),
		said(t, summary),
		said(t, "Pristina."),
	)
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "what is the capital of albania?", "model_id": modelID})
	session := env.sessionOf(frames)
	env.waitUntilNamed(session.ID)
	env.streamTurn(token, map[string]any{"prompt": "and its population?", "model_id": modelID, "chat_id": session.UID})

	// The control: with no summary, a turn is sent the whole conversation. If
	// this does not hold, the assertions after compacting prove nothing.
	before := vendor.sentMessages("and its population?")
	if !mentions(before, "what is the capital of albania?") || !mentions(before, "Tirana.") {
		t.Fatalf("an uncompacted turn was not sent the conversation so far: %v", before)
	}

	env.compact(token, session, modelID)

	// The summary was written from the whole conversation, tool calls and all.
	text, ok := vendor.askedToSummarize()
	if !ok {
		t.Fatal("the model was never asked for a summary")
	}
	for _, want := range []string{"what is the capital of albania?", "Tirana.", "and its population?", "About 900,000."} {
		if !strings.Contains(text, want) {
			t.Fatalf("the summary was not asked to cover %q:\n%s", want, text)
		}
	}

	// And kept, covering the last step there was, with who asked and which
	// model wrote it.
	ctx := context.Background()
	kept, err := env.app.Store.Agent().LatestCompaction(ctx, session.ID)
	if err != nil {
		t.Fatalf("the summary was not kept: %v", err)
	}
	steps, err := env.app.Store.Agent().Transcript(ctx, session.ID)
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	if kept.Summary != summary || kept.ThroughSeq != steps[len(steps)-1].Seq {
		t.Fatalf("kept %q through %d; want %q through %d", kept.Summary, kept.ThroughSeq, summary, steps[len(steps)-1].Seq)
	}
	if kept.CreatedByName != "u@acme.test" || kept.CreatedBy == 0 || kept.Model != "fake-1" {
		t.Fatalf("the summary does not record who asked and what wrote it: %+v", kept)
	}

	// The next turn: the summary, then the new question, and nothing it covers.
	env.streamTurn(token, map[string]any{"prompt": "what about kosovo?", "model_id": modelID, "chat_id": session.UID})
	after := vendor.sentMessages("what about kosovo?")
	roles := make([]string, 0, len(after))
	for _, m := range after {
		role, _ := m["role"].(string)
		roles = append(roles, role)
	}
	if strings.Join(roles, ",") != "system,system,user" {
		t.Fatalf("the turn after compacting was sent %v, want the instructions, the summary and the question", roles)
	}
	if content, _ := after[1]["content"].(string); !strings.Contains(content, summary) {
		t.Fatalf("the summary is not where the history was: %q", content)
	}
	for _, gone := range []string{"what is the capital of albania?", "and its population?", "About 900,000."} {
		if mentions(after, gone) && !strings.Contains(summary, gone) {
			t.Fatalf("the turn after compacting was still sent %q", gone)
		}
	}
	for _, m := range after[2:] {
		if content, _ := m["content"].(string); content != "what about kosovo?" {
			t.Fatalf("something besides the new question followed the summary: %v", m)
		}
	}

	// The person's view is untouched: every question and answer is still there.
	rec := env.do(http.MethodPost, "/v1/chat/history", token, map[string]any{"chat_id": session.UID})
	env.expectStatus(rec, http.StatusOK)
	var history historyResponse
	env.decode(rec, &history)
	var shown []string
	for _, m := range history.Messages {
		shown = append(shown, m.Content)
	}
	want := []string{"what is the capital of albania?", "Tirana.", "and its population?", "About 900,000.", "what about kosovo?", "Pristina."}
	if strings.Join(shown, "|") != strings.Join(want, "|") {
		t.Fatalf("the chat shows %q after compacting, want %q", shown, want)
	}
}

// A second compaction with nothing said since the first is refused, and the
// model is not asked to summarize a summary.
func TestNothingNewIsNotCompactedAgain(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t, said(t, "Hello."), said(t, "Greeting"), said(t, "They said hello."))
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	session := env.sessionOf(env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": modelID}))
	env.waitUntilNamed(session.ID)

	env.compact(token, session, modelID)
	calls := vendor.callCount()

	rec := env.do(http.MethodPost, "/v1/chat/chats/compact/"+session.UID, token, map[string]any{"model_id": modelID})
	env.expectFields(rec, http.StatusConflict, "nothing_to_compact", nil)
	if vendor.callCount() != calls {
		t.Fatal("the model was asked again with nothing new to summarize")
	}
}

// Compacting while something in the conversation is still working is refused
// before the model is asked, and nothing is kept.
func TestACompactionWaitsForTheConversationToFinish(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t, said(t, "Hello."), said(t, "Greeting"), said(t, "They said hello."))
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	session := env.sessionOf(env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": modelID}))
	env.waitUntilNamed(session.ID)
	ctx := context.Background()

	refused := func(why string) {
		t.Helper()
		calls := vendor.callCount()
		rec := env.do(http.MethodPost, "/v1/chat/chats/compact/"+session.UID, token, map[string]any{"model_id": modelID})
		env.expectFields(rec, http.StatusConflict, "chat_busy", nil)
		if vendor.callCount() != calls {
			t.Fatalf("%s: the model was asked anyway", why)
		}
		if _, err := env.app.Store.Agent().LatestCompaction(ctx, session.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: a summary was kept anyway (%v)", why, err)
		}
	}

	// A card waiting on the person: the call it would run is inside what the
	// summary would cover.
	if err := env.app.Store.Agent().SetSessionStatus(ctx, session.ID, model.SessionWaitingApproval); err != nil {
		t.Fatalf("set status: %v", err)
	}
	refused("a card is waiting")
	if err := env.app.Store.Agent().SetSessionStatus(ctx, session.ID, model.SessionCompleted); err != nil {
		t.Fatalf("set status: %v", err)
	}

	// An agent still working in the background: its result lands on a call the
	// summary would already cover.
	delegation := &model.AgentDelegation{
		SessionID: session.ID, WorkspaceID: env.ws.ID, ParentToolCallID: "call_1",
		AgentKey: "research", Mode: model.HandoffBackground,
	}
	if err := env.app.Store.Agent().CreateDelegation(ctx, delegation); err != nil {
		t.Fatalf("create delegation: %v", err)
	}
	refused("an agent is still working")
	if err := env.app.Store.Agent().CompleteDelegation(ctx, delegation.ID, model.DelegationDone, json.RawMessage(`{}`), ""); err != nil {
		t.Fatalf("complete delegation: %v", err)
	}

	// With nothing working, the same request goes through: the refusals above
	// were about what was running, not about this conversation.
	env.compact(token, session, modelID)
}

// Somebody else's conversation cannot be compacted, and is not told apart from
// one that does not exist.
func TestAnotherPersonsConversationCannotBeCompacted(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("owner@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	env.createUser("other@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	owner, _ := env.login("owner@acme.test", "dev-Passw0rd!")
	other, _ := env.login("other@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t, said(t, "Hello."), said(t, "Greeting"))
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	session := env.sessionOf(env.streamTurn(owner, map[string]any{"prompt": "hi", "model_id": modelID}))
	env.waitUntilNamed(session.ID)
	calls := vendor.callCount()

	rec := env.do(http.MethodPost, "/v1/chat/chats/compact/"+session.UID, other, map[string]any{"model_id": modelID})
	env.expectStatus(rec, http.StatusNotFound)
	rec = env.do(http.MethodPost, "/v1/chat/chats/compact/no-such-chat", other, map[string]any{"model_id": modelID})
	env.expectStatus(rec, http.StatusNotFound)
	if vendor.callCount() != calls {
		t.Fatal("the model was asked to summarize somebody else's conversation")
	}
}

// A turn still being answered is the last thing that refuses a compaction: it
// is adding the very steps the summary would be about. The stand-in model here
// holds its answer open until the test lets it go, so the turn is really
// running when the compaction is asked for.
func TestACompactionWaitsForATurnBeingAnswered(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	release := make(chan struct{})
	var released sync.Once
	letGo := func() { released.Do(func() { close(release) }) }
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if streaming, _ := body["stream"].(bool); !streaming {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(completionJSON(t, said(t, "Anything"))))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: " + said(t, "Still thinking")[0] + "\n\n"))
		flusher.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("data: " + said(t, "")[1] + "\n\ndata: [DONE]\n\n"))
		flusher.Flush()
	}))
	// Released before the server closes, in that order (deferred calls run last
	// first): closing waits for every answer in progress, and the one held open
	// here would otherwise keep a failing test waiting for ever.
	defer vendor.Close()
	defer letGo()
	modelID := env.registerModelAt(vendor.URL)
	env.GatewayAgent()

	rec := env.do(http.MethodPost, "/v1/chat/chats/create", token, map[string]any{"title": "Busy"})
	env.expectStatus(rec, http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	env.decode(rec, &created)
	session, err := env.app.Store.Agent().GetSessionByUID(context.Background(), env.ws.ID, created.ID)
	if err != nil {
		t.Fatalf("resolve chat: %v", err)
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		env.do(http.MethodPost, "/v1/chat/stream", token, map[string]any{
			"prompt": "take your time", "model_id": modelID, "chat_id": created.ID,
		})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !env.app.Runs.Answering(session.ID) {
		if time.Now().After(deadline) {
			t.Fatal("the turn never started, so this would prove nothing")
		}
		time.Sleep(10 * time.Millisecond)
	}

	rec = env.do(http.MethodPost, "/v1/chat/chats/compact/"+created.ID, token, map[string]any{"model_id": modelID})
	env.expectFields(rec, http.StatusConflict, "chat_busy", nil)
	if _, err := env.app.Store.Agent().LatestCompaction(context.Background(), session.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a summary was kept while the turn was running (%v)", err)
	}

	letGo()
	<-finished
}
