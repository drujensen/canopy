package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/modelsdev"
)

// fixtureCatalog is a small synthetic catalog exercising the cases
// DetectProviders cares about: a native type (openai) with a recent
// tool-call model and an older non-tool-call model; a native type (google)
// whose only tool-call model should win over a newer non-tool-call one; a
// generic/non-native provider (deepseek) with a BaseURL, detectable via the
// generalized fallback dispatch; and a generic provider with no BaseURL at
// all, which must never be detected regardless of its env var.
func fixtureCatalog() *modelsdev.Catalog {
	return &modelsdev.Catalog{
		"openai": {
			ID:   "openai",
			Name: "OpenAI",
			Env:  []string{"OPENAI_API_KEY"},
			Models: map[string]modelsdev.ModelData{
				"gpt-4o": {
					ID: "gpt-4o", ToolCall: true, ReleaseDate: "2024-05-13",
					Limit: modelsdev.LimitData{Context: 128000},
				},
				"gpt-5": {
					ID: "gpt-5", ToolCall: true, ReleaseDate: "2025-01-01",
					Limit: modelsdev.LimitData{Context: 256000},
				},
				"whisper": {
					ID: "whisper", ToolCall: false, ReleaseDate: "2026-01-01",
					Limit: modelsdev.LimitData{Context: 0},
				},
			},
		},
		"google": {
			ID:   "google",
			Name: "Google",
			Env:  []string{"GOOGLE_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY", "GEMINI_API_KEY"},
			Models: map[string]modelsdev.ModelData{
				"gemini-tts": {
					ID: "gemini-tts", ToolCall: false, ReleaseDate: "2026-04-15",
					Limit: modelsdev.LimitData{Context: 8192},
				},
				"gemini-flash": {
					ID: "gemini-flash", ToolCall: true, ReleaseDate: "2025-06-01",
					Limit: modelsdev.LimitData{Context: 1000000},
				},
			},
		},
		"deepseek": {
			ID:      "deepseek",
			Name:    "DeepSeek",
			Env:     []string{"DEEPSEEK_API_KEY"},
			BaseURL: "https://api.deepseek.com",
			Models: map[string]modelsdev.ModelData{
				"deepseek-chat": {
					ID: "deepseek-chat", ToolCall: true, ReleaseDate: "2025-01-01",
					Limit: modelsdev.LimitData{Context: 64000},
				},
			},
		},
		"nobase": {
			ID:   "nobase",
			Name: "No Base URL Provider",
			Env:  []string{"NOBASE_API_KEY"},
			// No BaseURL — must never be detected, even with its env var set.
			Models: map[string]modelsdev.ModelData{
				"nobase-model": {ID: "nobase-model", ToolCall: true, ReleaseDate: "2025-01-01"},
			},
		},
		"notooluse": {
			ID:   "notooluse",
			Name: "No Tool Call Provider",
			Env:  []string{"NOTOOLUSE_API_KEY"},
			Models: map[string]modelsdev.ModelData{
				"chat-only": {ID: "chat-only", ToolCall: false, ReleaseDate: "2025-01-01"},
			},
		},
		"unset": {
			ID:   "unset",
			Name: "Never Configured Provider",
			Env:  []string{"UNSET_PROVIDER_API_KEY"},
			Models: map[string]modelsdev.ModelData{
				"m": {ID: "m", ToolCall: true, ReleaseDate: "2025-01-01"},
			},
		},
	}
}

func TestDetectProviders_NativeTypeMostRecentToolCallModel(t *testing.T) {
	environ := []string{"OPENAI_API_KEY=sk-real-key"}

	file, detected, _ := DetectProviders(fixtureCatalog(), environ)

	require.Contains(t, detected, "openai")
	var provider entities.ProviderConfig
	for _, p := range file.Providers {
		if p.Name == "openai" {
			provider = p
		}
	}
	assert.Equal(t, entities.ProviderTypeOpenAI, provider.Type)
	assert.Equal(t, "OPENAI_API_KEY", provider.APIKeyEnv)
	assert.Empty(t, provider.APIKey, "must never carry the literal key")

	var model entities.ModelConfig
	for _, m := range file.Models {
		if m.Provider == "openai" {
			model = m
		}
	}
	// gpt-5 is the most recent tool-call-capable model (whisper is newer
	// but not tool-call-capable, and must be skipped).
	assert.Equal(t, "gpt-5", model.ModelName)
	assert.Equal(t, 256000, model.ContextWindowTokens)
}

