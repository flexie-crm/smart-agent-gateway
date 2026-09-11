package provider

import "flexie.io/sag/internal/model"

// Most vendors speak the OpenAI chat-completions wire format, so they share
// one adapter. Their differences are expressed as data here, never as
// branching inside the adapter: adding a vendor should be a Dialect entry,
// not an `if vendor == ...` in shared code.
type Dialect struct {
	// Key is the vendor key stored on the vendor row.
	Key string
	// Wire is which OpenAI-shaped protocol this vendor actually speaks.
	//
	// Chat completions is the zero value and is what almost everyone here
	// speaks, because almost everyone here COPIED it: DeepSeek, Z.ai, Mistral,
	// self-hosted servers and our own machines all implement that shape and
	// will go on doing so. OpenAI itself has moved on, and its current models
	// refuse function tools together with reasoning on the old endpoint, so the
	// first-party vendor speaks its successor.
	//
	// Per VENDOR, and that is the whole point of putting it here. There are
	// more models every month and a table of which model needs which wire would
	// be wrong within a release and wrong silently. A vendor speaks one
	// protocol; that is a fact about the vendor and it lives with the rest of
	// them.
	Wire WireFormat
	// DefaultBaseURL is used when the vendor row does not carry one. Empty
	// means the endpoint is mandatory (self-hosted servers, Azure).
	DefaultBaseURL string
	// Reasoning says how this vendor exposes model thinking.
	Reasoning ReasoningMode
	// ThinkingToggle, when set, is a request body field that switches
	// reasoning on AND off. It is sent on every call, never omitted:
	// DeepSeek and Z.ai default thinking ON, so leaving the field out
	// would silently bill an agent for reasoning it did not ask for. Off
	// has to be said out loud.
	ThinkingToggle *ThinkingToggle
	// Azure needs its own auth header and an api-version query parameter,
	// which the SDK's azure helper applies.
	IsAzure bool
	// AzureAPIVersion is the api-version Azure requires on every call.
	AzureAPIVersion string
	// MarkupToolCalls says this vendor sometimes emits tool calls as inline
	// markup in the content stream instead of through the function-calling
	// channel. DeepSeek does this when it decides to "talk" a tool call rather
	// than return it structured: the call arrives as a block of its own markup
	// (see markupToolCalls). Left as text it would leak into the chat AND never
	// reach the tool loop, so the adapter salvages it into a real tool call.
	MarkupToolCalls bool
	// MaxCompletionTokens says this vendor caps output length with the
	// `max_completion_tokens` request field, not `max_tokens`. OpenAI deprecated
	// `max_tokens` and its reasoning models (the o-series, gpt-5) REJECT it with
	// a 400, so the first-party OpenAI and Azure dialects send
	// `max_completion_tokens`; every other vendor on this adapter (DeepSeek,
	// Z.ai, Mistral, self-hosted OpenAI-compatible servers) still speaks the
	// older `max_tokens`. This is chosen ONCE per vendor, the way every other
	// difference here is: never an `if model == "o3"`, which would not scale as a
	// vendor turns its model line over.
	MaxCompletionTokens bool
	// Settings is what this vendor's models may be configured with.
	//
	// DECLARED HERE, chosen on the row. Vendors keep inventing attributes that
	// are neither standard nor optional, and a column for each would be a
	// migration per vendor whim. A settings bag holds the chosen values; this
	// says which keys are legal, what they may be, and what they are when
	// nobody says. Anything not declared here is ignored wherever it is stored.
	//
	// Per VENDOR for the same reason Wire is: a table of which model takes what
	// would be wrong within a release and wrong silently.
	Settings []model.SettingSpec
}

// WireFormat is which protocol an OpenAI-shaped vendor speaks.
type WireFormat int

const (
	// WireChatCompletions is /v1/chat/completions, the shape the whole
	// ecosystem copied. The zero value, so a vendor that says nothing gets it.
	WireChatCompletions WireFormat = iota
	// WireResponses is /v1/responses, OpenAI's own successor. Reasoning and
	// function tools work together only here, and reasoning is only VISIBLE
	// here: the chat wire never returned it for this vendor at all.
	WireResponses
)

