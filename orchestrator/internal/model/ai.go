package model

import "time"

// Vendor keys name the gateway adapter that talks to a provider. They are a
// closed set: an unknown key would be a vendor the gateway cannot reach.
//
// Anthropic has its own wire format and its own adapter. Everything else
// speaks the OpenAI chat-completions format and shares one adapter, with the
// per-vendor differences (endpoint, how thinking is returned, how it is
// switched on, Azure's auth) expressed as a dialect rather than as branching.
const (
	VendorAnthropic = "anthropic"
	VendorOpenAI    = "openai"
	VendorDeepSeek  = "deepseek"
	// VendorZAI is Z.ai (GLM).
	VendorZAI         = "zai"
	VendorMistral     = "mistral"
	VendorAzureOpenAI = "azure-openai"
	// VendorOpenAICompatible is the generic entry for any server speaking
	// the same format: vLLM, llama.cpp, LM Studio, and anything
	// self-hosted. It is how local models arrive: a row, not new code.
	VendorOpenAICompatible = "openai-compatible"
)

// VendorInfo is one entry of the catalog the vendor form renders: the key the
// gateway stores and validates, and the name a person reads. It has the same
// shape as the permission catalog for the same reason: the key is a thing the
// system needs, not a thing to show somebody, and a client that had to turn
// "azure-openai" into "Azure OpenAI" itself would be keeping a second copy of a
// list it does not own.
type VendorInfo struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	// RequiresBaseURL tells the form to ask for an endpoint: a vendor defined by
	// where it runs (a local server, or Azure's per-customer endpoint) rather
	// than by a fixed vendor API. Derived from RequiresBaseURL when served, so it
	// cannot drift from what validation enforces.
	RequiresBaseURL bool `json:"requires_base_url,omitempty"`
}

// VendorCatalog is the closed set in display order, and /v1/vendor-kinds serves
// it. It is the one place a vendor is named.
var VendorCatalog = []VendorInfo{
	{Key: VendorAnthropic, Name: "Anthropic"},
	{Key: VendorOpenAI, Name: "OpenAI"},
	{Key: VendorDeepSeek, Name: "DeepSeek"},
	{Key: VendorZAI, Name: "Z.ai"},
	{Key: VendorMistral, Name: "Mistral"},
	{Key: VendorAzureOpenAI, Name: "Azure OpenAI"},
	// Named for the server most people point it at, not for the protocol.
	//
	// It was "Local", and that stopped being true: OUR local story is a machine
	// that joins itself and appears under Machines, and a model from one is
	// added from Machines or from the models screen. What is left here is
	// somebody ELSE's server, which has no control plane and cannot join, so
	// typing an address and a key is the only way in. It accepts any server
	// speaking the same protocol (vLLM, LM Studio, llama.cpp), and the form's
	// hint says so.
	{Key: VendorOpenAICompatible, Name: "Ollama"},
}

// KnownVendorKeys is what validation checks against. It is DERIVED from the
// catalog rather than written out beside it: two hand-kept lists of the same
// thing are two lists that will disagree, and the disagreement shows up as a
// vendor you can pick and cannot save.
var KnownVendorKeys = vendorKeys()

func vendorKeys() []string {
	keys := make([]string, len(VendorCatalog))
	for i, vendor := range VendorCatalog {
		keys[i] = vendor.Key
	}
	return keys
}

// RequiresBaseURL reports whether a vendor is defined by the endpoint it
// points at rather than by a fixed vendor API. Azure gives every customer
// their own endpoint, and a self-hosted server is nothing but an endpoint.
func RequiresBaseURL(vendorKey string) bool {
	switch vendorKey {
	case VendorOpenAICompatible, VendorAzureOpenAI:
		return true
	default:
		return false
	}
}

