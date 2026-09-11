// Package setmodelstatus is the tool that enables or disables an AI model in a
// workspace. It is the first tool that changes something, so it is the first
// that asks: disabling a model takes an assistant offline for everyone here.
package setmodelstatus

import (
	"context"
	"encoding/json"
	"errors"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolkit"
)

// Name is the tool's canonical name.
const Name = "set_model_status"

type args struct {
	ModelID int64  `json:"model_id"`
	Status  string `json:"status"`
}

// New builds the set-model-status tool.
func New(st store.Store, auth toolkit.Authorizer) tool.Tool {
	return tool.Tool{
		Schema: tool.Schema{
			Name:         Name,
			FriendlyName: "Enable or disable an AI model",
			About: "Lets the agent switch one of this workspace's AI models on or off, for an administrator " +
				"managing the setup by conversation. It changes what everybody's agents run on, so it asks " +
				"before it acts.",
			FriendlyNarration: "Updating a model's status",
			Description: "Enable or disable an AI model in this workspace. " +
				"Disabling a model stops every agent that uses it. " +
				"Call list_models first to find the model id.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"model_id": {"type": "integer", "description": "The id of the model, from list_models"},
					"status": {"type": "string", "enum": ["active", "disabled"], "description": "The status to set"}
				},
				"required": ["model_id", "status"]
			}`),
			Kind:             tool.KindBuiltin,
			Risk:             tool.RiskAdminAction,
			RequiresApproval: true,
			ApprovalTitle:    "Turn an AI model on or off",
			ApprovalPrompt:   "This decides whether the workspace can use this model at all. Turning one off stops every agent that relies on it, straight away.",
			// Infrastructure/testing: real and grantable (it is the suite's
			// stand-in for an approval-gated tool), but enabling or disabling models
			// is an administrator's job in the console, not a chat assistant's, so it
			// is not advertised in the tools UI.
			Hidden: true,
		},
		Handle: func(ctx context.Context, call tool.Call) (tool.Result, error) {
			var a args
			if err := json.Unmarshal(call.Args, &a); err != nil {
				return toolkit.BadArguments("the arguments were not understood")
			}
			if a.Status != model.StatusActive && a.Status != model.StatusDisabled {
				return toolkit.BadArguments("status must be active or disabled")
			}
			if err := toolkit.Require(ctx, auth, call.UserID, model.PermModelsEdit); err != nil {
				return toolkit.Denied()
			}

			target, err := st.AIModels().GetByID(ctx, call.WorkspaceID, a.ModelID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return toolkit.BadArguments("no such model in this workspace; call list_models for the valid ids")
				}
				return tool.Result{}, err
			}
			target.Status = a.Status
			if err := st.AIModels().Update(ctx, target); err != nil {
				return tool.Result{}, err
			}
			return toolkit.Success(map[string]any{
				"success": true,
				"model":   target.ModelKey,
				"status":  target.Status,
			})
		},
	}
}
