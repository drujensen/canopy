package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// rateLimitedThenOKHandler returns an http.HandlerFunc that answers 429 for
// the first failCount requests and a normal chat completion after that —
// used to prove a client actually retries a 429 rather than surfacing it as
// an immediate error.
func rateLimitedThenOKHandler(t *testing.T, failCount int32, okText string) (http.HandlerFunc, *int32) {
	t.Helper()
	var calls int32
	return func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= failCount {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rate limited"}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse(okText))
	}, &calls
}

// TestOpenAIRetryOption_RetriesTransient429sAndSucceeds proves
// maxProviderRetries/openaiRetryOption (openaicompat.go) isn't just a
// no-op constant: an OpenAI-native client (factory.go's newOpenAINative,
// which now includes openaiRetryOption()) hitting a server that answers 429
// twice before succeeding must still end up with a successful response,
// having made 3 requests total, rather than surfacing the first 429 as an
// immediate error the way it would with the SDK's un-overridden retry
// count of 2 exhausted on a *different* failure count.
func TestOpenAIRetryOption_RetriesTransient429sAndSucceeds(t *testing.T) {
	handler, calls := rateLimitedThenOKHandler(t, 2, "ok after retries")
	server := httptest.NewServer(handler)
	defer server.Close()

	cfg := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "gpt-test"}

	a, err := New(context.Background(), cfg, model, agent.Config{Name: "a"})
	require.NoError(t, err)

	resp, err := a.RunText(context.Background(), "hi").Collect()
	require.NoError(t, err, "the client must retry past the two 429s and still succeed")
	assert.Equal(t, "ok after retries", resp.String())
	assert.Equal(t, int32(3), atomic.LoadInt32(calls), "exactly 2 failing attempts plus 1 succeeding attempt")
}

// TestOllamaRetryOption_RetriesTransient429sAndSucceeds is
// TestOpenAIRetryOption_RetriesTransient429sAndSucceeds for the Ollama path
// (ollama.go's newOllama, which also now includes openaiRetryOption()) —
// confirms the retry option survives Ollama's own BaseURL-normalization/
// transport-wrapping construction path, not just the plain OpenAI one.
func TestOllamaRetryOption_RetriesTransient429sAndSucceeds(t *testing.T) {
	handler, calls := rateLimitedThenOKHandler(t, 2, "ok from ollama")
	server := httptest.NewServer(handler)
	defer server.Close()

	cfg := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOllama, BaseURL: server.URL}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "llama3"}

	a, err := New(context.Background(), cfg, model, agent.Config{Name: "a"})
	require.NoError(t, err)

	resp, err := a.RunText(context.Background(), "hi").Collect()
	require.NoError(t, err)
	assert.Equal(t, "ok from ollama", resp.String())
	assert.Equal(t, int32(3), atomic.LoadInt32(calls))
}

// TestOpenAIRetryOption_GivesUpEventually proves the retry budget is still
// bounded, not infinite: a server that always 429s must eventually surface
// as an error rather than retrying forever. Bounded by a generous 60s
// (maxProviderRetries=5 with openai-go's exponential backoff capped at 8s/
// attempt caps the worst case around 15s of cumulative delay plus request
// time) so this only ever fails if retrying genuinely never stops.
func TestOpenAIRetryOption_GivesUpEventually(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rate limited"}})
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "gpt-test"}

	a, err := New(context.Background(), cfg, model, agent.Config{Name: "a"})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err = a.RunText(ctx, "hi").Collect()
	require.Error(t, err, "an always-429 server must eventually surface as an error, not hang or succeed")
	assert.GreaterOrEqual(t, atomic.LoadInt32(&calls), int32(2), "must have actually retried at least once, not given up on the first 429")
}
