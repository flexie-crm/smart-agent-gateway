// Package integrations is how an agent reaches a connected service without
// carrying it.
//
// A service connected to a workspace projects its tools into this one registry
// (KB/20), and each of them costs tokens on every turn merely by being
// described to the model. Measured on a real installation, three of them were
// 23,462 bytes of a 45,748-byte tool list, sent on every turn of every
// conversation whether or not that service was ever touched, and paid for each
// time.
//
// So they are held rather than offered. The prompt names the services, this
// lists what one can do, opens one tool in full, and calls it. It is the same
// map-and-drilldown rule the abilities (tool_guide) and the knowledge bases
// (brain) already run on.
//
// What this is NOT is a second way to run a tool. A call made here is resolved
// by the loop into a call to the tool itself, so the approval card, the
// definition-hash drift check, the transcript row and the loop's learning all
// happen exactly as they do for a tool the model was handed directly. A
// dispatcher that ran the handler itself would quietly lose all four.
package integrations

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolkit"
)

// Name is the tool's canonical name.
const Name = "integrations"

// CallOperation is the operation the LOOP resolves rather than this handler:
// naming it here keeps the two halves of that agreement in one place.
const CallOperation = "call"

// Held is what this turn can run without having offered. A turn with no
// connected service has none, and the tool then says so rather than pretending.
type Held func() []tool.Schema

// Services names the connected services, so a tool can be attributed to one and
// a service can be listed even before any of its tools is opened.
type Services func() []Service

// Service is one connected service, as a person configured it.
type Service struct {
	// ID is the connection's own id, which is a handle that cannot be ambiguous
	// and does not change when somebody renames the connection.
	ID int64
	// Name is what it is called in this workspace.
	Name string
	// Alias is the prefix every tool it projects carries, which is what makes a
	// name here unambiguous when two services offer a "search".
	Alias string
}

// New builds the tool over what this turn holds. Both funcs are bound per turn,
// because what is connected and what is granted are decided per turn.
func New(held Held, services Services) tool.Tool {
	return tool.Tool{
		Schema: Schema(),
		Handle: func(_ context.Context, call tool.Call) (tool.Result, error) {
			var args struct {
				Operation string `json:"operation"`
				Service   string `json:"service"`
				Tool      string `json:"tool"`
			}
			_ = json.Unmarshal(call.Args, &args)

			switch strings.TrimSpace(strings.ToLower(args.Operation)) {
			case "", "services":
				return listServices(services, held)
			case "tools":
				return listTools(held, args.Service)
			case "describe":
				return describe(held, args.Tool)
			case CallOperation:
				// The loop resolves this before a handler is ever reached. Here
				// means it did not, which is a wiring fault rather than a
				// mistake the model can correct, so it is said plainly.
				return toolkit.Failed("this ability cannot run a tool by itself")
			default:
				return toolkit.BadArguments(
					"operation must be \"services\", \"tools\", \"describe\" or \"call\"")
			}
		},
	}
}

// Schema is what the model is told: enough to know these exist and how to get
// at one, and nothing about any particular service.
func Schema() tool.Schema {
	input, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"operation": map[string]any{
				"type": "string",
				"enum": []string{"services", "tools", "describe", CallOperation},
				"description": "services: the connected services and what each is called. " +
					"tools: everything one service can do, by name, one line each. " +
					"describe: one tool in full, with its exact arguments. " +
					"call: run one.",
			},
			"service": map[string]any{
				"type":        "string",
				"description": "For tools: which service, by name or alias.",
			},
			"tool": map[string]any{
				"type":        "string",
				"description": "For describe and call: the exact tool name, as tools listed it.",
			},
			"arguments": map[string]any{
				"type":        "object",
				"description": "For call: the arguments for that tool, exactly as describe gave them.",
			},
		},
		"required": []string{"operation"},
	})
	return tool.Schema{
		Name:         Name,
		FriendlyName: "Connected services",
		About: "Lets the agent reach the services connected to this workspace without carrying all of " +
			"their tools in every conversation: it looks up what a service can do when it needs it, and " +
			"runs one. What each service allows is unchanged, and every action it takes is still " +
			"granted and confirmed exactly as it would be otherwise.",
		Description: "Reach a service connected to this workspace. Their tools are NOT listed with " +
			"your abilities, because there can be many: services lists what is connected, tools lists " +
			"one service's tools, describe gives a tool's exact arguments, call runs it. Describe " +
			"before calling rather than guessing arguments.",
		InputSchema: input,
		Kind:        tool.KindInternal,
		// The reading operations are read-only; a call is resolved into the real
		// tool, and carries THAT tool's risk and approval, never this one's.
		Risk: tool.RiskReadOnly,
	}
}

