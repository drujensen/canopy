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

// ---------------------------------------------------------------------
// normalizeOllamaBaseURL
// ---------------------------------------------------------------------

func TestNormalizeOllamaBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "bare host gets https scheme and /v1 appended",
			in:   "ai.drujensen.com",
			want: "https://ai.drujensen.com/v1",
		},
		{
			name: "bare host with port",
			in:   "localhost:11434",
			want: "https://localhost:11434/v1",
		},
		{
			name: "explicit http scheme preserved",
			in:   "http://localhost:11434",
			want: "http://localhost:11434/v1",
		},
		{
			name: "explicit https scheme preserved",
			in:   "https://ai.drujensen.com",
			want: "https://ai.drujensen.com/v1",
		},
		{
			name: "trailing slash trimmed before appending /v1",
			in:   "https://ai.drujensen.com/",
			want: "https://ai.drujensen.com/v1",
		},
		{
			name: "existing /v1 suffix left alone, not doubled",
			in:   "https://ai.drujensen.com/v1",
			want: "https://ai.drujensen.com/v1",
		},
		{
			name: "existing /v1 suffix with trailing slash left alone",
			in:   "https://ai.drujensen.com/v1/",
			want: "https://ai.drujensen.com/v1",
		},
		{
			name: "whitespace trimmed",
			in:   "  ai.drujensen.com  ",
			want: "https://ai.drujensen.com/v1",
		},
		{
			name:    "empty string errors",
			in:      "",
			wantErr: true,
		},
		{
			name:    "whitespace-only string errors",
			in:      "   ",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeOllamaBaseURL(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ---------------------------------------------------------------------
// newOllama / New dispatch
// ---------------------------------------------------------------------

func TestNew_Ollama_MissingBaseURL(t *testing.T) {
	cfg := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOllama, APIKey: "k"}
	model := entities.ModelConfig{Name: "m", Provider: "p", ModelName: "llama3"}
	a, err := New(context.Background(), cfg, model, agent.Config{Name: "a"})
	require.Error(t, err)
	assert.Nil(t, a)
	assert.Contains(t, err.Error(), "base_url")
}

func TestNew_Ollama_InvalidBaseURL(t *testing.T) {
	cfg := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOllama, APIKey: "k", BaseURL: "://not a url"}
	model := entities.ModelConfig{Name: "m", Provider: "p", ModelName: "llama3"}
	a, err := New(context.Background(), cfg, model, agent.Config{Name: "a"})
	require.Error(t, err)
	assert.Nil(t, a)
}

// TestNew_Ollama_AppendsV1AndDispatchesCorrectly asserts that a BaseURL with
// no "/v1" suffix (the ergonomic form this whole file exists for) still ends
// up hitting "/v1/chat/completions" on the wire, and that the request is
// authenticated the same way the other OpenAI-compatible paths are.
func TestNew_Ollama_AppendsV1AndDispatchesCorrectly(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("hello from ollama"))
	}))
	defer server.Close()

	// server.URL already has a scheme (http://127.0.0.1:PORT) but
	// deliberately no "/v1" suffix, exercising normalizeOllamaBaseURL's
	// append behavior end to end.
	cfg := entities.ProviderConfig{
		Name:    "drujensen",
		Type:    entities.ProviderTypeOllama,
		APIKey:  "test-key",
		BaseURL: server.URL,
	}
	model := entities.ModelConfig{Name: "ornith", Provider: cfg.Name, ModelName: "ornith:35b"}

	a, err := New(context.Background(), cfg, model, agent.Config{Name: "test-agent"})
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "openai", a.ProviderName())

	resp, err := a.RunText(context.Background(), "hi").Collect()
	require.NoError(t, err)
	assert.Equal(t, "hello from ollama", resp.String())

	assert.Equal(t, "/v1/chat/completions", gotPath)
	assert.Equal(t, "Bearer test-key", gotAuth)
	require.NotNil(t, gotBody)
	assert.Equal(t, "ornith:35b", gotBody["model"])
}
