package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
)

func TestProviderStore_LoadMissingFileReturnsEmpty(t *testing.T) {
	store := NewProviderStore(filepath.Join(t.TempDir(), "providers.json"))
	file, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, file.Providers)
	assert.Empty(t, file.Models)
}

func TestProviderStore_SaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".canopy", "providers.json")
	store := NewProviderStore(path)

	original := &ProvidersFile{
		Providers: []entities.ProviderConfig{
			{
				Name:    "work-openai",
				Type:    entities.ProviderTypeOpenAI,
				APIKey:  "sk-test-12345",
				BaseURL: "",
			},
			{
				Name:    "local-ollama",
				Type:    entities.ProviderTypeOllama,
				APIKey:  "",
				BaseURL: "http://localhost:11434/v1",
			},
		},
		Models: []entities.ModelConfig{
			{
				Name:      "fast",
				Provider:  "work-openai",
				ModelName: "gpt-4o-mini",
				Parameters: map[string]any{
					"temperature": 0.2,
					"max_tokens":  float64(4096),
				},
			},
		},
	}

	require.NoError(t, store.Save(original))

	loaded, err := store.Load()
	require.NoError(t, err)

	require.Len(t, loaded.Providers, 2)
	assert.Equal(t, original.Providers[0], loaded.Providers[0])
	assert.Equal(t, original.Providers[1], loaded.Providers[1])

	require.Len(t, loaded.Models, 1)
	assert.Equal(t, original.Models[0].Name, loaded.Models[0].Name)
	assert.Equal(t, original.Models[0].Provider, loaded.Models[0].Provider)
	assert.Equal(t, original.Models[0].ModelName, loaded.Models[0].ModelName)
	assert.Equal(t, 0.2, loaded.Models[0].Parameters["temperature"])
}

func TestProviderStore_SaveCreatesParentDirectories(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "dir", "providers.json")
	store := NewProviderStore(path)

	require.NoError(t, store.Save(&ProvidersFile{
		Providers: []entities.ProviderConfig{{Name: "p", Type: entities.ProviderTypeAnthropic}},
	}))

	loaded, err := store.Load()
	require.NoError(t, err)
	require.Len(t, loaded.Providers, 1)
	assert.Equal(t, "p", loaded.Providers[0].Name)
}

func TestNewGlobalProviderStore_UsesHomeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := NewGlobalProviderStore()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".canopy", "providers.json"), store.Path())
}

func TestNewProviderStoreForProject_PrefersProjectLocalWhenPresent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projectRoot := t.TempDir()
	projectStore := NewProjectProviderStore(projectRoot)
	require.NoError(t, projectStore.Save(&ProvidersFile{
		Providers: []entities.ProviderConfig{{Name: "project-provider", Type: entities.ProviderTypeGemini}},
	}))

	resolved, err := NewProviderStoreForProject(projectRoot, false)
	require.NoError(t, err)
	assert.Equal(t, projectStore.Path(), resolved.Path())
}

func TestNewProviderStoreForProject_FallsBackToGlobalWhenNoProjectFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projectRoot := t.TempDir()

	resolved, err := NewProviderStoreForProject(projectRoot, false)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".canopy", "providers.json"), resolved.Path())
}

func TestNewProviderStoreForProject_GlobalFlagForcesGlobalEvenWithProjectFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projectRoot := t.TempDir()
	projectStore := NewProjectProviderStore(projectRoot)
	require.NoError(t, projectStore.Save(&ProvidersFile{
		Providers: []entities.ProviderConfig{{Name: "project-provider", Type: entities.ProviderTypeGemini}},
	}))

	resolved, err := NewProviderStoreForProject(projectRoot, true)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".canopy", "providers.json"), resolved.Path())
}
