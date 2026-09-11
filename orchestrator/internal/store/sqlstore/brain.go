package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/store/sqldb"
)

// Brains.
//
// One store backs BOTH the admin console and the agent's tool. There is no
// second copy of this logic behind the tool, which is what stops the two drifting
// into disagreeing about what a brain is: a document an agent writes is a
// document a person edits.

type brainStore struct{ db *sqldb.DB }

// --- brains ---------------------------------------------------------------------

func (s *brainStore) CreateBrain(ctx context.Context, b *model.Brain) error {
	now := time.Now().UTC()
	b.CreatedAt, b.UpdatedAt = now, now
	if b.Slug == "" {
		if b.Slug = model.Slugify(b.Name); b.Slug == "" {
			// A name of nothing but punctuation still needs a handle, and a
			// brain with no slug could not be named by an agent at all.
			b.Slug = "brain"
		}
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO brains (workspace_id, name, slug, description, is_locked, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		b.WorkspaceID, b.Name, b.Slug, nullString(b.Description), b.Locked, b.CreatedAt, b.UpdatedAt)
	if err != nil {
		return wrapWriteErr("insert brain", err)
	}
	b.ID, err = res.LastInsertId()
	return err
}

func (s *brainStore) UpdateBrain(ctx context.Context, b *model.Brain) error {
	if err := requireExists(ctx, s.db, "update brain",
		`SELECT 1 FROM brains WHERE id = ? AND workspace_id = ?`, b.ID, b.WorkspaceID); err != nil {
		return err
	}
	b.UpdatedAt = time.Now().UTC()
	if b.Slug == "" {
		if b.Slug = model.Slugify(b.Name); b.Slug == "" {
			// A name of nothing but punctuation still needs a handle, and a
			// brain with no slug could not be named by an agent at all.
			b.Slug = "brain"
		}
	}

	_, err := s.db.ExecContext(ctx,
		`UPDATE brains SET name = ?, slug = ?, description = ?, is_locked = ?, updated_at = ?
		 WHERE id = ? AND workspace_id = ?`,
		b.Name, b.Slug, nullString(b.Description), b.Locked, b.UpdatedAt, b.ID, b.WorkspaceID)
	if err != nil {
		return wrapWriteErr("update brain", err)
	}
	return nil
}

// DeleteBrain takes its categories, its documents and their links with it. The
// database enforces that, not this function (KB/14).
func (s *brainStore) DeleteBrain(ctx context.Context, workspaceID, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM brains WHERE id = ? AND workspace_id = ?`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("delete brain: %w", err)
	}
	return requireAffected(res, "delete brain")
}

// Brains lists a workspace's knowledge bases, with the counts a list needs to be
// worth reading: an empty brain looks exactly like a full one until it says so.
func (s *brainStore) Brains(ctx context.Context, workspaceID int64) ([]*model.Brain, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT b.id, b.workspace_id, b.name, b.slug, b.description, b.is_locked,
		        b.created_at, b.updated_at,
		        (SELECT COUNT(*) FROM brain_categories c WHERE c.brain_id = b.id),
		        (SELECT COUNT(*) FROM brain_documents d WHERE d.brain_id = b.id)
		 FROM brains b
		 WHERE b.workspace_id = ?
		 ORDER BY b.name`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list brains: %w", err)
	}
	defer func() { _ = rows.Close() }()

	brains := []*model.Brain{}
	for rows.Next() {
		b := &model.Brain{}
		var description sql.NullString
		if err := rows.Scan(&b.ID, &b.WorkspaceID, &b.Name, &b.Slug, &description, &b.Locked,
			&b.CreatedAt, &b.UpdatedAt, &b.Categories, &b.Documents); err != nil {
			return nil, fmt.Errorf("scan brain: %w", err)
		}
		b.Description = description.String
		brains = append(brains, b)
	}
	return brains, rows.Err()
}

