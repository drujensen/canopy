package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadLastModel_MissingFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist", "last_model.json")
	name, err := LoadLastModel(path)
	require.NoError(t, err)
	assert.Equal(t, "", name)
}

func TestSaveLoadLastModel_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last_model.json")

	require.NoError(t, SaveLastModel(path, "openai/gpt-5"))
	got, err := LoadLastModel(path)
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-5", got)

	// Overwriting with a new name must replace, not append/merge.
	require.NoError(t, SaveLastModel(path, "anthropic/claude-sonnet-5"))
	got, err = LoadLastModel(path)
	require.NoError(t, err)
	assert.Equal(t, "anthropic/claude-sonnet-5", got)
}

func TestSaveLastModel_CreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "last_model.json")
	require.NoError(t, SaveLastModel(path, "openai/gpt-5"))

	got, err := LoadLastModel(path)
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-5", got)
}

func TestLoadLastModel_MalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last_model.json")
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o644))

	_, err := LoadLastModel(path)
	assert.Error(t, err)
}
