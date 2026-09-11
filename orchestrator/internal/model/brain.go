package model

import (
	"strings"
	"time"
	"unicode"
)

// A brain is a curated knowledge base the agent navigates and writes back to.
//
//	Brain          "Product manual", "Sales playbook"
//	  Category       "Billing", "Onboarding"
//	    Document       a title and Markdown
//	      related        documents relate to documents, and the links are symmetric
//
// The vocabulary matters and it is deliberate: to a person and to the agent
// these are DOCUMENTS, never "blocks" or "chunks". A knowledge base people can
// talk about is one they will curate.

// Brain is one knowledge base.
type Brain struct {
	ID          int64     `json:"id"`
	WorkspaceID int64     `json:"workspace_id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Locked means the agent may READ this brain and never write to it, while an
	// administrator still edits it by hand. A brain of company policy is not
	// something an agent should rewrite because it inferred something.
	Locked bool `json:"locked"`

	// Counts are what a list needs to be useful. A brain with no documents in it
	// looks exactly like a brain with a thousand until somebody says so.
	Categories int `json:"categories"`
	Documents  int `json:"documents"`
}

// BrainCategory is a section of a brain.
type BrainCategory struct {
	ID          int64     `json:"id"`
	BrainID     int64     `json:"brain_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Weight      int       `json:"weight"`
	Documents   int       `json:"documents"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// BrainDocument is a document: a title, and Markdown.
//
// Never HTML. The agent writes here, and what an agent writes must not be able
// to become a script tag in somebody's browser.
type BrainDocument struct {
	ID         int64     `json:"id"`
	BrainID    int64     `json:"brain_id"`
	CategoryID int64     `json:"category_id"`
	Title      string    `json:"title"`
	Content    string    `json:"content"`
	Weight     int       `json:"weight"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	// Related is the graph. The links are symmetric, and the store keeps them so:
	// a document reachable from one side and invisible from the other is lost.
	Related []BrainLink `json:"related"`
}

// BrainLink is a document seen from another document: enough to render it and
// to follow it, without loading what is inside.
type BrainLink struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	// The category the linked document lives in, as a name to print and an id to
	// select with. The name alone made an edit form fetch every category's
	// documents, one request each, purely to find out where a link it already
	// held actually lived.
	CategoryID int64  `json:"category_id"`
	Category   string `json:"category"`
}

// BrainHit is a search result. It carries the snippet that MATCHED, because the
// agent has to decide whether a document is worth opening, and a title alone is
// not enough to decide with.
type BrainHit struct {
	DocumentID int64   `json:"document_id"`
	BrainID    int64   `json:"brain_id"`
	Brain      string  `json:"brain"`
	Category   string  `json:"category"`
	Title      string  `json:"title"`
	Snippet    string  `json:"snippet"`
	Score      float64 `json:"score"`
}

// Slugify turns a name into the handle an agent can use to name a brain without
// knowing its id.
func Slugify(name string) string {
	var out strings.Builder
	lastDash := true // no leading dash

	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			out.WriteRune(r)
			lastDash = false
		case !lastDash:
			out.WriteRune('-')
			lastDash = true
		}
	}

	slug := strings.Trim(out.String(), "-")
	if len(slug) > 120 {
		slug = strings.Trim(slug[:120], "-")
	}
	// A name of nothing but punctuation yields nothing. What to do about that
	// is the caller's decision: a brain falls back to a handle, a workspace
	// refuses the name.
	return slug
}