func (s *brainStore) Brain(ctx context.Context, workspaceID, id int64) (*model.Brain, error) {
	b := &model.Brain{}
	var description sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT b.id, b.workspace_id, b.name, b.slug, b.description, b.is_locked,
		        b.created_at, b.updated_at,
		        (SELECT COUNT(*) FROM brain_categories c WHERE c.brain_id = b.id),
		        (SELECT COUNT(*) FROM brain_documents d WHERE d.brain_id = b.id)
		 FROM brains b WHERE b.id = ? AND b.workspace_id = ?`, id, workspaceID).
		Scan(&b.ID, &b.WorkspaceID, &b.Name, &b.Slug, &description, &b.Locked,
			&b.CreatedAt, &b.UpdatedAt, &b.Categories, &b.Documents)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan brain: %w", err)
	}
	b.Description = description.String
	return b, nil
}

// --- categories ------------------------------------------------------------------

// Categories lists a brain's sections. The brain is checked against the
// workspace here, so a caller cannot list the categories of a brain that is not
// theirs by knowing its id.
func (s *brainStore) Categories(ctx context.Context, workspaceID, brainID int64) ([]*model.BrainCategory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.brain_id, c.name, c.description, c.weight, c.created_at, c.updated_at,
		        (SELECT COUNT(*) FROM brain_documents d WHERE d.category_id = c.id)
		 FROM brain_categories c
		 JOIN brains b ON b.id = c.brain_id
		 WHERE b.workspace_id = ? AND c.brain_id = ?
		 ORDER BY c.weight, c.name`, workspaceID, brainID)
	if err != nil {
		return nil, fmt.Errorf("list categories: %w", err)
	}
	defer func() { _ = rows.Close() }()

	categories := []*model.BrainCategory{}
	for rows.Next() {
		c := &model.BrainCategory{}
		var description sql.NullString
		if err := rows.Scan(&c.ID, &c.BrainID, &c.Name, &description, &c.Weight,
			&c.CreatedAt, &c.UpdatedAt, &c.Documents); err != nil {
			return nil, fmt.Errorf("scan category: %w", err)
		}
		c.Description = description.String
		categories = append(categories, c)
	}
	return categories, rows.Err()
}

func (s *brainStore) CreateCategory(ctx context.Context, workspaceID int64, c *model.BrainCategory) error {
	if err := s.requireBrain(ctx, workspaceID, c.BrainID); err != nil {
		return err
	}
	now := time.Now().UTC()
	c.CreatedAt, c.UpdatedAt = now, now

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO brain_categories (brain_id, name, description, weight, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.BrainID, c.Name, nullString(c.Description), c.Weight, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return wrapWriteErr("insert category", err)
	}
	c.ID, err = res.LastInsertId()
	return err
}

func (s *brainStore) UpdateCategory(ctx context.Context, workspaceID int64, c *model.BrainCategory) error {
	if err := requireExists(ctx, s.db, "update category",
		`SELECT 1 FROM brain_categories c JOIN brains b ON b.id = c.brain_id
		 WHERE c.id = ? AND b.workspace_id = ?`, c.ID, workspaceID); err != nil {
		return err
	}
	c.UpdatedAt = time.Now().UTC()

	_, err := s.db.ExecContext(ctx,
		`UPDATE brain_categories SET name = ?, description = ?, weight = ?, updated_at = ?
		 WHERE id = ?`,
		c.Name, nullString(c.Description), c.Weight, c.UpdatedAt, c.ID)
	if err != nil {
		return wrapWriteErr("update category", err)
	}
	return nil
}

func (s *brainStore) DeleteCategory(ctx context.Context, workspaceID, id int64) error {
	if err := requireExists(ctx, s.db, "delete category",
		`SELECT 1 FROM brain_categories c JOIN brains b ON b.id = c.brain_id
		 WHERE c.id = ? AND b.workspace_id = ?`, id, workspaceID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM brain_categories WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete category: %w", err)
	}
	return nil
}

// --- documents -------------------------------------------------------------------

// Documents lists a category's documents WITHOUT their content.
//
// A category may hold hundreds, and their bodies are Markdown that nobody is
// reading yet. The content is fetched one document at a time, when somebody opens
// it, which is the difference between a list that renders and a list that hangs.
func (s *brainStore) Documents(ctx context.Context, workspaceID, categoryID int64) ([]*model.BrainDocument, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.id, d.brain_id, d.category_id, d.title, d.weight, d.created_at, d.updated_at
		 FROM brain_documents d
		 JOIN brains b ON b.id = d.brain_id
		 WHERE b.workspace_id = ? AND d.category_id = ?
		 ORDER BY d.weight, d.title`, workspaceID, categoryID)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	documents := []*model.BrainDocument{}
	for rows.Next() {
		d := &model.BrainDocument{}
		if err := rows.Scan(&d.ID, &d.BrainID, &d.CategoryID, &d.Title, &d.Weight,
			&d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		documents = append(documents, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return documents, s.attachLinkCounts(ctx, documents)
}

// Document is one document, with its content and its links: what you need to
// read it and to follow it onwards.
func (s *brainStore) Document(ctx context.Context, workspaceID, id int64) (*model.BrainDocument, error) {
	d := &model.BrainDocument{}
	var content sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT d.id, d.brain_id, d.category_id, d.title, d.content, d.weight,
		        d.created_at, d.updated_at
		 FROM brain_documents d
		 JOIN brains b ON b.id = d.brain_id
		 WHERE b.workspace_id = ? AND d.id = ?`, workspaceID, id).
		Scan(&d.ID, &d.BrainID, &d.CategoryID, &d.Title, &content, &d.Weight,
			&d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan document: %w", err)
	}
	d.Content = content.String

	related, err := s.related(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	d.Related = related
	return d, nil
}