func listServices(services Services, held Held) (tool.Result, error) {
	var known []Service
	if services != nil {
		known = services()
	}
	if len(known) == 0 {
		return toolkit.Success(map[string]any{
			"services": []any{},
			"note":     "no services are connected to this workspace",
		})
	}
	counts := map[string]int{}
	for _, s := range heldSchemas(held) {
		counts[serviceOf(s)]++
	}
	out := make([]map[string]any, 0, len(known))
	for _, s := range known {
		out = append(out, map[string]any{
			"id":      s.ID,
			"service": s.Name,
			"alias":   s.Alias,
			"tools":   counts[s.Name],
		})
	}
	return toolkit.Success(map[string]any{
		"services": out,
		"next":     "call this again with operation \"tools\" and a service to see what it can do",
	})
}

func listTools(held Held, service string) (tool.Result, error) {
	service = strings.TrimSpace(service)
	if service == "" {
		return toolkit.BadArguments("say which service, by name or alias")
	}
	var out []map[string]any
	for _, s := range heldSchemas(held) {
		if !belongsTo(s, service) {
			continue
		}
		out = append(out, map[string]any{
			"tool":    s.Name,
			"does":    firstSentence(s.Description),
			"service": serviceOf(s),
		})
	}
	if len(out) == 0 {
		// An answer, not a fault, and the same rule tool_guide follows: a name
		// that is not here is the question answered, and the answer carries
		// what IS here.
		return nothingBy(held, fmt.Sprintf(
			"No connected service called %q is available to you.", service))
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["tool"].(string) < out[j]["tool"].(string) })
	return toolkit.Success(map[string]any{
		"tools": out,
		"next":  "call this again with operation \"describe\" and a tool for its exact arguments",
	})
}

func describe(held Held, name string) (tool.Result, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return toolkit.BadArguments("say which tool, by its exact name")
	}
	for _, s := range heldSchemas(held) {
		if s.Name != name {
			continue
		}
		return toolkit.Success(map[string]any{
			"tool":      s.Name,
			"service":   serviceOf(s),
			"does":      s.Description,
			"arguments": s.InputSchema,
			"next":      "call this again with operation \"call\", this tool, and those arguments",
		})
	}
	// This is the one drawing red rows in real conversations. Discovery is the
	// ONLY way in to a service's tools, so a model hunting for one guessed at
	// names (nli_search, nli_report, nli_docs, nli_tool_guide) and every guess
	// reported a broken tool, though nothing had broken and the reply it needed
	// was the list of what it can actually reach.
	//
	// And note what "available to you" means, because it is the reason the list
	// is the whole answer: a service can project thirty tools and an
	// administrator allow the agent two, so a tool existing in the workspace is
	// not a tool this turn can call. Naming the reachable ones is the only
	// useful reply, and it is also the honest one.
	return nothingBy(held, fmt.Sprintf("No tool called %q is available to you.", name))
}

// nothingBy is what a lookup that found nothing answers with: the miss, and
// every tool this turn can actually reach, by its exact callable name.
//
// Exact, prefix and all, because that is the name a call takes. The names are
// the whole point: they are what turns a guess into a correction on the next
// step instead of another guess.
func nothingBy(held Held, why string) (tool.Result, error) {
	schemas := heldSchemas(held)
	names := make([]string, 0, len(schemas))
	for _, s := range schemas {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return toolkit.Success(map[string]any{
		"found":     false,
		"message":   why,
		"available": names,
		"next":      "call this with operation \"describe\" and one of those exact names, and never a name that is not in that list",
	})
}

func heldSchemas(held Held) []tool.Schema {
	if held == nil {
		return nil
	}
	return held()
}

// serviceOf is which service a tool came from, and is empty for one of ours.
// Read off the schema, which carries it from the projection; it used to be cut
// back out of the approval title, in this package and in app independently.
func serviceOf(s tool.Schema) string {
	return s.Service
}

// belongsTo matches a service by its name or by the alias its tools carry, so
// either of the two things a person sees works as a lookup.
func belongsTo(s tool.Schema, service string) bool {
	if strings.EqualFold(serviceOf(s), service) {
		return true
	}
	alias := strings.TrimSpace(service)
	return alias != "" && strings.HasPrefix(strings.ToLower(s.Name), strings.ToLower(alias)+"_")
}

// firstSentence is the one line a listing shows. A projected description is a
// third party's and can be a page of it; the whole thing is one describe away.
func firstSentence(description string) string {
	description = strings.TrimSpace(description)
	if i := strings.IndexAny(description, ".\n"); i > 0 {
		return strings.TrimSpace(description[:i+1])
	}
	const most = 160
	if len(description) > most {
		return strings.TrimSpace(description[:most]) + "…"
	}
	return description
}
