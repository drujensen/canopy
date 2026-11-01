package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// TestNew_OpenCode_SendsSessionHeader covers both opencode-family provider
// ids ("opencode", the pay-as-you-go endpoint, and "opencode-go", the Go
// subscription tier) — neither is a named entities.ProviderType const, so
// both only reach newOpenCode via New's default-branch
// isOpenCodeProviderType check. Asserts the request actually carries
// "x-opencode-session" set to whatever WithChatID attached to ctx — the
// real header OpenCode's own docs ask every client to send, and the exact
// one a live call to https://opencode.ai/inference/go/openai/v1 without it
// returned a hard 400 ("MissingSessionID ... cannot be routed
// efficiently") for.
func TestNew_OpenCode_SendsSessionHeader(t *testing.T) {
	for _, providerType := range []string{"opencode", "opencode-go"} {
		t.Run(providerType, func(t *testing.T) {
			var gotSessionHeader string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotSessionHeader = r.Header.Get("x-opencode-session")
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(chatCompletionResponse("hello"))
			}))
			defer server.Close()

			cfg := entities.ProviderConfig{
				Name:    "test-" + providerType,
				Type:    entities.ProviderType(providerType),
				APIKey:  "k",
				BaseURL: server.URL + "/v1",
			}
			model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "beam-free"}

			ctx := WithChatID(context.Background(), "chat-abc-123")
			a, err := New(ctx, cfg, model, agent.Config{Name: "test-agent"})
			require.NoError(t, err)

			resp, err := a.RunText(ctx, "hi").Collect()
			require.NoError(t, err)
			assert.Equal(t, "hello", resp.String())
			assert.Equal(t, "chat-abc-123", gotSessionHeader)
		})
	}
}

// TestNew_OpenCode_NoHeaderWithoutChatID asserts a caller that never
// attaches a chat ID (ctx has no WithChatID) doesn't send an empty/junk
// "x-opencode-session" header at all — newOpenCode only adds the option
// when chatID != "".
func TestNew_OpenCode_NoHeaderWithoutChatID(t *testing.T) {
	var sawHeader bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawHeader = r.Header["X-Opencode-Session"]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("hello"))
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{Name: "test-opencode", Type: entities.ProviderType("opencode"), APIKey: "k", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "beam-free"}

	a, err := New(context.Background(), cfg, model, agent.Config{Name: "test-agent"})
	require.NoError(t, err)

	_, err = a.RunText(context.Background(), "hi").Collect()
	require.NoError(t, err)
	assert.False(t, sawHeader, "must not send x-opencode-session at all when no chat ID was ever attached")
}

// TestNew_NonOpenCodeProvider_NeverSendsSessionHeader guards the scoping:
// a WithChatID value on ctx must never leak the header onto an unrelated
// OpenAI-compatible provider (e.g. Groq) just because one happened to be
// present — isOpenCodeProviderType is what gates this, not the mere
// presence of a chat ID.
func TestNew_NonOpenCodeProvider_NeverSendsSessionHeader(t *testing.T) {
	var sawHeader bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawHeader = r.Header["X-Opencode-Session"]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("hello"))
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{Name: "test-groq", Type: entities.ProviderTypeGroq, APIKey: "k", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: cfg.Name, ModelName: "some-model"}

	ctx := WithChatID(context.Background(), "chat-abc-123")
	a, err := New(ctx, cfg, model, agent.Config{Name: "test-agent"})
	require.NoError(t, err)

	_, err = a.RunText(ctx, "hi").Collect()
	require.NoError(t, err)
	assert.False(t, sawHeader, "a chat ID on ctx must not leak onto a non-opencode provider")
}
