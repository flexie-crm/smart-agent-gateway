// Package recall is how an agent reads what it has remembered.
//
// The notes used to be in the system prompt, both scopes of them, on every turn
// of every conversation. That is the right place for something small and fixed,
// and memory is neither: it is written to in the background whenever a
// conversation teaches something, so it only grows, and a prompt that carries it
// grows with it forever. What was 948 bytes on a young installation is a
// thousand turns of an old one.
//
// So the prompt says WHAT is known of (there are notes about this person, there
// are working notes for this workspace) and this is how they are read. The same
// rule the abilities and the knowledge bases already follow: the map is in the
// prompt, the territory is fetched when it is wanted.
//
// Read-only, and internal: it is infrastructure, like remembering itself, so it
// rides along with any tool set and is never something an administrator grants.
package recall

import (
	"context"
	"encoding/json"
	"strings"

	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolkit"
)

// Name is the tool's canonical name.
const Name = "recall"

// Memories is what this tool needs of the store: the two scopes, read.
//
// An interface rather than the whole store, so what the tool can do is what its
// name says: there is no write here, and nothing else is reachable through it.
type Memories interface {
	UserMemory(ctx context.Context, workspaceID, userID int64) (string, error)
	WorkspaceMemory(ctx context.Context, workspaceID int64) (string, error)
}

// New builds the recall tool over a store.
func New(st store.Store) tool.Tool {
	var memories Memories
	if st != nil {
		memories = st.Memory()
	}
	return tool.Tool{
		Schema: Schema(),
		Handle: func(ctx context.Context, call tool.Call) (tool.Result, error) {
			if memories == nil {
				return toolkit.Failed("there is nothing remembered here")
			}
			var args struct {
				Scope string `json:"scope"`
			}
			_ = json.Unmarshal(call.Args, &args)

			answer := map[string]any{}
			scope := strings.TrimSpace(strings.ToLower(args.Scope))
			if scope == "" || scope == "person" {
				notes, err := memories.UserMemory(ctx, call.WorkspaceID, call.UserID)
				if err != nil {
					return toolkit.Failed("what you know about this person could not be read")
				}
				answer["person"] = present(notes)
			}
			if scope == "" || scope == "workspace" {
				notes, err := memories.WorkspaceMemory(ctx, call.WorkspaceID)
				if err != nil {
					return toolkit.Failed("your working notes could not be read")
				}
				answer["workspace"] = present(notes)
			}
			if len(answer) == 0 {
				return toolkit.BadArguments("scope must be \"person\", \"workspace\", or left out for both")
			}
			return toolkit.Success(answer)
		},
	}
}

// Schema is what the model is told.
func Schema() tool.Schema {
	input, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scope": map[string]any{
				"type": "string",
				"enum": []string{"person", "workspace"},
				"description": "Which notes to read: \"person\" for what you have learned about the person you are " +
					"helping and how they like their answers, \"workspace\" for your own working notes on how the " +
					"work is done well here. Leave it out for both.",
			},
		},
	})
	return tool.Schema{
		Name:         Name,
		FriendlyName: "Recall what you know",
		About: "Lets the agent read back what it has remembered about the person and about how work is done " +
			"here, rather than carrying all of it in every conversation. It reads only.",
		Description: "Read what you remembered earlier: scope \"person\" for who you are helping and " +
			"how they like their answers, \"workspace\" for how the work is done here. Omit scope for " +
			"both. Read it when you start.",
		InputSchema: input,
		Kind:        tool.KindInternal,
		Risk:        tool.RiskReadOnly,
	}
}

// present turns an empty scope into something a model reads as an answer rather
// than as a failure: nothing remembered yet is a fact, not an error.
func present(notes string) string {
	if strings.TrimSpace(notes) == "" {
		return "(nothing remembered yet)"
	}
	return notes
}
