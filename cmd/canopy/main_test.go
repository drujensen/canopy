package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	"github.com/drujensen/canopy/internal/impl/config"
	"github.com/drujensen/canopy/internal/impl/modelsdev"
)

// TestComputeStartAgent_LastUsedStillExists is computeStartAgent's common
// case (post-v0.1.0 addendum, Design §5's addendum): once at least one
// prior session picked an agent that's still configured, auto-resume it.
func TestComputeStartAgent_LastUsedStillExists(t *testing.T) {
	agents := map[string]agentsource.AgentDefinition{
		"assistant": {Name: "assistant"},
		"general":   {Name: "general"},
	}
	assert.Equal(t, "assistant", computeStartAgent(agents, "assistant"))
}

// TestComputeStartAgent_LastUsedRemoved_FallsBackToGeneral covers the
// explicitly requested fallback: the last-used agent no longer exists, but
// "general" (agentsource.WriteDefaults' own default agent name) does.
func TestComputeStartAgent_LastUsedRemoved_FallsBackToGeneral(t *testing.T) {
	agents := map[string]agentsource.AgentDefinition{
		"general": {Name: "general"},
	}
	assert.Equal(t, "general", computeStartAgent(agents, "removed-agent"))
}

// TestComputeStartAgent_NoLastUsed_FallsBackToGeneral covers a brand-new
// install with no last_agent.json yet (lastUsed == "") but a "general"
// agent already configured (agentsource.WriteDefaults' zero-config path).
func TestComputeStartAgent_NoLastUsed_FallsBackToGeneral(t *testing.T) {
	agents := map[string]agentsource.AgentDefinition{
		"general": {Name: "general"},
	}
	assert.Equal(t, "general", computeStartAgent(agents, ""))
}

// TestComputeStartAgent_NoMatchAndNoGeneral_FallsBackToPicker is the final
// fallback: neither the last-used agent nor "general" exist, so there's no
// sensible single default to guess — computeStartAgent returns "", telling
// the caller to fall through to showing the picker exactly as before this
// feature existed.
func TestComputeStartAgent_NoMatchAndNoGeneral_FallsBackToPicker(t *testing.T) {
	agents := map[string]agentsource.AgentDefinition{
		"reviewer": {Name: "reviewer"},
		"writer":   {Name: "writer"},
	}
	assert.Equal(t, "", computeStartAgent(agents, "removed-agent"))
	assert.Equal(t, "", computeStartAgent(agents, ""))
}

// TestComputeStartAgent_EmptyAgentsMap covers the (defensive) zero-agents
// case — must not panic on a nil/empty map.
func TestComputeStartAgent_EmptyAgentsMap(t *testing.T) {
	assert.Equal(t, "", computeStartAgent(nil, "assistant"))
	assert.Equal(t, "", computeStartAgent(map[string]agentsource.AgentDefinition{}, ""))
}

// TestComputeDefaultModel_LastUsedStillExists is a regression test for a
// real reported bug: switching models via ctrl+o only ever set that one
// chat's Chat.ModelOverride, so starting a fresh chat/session afterward
// always fell back to providersFile.Models[0] regardless of what the user
// had actually switched to. computeDefaultModel's common case: the
// last-used model is still configured, so it's returned instead of
// models[0].
func TestComputeDefaultModel_LastUsedStillExists(t *testing.T) {
	models := []entities.ModelConfig{
		{Name: "drujensen/ornith"},
		{Name: "openai/gpt-5"},
		{Name: "anthropic/claude-sonnet-5"},
	}
	assert.Equal(t, "anthropic/claude-sonnet-5", computeDefaultModel(models, "anthropic/claude-sonnet-5"))
}

// TestComputeDefaultModel_LastUsedRemoved_FallsBackToFirst covers a
// last-used model that's since disappeared from the configured list (e.g. a
// provider removed, or --refresh-providers pruning a stale entry) — falls
// back to models[0], not an error or a dangling reference to a model that
// no longer resolves to any ProviderConfig.
func TestComputeDefaultModel_LastUsedRemoved_FallsBackToFirst(t *testing.T) {
	models := []entities.ModelConfig{
		{Name: "drujensen/ornith"},
		{Name: "openai/gpt-5"},
	}
	assert.Equal(t, "drujensen/ornith", computeDefaultModel(models, "removed-model"))
}

// TestComputeDefaultModel_NoLastUsed_FallsBackToFirst covers a brand-new
// install with no last_model.json yet (lastUsed == "") — the exact
// pre-addendum behavior, preserved as the fallback.
func TestComputeDefaultModel_NoLastUsed_FallsBackToFirst(t *testing.T) {
	models := []entities.ModelConfig{
		{Name: "drujensen/ornith"},
		{Name: "openai/gpt-5"},
	}
	assert.Equal(t, "drujensen/ornith", computeDefaultModel(models, ""))
}

