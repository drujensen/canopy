package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// runAndCaptureErr drives a single turn against server and returns
// whatever error the SDK surfaces — the real path FormatError's callers
// get their error from (c.statusErr, chat.go), not a hand-built fake.
func runAndCaptureErr(t *testing.T, cfg entities.ProviderConfig, model entities.ModelConfig) error {
	t.Helper()
	a, err := New(context.Background(), cfg, model, agent.Config{Name: "test-agent"})
	require.NoError(t, err)
	_, runErr := a.RunText(context.Background(), "hi").Collect()
	require.Error(t, runErr)
	return runErr
}

// TestFormatError_OpenAICompatible_RealIncidentBodies covers the two exact
// response bodies this was added for — OpenCode's Go-tier 403 and its
// free-tier 429 — both of which, before this, rendered as a single
// terminal-width line of "agent service: running chat ...: POST
// \"https://...\": 403 Forbidden {...}" with the actual human message
// (at the very end) truncated off by chat.go's renderStatusErrLine. Both
// go through openai-go for any OpenAI-compatible provider, so a Groq
// stand-in server exercises the exact same code path opencode/opencode-go
// hit live.
func TestFormatError_OpenAICompatible_RealIncidentBodies(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       map[string]any
		want       string
	}{
		{
			name:       "opencode-go 403",
			statusCode: http.StatusForbidden,
			body: map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "server_error",
					"message": "Upstream request failed: An active OpenCode Go subscription is required to use Go models.",
				},
			},
			want: "403 - Upstream request failed: An active OpenCode Go subscription is required to use Go models.",
		},
		{
			name:       "opencode free-tier 429",
			statusCode: http.StatusTooManyRequests,
			body: map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "FreeUsageLimitError",
					"message": "Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go",
				},
			},
			want: "429 - Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("x-should-retry", "false") // isolate formatting from the retry-cap fix's own behavior
				w.WriteHeader(tc.statusCode)
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			defer server.Close()

			cfg := entities.ProviderConfig{Name: "test-opencode", Type: entities.ProviderType("opencode-go"), APIKey: "k", BaseURL: server.URL + "/v1"}
			model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "some-model"}

			runErr := runAndCaptureErr(t, cfg, model)
			assert.Equal(t, tc.want, FormatError(runErr))
		})
	}
}

// TestFormatError_Anthropic covers the one SDK whose *anthropic.Error
// doesn't parse out a Message field itself (unlike openai.Error and
// genai.APIError) — FormatError has to reach into RawJSON() one level
// deeper to find it.
func TestFormatError_Anthropic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "invalid_request_error",
				"message": "max_tokens is too large for this model",
			},
		})
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{Name: "test-anthropic", Type: entities.ProviderTypeAnthropic, APIKey: "k", BaseURL: server.URL}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "claude-test"}

	runErr := runAndCaptureErr(t, cfg, model)
	assert.Equal(t, "400 - max_tokens is too large for this model", FormatError(runErr))
}

// TestFormatError_FallsBackForUnrecognizedError guards the "no regression"
// half: an error FormatError doesn't recognize at all (not any provider
// SDK's error type, not wrapping one) renders exactly as err.Error()
// always did.
func TestFormatError_FallsBackForUnrecognizedError(t *testing.T) {
	plain := errors.New("agent service: something unrelated went wrong")
	assert.Equal(t, plain.Error(), FormatError(plain))
}

// TestFormatError_NilIsEmpty guards the one input FormatError must never
// panic on — chat.go only calls this when c.statusErr != nil in practice,
// but the function's own contract should still hold for nil.
func TestFormatError_NilIsEmpty(t *testing.T) {
	assert.Equal(t, "", FormatError(nil))
}

// TestFormatError_SurvivesMultipleWrapLayers asserts errors.As still finds
// the underlying *openai.Error through the same multi-layer %w wrapping
// AgentService actually applies (e.g. "agent service: running chat %q:
// %w") — FormatError would be useless in practice if it only worked on an
// unwrapped SDK error, since that's never what c.statusErr actually holds.
func TestFormatError_SurvivesMultipleWrapLayers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-should-retry", "false")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "nope", "type": "server_error"},
		})
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{Name: "test-opencode-go", Type: entities.ProviderType("opencode-go"), APIKey: "k", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "some-model"}
	runErr := runAndCaptureErr(t, cfg, model)

	wrapped := fmt.Errorf("agent service: running chat %q: %w", "chat-1", runErr)
	wrapped = fmt.Errorf("tui: starting turn: %w", wrapped)

	assert.Equal(t, "403 - nope", FormatError(wrapped))
}
