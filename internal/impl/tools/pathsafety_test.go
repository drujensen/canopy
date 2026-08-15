package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSafePath_WithinRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o644))

	resolved, err := resolveSafePath(root, "a.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "a.txt"), resolved)
}

func TestResolveSafePath_NestedWithinRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub", "dir"), 0o755))

	resolved, err := resolveSafePath(root, "sub/dir/file.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "sub", "dir", "file.txt"), resolved)
}

func TestResolveSafePath_DotDotEscapeRejected(t *testing.T) {
	root := t.TempDir()

	_, err := resolveSafePath(root, "../escape.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes configured root")
}

func TestResolveSafePath_DeepDotDotEscapeRejected(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o755))

	_, err := resolveSafePath(root, "a/b/../../../escape.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes configured root")
}

func TestResolveSafePath_AbsolutePathOutsideRootRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // a different temp dir, guaranteed outside root

	_, err := resolveSafePath(root, filepath.Join(outside, "x.txt"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes configured root")
}

func TestResolveSafePath_SymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s3cr3t"), 0o644))

	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(outside, link))

	_, err := resolveSafePath(root, "link/secret.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes configured root")
}

func TestResolveSafePath_NoConfinementWhenRootEmpty(t *testing.T) {
	// Documented behavior: an empty root means no confinement is enforced —
	// the caller explicitly opted out. This test exists to make that a
	// visible, intentional contract rather than an accident.
	resolved, err := resolveSafePath("", "../anything/goes.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Clean("../anything/goes.txt"), resolved)
}

func TestResolveSafePath_NewFileUnderExistingDirWithinRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))

	// File doesn't exist yet (as in a file-write call); should still resolve
	// cleanly within root via nearest-existing-ancestor resolution.
	resolved, err := resolveSafePath(root, "sub/new-file.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "sub", "new-file.txt"), resolved)
}

func TestResolveSafePath_EmptyPathRejected(t *testing.T) {
	root := t.TempDir()
	_, err := resolveSafePath(root, "")
	require.Error(t, err)
}