// ReasoningMode enumerates how a vendor returns model thinking. The gateway
// normalizes all of them into EventReasoningDelta, so the rest of the system
// never learns which vendor it is talking to.
type ReasoningMode int

const (
	// ReasoningNone: the vendor returns no thinking.
	ReasoningNone ReasoningMode = iota
	// ReasoningContentField: thinking arrives in a non-standard
	// `reasoning_content` field on the streamed delta (DeepSeek, and the
	// vendors that copied its shape).
	ReasoningContentField
)

// ThinkingToggle is a body field with a value for each state.
type ThinkingToggle struct {
	Field    string
	Enabled  any
	Disabled any
}

// thinkingTypeToggle is the shape DeepSeek and Z.ai share.
func thinkingTypeToggle() *ThinkingToggle {
	return &ThinkingToggle{
		Field:    "thinking",
		Enabled:  map[string]string{"type": "enabled"},
		Disabled: map[string]string{"type": "disabled"},
	}
}

// defaultAzureAPIVersion is a stable GA version. It is overridable per
// vendor row because Azure pins features to versions.
const defaultAzureAPIVersion = "2024-10-21"

// dialects is the registry of OpenAI-compatible vendors. Local model servers
// (vLLM, llama.cpp, LM Studio) use the generic entry with their own
// base URL: they need a row, not code.
// reasoningEffortSetting is how hard a model is asked to think.
//
// Declared rather than constant. It was `const reasoningEffort =
// ReasoningEffortMax`, applied to every call this vendor took, which meant a
// one-line classification thought as hard as a research question: slow,
// expensive, and nobody had measured whether the depth was worth it. Worse, the
// loop treats "has said nothing yet" as safe to retry and reasoning does not
// count as saying something, so maximum effort widened that window on every
// turn and made a latent bug in the retry path a common one.
//
// Medium by default, because a default should be the one that is right most of
// the time rather than the most expensive one, and anybody who wants more can
// say so per model or per agent.
var reasoningEffortSetting = model.SettingSpec{
	Key:   "reasoning_effort",
	Label: "Reasoning effort",
	Kind:  model.SettingChoice,
	Help:  "How hard the model is asked to think before it answers. More is slower and costs more. A model that has no such setting ignores it and answers as it always would.",
	Choices: []model.SettingChoiceOption{
		{Value: "low", Label: "Low, answer quickly"},
		{Value: "medium", Label: "Medium"},
		{Value: "high", Label: "High"},
		{Value: "max", Label: "As much as it will do"},
	},
	Default:           "medium",
	RequiresReasoning: true,
}

