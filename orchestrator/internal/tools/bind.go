package tools

import (
	"sort"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/brain"
	"flexie.io/sag/internal/tools/integrations"
	"flexie.io/sag/internal/tools/machine"
	"flexie.io/sag/internal/tools/skills"
	"flexie.io/sag/internal/tools/toolguide"
)

// BindToolGuide points tool_guide at the abilities THIS turn has.
//
// On its own it reads the process-wide registry, which holds only the tools the
// code ships. A tool an administrator created, or one projected from a remote
// server, is bound into the turn's loadout and never appears there: its guide
// and its topics exist, and asking for them answers that no such ability exists.
// Binding it here also stops it describing abilities the assistant was not
// granted, since the loadout is already exactly what it may use.
// AgentTool is one tool an agent holds, and which agent holds it. The Gateway
// can read the guide of a tool it does not have, because it decides where a
// task goes and cannot decide that blind; what it must never be able to do is
// mistake reading for holding, so every one of these carries its owner.
type AgentTool struct {
	Schema tool.Schema
	Agent  string
}

func BindToolGuide(loadout tool.Loadout, agentTools ...AgentTool) {
	if _, ok := loadout.Handlers[toolguide.Name]; ok {
		// Both lists: a tool held on demand has a guide like any other, and the
		// model reaches it by name once it has discovered the name.
		own := append(append([]tool.Schema{}, loadout.Schemas...), loadout.OnDemand...)
		loadout.Handlers[toolguide.Name] = toolguide.Handler(turnTools{own: own, agents: agentTools})
	}
}

// BindIntegrations points the connected-services ability at what THIS turn
// holds without offering: which services they came from, and their tools.
//
// Bound per turn for the reason every other rebinding is: what is connected and
// what this person is granted are decided per turn, and a handler built at boot
// would answer from a workspace it has never seen.
func BindIntegrations(loadout tool.Loadout, services []integrations.Service) {
	if _, ok := loadout.Handlers[integrations.Name]; !ok {
		return
	}
	held := loadout.OnDemand
	loadout.Handlers[integrations.Name] = integrations.New(
		func() []tool.Schema { return held },
		func() []integrations.Service { return services },
	).Handle
}

// turnTools answers tool_guide's questions from one turn's schemas, plus the
// tools this turn's agents hold.
//
// Own tools are searched first, so a name the caller holds is never reported as
// somebody else's: the Gateway and one of its agents can be granted the same
// tool, and the caller's own is the true answer.
type turnTools struct {
	own    []tool.Schema
	agents []AgentTool
}

func (t turnTools) Lookup(name string) (tool.Schema, bool) {
	for _, schema := range t.own {
		if schema.Name == name {
			return schema, true
		}
	}
	for _, held := range t.agents {
		if held.Schema.Name == name {
			return held.Schema, true
		}
	}
	return tool.Schema{}, false
}

func (t turnTools) LookupTopic(id string) (tool.Topic, string, bool) {
	for _, schema := range t.own {
		for _, topic := range schema.Topics {
			if topic.ID == id {
				return topic, schema.Name, true
			}
		}
	}
	for _, held := range t.agents {
		for _, topic := range held.Schema.Topics {
			if topic.ID == id {
				return topic, held.Schema.Name, true
			}
		}
	}
	return tool.Topic{}, "", false
}

// Owner names the agent holding a tool the caller does not have.
func (t turnTools) Owner(name string) string {
	for _, schema := range t.own {
		if schema.Name == name {
			return ""
		}
	}
	for _, held := range t.agents {
		if held.Schema.Name == name {
			return held.Agent
		}
	}
	return ""
}

// Guided lists the caller's OWN documented tools and deliberately not its
// agents'.
//
// This list is the answer to "what can I look up", and it is read as an offer:
// an agent's tool in it would be a name the Gateway found in its own inventory
// with nothing saying otherwise, which is the shortest path to a hallucinated
// call. An agent's abilities are advertised by agent_guide, which says whose
// they are in the same breath, and once the Gateway has a key from there this
// registry resolves it.
func (t turnTools) Guided() []string {
	var names []string
	for _, schema := range t.own {
		if len(schema.Guide) > 0 || len(schema.Topics) > 0 {
			names = append(names, schema.Name)
		}
	}
	sort.Strings(names)
	return names
}