// DocumentByTitle locates a document by (category, title), the unique pair, so a
// write that should update an existing document rather than duplicate it can find
// the id to write to. It carries no content or links: the caller is resolving an
// id, not reading the document.
func (s *brainStore) DocumentByTitle(ctx context.Context, workspaceID, categoryID int64, title string) (*model.BrainDocument, error) {
	d := &model.BrainDocument{}
	err := s.db.QueryRowContext(ctx,
		`SELECT d.id, d.brain_id, d.category_id, d.title, d.weight, d.created_at, d.updated_at
		 FROM brain_documents d
		 JOIN brains b ON b.id = d.brain_id
		 WHERE b.workspace_id = ? AND d.category_id = ? AND d.title = ?`,
		workspaceID, categoryID, title).
		Scan(&d.ID, &d.BrainID, &d.CategoryID, &d.Title, &d.Weight, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("document by title: %w", err)
	}
	return d, nil
}

// SaveDocument writes a document and its links, atomically.
//
// An id of zero creates. Anything else updates.
func (s *brainStore) SaveDocument(ctx context.Context, workspaceID int64, d *model.BrainDocument, related []int64) error {
	// The category decides the brain. A document whose brain_id disagreed with
	// its category's would be reachable from a search and invisible in the tree.
	brainID, err := s.brainOfCategory(ctx, workspaceID, d.CategoryID)
	if err != nil {
		return err
	}

	// A document cannot MOVE between brains. A brain is a separate context: its
	// documents relate to each other and to nothing outside, and carrying one
	// across would either drag its links out of the brain or silently cut them.
	// Moving it between categories OF THE SAME BRAIN is fine, and is the only
	// move there is.
	if d.ID != 0 {
		var current int64
		err := s.db.QueryRowContext(ctx,
			`SELECT brain_id FROM brain_documents d
			 JOIN brains b ON b.id = d.brain_id
			 WHERE d.id = ? AND b.workspace_id = ?`, d.ID, workspaceID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("resolve document: %w", err)
		}
		if current != brainID {
			return fmt.Errorf("a document cannot move between brains: %w", store.ErrWrongBrain)
		}
	}
	d.BrainID = brainID

	now := time.Now().UTC()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now

	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		if d.ID == 0 {
			res, err := tx.ExecContext(ctx,
				`INSERT INTO brain_documents
				  (brain_id, category_id, title, content, weight, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				d.BrainID, d.CategoryID, d.Title, nullString(d.Content), d.Weight,
				d.CreatedAt, d.UpdatedAt)
			if err != nil {
				return wrapWriteErr("insert document", err)
			}
			if d.ID, err = res.LastInsertId(); err != nil {
				return err
			}
		} else {
			if err := requireExists(ctx, tx, "update document",
				`SELECT 1 FROM brain_documents d JOIN brains b ON b.id = d.brain_id
				 WHERE d.id = ? AND b.workspace_id = ?`, d.ID, workspaceID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE brain_documents
				 SET brain_id = ?, category_id = ?, title = ?, content = ?, weight = ?, updated_at = ?
				 WHERE id = ?`,
				d.BrainID, d.CategoryID, d.Title, nullString(d.Content), d.Weight,
				d.UpdatedAt, d.ID); err != nil {
				return wrapWriteErr("update document", err)
			}
		}
		return syncLinks(ctx, tx, d.ID, d.BrainID, related)
	})
}

func (s *brainStore) DeleteDocument(ctx context.Context, workspaceID, id int64) error {
	if err := requireExists(ctx, s.db, "delete document",
		`SELECT 1 FROM brain_documents d JOIN brains b ON b.id = d.brain_id
		 WHERE d.id = ? AND b.workspace_id = ?`, id, workspaceID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM brain_documents WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete document: %w", err)
	}
	return nil
}

// --- the graph ---------------------------------------------------------------------

