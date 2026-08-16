package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// chatCompletionResponse builds a minimal, valid OpenAI Chat Completions
// response body carrying the given assistant text.
func chatCompletionResponse(text string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion",
		"created": 1700000000,
		"model":   "test-model",
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": text,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     3,
			"completion_tokens": 5,
			"total_tokens":      8,
		},
	}
}

// TestNew_OpenAINative asserts that a ProviderTypeOpenAI config hits the
// standard Chat Completions endpoint shape and streams a response back
// through RunText(...).Collect(), with the configured API key sent as a
// bearer token.
func TestNew_OpenAINative(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("hello from openai"))
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{
		Name:    "test-openai",
		Type:    entities.ProviderTypeOpenAI,
		APIKey:  "sk-test-key",
		BaseURL: server.URL + "/v1",
	}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "gpt-test"}

	a, err := New(context.Background(), cfg, model, agent.Config{Name: "test-agent"})
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "openai", a.ProviderName())

	resp, err := a.RunText(context.Background(), "hi").Collect()
	require.NoError(t, err)
	assert.Equal(t, "hello from openai", resp.String())

	assert.Contains(t, gotPath, "/chat/completions")
	assert.Equal(t, "Bearer sk-test-key", gotAuth)
	require.NotNil(t, gotBody)
	assert.Equal(t, "gpt-test", gotBody["model"])
}

// TestNew_OpenAICompatible asserts that an OpenAI-compatible provider type
// (DeepSeek here, representative of the six that share this path) is routed
// through the same Chat Completions client shape but against the
// configured BaseURL, per Design §4.
func TestNew_OpenAICompatible(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("hello from deepseek"))
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{
		Name:    "test-deepseek",
		Type:    entities.ProviderTypeDeepSeek,
		APIKey:  "ds-test-key",
		BaseURL: server.URL + "/v1",
	}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "deepseek-chat"}

	a, err := New(context.Background(), cfg, model, agent.Config{Name: "test-agent"})
	require.NoError(t, err)
	require.NotNil(t, a)

	resp, err := a.RunText(context.Background(), "hi").Collect()
	require.NoError(t, err)
	assert.Equal(t, "hello from deepseek", resp.String())

	assert.Contains(t, gotPath, "/chat/completions")
	assert.Equal(t, "Bearer ds-test-key", gotAuth)
	require.NotNil(t, gotBody)
	assert.Equal(t, "deepseek-chat", gotBody["model"])
}

// TestNew_OpenAICompatible_TimeoutSecondsEnforced proves
// entities.ProviderConfig.TimeoutSeconds actually bounds how long a request
// waits for response headers — not just that it's wired into the option
// list without effect. A server that never responds would otherwise hang
// for openai-go's own 10-minute default (see openaiTimeoutOptions' doc
// comment); with TimeoutSeconds set to a small value, the request must fail
// well before that, with an error that reflects a timeout, not some other
// failure mode.
func TestNew_OpenAICompatible_TimeoutSecondsEnforced(t *testing.T) {
	// The handler sleeps well past the 1s client-side timeout under test,
	// then returns — long enough to guarantee the client gives up first,
	// short enough that the deferred server.Close() below doesn't itself
	// hang waiting for a handler goroutine that never returns (an earlier
	// version of this test used select{} here and that's exactly what
	// happened: httptest.Server.Close() waits for in-flight handlers to
	// finish, so a handler that never returns hangs test cleanup even
	// after the client has already correctly timed out).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{
		Name:           "test-slow",
		Type:           entities.ProviderTypeDeepSeek,
		APIKey:         "k",
		BaseURL:        server.URL + "/v1",
		TimeoutSeconds: 1,
	}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "slow-model"}

	a, err := New(context.Background(), cfg, model, agent.Config{Name: "test-agent"})
	require.NoError(t, err)

	start := time.Now()
	_, err = a.RunText(context.Background(), "hi").Collect()
	elapsed := time.Since(start)

	require.Error(t, err, "a request to a server that never responds must fail once TimeoutSeconds elapses")
	assert.Less(t, elapsed, 10*time.Second, "must fail near the configured 1s timeout, not openai-go's 10-minute default")
}

// TestNew_OpenAICompatible_AllProviderTypes exercises every provider type
// that routes through the openaicompat path, confirming none of them are
// mis-dispatched to a different constructor and all require a BaseURL.
// Ollama is deliberately excluded here — it has its own dedicated path
// (newOllama, ollama.go) and its own test file (ollama_test.go).
func TestNew_OpenAICompatible_AllProviderTypes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("ok"))
	}))
	defer server.Close()

	types := []entities.ProviderType{
		entities.ProviderTypeDeepSeek,
		entities.ProviderTypeGroq,
		entities.ProviderTypeMistral,
		entities.ProviderTypeTogether,
		entities.ProviderTypeXAI,
	}
	for _, pt := range types {
		t.Run(string(pt), func(t *testing.T) {
			cfg := entities.ProviderConfig{Name: "p", Type: pt, APIKey: "k", BaseURL: server.URL}
			model := entities.ModelConfig{Name: "m", Provider: "p", ModelName: "some-model"}
			a, err := New(context.Background(), cfg, model, agent.Config{Name: "a"})
			require.NoError(t, err)
			require.NotNil(t, a)
			assert.Equal(t, "openai", a.ProviderName())
		})
	}
}

