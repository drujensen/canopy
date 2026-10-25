package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// collectNotices returns a thread-safe recorder and the ctx a client's
// retry middleware will report into — RunText's underlying HTTP calls run
// on whatever goroutine the SDK's retry loop uses, so appends must be
// synchronized the same way a real concurrent caller would need to.
func collectNotices(ctx context.Context) (ctxOut context.Context, get func() []RetryNotice) {
	var mu sync.Mutex
	var notices []RetryNotice
	ctxOut = WithRetryNotifier(ctx, func(n RetryNotice) {
		mu.Lock()
		defer mu.Unlock()
		notices = append(notices, n)
	})
	get = func() []RetryNotice {
		mu.Lock()
		defer mu.Unlock()
		return append([]RetryNotice(nil), notices...)
	}
	return ctxOut, get
}

// TestOpenAIRetryableStatusOption_NotifiesOn413RateLimit asserts that the
// Groq-shaped 413 case (openaiRetryableStatusOption's own doc comment) both
// retries and reports exactly one RetryNotice describing it, via the ctx a
// caller attached with WithRetryNotifier — the plumbing tui.startTurn
// relies on end to end.
func TestOpenAIRetryableStatusOption_NotifiesOn413RateLimit(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		if attempts < 2 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_ = json.NewEncoder(w).Encode(rateLimitedTooLargeBody())
			return
		}
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("hello after retry"))
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{Name: "test-groq", Type: entities.ProviderTypeGroq, APIKey: "k", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "qwen/qwen3.8-27b"}
	a, err := New(context.Background(), cfg, model, agent.Config{Name: "test-agent"})
	require.NoError(t, err)

	ctx, notices := collectNotices(context.Background())
	resp, err := a.RunText(ctx, "hi").Collect()
	require.NoError(t, err)
	assert.Equal(t, "hello after retry", resp.String())

	got := notices()
	require.Len(t, got, 1)
	assert.Equal(t, http.StatusRequestEntityTooLarge, got[0].StatusCode)
	assert.Contains(t, got[0].Reason, "rate limited (413")
	assert.Equal(t, 1, got[0].Attempt)
	assert.Equal(t, maxProviderRetries+1, got[0].MaxAttempts)
	assert.Greater(t, got[0].Delay, time.Duration(0), "a retry notice should report a positive delay estimate")
}

// TestOpenAIRetryableStatusOption_NotifiesOnPlain429 asserts the same
// notifier fires for a plain 429 too — the SDK's own default retry
// predicate already covers that status code (shouldRetry,
// requestconfig.go); notifyRetry must report it just as it reports the
// 413-specific case, not just the Groq-specific one this was added for.
func TestOpenAIRetryableStatusOption_NotifiesOnPlain429(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		if attempts < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"message": "slow down", "type": "requests", "code": "rate_limit_exceeded"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("hello after 429 retry"))
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{Name: "test-groq", Type: entities.ProviderTypeGroq, APIKey: "k", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "some-model"}
	a, err := New(context.Background(), cfg, model, agent.Config{Name: "test-agent"})
	require.NoError(t, err)

	ctx, notices := collectNotices(context.Background())
	resp, err := a.RunText(ctx, "hi").Collect()
	require.NoError(t, err)
	assert.Equal(t, "hello after 429 retry", resp.String())

	got := notices()
	require.Len(t, got, 1)
	assert.Equal(t, http.StatusTooManyRequests, got[0].StatusCode)
	assert.Contains(t, got[0].Reason, "429")
}

// TestOpenAIRetryableStatusOption_NoNotifierNoPanic asserts that running a
// turn with no WithRetryNotifier attached to ctx at all — the overwhelming
// common case, any caller that isn't tui.startTurn — behaves exactly as
// before: the retry still happens, nothing panics or blocks on a nil
// callback.
func TestOpenAIRetryableStatusOption_NoNotifierNoPanic(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		if attempts < 2 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_ = json.NewEncoder(w).Encode(rateLimitedTooLargeBody())
			return
		}
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("ok"))
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{Name: "test-groq", Type: entities.ProviderTypeGroq, APIKey: "k", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "some-model"}
	a, err := New(context.Background(), cfg, model, agent.Config{Name: "test-agent"})
	require.NoError(t, err)

	resp, err := a.RunText(context.Background(), "hi").Collect()
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.String())
}
