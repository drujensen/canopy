package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/config"
	"github.com/drujensen/canopy/internal/impl/modelsdev"
)

// TestMergeNewProviders_AdditiveNeverClobbersExisting is the specific test
// requested for --refresh-providers' contract (Design §4 addendum): a
// provider already present in the destination file — including one the
// user may have hand-edited since Canopy first wrote it — must never be
// touched by a later merge, even if the newly-detected version of that same
// provider (by Name) carries different data.
func TestMergeNewProviders_AdditiveNeverClobbersExisting(t *testing.T) {
	dst := &config.ProvidersFile{
		Providers: []entities.ProviderConfig{
			{
				Name:      "openai",
				Type:      entities.ProviderTypeOpenAI,
				APIKeyEnv: "OPENAI_API_KEY",
				// Simulates a user hand-edit: a literal key pasted in,
				// diverging from what fresh detection would produce.
				APIKey: "sk-hand-edited-by-user",
			},
		},
		Models: []entities.ModelConfig{
			{Name: "openai", Provider: "openai", ModelName: "gpt-4o-mini-hand-edited"},
		},
	}

	detected := config.ProvidersFile{
		Providers: []entities.ProviderConfig{
			// Same Name as the existing entry, but different data — must be
			// ignored entirely, not merged/overwritten.
			{Name: "openai", Type: entities.ProviderTypeOpenAI, APIKeyEnv: "OPENAI_API_KEY"},
			// A genuinely new provider — must be added.
			{Name: "google", Type: entities.ProviderTypeGemini, APIKeyEnv: "GEMINI_API_KEY"},
		},
		Models: []entities.ModelConfig{
			{Name: "openai", Provider: "openai", ModelName: "gpt-5-fresh-from-catalog"},
			{Name: "google", Provider: "google", ModelName: "gemini-flash"},
		},
	}

	added := mergeNewProviders(dst, detected)

	assert.Equal(t, []string{"google"}, added)
	require.Len(t, dst.Providers, 2)
	require.Len(t, dst.Models, 2)

	// The pre-existing openai entry is byte-for-byte untouched.
	assert.Equal(t, "sk-hand-edited-by-user", dst.Providers[0].APIKey)
	assert.Equal(t, "gpt-4o-mini-hand-edited", dst.Models[0].ModelName)

	// The new google entry was appended.
	assert.Equal(t, "google", dst.Providers[1].Name)
	assert.Equal(t, "gemini-flash", dst.Models[1].ModelName)
}

func TestMergeNewProviders_NothingNewIsANoOp(t *testing.T) {
	dst := &config.ProvidersFile{
		Providers: []entities.ProviderConfig{{Name: "openai", Type: entities.ProviderTypeOpenAI}},
		Models:    []entities.ModelConfig{{Name: "openai", Provider: "openai", ModelName: "gpt-4o"}},
	}
	detected := config.ProvidersFile{
		Providers: []entities.ProviderConfig{{Name: "openai", Type: entities.ProviderTypeOpenAI, APIKeyEnv: "OPENAI_API_KEY"}},
		Models:    []entities.ModelConfig{{Name: "openai", Provider: "openai", ModelName: "different-model"}},
	}

	added := mergeNewProviders(dst, detected)

	assert.Empty(t, added)
	require.Len(t, dst.Providers, 1)
	require.Len(t, dst.Models, 1)
	assert.Equal(t, "gpt-4o", dst.Models[0].ModelName)
}

func TestDescribeProviders_PairsNameWithEnvVar(t *testing.T) {
	providers := []entities.ProviderConfig{
		{Name: "openai", APIKeyEnv: "OPENAI_API_KEY"},
		{Name: "google", APIKeyEnv: "GEMINI_API_KEY"},
		{Name: "local", APIKeyEnv: ""},
	}

	got := describeProviders(providers, []string{"google", "openai", "local"})

	assert.Equal(t, []string{
		"google (from GEMINI_API_KEY)",
		"openai (from OPENAI_API_KEY)",
		"local",
	}, got)
}

func TestKnownEnvVarNames_DeduplicatedAndSorted(t *testing.T) {
	catalog := &modelsdev.Catalog{
		"openai":    {ID: "openai", Env: []string{"OPENAI_API_KEY"}},
		"google":    {ID: "google", Env: []string{"GOOGLE_API_KEY", "GEMINI_API_KEY"}},
		"anthropic": {ID: "anthropic", Env: []string{"ANTHROPIC_API_KEY"}},
		"duplicate": {ID: "duplicate", Env: []string{"OPENAI_API_KEY"}}, // shares a var with openai
	}

	got := knownEnvVarNames(catalog)

	assert.Equal(t, []string{
		"ANTHROPIC_API_KEY",
		"GEMINI_API_KEY",
		"GOOGLE_API_KEY",
		"OPENAI_API_KEY",
	}, got)
}
