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

func TestNewFileReadTool_Contract(t *testing.T) {
	ft := NewFileReadTool(FileReadConfig{})

	var _ tool.FuncTool = ft
	assert.Equal(t, "file_read", ft.Name())
	assert.NotEmpty(t, ft.Description())
	assert.NotNil(t, ft.Schema())

	_, approvalGated := ft.(tool.ApprovalRequiredTool)
	assert.False(t, approvalGated, "file read must not be approval-gated")
}

func TestNewFileReadTool_ReadsActualFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello world"), 0o644))

	ft := NewFileReadTool(FileReadConfig{Root: dir})

	args, err := json.Marshal(map[string]string{"path": "hello.txt"})
	require.NoError(t, err)

	out, err := ft.Call(context.Background(), string(args))
	require.NoError(t, err)

	result, ok := out.(FileReadOutput)
	require.True(t, ok, "expected FileReadOutput, got %T", out)
	assert.Equal(t, "hello world", result.Content)
	assert.False(t, result.Truncated)
}

func TestNewFileReadTool_MissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	ft := NewFileReadTool(FileReadConfig{Root: dir})

	args, err := json.Marshal(map[string]string{"path": "nope.txt"})
	require.NoError(t, err)

	_, err = ft.Call(context.Background(), string(args))
	require.Error(t, err)
}

func TestNewFileReadTool_PathTraversalRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("nope"), 0o644))

	ft := NewFileReadTool(FileReadConfig{Root: root})

	args, err := json.Marshal(map[string]string{"path": "../" + filepath.Base(outside) + "/secret.txt"})
	require.NoError(t, err)

	_, err = ft.Call(context.Background(), string(args))
	require.Error(t, err, "reading outside the configured root must be rejected")
}

func TestNewFileReadTool_MaxBytesTruncates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	require.NoError(t, os.WriteFile(path, []byte("0123456789"), 0o644))

	ft := NewFileReadTool(FileReadConfig{Root: dir, MaxBytes: 4})

	args, err := json.Marshal(map[string]string{"path": "big.txt"})
	require.NoError(t, err)

	out, err := ft.Call(context.Background(), string(args))
	require.NoError(t, err)

	result, ok := out.(FileReadOutput)
	require.True(t, ok)
	assert.Equal(t, "0123", result.Content)
	assert.True(t, result.Truncated)
}
