// Package toolguide is the tool that surfaces any other tool's deep
// documentation on demand. A tool's short description is what the model sees
// every turn; its depth lives out of that path and costs nothing until asked
// for. There are two shapes of depth, and this tool serves both:
//
//   - a flat guide (the full parameter contract, precedence, limits, examples);
//   - a topic graph, a set of focused concepts the model navigates one at a
//     time, each pointing at the related ones, so it pays for only the depth it
//     needs on a hard task.
//
// Passing a tool's name returns its guide and the table of contents of its
// topics; passing a topic id returns that topic's body and its edges, which the
// model follows by calling again with the id an edge points at.
//
// It is internal infrastructure, not a capability an administrator switches on:
// it is auto-loaded whenever the assistant has any tools, and it never appears
// in the admin tool table.
package toolguide

import (
	"context"
	"encoding/json"
	"strings"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolkit"
)

// Name is the tool's canonical name.
const Name = "tool_guide"

// Registry is the subset of the tool registry this tool needs: look a tool up
// by name, find a topic by its id across all tools, and list which tools have
// documentation. Taking an interface keeps the tool testable without the whole
// registry.
type Registry interface {
	Lookup(name string) (tool.Schema, bool)
	LookupTopic(id string) (tool.Topic, string, bool)
	Guided() []string
}

// New builds the tool_guide tool over the given registry.
func New(reg Registry) tool.Tool {
	return tool.Tool{
		Schema: tool.Schema{
			Name:         Name,
			FriendlyName: "Look up how to use an ability",
			About: "Lets the agent read the full instructions for one of its own abilities before using it, " +
				"rather than carrying every instruction at all times. It reads only; it cannot do anything on its " +
				"own. Turning it off makes the agent clumsier with everything else it has.",
			Description: "Look up how to use one of your abilities in depth: its full parameters, its " +
				"limits and worked examples. Pass tool_name for a tool's guide and the list of its " +
				"topics, then a topic_id from that list (or from a topic's own links) to drill in.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"tool_name": {"type": "string", "description": "The exact name of the ability to look up. Returns its guide and the table of contents of its topics."},
					"topic_id": {"type": "string", "description": "A specific topic to open (e.g. \"http_request/auth\"), from a tool's topic list or from a topic's edges. Returns the topic's body and the related topics you can follow next."}
				},
				"required": []
			}`),
			// Internal: rides along with any tool set, invisible to the admin UI.
			Kind: tool.KindInternal,
			Risk: tool.RiskReadOnly,
		},
		Handle: Handler(reg),
	}
}

// Handler is the tool's behaviour over a given registry, so a turn can rebind it
// over the abilities that turn actually has rather than only the ones the code
// ships. Without that, a tool an administrator created is invisible here: its
// guide and topics exist and nothing can reach them.
func Handler(reg Registry) tool.Handler {
	return func(_ context.Context, call tool.Call) (tool.Result, error) {
		var args struct {
			ToolName string `json:"tool_name"`
			TopicID  string `json:"topic_id"`
		}
		_ = json.Unmarshal(call.Args, &args)
		args.ToolName = strings.TrimSpace(args.ToolName)
		args.TopicID = strings.TrimSpace(args.TopicID)

		switch {
		case args.TopicID != "":
			return openTopic(reg, args.TopicID)
		case args.ToolName != "":
			return openTool(reg, args.ToolName)
		default:
			// Neither given: point the model at what it can look up.
			return toolkit.Success(map[string]any{
				"message":          "Name an ability in tool_name to see its guide and topics, or open a topic with topic_id.",
				"guides_available": reg.Guided(),
			})
		}
	}
}

// openTopic returns one topic's body and the edges the model can follow next.
func openTopic(reg Registry, id string) (tool.Result, error) {
	topic, owner, ok := reg.LookupTopic(id)
	if !ok {
		return toolkit.Failed("there is no topic by that id; call tool_guide with tool_name to see a tool's topics. Abilities with documentation: " + join(reg.Guided()))
	}
	return toolkit.Success(map[string]any{
		"tool":        owner,
		"topic":       topic.ID,
		"title":       topic.Title,
		"body":        topic.Body,
		"related":     edges(topic.Edges),
		"next_action": "To follow a related topic, call tool_guide with topic_id set to its id.",
	})
}

// openTool returns a tool's flat guide and the table of contents of its topics.
func openTool(reg Registry, name string) (tool.Result, error) {
	schema, ok := reg.Lookup(name)
	if !ok {
		return toolkit.Failed("there is no ability by that name; the ones with documentation are: " + join(reg.Guided()))
	}
	out := map[string]any{"tool": name}
	if len(schema.Guide) > 0 {
		out["guide"] = schema.Guide
	}
	if toc := tableOfContents(schema.Topics); len(toc) > 0 {
		out["topics"] = toc
		out["next_action"] = "To drill into one concept, call tool_guide with topic_id set to a topic's id."
	}
	if len(schema.Guide) == 0 && len(schema.Topics) == 0 {
		out["note"] = "This ability has no deep documentation; its short description is all there is: " + schema.Description
	}
	return toolkit.Success(out)
}

// tableOfContents is the topic list without the bodies: just enough for the
// model to choose which one to open.
func tableOfContents(topics []tool.Topic) []map[string]string {
	toc := make([]map[string]string, 0, len(topics))
	for _, t := range topics {
		toc = append(toc, map[string]string{"id": t.ID, "title": t.Title})
	}
	return toc
}

// edges renders a topic's edges for the model: where each goes, what kind of
// link it is, and when to follow it.
func edges(list []tool.TopicEdge) []map[string]string {
	out := make([]map[string]string, 0, len(list))
	for _, e := range list {
		out = append(out, map[string]string{
			"topic_id": e.To,
			"type":     string(e.Type),
			"when":     e.When,
		})
	}
	return out
}

func join(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}
