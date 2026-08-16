package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadLastAgent_MissingFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist", "last_agent.json")
	name, err := LoadLastAgent(path)
	require.NoError(t, err)
	assert.Equal(t, "", name)
}

func TestSaveLoadLastAgent_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last_agent.json")

	require.NoError(t, SaveLastAgent(path, "assistant"))
	got, err := LoadLastAgent(path)
	require.NoError(t, err)
	assert.Equal(t, "assistant", got)

	// Overwriting with a new name must replace, not append/merge.
	require.NoError(t, SaveLastAgent(path, "reviewer"))
	got, err = LoadLastAgent(path)
	require.NoError(t, err)
	assert.Equal(t, "reviewer", got)
}

func TestSaveLastAgent_CreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "last_agent.json")
	require.NoError(t, SaveLastAgent(path, "assistant"))

	got, err := LoadLastAgent(path)
	require.NoError(t, err)
	assert.Equal(t, "assistant", got)
}

func TestLoadLastAgent_MalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last_agent.json")
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o644))

	_, err := LoadLastAgent(path)
	assert.Error(t, err)
}
