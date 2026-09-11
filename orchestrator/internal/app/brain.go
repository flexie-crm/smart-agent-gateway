package app

import (
	"context"
	"errors"
	"fmt"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// BrainSelection is what the caller says they are looking at. Any of it may be
// zero, which means "you decide".
type BrainSelection struct {
	BrainID    int64
	CategoryID int64
	DocumentID int64
}

// BrainOverview is the whole screen in one answer: the brains, the categories of
// the one selected, the documents of the category selected, and the document
// itself.
//
// It exists because the alternative is a client that asks four questions in a
// row, each one waiting on the answer before it (which brain? then which
// categories? then which documents? then which document?). That is four round
// trips to paint one page, and the person watching it sees the columns appear
// empty and fill in one at a time.
//
// The DEFAULTS are decided here rather than in the browser, for the same reason:
// a client that has to fetch the brains, pick the first, then ask for its
// categories, pick the first of those, has re-created the waterfall with extra
// steps. What "the first one" means is a product decision, and it belongs on the
// server that already has the rows.
type BrainOverview struct {
	Brains     []*model.Brain         `json:"brains"`
	BrainID    int64                  `json:"brain_id"`
	Categories []*model.BrainCategory `json:"categories"`
	CategoryID int64                  `json:"category_id"`
	Documents  []*model.BrainDocument `json:"documents"`
	DocumentID int64                  `json:"document_id"`
	// Document is the open one, read in full: the listed ones carry no content,
	// because a category may hold hundreds and nobody is reading them yet.
	Document *model.BrainDocument `json:"document"`
}

// BrainView resolves a selection into everything needed to draw it.
//
// A selection that names something gone (a deleted brain, a document that moved)
// is not an error: it is a stale link or a back button, and the answer is the
// same as asking for nothing, which is the first of everything.
func (a *App) BrainView(ctx context.Context, workspaceID int64, want BrainSelection) (*BrainOverview, error) {
	brains, err := a.Store.Brains().Brains(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list brains: %w", err)
	}
	view := &BrainOverview{
		Brains:     brains,
		Categories: []*model.BrainCategory{},
		Documents:  []*model.BrainDocument{},
	}
	if len(brains) == 0 {
		return view, nil
	}

	view.BrainID = firstOr(want.BrainID, brains, func(b *model.Brain) int64 { return b.ID })

	categories, err := a.Store.Brains().Categories(ctx, workspaceID, view.BrainID)
	if err != nil {
		return nil, fmt.Errorf("list categories: %w", err)
	}
	view.Categories = categories
	if len(categories) == 0 {
		return view, nil
	}

	view.CategoryID = firstOr(want.CategoryID, categories, func(c *model.BrainCategory) int64 { return c.ID })

	documents, err := a.Store.Brains().Documents(ctx, workspaceID, view.CategoryID)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	view.Documents = documents
	if len(documents) == 0 {
		return view, nil
	}

	view.DocumentID = firstOr(want.DocumentID, documents, func(d *model.BrainDocument) int64 { return d.ID })

	document, err := a.Store.Brains().Document(ctx, workspaceID, view.DocumentID)
	if errors.Is(err, store.ErrNotFound) {
		view.DocumentID = 0
		return view, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load document: %w", err)
	}
	view.Document = document
	return view, nil
}

// firstOr keeps the caller's choice if it is one of the things they can have,
// and otherwise gives them the first. It is what makes a stale URL land
// somewhere real instead of on an empty pane.
func firstOr[T any](want int64, items []T, id func(T) int64) int64 {
	for _, item := range items {
		if id(item) == want {
			return want
		}
	}
	return id(items[0])
}
