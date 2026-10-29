package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// openaiRetryableStatusOption extends which responses the openai-go SDK's
// own retry loop (internal/requestconfig/requestconfig.go's shouldRetry,
// honored via maxProviderRetries/openaiRetryOption above — exponential
// backoff with jitter, capped at 8s per attempt, same budget 429/5xx already
// get) treats as retryable, beyond its hardcoded 408/409/429/5xx set.
//
// Confirmed case: Groq returns the "too many input tokens for this model's
// per-minute allowance" condition as HTTP 413, not 429 — e.g.
// `{"error":{"message":"Request too large for model `qwen/qwen3.8-27b` ...
// input tokens per minute (ITPM): Limit 7000, Requested 10397 ...",
// "type":"tokens","code":"rate_limit_exceeded"}}`. That's the same class of
// transient, wait-and-retry condition 429 already covers (a per-minute
// budget, not a permanently-oversized body) — a real user hit it and the
// request failed immediately with zero retries because shouldRetry doesn't
// know 413 can mean this.
//
// Implemented as middleware (option.WithMiddleware) that sets the
// "x-should-retry" response header shouldRetry already special-cases ahead
// of the status-code check, rather than reimplementing the retry loop or
// backoff math. Scoped to error.code == "rate_limit_exceeded" specifically
// (not every 413) so a genuinely-oversized request — one no amount of
// waiting fixes, e.g. a request body some other OpenAI-compatible host
// flatly rejects for size — isn't retried into a predictable second failure;
// that shape doesn't carry this code, so it falls through unmarked and the
// SDK's existing default (don't retry) stands.
// It also reports every retry it lets through — and every retry the SDK's
// own default predicate (429/5xx/408/409) was always going to let through —
// to whatever notifier WithRetryNotifier (retrynotice.go) attached to the
// request's context, so a caller like the TUI can tell the user they're
// being throttled instead of a turn just silently stalling.
func openaiRetryableStatusOption() option.RequestOption {
	return option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		res, err := next(req)
		if err != nil || res == nil {
			return res, err
		}

		forced413 := false
		if res.StatusCode == http.StatusRequestEntityTooLarge && res.Body != nil {
			raw, readErr := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if readErr != nil {
				res.Body = io.NopCloser(bytes.NewReader(nil))
				return res, nil
			}
			res.Body = io.NopCloser(bytes.NewReader(raw))

			var parsed struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(raw, &parsed) == nil && parsed.Error.Code == "rate_limit_exceeded" {
				res.Header.Set("x-should-retry", "true")
				forced413 = true
			}
		}

		notifyRetry(req, res, forced413)
		return res, nil
	})
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
		openaiRetryableStatusOption(),
	}
	opts = append(opts, openaiTimeoutOptions(cfg)...)
	opts = append(opts, openaiStallTimeoutOption(cfg)...)
	client := openai.NewClient(opts...)
	return openaiprovider.NewChatCompletionsAgent(client, openaiprovider.AgentConfig{
		Config: agentCfg,
		Model:  model.ModelName,
	}), nil
}
