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

func TestNewFileWriteTool_ContractAndApproval(t *testing.T) {
	wt := NewFileWriteTool(FileWriteConfig{})

	var _ tool.FuncTool = wt
	assert.Equal(t, "file_write", wt.Name())
	assert.NotEmpty(t, wt.Description())
	assert.NotNil(t, wt.Schema())

	approval, ok := wt.(tool.ApprovalRequiredTool)
	require.True(t, ok, "file write tool must implement tool.ApprovalRequiredTool")
	assert.True(t, approval.ApprovalRequired(), "file write must require approval per FR5")
}

func TestNewFileWriteTool_WritesActualFile(t *testing.T) {
	dir := t.TempDir()
	wt := NewFileWriteTool(FileWriteConfig{Root: dir})

	args, err := json.Marshal(map[string]string{"path": "out.txt", "content": "written content"})
	require.NoError(t, err)

	out, err := wt.Call(context.Background(), string(args))
	require.NoError(t, err)

	result, ok := out.(FileWriteOutput)
	require.True(t, ok, "expected FileWriteOutput, got %T", out)
	assert.Equal(t, len("written content"), result.BytesWritten)

	data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	require.NoError(t, err)
	assert.Equal(t, "written content", string(data))
}

func TestNewFileWriteTool_CreatesMissingParentDirs(t *testing.T) {
	dir := t.TempDir()
	wt := NewFileWriteTool(FileWriteConfig{Root: dir})

	args, err := json.Marshal(map[string]string{"path": "a/b/c.txt", "content": "nested"})
	require.NoError(t, err)

	_, err = wt.Call(context.Background(), string(args))
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, "a", "b", "c.txt"))
	require.NoError(t, err)
	assert.Equal(t, "nested", string(data))
}

func TestNewFileWriteTool_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))

	wt := NewFileWriteTool(FileWriteConfig{Root: dir})
	args, err := json.Marshal(map[string]string{"path": "existing.txt", "content": "new"})
	require.NoError(t, err)

	_, err = wt.Call(context.Background(), string(args))
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))
}

func TestNewFileWriteTool_PathTraversalRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	wt := NewFileWriteTool(FileWriteConfig{Root: root})

	args, err := json.Marshal(map[string]string{
		"path":    "../" + filepath.Base(outside) + "/evil.txt",
		"content": "pwned",
	})
	require.NoError(t, err)

	_, err = wt.Call(context.Background(), string(args))
	require.Error(t, err, "writing outside the configured root must be rejected")

	_, statErr := os.Stat(filepath.Join(outside, "evil.txt"))
	assert.True(t, os.IsNotExist(statErr), "file must not have been created outside root")
}
