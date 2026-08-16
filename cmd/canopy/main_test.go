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

// TestUpdateExistingModelCosts_RefreshesCostOnAlreadyPresentModel is the
// specific behavior requested against --refresh-providers' original
// "existing entries are never touched" contract: cost specifically must be
// refreshed on a model whose provider was already configured (so
// mergeNewProviders would have skipped it entirely), while every other
// field of that same model entry stays exactly as the user left it.
func TestUpdateExistingModelCosts_RefreshesCostOnAlreadyPresentModel(t *testing.T) {
	dst := &config.ProvidersFile{
		Providers: []entities.ProviderConfig{{Name: "openai", Type: entities.ProviderTypeOpenAI}},
		Models: []entities.ModelConfig{
			{
				Name: "my-gpt", Provider: "openai", ModelName: "gpt-5",
				// Simulates stale cost captured before a price change, plus a
				// user-chosen display Name and hand-set ContextWindowTokens
				// that must both survive untouched.
				InputCostPerMillionTokens: 1, OutputCostPerMillionTokens: 5,
				ContextWindowTokens: 999999,
			},
		},
	}
	detected := config.ProvidersFile{
		Models: []entities.ModelConfig{
			// Same (Provider, ModelName) as dst's entry, but under detection's
			// own auto-generated Name ("openai/gpt-5") — proves matching is by
			// (Provider, ModelName), not Name, since a user is free to rename
			// the display Name after Canopy first wrote it.
			{Name: "openai/gpt-5", Provider: "openai", ModelName: "gpt-5", InputCostPerMillionTokens: 2.5, OutputCostPerMillionTokens: 10},
		},
	}

	updated := updateExistingModelCosts(dst, detected)

	assert.Equal(t, 1, updated)
	require.Len(t, dst.Models, 1)
	assert.Equal(t, 2.5, dst.Models[0].InputCostPerMillionTokens)
	assert.Equal(t, 10.0, dst.Models[0].OutputCostPerMillionTokens)
	// Everything else on the entry is untouched.
	assert.Equal(t, "my-gpt", dst.Models[0].Name)
	assert.Equal(t, 999999, dst.Models[0].ContextWindowTokens)
}

func TestUpdateExistingModelCosts_NoMatchIsUntouchedNotZeroed(t *testing.T) {
	dst := &config.ProvidersFile{
		Models: []entities.ModelConfig{
			// A self-hosted model models.dev has no pricing for at all.
			{Name: "ornith", Provider: "drujensen", ModelName: "ornith:35b", InputCostPerMillionTokens: 0, OutputCostPerMillionTokens: 0},
			// A model whose provider simply isn't in this detection run (e.g.
			// its env var is currently unset).
			{Name: "old-model", Provider: "anthropic", ModelName: "claude-legacy", InputCostPerMillionTokens: 3, OutputCostPerMillionTokens: 15},
		},
	}
	detected := config.ProvidersFile{
		Models: []entities.ModelConfig{
			{Name: "openai/gpt-5", Provider: "openai", ModelName: "gpt-5", InputCostPerMillionTokens: 2.5, OutputCostPerMillionTokens: 10},
		},
	}

	updated := updateExistingModelCosts(dst, detected)

	assert.Zero(t, updated)
	assert.Zero(t, dst.Models[0].InputCostPerMillionTokens, "no catalog data for a self-hosted model must never fabricate a cost")
	assert.Equal(t, 3.0, dst.Models[1].InputCostPerMillionTokens, "a model absent from this detection run must keep its previously-known cost, not have it cleared")
}

func TestUpdateExistingModelCosts_UnchangedValueDoesNotCountAsUpdated(t *testing.T) {
	dst := &config.ProvidersFile{
		Models: []entities.ModelConfig{
			{Name: "my-gpt", Provider: "openai", ModelName: "gpt-5", InputCostPerMillionTokens: 2.5, OutputCostPerMillionTokens: 10},
		},
	}
	detected := config.ProvidersFile{
		Models: []entities.ModelConfig{
			{Name: "openai/gpt-5", Provider: "openai", ModelName: "gpt-5", InputCostPerMillionTokens: 2.5, OutputCostPerMillionTokens: 10},
		},
	}

	updated := updateExistingModelCosts(dst, detected)
	assert.Zero(t, updated, "identical cost data must not be reported as a change")
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
