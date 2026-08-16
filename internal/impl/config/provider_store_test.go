package config

import (
	"os"
	"path/filepath"
	"runtime"
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

// TestProviderStore_LoadInvalidJSON asserts a corrupted/hand-edited config
// file produces a wrapped unmarshal error, not a panic or a silently empty
// result — this is real, reachable-from-user-input behavior (a user hand-
// editing ~/.canopy/providers.json and breaking the JSON).
func TestProviderStore_LoadInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	require.NoError(t, os.WriteFile(path, []byte("{not valid json"), 0o644))

	store := NewProviderStore(path)
	_, err := store.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
}

// TestProviderStore_LoadPathIsDirectory asserts Load's os.ReadFile error
// branch for a non-missing-file error (here: the configured path is itself
// a directory, e.g. from a prior Save's Rename step landing on a stray
// directory) is wrapped and returned rather than misreported as "no config
// yet".
func TestProviderStore_LoadPathIsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	require.NoError(t, os.Mkdir(path, 0o755))

	store := NewProviderStore(path)
	_, err := store.Load()
	require.Error(t, err)
}

// TestProviderStore_SaveFailsWhenParentPathComponentIsAFile asserts Save's
// MkdirAll error branch: a path whose directory component collides with an
// existing regular file (a plausible manual-meddling scenario for a
// hand-managed ~/.canopy directory) fails clearly instead of silently
// writing somewhere unexpected.
func TestProviderStore_SaveFailsWhenParentPathComponentIsAFile(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("not a directory"), 0o644))

	store := NewProviderStore(filepath.Join(blocker, "sub", "providers.json"))
	err := store.Save(&ProvidersFile{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config directory")
}

// TestProviderStore_SaveFailsWhenTargetPathIsADirectory asserts Save's final
// os.Rename error branch: renaming the finished temp file onto a path that
// is itself an existing directory (again, a plausible stray-directory
// scenario) fails clearly instead of silently succeeding or corrupting
// state.
func TestProviderStore_SaveFailsWhenTargetPathIsADirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	require.NoError(t, os.Mkdir(path, 0o755))

	store := NewProviderStore(path)
	err := store.Save(&ProvidersFile{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rename")
}

// TestProviderStore_SaveFailsWhenDirectoryNotWritable asserts Save's
// os.CreateTemp error branch: a config directory that exists but isn't
// writable (permission issue) fails clearly. Skipped when running as root,
// since root bypasses Unix permission bits and the write would spuriously
// succeed.
func TestProviderStore_SaveFailsWhenDirectoryNotWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits don't apply on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}

	dir := t.TempDir()
	sub := filepath.Join(dir, "readonly")
	require.NoError(t, os.Mkdir(sub, 0o555))
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })

	store := NewProviderStore(filepath.Join(sub, "providers.json"))
	err := store.Save(&ProvidersFile{})
	require.Error(t, err)
}
