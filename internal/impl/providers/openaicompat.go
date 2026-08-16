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
	}
	opts = append(opts, openaiTimeoutOptions(cfg)...)
	client := openai.NewClient(opts...)
	return openaiprovider.NewChatCompletionsAgent(client, openaiprovider.AgentConfig{
		Config: agentCfg,
		Model:  model.ModelName,
	}), nil
}
