// Package agentguide answers what one agent can actually do.
//
// The Gateway's prompt names its agents and says what each is for; it does not
// write out their tools, their knowledge or the services they reach. That is
// the same arithmetic that took the Gateway's own tool list out of the prompt:
// written as prose, an agent's abilities cost tokens on every turn of every
// conversation, whether anything is ever delegated or not, and they multiply by
// the number of agents. So the names are the map and this is how the territory
// is fetched, one agent at a time, exactly as tool_guide fetches a tool's depth
// and integrations fetches a service's.
//
// What it answers is resolved for the PERSON asking, never cached: an agent's
// tools are its own selection intersected with that person's grants, so two
// people asking about one agent are entitled to different answers.
package agentguide

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolguide"
	"flexie.io/sag/internal/tools/toolkit"
)

// Name is the tool's canonical name.
const Name = "agent_guide"

// Agent is one agent as the Gateway is allowed to read it.
type Agent struct {
	// Key is what the Gateway routes by, and what this tool takes.
	Key  string
	Name string
	// Instructions are the administrator's own text, whole. The prompt carries
	// it too, because the Gateway routes on it and must not have to fetch
	// anything to decide where a task goes; here it is repeated so one call
	// answers everything about an agent rather than half of it.
	Instructions string
	Abilities    []Ability
	Knowledge    []Knowledge
	// Memory is the name of the brain the agent manages as its own memory,
	// empty when it has none.
	Memory string
}

// Ability is one of an agent's tools.
//
// It carries the callable Key as well as the friendly Name, and that is a
// deliberate reversal of what the prompt roster used to do. The roster gave the
// friendly name only, because a key read to the Gateway as a tool IT could call
// and it hallucinated the call. A key is given here because it is the handle
// tool_guide takes, and the hallucination is answered where it belongs: this
// tool's answer says plainly that these are not the Gateway's to call.
type Ability struct {
	Key         string
	Name        string
	Description string
	// Group is where the ability comes from: "Built-in", "Custom", or the name
	// of the connected service that projects it. Same headings an administrator
	// sees when they choose an agent's tools, so the two do not disagree.
	Group string
}

// Knowledge is one knowledge base an agent can reach.
type Knowledge struct {
	Name     string
	ReadOnly bool
}

// Roster is the agents this turn may delegate to. Bound per turn, for the same
// reason every other rebinding is: what a person may reach is decided per turn,
// and a handler built at boot would answer from a workspace it has never seen.
type Roster func() []Agent

// New builds the tool over this turn's roster.
func New(roster Roster) tool.Tool {
	return tool.Tool{
		Schema: Schema(),
		Handle: func(_ context.Context, call tool.Call) (tool.Result, error) {
			var args struct {
				Agent string `json:"agent"`
			}
			_ = json.Unmarshal(call.Args, &args)
			key := strings.TrimSpace(args.Agent)

			var agents []Agent
			if roster != nil {
				agents = roster()
			}
			if key == "" {
				return list(agents)
			}
			for _, a := range agents {
				if strings.EqualFold(a.Key, key) {
					return describe(a)
				}
			}
			// An answer, not a fault. Same reasoning as tool_guide: this is an
			// internal lookup, the question was "is there an agent by this
			// key", and the answer carries the keys there are. A miss drawn as
			// a failure puts a red row in somebody's conversation for a lookup
			// that behaved exactly as designed, and list() above has always
			// answered a call with no key rather than refusing it.
			return toolkit.Success(map[string]any{
				"found":       false,
				"message":     "There is no agent by that key.",
				"agents":      keys(agents),
				"next_action": "Name one of those in `agent` to see what it can do.",
			})
		},
	}
}

// Schema is what the model is told: that its agents can be looked up, and how.
func Schema() tool.Schema {
	input, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"agent": map[string]any{
				"type":        "string",
				"description": "The key of the agent to look up, as your instructions list it. Leave it out to list the agents again.",
			},
		},
		"required": []string{},
	})
	return tool.Schema{
		Name:         Name,
		FriendlyName: "Look up what an agent can do",
		About: "Lets the assistant read what one of its agents can actually reach, its tools, its knowledge " +
			"and the services connected to it, when it is deciding where to send a task, rather than carrying " +
			"every agent's abilities in every conversation. It reads only.",
		Description: "Look up one of your agents in depth: what it is for, the tools it holds, the knowledge " +
			"it can consult and the connected services it reaches. Your instructions name your agents; this is " +
			"how you find out what one can actually do before delegating to it. An ability listed here belongs " +
			"to that agent and cannot be called by you: use " + toolguide.Name + " on its key to read how it " +
			"works, and delegate the task to the agent to have it used.",
		InputSchema: input,
		// Internal: rides along wherever there are agents, invisible to the
		// admin UI. Delegation is a property of the Gateway, not a capability
		// an administrator switches on, and so is reading what it delegates to.
		Kind: tool.KindInternal,
		Risk: tool.RiskReadOnly,
	}
}

// list names the agents and nothing more, which is what the prompt already
// carries: it is here so a call with no key is answered rather than refused.
func list(agents []Agent) (tool.Result, error) {
	named := make([]map[string]string, 0, len(agents))
	for _, a := range agents {
		named = append(named, map[string]string{"key": a.Key, "name": a.Name})
	}
	return toolkit.Success(map[string]any{
		"agents":      named,
		"next_action": "Name one in `agent` to see what it can do.",
	})
}

// describe is one agent, whole: what it is for, what it can reach, and what it
// knows.
func describe(a Agent) (tool.Result, error) {
	out := map[string]any{
		"agent": a.Key,
		"name":  a.Name,
	}
	if a.Instructions != "" {
		out["instructions"] = a.Instructions
	}
	if len(a.Abilities) > 0 {
		out["abilities"] = grouped(a.Abilities)
		// Said on the answer that carries the keys, because this is the answer
		// that could be misread as an offer.
		out["note"] = "These are " + a.Key + "'s abilities, not yours: you cannot call any of them. " +
			"Delegate the task to " + a.Key + " to have them used, and read one with " + toolguide.Name +
			" if you need to know exactly what it does first."
	} else {
		out["abilities"] = []any{}
		out["note"] = a.Key + " holds no tools: it answers from the model's own knowledge and its instructions."
	}
	if len(a.Knowledge) > 0 {
		bases := make([]map[string]any, 0, len(a.Knowledge))
		for _, k := range a.Knowledge {
			bases = append(bases, map[string]any{"name": k.Name, "read_only": k.ReadOnly})
		}
		out["knowledge"] = bases
	}
	if a.Memory != "" {
		out["memory"] = a.Memory
	}
	return toolkit.Success(out)
}

// grouped sorts the abilities under the heading each came from, so a connected
// service's tools read as that service's rather than as a flat list where
// nothing says which of two "search" tools belongs where.
func grouped(abilities []Ability) []map[string]any {
	order := make([]string, 0, 4)
	byGroup := make(map[string][]map[string]string, 4)
	for _, ab := range abilities {
		group := ab.Group
		if group == "" {
			group = "Built-in"
		}
		if _, seen := byGroup[group]; !seen {
			order = append(order, group)
		}
		byGroup[group] = append(byGroup[group], map[string]string{
			"name": ab.Name, "key": ab.Key, "description": ab.Description,
		})
	}
	sort.Strings(order)
	out := make([]map[string]any, 0, len(order))
	for _, group := range order {
		out = append(out, map[string]any{"from": group, "tools": byGroup[group]})
	}
	return out
}

func keys(agents []Agent) []string {
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		out = append(out, a.Key)
	}
	return out
}
