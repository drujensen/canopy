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

func TestNewFileSearchTool_Contract(t *testing.T) {
	ft := NewFileSearchTool(FileSearchConfig{})

	var _ tool.FuncTool = ft
	assert.Equal(t, "file_search", ft.Name())
	assert.NotEmpty(t, ft.Description())

	_, approvalGated := ft.(tool.ApprovalRequiredTool)
	assert.False(t, approvalGated, "file search must not be approval-gated")
}

func setupSearchFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package main\n\nfunc TargetFunc() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("TargetFunc appears here too\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "c.go"), []byte("no match in this one\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "d.go"), []byte("TargetFunc should be skipped\n"), 0o644))
	return dir
}

func TestNewFileSearchTool_FindsMatchesInRealFiles(t *testing.T) {
	dir := setupSearchFixture(t)
	ft := NewFileSearchTool(FileSearchConfig{Root: dir})

	args, err := json.Marshal(map[string]any{"pattern": "TargetFunc"})
	require.NoError(t, err)

	out, err := ft.Call(context.Background(), string(args))
	require.NoError(t, err)

	result, ok := out.(FileSearchOutput)
	require.True(t, ok, "expected FileSearchOutput, got %T", out)
	require.Len(t, result.Matches, 2, "expected matches in a.go and b.txt, .git skipped")

	var paths []string
	for _, m := range result.Matches {
		paths = append(paths, filepath.Base(m.Path))
		assert.Contains(t, m.Text, "TargetFunc")
		assert.Greater(t, m.Line, 0)
	}
	assert.ElementsMatch(t, []string{"a.go", "b.txt"}, paths)
}

func TestNewFileSearchTool_GlobFilter(t *testing.T) {
	dir := setupSearchFixture(t)
	ft := NewFileSearchTool(FileSearchConfig{Root: dir})

	args, err := json.Marshal(map[string]any{"pattern": "TargetFunc", "glob": "*.go"})
	require.NoError(t, err)

	out, err := ft.Call(context.Background(), string(args))
	require.NoError(t, err)

	result := out.(FileSearchOutput)
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "a.go", filepath.Base(result.Matches[0].Path))
}

func TestNewFileSearchTool_MaxResultsTruncates(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "many.txt"), []byte("hit\nhit\nhit\nhit\n"), 0o644))

	ft := NewFileSearchTool(FileSearchConfig{Root: dir})
	args, err := json.Marshal(map[string]any{"pattern": "hit", "max_results": 2})
	require.NoError(t, err)

	out, err := ft.Call(context.Background(), string(args))
	require.NoError(t, err)

	result := out.(FileSearchOutput)
	assert.Len(t, result.Matches, 2)
	assert.True(t, result.Truncated)
}

func TestNewFileSearchTool_InvalidPatternErrors(t *testing.T) {
	dir := t.TempDir()
	ft := NewFileSearchTool(FileSearchConfig{Root: dir})

	args, err := json.Marshal(map[string]any{"pattern": "("})
	require.NoError(t, err)

	_, err = ft.Call(context.Background(), string(args))
	require.Error(t, err)
}

func TestNewFileSearchTool_PathTraversalRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TargetFunc"), 0o644))

	ft := NewFileSearchTool(FileSearchConfig{Root: root})
	args, err := json.Marshal(map[string]any{
		"pattern": "TargetFunc",
		"path":    "../" + filepath.Base(outside),
	})
	require.NoError(t, err)

	_, err = ft.Call(context.Background(), string(args))
	require.Error(t, err, "searching outside the configured root must be rejected")
}