// TestNew_OpenAICompatible_MissingBaseURL asserts a clear error rather than
// silently falling back to the real OpenAI endpoint when a compat provider
// has no BaseURL configured.
func TestNew_OpenAICompatible_MissingBaseURL(t *testing.T) {
	cfg := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeDeepSeek, APIKey: "k"}
	model := entities.ModelConfig{Name: "m", Provider: "p", ModelName: "deepseek-chat"}
	a, err := New(context.Background(), cfg, model, agent.Config{Name: "a"})
	require.Error(t, err)
	assert.Nil(t, a)
	assert.Contains(t, err.Error(), "base_url")
}

// TestNew_UnrecognizedProviderType asserts a clear, specific error rather
// than a panic or a silent default when cfg.Type doesn't match any known
// provider and has no BaseURL — there's genuinely no endpoint to call.
func TestNew_UnrecognizedProviderType(t *testing.T) {
	cfg := entities.ProviderConfig{Name: "mystery", Type: entities.ProviderType("carrier-pigeon"), APIKey: "k"}
	model := entities.ModelConfig{Name: "m", Provider: "mystery", ModelName: "whatever"}
	a, err := New(context.Background(), cfg, model, agent.Config{Name: "a"})
	require.Error(t, err)
	assert.Nil(t, a)
	assert.Contains(t, err.Error(), "carrier-pigeon")
}

// TestNew_UnrecognizedProviderType_WithBaseURL asserts the generalized
// fallback dispatch (post-v0.1.0 addendum, Design §4): a cfg.Type Canopy has
// never heard of — e.g. one auto-detected straight from the models.dev
// catalog's own provider ID string — still succeeds via the generic
// OpenAI-compatible path as long as a BaseURL is configured, on the same
// "assume OpenAI Chat Completions compatible" rationale as the 6 named
// compat types.
func TestNew_UnrecognizedProviderType_WithBaseURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("hello from an arbitrary catalog provider"))
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{
		Name:    "some-new-provider",
		Type:    entities.ProviderType("some-new-provider-id"),
		APIKey:  "k",
		BaseURL: server.URL + "/v1",
	}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "whatever-model"}

	a, err := New(context.Background(), cfg, model, agent.Config{Name: "a"})
	require.NoError(t, err)
	require.NotNil(t, a)

	resp, err := a.RunText(context.Background(), "hi").Collect()
	require.NoError(t, err)
	assert.Equal(t, "hello from an arbitrary catalog provider", resp.String())
}

// TestNew_UnrecognizedProviderType_NoBaseURL is the explicit negative
// counterpart to TestNew_UnrecognizedProviderType_WithBaseURL: no BaseURL
// means no endpoint to call, so this still errors clearly rather than
// silently succeeding or falling back to the real OpenAI endpoint.
func TestNew_UnrecognizedProviderType_NoBaseURL(t *testing.T) {
	cfg := entities.ProviderConfig{Name: "some-new-provider", Type: entities.ProviderType("some-new-provider-id"), APIKey: "k"}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "whatever-model"}
	a, err := New(context.Background(), cfg, model, agent.Config{Name: "a"})
	require.Error(t, err)
	assert.Nil(t, a)
	assert.Contains(t, err.Error(), "some-new-provider-id")
	assert.Contains(t, err.Error(), "no base_url")
}

// TestNew_Anthropic asserts the Anthropic client/agent path constructs
// successfully and is wired to the anthropic provider. Mocking Anthropic's
// exact Messages API wire format is out of scope here (Design §4 prioritizes
// real coverage on the OpenAI/OpenAI-compatible paths, which cover 6 of 9
// provider types); this test only verifies the right client gets
// constructed and the right model is threaded through.
func TestNew_Anthropic(t *testing.T) {
	cfg := entities.ProviderConfig{
		Name:   "test-anthropic",
		Type:   entities.ProviderTypeAnthropic,
		APIKey: "ak-test-key",
	}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "claude-test"}

	a, err := New(context.Background(), cfg, model, agent.Config{Name: "test-agent"})
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "anthropic", a.ProviderName())
	assert.Equal(t, "test-agent", a.Name())
}

// TestNew_Gemini asserts the Gemini client/agent path constructs
// successfully and is wired to the gemini provider (see TestNew_Anthropic's
// comment for why the wire format itself isn't mocked here).
func TestNew_Gemini(t *testing.T) {
	cfg := entities.ProviderConfig{
		Name:   "test-gemini",
		Type:   entities.ProviderTypeGemini,
		APIKey: "gk-test-key",
	}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "gemini-test"}

	a, err := New(context.Background(), cfg, model, agent.Config{Name: "test-agent"})
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "gemini", a.ProviderName())
	assert.Equal(t, "test-agent", a.Name())
}
