package app

// The default agent, until workflows select one.
//
// This is the "default package" from the layered configuration model: a
// workspace works with no configuration at all, and a workflow later
// overrides the loop, the tools, and the model for a given group or channel.
// Everything here is a default, not a hardcoded rule.
//
// There is no default system-prompt string: the prompt is not a stored
// constant but is assembled every turn from what is true right now (see
// systemprompt.go). An administrator's own instructions extend that assembled
// base through the agent's Instructions field; they never replace it.

// DefaultTools is the tool set an agent gets with no configuration. The
// registry is the source of truth for what these names mean, and every call
// still passes the caller's own permission check.
//
// Memory is not among them: the assistant does not write what it remembers
// through a tool in the turn's path. It is distilled in the background, on a
// cadence and when the model flags a turn (see agent/memory.go), so rewriting
// memory never makes a person wait for their answer.
func (a *App) DefaultTools() []string {
	return []string{"current_time", "list_models", "set_model_status"}
}

// DefaultReasoning is off: thinking costs money and latency, and most turns
// do not need it. An agent turns it on deliberately.
const DefaultReasoning = false
