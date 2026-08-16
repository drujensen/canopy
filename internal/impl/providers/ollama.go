package providers

// Ollama (entities.ProviderTypeOllama) gets its own construction path rather
// than sharing openaicompat.go's generic newOpenAICompatible, for two
// reasons specific to Ollama and not to any other OpenAI-compatible provider
// type in that shared list (DeepSeek, Groq, Mistral, Together, xAI):
//
//  1. Base URL ergonomics (normalizeOllamaBaseURL below): every other
//     OpenAI-compatible provider is a hosted API whose documented base URL
//     already is the exact string that belongs in BaseURL — there's nothing
//     to normalize. Ollama is the one type here a user is realistically
//     self-hosting and typing from memory (a bare host like
//     "ai.drujensen.com", or "localhost:11434"), so Canopy appends the
//     "/v1" Chat-Completions-compatible path itself instead of requiring
//     the user to remember and type it.
//  2. A confirmed wire-format bug in Ollama's OpenAI-compatible *streaming*
//     endpoint (ollama_transport.go has the full analysis): tool-call delta
//     chunks carry a spurious `"content":""` alongside `"tool_calls"`,
//     which trips a state-detection bug in the openai-go SDK's
//     ChatCompletionAccumulator and causes agent-framework-go's
//     openaiprovider to silently never see the tool call. Scoping the fix
//     to a dedicated Ollama transport — rather than patching it into every
//     OpenAI/OpenAI-compatible request generically — keeps this quirk-fix
//     visibly tied to the one backend it's needed for, and never touches a
//     byte of traffic to a real OpenAI-compatible hosted API that has never
//     been observed to have this bug.

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/provider/openaiprovider"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// newOllama constructs an *agent.Agent backed by Ollama's OpenAI-compatible
// Chat Completions API, with cfg.BaseURL normalized (normalizeOllamaBaseURL)
// and a response-patching transport (ollama_transport.go) installed to work
// around the tool-call streaming bug described in this file's package
// comment.
func newOllama(cfg entities.ProviderConfig, model entities.ModelConfig, agentCfg agent.Config) (*agent.Agent, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("providers: provider %q (%s) requires base_url", cfg.Name, cfg.Type)
	}
	baseURL, err := normalizeOllamaBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("providers: provider %q (%s) has an invalid base_url %q: %w", cfg.Name, cfg.Type, cfg.BaseURL, err)
	}

	httpClient := newOllamaHTTPClient(nil)
	opts := []option.RequestOption{
		option.WithBaseURL(baseURL),
		option.WithAPIKey(cfg.APIKey),
		option.WithHTTPClient(httpClient),
		openaiRetryOption(),
	}
	opts = append(opts, openaiTimeoutOptions(cfg)...)
	client := openai.NewClient(opts...)
	return openaiprovider.NewChatCompletionsAgent(client, openaiprovider.AgentConfig{
		Config: agentCfg,
		Model:  model.ModelName,
	}), nil
}

// normalizeOllamaBaseURL turns a user-supplied Ollama host/URL into the full
// base URL openai-go needs to reach the OpenAI-compatible Chat Completions
// endpoint:
//
//   - A missing scheme defaults to "https://" — Ollama exposed to anything
//     other than localhost is overwhelmingly reverse-proxied behind TLS (as
//     ai.drujensen.com is); a user who genuinely wants plain HTTP (typical
//     for a local install, e.g. "localhost:11434") types "http://"
//     explicitly, same as they always have.
//   - A trailing "/v1" (with or without a trailing slash) is left alone —
//     an existing config carried over from before this normalization
//     existed, or a user who just prefers to type the full path, must not
//     end up with "/v1/v1".
//   - Otherwise "/v1" is appended, so BaseURL only needs to name the host
//     Canopy should reach Ollama on.
func normalizeOllamaBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("base_url is empty")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	trimmed = strings.TrimRight(trimmed, "/")

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("no host in %q", raw)
	}

	if strings.HasSuffix(trimmed, "/v1") {
		return trimmed, nil
	}
	return trimmed + "/v1", nil
}