// syncLinks makes the graph say what the caller asked for, in BOTH directions.
//
// The links are symmetric on purpose. A one-directional link would let a
// document be reachable from one side and invisible from the other, which for
// somebody trying to find it is the same as being lost.
//
// A link may only join documents in the SAME brain: the graph is a brain's
// internal structure, and a link out of it would let an agent reach, through a
// document it was allowed, a brain it was not.
func syncLinks(ctx context.Context, tx *sqldb.Tx, documentID, brainID int64, related []int64) error {
	// Both directions of what this document currently claims.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM brain_document_links WHERE document_id = ? OR related_id = ?`,
		documentID, documentID); err != nil {
		return fmt.Errorf("clear links: %w", err)
	}

	seen := map[int64]bool{documentID: true} // a document does not relate to itself
	for _, other := range related {
		if seen[other] {
			continue
		}
		seen[other] = true

		// The INSERT ... SELECT is the constraint: a document from another brain
		// simply matches nothing, so it cannot be linked, and no separate check
		// can be forgotten.
		if _, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO brain_document_links (document_id, related_id)
			 SELECT ?, id FROM brain_documents WHERE id = ? AND brain_id = ?`,
			documentID, other, brainID); err != nil {
			return fmt.Errorf("link document: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO brain_document_links (document_id, related_id)
			 SELECT id, ? FROM brain_documents WHERE id = ? AND brain_id = ?`,
			documentID, other, brainID); err != nil {
			return fmt.Errorf("link document back: %w", err)
		}
	}
	return nil
}

func (s *brainStore) related(ctx context.Context, documentID int64) ([]model.BrainLink, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.id, d.title, c.id, c.name
		 FROM brain_document_links l
		 JOIN brain_documents d ON d.id = l.related_id
		 JOIN brain_categories c ON c.id = d.category_id
		 WHERE l.document_id = ?
		 ORDER BY d.title`, documentID)
	if err != nil {
		return nil, fmt.Errorf("list related: %w", err)
	}
	defer func() { _ = rows.Close() }()

	links := []model.BrainLink{}
	for rows.Next() {
		var link model.BrainLink
		if err := rows.Scan(&link.ID, &link.Title, &link.CategoryID, &link.Category); err != nil {
			return nil, fmt.Errorf("scan related: %w", err)
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// attachLinkCounts fills in how many documents each one relates to, in one query
// rather than one per row.
func (s *brainStore) attachLinkCounts(ctx context.Context, documents []*model.BrainDocument) error {
	if len(documents) == 0 {
		return nil
	}
	byID := make(map[int64]*model.BrainDocument, len(documents))
	for _, d := range documents {
		// Start every document with an empty (non-nil) list, so one with no links
		// serialises as [] rather than null. A caller that maps over related must
		// never meet a null where the shape promises an array.
		d.Related = []model.BrainLink{}
		byID[d.ID] = d
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT l.document_id, d.id, d.title, c.name
		 FROM brain_document_links l
		 JOIN brain_documents d ON d.id = l.related_id
		 JOIN brain_categories c ON c.id = d.category_id
		 WHERE l.document_id IN (
		   SELECT id FROM brain_documents WHERE category_id = ?
		 )
		 ORDER BY d.title`, documents[0].CategoryID)
	if err != nil {
		return fmt.Errorf("list links: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var owner int64
		var link model.BrainLink
		if err := rows.Scan(&owner, &link.ID, &link.Title, &link.Category); err != nil {
			return fmt.Errorf("scan link: %w", err)
		}
		if d, ok := byID[owner]; ok {
			d.Related = append(d.Related, link)
		}
	}
	return rows.Err()
}

// --- search ------------------------------------------------------------------------

