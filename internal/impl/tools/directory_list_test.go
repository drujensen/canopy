package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDirectoryListTool_Contract(t *testing.T) {
	dt := NewDirectoryListTool(DirectoryListConfig{})

	var _ tool.FuncTool = dt
	assert.Equal(t, "directory_list", dt.Name())
	assert.NotEmpty(t, dt.Description())

	_, approvalGated := dt.(tool.ApprovalRequiredTool)
	assert.False(t, approvalGated, "directory listing must not be approval-gated")
}

func setupListFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aa"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bbb"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "c.txt"), []byte("c"), 0o644))
	return dir
}

func TestNewDirectoryListTool_ListsTopLevel(t *testing.T) {
	dir := setupListFixture(t)
	dt := NewDirectoryListTool(DirectoryListConfig{Root: dir})

	out, err := dt.Call(context.Background(), `{}`)
	require.NoError(t, err)

	result, ok := out.(DirectoryListOutput)
	require.True(t, ok, "expected DirectoryListOutput, got %T", out)
	require.Len(t, result.Entries, 3)

	names := map[string]DirectoryEntry{}
	for _, e := range result.Entries {
		names[filepath.Base(e.Path)] = e
	}
	require.Contains(t, names, "a.txt")
	assert.False(t, names["a.txt"].IsDir)
	assert.Equal(t, int64(2), names["a.txt"].Size)
	require.Contains(t, names, "sub")
	assert.True(t, names["sub"].IsDir)
}

func TestNewDirectoryListTool_Recursive(t *testing.T) {
	dir := setupListFixture(t)
	dt := NewDirectoryListTool(DirectoryListConfig{Root: dir})

	args, err := json.Marshal(map[string]any{"recursive": true})
	require.NoError(t, err)

	out, err := dt.Call(context.Background(), string(args))
	require.NoError(t, err)

	result := out.(DirectoryListOutput)
	var found bool
	for _, e := range result.Entries {
		if filepath.Base(e.Path) == "c.txt" {
			found = true
		}
	}
	assert.True(t, found, "recursive listing must include nested files")
}

func TestNewDirectoryListTool_NotADirectoryErrors(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644))

	dt := NewDirectoryListTool(DirectoryListConfig{Root: dir})
	args, err := json.Marshal(map[string]any{"path": "f.txt"})
	require.NoError(t, err)

	_, err = dt.Call(context.Background(), string(args))
	require.Error(t, err)
}

func TestNewDirectoryListTool_PathTraversalRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644))

	dt := NewDirectoryListTool(DirectoryListConfig{Root: root})
	args, err := json.Marshal(map[string]any{"path": "../" + filepath.Base(outside)})
	require.NoError(t, err)

	_, err = dt.Call(context.Background(), string(args))
	require.Error(t, err, "listing outside the configured root must be rejected")
}
