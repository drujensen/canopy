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

func TestNewFileEditTool_ContractAndNotApprovalGated(t *testing.T) {
	et := NewFileEditTool(FileEditConfig{})

	var _ tool.FuncTool = et
	assert.Equal(t, "file_edit", et.Name())
	assert.NotEmpty(t, et.Description())
	assert.NotNil(t, et.Schema())

	if approval, ok := et.(tool.ApprovalRequiredTool); ok {
		assert.False(t, approval.ApprovalRequired(), "file edit must not require approval — approval-gating was removed")
	}
}

func TestNewFileEditTool_ReplacesUniqueMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello world\n"), 0o644))

	et := NewFileEditTool(FileEditConfig{Root: dir})
	args, err := json.Marshal(map[string]string{"path": "existing.txt", "old_string": "world", "new_string": "canopy"})
	require.NoError(t, err)

	out, err := et.Call(context.Background(), string(args))
	require.NoError(t, err)

	result, ok := out.(FileEditOutput)
	require.True(t, ok, "expected FileEditOutput, got %T", out)
	assert.Equal(t, 1, result.LinesAdded)
	assert.Equal(t, 1, result.LinesRemoved)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello canopy\n", string(data))
}

func TestNewFileEditTool_AmbiguousMatchRejectedWithoutReplaceAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	require.NoError(t, os.WriteFile(path, []byte("a\na\na\n"), 0o644))

	et := NewFileEditTool(FileEditConfig{Root: dir})
	args, err := json.Marshal(map[string]string{"path": "existing.txt", "old_string": "a", "new_string": "b"})
	require.NoError(t, err)

	_, err = et.Call(context.Background(), string(args))
	require.Error(t, err, "a non-unique old_string must be rejected when replace_all isn't set")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "a\na\na\n", string(data), "file must be left untouched on rejection")
}

func TestNewFileEditTool_ReplaceAllReplacesEveryOccurrence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	require.NoError(t, os.WriteFile(path, []byte("a\na\na\n"), 0o644))

	et := NewFileEditTool(FileEditConfig{Root: dir})
	args, err := json.Marshal(map[string]any{"path": "existing.txt", "old_string": "a", "new_string": "b", "replace_all": true})
	require.NoError(t, err)

	_, err = et.Call(context.Background(), string(args))
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "b\nb\nb\n", string(data))
}

func TestNewFileEditTool_MissingOldStringRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello world\n"), 0o644))

	et := NewFileEditTool(FileEditConfig{Root: dir})
	args, err := json.Marshal(map[string]string{"path": "existing.txt", "old_string": "not present", "new_string": "x"})
	require.NoError(t, err)

	_, err = et.Call(context.Background(), string(args))
	require.Error(t, err, "old_string absent from the file must be rejected")
}

func TestNewFileEditTool_NoOpEditRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello world\n"), 0o644))

	et := NewFileEditTool(FileEditConfig{Root: dir})
	args, err := json.Marshal(map[string]string{"path": "existing.txt", "old_string": "world", "new_string": "world"})
	require.NoError(t, err)

	_, err = et.Call(context.Background(), string(args))
	require.Error(t, err, "old_string == new_string must be rejected rather than silently succeeding")
}

func TestNewFileEditTool_NonexistentFileRejected(t *testing.T) {
	dir := t.TempDir()

	et := NewFileEditTool(FileEditConfig{Root: dir})
	args, err := json.Marshal(map[string]string{"path": "missing.txt", "old_string": "x", "new_string": "y"})
	require.NoError(t, err)

	_, err = et.Call(context.Background(), string(args))
	require.Error(t, err, "editing a file that doesn't exist must fail — file_write is the tool for creating new files")
}

func TestNewFileEditTool_PathTraversalRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "evil.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("secret"), 0o644))

	et := NewFileEditTool(FileEditConfig{Root: root})
	args, err := json.Marshal(map[string]string{
		"path":       "../" + filepath.Base(outside) + "/evil.txt",
		"old_string": "secret",
		"new_string": "pwned",
	})
	require.NoError(t, err)

	_, err = et.Call(context.Background(), string(args))
	require.Error(t, err, "editing outside the configured root must be rejected")

	data, err := os.ReadFile(outsideFile)
	require.NoError(t, err)
	assert.Equal(t, "secret", string(data), "file outside root must be left untouched")
}
