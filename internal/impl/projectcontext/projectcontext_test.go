package projectcontext

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_BothFilesPresent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Claude instructions."), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Agents instructions."), 0o644))

	got, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, "Claude instructions.\n\nAgents instructions.", got)
}

func TestLoad_OnlyClaudeMD(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Claude only."), 0o644))

	got, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, "Claude only.", got)
}

func TestLoad_OnlyAgentsMD(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Agents only."), 0o644))

	got, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, "Agents only.", got)
}

func TestLoad_NeitherFilePresent(t *testing.T) {
	dir := t.TempDir()

	got, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

func TestLoad_TrimsSurroundingWhitespace(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("\n\n  Claude content.  \n\n"), 0o644))

	got, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, "Claude content.", got)
}
