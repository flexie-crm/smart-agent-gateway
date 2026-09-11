package tools

import (
	"sort"

	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/brain"
	"flexie.io/sag/internal/tools/integrations"
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
func BindToolGuide(loadout tool.Loadout) {
	if _, ok := loadout.Handlers[toolguide.Name]; ok {
		// Both lists: a tool held on demand has a guide like any other, and the
		// model reaches it by name once it has discovered the name.
		guided := append(append([]tool.Schema{}, loadout.Schemas...), loadout.OnDemand...)
		loadout.Handlers[toolguide.Name] = toolguide.Handler(turnTools(guided))
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

// turnTools answers tool_guide's questions from one turn's schemas.
type turnTools []tool.Schema

func (t turnTools) Lookup(name string) (tool.Schema, bool) {
	for _, schema := range t {
		if schema.Name == name {
			return schema, true
		}
	}
	return tool.Schema{}, false
}

func (t turnTools) LookupTopic(id string) (tool.Topic, string, bool) {
	for _, schema := range t {
		for _, topic := range schema.Topics {
			if topic.ID == id {
				return topic, schema.Name, true
			}
		}
	}
	return tool.Topic{}, "", false
}

func (t turnTools) Guided() []string {
	var names []string
	for _, schema := range t {
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
func BindBrains(loadout tool.Loadout, st store.Store, brains []int64) {
	if _, ok := loadout.Handlers[brain.ReadName]; ok {
		loadout.Handlers[brain.ReadName] = brain.ReadHandler(st.Brains(), brains)
	}
	if _, ok := loadout.Handlers[brain.WriteName]; ok {
		loadout.Handlers[brain.WriteName] = brain.WriteHandler(st.Brains(), brains)
		if loadout.Validators == nil {
			loadout.Validators = map[string]tool.Validator{}
		}
		loadout.Validators[brain.WriteName] = brain.WriteValidator(st.Brains(), brains)
	}
}

// AppendMemory adds the agent's own memory-brain tool to the loadout, when the
// agent has a memory brain. It is internal infrastructure: not granted, not
// approval-gated, and never advertised in the admin catalog. Present only when a
// memory brain is assigned, so an agent without one carries no memory tool. The
// Gateway and every agent wire it the same way.
func AppendMemory(loadout *tool.Loadout, st store.Store, memoryBrainID *int64) {
	if memoryBrainID == nil {
		return
	}
	loadout.Schemas = append(loadout.Schemas, brain.MemorySchema())
	if loadout.Handlers == nil {
		loadout.Handlers = map[string]tool.Handler{}
	}
	loadout.Handlers[brain.MemoryName] = brain.MemoryHandler(st.Brains(), *memoryBrainID)
}