func TestDetectProviders_GoogleMapsToGeminiType(t *testing.T) {
	environ := []string{"GEMINI_API_KEY=real-gemini-key"}

	file, detected, _ := DetectProviders(fixtureCatalog(), environ)

	require.Contains(t, detected, "google")
	var provider entities.ProviderConfig
	for _, p := range file.Providers {
		if p.Name == "google" {
			provider = p
		}
	}
	assert.Equal(t, entities.ProviderTypeGemini, provider.Type)
	assert.Equal(t, "GEMINI_API_KEY", provider.APIKeyEnv)

	var model entities.ModelConfig
	for _, m := range file.Models {
		if m.Provider == "google" {
			model = m
		}
	}
	// gemini-tts is newer but not tool-call-capable; gemini-flash must win.
	assert.Equal(t, "gemini-flash", model.ModelName)
}

func TestDetectProviders_AnthropicMapsToAnthropicType(t *testing.T) {
	catalog := &modelsdev.Catalog{
		"anthropic": {
			ID:   "anthropic",
			Name: "Anthropic",
			Env:  []string{"ANTHROPIC_API_KEY"},
			Models: map[string]modelsdev.ModelData{
				"claude": {ID: "claude", ToolCall: true, ReleaseDate: "2025-01-01"},
			},
		},
	}
	file, detected, _ := DetectProviders(catalog, []string{"ANTHROPIC_API_KEY=sk-ant"})
	require.Contains(t, detected, "anthropic")
	assert.Equal(t, entities.ProviderTypeAnthropic, file.Providers[0].Type)
}

func TestDetectProviders_GenericProviderUsesCatalogIDAsType(t *testing.T) {
	environ := []string{"DEEPSEEK_API_KEY=ds-key"}

	file, detected, _ := DetectProviders(fixtureCatalog(), environ)

	require.Contains(t, detected, "deepseek")
	var provider entities.ProviderConfig
	for _, p := range file.Providers {
		if p.Name == "deepseek" {
			provider = p
		}
	}
	assert.Equal(t, entities.ProviderType("deepseek"), provider.Type)
	assert.Equal(t, "https://api.deepseek.com", provider.BaseURL)
}

func TestDetectProviders_EnvVarNotSetIsSkipped(t *testing.T) {
	// None of the fixture's env vars are set.
	file, detected, _ := DetectProviders(fixtureCatalog(), []string{"UNRELATED=1"})

	assert.Empty(t, detected)
	assert.Empty(t, file.Providers)
	assert.Empty(t, file.Models)
}

func TestDetectProviders_NoToolCallModelsIsSkippedNoPanic(t *testing.T) {
	environ := []string{"NOTOOLUSE_API_KEY=set"}

	require.NotPanics(t, func() {
		file, detected, skipReasons := DetectProviders(fixtureCatalog(), environ)
		assert.NotContains(t, detected, "notooluse")
		assert.Empty(t, file.Providers)
		assert.NotEmpty(t, skipReasons)
	})
}

func TestDetectProviders_NoBaseURLNonNativeIsSkipped(t *testing.T) {
	environ := []string{"NOBASE_API_KEY=set"}

	file, detected, skipReasons := DetectProviders(fixtureCatalog(), environ)

	assert.NotContains(t, detected, "nobase")
	assert.Empty(t, file.Providers)
	assert.NotEmpty(t, skipReasons)
}

func TestDetectProviders_NilCatalog(t *testing.T) {
	file, detected, skipReasons := DetectProviders(nil, []string{"OPENAI_API_KEY=x"})
	assert.Empty(t, file.Providers)
	assert.Empty(t, detected)
	assert.Empty(t, skipReasons)
}

func TestDetectProviders_EmptyEnvValueDoesNotCount(t *testing.T) {
	// A var that's present in the environ slice but set to "" must not
	// count as "set" — matches shell semantics of an unset/blank key.
	file, detected, _ := DetectProviders(fixtureCatalog(), []string{"OPENAI_API_KEY="})
	assert.Empty(t, detected)
	assert.Empty(t, file.Providers)
}
