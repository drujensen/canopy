package json

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/microsoft/agent-framework-go/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
)

func newTestRepo(t *testing.T) *ChatRepository {
	t.Helper()
	root := filepath.Join(t.TempDir(), "chats")
	repo, err := NewChatRepository(root)
	require.NoError(t, err)
	return repo
}

func sampleChat(id string) *entities.Chat {
	now := time.Now().UTC().Truncate(time.Second)
	return &entities.Chat{
		ID:        id,
		AgentName: "coder",
		Messages: []*message.Message{
			message.NewText("hello there"),
		},
		SessionState: []byte{0x00, 0x01, 0xFF, 0x7F, 0x80, 'x', '{', '}'},
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func TestChatRepository_CreateGetRoundTrip(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	chat := sampleChat("chat-1")
	require.NoError(t, repo.Create(ctx, chat))

	got, err := repo.Get(ctx, "chat-1")
	require.NoError(t, err)

	assert.Equal(t, chat.ID, got.ID)
	assert.Equal(t, chat.AgentName, got.AgentName)
	assert.True(t, chat.CreatedAt.Equal(got.CreatedAt))
	assert.True(t, chat.UpdatedAt.Equal(got.UpdatedAt))
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "hello there", got.Messages[0].String())

	// The SessionState blob must round-trip byte-for-byte as raw bytes —
	// this is the field that will later carry a serialized *agent.Session
	// (Design §3.9); Phase 1 only needs the plumbing to be lossless.
	assert.Equal(t, chat.SessionState, got.SessionState)
}

func TestChatRepository_CreateDuplicateFails(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	chat := sampleChat("dup")
	require.NoError(t, repo.Create(ctx, chat))

	err := repo.Create(ctx, sampleChat("dup"))
	assert.ErrorIs(t, err, ErrChatAlreadyExists)
}

func TestChatRepository_GetMissingFails(t *testing.T) {
	repo := newTestRepo(t)
	_, err := repo.Get(context.Background(), "does-not-exist")
	assert.ErrorIs(t, err, ErrChatNotFound)
}

func TestChatRepository_CreateEmptyIDFails(t *testing.T) {
	repo := newTestRepo(t)
	err := repo.Create(context.Background(), &entities.Chat{})
	assert.Error(t, err)
}

func TestChatRepository_Update(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	chat := sampleChat("chat-2")
	require.NoError(t, repo.Create(ctx, chat))

	chat.AgentName = "reviewer"
	chat.Messages = append(chat.Messages, message.NewText("second message"))
	chat.SessionState = []byte("updated-session-state")
	chat.UpdatedAt = chat.UpdatedAt.Add(time.Minute)
	require.NoError(t, repo.Update(ctx, chat))

	got, err := repo.Get(ctx, "chat-2")
	require.NoError(t, err)
	assert.Equal(t, "reviewer", got.AgentName)
	assert.Len(t, got.Messages, 2)
	assert.Equal(t, []byte("updated-session-state"), got.SessionState)
}

func TestChatRepository_UpdateMissingFails(t *testing.T) {
	repo := newTestRepo(t)
	err := repo.Update(context.Background(), sampleChat("ghost"))
	assert.ErrorIs(t, err, ErrChatNotFound)
}

func TestChatRepository_Delete(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	chat := sampleChat("chat-3")
	require.NoError(t, repo.Create(ctx, chat))
	require.NoError(t, repo.Delete(ctx, "chat-3"))

	_, err := repo.Get(ctx, "chat-3")
	assert.ErrorIs(t, err, ErrChatNotFound)
}

func TestChatRepository_DeleteMissingFails(t *testing.T) {
	repo := newTestRepo(t)
	err := repo.Delete(context.Background(), "ghost")
	assert.ErrorIs(t, err, ErrChatNotFound)
}

func TestChatRepository_List(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	require.NoError(t, repo.Create(ctx, sampleChat("a")))
	require.NoError(t, repo.Create(ctx, sampleChat("b")))
	require.NoError(t, repo.Create(ctx, sampleChat("c")))

	chats, err := repo.List(ctx)
	require.NoError(t, err)
	require.Len(t, chats, 3)

	ids := make(map[string]bool, 3)
	for _, c := range chats {
		ids[c.ID] = true
	}
	assert.True(t, ids["a"])
	assert.True(t, ids["b"])
	assert.True(t, ids["c"])
}

func TestChatRepository_ListEmpty(t *testing.T) {
	repo := newTestRepo(t)
	chats, err := repo.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, chats)
}

