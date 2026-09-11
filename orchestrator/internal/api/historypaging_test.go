package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"flexie.io/sag/internal/model"
)

// A conversation is read in pages, and a page is counted in ROWS.
//
// The whole thing used to be sent on every open. On a real conversation that is
// a hundred and fifty steps, their tool calls, and nine thousand nodes in the
// page, every time somebody clicks it in the sidebar: a payload that grows
// without end, and measurably about half the cost of typing a character into
// the composer with it on the screen.
//
// A row is a message OR a tool call, because both are a line on the screen and
// both are part of what a page costs. Counting steps would have let one step
// with forty tool calls through as "one".
func TestAConversationArrivesInPagesCountedInRows(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// A conversation longer than one page: 140 rows of it.
	chatID := env.longConversation(t, token, 140)

	rec := env.do(http.MethodPost, "/v1/chat/history", token, map[string]any{"chat_id": chatID})
	env.expectStatus(rec, http.StatusOK)
	var page historyResponse
	env.decode(rec, &page)

	rows := rowsIn(page)
	if rows > model.HistoryRows {
		t.Fatalf("a page carried %d rows, over the %d it may", rows, model.HistoryRows)
	}
	if rows == 0 {
		t.Fatal("the newest page is empty")
	}
	if !page.Meta.More {
		t.Fatalf("a conversation of 140 rows said there was nothing older, having sent %d", rows)
	}
	if page.Meta.Oldest == 0 {
		t.Fatal("the page does not say where the one before it starts")
	}

	// And it is the NEWEST page: the last message of the conversation is in it.
	last := page.Messages[len(page.Messages)-1]
	if last.Content == "" && len(last.Tools) == 0 {
		t.Fatal("the newest page ends on nothing")
	}

	// Scrolling back: the page before, which is older and does not repeat.
	rec = env.do(http.MethodPost, "/v1/chat/history", token,
		map[string]any{"chat_id": chatID, "before": page.Meta.Oldest})
	env.expectStatus(rec, http.StatusOK)
	var older historyResponse
	env.decode(rec, &older)
	if len(older.Messages) == 0 {
		t.Fatal("there was said to be more, and asking for it gave nothing")
	}
	if older.Meta.Oldest >= page.Meta.Oldest {
		t.Fatalf("the older page starts at %d, which is not before %d",
			older.Meta.Oldest, page.Meta.Oldest)
	}
	newest := map[string]bool{}
	for _, m := range page.Messages {
		newest[m.ID] = true
	}
	for _, m := range older.Messages {
		if newest[m.ID] {
			t.Fatalf("message %s is in both pages", m.ID)
		}
	}
}

// And a short conversation arrives whole, with nothing behind it: paging must
// not turn a five-message chat into a thing with a "load more".
func TestAShortConversationArrivesWholeAndSaysSo(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	chatID := env.longConversation(t, token, 6)

	rec := env.do(http.MethodPost, "/v1/chat/history", token, map[string]any{"chat_id": chatID})
	env.expectStatus(rec, http.StatusOK)
	var page historyResponse
	env.decode(rec, &page)

	if page.Meta.More {
		t.Fatal("a six-row conversation claims there is more behind it")
	}
	if rowsIn(page) != 6 {
		t.Fatalf("a six-row conversation arrived as %d rows", rowsIn(page))
	}
}

func rowsIn(page historyResponse) int {
	rows := 0
	for _, m := range page.Messages {
		if m.Content != "" {
			rows++
		}
		rows += len(m.Tools)
	}
	return rows
}

// longConversation writes a conversation of about the given number of ROWS,
// alternating what somebody said with what the assistant did, so a page
// boundary can fall in either.
func (e *testEnv) longConversation(t *testing.T, token string, rows int) string {
	t.Helper()
	user, err := e.app.Store.Users().GetByEmail(context.Background(), "admin@acme.test")
	if err != nil {
		t.Fatalf("find the person: %v", err)
	}
	session := e.seededChat(user.ID)

	for seq, written := 1, 0; written < rows; seq += 2 {
		asked := &model.AgentStep{
			SessionID: session.ID, Seq: seq, Kind: model.StepUser,
			Text: "question " + itoa64(int64(seq)), CreatedAt: time.Now().UTC(),
		}
		if err := e.app.Store.Agent().SaveStep(context.Background(), asked); err != nil {
			t.Fatalf("write a question: %v", err)
		}
		answered := &model.AgentStep{
			SessionID: session.ID, Seq: seq + 1, Kind: model.StepAssistant,
			Text: "answer " + itoa64(int64(seq)), CreatedAt: time.Now().UTC(),
			ToolCalls: []*model.ToolCall{{
				SessionID: session.ID, WorkspaceID: e.ws.ID,
				ToolCallID: "call_" + itoa64(int64(seq)), ToolName: "current_time",
				FriendlyName: "Current time", Args: json.RawMessage(`{}`),
				Result: json.RawMessage(`{"utc":"2026-09-04T00:00:00Z"}`),
				Status: model.ToolCallCompleted, CreatedAt: time.Now().UTC(),
			}},
		}
		if err := e.app.Store.Agent().SaveStep(context.Background(), answered); err != nil {
			t.Fatalf("write an answer: %v", err)
		}
		written += 3 // a question, an answer, and the one tool row on it
	}
	return session.UID
}