// TestDirMissingOrEmpty_MissingDirectory is the zero-config first-run
// case: the directory (e.g. ~/.canopy/agents or ~/.canopy/skills) doesn't
// exist yet at all.
func TestDirMissingOrEmpty_MissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agents")
	needs, err := dirMissingOrEmpty(dir)
	require.NoError(t, err)
	assert.True(t, needs)
}

// TestDirMissingOrEmpty_EmptyDirectory covers a directory that exists (e.g.
// created but never populated) with no entries in it yet.
func TestDirMissingOrEmpty_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	needs, err := dirMissingOrEmpty(dir)
	require.NoError(t, err)
	assert.True(t, needs)
}

// TestDirMissingOrEmpty_HasAtLeastOneEntry is the "don't touch it" case
// this whole check exists for: as long as the directory has at least one
// entry — whether it's one of Canopy's own defaults, a user-authored file,
// or even an unrelated one — defaults must not be (re)written.
func TestDirMissingOrEmpty_HasAtLeastOneEntry(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "general.md"), []byte("---\nname: general\n---\n"), 0o644))

	needs, err := dirMissingOrEmpty(dir)
	require.NoError(t, err)
	assert.False(t, needs)
}

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

// TestMergeNewModelsForExistingProviders_AddsMissingModelsForKnownProvider
// is the specific bug this function exists to fix: a provider configured
// with only one model (e.g. an early "deepseek" entry predating
// DetectProviders' "list every tool-call-capable model, not just one"
// addendum) must pick up the catalog's other models for that same provider
// on a later --refresh-providers run, even though mergeNewProviders itself
// skips the provider entirely as "not new."
func TestMergeNewModelsForExistingProviders_AddsMissingModelsForKnownProvider(t *testing.T) {
	dst := &config.ProvidersFile{
		Providers: []entities.ProviderConfig{{Name: "deepseek", Type: entities.ProviderTypeDeepSeek}},
		Models: []entities.ModelConfig{
			{Name: "deepseek", Provider: "deepseek", ModelName: "deepseek-v4-pro"},
		},
	}
	detected := config.ProvidersFile{
		Models: []entities.ModelConfig{
			// Same model as the existing entry (different auto-generated
			// Name) — must not be duplicated.
			{Name: "deepseek/deepseek-v4-pro", Provider: "deepseek", ModelName: "deepseek-v4-pro", InputCostPerMillionTokens: 1},
			// Genuinely new models the catalog now lists for this provider.
			{Name: "deepseek/deepseek-v4-flash", Provider: "deepseek", ModelName: "deepseek-v4-flash"},
			{Name: "deepseek/deepseek-chat", Provider: "deepseek", ModelName: "deepseek-chat"},
			// A different provider entirely — must be ignored (that's
			// mergeNewProviders' job, and this provider isn't in dst at all).
			{Name: "openai/gpt-5", Provider: "openai", ModelName: "gpt-5"},
		},
	}

	added := mergeNewModelsForExistingProviders(dst, detected)

	require.Len(t, added, 2)
	addedNames := []string{added[0].Name, added[1].Name}
	assert.ElementsMatch(t, []string{"deepseek/deepseek-v4-flash", "deepseek/deepseek-chat"}, addedNames)

	require.Len(t, dst.Models, 3, "the original model plus exactly the two new ones")
	assert.Equal(t, "deepseek", dst.Models[0].Name, "the pre-existing model entry must be untouched, not replaced by the catalog's differently-named duplicate")
}

func TestMergeNewModelsForExistingProviders_IgnoresProvidersNotInDst(t *testing.T) {
	dst := &config.ProvidersFile{
		Providers: []entities.ProviderConfig{{Name: "openai", Type: entities.ProviderTypeOpenAI}},
		Models:    []entities.ModelConfig{{Name: "openai", Provider: "openai", ModelName: "gpt-4o"}},
	}
	detected := config.ProvidersFile{
		Models: []entities.ModelConfig{
			{Name: "anthropic/claude", Provider: "anthropic", ModelName: "claude-opus"},
		},
	}

	added := mergeNewModelsForExistingProviders(dst, detected)

	assert.Empty(t, added, "a provider not already in dst is mergeNewProviders' job, not this one's")
	assert.Len(t, dst.Models, 1)
}