// TestChatRepository_NewChatRepository_EmptyRootFails asserts the empty-root
// guard, real reachable-from-caller-input validation (e.g. an unresolved
// config path passed straight through).
func TestChatRepository_NewChatRepository_EmptyRootFails(t *testing.T) {
	_, err := NewChatRepository("")
	assert.Error(t, err)
}

// TestChatRepository_NewChatRepository_MkdirAllFails asserts a storage root
// whose parent path collides with an existing regular file (a plausible
// stray-file scenario in a hand-managed directory tree) fails construction
// clearly instead of silently misbehaving later.
func TestChatRepository_NewChatRepository_MkdirAllFails(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("not a directory"), 0o644))

	_, err := NewChatRepository(filepath.Join(blocker, "chats"))
	assert.Error(t, err)
}

// TestChatRepository_Get_UnmarshalError asserts a corrupted chat file (e.g.
// a hand-edited or partially-written-then-crashed store file, prior to the
// atomic-rename protection this package otherwise provides) produces a
// wrapped unmarshal error rather than a panic.
func TestChatRepository_Get_UnmarshalError(t *testing.T) {
	repo := newTestRepo(t)
	require.NoError(t, os.WriteFile(repo.path("broken"), []byte("{not valid json"), 0o644))

	_, err := repo.Get(context.Background(), "broken")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broken")
}

// TestChatRepository_List_PropagatesGetError asserts List surfaces a
// per-file Get error (here: corrupted JSON) instead of silently skipping
// the bad file, so a corrupted chat file is loudly reported rather than
// making chats vanish from the list.
func TestChatRepository_List_PropagatesGetError(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, sampleChat("good")))
	require.NoError(t, os.WriteFile(repo.path("broken"), []byte("{not valid json"), 0o644))

	_, err := repo.List(ctx)
	assert.Error(t, err)
}

// TestChatRepository_List_ReadDirFails asserts List's os.ReadDir error
// branch (here: the storage root itself disappeared out from under the
// repository, e.g. an external process/user removing it) is wrapped and
// returned.
func TestChatRepository_List_ReadDirFails(t *testing.T) {
	repo := newTestRepo(t)
	require.NoError(t, os.RemoveAll(repo.root))

	_, err := repo.List(context.Background())
	assert.Error(t, err)
}

// TestChatRepository_Delete_NonNotExistErrorPropagates asserts Delete
// distinguishes a genuine ErrChatNotFound from some other os.Remove failure
// (here: the chat "file" is actually a non-empty directory, which
// os.Remove refuses to remove) rather than misreporting it as not-found.
func TestChatRepository_Delete_NonNotExistErrorPropagates(t *testing.T) {
	repo := newTestRepo(t)
	dir := repo.path("dir-chat")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644))

	err := repo.Delete(context.Background(), "dir-chat")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrChatNotFound)
}

// TestChatRepository_Update_RenameFailsWhenPathIsDirectory asserts write's
// final os.Rename error branch: Update's own os.Stat existence check
// succeeds against a directory that happens to occupy the chat's path (a
// plausible stray-directory scenario), but the atomic rename onto that
// directory then fails with a clear wrapped error rather than corrupting
// state.
func TestChatRepository_Update_RenameFailsWhenPathIsDirectory(t *testing.T) {
	repo := newTestRepo(t)
	require.NoError(t, os.Mkdir(repo.path("dir-chat"), 0o755))

	err := repo.Update(context.Background(), sampleChat("dir-chat"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rename")
}

// TestChatRepository_Create_CreateTempFailsWhenRootNotWritable asserts
// write's os.CreateTemp error branch: a storage root that exists but isn't
// writable (a permission issue) fails clearly. Skipped when running as
// root, since root bypasses Unix permission bits.
func TestChatRepository_Create_CreateTempFailsWhenRootNotWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits don't apply on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}

	repo := newTestRepo(t)
	require.NoError(t, os.Chmod(repo.root, 0o555))
	t.Cleanup(func() { _ = os.Chmod(repo.root, 0o755) })

	err := repo.Create(context.Background(), sampleChat("chat-1"))
	require.Error(t, err)
}
