package entities

// ProviderType identifies which LLM provider family a ProviderConfig
// connects to (Requirements FR2).
type ProviderType string

const (
	ProviderTypeOpenAI    ProviderType = "openai"
	ProviderTypeAnthropic ProviderType = "anthropic"
	ProviderTypeGemini    ProviderType = "gemini"
	ProviderTypeDeepSeek  ProviderType = "deepseek"
	ProviderTypeOllama    ProviderType = "ollama"
	ProviderTypeGroq      ProviderType = "groq"
	ProviderTypeMistral   ProviderType = "mistral"
	ProviderTypeTogether  ProviderType = "together"
	ProviderTypeXAI       ProviderType = "xai"
)

// ProviderConfig holds connection details for one configured LLM provider
// (Requirements FR1/FR2, Design §4). Native providers (OpenAI, Anthropic,
// Gemini) typically only need APIKey; the OpenAI-compatible adapter
// (DeepSeek, Ollama, Groq, Mistral, Together, xAI, or any other provider
// type — see impl/providers.New's generalized fallback dispatch) also sets
// BaseURL.
//
// Addendum (post-v0.1.0, zero-config auto-detection): APIKeyEnv names an
// environment variable to resolve the real API key from (e.g.
// "OPENAI_API_KEY") instead of storing the literal secret in
// providers.json. impl/config.ProviderStore.Load resolves it into APIKey
// once, in memory, right after reading the file — every downstream consumer
// (impl/providers, AgentService) only ever sees a populated APIKey, exactly
// as if it had been written literally. If APIKey is already set (a user
// pasted a literal key directly, or a provider like local Ollama needs none
// at all), APIKeyEnv is ignored: an explicit literal value is a deliberate
// override and always wins over resolving from the environment.
type ProviderConfig struct {
	// Name is the user-chosen identifier for this provider configuration,
	// e.g. "work-openai". ModelConfig.Provider references it.
	Name string `json:"name"`

	Type    ProviderType `json:"type"`
	APIKey  string       `json:"api_key,omitempty"`
	BaseURL string       `json:"base_url,omitempty"`

	// APIKeyEnv is the name of an environment variable holding the real API
	// key, resolved into APIKey at load time (see ProviderStore.Load). Only
	// the env var *name* is ever persisted to disk — never the secret
	// itself.
	APIKeyEnv string `json:"api_key_env,omitempty"`

	// TimeoutSeconds overrides how long impl/providers waits for a
	// response's headers before giving up (post-v0.1.0 addendum,
	// Requirements §7). Zero means "use the provider SDK's own default" —
	// for the OpenAI/OpenAI-compatible path that's openai-go's own
	// 10-minute ResponseHeaderTimeout, which is generous for a hosted API
	// but can still be too short for a self-hosted, locally-run model
	// (e.g. Ollama cold-loading a large model before it can start
	// responding). Set this explicitly for a provider known to be slow
	// rather than living with a timeout error; it has no effect on an
	// already-started streaming response body, only on how long Canopy
	// waits for the response to *begin*.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// StallTimeoutSeconds bounds how long impl/providers will keep waiting
	// once a response has already started streaming without receiving any
	// further bytes (post-v0.1.0 addendum) — complementary to
	// TimeoutSeconds, not a replacement for it: TimeoutSeconds only governs
	// the wait for a response to *begin* (its doc comment above), so a
	// request that gets headers back quickly and then goes completely
	// silent mid-stream is, today, unbounded by TimeoutSeconds at all. Zero
	// disables this watchdog entirely — opt-in, since a sensible value
	// depends on how slow a provider's first token can legitimately be (a
	// cold-loading local model vs. an always-warm hosted API), and guessing
	// wrong is worse than asking the provider's owner to set it once where
	// it matters. A response that's merely slow but still producing bytes
	// is never aborted by this, no matter how long the whole response
	// takes — only a genuine gap with zero progress trips it. See
	// impl/providers/stalltimeout.go for the real incident (a self-hosted
	// Ollama request that went silent mid-generation while the host
	// memory-thrashed) that prompted adding this.
	StallTimeoutSeconds int `json:"stall_timeout_seconds,omitempty"`
}

// ModelConfig pairs a model with one configured provider and its invocation
// parameters (Requirements FR1). Any Agent can be paired with any
// ModelConfig by name.
type ModelConfig struct {
	// Name is the user-chosen identifier for this model configuration.
	Name string `json:"name"`

	// Provider references a ProviderConfig.Name in the same config file.
	Provider string `json:"provider"`

	// ModelName is the actual model identifier sent to the provider API,
	// e.g. "gpt-4o-mini" or "claude-sonnet-4-5".
	ModelName string `json:"model_name"`

	// Parameters holds free-form per-model invocation parameters
	// (temperature, max_tokens, etc.).
	Parameters map[string]any `json:"parameters,omitempty"`

	// ContextWindowTokens is the model's total context window, used by
	// impl/harness to size the compaction trigger (Design §3.5, Requirements
	// FR10). Zero/unset means "unknown" — impl/harness falls back to a fixed,
	// documented default rather than requiring every model configuration to
	// set this before compaction works. Follow-up: seed this automatically
	// from known-model tables per provider instead of requiring manual
	// configuration.
	//
	// impl/config.DetectProviders auto-populates this from the models.dev
	// catalog's own value for every provider type, including self-hosted
	// ones (Ollama, or a self-hosted server behind the generic
	// OpenAI-compatible fallback) — deliberately not special-cased, since
	// the catalog's number is a model's theoretical architecture max, not a
	// claim about what memory any particular box has behind it. If you're
	// self-hosting and the model's real usable context is smaller than
	// what got auto-detected, lower (or remove) this by hand in
	// providers.json: a model's ContextWindowTokens is only ever written
	// once, when it's first added, and a later --refresh-providers run
	// never overwrites an existing model's value for this field (unlike
	// per-model cost, which does stay synced — see
	// cmd/canopy/main.go's updateExistingModelCosts) — so a correction
	// made here is permanent.
	ContextWindowTokens int `json:"context_window_tokens,omitempty"`

	// InputCostPerMillionTokens and OutputCostPerMillionTokens are the
	// model's price in US dollars per million request/response tokens
	// (post-v0.1.0 addendum), shown in the TUI's ctrl+o model picker so a
	// user can compare cost before switching. Zero/unset means "unknown or
	// free" (e.g. a self-hosted Ollama model, or a model whose provider
	// doesn't publish per-token pricing) — the picker shows nothing rather
	// than a misleading "$0.00" in that case. impl/config.DetectProviders
	// populates these automatically from the models.dev catalog's own
	// "cost.input"/"cost.output" fields for an auto-detected provider; a
	// manually hand-edited providers.json entry can set them directly the
	// same way.
	InputCostPerMillionTokens  float64 `json:"input_cost_per_million_tokens,omitempty"`
	OutputCostPerMillionTokens float64 `json:"output_cost_per_million_tokens,omitempty"`
}
