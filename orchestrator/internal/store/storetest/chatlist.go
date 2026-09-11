package storetest

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// The chat list, and finding a conversation again.
//
// FULLTEXT search is the kind of thing that looks right until a real query
// meets it, so these run against real SQL and real indexes.

func mustChat(t *testing.T, st store.Store, wsID, userID int64, title string) *model.AgentSession {
	t.Helper()
	s := &model.AgentSession{
		WorkspaceID: wsID, UserID: userID,
		Channel: model.ChannelChat, Title: title,
	}
	if err := st.Agent().CreateSession(ctx(), s); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	return s
}

func say(t *testing.T, st store.Store, sessionID int64, seq int, role, content string) {
	t.Helper()
	kind := model.StepAssistant
	if role == model.RoleUser {
		kind = model.StepUser
	}
	if err := st.Agent().SaveStep(ctx(), &model.AgentStep{
		SessionID: sessionID, Seq: seq, Kind: kind, Text: content,
	}); err != nil {
		t.Fatalf("save step: %v", err)
	}
	if err := st.Agent().TouchChat(ctx(), sessionID); err != nil {
		t.Fatalf("touch chat: %v", err)
	}
}

func chatIDs(chats []*model.ChatListItem) []int64 {
	ids := make([]int64, 0, len(chats))
	for _, c := range chats {
		ids = append(ids, c.ID)
	}
	return ids
}

