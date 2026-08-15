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
// (DeepSeek, Ollama, Groq, Mistral, Together, xAI) also sets BaseURL.
type ProviderConfig struct {
	// Name is the user-chosen identifier for this provider configuration,
	// e.g. "work-openai". ModelConfig.Provider references it.
	Name string `json:"name"`

	Type    ProviderType `json:"type"`
	APIKey  string       `json:"api_key,omitempty"`
	BaseURL string       `json:"base_url,omitempty"`
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
}
