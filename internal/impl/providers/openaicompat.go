package providers

import (
	"fmt"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/provider/openaiprovider"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// openaiTimeoutOptions returns option.WithRequestTimeout(...) when
// cfg.TimeoutSeconds is set (see entities.ProviderConfig.TimeoutSeconds'
// doc comment), or no options at all when it's zero — in which case
// openai-go falls back to its own defaultHTTPClient's 10-minute
// ResponseHeaderTimeout, unchanged from before this field existed. Shared
// between the native OpenAI path (factory.go) and this file's
// OpenAI-compatible path, since both need the same override.
func openaiTimeoutOptions(cfg entities.ProviderConfig) []option.RequestOption {
	if cfg.TimeoutSeconds <= 0 {
		return nil
	}
	return []option.RequestOption{option.WithRequestTimeout(time.Duration(cfg.TimeoutSeconds) * time.Second)}
}

// maxProviderRetries is how many times a failed request (429 rate-limited,
// 408/409, or 5xx) is retried, with exponential backoff and jitter, before
// giving up. Applied explicitly via option.WithMaxRetries/
// anthropicoption.WithMaxRetries (factory.go, ollama.go) rather than
// silently inheriting whatever each vendored SDK happens to default to:
// openai-go and anthropic-sdk-go both default to only 2 retries (confirmed
// directly in each SDK's internal/requestconfig/requestconfig.go —
// `MaxRetries: 2`, with a retry predicate covering 408/409/429/5xx and a
// backoff that honors a `Retry-After` response header when the server sends
// one). google.golang.org/genai (Gemini) already defaults to 5 attempts
// including 429 in its retryable status codes (common.go's
// defaultRetryAttempts/defaultRetryHTTPStatusCodes) and is deliberately left
// on its own default rather than duplicating that configuration here — 5
// here just matches it, so a 429 gets the same retry budget regardless of
// which provider family is in play, instead of OpenAI/Anthropic silently
// giving up sooner than Gemini would for the identical error.
const maxProviderRetries = 5

// openaiRetryOption is maxProviderRetries as an option.RequestOption, used
// by every openai-go client construction site (newOpenAINative, factory.go;
// newOpenAICompatible, this file; newOllama, ollama.go).
func openaiRetryOption() option.RequestOption {
	return option.WithMaxRetries(maxProviderRetries)
}

// newOpenAICompatible constructs an *agent.Agent for any OpenAI-compatible
// provider (DeepSeek, Ollama, Groq, Mistral, Together, xAI — Design §4) by
// pointing openaiprovider's Chat Completions client at cfg.BaseURL. These
// providers generally only implement the Chat Completions API, not the
// Responses API, which is why every OpenAI-compatible provider shares this
// one path instead of getting its own adapter.
func newOpenAICompatible(cfg entities.ProviderConfig, model entities.ModelConfig, agentCfg agent.Config) (*agent.Agent, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("providers: provider %q (%s) requires base_url", cfg.Name, cfg.Type)
	}
	opts := []option.RequestOption{
		option.WithBaseURL(cfg.BaseURL),
		option.WithAPIKey(cfg.APIKey),
		openaiRetryOption(),
	}
	opts = append(opts, openaiTimeoutOptions(cfg)...)
	client := openai.NewClient(opts...)
	return openaiprovider.NewChatCompletionsAgent(client, openaiprovider.AgentConfig{
		Config: agentCfg,
		Model:  model.ModelName,
	}), nil
}