func contains(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func testChatList(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	user := mustUser(t, st, ws.ID, "u@acme.test")
	other := mustUser(t, st, ws.ID, "other@acme.test")

	first := mustChat(t, st, ws.ID, user.ID, "Invoice rounding")
	say(t, st, first.ID, 1, model.RoleUser, "why do invoice totals disagree")
	second := mustChat(t, st, ws.ID, user.ID, "Sales onboarding")
	say(t, st, second.ID, 1, model.RoleUser, "how should we train new staff")

	// Another person's conversation is not merely invisible: it is not theirs.
	theirs := mustChat(t, st, ws.ID, other.ID, "Not yours")
	say(t, st, theirs.ID, 1, model.RoleUser, "invoice invoice invoice")

	chats, err := st.Agent().ListChats(ctx(), store.ChatQuery{
		WorkspaceID: ws.ID, UserID: user.ID, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list chats: %v", err)
	}
	ids := chatIDs(chats)
	if len(ids) != 2 || contains(ids, theirs.ID) {
		t.Fatalf("the list crossed a user boundary: %+v", ids)
	}
	// Newest first: the list is a memory aid, and the last thing said is the
	// easiest thing to find.
	if ids[0] != second.ID {
		t.Fatalf("expected the most recent chat first, got %+v", ids)
	}

	// Message count and last-said are what the list sorts and shows.
	if chats[0].MessageCount != 1 || chats[0].LastMessageAt == nil {
		t.Fatalf("the list does not know when anything was said: %+v", chats[0])
	}

	// Every conversation carries an opaque public id, and it is not its row id.
	for _, c := range chats {
		if !strings.HasPrefix(c.UID, model.ChatUIDPrefix) {
			t.Fatalf("a chat has no public id: %+v", c)
		}
		if c.UID == strconv.FormatInt(c.ID, 10) {
			t.Fatal("the public id is the row id")
		}
	}
}

// testChatSearch is the point of the FULLTEXT indexes: a conversation is
// remembered by what it was ABOUT, and the title is only a summary of that.
func testChatSearch(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	user := mustUser(t, st, ws.ID, "u@acme.test")
	other := mustUser(t, st, ws.ID, "other@acme.test")

	invoices := mustChat(t, st, ws.ID, user.ID, "Invoice rounding errors")
	say(t, st, invoices.ID, 1, model.RoleUser, "why do the line items disagree with the total")

	// This chat's title says nothing about kubernetes. The word appears only
	// inside the conversation, which is exactly the case that matters.
	deploys := mustChat(t, st, ws.ID, user.ID, "Deployment question")
	say(t, st, deploys.ID, 1, model.RoleUser, "how do I roll back a kubernetes release")

	// Another user says the same word. It must never surface.
	theirs := mustChat(t, st, ws.ID, other.ID, "Their kubernetes chat")
	say(t, st, theirs.ID, 1, model.RoleUser, "kubernetes kubernetes")

	search := func(query string) []int64 {
		t.Helper()
		chats, err := st.Agent().ListChats(ctx(), store.ChatQuery{
			WorkspaceID: ws.ID, UserID: user.ID, Query: query, Limit: 10,
		})
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		return chatIDs(chats)
	}

	// By title.
	if ids := search("invoice"); len(ids) != 1 || ids[0] != invoices.ID {
		t.Fatalf("searching a title found %+v", ids)
	}

	// By what was SAID, when the title never mentioned it. This is the whole
	// reason the messages are indexed.
	if ids := search("kubernetes"); len(ids) != 1 || ids[0] != deploys.ID {
		t.Fatalf("searching the conversation content found %+v", ids)
	}

	// A partial word matches: search-as-you-type should find a word you have
	// not finished spelling.
	if ids := search("kubern"); len(ids) != 1 || ids[0] != deploys.ID {
		t.Fatalf("a prefix search found %+v", ids)
	}

	// Several words narrow rather than widen: every term must match.
	if ids := search("invoice rounding"); len(ids) != 1 || ids[0] != invoices.ID {
		t.Fatalf("a multi-word search found %+v", ids)
	}
	if ids := search("invoice kubernetes"); len(ids) != 0 {
		t.Fatalf("terms must narrow, not widen: %+v", ids)
	}

	// Nonsense finds nothing rather than everything.
	if ids := search("zzzznotathing"); len(ids) != 0 {
		t.Fatalf("a search for nothing found %+v", ids)
	}

	// Boolean operators a person typed are words, not syntax. Without this the
	// query would be a FULLTEXT syntax error and fail the whole request.
	if ids := search("invoice (rounding)"); len(ids) != 1 || ids[0] != invoices.ID {
		t.Fatalf("punctuation broke the search: %+v", ids)
	}
	if ids := search("+++"); len(ids) == 0 {
		// Nothing searchable was typed, so this lists rather than errors.
		t.Fatal("a query of only operators must not fail the request")
	}
}

func testChatManagement(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	user := mustUser(t, st, ws.ID, "u@acme.test")
	other := mustUser(t, st, ws.ID, "other@acme.test")

	chat := mustChat(t, st, ws.ID, user.ID, "Original")
	say(t, st, chat.ID, 1, model.RoleUser, "hello")

	// Rename.
	title := "Renamed"
	if err := st.Agent().UpdateChat(ctx(), ws.ID, user.ID, chat.UID,
		store.ChatUpdate{Title: &title}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// Pin. A partial update must leave the title alone.
	pinned := true
	if err := st.Agent().UpdateChat(ctx(), ws.ID, user.ID, chat.UID,
		store.ChatUpdate{IsPinned: &pinned}); err != nil {
		t.Fatalf("pin: %v", err)
	}
	chats, err := st.Agent().ListChats(ctx(), store.ChatQuery{
		WorkspaceID: ws.ID, UserID: user.ID, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if chats[0].Title != "Renamed" {
		t.Fatalf("pinning a chat overwrote its title: %q", chats[0].Title)
	}
	if !chats[0].IsPinned {
		t.Fatal("the pin was not stored")
	}

	// A pinned chat sorts to the top even when it is not the most recent.
	newer := mustChat(t, st, ws.ID, user.ID, "Newer")
	say(t, st, newer.ID, 1, model.RoleUser, "later")
	chats, _ = st.Agent().ListChats(ctx(), store.ChatQuery{
		WorkspaceID: ws.ID, UserID: user.ID, Limit: 10,
	})
	if chats[0].ID != chat.ID {
		t.Fatalf("a pinned chat did not sort to the top: %+v", chatIDs(chats))
	}

	// Another user cannot touch it.
	if err := st.Agent().UpdateChat(ctx(), ws.ID, other.ID, chat.UID,
		store.ChatUpdate{Title: &title}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another user renamed a chat: %v", err)
	}
	if err := st.Agent().DeleteChat(ctx(), ws.ID, other.ID, chat.UID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another user deleted a chat: %v", err)
	}

	// Deleting means gone, along with what the conversation produced.
	if err := st.Agent().DeleteChat(ctx(), ws.ID, user.ID, chat.UID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Agent().GetSession(ctx(), ws.ID, chat.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the chat survived its deletion: %v", err)
	}
	steps, err := st.Agent().Transcript(ctx(), chat.ID)
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("the transcript outlived the conversation: %d steps", len(steps))
	}
}
