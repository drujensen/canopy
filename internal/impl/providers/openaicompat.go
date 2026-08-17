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
// one).
//
// Bugfix (post-v0.1.0): google.golang.org/genai (Gemini) does NOT retry by
// default, despite an earlier version of this comment claiming otherwise —
// that was a misreading of the SDK source. genai's retryHTTPRequest
// (common.go) starts with `if opts == nil { return do(req) }`: retries are
// opt-in via an explicit *genai.HTTPRetryOptions on ClientConfig.HTTPOptions
// (or per-request types.HTTPOptions), matching the Python/JS SDKs'
// documented "retries must be requested explicitly" behavior.
// defaultRetryAttempts=5/defaultRetryHTTPStatusCodes (including 429) are
// only the *values* genai substitutes for an unset field once
// HTTPRetryOptions is non-nil — they were never a default-on behavior. A
// real user hit exactly this gap: a Gemini 429 propagated as an immediate
// hard error with zero retry attempts. Fixed in factory.go's newGemini by
// setting HTTPOptions.RetryOptions explicitly. 5 here matches that fix, so
// every provider family gets the same retry budget for the same class of
// error.
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