var dialects = map[string]Dialect{
	model.VendorOpenAI: {
		Key:            model.VendorOpenAI,
		DefaultBaseURL: "", // the SDK default
		// This vendor's own API, not the shape everyone copied from it. Its
		// current models will not take function tools and reasoning together on
		// the older endpoint, and every agent here has tools.
		Wire: WireResponses,
		// This wire is where reasoning is asked for and where it is visible, so
		// this is where the effort is chosen.
		Settings: []model.SettingSpec{reasoningEffortSetting},
		// The fields below belong to the chat wire and are inert while Wire is
		// WireResponses. They are kept because this entry is also what a flip
		// back would need, and because `MaxCompletionTokens` still describes
		// the truth about this vendor's chat endpoint.
		//
		// `Reasoning` is NOT set, and its absence is the correction: it used to
		// say ReasoningContentField, on the claim that "the o-series returns
		// thinking in the same non-standard field the others use". It does not.
		// `reasoning_content` is DeepSeek's, OpenAI has never sent it, and the
		// setting therefore never once fired. Every OpenAI model here has had
		// its thinking discarded in silence. On the Responses wire it arrives
		// as typed events and is shown.
		MaxCompletionTokens: true,
	},
	model.VendorDeepSeek: {
		Key:            model.VendorDeepSeek,
		DefaultBaseURL: "https://api.deepseek.com/v1",
		Reasoning:      ReasoningContentField,
		// Thinking defaults ON here, so "off" must be sent explicitly.
		ThinkingToggle: thinkingTypeToggle(),
		// DeepSeek occasionally emits tool calls as inline markup in the
		// content instead of through the function-calling channel.
		MarkupToolCalls: true,
	},
	model.VendorZAI: {
		Key:            model.VendorZAI,
		DefaultBaseURL: "https://api.z.ai/api/paas/v4",
		Reasoning:      ReasoningContentField,
		ThinkingToggle: thinkingTypeToggle(),
	},
	model.VendorMistral: {
		Key:            model.VendorMistral,
		DefaultBaseURL: "https://api.mistral.ai/v1",
	},
	model.VendorAzureOpenAI: {
		Key:             model.VendorAzureOpenAI,
		IsAzure:         true,
		AzureAPIVersion: defaultAzureAPIVersion,
		// Azure hosts the same first-party OpenAI models, o-series included, so
		// it shares the `max_completion_tokens` contract.
		MaxCompletionTokens: true,
	},
	model.VendorOpenAICompatible: {
		Key: model.VendorOpenAICompatible,
		// Thinking is READ from `reasoning_content`, the field every server in
		// this class puts it in: our own machines, vLLM, llama.cpp, LM Studio.
		// It was unset, so a model that thought had its thinking dropped on the
		// floor: the entry is the generic one, and generic was read as "assume
		// the least" when the honest default is "read what arrives".
		//
		// This changes only what is READ. What is SENT is driven by
		// ThinkingToggle, which is nil here, so no vendor is asked to think that
		// was not being asked before, and a server that sends no reasoning_content
		// yields an empty string and no event.
		Reasoning: ReasoningContentField,
	},
}

// DialectFor returns the wire dialect for a vendor key.
func DialectFor(vendorKey string) (Dialect, bool) {
	d, ok := dialects[vendorKey]
	return d, ok
}

// AgentSettings is what an agent may be configured with, whatever model it ends
// up pointed at.
//
// One list rather than the chosen vendor's, because the choice is the same four
// words wherever it exists and a vendor without it costs nothing: the adapter is
// refused once, drops the parameter and asks again (unsupported.go). A form that
// narrowed this would make the field appear and vanish as somebody picks a
// model, to guard against a failure that no longer happens.
func AgentSettings() []model.SettingSpec {
	return []model.SettingSpec{reasoningEffortSetting}
}

// SettingsForVendor is what an agent using this vendor's models may be
// configured with.
//
// It is the VENDOR's list and not a model's on purpose. A capability is a
// property of the model, nobody publishes it, and asking an administrator to
// record it per model is data entry that is wrong the moment a new model ships.
// So the choice is offered wherever the vendor has the concept, the form says
// plainly that a model without it will ignore the setting, and the adapter makes
// that true (see stripUnsupported): a vendor that refuses the parameter gets the
// request again without it, rather than the turn failing.
//
// One place to ask, because Anthropic is not in the dialect map: it has its own
// wire and its own adapter (gateway.go, buildProvider), so a caller that only
// read `dialects` would render an empty form for it and quietly offer nothing.
func SettingsForVendor(vendorKey string) []model.SettingSpec {
	if vendorKey == model.VendorAnthropic {
		return anthropicSettings
	}
	if d, ok := dialects[vendorKey]; ok {
		return d.Settings
	}
	return nil
}

// anthropicSettings is the same CONCEPT as OpenAI's effort, expressed the way
// this vendor expresses it.
//
// A person choosing how hard a model should think should not have to know that
// one vendor takes a word and the other takes a number of tokens. So the choice
// is the same four words everywhere and the adapter translates, which is the
// rule the whole provider layer runs on: vendor quirks are documented and
// converted at the edge, never branched in shared code.
//
// Medium is the budget this adapter has always used, so nothing changes for
// anybody who does not choose.
var anthropicSettings = []model.SettingSpec{reasoningEffortSetting}

// ThinkingBudget turns a chosen effort into this vendor's token budget.
func ThinkingBudget(settings model.Settings) int64 {
	switch reasoningEffortSetting.Value(settings) {
	case "low":
		return 1024
	case "high":
		return 8192
	case "max":
		return 16384
	default:
		return defaultThinkingBudget
	}
}
