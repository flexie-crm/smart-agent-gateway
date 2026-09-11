package provider

import (
	"strings"
	"sync"
)

// A setting a model does not have must cost nothing.
//
// How hard to think is chosen on the AGENT, which is the right place: it is a
// property of the job, and one model serves a classifier that should answer at
// once and a researcher that should not. But an agent is not re-pointed at a new
// model every time somebody changes one, and no vendor publishes which of its
// models reason. So the setting will be sent to a model that has no such idea,
// and on a strict wire that is not ignored, it is a rejected request: the turn
// fails, the person sees nothing, and the cause is a parameter they never typed.
//
// The alternative was to record the capability per model and make an
// administrator keep it true. That is data entry that is wrong the week a model
// ships, and this codebase already refuses the neighbouring version of it:
// "a table of them is a table that is always wrong" (responses.go).
//
// So the vendor is allowed to answer the question. Asked once per model, the
// refusal is read, the parameter is dropped, and the request goes again without
// it. Nothing above the adapter learns that any of this happened, which is what
// "it is ignored by a model that cannot do it" has to mean if we are going to
// print it under a checkbox.

// reasoningRefusals remembers, per model, that this vendor will not take the
// reasoning parameter for it.
//
// In memory and not in a column, deliberately. A column is a fact somebody has
// to maintain and can be wrong; this is a fact the vendor stated, and forgetting
// it on restart costs exactly one extra round trip on the next turn. It also
// heals itself: a model that gains reasoning next quarter is retried the first
// time this process is restarted, where a stored `false` would be wrong for ever.
var reasoningRefusals sync.Map // model key -> struct{}

// refusesReasoning reports whether this model has already told us so.
func refusesReasoning(modelKey string) bool {
	_, known := reasoningRefusals.Load(modelKey)
	return known
}

// rememberReasoningRefusal records it, so the next turn does not pay for the
// same discovery.
func rememberReasoningRefusal(modelKey string) {
	reasoningRefusals.Store(modelKey, struct{}{})
}

// isReasoningRefusal reads a vendor's error and says whether it is a complaint
// about the reasoning parameter rather than a real failure.
//
// Matched on the TEXT, which is not a thing to do lightly. There is no code for
// it: OpenAI returns invalid_request_error for a malformed tool call, an
// unknown model and this alike, and Anthropic the same, so the status and the
// type say only that we asked for something wrong. The parameter's own name is
// the one durable part, and both vendors name it, because an error that did not
// would be useless to the person reading it too.
//
// Deliberately narrow. A phrase that matched too much would swallow a genuine
// mistake and retry it silently, which is worse than the original problem: the
// retry drops reasoning, the model answers without thinking, and nobody is ever
// told the request was wrong. So it must name a reasoning parameter AND read as
// unsupported; anything else is returned to the caller untouched.
func isReasoningRefusal(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())

	named := strings.Contains(text, "reasoning") ||
		strings.Contains(text, "thinking") ||
		strings.Contains(text, "budget_tokens")
	if !named {
		return false
	}
	for _, complaint := range []string{
		"unsupported",
		"not supported",
		"unknown parameter",
		"unrecognized",
		"unexpected",
		"does not support",
		"is not permitted",
		"extra inputs are not permitted",
	} {
		if strings.Contains(text, complaint) {
			return true
		}
	}
	return false
}

// withoutReasoning is the same request with the parameter the vendor refused
// taken out, so the retry asks for the answer and nothing else.
func withoutReasoning(req GenerateRequest) GenerateRequest {
	plain := req
	plain.Reasoning = false
	return plain
}
