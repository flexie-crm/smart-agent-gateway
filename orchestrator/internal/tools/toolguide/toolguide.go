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
	// Owner names the agent a tool belongs to, and is empty for one the caller
	// holds itself.
	//
	// The Gateway is told the NAMES of its agents and looks up what they can do
	// (agent_guide), which hands back the callable key of every ability an
	// agent holds. Those keys are documented like any other, and refusing to
	// read them would mean the Gateway choosing where to send a task while
	// being unable to find out what the tool it is choosing actually does. So
	// they resolve, and every answer for one says whose it is: without that,
	// reading a tool's guide looks exactly like holding the tool.
	Owner(name string) string
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

// missed is what a lookup that found nothing answers with.
//
// found is stated rather than implied, because the rest of this tool answers
// with a body and a title: a payload carrying only prose could be read as
// documentation that happens to be short.
func missed(reg Registry, why string) (tool.Result, error) {
	return toolkit.Success(map[string]any{
		"found":            false,
		"message":          why,
		"guides_available": reg.Guided(),
	})
}

// openTopic returns one topic's body and the edges the model can follow next.
func openTopic(reg Registry, id string) (tool.Result, error) {
	topic, owner, ok := reg.LookupTopic(id)
	if !ok {
		// Not a failure. Nothing went wrong here: the question was "is there a
		// topic by this id", the answer is no, and the answer carries the list
		// the model needs to ask again. Classifying it as a failure wrote a red
		// row into somebody's conversation for a documentation lookup that
		// worked exactly as designed, and the person reading it cannot tell
		// that from a tool that broke.
		//
		// The branch below for neither argument given has always answered this
		// way. A miss is the same situation as an empty call, reached by a
		// different route, so it gets the same shape.
		return missed(reg, "There is no topic by that id. Call tool_guide with tool_name to see one ability's topics.")
	}
	out := map[string]any{
		"tool":        owner,
		"topic":       topic.ID,
		"title":       topic.Title,
		"body":        topic.Body,
		"related":     edges(topic.Edges),
		"next_action": "To follow a related topic, call tool_guide with topic_id set to its id.",
	}
	if agent := reg.Owner(owner); agent != "" {
		out["belongs_to"] = agent
		out["note"] = "This ability is " + agent + "'s, not yours: you cannot call it. " +
			"Delegate the task to " + agent + " to have it used."
	}
	return toolkit.Success(out)
}

// openTool returns a tool's flat guide and the table of contents of its topics.
func openTool(reg Registry, name string) (tool.Result, error) {
	schema, ok := reg.Lookup(name)
	if !ok {
		// Same reasoning as an unknown topic id: a name that is not here is an
		// answer, not a fault.
		return missed(reg, "There is no ability by that name.")
	}
	out := map[string]any{"tool": name}
	if owner := reg.Owner(name); owner != "" {
		out["belongs_to"] = owner
		out["note"] = "This ability is " + owner + "'s, not yours: you cannot call it. " +
			"Delegate the task to " + owner + " to have it used."
	}
	if len(schema.Guide) > 0 {
		out["guide"] = schema.Guide
	}
	if toc := tableOfContents(schema.Topics); len(toc) > 0 {
		out["topics"] = toc
		out["next_action"] = "To drill into one concept, call tool_guide with topic_id set to a topic's id."
	}
	// A tool a connected service projected is NOT ours to document, and saying
	// so is the whole of this branch.
	//
	// This guide covers the abilities we ship. A projected tool's description
	// comes from the service, and whether there is anything deeper to read is
	// the service's to answer: MCP carries no standard for documentation beyond
	// that description, so some vendors add a tool of their own for it (Flexie
	// projects one, as nli_tool_guide) and most do not.
	//
	// What was here before answered "this ability has no deep documentation;
	// its short description is all there is", which we cannot know and which
	// was false in the case that produced this: the model was told there was
	// nothing deeper about nli_query while the service's own guide had it, went
	// looking for a topic anyway, and invented an id (query/leads-and-contacts)
	// that failed. Asserting the absence of something only a third party can
	// report is worse than declining to answer.
	//
	// RemoteName is here because it is the name the service's own tools expect:
	// our prefix is ours, and a model that knows only "nli_query" would ask NLI
	// about a tool it has never heard of.
	if schema.Service != "" {
		out["service"] = schema.Service
		out["ours"] = false
		out["does"] = schema.Description
		if schema.RemoteName != "" {
			out["name_on_service"] = schema.RemoteName
		}
		out["documentation"] = documentationIsTheServices(schema)
		return toolkit.Success(out)
	}
	if len(schema.Guide) == 0 && len(schema.Topics) == 0 {
		out["note"] = "This ability has no deep documentation; its short description is all there is: " + schema.Description
	}
	return toolkit.Success(out)
}

// documentationIsTheServices says where a projected tool's documentation lives,
// in the only terms that are true: with the service.
//
// It routes rather than guesses. We do not look for a tool whose name suggests
// it is a guide, because that would be a convention we invented on a third
// party's behalf; integrations lists what the service actually offers and the
// model can see a documentation tool there if one exists.
func documentationIsTheServices(schema tool.Schema) string {
	where := "This is " + schema.Service + "'s tool, not one of ours, and this guide documents our own abilities. " +
		"Its description above is what " + schema.Service + " says about it, and anything deeper is " +
		schema.Service + "'s to give: call integrations with operation \"tools\" and service \"" + schema.Service +
		"\" to see everything it offers, including a documentation tool of its own if it has one."
	if schema.RemoteName != "" {
		where += " Ask any of those using the name " + schema.Service + " uses, which for this tool is \"" +
			schema.RemoteName + "\": the prefix on \"" + schema.Name + "\" is ours and " + schema.Service +
			" has never heard of it."
	}
	return where
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
