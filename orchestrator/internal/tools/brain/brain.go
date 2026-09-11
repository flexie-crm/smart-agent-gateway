// Package brain is the agent's window onto the knowledge bases (brains) assigned
// to it: a tool to search and read them. Every operation is confined to the
// allow-list the loadout binds the handler to for the turn, so an agent can only
// ever reach a brain it was given. The documents themselves never ride in the
// prompt; the agent drills down here when it needs them.
package brain

import (
	"context"
	"strings"

	"flexie.io/sag/internal/model"
)

// ReadStore is the narrow slice of the brain store the read tool needs. The full
// BrainStore satisfies it; naming only what is used keeps the tool's reach
// honest and its tests small.
type ReadStore interface {
	Brains(ctx context.Context, workspaceID int64) ([]*model.Brain, error)
	Categories(ctx context.Context, workspaceID, brainID int64) ([]*model.BrainCategory, error)
	Document(ctx context.Context, workspaceID, id int64) (*model.BrainDocument, error)
	Search(ctx context.Context, workspaceID int64, brainIDs []int64, query string, limit int) ([]model.BrainHit, error)
}

// idSet turns an allow-list into a membership set.
func idSet(ids []int64) map[int64]bool {
	set := make(map[int64]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// reachable returns the allowed brains in the order the store lists them. It is
// the one place "which brains can this agent see" is answered from the store, so
// discover and a named search cannot disagree about it.
func reachable(ctx context.Context, st ReadStore, workspaceID int64, allowed []int64) ([]*model.Brain, error) {
	set := idSet(allowed)
	all, err := st.Brains(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	kept := make([]*model.Brain, 0, len(set))
	for _, b := range all {
		if set[b.ID] {
			kept = append(kept, b)
		}
	}
	return kept, nil
}

// matchBrain finds a reachable brain the agent named, by slug or by name
// (case-insensitively). It returns nil when nothing matches, and the caller
// turns that into "not one of yours" WITHOUT revealing whether it exists in some
// other agent's set: a brain the agent was not assigned does not exist to it.
func matchBrain(brains []*model.Brain, ref string) *model.Brain {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	slug := model.Slugify(ref)
	for _, b := range brains {
		if strings.EqualFold(b.Name, ref) || b.Slug == ref || b.Slug == slug {
			return b
		}
	}
	return nil
}

// preview shortens a document body to a glance: enough to judge whether it is
// worth opening in full, without carrying the whole thing.
func preview(content string) string {
	const limit = 240
	content = strings.TrimSpace(content)
	if len([]rune(content)) <= limit {
		return content
	}
	return string([]rune(content)[:limit]) + "…"
}