func TestMergeNewModelsForExistingProviders_NothingNewIsANoOp(t *testing.T) {
	dst := &config.ProvidersFile{
		Providers: []entities.ProviderConfig{{Name: "openai", Type: entities.ProviderTypeOpenAI}},
		Models:    []entities.ModelConfig{{Name: "openai", Provider: "openai", ModelName: "gpt-4o"}},
	}
	detected := config.ProvidersFile{
		Models: []entities.ModelConfig{
			{Name: "openai/gpt-4o", Provider: "openai", ModelName: "gpt-4o"},
		},
	}

	added := mergeNewModelsForExistingProviders(dst, detected)

	assert.Empty(t, added)
	assert.Len(t, dst.Models, 1)
}

// ---------------------------------------------------------------------
// removeStaleModelsForRedetectedProviders
// ---------------------------------------------------------------------

// TestRemoveStaleModelsForRedetectedProviders_RemovesModelTheCatalogDropped
// is the "sync, not just add" behavior requested: a model the catalog no
// longer lists for a provider that *was* re-detected this run must be
// removed.
func TestRemoveStaleModelsForRedetectedProviders_RemovesModelTheCatalogDropped(t *testing.T) {
	dst := &config.ProvidersFile{
		Providers: []entities.ProviderConfig{{Name: "openai", Type: entities.ProviderTypeOpenAI}},
		Models: []entities.ModelConfig{
			{Name: "openai/gpt-4o", Provider: "openai", ModelName: "gpt-4o"},
			{Name: "openai/gpt-3-deprecated", Provider: "openai", ModelName: "gpt-3-deprecated"},
		},
	}
	detected := config.ProvidersFile{
		Providers: []entities.ProviderConfig{{Name: "openai", Type: entities.ProviderTypeOpenAI}},
		Models: []entities.ModelConfig{
			{Name: "openai/gpt-4o", Provider: "openai", ModelName: "gpt-4o"},
			// gpt-3-deprecated is absent — the catalog no longer lists it.
		},
	}

	removed := removeStaleModelsForRedetectedProviders(dst, detected)

	require.Len(t, removed, 1)
	assert.Equal(t, "openai/gpt-3-deprecated", removed[0].Name)
	require.Len(t, dst.Models, 1)
	assert.Equal(t, "openai/gpt-4o", dst.Models[0].Name, "the still-listed model must survive")
}

// TestRemoveStaleModelsForRedetectedProviders_NeverTouchesProviderNotRedetected
// is the specific safety property requested: a provider not re-detected
// this run — a self-hosted/manually-added provider like a private Ollama
// server (never a models.dev catalog provider at all), or a real catalog
// provider whose env var just isn't set this run — must have every one of
// its models left completely alone, never treated as "missing" and removed.
func TestRemoveStaleModelsForRedetectedProviders_NeverTouchesProviderNotRedetected(t *testing.T) {
	dst := &config.ProvidersFile{
		Providers: []entities.ProviderConfig{
			{Name: "drujensen", Type: entities.ProviderTypeOllama, BaseURL: "ai.drujensen.com"},
			{Name: "anthropic", Type: entities.ProviderTypeAnthropic},
		},
		Models: []entities.ModelConfig{
			{Name: "ornith", Provider: "drujensen", ModelName: "ornith:35b"},
			{Name: "qwen3.8", Provider: "drujensen", ModelName: "qwen3.8:latest"},
			{Name: "anthropic/claude", Provider: "anthropic", ModelName: "claude-opus"},
		},
	}
	// detected carries no "drujensen" provider at all (models.dev has never
	// heard of it — self-hosted) and no "anthropic" provider either (its
	// env var isn't set this particular run).
	detected := config.ProvidersFile{}

	removed := removeStaleModelsForRedetectedProviders(dst, detected)

	assert.Empty(t, removed, "nothing may be removed when no provider was actually re-detected")
	assert.Len(t, dst.Models, 3, "every model of every non-redetected provider must survive untouched")
}

func TestRemoveStaleModelsForRedetectedProviders_NothingStaleIsANoOp(t *testing.T) {
	dst := &config.ProvidersFile{
		Providers: []entities.ProviderConfig{{Name: "openai", Type: entities.ProviderTypeOpenAI}},
		Models:    []entities.ModelConfig{{Name: "openai/gpt-4o", Provider: "openai", ModelName: "gpt-4o"}},
	}
	detected := config.ProvidersFile{
		Providers: []entities.ProviderConfig{{Name: "openai", Type: entities.ProviderTypeOpenAI}},
		Models:    []entities.ModelConfig{{Name: "openai/gpt-4o", Provider: "openai", ModelName: "gpt-4o"}},
	}

	removed := removeStaleModelsForRedetectedProviders(dst, detected)

	assert.Empty(t, removed)
	assert.Len(t, dst.Models, 1)
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