// BindBrains scopes the brain tools to the agent's own brains for this turn.
//
// The brain tools are registered with an empty allow-list, so on their own they
// reach nothing. Here their handlers (and the write tool's pre-park validator)
// are rebound over exactly the brains the agent was assigned: the read tool over
// its knowledge bases, the write tool over the ones it may change. A tool the
// agent was not granted is absent from the loadout, so it is never rebound and
// stays absent; only what survived the grant check is scoped. This runs for the
// Gateway and every agent alike, because both resolve tools through Loadout.
// `by` is who this turn's writes are recorded as. It is bound here with the
// brains for the same reason they are: both are facts about THIS turn, and a
// handler that had to work out who was calling it would be guessing.
func BindBrains(loadout tool.Loadout, st store.Store, brains []int64, by model.Actor) {
	if _, ok := loadout.Handlers[brain.ReadName]; ok {
		loadout.Handlers[brain.ReadName] = brain.ReadHandler(st.Brains(), brains)
	}
	if _, ok := loadout.Handlers[brain.WriteName]; ok {
		loadout.Handlers[brain.WriteName] = brain.WriteHandler(st.Brains(), brains, by)
		if loadout.Validators == nil {
			loadout.Validators = map[string]tool.Validator{}
		}
		loadout.Validators[brain.WriteName] = brain.WriteValidator(st.Brains(), brains)
	}
}

// AppendSkills adds the two skill tools to the loadout, scoped to the skills
// this agent was assigned.
//
// APPENDED rather than registered-and-rebound, which is the difference from the
// brain tools, and the reason is the same one agent_guide has: a tool whose only
// possible answer is "you hold none" should not be there at all. An agent with
// no skills carries neither, so the model never learns of an ability it cannot
// use, and a workspace that imports its first skill does not change what every
// other agent is told.
//
// Internal infrastructure: not granted, not approval-gated, and never in the
// admin catalogue. Reading a skill an administrator already assigned is not a
// second capability to switch on. They read only, so there is nothing to
// confirm.
func AppendSkills(loadout *tool.Loadout, lib skills.Library, machines machine.Machines, on Computer, assigned []int64) {
	if len(assigned) == 0 {
		return
	}
	if loadout.Handlers == nil {
		loadout.Handlers = map[string]tool.Handler{}
	}
	offered := []tool.Tool{
		skills.NewSearch(lib, assigned),
		skills.NewLoad(lib, assigned),
	}
	// Running a script is offered only where there is a computer that can run
	// one, which is the rule every machine tool follows (machine.Offers): the
	// model never learns of an ability it cannot use, so nothing fails in the
	// middle of a conversation and nobody is asked to update before using the
	// things that do work. A browser reaches no computer at all, and an
	// application a release behind has never heard of the call.
	if machine.Speaks(machines, on.WorkspaceID, on.UserID, on.DeviceID, machine.SkillRunName) {
		offered = append(offered, skills.NewExec(lib, machines, assigned))
	}
	for _, t := range offered {
		loadout.Schemas = append(loadout.Schemas, t.Schema)
		loadout.Handlers[t.Schema.Name] = t.Handle
	}
}

// Computer is which computer this turn may reach, which is three facts and not
// one: a device id means nothing without the person and the workspace it was
// minted under.
type Computer struct {
	WorkspaceID int64
	UserID      int64
	DeviceID    string
}

// AppendMemory adds the agent's own memory-brain tool to the loadout, when the
// agent has a memory brain. It is internal infrastructure: not granted, not
// approval-gated, and never advertised in the admin catalog. Present only when a
// memory brain is assigned, so an agent without one carries no memory tool. The
// Gateway and every agent wire it the same way.
func AppendMemory(loadout *tool.Loadout, st store.Store, memoryBrainID *int64, by model.Actor) {
	if memoryBrainID == nil {
		return
	}
	loadout.Schemas = append(loadout.Schemas, brain.MemorySchema())
	if loadout.Handlers == nil {
		loadout.Handlers = map[string]tool.Handler{}
	}
	loadout.Handlers[brain.MemoryName] = brain.MemoryHandler(st.Brains(), *memoryBrainID, by)
}
