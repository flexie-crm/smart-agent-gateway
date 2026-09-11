package app

import (
	"context"
	"fmt"

	"flexie.io/sag/internal/model"
)

// SyncTools reconciles a workspace's tool table with the tools this build
// actually has code for.
//
// The registry is the source of truth for what a tool IS. The table is the
// source of truth for what the workspace DECIDED about it. Sync writes the
// first into the second without ever touching the second's own columns: a
// deploy that improves a tool's description must not quietly re-enable a tool
// an administrator switched off, nor drop the grants that restrict it.
//
// It runs at boot for every workspace, so a tool added in a release is on
// offer the moment the server comes up, rather than the first time somebody
// happens to open an admin page.
func (a *App) SyncTools(ctx context.Context, workspaceID int64) error {
	registered := a.Tools.All()
	rows := make([]*model.Tool, 0, len(registered))
	for _, t := range registered {
		rows = append(rows, &model.Tool{
			WorkspaceID:      workspaceID,
			Name:             t.Schema.Name,
			Kind:             string(t.Schema.Kind),
			FriendlyName:     t.Schema.FriendlyName,
			Description:      t.Schema.Description,
			InputSchema:      t.Schema.InputSchema,
			Risk:             string(t.Schema.Risk),
			RequiresApproval: t.Schema.RequiresApproval,
		})
	}
	if len(rows) == 0 {
		return nil
	}
	if err := a.Store.Tools().Sync(ctx, workspaceID, rows); err != nil {
		return fmt.Errorf("sync tools: %w", err)
	}
	return nil
}

// SyncAllTools brings every workspace up to date with this build.
func (a *App) SyncAllTools(ctx context.Context) error {
	workspaces, err := a.Store.Workspaces().List(ctx)
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}
	for _, w := range workspaces {
		if err := a.SyncTools(ctx, w.ID); err != nil {
			return fmt.Errorf("workspace %s: %w", w.Slug, err)
		}
	}
	a.Log.Info().Int("workspaces", len(workspaces)).Int("tools", len(a.Tools.All())).
		Msg("tools synced")
	return nil
}
