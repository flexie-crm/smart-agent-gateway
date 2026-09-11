package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// The chat list, and finding a conversation again.
//
// Search runs over the title AND the messages, because people look for a chat
// by what it was about, and a title is only a summary of that. A conversation
// matches if either does.

const chatColumns = `id, uid, title, is_pinned, last_message_at, message_count`

// The two shapes a chat list comes in. Neither is assembled: listing and
// searching are different questions, so they are different statements.
const (
	listChatsStmt = `SELECT ` + chatColumns + ` FROM agent_sessions
		 WHERE workspace_id = ? AND user_id = ?
		 ORDER BY is_pinned DESC, COALESCE(last_message_at, started_at) DESC, id DESC
		 LIMIT ?`

	// A conversation matches if its title does, or if anything said in it does.
	searchChatsStmt = `SELECT ` + chatColumns + ` FROM agent_sessions
		 WHERE workspace_id = ? AND user_id = ?
		   AND (
		     MATCH(title) AGAINST (? IN BOOLEAN MODE)
		     OR EXISTS (
		       SELECT 1 FROM agent_messages m
		       WHERE m.session_id = agent_sessions.id
		         AND MATCH(m.content) AGAINST (? IN BOOLEAN MODE)
		     )
		   )
		 ORDER BY is_pinned DESC, COALESCE(last_message_at, started_at) DESC, id DESC
		 LIMIT ?`
)

func (s *agentStore) ListChats(ctx context.Context, q store.ChatQuery) ([]*model.ChatListItem, error) {
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		// A sidebar is not a data export. A caller asking for everything is
		// asking for a slow query, not a feature.
		limit = 50
	}

	var (
		rows *sql.Rows
		err  error
	)
	if term := booleanTerm(q.Query); term != "" {
		rows, err = s.db.QueryContext(ctx, searchChatsStmt,
			q.WorkspaceID, q.UserID, term, term, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, listChatsStmt, q.WorkspaceID, q.UserID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	chats := []*model.ChatListItem{}
	for rows.Next() {
		item := &model.ChatListItem{}
		var title sql.NullString
		if err := rows.Scan(&item.ID, &item.UID, &title, &item.IsPinned, &item.LastMessageAt,
			&item.MessageCount); err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}
		item.Title = title.String
		chats = append(chats, item)
	}
	return chats, rows.Err()
}

// booleanTerm turns what a person typed into a safe full-text query.
//
// Boolean mode gives the user operators (+, -, *, "", ()) whose syntax errors
// would fail the whole query, so the operators are stripped: someone searching
// for "invoice (draft)" means the words, not a grouping. Each word gets a
// trailing wildcard, because searching as you type should match a word you have
// not finished spelling.
func booleanTerm(query string) string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		switch r {
		case '+', '-', '~', '<', '>', '(', ')', '"', '*', '@':
			return true
		default:
			return r == ' ' || r == '\t' || r == '\n'
		}
	})

	terms := make([]string, 0, len(fields))
	for _, word := range fields {
		// A one or two letter word is below the index's token size, so it
		// matches nothing and would silently empty the result.
		if len([]rune(word)) < 3 {
			continue
		}
		terms = append(terms, "+"+word+"*")
	}
	return strings.Join(terms, " ")
}

// UpdateChat and DeleteChat are addressed by the public uid, because that is
// what a request carries. The row id never leaves the server.
func (s *agentStore) UpdateChat(ctx context.Context, workspaceID, userID int64, uid string, update store.ChatUpdate) error {
	id, err := s.chatRowID(ctx, workspaceID, userID, uid)
	if err != nil {
		return err
	}

	// Each field is its own statement rather than a SET clause assembled from
	// strings. Assembling SQL is how injection gets in, even when today's
	// fragments are all constants, and two small writes are cheaper than that
	// risk.
	if update.Title != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE agent_sessions SET title = ? WHERE id = ?`,
			nullString(*update.Title), id); err != nil {
			return fmt.Errorf("rename chat: %w", err)
		}
	}
	if update.IsPinned != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE agent_sessions SET is_pinned = ? WHERE id = ?`,
			*update.IsPinned, id); err != nil {
			return fmt.Errorf("pin chat: %w", err)
		}
	}
	return nil
}

// DeleteChat removes the conversation and everything it produced. A user
// deleting a chat means it is gone, not hidden.
func (s *agentStore) DeleteChat(ctx context.Context, workspaceID, userID int64, uid string) error {
	id, err := s.chatRowID(ctx, workspaceID, userID, uid)
	if err != nil {
		return err
	}

	// One statement. The steps, the messages, the reasoning, the tool calls, the
	// cost records, the parked approvals and the runs all go with it, because
	// each of them IS part of the conversation and the database enforces that.
	// This used to be six deletes in a transaction, which worked exactly as long
	// as nobody added a seventh table and forgot to add a seventh line.
	res, err := s.db.ExecContext(ctx, `DELETE FROM agent_sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete chat: %w", err)
	}
	return requireAffected(res, "delete chat")
}

// TouchChat records that something was said, which is what the list sorts by.
//
// The count is recomputed rather than incremented. An increment is a guess that
// every write happened exactly once, and a checkpoint that refines a step in
// place would break it: the number would climb while the conversation stayed
// the same length. Counting the rows cannot drift from the rows.
func (s *agentStore) TouchChat(ctx context.Context, sessionID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_sessions
		 SET last_message_at = ?,
		     message_count = (SELECT COUNT(*) FROM agent_messages m WHERE m.session_id = ?)
		 WHERE id = ?`, time.Now().UTC(), sessionID, sessionID)
	if err != nil {
		return fmt.Errorf("touch chat: %w", err)
	}
	return nil
}

// chatRowID resolves a public uid to the row it names, but only for the person
// who owns it: a conversation that is not yours does not exist.
func (s *agentStore) chatRowID(ctx context.Context, workspaceID, userID int64, uid string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM agent_sessions WHERE uid = ? AND workspace_id = ? AND user_id = ?`,
		uid, workspaceID, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, store.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("resolve chat: %w", err)
	}
	return id, nil
}
