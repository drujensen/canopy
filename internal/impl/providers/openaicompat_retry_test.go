package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// rateLimitedTooLargeBody is the response shape a real Groq 413 carries for
// an input-tokens-per-minute condition (see openaiRetryableStatusOption's
// doc comment) — e.g. qwen/qwen3.8-27b, ITPM limit 7000, requested 10397.
func rateLimitedTooLargeBody() map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": "Request too large for model `qwen/qwen3.8-27b` ... input tokens per minute (ITPM): Limit 7000, Requested 10397, please reduce your message size and try again.",
			"type":    "tokens",
			"code":    "rate_limit_exceeded",
		},
	}
}

// TestNew_OpenAICompatible_Retries413RateLimited asserts that a 413 shaped
// like Groq's ITPM rate-limit condition is retried (not surfaced as an
// immediate hard error) and that the agent ultimately succeeds once the
// server starts responding normally — proving openaiRetryableStatusOption
// actually feeds back into the SDK's retry loop end to end, not just that
// the header gets set.
func TestNew_OpenAICompatible_Retries413RateLimited(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n < 3 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_ = json.NewEncoder(w).Encode(rateLimitedTooLargeBody())
			return
		}
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("hello after retry"))
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{
		Name:    "test-groq",
		Type:    entities.ProviderTypeGroq,
		APIKey:  "gq-test-key",
		BaseURL: server.URL + "/v1",
	}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "qwen/qwen3.8-27b"}

	a, err := New(context.Background(), cfg, model, agent.Config{Name: "test-agent"})
	require.NoError(t, err)

	resp, err := a.RunText(context.Background(), "hi").Collect()
	require.NoError(t, err)
	assert.Equal(t, "hello after retry", resp.String())
	assert.Equal(t, int32(3), attempts.Load(), "must have retried the 413 twice before the third attempt succeeded")
}

// TestNew_OpenAICompatible_DoesNotRetryUnrelated413 is the negative
// counterpart: a 413 that doesn't carry the rate_limit_exceeded shape (e.g.
// a plain oversized-payload rejection with no retryable condition) must
// still fail on the first attempt — retrying it would just reproduce the
// same failure every time and waste the retry budget.
func TestNew_OpenAICompatible_DoesNotRetryUnrelated413(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "payload exceeds the maximum allowed size",
				"type":    "invalid_request_error",
				"code":    "request_too_large",
			},
		})
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{
		Name:    "test-groq",
		Type:    entities.ProviderTypeGroq,
		APIKey:  "gq-test-key",
		BaseURL: server.URL + "/v1",
	}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "qwen/qwen3.8-27b"}

	a, err := New(context.Background(), cfg, model, agent.Config{Name: "test-agent"})
	require.NoError(t, err)

	_, err = a.RunText(context.Background(), "hi").Collect()
	require.Error(t, err)
	assert.Equal(t, int32(1), attempts.Load(), "an unrelated 413 shape must not be retried")
}