// AIVendor is a configured provider account. Credentials are sealed
// (envelope-encrypted) and never leave the server: the API exposes only
// whether a credential is present.
type AIVendor struct {
	ID          int64
	WorkspaceID int64
	VendorKey   string
	Name        string
	BaseURL     string
	// NodeID points this vendor at a machine of ours (KB/35), and when it is set
	// this row carries NO address and NO key of its own: both are read from the
	// machine. A machine belongs to the platform and a model belongs to a
	// workspace, so a workspace that has been given a model from a machine gets
	// one of these, and adding the same model to a second workspace writes rows
	// rather than downloading anything again.
	//
	// Zero for every hosted vendor, which is most of them.
	NodeID      int64
	Credentials []byte // sealed; nil when the vendor needs no key (a local server)
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// Settings are chosen values for keys this vendor's dialect declares.
	Settings Settings
}

// HasCredentials reports whether a secret is stored, without revealing it.
func (v *AIVendor) HasCredentials() bool { return len(v.Credentials) > 0 }

// Model types the gateway can serve. Chat is the only one the agent loop
// uses today; the others are registered so the registry is ready for
// embeddings (brains), reranking, and voice.
const (
	ModelTypeChat      = "chat"
	ModelTypeEmbedding = "embedding"
	ModelTypeRerank    = "rerank"
	ModelTypeSTT       = "stt"
	ModelTypeTTS       = "tts"
)

var KnownModelTypes = []string{
	ModelTypeChat, ModelTypeEmbedding, ModelTypeRerank, ModelTypeSTT, ModelTypeTTS,
}

// VendorIsLocal reports whether a vendor keeps the work in the building: a
// self-hosted endpoint. Whether a model's data leaves is a property of WHERE it
// runs (the vendor), never a flag on the model, so it is derived here rather
// than stored.
func VendorIsLocal(vendorKey string) bool {
	return vendorKey == VendorOpenAICompatible
}

// InferenceNode is one machine that runs models we own.
//
// It belongs to the PLATFORM and not to a workspace: a machine is bought once
// and racked once, and every workspace on the deployment may have models on it.
// What a workspace owns is the model row that routes to one.
//
// It registers itself (KB/35): started with where we are and a shared token, it
// mints its own key, sends it once, and appears. Nobody types a row.
type InferenceNode struct {
	ID int64
	// NodeID is minted on the machine's own disk and never changes, which is how
	// a machine that comes back is recognised as the same machine rather than
	// added twice under a new address.
	NodeID string
	Name   string
	// BaseURL is the INFERENCE address, ending in the version segment, because
	// that is what the gateway uses unchanged. The control surface is its
	// sibling and is derived where it is needed.
	BaseURL string
	// Key is sealed. It was minted by the machine and is unique to it, so
	// removing one machine affects no other.
	Key     []byte
	Version string
	// CertExpiresAt is when the certificate we issued this machine runs out.
	// Nil for a machine that has not been issued one yet, which is a machine
	// that joined before there was an authority and has not been back since.
	CertExpiresAt *time.Time
	// PinnedCert is the certificate of a machine that issued its own, held
	// because it chains to nothing and there is no authority to check it
	// against. Empty for every machine that joined and was signed by ours,
	// which is how the two are told apart when dialling.
	PinnedCert *string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// AIModel is a model a workspace may route to. Pricing is stored per
// million tokens and versioned by the row, so cost is always computed from
// raw token counts rather than stored as a precomputed number.
type AIModel struct {
	ID            int64
	WorkspaceID   int64
	VendorID      int64
	ModelKey      string
	Type          string
	ContextWindow int
	// Description is a free note kept about this specific model.
	Description      string
	InputPricePer1M  float64
	OutputPricePer1M float64
	Status           string
	// Settings are chosen values for keys the model may be configured with
	// (provider.SettingsForModel). Anything not declared there is ignored.
	Settings  Settings
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CallCost is the dollar cost of one model call, derived from its token counts
// and this model's per-million pricing. Cost is always computed from raw token
// counts, never stored precomputed (KB/06); a model with no pricing costs zero.
func (m *AIModel) CallCost(inputTokens, outputTokens int64) float64 {
	return float64(inputTokens)/1_000_000*m.InputPricePer1M +
		float64(outputTokens)/1_000_000*m.OutputPricePer1M
}
