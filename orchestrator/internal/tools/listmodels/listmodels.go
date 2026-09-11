// Package listmodels is the tool that lets the assistant answer questions about
// the workspace's own AI setup: which models are configured, and what they can
// do.
package listmodels

import (
	"context"
	"encoding/json"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolkit"
)

// Name is the tool's canonical name.
const Name = "list_models"

// New builds the list-models tool. It reads through the store and checks the
// caller's live permission first, like any other reader of workspace data.
func New(st store.Store, auth toolkit.Authorizer) tool.Tool {
	return tool.Tool{
		Schema: tool.Schema{
			Name:         Name,
			FriendlyName: "List the available AI models",
			About: "Lets the agent see which AI models this workspace has available and which are switched on. " +
				"It is for administrative conversations about the setup itself, not for everyday work.",
			FriendlyNarration: "Checking the available models",
			Description:       "List the AI models configured in this workspace, with their capabilities and status.",
			InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
			Kind:              tool.KindBuiltin,
			Risk:              tool.RiskReadOnly,
			// Infrastructure/testing: real and grantable, but not advertised in the
			// admin catalog. The orchestrator's general assistants have no business
			// listing the workspace's model configuration.
			Hidden: true,
		},
		Handle: func(ctx context.Context, call tool.Call) (tool.Result, error) {
			if err := toolkit.Require(ctx, auth, call.UserID, model.PermModelsView); err != nil {
				return toolkit.Denied()
			}
			models, err := st.AIModels().List(ctx, call.WorkspaceID)
			if err != nil {
				return tool.Result{}, err
			}
			out := make([]map[string]any, 0, len(models))
			for _, m := range models {
				out = append(out, map[string]any{
					"id":     m.ID,
					"model":  m.ModelKey,
					"type":   m.Type,
					"status": m.Status,
				})
			}
			return toolkit.Success(map[string]any{"models": out})
		},
	}
}