// Search finds documents by relevance across the given brains.
//
// It returns the SNIPPET that matched, not just a title. The agent has to decide
// whether a document is worth opening, and a title alone is not something you can
// decide with: "Billing" tells it nothing, and the sentence that mentioned the
// refund window tells it everything.
//
// brainIDs is the allow-list. An empty list finds nothing, deliberately: an agent
// with no brains assigned reaches no brains at all.
func (s *brainStore) Search(ctx context.Context, workspaceID int64, brainIDs []int64, query string, limit int) ([]model.BrainHit, error) {
	if len(brainIDs) == 0 || strings.TrimSpace(query) == "" {
		return []model.BrainHit{}, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}

	term := booleanTerm(query)
	if term == "" {
		// Everything the person typed was too short to index. Say nothing rather
		// than returning the whole brain, which is what an empty MATCH does.
		return []model.BrainHit{}, nil
	}

	// The allow-list is bound as ONE value, never spliced into the statement:
	// this is the security boundary of the whole feature, and a boundary
	// assembled from strings is not one (KB/14).
	rows, err := s.db.QueryContext(ctx, searchDocumentsStmt,
		term, workspaceID, allowed(brainIDs), term, limit)
	if err != nil {
		return nil, fmt.Errorf("search documents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	hits := []model.BrainHit{}
	for rows.Next() {
		var hit model.BrainHit
		var content sql.NullString
		if err := rows.Scan(&hit.DocumentID, &hit.BrainID, &hit.Brain, &hit.Category,
			&hit.Title, &content, &hit.Score); err != nil {
			return nil, fmt.Errorf("scan hit: %w", err)
		}
		hit.Snippet = snippet(content.String, query)
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// The allow-list is ONE bound parameter whatever its length: a comma-separated
// list, matched with FIND_IN_SET. The statement is therefore a constant, and no
// query is ever assembled from a caller's ids (KB/14). The full-text index
// drives the query, so the list is a filter over the hits rather than a scan.
const searchDocumentsStmt = `
	SELECT d.id, d.brain_id, b.name, c.name, d.title, d.content,
	       MATCH(d.title, d.content) AGAINST (? IN BOOLEAN MODE) AS score
	FROM brain_documents d
	JOIN brains b ON b.id = d.brain_id
	JOIN brain_categories c ON c.id = d.category_id
	WHERE b.workspace_id = ?
	  AND FIND_IN_SET(d.brain_id, ?)
	  AND MATCH(d.title, d.content) AGAINST (? IN BOOLEAN MODE)
	ORDER BY score DESC, d.title
	LIMIT ?`

// allowed renders the brain ids as the one value FIND_IN_SET takes. They are ids
// we read from our own database, never anything a caller typed.
func allowed(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

// snippet is the text AROUND the match, so the reader sees why the document came
// back. A snippet from the top of the document would look identical for every
// hit and tell nobody anything.
const snippetRadius = 120

func snippet(content, query string) string {
	if content == "" {
		return ""
	}
	flat := strings.Join(strings.Fields(content), " ")

	lower := strings.ToLower(flat)
	at := -1
	for _, word := range strings.Fields(strings.ToLower(query)) {
		if found := strings.Index(lower, word); found != -1 {
			at = found
			break
		}
	}
	if at == -1 {
		// It matched on the title, or on a word stem the index knows and a
		// literal search does not. The opening is the best we can honestly offer.
		return truncate(flat, snippetRadius*2)
	}

	start := max(at-snippetRadius, 0)
	end := min(at+snippetRadius, len(flat))

	out := flat[start:end]
	if start > 0 {
		out = "..." + out
	}
	if end < len(flat) {
		out += "..."
	}
	return out
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}

// --- the allow-list -----------------------------------------------------------------

// AgentBrains is what an agent may reach. Every operation of the tool intersects
// this, so a brain nobody assigned is a brain nobody's agent can read.
func (s *brainStore) AgentBrains(ctx context.Context, workspaceID, agentID int64) ([]*model.Brain, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT b.id, b.workspace_id, b.name, b.slug, b.description, b.is_locked,
		        b.created_at, b.updated_at
		 FROM agent_brains ab
		 JOIN brains b ON b.id = ab.brain_id
		 WHERE ab.agent_id = ? AND b.workspace_id = ?
		 ORDER BY b.name`, agentID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list agent brains: %w", err)
	}
	defer func() { _ = rows.Close() }()

	brains := []*model.Brain{}
	for rows.Next() {
		b := &model.Brain{}
		var description sql.NullString
		if err := rows.Scan(&b.ID, &b.WorkspaceID, &b.Name, &b.Slug, &description, &b.Locked,
			&b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan brain: %w", err)
		}
		b.Description = description.String
		brains = append(brains, b)
	}
	return brains, rows.Err()
}

// --- helpers --------------------------------------------------------------------------

func (s *brainStore) requireBrain(ctx context.Context, workspaceID, brainID int64) error {
	return requireExists(ctx, s.db, "resolve brain",
		`SELECT 1 FROM brains WHERE id = ? AND workspace_id = ?`, brainID, workspaceID)
}

// brainOfCategory answers which brain a category belongs to, and refuses one that
// is not this workspace's. It is the single place a document's brain is decided,
// so a document can never end up in a brain its category is not in.
func (s *brainStore) brainOfCategory(ctx context.Context, workspaceID, categoryID int64) (int64, error) {
	var brainID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT c.brain_id FROM brain_categories c
		 JOIN brains b ON b.id = c.brain_id
		 WHERE c.id = ? AND b.workspace_id = ?`, categoryID, workspaceID).Scan(&brainID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, store.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("resolve category: %w", err)
	}
	return brainID, nil
}
