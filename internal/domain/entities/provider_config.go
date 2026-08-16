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
	ContextWindowTokens int `json:"context_window_tokens,omitempty"`
}
