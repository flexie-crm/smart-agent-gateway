package agent

import (
	"flexie.io/sag/internal/provider"
)

// measureContext tells the turn's listener how much of its model's window the
// conversation takes, counted with the one measure the trim uses
// (provider.RequestChars), so the number shown and the trim cannot disagree.
//
// messages is the whole conversation as it stands, before any trim: how full it
// is, not how much of it survived. The base is the system prompt and the tools,
// the part that would still be sent if the conversation were empty, which is
// what a compaction adds its summary to.
func measureContext(turn Turn, resolved *provider.Resolved, messages []provider.Message, tools []provider.ToolDef) {
	if turn.ContextUsed == nil || resolved == nil || resolved.Model == nil {
		return
	}
	var base []provider.Message
	if turn.SystemPrompt != "" {
		base = []provider.Message{{Role: provider.RoleSystem, Content: turn.SystemPrompt}}
	}
	turn.ContextUsed(resolved.Model, provider.RequestChars(messages, tools), provider.RequestChars(base, tools))
}

// sentChars is the characters a request carried, in the measure the trim uses,
// or zero when it carried files: a picture or a document is sent as bytes that
// the vendor counts in its own way, so its characters say nothing about how big
// the model's tokens are and would only teach it a wrong rate.
func sentChars(req provider.GenerateRequest) int {
	for _, m := range req.Messages {
		if len(m.Files) > 0 {
			return 0
		}
	}
	return provider.RequestChars(req.Messages, req.Tools)
}

// SummaryChars is what a conversation's summary adds to a request, in the same
// measure: the message it is sent as, heading and all. With the base of the
// last measurement it is how full a conversation is the moment it is compacted.
func SummaryChars(summary string) int {
	return provider.RequestChars(withSummary(nil, summary), nil)
}
